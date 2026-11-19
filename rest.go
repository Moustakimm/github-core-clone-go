// rest.go — GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests, over the in-memory store.
//
// Ported from the Node reference (rest.ts) into idiomatic Go, stdlib only.
// Every reply is application/json with a non-empty X-GitHub-Request-Id (the
// reqCtx helpers add both); errors carry {message}. Auth is decided here:
// the API root and /meta are open, everything else needs the bearer token.
// The router holds the store lock for the whole request, so this handler
// never locks the store itself.
//
// The Go store keeps a few fields the Node store carries inline out of the
// structs (label rest ids, comment sequence numbers, a pull's merge commit
// sha), so this file keeps small side tables for them, keyed by the stable
// object pointers the store hands out. A cached serialized reply is served
// from Repo.JSONCache while Repo.Rev still matches the revision it was built
// at; every mutation bumps Rev through TouchRepo, so a stale reply is never
// served. That makes the read-hot paths (one issue read thousands of times)
// pay the serialization once.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// side tables for fields the store structs do not carry
// ---------------------------------------------------------------------------

// labelRestIDs is the sequential REST id assigned to each label at creation
// (the Node store's label.restId, drawn from the store's object counter).
var labelRestIDs = map[*Label]int{}

// commentSeqs is the per-repository comment sequence number assigned to each
// comment at creation (the Node store's repo.nextCommentId).
var commentSeqs = map[*Comment]int{}

// mergeCommitShas records the squash commit sha each merged pull request
// produced (the Node store's pr.mergeCommitSha).
var mergeCommitShas = map[*Pull]string{}

// jsonCacheRevs records the repository revision each cached serialized reply
// in Repo.JSONCache was built at, so a hit can be checked against Repo.Rev.
var jsonCacheRevs = map[*Repo]map[string]int64{}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// globalIDStr returns base64("Kind:id") for a composite id, matching the
// Node store's globalId(kind, id) when id is a string such as "3:5".
func globalIDStr(kind, id string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + id))
}

// encodeURIComponent escapes a string the way JavaScript's
// encodeURIComponent does (unreserved characters pass through, everything
// else becomes %XX).
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// pathSegments splits a request path into its non-empty segments.
func pathSegments(p string) []string {
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parsePage reads per_page/page the way the Node reference does: per_page
// defaults to 30 and is capped at 100, page defaults to 1.
func parsePage(q url.Values) (perPage, page int) {
	perPage = 30
	if raw := q.Get("per_page"); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
			perPage = int(f)
			if perPage > 100 {
				perPage = 100
			}
			if perPage < 1 {
				perPage = 30
			}
		}
	}
	page = 1
	if raw := q.Get("page"); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
			page = int(f)
			if page < 1 {
				page = 1
			}
		}
	}
	return perPage, page
}

// paginate slices a list to the requested page and, when more pages follow,
// sets the Link header with rel="next" and rel="last".
func paginate[T any](c *reqCtx, list []T) []T {
	perPage, page := parsePage(c.r.URL.Query())
	start := (page - 1) * perPage
	if start >= len(list) {
		return []T{}
	}
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}
	if start+perPage < len(list) {
		origin := "http://" + c.r.Host
		next := fmt.Sprintf("%s%s?per_page=%d&page=%d", origin, c.r.URL.Path, perPage, page+1)
		lastPage := (len(list) + perPage - 1) / perPage
		last := fmt.Sprintf("%s%s?per_page=%d&page=%d", origin, c.r.URL.Path, perPage, lastPage)
		c.header("Link", fmt.Sprintf("<%s>; rel=\"next\", <%s>; rel=\"last\"", next, last))
	}
	return list[start:end]
}

// cachedJSON serves a serialized reply from the repository's cache when the
// revision it was built at still matches, and stores it there on a miss.
func cachedJSON(c *reqCtx, repo *Repo, key string, build func() map[string]any) {
	if revs, ok := jsonCacheRevs[repo]; ok {
		if rev, ok := revs[key]; ok && rev == repo.Rev {
			if buf, ok := repo.JSONCache[key]; ok {
				c.raw(200, buf, "application/json")
				return
			}
		}
	}
	b, err := json.Marshal(build())
	if err != nil {
		c.jsonErr(500, "Internal Server Error")
		return
	}
	if repo.JSONCache == nil {
		repo.JSONCache = make(map[string][]byte)
	}
	repo.JSONCache[key] = b
	if jsonCacheRevs[repo] == nil {
		jsonCacheRevs[repo] = make(map[string]int64)
	}
	jsonCacheRevs[repo][key] = repo.Rev
	c.raw(200, b, "application/json")
}

// readBodyObject parses the request body as a JSON object. An empty body is
// {}; a malformed body, or a JSON value that is not an object, is a 400.
func readBodyObject(c *reqCtx) (map[string]any, bool) {
	if len(c.body) == 0 {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(c.body, &v); err != nil {
		c.jsonErr(400, "Malformed JSON body")
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		c.jsonErr(400, "Malformed JSON body")
		return nil, false
	}
	return obj, true
}

// bodyString returns the body's field as a string, or "" when it is absent
// or not a string.
func bodyString(body map[string]any, field string) string {
	if s, ok := body[field].(string); ok {
		return s
	}
	return ""
}

// bodyBool returns the body's field as a bool, reporting whether it was one.
func bodyBool(body map[string]any, field string) (bool, bool) {
	if b, ok := body[field].(bool); ok {
		return b, true
	}
	return false, false
}

// needRepo looks a repository up and answers 404 JSON when it is absent (a
// repository under another owner is 404, never 403).
func needRepo(c *reqCtx, owner, name string) *Repo {
	repo := FindRepo(c.s, owner, name)
	if repo == nil {
		c.jsonErr(404, "Not Found")
		return nil
	}
	return repo
}

// decodeBase64Loose decodes base64 the way the Node reference does: it
// strips whitespace first and tolerates a missing padding.
func decodeBase64Loose(s string) ([]byte, error) {
	clean := strings.NewReplacer("\r", "", "\n", "", "\t", "", " ", "").Replace(s)
	if b, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return b, nil
	}
	if m := len(clean) % 4; m != 0 {
		clean += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(clean)
}

// treeOIDAtPath walks a tree to the subtree at path, or "" when it is absent.
func treeOIDAtPath(repo *Repo, treeOID, path string) string {
	current := treeOID
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		obj := repo.Objects[current]
		if obj == nil || obj.Type != "tree" {
			return ""
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return ""
		}
		found := ""
		for _, e := range entries {
			if e.Name == segment {
				found = e.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		current = found
	}
	return current
}

// tipTree flattens the tree at a commit tip, or returns an empty map when
// the tip has no commit.
func tipTree(repo *Repo, tip string) map[string]FlatEntry {
	flat := make(map[string]FlatEntry)
	if tip == "" {
		return flat
	}
	obj := repo.Objects[tip]
	if obj == nil || obj.Type != "commit" {
		return flat
	}
	c, err := ParseCommit(obj.Data)
	if err != nil {
		return flat
	}
	return FlattenTree(repo, c.Tree, "", nil)
}

// ---------------------------------------------------------------------------
// serializers
// ---------------------------------------------------------------------------

func userJSON() map[string]any {
	return map[string]any{
		"login":       ViewerLogin,
		"id":          ViewerID,
		"node_id":     GlobalID("User", ViewerID),
		"avatar_url":  "",
		"gravatar_id": "",
		"url":         "/api/v3/users/" + ViewerLogin,
		"html_url":    "https://example.invalid/" + ViewerLogin,
		"type":        "User",
		"site_admin":  false,
		"name":        ViewerName,
		"email":       ViewerEmail,
	}
}

func repoJSON(s *Store, r *Repo) map[string]any {
	openIssues := 0
	for _, iss := range r.Issues {
		if iss.State == "open" {
			openIssues++
		}
	}
	for _, pr := range r.Pulls {
		if pr.State == "open" {
			openIssues++
		}
	}
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	return map[string]any{
		"id":                r.RestID,
		"node_id":           r.NodeID,
		"name":              r.Name,
		"full_name":         r.Owner + "/" + r.Name,
		"owner":             userJSON(),
		"private":           r.Private,
		"description":       r.Description,
		"fork":              false,
		"created_at":        FIXED_ISO,
		"updated_at":        FIXED_ISO,
		"pushed_at":         FIXED_ISO,
		"homepage":          nil,
		"size":              0,
		"stargazers_count":  0,
		"watchers_count":    0,
		"language":          nil,
		"has_issues":        true,
		"has_projects":      true,
		"has_downloads":     true,
		"has_wiki":          true,
		"has_pages":         false,
		"has_discussions":   false,
		"forks_count":       0,
		"archived":          false,
		"disabled":          false,
		"open_issues_count": openIssues,
		"license":           nil,
		"allow_forking":     true,
		"is_template":       false,
		"topics":            []any{},
		"visibility":        visibility,
		"default_branch":    r.DefaultBranch,
		"permissions":       map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
		"html_url":          "https://example.invalid/" + r.Owner + "/" + r.Name,
		"url":               "/api/v3/repos/" + r.Owner + "/" + r.Name,
	}
}

func labelJSON(repo *Repo, l *Label) map[string]any {
	id := labelRestIDs[l]
	return map[string]any{
		"id":          id,
		"node_id":     globalIDStr("Label", fmt.Sprintf("%d:%s", repo.DbID, l.Name)),
		"name":        l.Name,
		"color":       l.Color,
		"description": "",
		"default":     false,
		"url":         fmt.Sprintf("/api/v3/labels/%d", id),
	}
}

func issueJSON(s *Store, r *Repo, iss *Issue) map[string]any {
	labels := make([]map[string]any, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		if l, ok := r.Labels[name]; ok {
			labels = append(labels, labelJSON(r, l))
		}
	}
	var closedAt any
	if iss.State == "closed" {
		closedAt = FIXED_ISO
	}
	base := "/api/v3/repos/" + r.Owner + "/" + r.Name
	return map[string]any{
		"id":                 iss.RestID,
		"node_id":            iss.NodeID,
		"number":             iss.Number,
		"title":              iss.Title,
		"body":               iss.Body,
		"state":              iss.State,
		"locked":             false,
		"user":               userJSON(),
		"labels":             labels,
		"assignee":           nil,
		"assignees":          []any{},
		"milestone":          nil,
		"comments":           len(iss.Comments),
		"created_at":         iss.CreatedAt,
		"updated_at":         iss.UpdatedAt,
		"closed_at":          closedAt,
		"author_association": "OWNER",
		"html_url":           fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", r.Owner, r.Name, iss.Number),
		"url":                fmt.Sprintf("%s/issues/%d", base, iss.Number),
		"repository_url":     base,
		"labels_url":         fmt.Sprintf("%s/issues/%d/labels{/name}", base, iss.Number),
		"comments_url":       fmt.Sprintf("%s/issues/%d/comments", base, iss.Number),
		"events_url":         fmt.Sprintf("%s/issues/%d/events", base, iss.Number),
	}
}

func commentJSON(s *Store, repo *Repo, iss *Issue, cm *Comment) map[string]any {
	seq := commentSeqs[cm]
	base := "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return map[string]any{
		"id":                 cm.RestID,
		"node_id":            globalIDStr("IssueComment", fmt.Sprintf("%d:%d:%d", repo.DbID, iss.Number, seq)),
		"body":               cm.Body,
		"user":               userJSON(),
		"created_at":         cm.CreatedAt,
		"updated_at":         cm.UpdatedAt,
		"html_url":           fmt.Sprintf("https://example.invalid/%s/%s/issues/%d#issuecomment-%d", repo.Owner, repo.Name, iss.Number, cm.RestID),
		"url":                fmt.Sprintf("%s/issues/%d/comments/%d", base, iss.Number, cm.RestID),
		"issue_url":          fmt.Sprintf("%s/issues/%d", base, iss.Number),
		"author_association": "OWNER",
	}
}

// prDiff returns the file diff of a pull request: the head's tree against
// the merge-base tree (falling back to the base tip at creation when the two
// branches share no history), or nil when either branch is missing.
func prDiff(repo *Repo, pr *Pull) []TreeDiff {
	headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
	baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
	if headTip == "" || baseTip == "" {
		return nil
	}
	base := MergeBase(repo, headTip, baseTip)
	if base == "" {
		base = pr.Base.SHA
	}
	var baseTree string
	if base != "" {
		if obj := repo.Objects[base]; obj != nil && obj.Type == "commit" {
			if cm, err := ParseCommit(obj.Data); err == nil {
				baseTree = cm.Tree
			}
		}
	}
	var headTree string
	if obj := repo.Objects[headTip]; obj != nil && obj.Type == "commit" {
		if cm, err := ParseCommit(obj.Data); err == nil {
			headTree = cm.Tree
		}
	}
	return DiffTrees(repo, baseTree, headTree)
}

func prJSON(s *Store, r *Repo, pr *Pull) map[string]any {
	files := prDiff(r, pr)
	additions, deletions := 0, 0
	for _, f := range files {
		additions += f.Additions
		deletions += f.Deletions
	}
	headSha := RefCommit(r, "refs/heads/"+pr.Head.Ref)
	if headSha == "" {
		headSha = pr.Head.SHA
	}
	baseSha := RefCommit(r, "refs/heads/"+pr.Base.Ref)
	if baseSha == "" {
		baseSha = pr.Base.SHA
	}
	var closedAt, mergedAt, mergeCommitSha any
	if pr.State == "closed" {
		closedAt = FIXED_ISO
	}
	if pr.Merged {
		mergedAt = pr.MergedAt
		if mergedAt == "" {
			mergedAt = FIXED_ISO
		}
		if sha, ok := mergeCommitShas[pr]; ok {
			mergeCommitSha = sha
		}
	}
	mergeable := false
	mergeableState := "unknown"
	if !pr.Merged {
		conflict := PRConflict(r, pr)
		mergeable = !conflict
		if conflict {
			mergeableState = "conflicting"
		} else {
			mergeableState = "clean"
		}
	}
	var mergedBy any
	if pr.Merged {
		mergedBy = userJSON()
	}
	base := "/api/v3/repos/" + r.Owner + "/" + r.Name
	return map[string]any{
		"id":               pr.RestID,
		"node_id":          pr.NodeID,
		"number":           pr.Number,
		"state":            pr.State,
		"locked":           false,
		"title":            pr.Title,
		"body":             pr.Body,
		"user":             userJSON(),
		"created_at":       FIXED_ISO,
		"updated_at":       FIXED_ISO,
		"closed_at":        closedAt,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"merged":           pr.Merged,
		"mergeable":        mergeable,
		"mergeable_state":  mergeableState,
		"merged_by":        mergedBy,
		"draft":            false,
		"head": map[string]any{
			"label": r.Owner + ":" + pr.Head.Ref,
			"ref":   pr.Head.Ref,
			"sha":   headSha,
			"user":  userJSON(),
			"repo":  repoJSON(s, r),
		},
		"base": map[string]any{
			"label": r.Owner + ":" + pr.Base.Ref,
			"ref":   pr.Base.Ref,
			"sha":   baseSha,
			"user":  userJSON(),
			"repo":  repoJSON(s, r),
		},
		"html_url":              fmt.Sprintf("https://example.invalid/%s/%s/pull/%d", r.Owner, r.Name, pr.Number),
		"url":                   fmt.Sprintf("%s/pulls/%d", base, pr.Number),
		"diff_url":              fmt.Sprintf("https://example.invalid/%s/%s/pull/%d.diff", r.Owner, r.Name, pr.Number),
		"patch_url":             fmt.Sprintf("https://example.invalid/%s/%s/pull/%d.patch", r.Owner, r.Name, pr.Number),
		"issue_url":             fmt.Sprintf("%s/issues/%d", base, pr.Number),
		"commits_url":           fmt.Sprintf("%s/pulls/%d/commits", base, pr.Number),
		"review_comments_url":   fmt.Sprintf("%s/pulls/%d/comments", base, pr.Number),
		"comments_url":          fmt.Sprintf("%s/issues/%d/comments", base, pr.Number),
		"statuses_url":          fmt.Sprintf("%s/statuses/%s", base, headSha),
		"additions":             additions,
		"deletions":             deletions,
		"changed_files":         len(files),
		"commits":               1,
		"review_comments":       0,
		"comments":              0,
		"maintainer_can_modify": false,
		"author_association":    "OWNER",
	}
}

// ---------------------------------------------------------------------------
// handler
// ---------------------------------------------------------------------------

// handleRest serves the REST v3 API under /api/v3/*.
func handleRest(c *reqCtx) {
	seg := pathSegments(c.r.URL.Path)
	method := c.r.Method
	if len(seg) < 2 || seg[0] != "api" || seg[1] != "v3" {
		c.jsonErr(404, "Not Found")
		return
	}

	// GET /api/v3/ — the API root (no auth needed, GitHub answers it openly).
	if method == "GET" && len(seg) == 2 {
		c.header("X-OAuth-Scopes", "repo, read:org")
		c.json(200, map[string]any{
			"current_user_url":                     "/api/v3/user",
			"current_user_authorizations_html_url": "https://example.invalid/settings/applications",
			"authorizations_url":                   "/api/v3/authorizations",
			"code_search_url":                      "/api/v3/search/code?q={query}{&page,per_page,sort,order}",
			"commit_search_url":                    "/api/v3/search/commits?q={query}{&page,per_page,sort,order}",
			"emails_url":                           "/api/v3/user/emails",
			"emojis_url":                           "/api/v3/emojis",
			"events_url":                           "/api/v3/events",
			"feeds_url":                            "/api/v3/feeds",
			"followers_url":                        "/api/v3/user/followers",
			"following_url":                        "/api/v3/user/following{/target}",
			"gists_url":                            "/api/v3/gists{/gist_id}",
			"hub_url":                              "https://example.invalid/hub",
			"issue_search_url":                     "/api/v3/search/issues?q={query}{&page,per_page,sort,order}",
			"issues_url":                           "/api/v3/issues",
			"keys_url":                             "/api/v3/user/keys",
			"label_search_url":                     "/api/v3/search/labels?q={query}&repository_id={repository_id}{&page,per_page}",
			"notifications_url":                    "/api/v3/notifications",
			"organization_url":                     "/api/v3/orgs/{org}",
			"organization_repositories_url":        "/api/v3/orgs/{org}/repos{?type,page,per_page,sort}",
			"organization_teams_url":               "/api/v3/orgs/{org}/teams",
			"public_gists_url":                     "/api/v3/gists/public",
			"rate_limit_url":                       "/api/v3/rate_limit",
			"repository_url":                       "/api/v3/repos/{owner}/{repo}",
			"repository_search_url":                "/api/v3/search/repositories?q={query}{&page,per_page,sort,order}",
			"current_user_repositories_url":        "/api/v3/user/repos{?type,page,per_page,sort}",
			"starred_url":                          "/api/v3/user/starred{/owner}{/repo}",
			"starred_gists_url":                    "/api/v3/gists/starred",
			"topic_search_url":                     "/api/v3/search/topics?q={query}{&page,per_page}",
			"user_url":                             "/api/v3/users/{user}",
			"user_organizations_url":               "/api/v3/user/orgs",
			"user_repositories_url":                "/api/v3/user/{user}/repos{?type,page,per_page,sort}",
			"user_search_url":                      "/api/v3/search/users?q={query}{&page,per_page,sort,order}",
		})
		return
	}

	// GET /api/v3/meta
	if method == "GET" && len(seg) == 3 && seg[2] == "meta" {
		c.json(200, map[string]any{
			"verifiable_password_authentication": false,
			"installed_version":                  FORGE_VERSION,
			"github_enterprise_version":          FORGE_VERSION,
		})
		return
	}

	// Everything else under /api/v3 requires the bearer token.
	if !authed(c.r) {
		c.header("WWW-Authenticate", `Basic realm="github-core-clone", Bearer`)
		c.jsonErr(401, "Bad credentials")
		return
	}

	// GET /api/v3/user
	if method == "GET" && len(seg) == 3 && seg[2] == "user" {
		c.json(200, userJSON())
		return
	}

	// /api/v3/user/repos
	if len(seg) == 4 && seg[2] == "user" && seg[3] == "repos" {
		if method == "GET" {
			all := make([]*Repo, 0, len(c.s.Repos))
			for _, repo := range c.s.Repos {
				all = append(all, repo)
			}
			sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
			items := make([]map[string]any, 0, len(all))
			for _, repo := range all {
				items = append(items, repoJSON(c.s, repo))
			}
			c.json(200, paginate(c, items))
			return
		}
		if method == "POST" {
			body, ok := readBodyObject(c)
			if !ok {
				return
			}
			name := strings.TrimSpace(bodyString(body, "name"))
			if name == "" {
				c.jsonErr(422, "name is required")
				return
			}
			if _, exists := c.s.Repos[name]; exists {
				c.jsonErr(422, "Repository already exists")
				return
			}
			private, _ := bodyBool(body, "private")
			repo := NewRepo(c.s, name, RepoOptions{
				Description: bodyString(body, "description"),
				Private:     private,
			})
			c.json(201, repoJSON(c.s, repo))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}

	// /api/v3/repos/{owner}/{name}[/...]
	if len(seg) >= 5 && seg[2] == "repos" {
		owner, name := seg[3], seg[4]
		rest := seg[5:]

		// GET/PATCH/DELETE /repos/{o}/{r}
		if len(rest) == 0 {
			switch method {
			case "GET":
				repo := needRepo(c, owner, name)
				if repo == nil {
					return
				}
				cachedJSON(c, repo, "repo", func() map[string]any { return repoJSON(c.s, repo) })
			case "PATCH":
				repo := needRepo(c, owner, name)
				if repo == nil {
					return
				}
				body, ok := readBodyObject(c)
				if !ok {
					return
				}
				if desc, isStr := body["description"].(string); isStr {
					repo.Description = desc
				}
				if priv, isBool := bodyBool(body, "private"); isBool {
					repo.Private = priv
				}
				TouchRepo(repo)
				c.json(200, repoJSON(c.s, repo))
			case "DELETE":
				repo := needRepo(c, owner, name)
				if repo == nil {
					return
				}
				delete(c.s.Repos, name)
				c.empty(204)
			default:
				c.jsonErr(404, "Not Found")
			}
			return
		}

		repo := needRepo(c, owner, name)
		if repo == nil {
			return
		}

		switch rest[0] {
		case "branches":
			handleBranches(c, repo, owner, name, rest)
		case "commits":
			handleCommits(c, repo, owner, name, rest)
		case "git":
			handleGitRefs(c, repo, owner, name, rest)
		case "contents":
			handleContents(c, repo, owner, name, rest)
		case "labels":
			handleLabels(c, repo, rest)
		case "issues":
			handleIssues(c, repo, rest)
		case "pulls":
			handlePulls(c, repo, rest)
		default:
			c.jsonErr(404, "Not Found")
		}
		return
	}

	c.jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// branches and commits
// ---------------------------------------------------------------------------

func handleBranches(c *reqCtx, repo *Repo, owner, name string, rest []string) {
	if c.r.Method != "GET" {
		c.jsonErr(404, "Not Found")
		return
	}
	names := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	branchJSON := func(branch string) map[string]any {
		sha := repo.Refs["refs/heads/"+branch]
		return map[string]any{
			"name":      branch,
			"commit":    map[string]any{"sha": sha, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, sha)},
			"protected": false,
		}
	}
	if len(rest) == 1 {
		items := make([]map[string]any, 0, len(names))
		for _, branch := range names {
			items = append(items, branchJSON(branch))
		}
		c.json(200, items)
		return
	}
	branch := strings.Join(rest[1:], "/")
	if _, ok := repo.Refs["refs/heads/"+branch]; !ok {
		c.jsonErr(404, "Branch not found")
		return
	}
	c.json(200, branchJSON(branch))
}

func handleCommits(c *reqCtx, repo *Repo, owner, name string, rest []string) {
	if c.r.Method != "GET" || len(rest) != 1 {
		c.jsonErr(404, "Not Found")
		return
	}
	from := c.r.URL.Query().Get("sha")
	if from == "" {
		from = repo.DefaultBranch
	}
	var tip string
	if IsObjectID(from) {
		tip = from
	} else {
		tip = RefCommit(repo, "refs/heads/"+from)
		if tip == "" {
			tip = RefCommit(repo, from)
		}
	}
	if tip == "" {
		// Unknown ref: GitHub answers 404 when a sha/branch does not exist,
		// except that an empty repository lists no commits.
		if len(repo.Refs) == 0 && !IsObjectID(from) {
			c.json(200, []map[string]any{})
			return
		}
		c.jsonErr(404, "No commit found for SHA")
		return
	}
	shas := paginate(c, ListCommits(repo, tip, 500))
	items := make([]map[string]any, 0, len(shas))
	for _, sha := range shas {
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		parents := make([]map[string]any, 0, len(commit.Parents))
		for _, p := range commit.Parents {
			parents = append(parents, map[string]any{"sha": p})
		}
		items = append(items, map[string]any{
			"sha":     sha,
			"node_id": globalIDStr("Commit", sha),
			"commit": map[string]any{
				// The raw object stores the message with a trailing newline;
				// the API surface shows it without one.
				"message":       strings.TrimSuffix(commit.Message, "\n"),
				"author":        map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FIXED_ISO},
				"committer":     map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FIXED_ISO},
				"tree":          map[string]any{"sha": commit.Tree},
				"url":           fmt.Sprintf("/api/v3/repos/%s/%s/git/commits/%s", owner, name, sha),
				"comment_count": 0,
			},
			"url":       fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, name, sha),
			"html_url":  fmt.Sprintf("https://example.invalid/%s/%s/commit/%s", owner, name, sha),
			"author":    userJSON(),
			"committer": userJSON(),
			"parents":   parents,
		})
	}
	c.json(200, items)
}

// ---------------------------------------------------------------------------
// git refs
// ---------------------------------------------------------------------------

func refJSON(repo *Repo, owner, name, ref, sha string, withObjectURL bool) map[string]any {
	typ := "commit"
	if obj := repo.Objects[sha]; obj != nil {
		typ = obj.Type
	}
	object := map[string]any{"sha": sha, "type": typ}
	if withObjectURL {
		object["url"] = fmt.Sprintf("/api/v3/repos/%s/%s/git/%ss/%s", owner, name, typ, sha)
	}
	return map[string]any{
		"ref":     ref,
		"node_id": globalIDStr("Ref", fmt.Sprintf("%d:%s", repo.DbID, ref)),
		"url":     fmt.Sprintf("/api/v3/repos/%s/%s/git/refs/%s", owner, name, encodeURIComponent(ref)),
		"object":  object,
	}
}

func handleGitRefs(c *reqCtx, repo *Repo, owner, name string, rest []string) {
	if len(rest) < 2 || rest[1] != "refs" {
		c.jsonErr(404, "Not Found")
		return
	}
	if c.r.Method == "POST" && len(rest) == 2 {
		body, ok := readBodyObject(c)
		if !ok {
			return
		}
		ref := bodyString(body, "ref")
		sha := bodyString(body, "sha")
		if !strings.HasPrefix(ref, "refs/") || !IsObjectID(sha) {
			c.jsonErr(422, "Invalid ref or sha")
			return
		}
		if _, exists := repo.Refs[ref]; exists {
			c.jsonErr(422, "Reference already exists")
			return
		}
		object := repo.Objects[sha]
		if object == nil || (object.Type != "commit" && object.Type != "tag") {
			c.jsonErr(422, "Object not found")
			return
		}
		repo.Refs[ref] = sha
		c.json(201, refJSON(repo, owner, name, ref, sha, true))
		return
	}
	if c.r.Method == "GET" {
		if len(rest) == 2 {
			refs := make([]string, 0, len(repo.Refs))
			for ref := range repo.Refs {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			items := make([]map[string]any, 0, len(refs))
			for _, ref := range refs {
				items = append(items, refJSON(repo, owner, name, ref, repo.Refs[ref], false))
			}
			c.json(200, items)
			return
		}
		ref := "refs/" + strings.Join(rest[2:], "/")
		sha, ok := repo.Refs[ref]
		if !ok {
			c.jsonErr(404, "Reference not found")
			return
		}
		c.json(200, refJSON(repo, owner, name, ref, sha, false))
		return
	}
	c.jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

func handleContents(c *reqCtx, repo *Repo, owner, name string, rest []string) {
	filePath := strings.Join(rest[1:], "/")
	switch c.r.Method {
	case "PUT":
		handleContentsPut(c, repo, owner, name, filePath)
	case "GET":
		handleContentsGet(c, repo, owner, name, filePath)
	case "DELETE":
		handleContentsDelete(c, repo, owner, name, filePath)
	default:
		c.jsonErr(404, "Not Found")
	}
}

func handleContentsPut(c *reqCtx, repo *Repo, owner, name, filePath string) {
	body, ok := readBodyObject(c)
	if !ok {
		return
	}
	branch := bodyString(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	branchTip := RefCommit(repo, "refs/heads/"+branch)
	// A branch that does not exist cannot receive a contents commit, except
	// the default branch of an empty repo (the first commit creates it).
	if branchTip == "" && (len(repo.Refs) > 0 || branch != repo.DefaultBranch) {
		c.jsonErr(404, "Branch not found")
		return
	}
	contentB64, hasContent := body["content"].(string)
	message, hasMessage := body["message"].(string)
	if !hasContent || !hasMessage {
		c.jsonErr(422, "content and message are required")
		return
	}
	content, err := decodeBase64Loose(contentB64)
	if err != nil {
		c.jsonErr(422, "content is not valid base64")
		return
	}
	flat := tipTree(repo, branchTip)
	existing, isUpdate := flat[filePath]
	if isUpdate {
		sha := bodyString(body, "sha")
		if sha == "" {
			c.jsonErr(422, "sha is required to update a file")
			return
		}
		if !strings.EqualFold(sha, existing.OID) {
			c.jsonErr(409, "sha does not match the current file")
			return
		}
	} else if sha := bodyString(body, "sha"); sha != "" {
		c.jsonErr(422, "sha does not match: the file does not exist")
		return
	}
	commitSha := CommitFiles(repo, branch, []FileChange{{Path: filePath, Content: content}}, message)
	blobOID := tipTree(repo, commitSha)[filePath].OID
	baseName := filePath
	if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
		baseName = filePath[i+1:]
	}
	status := 201
	if isUpdate {
		status = 200
	}
	c.json(status, map[string]any{
		"content": map[string]any{
			"name":         baseName,
			"path":         filePath,
			"sha":          blobOID,
			"size":         len(content),
			"url":          fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, filePath),
			"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/blob/%s/%s", owner, name, branch, filePath),
			"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, name, blobOID),
			"download_url": nil,
			"type":         "file",
			"encoding":     "base64",
			"content":      base64.StdEncoding.EncodeToString(content),
			"_links":       map[string]any{},
		},
		"commit": map[string]any{
			"sha":       commitSha,
			"node_id":   globalIDStr("Commit", commitSha),
			"message":   message,
			"author":    map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FIXED_ISO},
			"committer": map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FIXED_ISO},
		},
	})
}

func handleContentsGet(c *reqCtx, repo *Repo, owner, name, filePath string) {
	var tip string
	if refParam := c.r.URL.Query().Get("ref"); refParam != "" {
		if IsObjectID(refParam) {
			tip = refParam
		} else {
			tip = RefCommit(repo, "refs/heads/"+refParam)
			if tip == "" {
				tip = RefCommit(repo, refParam)
			}
		}
		if tip == "" || repo.Objects[tip] == nil {
			c.jsonErr(404, "Not Found")
			return
		}
	} else {
		tip = RefCommit(repo, "refs/heads/"+repo.DefaultBranch)
		if tip == "" {
			c.jsonErr(404, "Not Found")
			return
		}
	}
	tipObj := repo.Objects[tip]
	if tipObj == nil || tipObj.Type != "commit" {
		c.jsonErr(404, "Not Found")
		return
	}
	commit, err := ParseCommit(tipObj.Data)
	if err != nil {
		c.jsonErr(404, "Not Found")
		return
	}
	treeOID := commit.Tree
	flat := FlattenTree(repo, treeOID, "", nil)
	if filePath == "" {
		// Root listing.
		treeObj := repo.Objects[treeOID]
		if treeObj == nil || treeObj.Type != "tree" {
			c.json(200, []map[string]any{})
			return
		}
		entries, err := ParseTree(treeObj.Data)
		if err != nil {
			c.json(200, []map[string]any{})
			return
		}
		items := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			isDir := entry.Mode == "40000"
			item := map[string]any{
				"type":         map[bool]string{true: "dir", false: "file"}[isDir],
				"name":         entry.Name,
				"path":         entry.Name,
				"sha":          entry.OID,
				"url":          fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, entry.Name),
				"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/tree/%s/%s", owner, name, repo.DefaultBranch, entry.Name),
				"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/trees/%s", owner, name, entry.OID),
				"download_url": nil,
			}
			if !isDir {
				size := 0
				if obj := repo.Objects[entry.OID]; obj != nil {
					size = len(obj.Data)
				}
				item["size"] = size
			}
			items = append(items, item)
		}
		c.json(200, items)
		return
	}
	if file, ok := flat[filePath]; ok {
		data := []byte{}
		if obj := repo.Objects[file.OID]; obj != nil {
			data = obj.Data
		}
		baseName := filePath
		if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
			baseName = filePath[i+1:]
		}
		c.json(200, map[string]any{
			"type":         "file",
			"encoding":     "base64",
			"size":         len(data),
			"name":         baseName,
			"path":         filePath,
			"content":      base64.StdEncoding.EncodeToString(data),
			"sha":          file.OID,
			"url":          fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, filePath),
			"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/blob/%s/%s", owner, name, repo.DefaultBranch, filePath),
			"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, name, file.OID),
			"download_url": nil,
		})
		return
	}
	// Directory?
	prefix := filePath + "/"
	type child struct {
		isDir bool
		entry FlatEntry
		path  string
	}
	children := make(map[string]*child)
	for path, entry := range flat {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		restPath := path[len(prefix):]
		first := restPath
		if i := strings.IndexByte(restPath, '/'); i >= 0 {
			first = restPath[:i]
		}
		if _, ok := children[first]; !ok {
			children[first] = &child{isDir: strings.ContainsRune(restPath, '/'), entry: entry, path: filePath + "/" + first}
		}
	}
	if len(children) == 0 {
		c.jsonErr(404, "Not Found")
		return
	}
	names := make([]string, 0, len(children))
	for childName := range children {
		names = append(names, childName)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, childName := range names {
		ch := children[childName]
		sha := ch.entry.OID
		if ch.isDir {
			if sub := treeOIDAtPath(repo, treeOID, ch.path); sub != "" {
				sha = sub
			}
		}
		item := map[string]any{
			"type":         map[bool]string{true: "dir", false: "file"}[ch.isDir],
			"name":         childName,
			"path":         ch.path,
			"sha":          sha,
			"url":          fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", owner, name, ch.path),
			"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/tree/%s/%s", owner, name, repo.DefaultBranch, ch.path),
			"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/trees/%s", owner, name, sha),
			"download_url": nil,
		}
		if !ch.isDir {
			size := 0
			if obj := repo.Objects[ch.entry.OID]; obj != nil {
				size = len(obj.Data)
			}
			item["size"] = size
		}
		items = append(items, item)
	}
	c.json(200, items)
}

func handleContentsDelete(c *reqCtx, repo *Repo, owner, name, filePath string) {
	body, ok := readBodyObject(c)
	if !ok {
		return
	}
	branch := bodyString(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	branchTip := RefCommit(repo, "refs/heads/"+branch)
	if branchTip == "" {
		c.jsonErr(404, "Not Found")
		return
	}
	flat := tipTree(repo, branchTip)
	existing, ok := flat[filePath]
	if !ok {
		c.jsonErr(404, "Not Found")
		return
	}
	if sha := bodyString(body, "sha"); sha != "" && !strings.EqualFold(sha, existing.OID) {
		c.jsonErr(409, "sha does not match the current file")
		return
	}
	message := bodyString(body, "message")
	if message == "" {
		message = "Delete " + filePath
	}
	commitSha := CommitFiles(repo, branch, []FileChange{{Path: filePath, Content: nil}}, message)
	c.json(200, map[string]any{
		"commit": map[string]any{
			"sha":     commitSha,
			"message": message,
			"author":  map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FIXED_ISO},
		},
	})
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func handleLabels(c *reqCtx, repo *Repo, rest []string) {
	if len(rest) != 1 {
		c.jsonErr(404, "Not Found")
		return
	}
	if c.r.Method == "GET" {
		names := make([]string, 0, len(repo.Labels))
		for name := range repo.Labels {
			names = append(names, name)
		}
		sort.Strings(names)
		items := make([]map[string]any, 0, len(names))
		for _, name := range names {
			items = append(items, labelJSON(repo, repo.Labels[name]))
		}
		c.json(200, items)
		return
	}
	if c.r.Method == "POST" {
		body, ok := readBodyObject(c)
		if !ok {
			return
		}
		labelName := strings.TrimSpace(bodyString(body, "name"))
		if labelName == "" {
			c.jsonErr(422, "name is required")
			return
		}
		if existing, ok := repo.Labels[labelName]; ok {
			c.json(201, labelJSON(repo, existing))
			return
		}
		color := bodyString(body, "color")
		if color == "" {
			color = "ededed"
		} else {
			color = strings.TrimPrefix(color, "#")
		}
		label := &Label{Name: labelName, Color: color}
		labelRestIDs[label] = c.s.NextObjectID
		c.s.NextObjectID++
		repo.Labels[labelName] = label
		c.json(201, labelJSON(repo, label))
		return
	}
	c.jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// issues and comments
// ---------------------------------------------------------------------------

func handleIssues(c *reqCtx, repo *Repo, rest []string) {
	if len(rest) == 1 {
		if c.r.Method == "GET" {
			state := c.r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			direction := "desc"
			if strings.EqualFold(c.r.URL.Query().Get("direction"), "asc") {
				direction = "asc"
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for _, iss := range repo.Issues {
				if state == "open" && iss.State != "open" {
					continue
				}
				if state == "closed" && iss.State != "closed" {
					continue
				}
				list = append(list, iss)
			}
			sort.Slice(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].Number < list[j].Number
				}
				return list[i].Number > list[j].Number
			})
			page := paginate(c, list)
			items := make([]map[string]any, 0, len(page))
			for _, iss := range page {
				items = append(items, issueJSON(c.s, repo, iss))
			}
			c.json(200, items)
			return
		}
		if c.r.Method == "POST" {
			body, ok := readBodyObject(c)
			if !ok {
				return
			}
			title := bodyString(body, "title")
			if title == "" {
				c.jsonErr(422, "title is required")
				return
			}
			var wanted []any
			if arr, isArr := body["labels"].([]any); isArr {
				wanted = arr
			}
			labelNames := make([]string, 0, len(wanted))
			for _, raw := range wanted {
				var labelName string
				switch v := raw.(type) {
				case string:
					labelName = v
				case map[string]any:
					labelName = bodyString(v, "name")
				}
				if _, exists := repo.Labels[labelName]; !exists {
					c.jsonErr(422, fmt.Sprintf("Label '%s' does not exist", labelName))
					return
				}
				labelNames = append(labelNames, labelName)
			}
			number := repo.NextNumber
			repo.NextNumber++
			restID := c.s.NextObjectID
			c.s.NextObjectID++
			iss := &Issue{
				Number:    number,
				RestID:    restID,
				NodeID:    globalIDStr("Issue", fmt.Sprintf("%d:%d", repo.DbID, number)),
				Title:     title,
				Body:      bodyString(body, "body"),
				State:     "open",
				Labels:    labelNames,
				Comments:  []*Comment{},
				CreatedAt: FIXED_ISO,
				UpdatedAt: FIXED_ISO,
			}
			repo.Issues[number] = iss
			TouchRepo(repo)
			c.json(201, issueJSON(c.s, repo, iss))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}

	number, err := strconv.Atoi(rest[1])
	if err != nil || number <= 0 {
		c.jsonErr(404, "Not Found")
		return
	}
	iss := repo.Issues[number]
	if len(rest) == 2 {
		if iss == nil {
			c.jsonErr(404, "Not Found")
			return
		}
		if c.r.Method == "GET" {
			cachedJSON(c, repo, fmt.Sprintf("issue:%d", number), func() map[string]any {
				return issueJSON(c.s, repo, iss)
			})
			return
		}
		if c.r.Method == "PATCH" {
			body, ok := readBodyObject(c)
			if !ok {
				return
			}
			if title, isStr := body["title"].(string); isStr {
				iss.Title = title
			}
			if text, isStr := body["body"].(string); isStr {
				iss.Body = text
			}
			if state, isStr := body["state"].(string); isStr && (state == "open" || state == "closed") {
				iss.State = state
			}
			iss.UpdatedAt = FIXED_ISO
			TouchRepo(repo)
			c.json(200, issueJSON(c.s, repo, iss))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}
	if len(rest) == 3 && rest[2] == "comments" {
		if iss == nil {
			c.jsonErr(404, "Not Found")
			return
		}
		if c.r.Method == "GET" {
			items := make([]map[string]any, 0, len(iss.Comments))
			for _, cm := range iss.Comments {
				items = append(items, commentJSON(c.s, repo, iss, cm))
			}
			c.json(200, items)
			return
		}
		if c.r.Method == "POST" {
			body, ok := readBodyObject(c)
			if !ok {
				return
			}
			text := bodyString(body, "body")
			if text == "" {
				c.jsonErr(422, "body is required")
				return
			}
			seq := repo.NextCommentID
			repo.NextCommentID++
			restID := c.s.NextObjectID
			c.s.NextObjectID++
			cm := &Comment{RestID: restID, Body: text, CreatedAt: FIXED_ISO, UpdatedAt: FIXED_ISO}
			commentSeqs[cm] = seq
			iss.Comments = append(iss.Comments, cm)
			iss.UpdatedAt = FIXED_ISO
			TouchRepo(repo)
			c.json(201, commentJSON(c.s, repo, iss, cm))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}
	c.jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

func handlePulls(c *reqCtx, repo *Repo, rest []string) {
	if len(rest) == 1 {
		if c.r.Method == "GET" {
			state := c.r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if state == "open" && pr.State != "open" {
					continue
				}
				if state == "closed" && pr.State != "closed" {
					continue
				}
				list = append(list, pr)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
			page := paginate(c, list)
			items := make([]map[string]any, 0, len(page))
			for _, pr := range page {
				items = append(items, prJSON(c.s, repo, pr))
			}
			c.json(200, items)
			return
		}
		if c.r.Method == "POST" {
			body, ok := readBodyObject(c)
			if !ok {
				return
			}
			head := bodyString(body, "head")
			base := bodyString(body, "base")
			if base == "" {
				base = repo.DefaultBranch
			}
			title := bodyString(body, "title")
			if head == "" || title == "" {
				c.jsonErr(422, "head and title are required")
				return
			}
			headTip := RefCommit(repo, "refs/heads/"+head)
			if headTip == "" {
				c.jsonErr(422, fmt.Sprintf("Head branch '%s' not found", head))
				return
			}
			baseTip := RefCommit(repo, "refs/heads/"+base)
			if baseTip == "" {
				c.jsonErr(422, fmt.Sprintf("Base branch '%s' not found", base))
				return
			}
			for _, existing := range repo.Pulls {
				if existing.State == "open" && existing.Head.Ref == head && existing.Base.Ref == base {
					c.jsonErr(422, "A pull request already exists for this head and base")
					return
				}
			}
			number := repo.NextNumber
			repo.NextNumber++
			restID := c.s.NextObjectID
			c.s.NextObjectID++
			pr := &Pull{
				Number: number,
				RestID: restID,
				NodeID: globalIDStr("PullRequest", fmt.Sprintf("%d:%d", repo.DbID, number)),
				Title:  title,
				Body:   bodyString(body, "body"),
				State:  "open",
				Head:   Ref{Ref: head, SHA: headTip},
				Base:   Ref{Ref: base, SHA: baseTip},
				Merged: false,
			}
			repo.Pulls[number] = pr
			TouchRepo(repo)
			c.json(201, prJSON(c.s, repo, pr))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}

	number, err := strconv.Atoi(rest[1])
	if err != nil || number <= 0 {
		c.jsonErr(404, "Not Found")
		return
	}
	pr := repo.Pulls[number]
	if len(rest) == 2 {
		if pr == nil {
			c.jsonErr(404, "Not Found")
			return
		}
		if c.r.Method == "GET" {
			c.json(200, prJSON(c.s, repo, pr))
			return
		}
		c.jsonErr(404, "Not Found")
		return
	}
	if len(rest) == 3 && rest[2] == "files" {
		if pr == nil {
			c.jsonErr(404, "Not Found")
			return
		}
		if c.r.Method != "GET" {
			c.jsonErr(404, "Not Found")
			return
		}
		files := prDiff(repo, pr)
		items := make([]map[string]any, 0, len(files))
		for _, f := range files {
			sha := f.NewOID
			if sha == "" {
				sha = f.OldOID
			}
			items = append(items, map[string]any{
				"sha":          sha,
				"filename":     f.Path,
				"status":       f.Status,
				"additions":    f.Additions,
				"deletions":    f.Deletions,
				"changes":      f.Changes,
				"blob_url":     fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", repo.Owner, repo.Name, sha),
				"raw_url":      fmt.Sprintf("https://example.invalid/%s/%s/raw/%s/%s", repo.Owner, repo.Name, pr.Head.Ref, f.Path),
				"contents_url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s?ref=%s", repo.Owner, repo.Name, f.Path, pr.Head.Ref),
				"patch":        "",
			})
		}
		c.json(200, items)
		return
	}
	if len(rest) == 3 && rest[2] == "merge" {
		if c.r.Method != "PUT" {
			c.jsonErr(404, "Not Found")
			return
		}
		if pr == nil {
			c.jsonErr(404, "Not Found")
			return
		}
		if pr.Merged || pr.State != "open" {
			c.jsonErr(405, "Pull request is already merged or closed")
			return
		}
		if PRConflict(repo, pr) {
			c.jsonErr(405, "Merge conflict")
			return
		}
		headTip := RefCommit(repo, "refs/heads/"+pr.Head.Ref)
		baseTip := RefCommit(repo, "refs/heads/"+pr.Base.Ref)
		if headTip == "" || baseTip == "" {
			c.jsonErr(405, "Head or base branch is missing")
			return
		}
		var headTree string
		if obj := repo.Objects[headTip]; obj != nil && obj.Type == "commit" {
			if cm, err := ParseCommit(obj.Data); err == nil {
				headTree = cm.Tree
			}
		}
		// A squash commit: the head's tree, the base tip as parent.
		mergeSha := PutObject(repo, "commit", BuildCommit(Commit{
			Tree:    headTree,
			Parents: []string{baseTip},
			Message: fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", pr.Number, pr.Head.Ref, pr.Title),
		}))
		repo.Refs["refs/heads/"+pr.Base.Ref] = mergeSha
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = FIXED_ISO
		mergeCommitShas[pr] = mergeSha
		TouchRepo(repo)
		c.json(200, map[string]any{
			"sha":     mergeSha,
			"merged":  true,
			"message": "Pull request successfully merged",
		})
		return
	}
	c.jsonErr(404, "Not Found")
}
