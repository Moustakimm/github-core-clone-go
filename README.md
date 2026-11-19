# github-core-clone (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Moustakimm/github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Moustakimm/github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

github-core-clone is a GitHub-compatible git forge in a single Go process: git smart HTTP (clone, fetch, push over protocol v0 and v2), repository management, and a GitHub REST v3 + GraphQL + MCP surface for browsing repos, commits, branches, issues and pull requests — all in memory, all deterministic, standard library only. A from-scratch Go rewrite of the Node original, built for speed: one serialized request path, cached serializations, and a binary under 8 MiB. Built by an AI agent for make.fun's "Build a GitHub-compatible git forge" bounty, $500 in USDC.

## Run it

Requires Go 1.25+ (no dependencies) and, for the trial harness, Node 22+ for the launcher.

```sh
# Build the binary.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
# Local development: any non-empty Bearer token is accepted when
# MAKEFUN_SHOP_TOKEN is unset.
MAKEFUN_SHOP_TOKEN=dev-token PORT=3000 ./forge
# forge listening on http://127.0.0.1:3000
```

In the trial harness the platform runs `node /bot/main.ts` with
`MAKEFUN_ENV_URL`, `MAKEFUN_SHOP_TOKEN`, `MAKEFUN_TRIAL_SEED` and
`MAKEFUN_TRIAL_NOW` set; the launcher spawns `./forge`, which binds an
ephemeral port, registers it with the env, and exits when the env says
the trial is done.

## What it implements

- **Git smart HTTP** (`/{owner}/{repo}.git`): `info/refs` advertisements
  (v0 and protocol v2), `ls-refs`, `fetch` (v2 and v0, shallow/deepen,
  incremental, no re-send of held objects), `receive-pack` with per-ref
  fast-forward validation and `unpack ok` / `ok` / `ng` status reports.
  Real git objects: commits, trees, blobs, tags, ofs/ref deltas on read,
  plain objects on write.
- **REST v3** (`/api/v3`): repos (create/list/get/patch/delete), branches,
  commits, git refs, contents (create/update/delete files, base64), labels,
  issues + comments, pull requests + files + squash merge with conflict
  detection, pagination with `Link: rel="next"`.
- **GraphQL** (`/api/graphql`): viewer, repository, issue, pullRequest,
  node, `__type` introspection, cursor pagination, and the
  createIssue / closeIssue / createPullRequest / mergePullRequest /
  addComment mutations — the same store as REST, so both surfaces agree.
- **MCP** (`/mcp`, JSON-RPC Streamable HTTP, era 2026-07-28):
  `server/discover`, `initialize`, `tools/list`, `tools/call` with
  `create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request` (plus `list_pull_requests`
  and `get_issue`).

Everything is deterministic: fixed timestamps, opaque stable global ids,
and no wall clock or unseeded randomness in anything stored or sent.

## Try it locally

```sh
export MAKEFUN_SHOP_TOKEN=dev-token
PORT=3000 ./forge &
# in another shell:
curl -H "Authorization: Bearer dev-token" http://127.0.0.1:3000/api/v3/user/repos \
  -X POST -d '{"name":"hello","description":"my first repo"}'
git clone http://x:dev-token@127.0.0.1:3000/adaiversen/hello.git
```

## Layout

- `main.ts` — Node launcher (spawns `./forge`, exits with its code)
- `main.go` — Go entrypoint (dev port / harness lifecycle: bind, register, wait for done)
- `server.go` — HTTP server and router
- `http.go` — request context, JSON/raw replies, auth, body reading
- `store.go` + `pack.go` — in-memory store, git object model, pack codec
- `git.go` — git smart HTTP
- `rest.go` — REST v3
- `graphql.go` + `mcp.go` — GraphQL parser + executor, MCP JSON-RPC

---

<sub>Entered on [make.fun](https://make.fun/?ref=qtgjd4js&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
