// graphql.go — GraphQL v4 API at POST /api/graphql.
//
// A small, dependency-free executor for the fixed set of documents the
// conformance suite sends (the ones gh and github-mcp-server send): it
// parses operations, aliases, variables, enums, inline input objects/lists,
// inline fragments and __type introspection, then resolves against the same
// in-memory store as REST. Ported from the Node reference (graphql.ts) into
// idiomatic Go, stdlib only.
//
// Rules honoured: global ids are opaque base64; unknown repo ->
// data.repository null with no errors; unknown field -> HTTP 200 with an
// errors entry naming the field; batched requests (JSON array) -> array of
// results in order; unauthenticated -> 401.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tokenizer + parser
// ---------------------------------------------------------------------------

type gqlToken struct {
	typ string // "{", "}", "(", ")", ":", "$", "!", "[", "]", "=", "@", "...", "string", "number", "name", "EOF"
	val string
	num float64
}

func gqlTokenize(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '{' || ch == '}' || ch == '(' || ch == ')' || ch == ':' || ch == '$' || ch == '!' || ch == '[' || ch == ']' || ch == '=' || ch == '@':
			tokens = append(tokens, gqlToken{typ: string(ch), val: string(ch)})
			i++
		case strings.HasPrefix(src[i:], "..."):
			tokens = append(tokens, gqlToken{typ: "...", val: "..."})
			i += 3
		case ch == '"':
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				tokens = append(tokens, gqlToken{typ: "string", val: src[i+3 : i+3+end]})
				i += 3 + end + 3
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					esc := src[j+1]
					if esc == 'u' {
						n, err := strconv.ParseInt(src[j+2:j+6], 16, 32)
						if err != nil {
							return nil, fmt.Errorf("bad unicode escape")
						}
						out.WriteRune(rune(n))
						j += 6
					} else {
						switch esc {
						case 'n':
							out.WriteByte('\n')
						case 't':
							out.WriteByte('\t')
						case 'r':
							out.WriteByte('\r')
						default:
							out.WriteByte(esc)
						}
						j += 2
					}
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			tokens = append(tokens, gqlToken{typ: "string", val: out.String()})
			i = j + 1
		case ch >= '0' && ch <= '9' || (ch == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			raw := src[i:j]
			num, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("bad number %s", raw)
			}
			tokens = append(tokens, gqlToken{typ: "number", val: raw, num: num})
			i = j
		case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_':
			j := i
			for j < len(src) && (src[j] >= 'A' && src[j] <= 'Z' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= '0' && src[j] <= '9' || src[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{typ: "name", val: src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", string(ch))
		}
	}
	tokens = append(tokens, gqlToken{typ: "EOF"})
	return tokens, nil
}

// gqlValue is one parsed argument/default value.
type gqlValue struct {
	kind   string // variable, string, number, boolean, null, enum, list, object
	str    string
	num    float64
	b      bool
	list   []*gqlValue
	fields map[string]*gqlValue
	keys   []string // object field order (kept for determinism)
}

type gqlSelection struct {
	kind          string // field, inlineFragment, fragmentSpread
	name          string // field name or fragment name
	alias         string
	typeCondition string
	args          map[string]*gqlValue
	selections    []*gqlSelection
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValue
}

type gqlOperation struct {
	operation  string // query, mutation, subscription
	name       string
	variables  []gqlVarDef
	selections []*gqlSelection
}

type gqlFragment struct {
	typeCondition string
	selections    []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(typ, what string) (gqlToken, error) {
	t := p.next()
	if t.typ != typ {
		return t, fmt.Errorf("expected %s, got %s %q", what, t.typ, t.val)
	}
	return t, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().typ == "@" {
		p.next()
		if _, err := p.expect("name", "directive name"); err != nil {
			return err
		}
		if p.peek().typ == "(" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: make(map[string]*gqlFragment)}
	for p.peek().typ != "EOF" {
		if p.peek().typ == "name" && p.peek().val == "fragment" {
			p.next()
			nameTok, err := p.expect("name", "fragment name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect("name", "'on'"); err != nil {
				return nil, err
			}
			typeTok, err := p.expect("name", "type condition")
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[nameTok.val] = &gqlFragment{typeCondition: typeTok.val, selections: sels}
			continue
		}
		operation := "query"
		if p.peek().typ == "name" && (p.peek().val == "query" || p.peek().val == "mutation" || p.peek().val == "subscription") {
			operation = p.next().val
		}
		var name string
		if p.peek().typ == "name" {
			name = p.next().val
		}
		var vars []gqlVarDef
		if p.peek().typ == "(" {
			var err error
			vars, err = p.parseVariableDefinitions()
			if err != nil {
				return nil, err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, &gqlOperation{operation: operation, name: name, variables: vars, selections: sels})
	}
	return doc, nil
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	if _, err := p.expect("(", "'('"); err != nil {
		return nil, err
	}
	var defs []gqlVarDef
	for p.peek().typ != ")" {
		if _, err := p.expect("$", "'$'"); err != nil {
			return nil, err
		}
		nameTok, err := p.expect("name", "variable name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(":", "':'"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var def *gqlValue
		if p.peek().typ == "=" {
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			def = v
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		defs = append(defs, gqlVarDef{name: nameTok.val, defaultValue: def})
	}
	if _, err := p.expect(")", "')'"); err != nil {
		return nil, err
	}
	return defs, nil
}

// parseType consumes a type annotation; only its shape matters here.
func (p *gqlParser) parseType() error {
	if p.peek().typ == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if _, err := p.expect("]", "']'"); err != nil {
			return err
		}
	} else {
		if _, err := p.expect("name", "type name"); err != nil {
			return err
		}
	}
	if p.peek().typ == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if _, err := p.expect("{", "'{'"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for p.peek().typ != "}" {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if _, err := p.expect("}", "'}'"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.peek().typ == "..." {
		p.next()
		if p.peek().typ == "name" && p.peek().val == "on" {
			p.next()
			typeTok, err := p.expect("name", "type condition")
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "inlineFragment", typeCondition: typeTok.val, selections: sels}, nil
		}
		nameTok, err := p.expect("name", "fragment name")
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "fragmentSpread", name: nameTok.val}, nil
	}
	nameTok, err := p.expect("name", "field name")
	if err != nil {
		return nil, err
	}
	name := nameTok.val
	alias := name
	if p.peek().typ == ":" {
		p.next()
		alias = name
		realTok, err := p.expect("name", "field name")
		if err != nil {
			return nil, err
		}
		name = realTok.val
	}
	var args map[string]*gqlValue
	if p.peek().typ == "(" {
		var err error
		args, err = p.parseArguments()
		if err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	if p.peek().typ == "{" {
		var err error
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{kind: "field", name: name, alias: alias, args: args, selections: sels}, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	if _, err := p.expect("(", "'('"); err != nil {
		return nil, err
	}
	args := make(map[string]*gqlValue)
	for p.peek().typ != ")" {
		nameTok, err := p.expect("name", "argument name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(":", "':'"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.val] = v
	}
	if _, err := p.expect(")", "')'"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	tok := p.next()
	switch tok.typ {
	case "$":
		nameTok, err := p.expect("name", "variable name")
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "variable", str: nameTok.val}, nil
	case "string":
		return &gqlValue{kind: "string", str: tok.val}, nil
	case "number":
		return &gqlValue{kind: "number", num: tok.num}, nil
	case "[":
		v := &gqlValue{kind: "list"}
		for p.peek().typ != "]" {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, item)
		}
		if _, err := p.expect("]", "']'"); err != nil {
			return nil, err
		}
		return v, nil
	case "{":
		v := &gqlValue{kind: "object", fields: make(map[string]*gqlValue)}
		for p.peek().typ != "}" {
			nameTok, err := p.expect("name", "object field name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":", "':'"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.fields[nameTok.val] = val
			v.keys = append(v.keys, nameTok.val)
		}
		if _, err := p.expect("}", "'}'"); err != nil {
			return nil, err
		}
		return v, nil
	case "name":
		switch tok.val {
		case "true":
			return &gqlValue{kind: "boolean", b: true}, nil
		case "false":
			return &gqlValue{kind: "boolean", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: tok.val}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %s", tok.typ)
}

// evalValue resolves a parsed value against the operation variables.
func evalValue(node *gqlValue, vars map[string]any) any {
	if node == nil {
		return nil
	}
	switch node.kind {
	case "variable":
		return vars[node.str]
	case "string", "enum":
		return node.str
	case "number":
		return node.num
	case "boolean":
		return node.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(node.list))
		for _, v := range node.list {
			out = append(out, evalValue(v, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(node.fields))
		for _, k := range node.keys {
			out[k] = evalValue(node.fields[k], vars)
		}
		return out
	}
	return nil
}

func evalArgs(field *gqlSelection, vars map[string]any) map[string]any {
	out := make(map[string]any, len(field.args))
	for k, node := range field.args {
		out[k] = evalValue(node, vars)
	}
	return out
}

// ---------------------------------------------------------------------------
// Static introspection model
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":                     {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "closedAt", "author", "url", "databaseId"},
	"PullRequest":               {"id", "number", "title", "state", "body", "headRefName", "baseRefName", "mergeable", "merged", "mergeCommit", "files", "createdAt", "updatedAt", "closedAt", "author", "url", "databaseId"},
	"Repository":                {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":                      {"id", "login", "name", "email", "databaseId"},
	"Ref":                       {"name", "prefix", "target"},
	"IssueConnection":           {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":     {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":                 {"node", "cursor"},
	"PullRequestEdge":           {"node", "cursor"},
	"PullRequestFileEdge":       {"node", "cursor"},
	"PageInfo":                  {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"IssueComment":              {"id", "body", "author", "createdAt", "updatedAt", "url"},
	"IssueCommentEdge":          {"node", "cursor"},
	"PullRequestFile":           {"path"},
	"Commit":                    {"oid", "message"},
	"Query":                     {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":                  {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"CreateIssuePayload":        {"issue", "clientMutationId"},
	"CloseIssuePayload":         {"issue", "clientMutationId"},
	"CreatePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":   {"pullRequest", "clientMutationId"},
	"AddCommentPayload":         {"commentEdge", "clientMutationId"},
	"__Type":                    {"kind", "name", "fields", "enumValues", "description"},
	"__Field":                   {"name", "description", "args", "type", "isDeprecated", "deprecationReason"},
	"__EnumValue":               {"name", "description", "isDeprecated", "deprecationReason"},
	"__InputValue":              {"name", "description", "type", "defaultValue"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":       {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"IssueOrderField":  {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":   {"ASC", "DESC"},
}

func gqlEncodeCursor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(index)))
}

func gqlDecodeCursor(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return -1
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// gqlCompoundID is base64("Kind:a:b:..."), the Node reference's global-id
// shape for issues, pull requests and comments (GlobalID in store.go only
// takes a single int, so compound ids are built here).
func gqlCompoundID(kind string, parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strings.Join(parts, ":")))
}

// ---------------------------------------------------------------------------
// Object wrappers (carry __typename so inline fragments resolve)
// ---------------------------------------------------------------------------

// gqlObj is one resolved value that can carry sub-selections.
type gqlObj struct {
	typename         string
	repo             *Repo
	issue            *Issue
	pr               *Pull
	comment          *Comment
	refName          string
	filePath         string
	commitOID        string
	commitMsg        string
	nodes            []*gqlObj
	pageInfo         *gqlObj
	totalCount       int
	hasNext          bool
	endCursor        any // string or nil
	edgeNode         *gqlObj
	cursor           string
	typeName         string  // __Type
	fieldName        string  // __Field / __EnumValue
	payload          *gqlObj // payload's wrapped object (or nil)
	payloadKey       string
	clientMutationId any
}

func gqlRepoObj(repo *Repo) *gqlObj { return &gqlObj{typename: "Repository", repo: repo} }
func gqlIssueObj(repo *Repo, iss *Issue) *gqlObj {
	return &gqlObj{typename: "Issue", repo: repo, issue: iss}
}
func gqlPrObj(repo *Repo, pr *Pull) *gqlObj {
	return &gqlObj{typename: "PullRequest", repo: repo, pr: pr}
}
func gqlUserObj() *gqlObj { return &gqlObj{typename: "User"} }
func gqlRefObj(repo *Repo, name string) *gqlObj {
	return &gqlObj{typename: "Ref", repo: repo, refName: name}
}
func gqlCommentObj(repo *Repo, iss *Issue, c *Comment) *gqlObj {
	return &gqlObj{typename: "IssueComment", repo: repo, issue: iss, comment: c}
}
func gqlFileObj(path string) *gqlObj { return &gqlObj{typename: "PullRequestFile", filePath: path} }

func gqlConnectionObj(kind string, nodes []*gqlObj, pageInfo *gqlObj, total int) *gqlObj {
	return &gqlObj{typename: kind, nodes: nodes, pageInfo: pageInfo, totalCount: total}
}
func gqlPageInfoObj(hasNext bool, endCursor any) *gqlObj {
	return &gqlObj{typename: "PageInfo", hasNext: hasNext, endCursor: endCursor}
}

// ---------------------------------------------------------------------------
// Global-id lookups
// ---------------------------------------------------------------------------

func gqlFindRepoByDbID(s *Store, dbID int) *Repo {
	for _, repo := range s.Repos {
		if repo.DbID == dbID {
			return repo
		}
	}
	return nil
}

// gqlDecodeCompound splits a base64 global id into kind + rest parts.
func gqlDecodeCompound(gid string) (kind string, rest []string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(gid)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 1 || parts[0] == "" {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func gqlAtoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// gqlFindIssueByGlobalID resolves an Issue id in every shape it may arrive
// in: the compound "Issue:dbId:number" the reference writes, or a single
// number / REST id when the id came from another surface.
func gqlFindIssueByGlobalID(s *Store, gid string) (*Repo, *Issue) {
	kind, rest, ok := gqlDecodeCompound(gid)
	if !ok || kind != "Issue" {
		return nil, nil
	}
	if len(rest) >= 2 {
		if dbID, ok1 := gqlAtoi(rest[0]); ok1 {
			if num, ok2 := gqlAtoi(rest[1]); ok2 {
				if repo := gqlFindRepoByDbID(s, dbID); repo != nil {
					if iss := repo.Issues[num]; iss != nil {
						return repo, iss
					}
				}
			}
		}
		return nil, nil
	}
	if len(rest) == 1 {
		v, ok := gqlAtoi(rest[0])
		if !ok {
			return nil, nil
		}
		for _, repo := range s.Repos {
			if iss := repo.Issues[v]; iss != nil {
				return repo, iss
			}
		}
		for _, repo := range s.Repos {
			for _, iss := range repo.Issues {
				if iss.RestID == v {
					return repo, iss
				}
			}
		}
	}
	return nil, nil
}

func gqlFindPrByGlobalID(s *Store, gid string) (*Repo, *Pull) {
	kind, rest, ok := gqlDecodeCompound(gid)
	if !ok || kind != "PullRequest" {
		return nil, nil
	}
	if len(rest) >= 2 {
		if dbID, ok1 := gqlAtoi(rest[0]); ok1 {
			if num, ok2 := gqlAtoi(rest[1]); ok2 {
				if repo := gqlFindRepoByDbID(s, dbID); repo != nil {
					if pr := repo.Pulls[num]; pr != nil {
						return repo, pr
					}
				}
			}
		}
		return nil, nil
	}
	if len(rest) == 1 {
		v, ok := gqlAtoi(rest[0])
		if !ok {
			return nil, nil
		}
		for _, repo := range s.Repos {
			if pr := repo.Pulls[v]; pr != nil {
				return repo, pr
			}
		}
		for _, repo := range s.Repos {
			for _, pr := range repo.Pulls {
				if pr.RestID == v {
					return repo, pr
				}
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// gqlUndefined marks "this field does not exist on this object".
type gqlUndefined struct{}

type gqlExecutor struct {
	s      *Store
	errors []map[string]any
}

func (e *gqlExecutor) errf(format string, args ...any) {
	e.errors = append(e.errors, map[string]any{"message": fmt.Sprintf(format, args...)})
}

func (e *gqlExecutor) run(doc *gqlDocument, vars map[string]any, operationName string) map[string]any {
	var op *gqlOperation
	if len(doc.operations) > 0 {
		op = doc.operations[0]
	}
	if operationName != "" {
		for _, o := range doc.operations {
			if o.name == operationName {
				op = o
				break
			}
		}
	}
	if op == nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "No operation found"}}}
	}
	merged := make(map[string]any, len(vars)+len(op.variables))
	for k, v := range vars {
		merged[k] = v
	}
	for _, def := range op.variables {
		if _, ok := merged[def.name]; !ok && def.defaultValue != nil {
			merged[def.name] = evalValue(def.defaultValue, merged)
		}
	}
	rootType := "Query"
	if op.operation == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlObj{typename: rootType}
	data := e.executeSelections(rootType, root, op.selections, merged, doc.fragments)
	out := map[string]any{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}

func (e *gqlExecutor) executeSelections(typeName string, obj *gqlObj, sels []*gqlSelection, vars map[string]any, frags map[string]*gqlFragment) map[string]any {
	effectiveType := typeName
	if obj.typename != "" {
		effectiveType = obj.typename
	}
	result := make(map[string]any, len(sels))
	for _, sel := range sels {
		switch sel.kind {
		case "inlineFragment":
			if sel.typeCondition == "" || sel.typeCondition == obj.typename {
				for k, v := range e.executeSelections(effectiveType, obj, sel.selections, vars, frags) {
					result[k] = v
				}
			}
		case "fragmentSpread":
			if frag, ok := frags[sel.name]; ok {
				if frag.typeCondition == "" || frag.typeCondition == obj.typename {
					for k, v := range e.executeSelections(effectiveType, obj, frag.selections, vars, frags) {
						result[k] = v
					}
				}
			}
		case "field":
			result[sel.alias] = e.resolveField(effectiveType, obj, sel, vars, frags)
		}
	}
	return result
}

func (e *gqlExecutor) resolveField(typeName string, obj *gqlObj, field *gqlSelection, vars map[string]any, frags map[string]*gqlFragment) any {
	args := evalArgs(field, vars)
	if field.name == "__typename" {
		if obj.typename != "" {
			return obj.typename
		}
		return typeName
	}
	if known, ok := gqlTypeFields[typeName]; ok {
		found := false
		for _, f := range known {
			if f == field.name {
				found = true
				break
			}
		}
		if !found {
			e.errf("Cannot query field %q on type %q.", field.name, typeName)
			return nil
		}
	}
	value := e.fieldValue(typeName, obj, field.name, args)
	if _, undef := value.(gqlUndefined); undef {
		e.errf("Cannot query field %q on type %q.", field.name, typeName)
		return nil
	}
	if field.selections == nil {
		return value
	}
	if value == nil {
		return nil
	}
	if arr, ok := value.([]*gqlObj); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, e.executeSelections(item.typename, item, field.selections, vars, frags))
		}
		return out
	}
	if child, ok := value.(*gqlObj); ok {
		return e.executeSelections(child.typename, child, field.selections, vars, frags)
	}
	return value
}

func (e *gqlExecutor) fieldValue(typeName string, obj *gqlObj, name string, args map[string]any) any {
	switch typeName {
	case "Query":
		return e.queryField(name, args)
	case "Mutation":
		return e.mutationField(name, args)
	case "User":
		return e.userField(name)
	case "Repository":
		return e.repositoryField(obj, name, args)
	case "Issue":
		return e.issueField(obj, name)
	case "PullRequest":
		return e.pullRequestField(obj, name, args)
	case "Ref":
		return e.refField(obj, name)
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		return e.connectionField(obj, name)
	case "IssueEdge", "PullRequestEdge", "PullRequestFileEdge", "IssueCommentEdge":
		return e.edgeField(obj, name)
	case "PageInfo":
		switch name {
		case "hasNextPage":
			return obj.hasNext
		case "endCursor":
			return obj.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return nil
		}
		return gqlUndefined{}
	case "IssueComment":
		return e.commentField(obj, name)
	case "PullRequestFile":
		if name == "path" {
			return obj.filePath
		}
		return gqlUndefined{}
	case "Commit":
		switch name {
		case "oid":
			return obj.commitOID
		case "message":
			return strings.TrimSuffix(obj.commitMsg, "\n")
		}
		return gqlUndefined{}
	case "CreateIssuePayload", "CloseIssuePayload":
		return e.payloadField(obj, name, "issue")
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		return e.payloadField(obj, name, "pullRequest")
	case "AddCommentPayload":
		return e.payloadField(obj, name, "commentEdge")
	case "__Type":
		return e.introspectionTypeField(obj, name)
	case "__Field":
		return e.introspectionFieldField(obj, name)
	case "__EnumValue":
		return e.introspectionEnumField(obj, name)
	case "__InputValue":
		return gqlUndefined{}
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) queryField(name string, args map[string]any) any {
	switch name {
	case "viewer":
		return gqlUserObj()
	case "repository":
		owner, _ := args["owner"].(string)
		repoName, _ := args["name"].(string)
		repo := FindRepo(e.s, owner, repoName)
		if repo == nil {
			return nil
		}
		return gqlRepoObj(repo)
	case "node":
		gid, _ := args["id"].(string)
		if gid == "" {
			return nil
		}
		kind, rest, ok := gqlDecodeCompound(gid)
		if !ok {
			return nil
		}
		switch kind {
		case "Repository":
			if len(rest) >= 1 {
				if dbID, ok := gqlAtoi(rest[0]); ok {
					if repo := gqlFindRepoByDbID(e.s, dbID); repo != nil {
						return gqlRepoObj(repo)
					}
				}
			}
		case "Issue":
			if repo, iss := gqlFindIssueByGlobalID(e.s, gid); iss != nil {
				return gqlIssueObj(repo, iss)
			}
		case "PullRequest":
			if repo, pr := gqlFindPrByGlobalID(e.s, gid); pr != nil {
				return gqlPrObj(repo, pr)
			}
		}
		return nil
	case "__type":
		typeName, _ := args["name"].(string)
		if typeName == "" {
			return nil
		}
		if _, ok := gqlTypeFields[typeName]; ok {
			return &gqlObj{typename: "__Type", typeName: typeName}
		}
		if _, ok := gqlEnumValues[typeName]; ok {
			return &gqlObj{typename: "__Type", typeName: typeName}
		}
		return nil
	case "__schema":
		return &gqlObj{typename: "__Schema"}
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) userField(name string) any {
	switch name {
	case "id":
		return GlobalID("User", ViewerID)
	case "login":
		return ViewerLogin
	case "name":
		return ViewerName
	case "email":
		return ViewerEmail
	case "databaseId":
		return ViewerID
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) repositoryField(obj *gqlObj, name string, args map[string]any) any {
	repo := obj.repo
	switch name {
	case "id":
		// The opaque id is REST's node_id, so both surfaces agree.
		if v, ok := repoJSON(e.s, repo)["node_id"]; ok {
			return v
		}
		return repo.NodeID
	case "databaseId":
		return repo.DbID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return gqlUserObj()
	case "defaultBranchRef":
		tip := RefCommit(repo, "refs/heads/"+repo.DefaultBranch)
		if tip == "" {
			return nil
		}
		return gqlRefObj(repo, repo.DefaultBranch)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		num := gqlArgInt(args["number"])
		if iss := repo.Issues[num]; iss != nil {
			return gqlIssueObj(repo, iss)
		}
		return nil
	case "issues":
		return e.issuesConnection(repo, args)
	case "pullRequest":
		num := gqlArgInt(args["number"])
		if pr := repo.Pulls[num]; pr != nil {
			return gqlPrObj(repo, pr)
		}
		return nil
	case "pullRequests":
		return e.pullsConnection(repo, args)
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) issuesConnection(repo *Repo, args map[string]any) *gqlObj {
	list := make([]*Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		list = append(list, iss)
	}
	if rawStates, ok := args["states"]; ok {
		if states, ok := rawStates.([]any); ok && len(states) > 0 {
			wanted := make(map[string]bool, len(states))
			for _, st := range states {
				if s, ok := st.(string); ok {
					wanted[strings.ToLower(s)] = true
				}
			}
			filtered := list[:0]
			for _, iss := range list {
				if wanted[iss.State] {
					filtered = append(filtered, iss)
				}
			}
			list = filtered
		}
	}
	desc := true
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if dir, ok := ob["direction"].(string); ok && dir == "ASC" {
			desc = false
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if desc {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	nodes := make([]*gqlObj, 0, len(list))
	for _, iss := range list {
		nodes = append(nodes, gqlIssueObj(repo, iss))
	}
	return e.paginate(nodes, args, "IssueConnection")
}

func (e *gqlExecutor) pullsConnection(repo *Repo, args map[string]any) *gqlObj {
	list := make([]*Pull, 0, len(repo.Pulls))
	for _, pr := range repo.Pulls {
		list = append(list, pr)
	}
	if rawStates, ok := args["states"]; ok {
		if states, ok := rawStates.([]any); ok && len(states) > 0 {
			wanted := make(map[string]bool, len(states))
			for _, st := range states {
				if s, ok := st.(string); ok {
					wanted[strings.ToUpper(s)] = true
				}
			}
			filtered := list[:0]
			for _, pr := range list {
				state := "CLOSED"
				if pr.State == "open" {
					state = "OPEN"
				} else if pr.Merged {
					state = "MERGED"
				}
				if wanted[state] {
					filtered = append(filtered, pr)
				}
			}
			list = filtered
		}
	}
	desc := true
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if dir, ok := ob["direction"].(string); ok && dir == "ASC" {
			desc = false
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if desc {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	nodes := make([]*gqlObj, 0, len(list))
	for _, pr := range list {
		nodes = append(nodes, gqlPrObj(repo, pr))
	}
	return e.paginate(nodes, args, "PullRequestConnection")
}

func (e *gqlExecutor) paginate(nodes []*gqlObj, args map[string]any, kind string) *gqlObj {
	first := len(nodes)
	if f, ok := args["first"]; ok {
		if n := gqlArgInt(f); n >= 0 {
			first = n
		}
	}
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if idx := gqlDecodeCursor(after); idx >= 0 {
			start = idx + 1
		}
	}
	if start > len(nodes) {
		start = len(nodes)
	}
	end := start + first
	if end > len(nodes) {
		end = len(nodes)
	}
	page := nodes[start:end]
	hasNext := end < len(nodes)
	var endCursor any
	if len(page) > 0 {
		endCursor = gqlEncodeCursor(start + len(page) - 1)
	}
	return gqlConnectionObj(kind, page, gqlPageInfoObj(hasNext, endCursor), len(nodes))
}

func (e *gqlExecutor) issueField(obj *gqlObj, name string) any {
	iss := obj.issue
	switch name {
	case "id":
		if v, ok := issueJSON(e.s, obj.repo, iss)["node_id"]; ok {
			return v
		}
		return iss.NodeID
	case "number":
		return iss.Number
	case "title":
		return iss.Title
	case "body":
		return iss.Body
	case "state":
		if iss.State == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "databaseId":
		return iss.Number
	case "createdAt":
		return iss.CreatedAt
	case "updatedAt":
		return iss.UpdatedAt
	case "closedAt":
		// The Go store carries no closedAt; closed issues report null here.
		return nil
	case "author":
		return gqlUserObj()
	case "url":
		return "https://example.invalid/" + obj.repo.Owner + "/" + obj.repo.Name + "/issues/" + strconv.Itoa(iss.Number)
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) pullRequestField(obj *gqlObj, name string, args map[string]any) any {
	repo := obj.repo
	pr := obj.pr
	switch name {
	case "id":
		if v, ok := prJSON(e.s, repo, pr)["node_id"]; ok {
			return v
		}
		return pr.NodeID
	case "number":
		return pr.Number
	case "title":
		return pr.Title
	case "body":
		return pr.Body
	case "state":
		if pr.State == "open" {
			return "OPEN"
		}
		if pr.Merged {
			return "MERGED"
		}
		return "CLOSED"
	case "headRefName":
		return pr.Head.Ref
	case "baseRefName":
		return pr.Base.Ref
	case "mergeable":
		if pr.Merged || pr.State != "open" {
			return "UNKNOWN"
		}
		if PRConflict(repo, pr) {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "merged":
		return pr.Merged
	case "mergeCommit":
		// The Go store keeps no merge-commit sha; merged PRs report null.
		return nil
	case "files":
		files := e.prFiles(repo, pr)
		nodes := make([]*gqlObj, 0, len(files))
		for _, f := range files {
			nodes = append(nodes, gqlFileObj(f.Path))
		}
		return e.paginate(nodes, args, "PullRequestFileConnection")
	case "databaseId":
		return pr.Number
	case "createdAt":
		return FIXED_ISO
	case "updatedAt":
		return FIXED_ISO
	case "closedAt":
		if pr.MergedAt != "" {
			return pr.MergedAt
		}
		return nil
	case "author":
		return gqlUserObj()
	case "url":
		return "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pr.Number)
	}
	return gqlUndefined{}
}

// prFiles diffs the PR's head against its merge-base with the base branch
// (falling back to the base tip when there is no merge-base).
func (e *gqlExecutor) prFiles(repo *Repo, pr *Pull) []TreeDiff {
	headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
	baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
	if headTip == "" || baseTip == "" {
		return nil
	}
	base := MergeBase(repo, headTip, baseTip)
	if base == "" {
		base = baseTip
	}
	baseTree := ""
	if obj := repo.Objects[base]; obj != nil && obj.Type == "commit" {
		if c, err := ParseCommit(obj.Data); err == nil {
			baseTree = c.Tree
		}
	}
	headTree := ""
	if obj := repo.Objects[headTip]; obj != nil && obj.Type == "commit" {
		if c, err := ParseCommit(obj.Data); err == nil {
			headTree = c.Tree
		}
	}
	return DiffTrees(repo, baseTree, headTree)
}

func (e *gqlExecutor) refField(obj *gqlObj, name string) any {
	switch name {
	case "name":
		return obj.refName
	case "prefix":
		return "refs/heads/"
	case "target":
		tip := RefCommit(obj.repo, "refs/heads/"+obj.refName)
		if tip == "" {
			return nil
		}
		obj2 := obj.repo.Objects[tip]
		if obj2 == nil || obj2.Type != "commit" {
			return nil
		}
		c, err := ParseCommit(obj2.Data)
		if err != nil {
			return nil
		}
		return &gqlObj{typename: "Commit", commitOID: tip, commitMsg: c.Message}
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) connectionField(obj *gqlObj, name string) any {
	switch name {
	case "nodes":
		return obj.nodes
	case "edges":
		edges := make([]*gqlObj, 0, len(obj.nodes))
		edgeType := strings.Replace(obj.typename, "Connection", "Edge", 1)
		for i, node := range obj.nodes {
			edges = append(edges, &gqlObj{typename: edgeType, edgeNode: node, cursor: gqlEncodeCursor(i)})
		}
		return edges
	case "pageInfo":
		return obj.pageInfo
	case "totalCount":
		return obj.totalCount
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) edgeField(obj *gqlObj, name string) any {
	switch name {
	case "node":
		return obj.edgeNode
	case "cursor":
		return obj.cursor
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) commentField(obj *gqlObj, name string) any {
	c := obj.comment
	switch name {
	case "id":
		return gqlCompoundID("IssueComment", strconv.Itoa(c.RestID))
	case "body":
		return c.Body
	case "author":
		return gqlUserObj()
	case "createdAt":
		return c.CreatedAt
	case "updatedAt":
		return c.UpdatedAt
	case "url":
		return "https://example.invalid/" + obj.repo.Owner + "/" + obj.repo.Name + "/issues/" + strconv.Itoa(obj.issue.Number) + "#issuecomment-" + strconv.Itoa(c.RestID)
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) payloadField(obj *gqlObj, name, key string) any {
	if name == key {
		return obj.payload
	}
	if name == "clientMutationId" {
		return obj.clientMutationId
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) introspectionTypeField(obj *gqlObj, name string) any {
	typeName := obj.typeName
	switch name {
	case "kind":
		if _, ok := gqlEnumValues[typeName]; ok {
			return "ENUM"
		}
		return "OBJECT"
	case "name":
		return typeName
	case "description":
		return nil
	case "fields":
		fields := gqlTypeFields[typeName]
		out := make([]*gqlObj, 0, len(fields))
		for _, f := range fields {
			out = append(out, &gqlObj{typename: "__Field", fieldName: f, typeName: typeName})
		}
		return out
	case "enumValues":
		values := gqlEnumValues[typeName]
		out := make([]*gqlObj, 0, len(values))
		for _, v := range values {
			out = append(out, &gqlObj{typename: "__EnumValue", fieldName: v})
		}
		return out
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) introspectionFieldField(obj *gqlObj, name string) any {
	switch name {
	case "name":
		return obj.fieldName
	case "description":
		return nil
	case "args":
		return []*gqlObj{}
	case "type":
		return &gqlObj{typename: "__Type", typeName: "String"}
	case "isDeprecated":
		return false
	case "deprecationReason":
		return nil
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) introspectionEnumField(obj *gqlObj, name string) any {
	switch name {
	case "name":
		return obj.fieldName
	case "description":
		return nil
	case "isDeprecated":
		return false
	case "deprecationReason":
		return nil
	}
	return gqlUndefined{}
}

// ---------------------------------------------------------------------------
// Mutations (same store semantics as REST)
// ---------------------------------------------------------------------------

func (e *gqlExecutor) mutationField(name string, args map[string]any) any {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "createIssue":
		return e.createIssue(input)
	case "closeIssue":
		return e.closeIssue(input)
	case "createPullRequest":
		return e.createPullRequest(input)
	case "mergePullRequest":
		return e.mergePullRequest(input)
	case "addComment":
		return e.addComment(input)
	}
	return gqlUndefined{}
}

func (e *gqlExecutor) clientMutationID(input map[string]any) any {
	return input["clientMutationId"]
}

func (e *gqlExecutor) createIssue(input map[string]any) *gqlObj {
	repo := e.repoFromGlobalID(input["repositoryId"])
	title, _ := input["title"].(string)
	if repo == nil || title == "" {
		e.errf("Could not resolve to a Repository with the provided id.")
		return &gqlObj{typename: "CreateIssuePayload", clientMutationId: e.clientMutationID(input)}
	}
	body, _ := input["body"].(string)
	number := repo.NextNumber
	repo.NextNumber++
	iss := &Issue{
		Number:    number,
		RestID:    e.s.NextObjectID,
		NodeID:    gqlCompoundID("Issue", strconv.Itoa(repo.DbID), strconv.Itoa(number)),
		Title:     title,
		Body:      body,
		State:     "open",
		CreatedAt: FIXED_ISO,
		UpdatedAt: FIXED_ISO,
	}
	e.s.NextObjectID++
	repo.Issues[number] = iss
	TouchRepo(repo)
	return &gqlObj{typename: "CreateIssuePayload", payload: gqlIssueObj(repo, iss), clientMutationId: e.clientMutationID(input)}
}

func (e *gqlExecutor) closeIssue(input map[string]any) *gqlObj {
	gid, _ := input["issueId"].(string)
	repo, iss := gqlFindIssueByGlobalID(e.s, gid)
	if iss == nil {
		e.errf("Could not resolve to an Issue with the provided id.")
		return &gqlObj{typename: "CloseIssuePayload", clientMutationId: e.clientMutationID(input)}
	}
	iss.State = "closed"
	iss.UpdatedAt = FIXED_ISO
	TouchRepo(repo)
	return &gqlObj{typename: "CloseIssuePayload", payload: gqlIssueObj(repo, iss), clientMutationId: e.clientMutationID(input)}
}

func (e *gqlExecutor) createPullRequest(input map[string]any) *gqlObj {
	repo := e.repoFromGlobalID(input["repositoryId"])
	if repo == nil {
		e.errf("Could not resolve to a Repository with the provided id.")
		return &gqlObj{typename: "CreatePullRequestPayload", clientMutationId: e.clientMutationID(input)}
	}
	head, _ := input["headRefName"].(string)
	base, _ := input["baseRefName"].(string)
	headTip := RefCommit(repo, "refs/heads/"+head)
	baseTip := RefCommit(repo, "refs/heads/"+base)
	if headTip == "" || baseTip == "" {
		e.errf("Head or base ref not found.")
		return &gqlObj{typename: "CreatePullRequestPayload", clientMutationId: e.clientMutationID(input)}
	}
	for _, existing := range repo.Pulls {
		if existing.State == "open" && existing.Head.Ref == head && existing.Base.Ref == base {
			e.errf("A pull request already exists for this head and base.")
			return &gqlObj{typename: "CreatePullRequestPayload", clientMutationId: e.clientMutationID(input)}
		}
	}
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	number := repo.NextNumber
	repo.NextNumber++
	pr := &Pull{
		Number: number,
		RestID: e.s.NextObjectID,
		NodeID: gqlCompoundID("PullRequest", strconv.Itoa(repo.DbID), strconv.Itoa(number)),
		Title:  title,
		Body:   body,
		State:  "open",
		Head:   Ref{Ref: head, SHA: headTip},
		Base:   Ref{Ref: base, SHA: baseTip},
		Merged: false,
	}
	e.s.NextObjectID++
	repo.Pulls[number] = pr
	TouchRepo(repo)
	return &gqlObj{typename: "CreatePullRequestPayload", payload: gqlPrObj(repo, pr), clientMutationId: e.clientMutationID(input)}
}

func (e *gqlExecutor) mergePullRequest(input map[string]any) *gqlObj {
	gid, _ := input["pullRequestId"].(string)
	repo, pr := gqlFindPrByGlobalID(e.s, gid)
	if pr == nil {
		e.errf("Could not resolve to a PullRequest with the provided id.")
		return &gqlObj{typename: "MergePullRequestPayload", clientMutationId: e.clientMutationID(input)}
	}
	if pr.Merged || pr.State != "open" {
		e.errf("Pull request is already merged or closed.")
		return &gqlObj{typename: "MergePullRequestPayload", payload: gqlPrObj(repo, pr), clientMutationId: e.clientMutationID(input)}
	}
	if PRConflict(repo, pr) {
		e.errf("Merge conflict.")
		return &gqlObj{typename: "MergePullRequestPayload", payload: gqlPrObj(repo, pr), clientMutationId: e.clientMutationID(input)}
	}
	headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
	baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
	headTree := ""
	if obj := repo.Objects[headTip]; obj != nil && obj.Type == "commit" {
		if c, err := ParseCommit(obj.Data); err == nil {
			headTree = c.Tree
		}
	}
	mergeSha := PutObject(repo, "commit", BuildCommit(Commit{
		Tree:    headTree,
		Parents: []string{baseTip},
		Message: "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.Head.Ref + "\n\n" + pr.Title + "\n",
	}))
	repo.Refs["refs/heads/"+pr.Base.Ref] = mergeSha
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = FIXED_ISO
	TouchRepo(repo)
	return &gqlObj{typename: "MergePullRequestPayload", payload: gqlPrObj(repo, pr), clientMutationId: e.clientMutationID(input)}
}

func (e *gqlExecutor) addComment(input map[string]any) *gqlObj {
	gid, _ := input["subjectId"].(string)
	body, _ := input["body"].(string)
	var repo *Repo
	var iss *Issue
	if r, i := gqlFindIssueByGlobalID(e.s, gid); i != nil {
		repo, iss = r, i
	} else if r, pr := gqlFindPrByGlobalID(e.s, gid); pr != nil {
		// Comments on a pull request land on its issue thread (shared number).
		repo = r
		iss = r.Issues[pr.Number]
	}
	if repo == nil || iss == nil || body == "" {
		e.errf("Could not resolve to a commentable subject with the provided id.")
		return &gqlObj{typename: "AddCommentPayload", clientMutationId: e.clientMutationID(input)}
	}
	comment := &Comment{
		RestID:    e.s.NextObjectID,
		Body:      body,
		CreatedAt: FIXED_ISO,
		UpdatedAt: FIXED_ISO,
	}
	e.s.NextObjectID++
	iss.Comments = append(iss.Comments, comment)
	iss.UpdatedAt = FIXED_ISO
	TouchRepo(repo)
	edge := &gqlObj{
		typename: "IssueCommentEdge",
		edgeNode: gqlCommentObj(repo, iss, comment),
		cursor:   gqlEncodeCursor(len(iss.Comments) - 1),
	}
	return &gqlObj{typename: "AddCommentPayload", payload: edge, payloadKey: "commentEdge", clientMutationId: e.clientMutationID(input)}
}

// repoFromGlobalID resolves a Repository global id to its repo, or nil.
// Accepts base64("Repository:dbId") and, leniently, a bare numeric id.
func (e *gqlExecutor) repoFromGlobalID(v any) *Repo {
	gid, _ := v.(string)
	if gid == "" {
		return nil
	}
	kind, rest, ok := gqlDecodeCompound(gid)
	if !ok || kind != "Repository" || len(rest) < 1 {
		if n, ok := gqlAtoi(gid); ok {
			return gqlFindRepoByDbID(e.s, n)
		}
		return nil
	}
	dbID, ok := gqlAtoi(rest[0])
	if !ok {
		return nil
	}
	return gqlFindRepoByDbID(e.s, dbID)
}

// ---------------------------------------------------------------------------
// HTTP entry point (called from server.go after auth)
// ---------------------------------------------------------------------------

// handleGraphQL serves POST /api/graphql. The router holds the store lock
// for the whole request, so this handler never locks.
func handleGraphQL(c *reqCtx) {
	if !authed(c.r) {
		c.jsonErr(401, "Unauthorized")
		return
	}
	if len(c.body) == 0 {
		c.json(400, map[string]any{"errors": []map[string]any{{"message": "GraphQL request body must be an object"}}})
		return
	}
	trimmed := strings.TrimSpace(string(c.body))
	if strings.HasPrefix(trimmed, "[") {
		var ops []map[string]any
		if err := json.Unmarshal(c.body, &ops); err != nil {
			c.json(400, map[string]any{"errors": []map[string]any{{"message": "Malformed JSON body"}}})
			return
		}
		results := make([]map[string]any, 0, len(ops))
		for _, op := range ops {
			results = append(results, executeGraphQLOperation(c.s, op))
		}
		c.json(200, results)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(c.body, &body); err != nil {
		c.json(400, map[string]any{"errors": []map[string]any{{"message": "Malformed JSON body"}}})
		return
	}
	c.json(200, executeGraphQLOperation(c.s, body))
}

func executeGraphQLOperation(s *Store, operation map[string]any) map[string]any {
	query, _ := operation["query"].(string)
	if query == "" {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "Request must include a query string"}}}
	}
	tokens, err := gqlTokenize(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "GraphQL syntax error: " + err.Error()}}}
	}
	doc, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "GraphQL syntax error: " + err.Error()}}}
	}
	vars := map[string]any{}
	if v, ok := operation["variables"].(map[string]any); ok {
		vars = v
	}
	operationName, _ := operation["operationName"].(string)
	return (&gqlExecutor{s: s}).run(doc, vars, operationName)
}

// gqlArgInt converts a JSON-decoded argument value to int (0 when absent).
func gqlArgInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}
