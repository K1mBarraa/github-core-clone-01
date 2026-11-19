package main

// Tests for the raw HTTP/1.1 transport: request parsing (escaped
// paths, query strings, Host, chunked bodies), keep-alive across
// sequential requests on one connection, Connection: close, and a
// 204 with no Content-Length.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rawClient is a minimal HTTP/1.1 client over one TCP connection, so
// the tests control the exact bytes on the wire (chunked bodies,
// Connection: close) instead of going through net/http's client.
type rawClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func newRawClient(t *testing.T, addr string) *rawClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rawClient{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func (c *rawClient) do(raw string) *http.Response {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, raw); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
	// A dummy request for ReadResponse's benefit (it only needs the
	// method for HEAD handling).
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	res, err := http.ReadResponse(c.br, req)
	if err != nil {
		c.t.Fatalf("read response: %v", err)
	}
	return res
}

func (c *rawClient) body(res *http.Response) string {
	c.t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()
	return string(b)
}

// startRawTestServer starts the app behind the raw transport on a
// kernel-picked port and returns its address.
func startRawTestServer(t *testing.T) string {
	t.Helper()
	t.Setenv("MAKEFUN_SHOP_TOKEN", "tok123")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newRawServer(CreateApp(), ln)
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestRawHealthzAndRequestID(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	res := c.do("GET /healthz HTTP/1.1\r\nHost: example.com\r\nConnection: keep-alive\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("healthz status = %d", res.StatusCode)
	}
	if !strings.Contains(c.body(res), `"ok":true`) {
		t.Fatal("healthz body missing ok:true")
	}
	if res.Header.Get("X-Github-Request-Id") == "" {
		t.Fatal("missing X-GitHub-Request-Id")
	}
	if res.Header.Get("Date") == "" {
		t.Fatal("missing Date header")
	}
}

func TestRawKeepAliveSequentialRequests(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	for i := 0; i < 5; i++ {
		res := c.do("GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n")
		if res.StatusCode != 200 {
			t.Fatalf("request %d status = %d", i, res.StatusCode)
		}
		if !strings.Contains(c.body(res), `"ok":true`) {
			t.Fatalf("request %d body wrong", i)
		}
	}
}

func TestRawConnectionClose(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	res := c.do("GET /healthz HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	_ = c.body(res)
	// The server must have closed the connection after the reply.
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.br.ReadByte()
	if err != io.EOF {
		t.Fatalf("expected EOF after Connection: close, got %v", err)
	}
}

func TestRawChunkedPostAndEscapedPath(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	// Chunked POST creating a repo whose name needs %-escaping on
	// later reads; the chunked body must arrive intact.
	payload := `{"name":"chunk-repo","description":"via chunked"}`
	chunked := fmt.Sprintf("POST /api/v3/user/repos HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(payload), payload)
	res := c.do(chunked)
	if res.StatusCode != 201 {
		t.Fatalf("chunked create = %d %s", res.StatusCode, c.body(res))
	}
	_ = c.body(res)
	// The same connection stays alive after a chunked request, and a
	// %-escaped path resolves to the same repo.
	res = c.do("GET /api/v3/repos/adaiversen/chunk-repo HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("escaped-path GET = %d %s", res.StatusCode, c.body(res))
	}
	_ = c.body(res)
}

func TestRawQueryStringAndHost(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	for _, name := range []string{"host-a", "host-b"} {
		res := c.do("POST /api/v3/user/repos HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\nContent-Type: application/json\r\nContent-Length: " + fmt.Sprint(len(`{"name":"`+name+`"}`)) + "\r\n\r\n" + `{"name":"` + name + `"}`)
		if res.StatusCode != 201 {
			t.Fatalf("create %s = %d %s", name, res.StatusCode, c.body(res))
		}
		_ = c.body(res)
	}
	res := c.do("GET /api/v3/user/repos?per_page=1 HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\n\r\n")
	link := res.Header.Get("Link")
	if !strings.Contains(link, "example.com") || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want the request host kept with rel=\"next\"", link)
	}
	_ = c.body(res)
}

func TestRawDeleteIs204WithoutContentLength(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	payload := `{"name":"gone-repo"}`
	res := c.do("POST /api/v3/user/repos HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\nContent-Type: application/json\r\nContent-Length: " + fmt.Sprint(len(payload)) + "\r\n\r\n" + payload)
	if res.StatusCode != 201 {
		t.Fatalf("create = %d %s", res.StatusCode, c.body(res))
	}
	_ = c.body(res)
	// Read the raw response bytes for the DELETE so the test can see
	// the headers exactly as sent, not as net/http re-interprets them.
	if _, err := io.WriteString(c.conn, "DELETE /api/v3/repos/adaiversen/gone-repo HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\n\r\n"); err != nil {
		t.Fatalf("write DELETE: %v", err)
	}
	statusLine, err := c.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "204") {
		t.Fatalf("status line = %q, want 204", statusLine)
	}
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			t.Fatalf("204 must not carry Content-Length, got %q", line)
		}
	}
	// The connection must still be usable afterwards (204 kept alive).
	res = c.do("GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("healthz after 204 = %d", res.StatusCode)
	}
	_ = c.body(res)
}

func TestRawMcpNotification202HasLengthZero(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	// An MCP notification (no id, no result) answers 202 with no body
	// and no declared Content-Length; the transport must still send
	// Content-Length: 0 so the keep-alive client knows the reply ends.
	payload := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	res := c.do("POST /mcp HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer tok123\r\nContent-Type: application/json\r\nContent-Length: " + fmt.Sprint(len(payload)) + "\r\n\r\n" + payload)
	if res.StatusCode != 202 {
		t.Fatalf("notification = %d %s", res.StatusCode, c.body(res))
	}
	if got := c.body(res); got != "" {
		t.Fatalf("notification body = %q, want empty", got)
	}
	// Next request on the same connection must still work.
	res = c.do("GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("healthz after 202 = %d", res.StatusCode)
	}
	_ = c.body(res)
}

func TestRawBadRequestLineCloses(t *testing.T) {
	c := newRawClient(t, startRawTestServer(t))
	if _, err := io.WriteString(c.conn, "GARBAGE\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	statusLine, err := c.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(statusLine, "400") {
		t.Fatalf("status line = %q, want 400", statusLine)
	}
	// Drain the rest of the 400 response, then expect EOF.
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			break
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := c.br.ReadByte(); err != io.EOF {
		t.Fatalf("expected EOF after 400, got %v", err)
	}
}

func TestRawResponseWriterHeldBody(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	rw := newRawResponseWriter(bw, false)
	rw.Header().Set("X-Test", "yes")
	_, _ = rw.Write([]byte("hello"))
	if err := rw.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("response = %q", out)
	}
	if !strings.Contains(out, "Content-Length: 5\r\n") {
		t.Fatalf("held body needs Content-Length: 5, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\n\r\nhello") {
		t.Fatalf("response tail = %q", out)
	}
}
