package main

// Unit tests for Builder B's server core: helpers, auth, body parsing
// and routing. These run against the stub store, so they only assert
// behaviour that does not depend on the other builders' files.

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsoOfMatchesJSISOString(t *testing.T) {
	if got := IsoOf(1735689600); got != "2025-01-01T00:00:00.000Z" {
		t.Fatalf("IsoOf(1735689600) = %q", got)
	}
	if got := IsoOf(0); got != "1970-01-01T00:00:00.000Z" {
		t.Fatalf("IsoOf(0) = %q", got)
	}
}

func TestSha1HexAndIsOID(t *testing.T) {
	if got := Sha1Hex([]byte("abc")); got != "a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Fatalf("Sha1Hex = %q", got)
	}
	if !IsOID(Sha1Hex([]byte("abc"))) || IsOID("xyz") || IsOID(strings.Repeat("a", 39)) {
		t.Fatal("IsOID mismatch")
	}
}

func TestB64RoundTripStripsWhitespace(t *testing.T) {
	enc := B64Encode("hello world")
	dec, err := B64Decode(enc[:4] + "\r\n " + enc[4:] + "\n")
	if err != nil || string(dec) != "hello world" {
		t.Fatalf("B64Decode = %q, %v", dec, err)
	}
	if B64URL(">>>") != "Pj4-" {
		t.Fatalf("B64URL = %q", B64URL(">>>"))
	}
}

// B64Decode must still return the bytes decoded before malformed input,
// alongside the error: the TS reference never throws on bad base64 chars,
// and callers use the bytes while ignoring the error.
func TestB64DecodeReturnsPartialBytesOnError(t *testing.T) {
	// "aGVsbG8=" is "hello"; the trailing "!" is illegal base64.
	dec, err := B64Decode("aGVsbG8=!")
	if err == nil {
		t.Fatal("expected an error for trailing junk")
	}
	if string(dec) != "hello" {
		t.Fatalf("partial bytes = %q, want %q", dec, "hello")
	}
	if _, err := B64Decode("!!!"); err == nil {
		t.Fatal("expected an error for all-junk input")
	}
}

// The Host header must reach handlers through ctx.Headers: Go keeps the
// host in r.Host, not r.Header, and the REST pagination code builds its
// absolute `next` Link from ctx.Headers.Get("Host"). Without the copy
// the link falls back to bare 127.0.0.1 and loses the port.
func TestHostHeaderReachesPagination(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "tok123")
	app := CreateApp()
	call := func(method, target, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, rdr)
		req.Header.Set("Authorization", "Bearer tok123")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec
	}
	for _, name := range []string{"host-a", "host-b"} {
		rec := call(http.MethodPost, "/api/v3/user/repos", `{"name":"`+name+`"}`)
		if rec.Code != 201 {
			t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := call(http.MethodGet, "/api/v3/user/repos?per_page=1", "")
	link := rec.Header().Get("Link")
	if !strings.Contains(link, "example.com") || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want the request host kept with rel=\"next\"", link)
	}
}

func TestJSONTextNeverThrows(t *testing.T) {
	if JSONText(nil) != "null" {
		t.Fatalf("JSONText(nil) = %q", JSONText(nil))
	}
	if JSONText(map[string]any{"a": "<b>"}) != `{"a":"<b>"}` {
		t.Fatalf("JSONText escapes HTML: %q", JSONText(map[string]any{"a": "<b>"}))
	}
}

func TestNextRequestIDIncrements(t *testing.T) {
	a, b := NextRequestID(), NextRequestID()
	if !strings.HasPrefix(a, "req-") || a == b {
		t.Fatalf("ids = %q, %q", a, b)
	}
}

func TestAuthorizedBearerAndBasic(t *testing.T) {
	ctx := &Ctx{Token: "sekret", Headers: http.Header{}}
	if Authorized(ctx) {
		t.Fatal("empty auth must fail")
	}
	ctx.Headers.Set("Authorization", "Bearer sekret")
	if !Authorized(ctx) {
		t.Fatal("Bearer token must pass")
	}
	ctx.Headers.Set("Authorization", "Bearer wrong")
	if Authorized(ctx) {
		t.Fatal("wrong Bearer must fail")
	}
	ctx.Headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:sekret")))
	if !Authorized(ctx) {
		t.Fatal("Basic with token as password must pass")
	}
	ctx.Headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekret:pass")))
	if !Authorized(ctx) {
		t.Fatal("Basic with token as user must pass")
	}
	ctx.Headers.Set("Authorization", "Basic !!!not-base64!!!")
	if Authorized(ctx) {
		t.Fatal("malformed Basic must fail")
	}
}

func TestJSONBodyStates(t *testing.T) {
	if v, ok := JSONBody(&Ctx{}); v != nil || !ok {
		t.Fatalf("empty body = %v, %v", v, ok)
	}
	if v, ok := JSONBody(&Ctx{Body: []byte(`{"a":1}`)}); !ok || v.(map[string]any)["a"] != float64(1) {
		t.Fatalf("parsed body = %v, %v", v, ok)
	}
	if _, ok := JSONBody(&Ctx{Body: []byte(`{broken`)}); ok {
		t.Fatal("malformed body must report false")
	}
}

func TestHealthzAndUnknownRoute(t *testing.T) {
	app := CreateApp()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Github-Request-Id") == "" {
		t.Fatal("healthz must carry X-GitHub-Request-Id")
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Not Found") {
		t.Fatalf("unknown route = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("404 must be JSON, got Content-Type %q", ct)
	}
}

func TestGitRouteWrongMethodIsJSON404(t *testing.T) {
	app := CreateApp()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/adaiversen/x.git/git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok")
	app.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("GET upload-pack = %d, want 404", rec.Code)
	}
}

func TestGitInfoRefsRequiresAuth(t *testing.T) {
	app := CreateApp()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/adaiversen/x.git/info/refs?service=git-upload-pack", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated info/refs = %d, want 401", rec.Code)
	}
}

func TestBodyTooLargeIsJSON500(t *testing.T) {
	app := CreateApp()
	huge := strings.NewReader(strings.Repeat("x", MaxBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v3/repos", huge)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("oversize body = %d %q", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Result().Body)
	_ = body
}
