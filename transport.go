package main

// Raw HTTP/1.1 transport over net.Conn (anchor d5c68678).
//
// This replaces net/http's server side only. The handlers still see the
// same *http.Request and http.ResponseWriter, so no handler file changes:
// the transport builds a request by hand, calls CreateApp()'s handler,
// and writes the reply back over the same connection with one buffered
// flush per response.
//
// Why: the read workload is 2,000 sequential GETs on one keep-alive
// connection. net/http pays per request for its generic parser, its
// response sniffing, its date handling and its chunked/body machinery.
// Here the hot path is a small hand parser plus a single buffered
// write, and the body is read in full before dispatch (as before).

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rawServer is the accept loop: one goroutine per connection, each
// looping requests until the client closes or asks to close.
type rawServer struct {
	handler http.Handler
	ln      net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  chan struct{}
}

func newRawServer(handler http.Handler, ln net.Listener) *rawServer {
	return &rawServer{
		handler: handler,
		ln:      ln,
		conns:   make(map[net.Conn]struct{}),
		done:    make(chan struct{}),
	}
}

func (s *rawServer) track(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *rawServer) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Serve accepts connections until the listener closes.
func (s *rawServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return err
			}
		}
		s.track(conn)
		go s.serveConn(conn)
	}
}

// Close stops accepting and closes every open connection.
func (s *rawServer) Close() error {
	close(s.done)
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	return err
}

// connState holds the reusable per-connection buffers: one buffered
// reader, one buffered writer, one body buffer and one line buffer.
// Reusing them across the 2,000 requests on one connection is the
// whole point — no per-request buffer allocation on the hot path.
type connState struct {
	br      *bufio.Reader
	bw      *bufio.Writer
	bodyBuf []byte
	lineBuf []byte
}

// readLine reads one CRLF-terminated line into the reusable line buffer
// and returns it (without the trailing CR/LF). The returned slice is
// only valid until the next readLine call.
func (cs *connState) readLine() ([]byte, error) {
	cs.lineBuf = cs.lineBuf[:0]
	for {
		frag, err := cs.br.ReadSlice('\n')
		cs.lineBuf = append(cs.lineBuf, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		line := cs.lineBuf
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
		}
		return line, nil
	}
}

// errBodyTooLarge is returned by a body reader when the declared body
// exceeds MaxBodyBytes; readBody in server.go turns it into a JSON 500.
var errBodyTooLarge = errors.New("request body too large")

type errorBodyReader struct{ err error }

func (e errorBodyReader) Read([]byte) (int, error) { return 0, e.err }
func (e errorBodyReader) Close() error             { return nil }

// readRequest parses one request from the connection. It returns the
// request, whether the connection must close after the reply, and an
// error (io.EOF when the client went away before sending a request
// line — that is a clean close, not a failure).
func (cs *connState) readRequest(conn net.Conn) (*http.Request, bool, error) {
	line, err := cs.readLine()
	if err != nil {
		return nil, false, err
	}
	if len(line) == 0 {
		// A stray blank line before the request line (some clients
		// send one); skip it and read the real line.
		line, err = cs.readLine()
		if err != nil {
			return nil, false, err
		}
	}
	// Request line: METHOD SP TARGET SP PROTO
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, true, errors.New("malformed request line")
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return nil, true, errors.New("malformed request line")
	}
	sp2 += sp1 + 1
	method := string(line[:sp1])
	target := string(line[sp1+1 : sp2])
	proto := string(line[sp2+1:])
	if method == "" || target == "" {
		return nil, true, errors.New("malformed request line")
	}
	keepAlive := true
	if proto == "HTTP/1.0" {
		keepAlive = false
	} else if proto != "HTTP/1.1" {
		// Unknown version: answer as HTTP/1.1 and close afterwards.
		keepAlive = false
	}

	// Headers, one line each until the blank line.
	header := make(http.Header, 16)
	var host string
	var contentLength int64 = -1
	chunked := false
	expectContinue := false
	for {
		hline, err := cs.readLine()
		if err != nil {
			return nil, false, err
		}
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon < 0 {
			return nil, true, errors.New("malformed header line")
		}
		name := string(hline[:colon])
		value := strings.TrimLeft(string(hline[colon+1:]), " \t")
		key := textproto.CanonicalMIMEHeaderKey(name)
		header[key] = append(header[key], value)
		switch key {
		case "Host":
			host = value
		case "Content-Length":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && n >= 0 {
				contentLength = n
			}
		case "Transfer-Encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "Connection":
			lower := strings.ToLower(value)
			if strings.Contains(lower, "close") {
				keepAlive = false
			} else if strings.Contains(lower, "keep-alive") {
				keepAlive = true
			}
		case "Expect":
			if strings.EqualFold(strings.TrimSpace(value), "100-continue") {
				expectContinue = true
			}
		}
	}

	// Split the target into raw path + raw query. Absolute-form
	// targets (a proxy-style http://host/path) are reduced to origin
	// form; the suite only ever sends origin form.
	rawPath, rawQuery := target, ""
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if u, err := url.Parse(target); err == nil {
			rawPath, rawQuery = u.EscapedPath(), u.RawQuery
			if host == "" {
				host = u.Host
			}
			if rawPath == "" {
				rawPath = "/"
			}
		}
	} else {
		if i := strings.IndexByte(target, '?'); i >= 0 {
			rawPath, rawQuery = target[:i], target[i+1:]
		}
		if i := strings.IndexByte(target, '#'); i >= 0 {
			rawPath = rawPath[:i]
		}
	}
	if rawPath == "" {
		rawPath = "/"
	}
	decoded := decodePath(rawPath)

	if expectContinue {
		_, _ = cs.bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		_ = cs.bw.Flush()
	}

	// Read the body in full before dispatch, as before. Chunked and
	// Content-Length both land in the reusable body buffer.
	closeAfter := !keepAlive
	var body io.ReadCloser
	cs.bodyBuf = cs.bodyBuf[:0]
	if chunked {
		for {
			sizeLine, err := cs.readLine()
			if err != nil {
				return nil, false, err
			}
			if i := bytes.IndexByte(sizeLine, ';'); i >= 0 {
				sizeLine = sizeLine[:i]
			}
			size, err := strconv.ParseInt(strings.TrimSpace(string(sizeLine)), 16, 64)
			if err != nil || size < 0 {
				return nil, true, errors.New("malformed chunk size")
			}
			if size == 0 {
				// Trailers until the blank line.
				for {
					tline, err := cs.readLine()
					if err != nil {
						return nil, false, err
					}
					if len(tline) == 0 {
						break
					}
				}
				break
			}
			if int64(len(cs.bodyBuf))+size > MaxBodyBytes {
				body = errorBodyReader{errBodyTooLarge}
				closeAfter = true
				break
			}
			start := len(cs.bodyBuf)
			cs.bodyBuf = append(cs.bodyBuf, make([]byte, size)...)
			if _, err := io.ReadFull(cs.br, cs.bodyBuf[start:]); err != nil {
				return nil, false, err
			}
			// The CRLF after the chunk.
			if _, err := cs.br.ReadSlice('\n'); err != nil {
				return nil, false, err
			}
		}
		if body == nil {
			body = io.NopCloser(bytes.NewReader(cs.bodyBuf))
		}
	} else if contentLength > 0 {
		if contentLength > MaxBodyBytes {
			body = errorBodyReader{errBodyTooLarge}
			closeAfter = true
		} else {
			cs.bodyBuf = append(cs.bodyBuf, make([]byte, contentLength)...)
			if _, err := io.ReadFull(cs.br, cs.bodyBuf); err != nil {
				return nil, false, err
			}
			body = io.NopCloser(bytes.NewReader(cs.bodyBuf))
		}
	} else {
		body = io.NopCloser(bytes.NewReader(nil))
	}

	u := &url.URL{Path: decoded, RawQuery: rawQuery}
	if rawPath != decoded {
		u.RawPath = rawPath
	}
	req := &http.Request{
		Method:        method,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		URL:           u,
		Header:        header,
		Host:          host,
		Body:          body,
		ContentLength: contentLength,
		RemoteAddr:    conn.RemoteAddr().String(),
		RequestURI:    target,
	}
	if chunked {
		req.TransferEncoding = []string{"chunked"}
		req.ContentLength = -1
	}
	return req, closeAfter, nil
}

// dateCache holds the current HTTP Date header value, refreshed at most
// once a second — net/http does the same, and formatting a date per
// request would cost more than the rest of the reply combined.
var (
	dateMu   sync.Mutex
	dateSec  int64
	dateText string
)

func httpDate() string {
	now := time.Now().Unix()
	dateMu.Lock()
	defer dateMu.Unlock()
	if now != dateSec {
		dateSec = now
		dateText = time.Unix(now, 0).UTC().Format(http.TimeFormat)
	}
	return dateText
}

// rawResponseWriter is the minimal http.ResponseWriter over a buffered
// connection. Headers are held until the first body write (or the end
// of the request), so the status line, headers and body go out in one
// buffered flush. When the handler sets a Content-Length (every Send*
// helper does), the body streams straight into the buffer; when it does
// not, the body is held until finish so a correct Content-Length can
// be computed — a keep-alive client must know where the body ends.
type rawResponseWriter struct {
	bw         *bufio.Writer
	header     http.Header
	status     int
	headerSent bool
	heldBody   []byte
	closeAfter bool
}

func newRawResponseWriter(bw *bufio.Writer, closeAfter bool) *rawResponseWriter {
	return &rawResponseWriter{bw: bw, header: make(http.Header, 8), closeAfter: closeAfter}
}

func (w *rawResponseWriter) Header() http.Header { return w.header }

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.headerSent || w.status != 0 {
		return
	}
	w.status = status
}

// emitHeaders writes the status line and headers into the buffer.
func (w *rawResponseWriter) emitHeaders() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	reason := http.StatusText(status)
	if reason == "" {
		reason = "Status"
	}
	w.bw.WriteString("HTTP/1.1 ")
	w.bw.WriteString(strconv.Itoa(status))
	w.bw.WriteString(" ")
	w.bw.WriteString(reason)
	w.bw.WriteString("\r\n")
	if _, ok := w.header["Date"]; !ok {
		w.bw.WriteString("Date: ")
		w.bw.WriteString(httpDate())
		w.bw.WriteString("\r\n")
	}
	if w.closeAfter {
		w.bw.WriteString("Connection: close\r\n")
	}
	keys := make([]string, 0, len(w.header))
	for k := range w.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range w.header[k] {
			w.bw.WriteString(k)
			w.bw.WriteString(": ")
			w.bw.WriteString(v)
			w.bw.WriteString("\r\n")
		}
	}
	w.bw.WriteString("\r\n")
	w.headerSent = true
}

func (w *rawResponseWriter) Write(b []byte) (int, error) {
	if !w.headerSent {
		if _, ok := w.header["Content-Length"]; ok {
			w.emitHeaders()
			return w.bw.Write(b)
		}
		// No declared length: hold the body until finish() so the
		// length can be set exactly (no chunked encoding needed).
		w.heldBody = append(w.heldBody, b...)
		return len(b), nil
	}
	return w.bw.Write(b)
}

// finish sends any held headers/body and flushes the connection.
// It runs after the handler returns, even when the handler only
// called WriteHeader (the MCP 202 notification) and wrote no body.
func (w *rawResponseWriter) finish() error {
	if !w.headerSent {
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		// A reply with no body and no declared length still needs a
		// length on a keep-alive connection, or the client cannot tell
		// where the reply ends: net/http sends Content-Length: 0
		// here. 204/304/1xx carry no body by definition.
		if _, ok := w.header["Content-Length"]; !ok && status != http.StatusNoContent && status != http.StatusNotModified && status >= 200 {
			w.header.Set("Content-Length", strconv.Itoa(len(w.heldBody)))
		}
		w.emitHeaders()
		if len(w.heldBody) > 0 {
			_, _ = w.bw.Write(w.heldBody)
		}
	}
	return w.bw.Flush()
}

// serveConn loops requests on one connection until the client closes,
// sends Connection: close, or the request is malformed.
func (s *rawServer) serveConn(conn net.Conn) {
	defer func() {
		s.untrack(conn)
		_ = conn.Close()
	}()
	cs := &connState{
		br: bufio.NewReaderSize(conn, 32768),
		bw: bufio.NewWriterSize(conn, 32768),
	}
	for {
		req, closeAfter, err := cs.readRequest(conn)
		if err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			// A malformed request: try a 400, then drop the conn.
			_, _ = cs.bw.WriteString("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			_ = cs.bw.Flush()
			return
		}
		rw := newRawResponseWriter(cs.bw, closeAfter)
		s.handler.ServeHTTP(rw, req)
		_ = rw.finish()
		if closeAfter {
			return
		}
	}
}
