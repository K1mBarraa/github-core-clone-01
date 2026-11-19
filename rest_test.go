// Unit tests for rest.go (Builder C). The HTTP-level tests drive
// HandleRest with a hand-built Ctx and httptest.ResponseRecorder.
//
// NOTE: rest.go depends on ctx.go (Authorized/JSONBody), respond.go
// (SendJSON/SendError/...) and store.go (the Store methods). While those
// files are still stubs, the HTTP-level tests skip themselves via the
// restLive* probes below (a stub probe fails loudly instead of passing
// vacuously). Once Builders A2/B land their implementations, the same
// tests run for real — no test changes needed.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// restLiveRespond skips the test when respond.go is still a stub.
func restLiveRespond(t *testing.T) {
	t.Helper()
	rec := httptest.NewRecorder()
	SendJSON(rec, 200, map[string]any{"x": 1}, nil)
	if rec.Body.Len() == 0 {
		t.Skip("respond.go still a stub (SendJSON writes nothing)")
	}
}

// restLiveAuth skips the test when ctx.go is still a stub.
func restLiveAuth(t *testing.T) {
	t.Helper()
	ctx := &Ctx{
		Store:   NewStore(),
		Token:   "test-token",
		Headers: http.Header{"Authorization": []string{"Bearer test-token"}},
	}
	if !Authorized(ctx) {
		t.Skip("ctx.go still a stub (Authorized always false)")
	}
}

// restLiveJSONBody skips the test when ctx.go JSONBody is still a stub.
func restLiveJSONBody(t *testing.T) {
	t.Helper()
	ctx := &Ctx{Body: []byte(`{"a":1}`)}
	v, ok := JSONBody(ctx)
	if !ok {
		t.Skip("ctx.go JSONBody reports malformed JSON for valid JSON")
	}
	if _, isMap := v.(map[string]any); !isMap {
		t.Skip("ctx.go JSONBody still a stub (returns nil for an object body)")
	}
}

// restLiveStore skips the test when store.go is still a stub, and
// otherwise returns a fresh live store.
func restLiveStore(t *testing.T) *Store {
	t.Helper()
	probe := NewStore()
	if probe.CreateRepo(ViewerLogin, "probe", "", false) == nil {
		t.Skip("store.go still a stub (CreateRepo returns nil)")
	}
	return NewStore()
}

// restTestCtx builds a Ctx for a request line like "GET /api/v3/user".
// The path is split at '?'; the query is parsed into url.Values.
func restTestCtx(t *testing.T, store *Store, method, rawPath, body string, auth bool) *Ctx {
	t.Helper()
	path := rawPath
	q := url.Values{}
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		path = rawPath[:i]
		var err error
		q, err = url.ParseQuery(rawPath[i+1:])
		if err != nil {
			t.Fatalf("bad query in %q: %v", rawPath, err)
		}
	}
	h := http.Header{}
	if auth {
		h.Set("Authorization", "Bearer test-token")
	}
	return &Ctx{
		Store:   store,
		Token:   "test-token",
		Method:  method,
		Path:    path,
		Query:   q,
		Headers: h,
		Body:    []byte(body),
	}
}

// restCall runs HandleRest and returns the recorder; it fails the test
// when the route was not handled.
func restCall(t *testing.T, ctx *Ctx) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if !HandleRest(ctx, rec) {
		t.Fatalf("HandleRest did not handle %s %s", ctx.Method, ctx.Path)
	}
	return rec
}

// restDecode decodes a recorder body as JSON into a fresh value.
func restDecode(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response body is not JSON (%v): %q", err, rec.Body.String())
	}
	return v
}

func restObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	v := restDecode(t, rec)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("response body is not a JSON object: %q", rec.Body.String())
	}
	return m
}

func restArray(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	v := restDecode(t, rec)
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("response body is not a JSON array: %q", rec.Body.String())
	}
	return a
}

func TestRestSegmentsOf(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/api/v3/repos/a/b/issues/1", []string{"api", "v3", "repos", "a", "b", "issues", "1"}},
		{"/api/v3/repos/a%20b/c", []string{"api", "v3", "repos", "a b", "c"}},
		{"/api/v3//repos/", []string{"api", "v3", "repos"}},
		{"/api/v3/repos/%zz/c", []string{"api", "v3", "repos", "%zz", "c"}},
		{"/", []string{}},
		{"", []string{}},
	}
	for _, c := range cases {
		got := restSegmentsOf(c.path)
		if len(got) != len(c.want) {
			t.Fatalf("segmentsOf(%q) = %v, want %v", c.path, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("segmentsOf(%q) = %v, want %v", c.path, got, c.want)
			}
		}
	}
}

func TestRestPageSizeAndNumber(t *testing.T) {
	ctx := &Ctx{Query: url.Values{}}
	if got := restPageSize(ctx); got != 30 {
		t.Fatalf("pageSize default = %d, want 30", got)
	}
	if got := restPageNumber(ctx); got != 1 {
		t.Fatalf("pageNumber default = %d, want 1", got)
	}
	ctx.Query.Set("per_page", "2")
	ctx.Query.Set("page", "3")
	if got := restPageSize(ctx); got != 2 {
		t.Fatalf("pageSize(2) = %d", got)
	}
	if got := restPageNumber(ctx); got != 3 {
		t.Fatalf("pageNumber(3) = %d", got)
	}
	ctx.Query.Set("per_page", "200")
	if got := restPageSize(ctx); got != 100 {
		t.Fatalf("pageSize(200) = %d, want 100 (cap)", got)
	}
	ctx.Query.Set("per_page", "0")
	if got := restPageSize(ctx); got != 30 {
		t.Fatalf("pageSize(0) = %d, want 30", got)
	}
	ctx.Query.Set("per_page", "abc")
	if got := restPageSize(ctx); got != 30 {
		t.Fatalf("pageSize(abc) = %d, want 30", got)
	}
	ctx.Query.Set("per_page", "2.9")
	if got := restPageSize(ctx); got != 2 {
		t.Fatalf("pageSize(2.9) = %d, want 2 (floor)", got)
	}
	ctx.Query.Set("page", "0")
	if got := restPageNumber(ctx); got != 1 {
		t.Fatalf("pageNumber(0) = %d, want 1", got)
	}
}

func TestRestParseNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		{"1e2", 100, true},
		{"01", 1, true},
		{"1.5", 0, false},
		{"0", 0, false},
		{"-3", 0, false},
		{"12abc", 0, false},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := restParseNumber(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("restParseNumber(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRestRootGet(t *testing.T) {
	restLiveRespond(t)
	rec := restCall(t, restTestCtx(t, NewStore(), "GET", "/api/v3/", "", false))
	if rec.Code != 200 {
		t.Fatalf("root GET status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-OAuth-Scopes"); got != "repo, read:org" {
		t.Fatalf("root GET X-OAuth-Scopes = %q, want %q", got, "repo, read:org")
	}
	body := restObject(t, rec)
	if body["current_user_url"] != "/api/v3/user" {
		t.Fatalf("root body = %v", body)
	}
	if body["rate_limit_url"] != "/api/v3/rate_limit" {
		t.Fatalf("root body = %v", body)
	}
}

func TestRestRootPostIs404(t *testing.T) {
	restLiveRespond(t)
	rec := restCall(t, restTestCtx(t, NewStore(), "POST", "/api/v3/", "", false))
	if rec.Code != 404 {
		t.Fatalf("root POST status = %d, want 404", rec.Code)
	}
}

func TestRestNonAPIReturnsFalse(t *testing.T) {
	ctx := restTestCtx(t, NewStore(), "GET", "/something/else", "", false)
	if HandleRest(ctx, httptest.NewRecorder()) {
		t.Fatalf("HandleRest handled a non-/api/v3 path")
	}
}

func TestRestUnauthorizedWithoutToken(t *testing.T) {
	restLiveRespond(t)
	rec := restCall(t, restTestCtx(t, NewStore(), "GET", "/api/v3/meta", "", false))
	if rec.Code != 401 {
		t.Fatalf("GET /meta without token status = %d, want 401", rec.Code)
	}
	body := restObject(t, rec)
	if _, ok := body["message"]; !ok {
		t.Fatalf("401 body has no message: %q", rec.Body.String())
	}
}

func TestRestCreateAndGetRepo(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	restLiveJSONBody(t)
	store := restLiveStore(t)

	rec := restCall(t, restTestCtx(t, store, "POST", "/api/v3/user/repos", `{"name":"demo","description":"d"}`, true))
	if rec.Code != 201 {
		t.Fatalf("POST /user/repos status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	created := restObject(t, rec)
	if created["name"] != "demo" || created["full_name"] == nil && created["name"] != "demo" {
		t.Fatalf("created repo body = %v", created)
	}

	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/demo", "", true))
	if rec.Code != 200 {
		t.Fatalf("GET repo status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := restObject(t, rec)
	if got["name"] != "demo" {
		t.Fatalf("GET repo body = %v", got)
	}

	// Duplicate create is 422.
	rec = restCall(t, restTestCtx(t, store, "POST", "/api/v3/user/repos", `{"name":"demo"}`, true))
	if rec.Code != 422 {
		t.Fatalf("duplicate POST repo status = %d, want 422", rec.Code)
	}

	// PATCH updates the description.
	rec = restCall(t, restTestCtx(t, store, "PATCH", "/api/v3/repos/adaiversen/demo", `{"description":"new desc"}`, true))
	if rec.Code != 200 {
		t.Fatalf("PATCH repo status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := restObject(t, rec)["description"]; got != "new desc" {
		t.Fatalf("PATCH repo description = %v, want %q", got, "new desc")
	}

	// DELETE then GET is 404.
	rec = restCall(t, restTestCtx(t, store, "DELETE", "/api/v3/repos/adaiversen/demo", "", true))
	if rec.Code != 204 {
		t.Fatalf("DELETE repo status = %d, want 204", rec.Code)
	}
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/demo", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET deleted repo status = %d, want 404", rec.Code)
	}
}

func TestRestCreateIssueFastPathAndUnknownLabel(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	restLiveJSONBody(t)
	store := restLiveStore(t)
	repo := store.CreateRepo(ViewerLogin, "iss", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}

	// A label must exist before an issue may carry it.
	rec := restCall(t, restTestCtx(t, store, "POST", "/api/v3/repos/adaiversen/iss/labels", `{"name":"bug"}`, true))
	if rec.Code != 201 {
		t.Fatalf("POST label status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := restObject(t, rec)["name"]; got != "bug" {
		t.Fatalf("label body = %v", restObject(t, rec))
	}

	// Unknown label on issue create is 422.
	rec = restCall(t, restTestCtx(t, store, "POST", "/api/v3/repos/adaiversen/iss/issues", `{"title":"x","labels":["nope"]}`, true))
	if rec.Code != 422 {
		t.Fatalf("POST issue with unknown label status = %d, want 422", rec.Code)
	}

	// Create an issue with the known label.
	rec = restCall(t, restTestCtx(t, store, "POST", "/api/v3/repos/adaiversen/iss/issues", `{"title":"First bug","body":"b","labels":["bug"]}`, true))
	if rec.Code != 201 {
		t.Fatalf("POST issue status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	issue := restObject(t, rec)
	if issue["title"] != "First bug" || issue["state"] != "open" {
		t.Fatalf("issue body = %v", issue)
	}
	num, ok := issue["number"].(float64)
	if !ok || num != 1 {
		t.Fatalf("issue number = %v, want 1", issue["number"])
	}

	// Fast path: exact-shape GET with no query string.
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/iss/issues/1", "", true))
	if rec.Code != 200 {
		t.Fatalf("fast-path GET issue status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "First bug") {
		t.Fatalf("fast-path GET issue body missing title: %q", rec.Body.String())
	}
	cached := store.IssueJSONText(repo, store.FindIssue(repo, 1))
	if strings.TrimSpace(rec.Body.String()) != strings.TrimSpace(cached) {
		t.Fatalf("fast-path body %q != cached issue text %q", rec.Body.String(), cached)
	}

	// Same GET with a query string takes the general path (still 200).
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/iss/issues/1?x=1", "", true))
	if rec.Code != 200 {
		t.Fatalf("GET issue with query status = %d, want 200", rec.Code)
	}

	// Unknown issue is 404 on both paths.
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/iss/issues/99", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET missing issue status = %d, want 404", rec.Code)
	}
}

func TestRestWrongOwnerIs404Not403(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	store := restLiveStore(t)
	store.CreateRepo(ViewerLogin, "mine", "", false)
	rec := restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/someone-else/mine", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET repo under another owner status = %d, want 404 (never 403)", rec.Code)
	}
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/nope", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET missing repo status = %d, want 404", rec.Code)
	}
}

func TestRestUserReposPaginationLink(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	store := restLiveStore(t)
	for _, name := range []string{"p1", "p2", "p3"} {
		if store.CreateRepo(ViewerLogin, name, "", false) == nil {
			t.Fatalf("CreateRepo(%q) returned nil", name)
		}
	}
	rec := restCall(t, restTestCtx(t, store, "GET", "/api/v3/user/repos?per_page=2&page=1", "", true))
	if rec.Code != 200 {
		t.Fatalf("GET /user/repos status = %d, want 200", rec.Code)
	}
	if got := len(restArray(t, rec)); got != 2 {
		t.Fatalf("page 1 has %d repos, want 2", got)
	}
	link := rec.Header().Get("Link")
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link header = %q, want a rel=\"next\"", link)
	}
	if !strings.Contains(link, "page=2") {
		t.Fatalf("page 1 Link header = %q, want a page=2 next link", link)
	}
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/user/repos?per_page=2&page=2", "", true))
	if got := len(restArray(t, rec)); got != 1 {
		t.Fatalf("page 2 has %d repos, want 1", got)
	}
	if link := rec.Header().Get("Link"); strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 2 Link header = %q, want no rel=\"next\"", link)
	}
}

func TestRestContentsPutGetDelete(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	restLiveJSONBody(t)
	store := restLiveStore(t)
	if store.CreateRepo(ViewerLogin, "cnt", "", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}
	content := base64.StdEncoding.EncodeToString([]byte("hello\n"))

	// Create (201).
	rec := restCall(t, restTestCtx(t, store, "PUT", "/api/v3/repos/adaiversen/cnt/contents/hello.txt",
		`{"message":"add hello","content":"`+content+`"}`, true))
	if rec.Code != 201 {
		t.Fatalf("PUT create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	created := restObject(t, rec)
	sha, _ := created["content"].(map[string]any)["sha"].(string)
	if sha == "" {
		t.Fatalf("PUT create body has no content.sha: %v", created)
	}

	// Read back (200, base64 content).
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/cnt/contents/hello.txt", "", true))
	if rec.Code != 200 {
		t.Fatalf("GET contents status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := restObject(t, rec)
	if got["content"] != content {
		t.Fatalf("GET contents content = %v, want %q", got["content"], content)
	}

	// Stale sha on update is 409.
	rec = restCall(t, restTestCtx(t, store, "PUT", "/api/v3/repos/adaiversen/cnt/contents/hello.txt",
		`{"message":"upd","content":"`+content+`","sha":"0000000000000000000000000000000000000000"}`, true))
	if rec.Code != 409 {
		t.Fatalf("PUT stale sha status = %d, want 409", rec.Code)
	}

	// Update with the right sha is 200.
	rec = restCall(t, restTestCtx(t, store, "PUT", "/api/v3/repos/adaiversen/cnt/contents/hello.txt",
		`{"message":"upd","content":"`+content+`","sha":"`+sha+`"}`, true))
	if rec.Code != 200 {
		t.Fatalf("PUT update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	updated := restObject(t, rec)
	sha2, _ := updated["content"].(map[string]any)["sha"].(string)
	if sha2 == "" {
		t.Fatalf("PUT update body has no content.sha: %v", updated)
	}

	// Delete (200) then GET is 404.
	rec = restCall(t, restTestCtx(t, store, "DELETE", "/api/v3/repos/adaiversen/cnt/contents/hello.txt",
		`{"message":"del","sha":"`+sha2+`"}`, true))
	if rec.Code != 200 {
		t.Fatalf("DELETE contents status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if commit, ok := restObject(t, rec)["commit"].(map[string]any); !ok || commit["sha"] == nil {
		t.Fatalf("DELETE body has no commit.sha: %q", rec.Body.String())
	}
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/cnt/contents/hello.txt", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET deleted file status = %d, want 404", rec.Code)
	}
}

func TestRestMergePull(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	restLiveJSONBody(t)
	store := restLiveStore(t)
	repo := store.CreateRepo(ViewerLogin, "prj", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}

	// Base commit on main, then a feature branch off it.
	blob := store.PutObject(repo, ObjBlob, []byte("hello\n"))
	tree := store.BuildTreeFromFlat(repo, map[string]FlatFile{"a.txt": {OID: blob, Mode: "100644"}})
	base := store.CreateCommit(repo, tree, []string{}, "base", nil)
	repo.Refs["refs/heads/main"] = base
	blob2 := store.PutObject(repo, ObjBlob, []byte("hello world\n"))
	tree2 := store.BuildTreeFromFlat(repo, map[string]FlatFile{"a.txt": {OID: blob2, Mode: "100644"}})
	feat := store.CreateCommit(repo, tree2, []string{base}, "feat", nil)
	repo.Refs["refs/heads/feature"] = feat

	// Create the pull (201, open, head/base).
	rec := restCall(t, restTestCtx(t, store, "POST", "/api/v3/repos/adaiversen/prj/pulls",
		`{"title":"Change a","head":"feature","base":"main"}`, true))
	if rec.Code != 201 {
		t.Fatalf("POST pull status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	pull := restObject(t, rec)
	if pull["state"] != "open" || pull["head"] == nil || pull["base"] == nil {
		t.Fatalf("pull body = %v", pull)
	}

	// Unknown pull is 404.
	rec = restCall(t, restTestCtx(t, store, "GET", "/api/v3/repos/adaiversen/prj/pulls/99", "", true))
	if rec.Code != 404 {
		t.Fatalf("GET missing pull status = %d, want 404", rec.Code)
	}

	// Merge (200, merged true, squash commit on main).
	rec = restCall(t, restTestCtx(t, store, "PUT", "/api/v3/repos/adaiversen/prj/pulls/1/merge", `{}`, true))
	if rec.Code != 200 {
		t.Fatalf("PUT merge status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	merged := restObject(t, rec)
	if merged["merged"] != true || merged["sha"] == nil || merged["sha"] == "" {
		t.Fatalf("merge body = %v", merged)
	}
	if got := repo.Refs["refs/heads/main"]; got != merged["sha"] {
		t.Fatalf("main ref = %q, want merge sha %v", got, merged["sha"])
	}

	// Merging again is 405.
	rec = restCall(t, restTestCtx(t, store, "PUT", "/api/v3/repos/adaiversen/prj/pulls/1/merge", `{}`, true))
	if rec.Code != 405 {
		t.Fatalf("second PUT merge status = %d, want 405", rec.Code)
	}
}

func TestRestMalformedJSONIs400(t *testing.T) {
	restLiveRespond(t)
	restLiveAuth(t)
	restLiveJSONBody(t)
	store := restLiveStore(t)
	if store.CreateRepo(ViewerLogin, "bad", "", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}
	rec := restCall(t, restTestCtx(t, store, "POST", "/api/v3/repos/adaiversen/bad/issues", `{"title":`, true))
	if rec.Code != 400 {
		t.Fatalf("POST malformed JSON status = %d, want 400", rec.Code)
	}
}
