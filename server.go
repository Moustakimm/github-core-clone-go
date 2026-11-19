// server.go — HTTP router for the forge, ported from the Node app.ts.
//
// One handler serves the whole forge:
//
//	GET  /healthz                      -> {"ok": true}
//	/api/v3/*                          -> REST v3 (rest.go)
//	POST /api/graphql                  -> GraphQL (graphql.go)
//	POST /mcp, DELETE /mcp            -> MCP (mcp.go)
//	/{owner}/{repo}.git/*              -> git smart HTTP (git.go)
//	everything else                    -> JSON 404
//
// The router reads the body (when the request may carry one), builds the
// *reqCtx, and holds the store lock for the whole request — the handlers
// never lock. Auth is a Bearer token via authed() (http.go): /healthz,
// GET /api/v3/ and GET /api/v3/meta are auth-free; everything else needs
// a valid credential or gets a 401. DELETE /mcp closes a session and, like
// the Node router, answers before any auth check.

package main

import (
	"net/http"
	"net/url"
	"strings"
)

// createHandler returns the single http.Handler for the whole forge.
func createHandler(s *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := nextReqID()

		// Read the body when the request may carry one; a body that cannot
		// be read or is too large is a 400.
		var body []byte
		if hasBody(r) {
			b, ok := readBody(r)
			if !ok {
				c := &reqCtx{w: w, r: r, s: s, id: id}
				c.jsonErr(400, "Bad Request")
				return
			}
			body = b
		}

		// Split the URL by hand and percent-decode the path, like the Node
		// router; a path that does not decode is a 400.
		path, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			c := &reqCtx{w: w, r: r, s: s, id: id}
			c.jsonErr(400, "Bad request path")
			return
		}
		// Segments without the leading empty element: '/a/b' -> ['a', 'b'].
		var segments []string
		if path == "/" {
			segments = []string{""}
		} else {
			segments = strings.Split(path[1:], "/")
		}

		c := &reqCtx{w: w, r: r, s: s, id: id, body: body}
		method := r.Method

		// GET /healthz — auth-free.
		if method == "GET" && path == "/healthz" {
			c.json(200, map[string]any{"ok": true})
			return
		}

		// GraphQL — POST only; a non-POST is a 404 before any auth check.
		if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
			if method != "POST" {
				c.jsonErr(404, "Not Found")
				return
			}
			if !authed(r) {
				c.jsonErr(401, "Unauthorized")
				return
			}
			s.Lock()
			defer s.Unlock()
			handleGraphQL(c)
			return
		}

		// MCP — DELETE closes a session (answered by the MCP handler, no
		// auth check); anything but POST is a 405 before any auth check.
		if len(segments) == 1 && segments[0] == "mcp" {
			if method == "DELETE" {
				s.Lock()
				defer s.Unlock()
				handleMcp(c)
				return
			}
			if method != "POST" {
				c.jsonErr(405, "Method not allowed")
				return
			}
			if !authed(r) {
				c.jsonErr(401, "Unauthorized")
				return
			}
			s.Lock()
			defer s.Unlock()
			handleMcp(c)
			return
		}

		// REST v3 — GET /api/v3/ and GET /api/v3/meta are auth-free.
		if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
			authFree := method == "GET" && (len(segments) == 2 ||
				(len(segments) == 3 && (segments[2] == "" || segments[2] == "meta")))
			if !authFree && !authed(r) {
				c.jsonErr(401, "Unauthorized")
				return
			}
			s.Lock()
			defer s.Unlock()
			handleRest(c)
			return
		}

		// Git smart HTTP: /{owner}/{repo}.git/...
		if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
			if !authed(r) {
				c.jsonErr(401, "Unauthorized")
				return
			}
			s.Lock()
			defer s.Unlock()
			handleGit(c)
			return
		}

		c.jsonErr(404, "Not Found")
	})
}
