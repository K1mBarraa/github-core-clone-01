package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The other builders' real store.go / ctx.go / respond.go / util.go have
// landed, so these tests drive the full HTTP path through HandleMcp with a
// hand-built Ctx + httptest.ResponseRecorder, plus mcpHandleRpc directly
// for the pure protocol cases.

func mcpTestCtx() *Ctx {
	return &Ctx{
		Store:   NewStore(),
		Token:   "test-token",
		Method:  "POST",
		Path:    "/mcp",
		Headers: http.Header{"Authorization": []string{"Bearer test-token"}},
	}
}

// mcpHTTP posts a raw JSON-RPC body through HandleMcp and returns the HTTP
// status and the decoded envelope (empty map when the body is empty, as
// for a 202 notification reply).
func mcpHTTP(t *testing.T, ctx *Ctx, body string) (int, map[string]any) {
	t.Helper()
	ctx.Body = []byte(body)
	rec := httptest.NewRecorder()
	if !HandleMcp(ctx, rec) {
		t.Fatal("HandleMcp returned false for POST /mcp")
	}
	env := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("response body is not JSON: %v (body %q)", err, rec.Body.String())
		}
	}
	return rec.Code, env
}

// toolText extracts the text of a tools/call result from a decoded
// envelope, plus its isError flag.
func toolText(t *testing.T, env map[string]any) (string, bool) {
	t.Helper()
	result, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no result object: %v", env)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("result content = %v", result["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "text" {
		t.Fatalf("result content part = %v", content[0])
	}
	text, _ := part["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func mcpCallBody(method string, id any, params string) string {
	return `{"jsonrpc":"2.0","id":` + mcpJSONValue(id) + `,"method":"` + method + `","params":` + params + `}`
}

func mcpJSONValue(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mcpCall(t *testing.T, ctx *Ctx, req map[string]any) any {
	t.Helper()
	return mcpHandleRpc(ctx, req)
}

// rpcResult pulls the result out of a success envelope, failing the test
// on any other shape.
func rpcResult(t *testing.T, resp any) any {
	t.Helper()
	ok, isOK := resp.(mcpOKResponse)
	if !isOK {
		t.Fatalf("expected success envelope, got %#v", resp)
	}
	if ok.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", ok.JSONRPC)
	}
	return ok.Result
}

func rpcErr(t *testing.T, resp any) mcpErrorBody {
	t.Helper()
	e, isErr := resp.(mcpErrorResponse)
	if !isErr {
		t.Fatalf("expected error envelope, got %#v", resp)
	}
	return e.Error
}

// callResult pulls the tools/call result payload out of an envelope.
func callResult(t *testing.T, resp any) mcpCallResult {
	t.Helper()
	res, ok := rpcResult(t, resp).(mcpCallResult)
	if !ok {
		t.Fatalf("expected mcpCallResult, got %#v", rpcResult(t, resp))
	}
	return res
}

func TestHandleMcpWrongPath(t *testing.T) {
	ctx := mcpTestCtx()
	ctx.Path = "/not-mcp"
	if HandleMcp(ctx, httptest.NewRecorder()) {
		t.Fatal("HandleMcp must return false for a non-/mcp path")
	}
}

func TestHandleMcpGetIsNotFoundPath(t *testing.T) {
	ctx := mcpTestCtx()
	ctx.Method = "GET"
	rec := httptest.NewRecorder()
	if !HandleMcp(ctx, rec) {
		t.Fatal("HandleMcp must return true (answer 404) for GET /mcp")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /mcp status = %d, want 404", rec.Code)
	}
}

func TestHandleMcpUnauthorizedWithoutToken(t *testing.T) {
	ctx := mcpTestCtx()
	ctx.Headers = http.Header{} // no Authorization header
	rec := httptest.NewRecorder()
	if !HandleMcp(ctx, rec) {
		t.Fatal("HandleMcp must return true (answer 401) for POST /mcp without a token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /mcp status = %d, want 401", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["message"] != "Bad credentials" {
		t.Fatalf("401 body = %v, want Bad credentials", body)
	}
}

func TestHandleMcpAuthPrecedesParse(t *testing.T) {
	// TS order: auth is checked before the body is parsed, so a malformed
	// body without a token is a 401, not a 400.
	ctx := mcpTestCtx()
	ctx.Headers = http.Header{}
	ctx.Body = []byte(`{not json`)
	rec := httptest.NewRecorder()
	if !HandleMcp(ctx, rec) {
		t.Fatal("HandleMcp must return true for an unauthenticated malformed-body POST")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated malformed POST status = %d, want 401", rec.Code)
	}
}

func TestHandleMcpInitializeEndToEnd(t *testing.T) {
	ctx := mcpTestCtx()
	status, env := mcpHTTP(t, ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if status != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", status)
	}
	result := env["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v, want 2025-06-18", result["protocolVersion"])
	}
	if env["id"] != float64(1) {
		t.Fatalf("id echoed as %v, want 1", env["id"])
	}
	if env["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", env["jsonrpc"])
	}
}

func TestHandleMcpMalformedJSON400(t *testing.T) {
	ctx := mcpTestCtx()
	status, env := mcpHTTP(t, ctx, `{not json`)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", status)
	}
	errBody, ok := env["error"].(map[string]any)
	if !ok || errBody["code"] != float64(-32700) || errBody["message"] != "Parse error" {
		t.Fatalf("malformed JSON error = %v, want -32700 Parse error", env["error"])
	}
	if env["id"] != nil {
		t.Fatalf("parse error id = %v, want null", env["id"])
	}
}

func TestHandleMcpArrayBody400(t *testing.T) {
	// A JSON array is valid JSON but not a request object: -32600.
	ctx := mcpTestCtx()
	status, env := mcpHTTP(t, ctx, `[1,2,3]`)
	if status != http.StatusBadRequest {
		t.Fatalf("array body status = %d, want 400", status)
	}
	errBody, _ := env["error"].(map[string]any)
	if errBody["code"] != float64(-32600) || errBody["message"] != "Invalid Request" {
		t.Fatalf("array body error = %v, want -32600 Invalid Request", env["error"])
	}
}

func TestHandleMcpNotification202(t *testing.T) {
	ctx := mcpTestCtx()
	ctx.Body = []byte(`{"jsonrpc":"2.0","method":"ping"}`)
	rec := httptest.NewRecorder()
	if !HandleMcp(ctx, rec) {
		t.Fatal("HandleMcp must return true for a notification")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("notification body = %q, want empty", rec.Body.String())
	}
	if rec.Header().Get("X-GitHub-Request-Id") == "" {
		t.Fatal("notification reply is missing X-GitHub-Request-Id")
	}
}

func TestHandleMcpCreateRepoAndGetRepo(t *testing.T) {
	ctx := mcpTestCtx()
	// Mutating tools/call: create_repo succeeds and the repo is visible
	// to a later read via get_repo on the same store.
	_, env := mcpHTTP(t, ctx, mcpCallBody("tools/call", 1,
		`{"name":"create_repo","arguments":{"name":"demo","description":"d","private":false}}`))
	text, isErr := toolText(t, env)
	if isErr {
		t.Fatalf("create_repo failed: %s", text)
	}
	var repo map[string]any
	if err := json.Unmarshal([]byte(text), &repo); err != nil {
		t.Fatalf("create_repo text is not JSON: %v", err)
	}
	if repo["name"] != "demo" || repo["full_name"] != "adaiversen/demo" {
		t.Fatalf("created repo = %v", repo)
	}

	_, env = mcpHTTP(t, ctx, mcpCallBody("tools/call", 2,
		`{"name":"get_repo","arguments":{"owner":"adaiversen","repo":"demo"}}`))
	text, isErr = toolText(t, env)
	if isErr {
		t.Fatalf("get_repo failed: %s", text)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(text), &got)
	if got["name"] != "demo" {
		t.Fatalf("get_repo = %v", got)
	}
}

func TestHandleMcpIssueCommentFlow(t *testing.T) {
	ctx := mcpTestCtx()
	mcpHTTP(t, ctx, mcpCallBody("tools/call", 1,
		`{"name":"create_repo","arguments":{"name":"flow"}}`))

	_, env := mcpHTTP(t, ctx, mcpCallBody("tools/call", 2,
		`{"name":"create_issue","arguments":{"owner":"adaiversen","repo":"flow","title":"Bug","body":"it broke"}}`))
	text, isErr := toolText(t, env)
	if isErr {
		t.Fatalf("create_issue failed: %s", text)
	}
	var issue map[string]any
	_ = json.Unmarshal([]byte(text), &issue)
	if issue["number"] != float64(1) || issue["title"] != "Bug" || issue["state"] != "open" {
		t.Fatalf("created issue = %v", issue)
	}

	_, env = mcpHTTP(t, ctx, mcpCallBody("tools/call", 3,
		`{"name":"create_comment","arguments":{"owner":"adaiversen","repo":"flow","issue_number":1,"body":"looking"}}`))
	text, isErr = toolText(t, env)
	if isErr {
		t.Fatalf("create_comment failed: %s", text)
	}
	var comment map[string]any
	_ = json.Unmarshal([]byte(text), &comment)
	if comment["body"] != "looking" {
		t.Fatalf("created comment = %v", comment)
	}

	_, env = mcpHTTP(t, ctx, mcpCallBody("tools/call", 4,
		`{"name":"list_issues","arguments":{"owner":"adaiversen","repo":"flow"}}`))
	text, isErr = toolText(t, env)
	if isErr {
		t.Fatalf("list_issues failed: %s", text)
	}
	var issues []any
	_ = json.Unmarshal([]byte(text), &issues)
	if len(issues) != 1 {
		t.Fatalf("list_issues = %s, want 1 issue", text)
	}
}

func TestHandleMcpMergePullRequest(t *testing.T) {
	ctx := mcpTestCtx()
	mcpHTTP(t, ctx, mcpCallBody("tools/call", 1,
		`{"name":"create_repo","arguments":{"name":"mergerepo"}}`))
	repo := ctx.Store.GetRepo("adaiversen", "mergerepo")
	if repo == nil {
		t.Fatal("repo not found after create_repo")
	}
	// Two branches off one commit, so the merge is conflict-free.
	blob := ctx.Store.PutObject(repo, ObjBlob, []byte("hello\n"))
	tree := ctx.Store.PutObject(repo, ObjTree, BuildTree([]TreeEntry{{Mode: "100644", Name: "a.txt", OID: blob}}))
	commit := ctx.Store.CreateCommit(repo, tree, nil, "base", nil)
	ctx.Store.SetRef(repo, "refs/heads/main", commit)
	ctx.Store.SetRef(repo, "refs/heads/feature", commit)

	_, env := mcpHTTP(t, ctx, mcpCallBody("tools/call", 2,
		`{"name":"create_pull_request","arguments":{"owner":"adaiversen","repo":"mergerepo","title":"Change","head":"feature","base":"main"}}`))
	text, isErr := toolText(t, env)
	if isErr {
		t.Fatalf("create_pull_request failed: %s", text)
	}
	var pr map[string]any
	_ = json.Unmarshal([]byte(text), &pr)
	if pr["number"] != float64(1) {
		t.Fatalf("created PR = %v", pr)
	}

	_, env = mcpHTTP(t, ctx, mcpCallBody("tools/call", 3,
		`{"name":"merge_pull_request","arguments":{"owner":"adaiversen","repo":"mergerepo","pull_number":1}}`))
	text, isErr = toolText(t, env)
	if isErr {
		t.Fatalf("merge_pull_request failed: %s", text)
	}
	var merged map[string]any
	_ = json.Unmarshal([]byte(text), &merged)
	if merged["merged"] != true || merged["message"] != "Pull request successfully merged" {
		t.Fatalf("merge result = %v", merged)
	}
	sha, _ := merged["sha"].(string)
	if sha == "" || sha == commit {
		t.Fatalf("merge sha = %q (base commit %q)", sha, commit)
	}
	// Merging again must fail: already merged.
	_, env = mcpHTTP(t, ctx, mcpCallBody("tools/call", 4,
		`{"name":"merge_pull_request","arguments":{"owner":"adaiversen","repo":"mergerepo","pull_number":1}}`))
	text, isErr = toolText(t, env)
	if !isErr || text != "Pull request already merged" {
		t.Fatalf("second merge = isError=%v text=%q, want already merged", isErr, text)
	}
}

func TestInitializeDefaultVersion(t *testing.T) {
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{},
	})
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if result["protocolVersion"] != "2026-07-28" {
		t.Fatalf("protocolVersion = %v, want 2026-07-28", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "github-core-clone-forge" || info["version"] != "1.0.0" {
		t.Fatalf("serverInfo = %v", info)
	}
	if result["instructions"] != mcpInstructions {
		t.Fatalf("instructions = %v", result["instructions"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("capabilities.tools missing")
	}
}

func TestInitializeClientVersionEcho(t *testing.T) {
	// protocolVersion from params wins; _meta wins over params.
	ctx := mcpTestCtx()
	r1 := rpcResult(t, mcpCall(t, ctx, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-03-26"},
	}))
	raw, _ := json.Marshal(r1)
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	if d["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v, want 2025-03-26", d["protocolVersion"])
	}

	r2 := rpcResult(t, mcpCall(t, ctx, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"_meta":  map[string]any{"io.modelcontextprotocol/protocolVersion": "2024-11-05"},
		"params": map[string]any{"protocolVersion": "2025-03-26"},
	}))
	raw, _ = json.Marshal(r2)
	_ = json.Unmarshal(raw, &d)
	if d["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v, want _meta value 2024-11-05", d["protocolVersion"])
	}
}

func TestServerDiscover(t *testing.T) {
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": "abc", "method": "server/discover",
	})
	raw, _ := json.Marshal(rpcResult(t, resp))
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	versions, ok := d["supportedVersions"].([]any)
	if !ok || len(versions) != 5 || versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %v", d["supportedVersions"])
	}
	if d["serverInfo"].(map[string]any)["name"] != "github-core-clone-forge" {
		t.Fatalf("serverInfo = %v", d["serverInfo"])
	}
}

func TestPing(t *testing.T) {
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "ping",
	})
	raw, _ := json.Marshal(rpcResult(t, resp))
	if strings.TrimSpace(string(raw)) != "{}" {
		t.Fatalf("ping result = %s, want {}", raw)
	}
}

func TestToolsList(t *testing.T) {
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	raw, _ := json.Marshal(rpcResult(t, resp))
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	tools, ok := d["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %v", d["tools"])
	}
	want := []string{"create_repo", "get_repo", "create_issue", "list_issues",
		"create_pull_request", "merge_pull_request", "list_repos", "get_issue",
		"create_comment", "list_pull_requests"}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	for i, name := range want {
		tool := tools[i].(map[string]any)
		if tool["name"] != name {
			t.Fatalf("tools[%d].name = %v, want %s", i, tool["name"], name)
		}
		if tool["description"] == "" || tool["inputSchema"] == nil {
			t.Fatalf("tool %s missing description/inputSchema", name)
		}
	}
	createRepo := tools[0].(map[string]any)
	schema := createRepo["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("create_repo required = %v, want [name]", required)
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	ctx := mcpTestCtx()
	errBody := rpcErr(t, mcpCall(t, ctx, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "nope", "arguments": map[string]any{}},
	}))
	if errBody.Code != -32601 || errBody.Message != "Unknown tool: nope" {
		t.Fatalf("unknown tool error = %+v", errBody)
	}
	errBody = rpcErr(t, mcpCall(t, ctx, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "resources/list",
	}))
	if errBody.Code != -32601 || errBody.Message != "Method not found: resources/list" {
		t.Fatalf("unknown method error = %+v", errBody)
	}
}

func TestNotificationGetsNoReply(t *testing.T) {
	ctx := mcpTestCtx()
	// No id at all: notification.
	if got := mcpCall(t, ctx, map[string]any{"jsonrpc": "2.0", "method": "ping"}); got != nil {
		t.Fatalf("notification without id must return nil, got %#v", got)
	}
	// Explicit null id: also a notification.
	if got := mcpCall(t, ctx, map[string]any{"jsonrpc": "2.0", "id": nil, "method": "ping"}); got != nil {
		t.Fatalf("notification with null id must return nil, got %#v", got)
	}
	// Unknown method as a notification: still no reply.
	if got := mcpCall(t, ctx, map[string]any{"jsonrpc": "2.0", "method": "bogus"}); got != nil {
		t.Fatalf("unknown-method notification must return nil, got %#v", got)
	}
}

func TestMissingMethodIsInvalidRequest(t *testing.T) {
	errBody := rpcErr(t, mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 3,
	}))
	if errBody.Code != -32600 || errBody.Message != "Invalid Request: method is required" {
		t.Fatalf("missing method error = %+v", errBody)
	}
}

func TestToolsCallReadGetRepoMissing(t *testing.T) {
	// Read path: get_repo for a repo that does not exist.
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_repo", "arguments": map[string]any{
			"owner": "adaiversen", "repo": "nope",
		}},
	})
	res := callResult(t, resp)
	if !res.IsError || len(res.Content) != 1 || res.Content[0].Type != "text" ||
		res.Content[0].Text != "Repository adaiversen/nope not found" {
		t.Fatalf("get_repo missing = %+v", res)
	}
}

func TestToolsCallOwnerRepoFullName(t *testing.T) {
	// full_name "owner/repo" splits; a foreign owner is rejected before
	// any store lookup.
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_repo", "arguments": map[string]any{
			"full_name": "someone/else",
		}},
	})
	res := callResult(t, resp)
	if !res.IsError || res.Content[0].Text != "Repository someone/else not found" {
		t.Fatalf("full_name foreign owner = %+v", res)
	}
}

func TestToolsCallMutateCreateRepoDuplicate(t *testing.T) {
	// Mutation path: create_repo twice with the same name. The first call
	// succeeds, the second must fail as a duplicate.
	ctx := mcpTestCtx()
	params := map[string]any{"name": "create_repo", "arguments": map[string]any{"name": "dup"}}
	first := callResult(t, mcpCall(t, ctx, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params}))
	if first.IsError {
		t.Fatalf("first create_repo failed: %s", first.Content[0].Text)
	}
	res := callResult(t, mcpCall(t, ctx, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": params}))
	if !res.IsError || res.Content[0].Text != "Repository dup already exists" {
		t.Fatalf("second create_repo = %+v", res)
	}
}

func TestToolsCallCreateRepoMissingName(t *testing.T) {
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_repo", "arguments": map[string]any{}},
	})
	res := callResult(t, resp)
	if !res.IsError || res.Content[0].Text != "name is required" {
		t.Fatalf("create_repo without name = %+v", res)
	}
}

func TestToolsCallListReposEmpty(t *testing.T) {
	// A fresh store lists zero repos: the tool result text is "[]", not null.
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_repos", "arguments": map[string]any{}},
	})
	res := callResult(t, resp)
	if res.IsError || res.Content[0].Text != "[]" {
		t.Fatalf("list_repos on a fresh store = %+v", res)
	}
}

func TestToolsCallMissingRepoErrors(t *testing.T) {
	ctx := mcpTestCtx()
	args := map[string]any{"owner": "adaiversen", "repo": "nope"}
	cases := []struct{ tool, want string }{
		{"list_issues", "Repository adaiversen/nope not found"},
		{"create_issue", "Repository adaiversen/nope not found"},
		{"list_pull_requests", "Repository adaiversen/nope not found"},
		{"create_pull_request", "Repository adaiversen/nope not found"},
		{"merge_pull_request", "Repository adaiversen/nope not found"},
		{"get_issue", "Repository adaiversen/nope not found"},
		{"create_comment", "Repository adaiversen/nope not found"},
	}
	for _, tc := range cases {
		resp := mcpCall(t, ctx, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tc.tool, "arguments": args},
		})
		res := callResult(t, resp)
		if !res.IsError || res.Content[0].Text != tc.want {
			t.Fatalf("%s on missing repo = %+v, want error %q", tc.tool, res, tc.want)
		}
	}
}

func TestToolsCallNonObjectArguments(t *testing.T) {
	// arguments that is not an object behaves as {} (the TS cast reads
	// undefined fields off it).
	resp := mcpCall(t, mcpTestCtx(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_repo", "arguments": "not-an-object"},
	})
	res := callResult(t, resp)
	if !res.IsError || res.Content[0].Text != "Repository / not found" {
		t.Fatalf("non-object arguments = %+v", res)
	}
}
