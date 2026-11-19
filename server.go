package main

// The server: one process, one in-memory store, four surfaces. It reads the
// whole request body, builds the context, and routes: git smart HTTP at
// /{owner}/{repo}.git, MCP at /mcp, GraphQL at /api/graphql, REST v3 under
// /api/v3, and /healthz for the harness's liveness probe. Every reply is
// JSON (or a git binary body) with a non-empty X-GitHub-Request-Id; an
// unknown route is a JSON 404, never HTML.

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// MaxBodyBytes is the largest request body accepted (the suite's packs
// are far smaller).
const MaxBodyBytes = 64 * 1024 * 1024

// gitRoute matches /{owner}/{repo}.git/{service}; it runs only on paths
// containing ".git/", never on a REST/GraphQL/MCP path.
var gitRoute = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// trackingWriter remembers whether headers were already sent, so a
// recovered panic (or a body-read failure) only writes a 500 when the
// response has not started yet.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// hasBody reports whether the request carries a body: a chunked transfer,
// or a positive Content-Length. A GET with no Content-Length, and a
// Content-Length of 0, has none.
func hasBody(r *http.Request) bool {
	if len(r.TransferEncoding) > 0 {
		return true
	}
	return r.ContentLength > 0
}

// decodePath decodes a request path, falling back to the raw path when it
// is malformed. A path with no `%` is already decoded, so it skips the
// unescape work.
func decodePath(raw string) string {
	if !strings.Contains(raw, "%") {
		return raw
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}

// readBody reads the whole request body, bounded. It resolves as soon as
// the declared Content-Length bytes have arrived (net/http delivers them
// without waiting for the stream end), and errors past the bound.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("request body too large")
	}
	return body, nil
}

// routeKind classifies a request path before any lock is taken, so the
// server can choose the right lock mode for the handler it will call.
type routeKind int

const (
	kindHealthz routeKind = iota
	kindRest
	kindGraphql
	kindMcp
	kindGitInfoRefs
	kindGitUploadPack
	kindGitReceivePack
	kindUnknown
)

// classify returns the route kind and, for git routes, the owner, repo
// and service. A git path with a wrong method still classifies as its
// git kind (the router answers 404 for it, never falls through).
func classify(path, method string) (routeKind, string, string, string) {
	if path == "/healthz" && method == http.MethodGet {
		return kindHealthz, "", "", ""
	}
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		return kindRest, "", "", ""
	}
	if path == "/api/graphql" {
		return kindGraphql, "", "", ""
	}
	if path == "/mcp" {
		return kindMcp, "", "", ""
	}
	if strings.Contains(path, ".git/") {
		if m := gitRoute.FindStringSubmatch(path); m != nil {
			switch m[3] {
			case "info/refs":
				return kindGitInfoRefs, m[1], m[2], m[3]
			case "git-upload-pack":
				return kindGitUploadPack, m[1], m[2], m[3]
			case "git-receive-pack":
				return kindGitReceivePack, m[1], m[2], m[3]
			}
		}
	}
	return kindUnknown, "", "", ""
}

// CreateApp builds the HTTP handler the lifecycle in main.go binds and
// registers. The store is created once here and shared by every request.
func CreateApp() http.Handler {
	store := NewStore()
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			// A handler that panicked: answer a JSON 500 rather than
			// hanging the connection, unless the reply already started.
			if rec := recover(); rec != nil {
				if !tw.wrote {
					msg := "internal error"
					if err, ok := rec.(error); ok {
						msg = err.Error()
					} else if s, ok := rec.(string); ok {
						msg = s
					}
					SendJSON(tw, http.StatusInternalServerError, map[string]any{"message": msg}, nil)
				}
			}
		}()

		// A request with no body (every GET, and a bodyless DELETE) need
		// not wait for a body read: the body is empty either way.
		var body []byte
		if hasBody(r) {
			var err error
			body, err = readBody(r)
			if err != nil {
				SendJSON(tw, http.StatusInternalServerError, map[string]any{"message": err.Error()}, nil)
				return
			}
		} else {
			body = []byte{}
		}

		// Split the path and query by hand: r.URL is already parsed by
		// net/http, so only the path and the query string are ever read.
		// EscapedPath keeps the raw %XX form so decodePath can skip its
		// work entirely when the path has no escapes.
		path := decodePath(r.URL.EscapedPath())
		// Go's net/http keeps the request host in r.Host, NOT in
		// r.Header — but handlers read it via ctx.Headers.Get("Host")
		// (the TS reference sees host as a header, and the pagination
		// code builds absolute Link URLs from it). Copy it in, or
		// every paginated `next` link loses the port.
		headers := r.Header.Clone()
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Host", r.Host)
		ctx := &Ctx{
			Store:   store,
			Token:   token,
			Method:  r.Method,
			Path:    path,
			Query:   r.URL.Query(),
			Headers: headers,
			Body:    body,
		}

		kind, owner, repoName, _ := classify(path, r.Method)
		switch kind {
		case kindHealthz:
			// The harness's liveness probe (no auth, no lock).
			SendJSON(tw, http.StatusOK, map[string]any{"ok": true}, nil)
			return
		case kindUnknown:
			SendNotFound(tw, "Not Found")
			return
		}

		// Lock once per request and hold it for the whole handler call:
		// a read lock for read-only routes, a write lock for mutating
		// ones. The handlers themselves must NOT lock again.
		readOnly := r.Method == http.MethodGet || kind == kindGitInfoRefs || kind == kindGitUploadPack
		if readOnly {
			store.RLock()
			defer store.RUnlock()
		} else {
			store.Lock()
			defer store.Unlock()
		}

		switch kind {
		case kindRest:
			if !HandleRest(ctx, tw) {
				SendNotFound(tw, "Not Found")
			}
		case kindGraphql:
			if !HandleGraphql(ctx, tw) {
				SendNotFound(tw, "Not Found")
			}
		case kindMcp:
			if !HandleMcp(ctx, tw) {
				SendNotFound(tw, "Not Found")
			}
		case kindGitInfoRefs:
			if r.Method != http.MethodGet {
				SendNotFound(tw, "Not Found")
				return
			}
			HandleInfoRefs(ctx, tw, owner, repoName)
		case kindGitUploadPack:
			if r.Method != http.MethodPost {
				SendNotFound(tw, "Not Found")
				return
			}
			HandleUploadPack(ctx, tw, owner, repoName)
		case kindGitReceivePack:
			if r.Method != http.MethodPost {
				SendNotFound(tw, "Not Found")
				return
			}
			HandleReceivePack(ctx, tw, owner, repoName)
		}
	})
}
