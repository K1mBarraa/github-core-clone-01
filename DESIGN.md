# Go rewrite of github-core-clone-01 — DESIGN CONTRACT

Read this whole file before writing any code. It is the contract every
builder honors. If it conflicts with the TypeScript reference, the
TypeScript reference's BEHAVIOUR wins (status codes, headers, response
shapes, error cases) — but the signatures below win for Go structure.

## 0. Goal and reference

- Port the TypeScript forge (reference: `~/workspace/makefun/github-core-clone-01-v3/`,
  commit `efcbaa8` — the LATEST TS, includes the v3 improvements; the v2 dir
  `~/workspace/makefun/github-core-clone-01-v2/` is an older copy, do not port from it)
  to Go, as a statically linked Linux amd64 binary launched by a `main.py`
  entrypoint. Behaviour-identical: the sealed suite's 120 checks must all pass.
- Target: production perf p50 cut ~40% vs v2 (local geomean ~211 ms -> ~122 ms).
  The local runner's score (~993) does NOT map to production — measure the
  geomean/p50, not the local score.
- Module: `forge`, Go 1.25, package `main`, stdlib ONLY (no external deps —
  the grading pod has no network and no install step).
- Work dir: `~/workspace/makefun/github-core-clone-01-v3-go/`.
- Go toolchain: `$HOME/go/bin/go` (1.25.4). Set `GOPATH=$HOME/gopath`
  (NOT `$HOME/go` — that is GOROOT). Build with
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .`
- BINARY SIZE CAP: the binary must be <= 8 MiB (rules: <= 8 MiB/file,
  <= 16 MiB total). A hello-world net/http binary is already 6.04 MiB.
  Measure the size after your first build and report it. If it exceeds
  8 MiB, STOP and report to the coordinator — do NOT silently switch the
  HTTP layer; the coordinator decides (options: trim imports, or a raw
  net.Conn HTTP layer).

## 1. File layout (one Go file per TS file, same names in .go)

| Go file | Ports (TS) | Owner |
|---|---|---|
| `util.go` | util.ts (84 lines) | B (server core) |
| `ctx.go` | ctx.ts (54) | B |
| `respond.go` | respond.ts (75) | B |
| `server.go` | app.ts (161) — HTTP server + routing | B |
| `main.go` | main.ts (130) — lifecycle (bind/register/poll/exit) | B |
| `main.py` | NEW — entrypoint, execs `./forge` | B |
| `gitobj.go` | gitobj.ts (757) — objects, packs, deltas, pkt-lines, fetch plan | A1 |
| `store.go` | store.ts (675) — Store, repos, issues, pulls, merges, JSON shapes | A2 |
| `githttp.go` | githttp.ts (478) — info/refs, upload-pack, receive-pack | B (with A1) |
| `rest.go` | rest.ts (829) — REST v3 | C |
| `graphql.go` | graphql.ts (1095) — GraphQL v4 | D1 |
| `mcp.go` | mcp.ts (399) — MCP | D2 |

`githttp.go` belongs to Builder B (it is the HTTP surface closest to the
server core), but B may only write it after A1's `gitobj.go` signatures are
stable — B reads A1's file, does not redefine its types.

## 2. Shared types — exact signatures (do not rename, do not change)

### util.go
```go
func Sha1Hex(data []byte) string
const ZeroOID = "0000000000000000000000000000000000000000"
func IsOID(s string) bool
func B64Encode(s string) string
func B64EncodeBytes(b []byte) string
func B64Decode(s string) ([]byte, error) // strip [ \r\n\t] first, then std base64
func B64URL(s string) string             // base64url, no padding
func TrialNowSec() int64                 // MAKEFUN_TRIAL_NOW ms -> sec, else 1735689600
func IsoOf(sec int64) string             // ISO-8601 UTC, same as JS toISOString()
func JSONText(v any) string              // JSON.stringify that never throws
func NextRequestID() string              // "req-<counter>", atomic counter
func CmpStr(a, b string) int             // -1 / 0 / 1, byte-wise
```

### gitobj.go
```go
type ObjType string
const (ObjBlob ObjType = "blob"; ObjTree ObjType = "tree"; ObjCommit ObjType = "commit"; ObjTag ObjType = "tag")
type GitObject struct { Type ObjType; Data []byte }
type TreeEntry struct { Mode, Name, OID string }
type Identity struct { Name, Email string }
type ParsedCommit struct { Tree string; Parents []string; Author, Committer Identity; Timestamp int64; Message string }
type ParsedTag struct { Object string; Type ObjType; Tag string; Tagger Identity; Timestamp int64; Message string }
type CommitInput struct { Tree string; Parents []string; Message string; Author, Committer Identity; Timestamp int64 }
type TagInput struct { Object string; Type ObjType; Tag string; Tagger Identity; Timestamp int64; Message string }
func HashObject(typ ObjType, data []byte) string
func BuildTree(entries []TreeEntry) []byte
func ParseTree(data []byte) []TreeEntry
func BuildCommit(in CommitInput) []byte
func ParseCommit(data []byte) ParsedCommit
func BuildTag(in TagInput) []byte
func ParseTag(data []byte) ParsedTag
func EncodePktLine(data []byte) []byte
var FlushPkt = []byte("0000"); var DelimPkt = []byte("0001"); var EndPkt = []byte("0002")
type PktKind int
const (PktData PktKind = iota; PktFlush; PktDelim; PktEnd)
type Pkt struct { Kind PktKind; Data []byte }
func EncodePktLines(pkts []Pkt) []byte
type PktLineDecoder struct { /* internal buffer */ }
func (d *PktLineDecoder) Push(chunk []byte)
func (d *PktLineDecoder) Next() (Pkt, bool)
func (d *PktLineDecoder) Finish() error
func DecodePktLines(buf []byte) []Pkt
func PktLinesAsStrings(buf []byte) []string
type PackObjectInput struct { Type ObjType; Data []byte }
func WritePack(objects []PackObjectInput) []byte   // pack v2, deflate level 1, SHA-1 trailer
func ApplyDelta(base, delta []byte) ([]byte, error)
type ReadPackResult struct { Objects map[string]GitObject; DeltaCount int }
func ReadPack(pack []byte, baseOf func(oid string) *GitObject) (ReadPackResult, error)
type FetchPlan struct { Objects []GitObject; Shallow []string; Unshallow []string }
func PlanFetch(lookup func(oid string) *GitObject, wants, haves []string, depth *int, priorShallow []string) FetchPlan
func ReachableObjects(lookup func(oid string) *GitObject, tips []string) map[string]bool
```
Notes: TS `readPack` throws on corruption — Go returns an error; the caller
turns it into a 500 JSON (same as the TS app-level catch). TS `depth` is
`number | undefined` — Go uses `*int` (nil = whole history). `writePack`
uses deflate level 1 (Go: `compress/zlib.NewWriterLevel(w, 1)`; note Go's
zlib writes the same format as Node's deflate — the bytes need not match
Node's, only decompress to the same data).
IMPORTANT — port the v3 improvements from the v3 dir: the commit-parents
cache in `store.go` (see §4), the single-allocation pkt-lines, and the
precomputed info/refs advertisement. Do NOT reintroduce the v2
`inflatedConsumed` binary search (v2's slow path) — the v3 Adler-32
stream-end finder is the reference.

### store.go
```go
const ViewerLogin = "adaiversen"; const ViewerID = 1000
const ViewerEmail = "adaiversen@example.invalid"; const DefaultBranch = "main"
type Label struct { Name, Color, Description string }
type Comment struct { ID int; Body, User string; CreatedSeq int }
type Issue struct { ID, Number int; Title, Body, State string; Labels []string; Comments []Comment; CreatedSeq, UpdatedSeq int; User string }
type Pull struct { ID, Number int; Title, Body, State string; Merged bool; MergedAt *string; MergeCommitSha *string; HeadRef, HeadSha, BaseRef, BaseSha string; CreatedSeq int; User string }
type Repo struct {
  Owner, Name string; ID int; Description string; Private bool; DefaultBranch string; CreatedSeq int
  Objects map[string]GitObject; Refs map[string]string
  Issues []*Issue; Pulls []*Pull; Labels map[string]Label
  NextIssueNumber, NextPullNumber, NextCommentID int
  IssueCache sync.Map // int -> string (serialized issue JSON)
}
type FlatFile struct { OID, Mode string }
type Store struct {
  mu sync.RWMutex
  repos map[string]*Repo
  nextRepoID, nextIssueID, nextPullID, nextCommentID int
  seq int
  isoCache sync.Map // int64 -> string
  packCache sync.Map // string -> []byte
}
```
Store methods (same semantics as TS; `nil`/`false` where TS returns
null/undefined/false; `(string, bool)` where TS returns `string | undefined`):
`NextSeq() int`, `NowSec() int64`, `IsoFor(seq int) string`,
`IsoSec(sec int64) string`, `TimeFor(seq int) int64`, `Key(owner, name string) string`,
`GetRepo(owner, name string) *Repo`, `ListViewerRepos() []*Repo`,
`CreateRepo(owner, name, description string, isPrivate bool) *Repo` (nil if exists),
`DeleteRepo(owner, name string) bool`,
`GetObject(repo *Repo, oid string) *GitObject`, `HasObject(repo *Repo, oid string) bool`,
`PutObject(repo *Repo, typ ObjType, data []byte) string`,
`InvalidatePackCache()`,
`HeadOID(repo *Repo) (string, bool)`, `RefOID(repo *Repo, ref string) (string, bool)`,
`SetRef(repo *Repo, ref, oid string)`, `DeleteRef(repo *Repo, ref string) bool`,
`PeelToCommit(repo *Repo, oid string) (string, bool)`,
`IsAncestor(repo *Repo, ancestor, descendant string) bool`,
`Ancestors(repo *Repo, oid string) map[string]int`,
`MergeBase(repo *Repo, a, b string) (string, bool)`,
`FlatTree(repo *Repo, commitOID string) map[string]FlatFile`,
`BuildTreeFromFlat(repo *Repo, flat map[string]FlatFile) string`,
`CreateCommit(repo *Repo, tree string, parents []string, message string, timestampSec *int64) string`,
`ThreeWayMerge(repo *Repo, baseOID, headOID string) (map[string]FlatFile, bool)`,
`Mergeable(repo *Repo, pr *Pull) bool`,
`CreateIssue(repo *Repo, title, body string, labels []string) *Issue`,
`CreatePull(repo *Repo, title, body, headRef, headSha, baseRef, baseSha string) *Pull`,
`FindIssue(repo *Repo, number int) *Issue`, `FindPull(repo *Repo, number int) *Pull`,
`AddComment(repo *Repo, issue *Issue, body string) Comment`,
`RepoGlobalID(repo *Repo) string`, `IssueGlobalID(repo *Repo, issue *Issue) string`,
`PullGlobalID(repo *Repo, pr *Pull) string`,
`CommentGlobalID(repo *Repo, issue *Issue, comment Comment) string`,
`ResolveGlobalID(id string) (kind string, repo *Repo, issue *Issue, pull *Pull)` — kind is
  `"repo"`, `"issue"`, `"pull"`, or `""` (not found); unused pointers are nil.
`RepoJSON(repo *Repo) map[string]any`, `LabelJSON(label Label) map[string]any`,
`IssueJSON(repo *Repo, issue *Issue) map[string]any`,
`IssueJSONText(repo *Repo, issue *Issue) string`,
`TouchIssue(repo *Repo, issue *Issue)`,
`CommentJSON(repo *Repo, issue *Issue, comment Comment) map[string]any`,
`PullJSON(repo *Repo, pr *Pull) map[string]any`,
`PullFiles(repo *Repo, pr *Pull) []PullFile` where
  `type PullFile struct { Filename, Status string; Additions, Deletions, Changes int }`
  (JSON keys: filename, status, additions, deletions, changes).

### ctx.go
```go
type Ctx struct {
  Store *Store
  Token string
  Method string
  Path string              // decoded, no query string
  Query url.Values
  Headers http.Header
  Body []byte
}
func Authorized(ctx *Ctx) bool // Bearer token == ctx.Token, or Basic with user/password == token
func JSONBody(ctx *Ctx) (any, bool) // (nil, true) when no body; (value, true) when parsed; (nil, false) when malformed
```

### respond.go
```go
const JSONType = "application/json; charset=utf-8"
func SendJSON(w http.ResponseWriter, status int, body any, headers map[string]string)
func SendJSONText(w http.ResponseWriter, status int, text string, headers map[string]string)
func SendNoContent(w http.ResponseWriter)              // 204, X-GitHub-Request-Id, no body
func SendBytes(w http.ResponseWriter, status int, data []byte, contentType string, headers map[string]string)
func SendError(w http.ResponseWriter, status int, message string) // JSON {"message": ...}
func SendNotFound(w http.ResponseWriter, what string)  // 404 JSON
func SendUnauthorized(w http.ResponseWriter)            // 401 JSON
```
Every reply carries a non-empty `X-GitHub-Request-Id` (from `NextRequestID()`).
Read `respond.ts` for the exact header set (including `X-OAuth-Scopes`
where the TS sets it) — port it exactly.

### Handlers (exact signatures)
```go
func HandleRest(ctx *Ctx, w http.ResponseWriter) bool
func HandleGraphql(ctx *Ctx, w http.ResponseWriter) bool
func HandleMcp(ctx *Ctx, w http.ResponseWriter) bool
func HandleInfoRefs(ctx *Ctx, w http.ResponseWriter, owner, repoName string)
func HandleUploadPack(ctx *Ctx, w http.ResponseWriter, owner, repoName string)
func HandleReceivePack(ctx *Ctx, w http.ResponseWriter, owner, repoName string)
```
`HandleRest/Graphql/Mcp` return false when the path is not theirs (so the
router falls through to a 404); the git handlers always answer (or 404).

## 3. Concurrency — the rule that prevents races and deadlocks

- `Store.mu` is a `sync.RWMutex`. **Store methods NEVER lock.**
  The HANDLER locks once per request and holds it for the whole handler call:
  - Read-only routes (GETs, `info/refs`, `upload-pack`, GraphQL queries):
    `mu.RLock()`.
  - Mutating routes (receive-pack, REST POST/PUT/PATCH/DELETE, GraphQL
    mutations, MCP tools that mutate): `mu.Lock()`.
  - When in doubt, take `mu.Lock()`.
- `packCache` and `isoCache` and per-repo `IssueCache` are `sync.Map` —
  safe under either lock, including cache writes on a read path.
- `NextRequestID` uses `sync/atomic`. `TrialNowSec` reads the env once at
  startup into a package var (no per-request env read).
- Do NOT hold the store lock across network I/O: the body is already read
  before routing, and response writing happens after the handler returns
  its data — but the TS handlers write directly to `res` inside the lock.
  Keep the same shape (write inside the handler while holding the lock):
  response writes are local socket writes, not network round-trips, and the
  TS version does the same. Do NOT call out to the env (`/register`,
  `/done`) while holding the store lock.

## 4. Performance rules (from the TS rounds — do not regress these)

- No per-request URL parsing: use `r.URL.Path` (already parsed by net/http).
  Split path/query by hand where the TS does (REST segments, git regex).
- REST routes checked before the git regex; the regex runs only on paths
  containing `.git/`.
- Bodyless requests (every GET) must not wait for a body read.
- Skip `decodeURIComponent` work when the path has no `%`.
- Cache: the fetch-pack cache (keyed by fetch parameters, cleared on any
  new object), the precomputed info/refs advertisement, the cached issue
  JSON text, the commit-parents cache (v3 `store.ts`: a per-repo
  `commitParents` map so push's fast-forward check does not re-parse every
  commit — port it; in Go it is a `map[string][]string` on Repo, guarded
  by the store lock).
- Pack writing: deflate level 1. Pack reading: the v3 Adler-32 stream-end
  finder (inflate once, scan for the trailer), NOT a binary search.
- Keep-alive must work: the suite reuses connections (the read workload is
  2,000 sequential GETs on shared connections). net/http handles this if
  the handler always reads (or discards) the request body — the router
  reads the body fully before routing, exactly like the TS `readBody`.

## 5. HTTP layer decision

- Builder B uses `net/http` (`http.Server` + a `http.HandlerFunc` that
  builds the `Ctx` and routes). This is the default.
- If the binary exceeds 8 MiB, B reports to the coordinator immediately
  with the exact size and the `go version -m` / `go list` breakdown — do
  not switch layers unilaterally.

## 6. Lifecycle (main.go + main.py)

- `main.go` (port of main.ts): read `MAKEFUN_ENV_URL`, exit 1 if missing;
  `net.Listen("tcp", "127.0.0.1:0")`; print `[forge] listening on port N`
  to stdout (the local harness parses this); POST `{"port": N}` to
  `$MAKEFUN_ENV_URL/register` with retries (250 ms start, doubling, cap
  2 s); 200 or 409 = recorded, 400/405 = fatal; poll GET `$MAKEFUN_ENV_URL/done`
  until `{"done": true}`; close the server; exit 0.
- `main.py` (repo root): `os.execv` the `./forge` binary with the same
  argv. It must contain at least one line authored in Builder B's own
  session (the transcript gate requires an authored line in the entrypoint).
- The entry must NOT bind 8080 (the env owns it).

## 7. Authorship (transcript gate — mandatory)

- Write ALL Go code fresh in your own session (write/edit tool calls).
  Never paste TS verbatim (different language — this is natural), never
  paste code from this DESIGN.md or from a coordinator message into a file.
  First appearance decides authorship: a line is "authored" only if its
  first appearance is in YOUR write/edit.
- `thinking_revised`: some file must take its final content from at least
  2 assistant messages that write/edit it — so revise each file at least
  once (write, then a genuine edit pass), do not write a file once and
  never touch it again.
- Do NOT commit the binary to git until the coordinator says so (the binary
  is built locally for measurement; the submitted commit includes it only
  via the coordinator's submit step — the coordinator will confirm the
  size cap and the `.makefun/owner.json` requirement then).

## 8. Testing your module

- `gofmt -l .` must be clean; `go vet ./...` must pass; `go build` must pass.
- You cannot run the full 120-check suite alone until integration, but you
  CAN and MUST: `go vet`, `go test` with a few unit tests you write
  (pack round-trip, delta apply, pkt-line codec, tree/commit build+parse —
  port the invariants from the TS behaviour), and a `go build` that proves
  your file compiles against the contract types.
- If a contract signature is wrong or unworkable, tell the coordinator
  immediately — do NOT silently change a shared signature (that breaks the
  other builders). The coordinator will amend this file and re-brief all.
