# github-core-clone-forge

A standalone GitHub-compatible git forge, built for make.fun's
`github-core-clone-01` bounty. One Node 22 process, standard library only
(`node:http`, `node:crypto`, `node:zlib`), an in-memory store, no UI, no
outside service, no git binary.

## What it serves

- **Git smart HTTP** at `/<owner>/<repo>.git`: `info/refs`,
  `git-upload-pack` (protocol v2 with `ls-refs` and `fetch`, plus a v0
  fallback) and `git-receive-pack` (v0, `report-status`, `side-band-64k`,
  `delete-refs`). A real git object model: blobs, trees, commits and
  annotated tags, SHA-1 ids, packfiles (plain objects, version 2), shallow
  clones with `deepen`, incremental fetches that never re-send held objects.
- **REST v3** under `/api/v3`: repositories, branches, commits, contents,
  labels, issues, comments, pull requests, files and squash merges, with
  GitHub's statuses, JSON bodies, pagination `Link` headers and auth.
- **GraphQL v4** at `/api/graphql`: the documents `gh` and
  `github-mcp-server` actually send (viewer, repository, issues, pull
  requests, mutations, `__type` introspection, `node(id:)`, batching).
- **MCP** at `/mcp`: Streamable HTTP, revision `2026-07-28`
  (`server/discover`) with the `2025-11-25` handshake as a fallback, and
  six tools (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`) acting on the same store
  as REST.

## Run it

```sh
node --experimental-strip-types main.ts
```

The entry binds an ephemeral port, registers it at `$MAKEFUN_ENV_URL`,
then polls `$MAKEFUN_ENV_URL/done` until the trial ends and exits 0.
Every API call must carry `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`
(`/healthz` takes none). The token's owner is the forge's one user.

## Layout

- `main.ts` — the entry/env lifecycle (bind, register, wait for done).
- `app.ts` — the HTTP server, routing, auth and JSON discipline.
- `store.ts` — the in-memory store: repositories, objects, refs, issues,
  pull requests, labels, comments, and the id counters.
- `gitobj.ts` — the git object model, pkt-line codec, pack read/write,
  reachability and shallow/deepen logic.
- `githttp.ts` — the smart-HTTP handlers.
- `rest.ts` — the REST v3 handlers.
- `graphql.ts` — a small GraphQL parser and executor.
- `mcp.ts` — the MCP endpoint and tools.
- `util.ts` — small shared helpers.
