// mcp.go — Model Context Protocol endpoint (Streamable HTTP, JSON-RPC 2.0).
//
// Speaks revision 2026-07-28 (server/discover, no handshake, no session)
// plus the legacy handshake revisions (2025-11-25 and earlier) through
// initialize + notifications/initialized. Ported from the Node reference
// (mcp.ts) into idiomatic Go, stdlib only. Every tool acts on the same
// in-memory store as REST, so ids and refs stay stable across interfaces.
//
// Auth is decided before dispatch by the router; handleMcp still returns
// 401 when a request somehow arrives unauthenticated. Malformed JSON
// answers HTTP 400 with JSON-RPC -32700. Unknown tool answers -32602.
// Missing required arguments answer as a tool result with isError: true.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const mcpServerName = FORGE_NAME
const mcpServerVersion = FORGE_VERSION

// mcpSupportedRevisions are the protocol revisions this server speaks,
// newest first. 2026-07-28 is the current revision; the rest are the
// handshake-era revisions the suite negotiates down to.
var mcpSupportedRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpRevisionSupported(rev string) bool {
	for _, r := range mcpSupportedRevisions {
		if r == rev {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

// mcpTool is one tool as tools/list answers it. Struct encoding keeps the
// field order (and therefore the tools/list bytes) identical on every call.
type mcpTool struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	InputSchema mcpToolInputSchema `json:"inputSchema"`
}

type mcpToolInputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required,omitempty"`
}

func mcpStrProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func mcpBoolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func mcpIntProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// mcpTools is the tool list, in a fixed order. Every tool has a non-empty
// description and an inputSchema of type object; the required properties
// are exactly the arguments the suite can fill for that tool.
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository under the authenticated user.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"name":        mcpStrProp("Repository name"),
				"description": mcpStrProp("Repository description"),
				"private":     mcpBoolProp("Whether the repository is private"),
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":     mcpStrProp("Repository owner login"),
				"repo":      mcpStrProp("Repository name"),
				"full_name": mcpStrProp("Repository full name \"owner/repo\""),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":     mcpStrProp("Repository owner login"),
				"repo":      mcpStrProp("Repository name"),
				"full_name": mcpStrProp("Repository full name \"owner/repo\""),
				"title":     mcpStrProp("Issue title"),
				"body":      mcpStrProp("Issue body"),
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":     mcpStrProp("Repository owner login"),
				"repo":      mcpStrProp("Repository name"),
				"full_name": mcpStrProp("Repository full name \"owner/repo\""),
				"state":     map[string]any{"type": "string", "description": "Issue state filter: open, closed, or all", "enum": []string{"open", "closed", "all"}},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":     mcpStrProp("Repository owner login"),
				"repo":      mcpStrProp("Repository name"),
				"full_name": mcpStrProp("Repository full name \"owner/repo\""),
				"title":     mcpStrProp("Pull request title"),
				"head":      mcpStrProp("Head branch name"),
				"base":      mcpStrProp("Base branch name"),
				"body":      mcpStrProp("Pull request body"),
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":       mcpStrProp("Repository owner login"),
				"repo":        mcpStrProp("Repository name"),
				"full_name":   mcpStrProp("Repository full name \"owner/repo\""),
				"pull_number": mcpIntProp("Pull request number"),
				"number":      mcpIntProp("Pull request number (alias)"),
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "list_pull_requests",
		Description: "List pull requests in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":     mcpStrProp("Repository owner login"),
				"repo":      mcpStrProp("Repository name"),
				"full_name": mcpStrProp("Repository full name \"owner/repo\""),
				"state":     map[string]any{"type": "string", "description": "Pull request state filter: open, closed, or all", "enum": []string{"open", "closed", "all"}},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "get_issue",
		Description: "Get an issue by number in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner":        mcpStrProp("Repository owner login"),
				"repo":         mcpStrProp("Repository name"),
				"full_name":    mcpStrProp("Repository full name \"owner/repo\""),
				"issue_number": mcpIntProp("Issue number"),
				"number":       mcpIntProp("Issue number (alias)"),
			},
			Required: []string{"owner", "repo", "issue_number"},
		},
	},
}

func mcpFindTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	for i := range mcpTools {
		if strings.EqualFold(mcpTools[i].Name, name) {
			return &mcpTools[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON-RPC replies
// ---------------------------------------------------------------------------

func mcpReply(c *reqCtx, id any, result any) {
	c.json(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func mcpReplyError(c *reqCtx, status int, id any, code int, message string) {
	c.json(status, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func mcpReplyErrorData(c *reqCtx, status int, id any, code int, message string, data any) {
	c.json(status, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message, "data": data},
	})
}

// mcpToolResult wraps a payload as a successful tool result: a text block
// carrying the JSON plus the structured content itself.
func mcpToolResult(payload any) map[string]any {
	b, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(b)}},
		"structuredContent": payload,
	}
}

// mcpToolError is a tool execution failure: isError true, never a
// JSON-RPC error, so a missing argument does not fail the whole call.
func mcpToolError(message string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	}
}

// ---------------------------------------------------------------------------
// argument helpers
// ---------------------------------------------------------------------------

// mcpPickStr returns the first named argument that is a non-empty string.
func mcpPickStr(args map[string]any, names ...string) string {
	for _, name := range names {
		if v, ok := args[name]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// mcpPickNum returns the first named argument as an integer, accepting a
// JSON number, a numeric string, or a json.Number.
func mcpPickNum(args map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		v, ok := args[name]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n == float64(int(n)) {
				return int(n), true
			}
		case int:
			return n, true
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return i, true
			}
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i), true
			}
		}
	}
	return 0, false
}

// mcpSplitFullName resolves owner/repo out of the arguments. The suite
// fills a full_name property with "owner/repo" when it calls tools, so
// that wins; otherwise separate owner/repo arguments are used.
func mcpSplitFullName(args map[string]any) (owner, repo string) {
	if full := mcpPickStr(args, "full_name", "fullName", "repository_full_name"); full != "" {
		if i := strings.IndexByte(full, '/'); i >= 0 {
			return full[:i], full[i+1:]
		}
	}
	owner = mcpPickStr(args, "owner", "owner_login", "org", "organization", "user", "user_login", "login")
	repo = mcpPickStr(args, "repo", "repository", "name")
	return owner, repo
}

// mcpGlobalID builds a global id in the Node reference's exact form:
// base64("Kind:dbId:number"). The store's GlobalID takes a single int,
// so the two-part form is encoded here directly.
func mcpGlobalID(kind string, dbID, number int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(dbID) + ":" + strconv.Itoa(number)))
}

// mcpRequestedRevision reads the protocol revision the request declares,
// from params._meta (either spelling) or a top-level protocolVersion.
func mcpRequestedRevision(req map[string]any) string {
	if params, ok := req["params"].(map[string]any); ok {
		if meta, ok := params["_meta"].(map[string]any); ok {
			if v, ok := meta["io.modelcontextprotocol/protocolVersion"].(string); ok && v != "" {
				return v
			}
			if v, ok := meta["protocolVersion"].(string); ok && v != "" {
				return v
			}
		}
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// tool implementations
// ---------------------------------------------------------------------------

func mcpCallTool(c *reqCtx, name string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "create_repo":
		repoName := mcpPickStr(args, "name", "repo")
		if repoName == "" {
			return mcpToolError("Missing required argument: name")
		}
		if _, exists := c.s.Repos[repoName]; exists {
			return mcpToolError(fmt.Sprintf("Repository '%s' already exists", repoName))
		}
		description := mcpPickStr(args, "description")
		private, _ := args["private"].(bool)
		repo := NewRepo(c.s, repoName, RepoOptions{Description: description, Private: private})
		return mcpToolResult(repoJSON(c.s, repo))

	case "get_repo":
		owner, repoName := mcpSplitFullName(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		return mcpToolResult(repoJSON(c.s, repo))

	case "create_issue":
		owner, repoName := mcpSplitFullName(args)
		title := mcpPickStr(args, "title")
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		if title == "" {
			return mcpToolError("Missing required argument: title")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		number := repo.NextNumber
		repo.NextNumber++
		restID := c.s.NextObjectID
		c.s.NextObjectID++
		issue := &Issue{
			Number:    number,
			Title:     title,
			Body:      mcpPickStr(args, "body"),
			State:     "open",
			Labels:    []string{},
			Comments:  []*Comment{},
			RestID:    restID,
			NodeID:    mcpGlobalID("Issue", repo.DbID, number),
			CreatedAt: FIXED_ISO,
			UpdatedAt: FIXED_ISO,
		}
		repo.Issues[number] = issue
		TouchRepo(repo)
		return mcpToolResult(issueJSON(c.s, repo, issue))

	case "list_issues":
		owner, repoName := mcpSplitFullName(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		state := mcpPickStr(args, "state")
		if state == "" {
			state = "open"
		}
		var list []*Issue
		for _, issue := range repo.Issues {
			switch state {
			case "open":
				if issue.State != "open" {
					continue
				}
			case "closed":
				if issue.State != "closed" {
					continue
				}
			}
			list = append(list, issue)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
		out := make([]map[string]any, 0, len(list))
		for _, issue := range list {
			out = append(out, issueJSON(c.s, repo, issue))
		}
		return mcpToolResult(map[string]any{"issues": out})

	case "create_pull_request":
		owner, repoName := mcpSplitFullName(args)
		title := mcpPickStr(args, "title")
		head := mcpPickStr(args, "head")
		base := mcpPickStr(args, "base")
		if base == "" {
			base = "main"
		}
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		if title == "" {
			return mcpToolError("Missing required argument: title")
		}
		if head == "" {
			return mcpToolError("Missing required argument: head")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		headTip := RefCommit(repo, "refs/heads/"+head)
		if headTip == "" {
			return mcpToolError(fmt.Sprintf("Head branch '%s' not found", head))
		}
		baseTip := RefCommit(repo, "refs/heads/"+base)
		if baseTip == "" {
			return mcpToolError(fmt.Sprintf("Base branch '%s' not found", base))
		}
		for _, existing := range repo.Pulls {
			if existing.State == "open" && existing.Head.Ref == head && existing.Base.Ref == base {
				return mcpToolError("A pull request already exists for this head and base")
			}
		}
		number := repo.NextNumber
		repo.NextNumber++
		restID := c.s.NextObjectID
		c.s.NextObjectID++
		pr := &Pull{
			Number:   number,
			Title:    title,
			Body:     mcpPickStr(args, "body"),
			State:    "open",
			Head:     Ref{Ref: head, SHA: headTip},
			Base:     Ref{Ref: base, SHA: baseTip},
			Merged:   false,
			MergedAt: "",
			RestID:   restID,
			NodeID:   mcpGlobalID("PullRequest", repo.DbID, number),
		}
		repo.Pulls[number] = pr
		TouchRepo(repo)
		return mcpToolResult(prJSON(c.s, repo, pr))

	case "merge_pull_request":
		owner, repoName := mcpSplitFullName(args)
		pullNumber, ok := mcpPickNum(args, "pull_number", "pullNumber", "number")
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		if !ok || pullNumber <= 0 {
			return mcpToolError("Missing required argument: pull_number")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		pr := repo.Pulls[pullNumber]
		if pr == nil {
			return mcpToolError(fmt.Sprintf("Pull request #%d not found", pullNumber))
		}
		if pr.Merged || pr.State != "open" {
			return mcpToolError("Pull request is already merged or closed")
		}
		if PRConflict(repo, pr) {
			return mcpToolError("Merge conflict")
		}
		headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
		baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
		if headTip == "" || baseTip == "" {
			return mcpToolError("Head or base branch is missing")
		}
		headObj := repo.Objects[headTip]
		if headObj == nil || headObj.Type != "commit" {
			return mcpToolError("Head or base branch is missing")
		}
		headCommit, err := ParseCommit(headObj.Data)
		if err != nil {
			return mcpToolError("Head or base branch is missing")
		}
		mergeSha := PutObject(repo, "commit", BuildCommit(Commit{
			Tree:    headCommit.Tree,
			Parents: []string{baseTip},
			Message: fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", pr.Number, pr.Head.Ref, pr.Title),
		}))
		repo.Refs["refs/heads/"+pr.Base.Ref] = mergeSha
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = FIXED_ISO
		TouchRepo(repo)
		return mcpToolResult(map[string]any{
			"number":           pr.Number,
			"pull_number":      pr.Number,
			"merged":           true,
			"state":            "closed",
			"sha":              mergeSha,
			"merge_commit_sha": mergeSha,
		})

	case "list_pull_requests":
		owner, repoName := mcpSplitFullName(args)
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		state := mcpPickStr(args, "state")
		if state == "" {
			state = "open"
		}
		var list []*Pull
		for _, pr := range repo.Pulls {
			switch state {
			case "open":
				if pr.State != "open" {
					continue
				}
			case "closed":
				if pr.State != "closed" {
					continue
				}
			}
			list = append(list, pr)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
		out := make([]map[string]any, 0, len(list))
		for _, pr := range list {
			out = append(out, prJSON(c.s, repo, pr))
		}
		return mcpToolResult(map[string]any{"pull_requests": out})

	case "get_issue":
		owner, repoName := mcpSplitFullName(args)
		issueNumber, ok := mcpPickNum(args, "issue_number", "issueNumber", "number")
		if owner == "" || repoName == "" {
			return mcpToolError("Missing required arguments: owner and repo")
		}
		if !ok || issueNumber <= 0 {
			return mcpToolError("Missing required argument: issue_number")
		}
		repo := FindRepo(c.s, owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("Repository '%s/%s' not found", owner, repoName))
		}
		issue := repo.Issues[issueNumber]
		if issue == nil {
			return mcpToolError(fmt.Sprintf("Issue #%d not found", issueNumber))
		}
		return mcpToolResult(issueJSON(c.s, repo, issue))
	}
	return nil // unreachable: the dispatcher rejects unknown tools first
}

// ---------------------------------------------------------------------------
// dispatcher
// ---------------------------------------------------------------------------

// handleMcp serves POST /mcp (and DELETE /mcp) as JSON-RPC 2.0 over
// Streamable HTTP, answering application/json. The router holds the store
// lock for the whole request, so this handler never locks the store.
func handleMcp(c *reqCtx) {
	// A legacy session teardown: no body, no reply content.
	if c.r.Method == "DELETE" {
		c.empty(204)
		return
	}
	if c.r.Method != "POST" {
		c.jsonErr(405, "Method Not Allowed")
		return
	}
	if !authed(c.r) {
		mcpReplyError(c, 401, nil, -32000, "Unauthorized")
		return
	}
	// Malformed JSON (or a body that is not a JSON object) answers
	// HTTP 400 with JSON-RPC -32700.
	var req map[string]any
	if len(c.body) == 0 || json.Unmarshal(c.body, &req) != nil || req == nil {
		mcpReplyError(c, 400, nil, -32700, "Parse error")
		return
	}
	var id any
	if v, ok := req["id"]; ok {
		id = v
	}
	method, _ := req["method"].(string)

	// Notifications carry no id and get no reply body.
	if id == nil {
		c.empty(202)
		return
	}
	if method == "" {
		mcpReplyError(c, 200, id, -32600, "Invalid Request")
		return
	}

	serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
	reply := func(result any) { mcpReply(c, id, result) }
	replyError := func(code int, message string) { mcpReplyError(c, 200, id, code, message) }

	switch method {
	case "server/discover":
		// A revision this server does not speak is refused with the
		// versions it does support, so the client can retry with one.
		if rev := mcpRequestedRevision(req); rev != "" && !mcpRevisionSupported(rev) {
			mcpReplyErrorData(c, 200, id, -32022, "Unsupported protocol version",
				map[string]any{"supported": mcpSupportedRevisions})
			return
		}
		reply(map[string]any{
			"supportedVersions": mcpSupportedRevisions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		})

	case "initialize":
		// Echo the requested revision when it is one this server speaks,
		// else negotiate the newest handshake revision.
		negotiated := "2025-11-25"
		if rev := mcpRequestedRevision(req); mcpRevisionSupported(rev) {
			negotiated = rev
		}
		reply(map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo,
		})

	case "ping":
		reply(map[string]any{})

	case "tools/list":
		reply(map[string]any{"tools": mcpTools})

	case "tools/call":
		params, _ := req["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		name, _ := params["name"].(string)
		if name == "" {
			replyError(-32602, "Missing required parameter: name")
			return
		}
		tool := mcpFindTool(name)
		if tool == nil {
			replyError(-32602, fmt.Sprintf("Unknown tool: %s", name))
			return
		}
		args, _ := params["arguments"].(map[string]any)
		reply(mcpCallTool(c, tool.Name, args))

	case "resources/list":
		reply(map[string]any{"resources": []any{}})

	case "prompts/list":
		reply(map[string]any{"prompts": []any{}})

	default:
		replyError(-32601, fmt.Sprintf("Method not found: %s", method))
	}
}
