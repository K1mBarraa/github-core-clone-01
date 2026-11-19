package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// testGraphqlCtx builds a Ctx for POST /api/graphql with a JSON body.
// token "" means no Authorization header is sent.
func testGraphqlCtx(store *Store, token, body string) *Ctx {
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return &Ctx{
		Store:   store,
		Token:   "test-token",
		Method:  "POST",
		Path:    "/api/graphql",
		Query:   url.Values{},
		Headers: headers,
		Body:    []byte(body),
	}
}

// testGraphqlStore seeds a store with one repo and one issue.
func testGraphqlStore(t *testing.T) (*Store, *Repo, *Issue) {
	t.Helper()
	store := NewStore()
	repo := store.CreateRepo(ViewerLogin, "demo", "demo repo", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	issue := store.CreateIssue(repo, "First issue", "first body", nil)
	if issue == nil {
		t.Fatal("CreateIssue returned nil")
	}
	return store, repo, issue
}

// testGraphqlCall drives HandleGraphql and decodes the JSON response body.
func testGraphqlCall(t *testing.T, ctx *Ctx) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	if !HandleGraphql(ctx, rec) {
		t.Fatal("HandleGraphql returned false for /api/graphql")
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, decoded
}

func testGraphqlData(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatalf("response has no data object: %v", decoded)
	}
	return data
}

func testGraphqlErrors(t *testing.T, decoded map[string]any) []any {
	t.Helper()
	errs, _ := decoded["errors"].([]any)
	return errs
}

func TestHandleGraphqlWrongPath(t *testing.T) {
	store := NewStore()
	ctx := testGraphqlCtx(store, "test-token", `{"query":"{ viewer { login } }"}`)
	ctx.Path = "/api/graphq"
	rec := httptest.NewRecorder()
	if HandleGraphql(ctx, rec) {
		t.Error("HandleGraphql should return false for a non-graphql path")
	}
}

func TestHandleGraphqlUnauthorizedWithoutToken(t *testing.T) {
	store := NewStore()
	ctx := testGraphqlCtx(store, "", `{"query":"{ viewer { login } }"}`)
	rec := httptest.NewRecorder()
	if !HandleGraphql(ctx, rec) {
		t.Fatal("HandleGraphql returned false for /api/graphql")
	}
	if rec.Code != 401 {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if decoded["message"] != "Bad credentials" {
		t.Fatalf("expected Bad credentials message, got %v", decoded)
	}
}

func TestHandleGraphqlMalformedJSON(t *testing.T) {
	store := NewStore()
	ctx := testGraphqlCtx(store, "test-token", `{not json`)
	rec := httptest.NewRecorder()
	if !HandleGraphql(ctx, rec) {
		t.Fatal("HandleGraphql returned false for /api/graphql")
	}
	if rec.Code != 400 {
		t.Fatalf("expected 400 for malformed JSON, got %d", rec.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if decoded["message"] != "Invalid JSON body" {
		t.Fatalf("expected Invalid JSON body message, got %v", decoded)
	}
}

func TestHandleGraphqlViewer(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	code, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", `{"query":"{ viewer { login id name email url } }"}`))
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	viewer := testGraphqlData(t, decoded)["viewer"].(map[string]any)
	if viewer["login"] != ViewerLogin {
		t.Fatalf("viewer login = %v, want %q", viewer["login"], ViewerLogin)
	}
	if viewer["id"] != float64(ViewerID) {
		t.Fatalf("viewer id = %v, want %d", viewer["id"], ViewerID)
	}
	if viewer["name"] != "ada iversen" {
		t.Fatalf("viewer name = %v, want ada iversen", viewer["name"])
	}
	if viewer["email"] != ViewerEmail {
		t.Fatalf("viewer email = %v, want %q", viewer["email"], ViewerEmail)
	}
}

func TestHandleGraphqlRepositoryByName(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	code, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ repository(owner: \"adaiversen\", name: \"demo\") { name nameWithOwner description url isPrivate fork viewerPermission hasIssuesEnabled owner { login } } }"}`))
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	repo := testGraphqlData(t, decoded)["repository"].(map[string]any)
	if repo["name"] != "demo" || repo["nameWithOwner"] != "adaiversen/demo" {
		t.Fatalf("repository = %v", repo)
	}
	if repo["description"] != "demo repo" || repo["url"] != "https://github.com/adaiversen/demo" {
		t.Fatalf("repository fields = %v", repo)
	}
	if repo["isPrivate"] != false || repo["fork"] != false || repo["viewerPermission"] != "ADMIN" || repo["hasIssuesEnabled"] != true {
		t.Fatalf("repository flags = %v", repo)
	}
}

func TestHandleGraphqlRepositoryMissing(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ repository(owner: \"adaiversen\", name: \"nope\") { name } }"}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if testGraphqlData(t, decoded)["repository"] != nil {
		t.Fatalf("expected null repository, got %v", testGraphqlData(t, decoded)["repository"])
	}
}

func TestHandleGraphqlIssueLookup(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ repository(owner: \"adaiversen\", name: \"demo\") { issue(number: 1) { number title state body url resourcePath author { login } } } }"}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	repo := testGraphqlData(t, decoded)["repository"].(map[string]any)
	issue := repo["issue"].(map[string]any)
	if issue["number"] != float64(1) || issue["title"] != "First issue" || issue["state"] != "OPEN" {
		t.Fatalf("issue = %v", issue)
	}
	if issue["url"] != "https://github.com/adaiversen/demo/issues/1" {
		t.Fatalf("issue url = %v", issue["url"])
	}
	if issue["resourcePath"] != "/adaiversen/demo/issues/1" {
		t.Fatalf("issue resourcePath = %v", issue["resourcePath"])
	}
}

func TestHandleGraphqlIssueConnectionPagination(t *testing.T) {
	store, repo, _ := testGraphqlStore(t)
	store.CreateIssue(repo, "Second", "", nil)
	store.CreateIssue(repo, "Third", "", nil)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ repository(owner: \"adaiversen\", name: \"demo\") { issues(first: 2) { totalCount nodes { number } pageInfo { hasNextPage endCursor } } } }"}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	repoData := testGraphqlData(t, decoded)["repository"].(map[string]any)
	conn := repoData["issues"].(map[string]any)
	if conn["totalCount"] != float64(3) {
		t.Fatalf("totalCount = %v, want 3", conn["totalCount"])
	}
	nodes := conn["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	pageInfo := conn["pageInfo"].(map[string]any)
	if pageInfo["hasNextPage"] != true {
		t.Fatalf("hasNextPage = %v, want true", pageInfo["hasNextPage"])
	}
	endCursor, _ := pageInfo["endCursor"].(string)
	if endCursor == "" {
		t.Fatal("endCursor is empty")
	}
	// Second page via the cursor: exactly one node remains.
	query := `{"query":"{ repository(owner: \"adaiversen\", name: \"demo\") { issues(first: 2, after: \"` + endCursor + `\") { nodes { number } pageInfo { hasNextPage } } } }"}`
	_, decoded2 := testGraphqlCall(t, testGraphqlCtx(store, "test-token", query))
	if errs := testGraphqlErrors(t, decoded2); len(errs) != 0 {
		t.Fatalf("unexpected errors on page 2: %v", errs)
	}
	conn2 := testGraphqlData(t, decoded2)["repository"].(map[string]any)["issues"].(map[string]any)
	if nodes2 := conn2["nodes"].([]any); len(nodes2) != 1 {
		t.Fatalf("page 2 nodes = %d, want 1", len(nodes2))
	}
	if conn2["pageInfo"].(map[string]any)["hasNextPage"] != false {
		t.Fatalf("page 2 hasNextPage should be false")
	}
}

func TestHandleGraphqlNodeLookup(t *testing.T) {
	store, repo, issue := testGraphqlStore(t)
	issueID := store.IssueGlobalID(repo, issue)
	query := `{"query":"{ node(id: \"` + issueID + `\") { __typename ... on Issue { number title } } }"}`
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", query))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	node := testGraphqlData(t, decoded)["node"].(map[string]any)
	if node["__typename"] != "Issue" || node["number"] != float64(1) || node["title"] != "First issue" {
		t.Fatalf("node = %v", node)
	}
	// Unknown global id resolves to null.
	_, decoded2 := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ node(id: \"bogus\") { __typename } }"}`))
	if testGraphqlData(t, decoded2)["node"] != nil {
		t.Fatalf("expected null node for bogus id, got %v", testGraphqlData(t, decoded2)["node"])
	}
}

func TestHandleGraphqlCreateIssueMutation(t *testing.T) {
	store, repo, _ := testGraphqlStore(t)
	repoID := store.RepoGlobalID(repo)
	query := `{"query":"mutation { createIssue(input: {repositoryId: \"` + repoID + `\", title: \"Via GraphQL\", body: \"created by test\", clientMutationId: \"m1\"}) { issue { number title state } clientMutationId } }"}`
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", query))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	payload := testGraphqlData(t, decoded)["createIssue"].(map[string]any)
	if payload["clientMutationId"] != "m1" {
		t.Fatalf("clientMutationId = %v, want m1", payload["clientMutationId"])
	}
	issue := payload["issue"].(map[string]any)
	if issue["number"] != float64(2) || issue["title"] != "Via GraphQL" || issue["state"] != "OPEN" {
		t.Fatalf("created issue = %v", issue)
	}
	if got := store.FindIssue(repo, 2); got == nil || got.Title != "Via GraphQL" {
		t.Fatal("mutation did not persist the issue in the store")
	}
}

func TestHandleGraphqlCloseIssueMutation(t *testing.T) {
	store, repo, issue := testGraphqlStore(t)
	issueID := store.IssueGlobalID(repo, issue)
	query := `{"query":"mutation { closeIssue(input: {issueId: \"` + issueID + `\"}) { issue { number state } } }"}`
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", query))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	closed := testGraphqlData(t, decoded)["closeIssue"].(map[string]any)["issue"].(map[string]any)
	if closed["state"] != "CLOSED" {
		t.Fatalf("closed issue state = %v, want CLOSED", closed["state"])
	}
	if store.FindIssue(repo, 1).State != "closed" {
		t.Fatal("mutation did not close the issue in the store")
	}
}

func TestHandleGraphqlMutationUnknownRepo(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"mutation { createIssue(input: {repositoryId: \"bogus\", title: \"x\"}) { issue { number } } }"}`))
	errs := testGraphqlErrors(t, decoded)
	if len(errs) == 0 {
		t.Fatal("expected an error for an unknown repository id")
	}
	msg, _ := errs[0].(map[string]any)["message"].(string)
	if msg != "Repository not found" {
		t.Fatalf("error message = %q, want Repository not found", msg)
	}
	if testGraphqlData(t, decoded)["createIssue"] != nil {
		t.Fatalf("expected null createIssue payload, got %v", testGraphqlData(t, decoded)["createIssue"])
	}
}

func TestHandleGraphqlUnknownField(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ viewer { login nosuchfield } }"}`))
	errs := testGraphqlErrors(t, decoded)
	if len(errs) == 0 {
		t.Fatal("expected an error for an unknown field")
	}
	msg, _ := errs[0].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "nosuchfield") || !strings.Contains(msg, "User") {
		t.Fatalf("error message = %q, want it to name nosuchfield on User", msg)
	}
	viewer := testGraphqlData(t, decoded)["viewer"].(map[string]any)
	if viewer["login"] != ViewerLogin {
		t.Fatalf("viewer login = %v, want %q", viewer["login"], ViewerLogin)
	}
	if viewer["nosuchfield"] != nil {
		t.Fatalf("unknown field should be null, got %v", viewer["nosuchfield"])
	}
}

func TestHandleGraphqlParseError(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ viewer { login }"}`))
	errs := testGraphqlErrors(t, decoded)
	if len(errs) == 0 {
		t.Fatal("expected a parse error")
	}
	// A parse error still returns a data key (null), per the TS executor.
	if _, ok := decoded["data"]; !ok {
		t.Fatal("expected a data key alongside parse errors")
	}
	if decoded["data"] != nil {
		t.Fatalf("expected null data on parse error, got %v", decoded["data"])
	}
}

func TestHandleGraphqlMissingQuery(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", `{}`))
	errs := testGraphqlErrors(t, decoded)
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing query string")
	}
	msg, _ := errs[0].(map[string]any)["message"].(string)
	if msg != "Must provide query string." {
		t.Fatalf("error message = %q", msg)
	}
}

func TestHandleGraphqlBatchRaw(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	ctx := testGraphqlCtx(store, "test-token",
		`[{"query":"{ viewer { login } }"},{"query":"{ repository(owner: \"adaiversen\", name: \"demo\") { name } }"}]`)
	rec := httptest.NewRecorder()
	if !HandleGraphql(ctx, rec) {
		t.Fatal("HandleGraphql returned false for /api/graphql")
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var batch []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch response is not a JSON array: %v (body %q)", err, rec.Body.String())
	}
	if len(batch) != 2 {
		t.Fatalf("batch length = %d, want 2", len(batch))
	}
	if batch[0]["data"].(map[string]any)["viewer"].(map[string]any)["login"] != ViewerLogin {
		t.Fatalf("batch[0] = %v", batch[0])
	}
	if batch[1]["data"].(map[string]any)["repository"].(map[string]any)["name"] != "demo" {
		t.Fatalf("batch[1] = %v", batch[1])
	}
}

func TestHandleGraphqlGetQuery(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	ctx := &Ctx{
		Store:   store,
		Token:   "test-token",
		Method:  "GET",
		Path:    "/api/graphql",
		Query:   url.Values{"query": []string{"{ viewer { login } }"}},
		Headers: http.Header{"Authorization": []string{"Bearer test-token"}},
	}
	code, decoded := testGraphqlCall(t, ctx)
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if testGraphqlData(t, decoded)["viewer"].(map[string]any)["login"] != ViewerLogin {
		t.Fatalf("GET viewer = %v", decoded)
	}
}

func TestHandleGraphqlIntrospection(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ __type(name: \"SearchType\") { name kind enumValues { name } } }"}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	typ := testGraphqlData(t, decoded)["__type"].(map[string]any)
	if typ["name"] != "SearchType" || typ["kind"] != "ENUM" {
		t.Fatalf("__type = %v", typ)
	}
	enumValues := typ["enumValues"].([]any)
	if len(enumValues) != 9 {
		t.Fatalf("enumValues = %d, want 9", len(enumValues))
	}
	if enumValues[0].(map[string]any)["name"] != "CODE" {
		t.Fatalf("first enum value = %v, want CODE", enumValues[0])
	}
	_, decoded2 := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ __type(name: \"Repository\") { fields { name } } }"}`))
	fields := testGraphqlData(t, decoded2)["__type"].(map[string]any)["fields"].([]any)
	found := false
	for _, f := range fields {
		if f.(map[string]any)["name"] == "nameWithOwner" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Repository fields lack nameWithOwner: %v", fields)
	}
}

func TestHandleGraphqlDirectivesAndFragments(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ viewer { login name @skip(if: true) email @include(if: false) } }"}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	viewer := testGraphqlData(t, decoded)["viewer"].(map[string]any)
	if viewer["login"] != ViewerLogin {
		t.Fatalf("login = %v", viewer["login"])
	}
	if _, ok := viewer["name"]; ok {
		t.Fatalf("skipped field should be absent, got %v", viewer)
	}
	if _, ok := viewer["email"]; ok {
		t.Fatalf("excluded field should be absent, got %v", viewer)
	}
	_, decoded2 := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"{ viewer { ...UserBits } } fragment UserBits on User { login id } "}`))
	if errs := testGraphqlErrors(t, decoded2); len(errs) != 0 {
		t.Fatalf("unexpected errors for fragment: %v", errs)
	}
	viewer2 := testGraphqlData(t, decoded2)["viewer"].(map[string]any)
	if viewer2["login"] != ViewerLogin || viewer2["id"] != float64(ViewerID) {
		t.Fatalf("fragment viewer = %v", viewer2)
	}
}

func TestHandleGraphqlPullRequestFlow(t *testing.T) {
	store, repo, issue := testGraphqlStore(t)
	// Two commits on an empty tree: base on main, head on feature.
	tree := store.BuildTreeFromFlat(repo, map[string]FlatFile{})
	base := store.CreateCommit(repo, tree, nil, "base", nil)
	head := store.CreateCommit(repo, tree, []string{base}, "head", nil)
	repo.Refs["refs/heads/main"] = base
	repo.Refs["refs/heads/feature"] = head
	repoID := store.RepoGlobalID(repo)

	createQuery := `{"query":"mutation { createPullRequest(input: {repositoryId: \"` + repoID +
		`\", title: \"Add feature\", headRefName: \"feature\", baseRefName: \"main\"}) { pullRequest { number title state merged mergeable headRefName baseRefName } } }"}`
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token", createQuery))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors creating PR: %v", errs)
	}
	pr := testGraphqlData(t, decoded)["createPullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	if pr["number"] != float64(1) || pr["state"] != "OPEN" || pr["merged"] != false {
		t.Fatalf("created PR = %v", pr)
	}
	if pr["mergeable"] != "MERGEABLE" {
		t.Fatalf("mergeable = %v, want MERGEABLE", pr["mergeable"])
	}
	prID := store.PullGlobalID(repo, store.FindPull(repo, 1))
	mergeQuery := `{"query":"mutation { mergePullRequest(input: {pullRequestId: \"` + prID +
		`\"}) { pullRequest { number state merged mergedAt mergeCommitSha } } }"}`
	_, decoded2 := testGraphqlCall(t, testGraphqlCtx(store, "test-token", mergeQuery))
	if errs := testGraphqlErrors(t, decoded2); len(errs) != 0 {
		t.Fatalf("unexpected errors merging PR: %v", errs)
	}
	merged := testGraphqlData(t, decoded2)["mergePullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	if merged["state"] != "MERGED" || merged["merged"] != true {
		t.Fatalf("merged PR = %v", merged)
	}
	if merged["mergedAt"] == nil || merged["mergeCommitSha"] == nil {
		t.Fatalf("merged PR lacks mergedAt/mergeCommitSha: %v", merged)
	}
	if got := store.FindPull(repo, 1); !got.Merged || got.State != "closed" {
		t.Fatal("merge did not persist in the store")
	}
	// addComment on the seeded issue.
	issueID := store.IssueGlobalID(repo, issue)
	commentQuery := `{"query":"mutation { addComment(input: {subjectId: \"` + issueID +
		`\", body: \"hello\"}) { commentEdge { node { body } cursor } } }"}`
	_, decoded3 := testGraphqlCall(t, testGraphqlCtx(store, "test-token", commentQuery))
	if errs := testGraphqlErrors(t, decoded3); len(errs) != 0 {
		t.Fatalf("unexpected errors adding comment: %v", errs)
	}
	edge := testGraphqlData(t, decoded3)["addComment"].(map[string]any)["commentEdge"].(map[string]any)
	if edge["node"].(map[string]any)["body"] != "hello" {
		t.Fatalf("comment edge = %v", edge)
	}
	if edge["cursor"] == nil || edge["cursor"] == "" {
		t.Fatalf("comment edge lacks cursor: %v", edge)
	}
}

func TestHandleGraphqlVariables(t *testing.T) {
	store, _, _ := testGraphqlStore(t)
	_, decoded := testGraphqlCall(t, testGraphqlCtx(store, "test-token",
		`{"query":"query GetRepo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { name } }","variables":{"owner":"adaiversen","name":"demo"}}`))
	if errs := testGraphqlErrors(t, decoded); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if testGraphqlData(t, decoded)["repository"].(map[string]any)["name"] != "demo" {
		t.Fatalf("repository via variables = %v", decoded)
	}
}
