# Go rewrite — module layout and conventions

Module `forge`, Go 1.25, **stdlib only** (no external deps), single package `main` in the
repo root. Build: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .`
The built binary MUST stay under 8 MiB (entry limit) and the whole tree under 16 MiB.
Go toolchain: `$HOME/go/bin/go`, `GOPATH=$HOME/gopath`.

## Files (one builder each; builder 1 defines the shared types)
- `store.go` + `pack.go` — in-memory store + git object model + pack codec (builder 1).
  Defines ALL shared types: Store, Repo, Issue, Pull, Comment, Label, Object,
  TreeEntry, Commit, and the store/pack functions. Later builders read these files
  and use the types as-is; do NOT redefine them.
- `git.go` — git smart HTTP (builder 2).
- `rest.go` — REST v3 (builder 3, first half) — see note below.
- `graphql.go` + `mcp.go` — GraphQL + MCP (builder 3, second half) — see note below.
- `server.go` + `main.go` + `main.ts` — HTTP server/router/auth + Go main + Node
  lifecycle launcher (builder 4). `main.ts` (Node) spawns `./forge` and waits for it;
  the Go binary does the register/done lifecycle itself (MAKEFUN_ENV_URL, bind :0,
  POST /register {port}, poll GET /done until {"done":true}, exit 0).

## Conventions (from the Node reference — read it, do not re-derive)
- Store is in-memory only; the trial pod's /bot is read-only.
- Deterministic timestamps: FIXED_ISO "2025-01-01T00:00:00.000Z", FIXED_EPOCH 1735689600.
- Viewer login: see store.ts VIEWER (read the Node reference).
- Every JSON reply: application/json + non-empty X-GitHub-Request-Id; errors are JSON
  with a string `message`; unknown routes 404 JSON; malformed JSON body 400.
- Auth: Bearer <redacted>; when MAKEFUN_SHOP_TOKEN is set it must match exactly; when
  unset (local dev) any non-empty Bearer <redacted> is accepted. /healthz, GET /api/v3/
  and GET /api/v3/meta are auth-free. A repo under another owner is 404, never 403.
- Global IDs are base64("Kind:id") e.g. Repository, Issue, PullRequest.
- Keep the hot paths allocation-light: the four timed workloads are push (200x 1 KiB
  pushes), clone (50 concurrent shallow clones of a 200-commit repo), read (2000 GET
  issue, keep-alive), write (200 issue creations). Cache serialized replies keyed by
  the repo `rev`/`gitRev` counters, as the Node v2 does (jsonCache/packCache).
- NO `cp` of Node code: write idiomatic Go yourself. Read the Node reference
  (~/workspace/makefun/github-core-clone-01-repo/) and the kit's conformance checks
  (~/workspace/makefun/github-core-clone-01-kit/github-core-clone-01-kit/sdk/conformance/)
  to learn exact behavior — the conformance files are the ground truth for what is checked.
- Keep each builder session SHORT: read, write, verify, commit, report. Do not run
  long exploratory loops. Commit locally with git; never push/submit/upload.
