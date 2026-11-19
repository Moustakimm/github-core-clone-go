// http.go — shared HTTP plumbing for the forge: request context, JSON/raw
// replies, auth, body reading. Ported from the Node app.ts. Every handler in
// this package receives a *reqCtx; the router (server.go) builds it per request.

package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const maxBodyBytes = 64 * 1024 * 1024

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
var expectedBearer = func() string {
	if shopToken == "" {
		return ""
	}
	return "Bearer " + shopToken
}()

var reqCounter int64

func nextReqID() string {
	return "req-" + strconv.FormatInt(atomic.AddInt64(&reqCounter, 1), 10)
}

// reqCtx is one request: the response writer, the request, the store, the
// request id for X-GitHub-Request-Id, and the already-read body (nil when the
// request carries none).
type reqCtx struct {
	w    http.ResponseWriter
	r    *http.Request
	s    *Store
	id   string
	body []byte
	// extra headers set before the reply is written
	extra map[string]string
}

func (c *reqCtx) header(key, val string) {
	if c.extra == nil {
		c.extra = map[string]string{}
	}
	c.extra[key] = val
}

func (c *reqCtx) writeHeaders(status int, contentType string, length int) {
	h := c.w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-GitHub-Request-Id", c.id)
	if length >= 0 {
		h.Set("Content-Length", strconv.Itoa(length))
	}
	for k, v := range c.extra {
		h.Set(k, v)
	}
	c.w.WriteHeader(status)
}

// json writes a JSON reply (application/json, X-GitHub-Request-Id).
func (c *reqCtx) json(status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		c.jsonErr(500, "Internal Server Error")
		return
	}
	c.writeHeaders(status, "application/json", len(b))
	_, _ = c.w.Write(b)
}

// jsonErr writes {"message": msg} with the given status.
func (c *reqCtx) jsonErr(status int, msg string) {
	c.json(status, map[string]any{"message": msg})
}

// raw writes a binary reply with the given content type.
func (c *reqCtx) raw(status int, data []byte, contentType string) {
	c.writeHeaders(status, contentType, len(data))
	_, _ = c.w.Write(data)
}

// empty writes a status with no body (e.g. 204).
func (c *reqCtx) empty(status int) {
	h := c.w.Header()
	h.Set("X-GitHub-Request-Id", c.id)
	for k, v := range c.extra {
		h.Set(k, v)
	}
	c.w.WriteHeader(status)
}

// schemeToken returns the token part of an Authorization header that starts
// with `scheme ` (one space, then optional spaces/tabs), or "" when it does not.
func schemeToken(auth, scheme string) string {
	if !strings.HasPrefix(auth, scheme) {
		return ""
	}
	i := len(scheme)
	for i < len(auth) && (auth[i] == ' ' || auth[i] == '\t') {
		i++
	}
	if i >= len(auth) {
		return ""
	}
	tok := auth[i:]
	for j := 0; j < len(tok); j++ {
		if tok[j] == ' ' || tok[j] == '\t' || tok[j] == '\n' || tok[j] == '\r' {
			return ""
		}
	}
	return tok
}

// authed reports whether the request carries a valid credential: the suite
// sends exactly `Bearer <token>`; when MAKEFUN_SHOP_TOKEN is set it must match
// exactly, otherwise any non-empty Bearer <redacted> is accepted. Real git clients
// authenticate with Basic (token as password, or as the username when no
// password is given), which is accepted too.
func authed(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	if expectedBearer != "" && auth == expectedBearer {
		return true
	}
	if b := schemeToken(auth, "Bearer "); b != "" {
		if shopToken == "" {
			return true
		}
		return b == shopToken
	}
	if b := schemeToken(auth, "Basic "); b != "" {
		decoded, err := base64.StdEncoding.DecodeString(b)
		if err != nil {
			return false
		}
		s := string(decoded)
		user, pass := s, ""
		if i := strings.IndexByte(s, ':'); i >= 0 {
			user, pass = s[:i], s[i+1:]
		}
		if shopToken == "" {
			return true
		}
		return pass == shopToken || user == shopToken
	}
	return false
}

// readBody reads the request body up to maxBodyBytes; ok is false when the
// body is larger or unreadable. Callers treat !ok as a 400.
func readBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, false
	}
	if len(b) > maxBodyBytes {
		return nil, false
	}
	return b, true
}

// hasBody reports whether the request may carry a body (same rule as Node:
// methods other than GET/HEAD/DELETE, or a non-empty content-length, or a
// transfer-encoding).
func hasBody(r *http.Request) bool {
	m := r.Method
	if m != "GET" && m != "HEAD" && m != "DELETE" {
		return true
	}
	if cl := r.Header.Get("Content-Length"); cl != "" && cl != "0" {
		return true
	}
	return r.Header.Get("Transfer-Encoding") != ""
}
