// The HTTP/1.1 server, hand-rolled over raw TCP: one goroutine per
// connection, sequential requests on keep-alive connections, Content-Length
// and chunked request bodies, 100-continue. No net/http: the surface is small
// and the benchmark's read workload is 2,000 sequential requests on one warm
// connection, so the parse/write path stays allocation-light.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// request is one parsed HTTP request.
type request struct {
	method  string
	path    string // decoded path, no query, no trailing slash (except root)
	query   string
	headers requestHeaders
	body    []byte
	host    string

	segs     []string // memoized segments(); valid once segsDone is set
	segsDone bool
}

type headerSlot uint8

const (
	hostHeader headerSlot = iota
	authHeader
	protocolHeader
	expectHeader
	transferHeader
	lengthHeader
	connectionHeader
	headerSlots
)

type requestHeaders struct {
	seen   uint16
	values [headerSlots]string
}

func (h *requestHeaders) read(line []byte) error {
	name, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		return fmt.Errorf("bad header %q", line)
	}
	slot := matchHeader(bytes.TrimSpace(name))
	if slot == headerSlots {
		return nil
	}
	mask := uint16(1) << slot
	if h.seen&mask == 0 {
		// The line may alias the reader buffer. Retain only values used by
		// the server, and preserve first-value semantics even for blank ones.
		h.values[slot] = string(bytes.TrimSpace(value))
		h.seen |= mask
	}
	return nil
}

func matchHeader(name []byte) headerSlot {
	switch len(name) {
	case 4:
		if bytes.EqualFold(name, []byte("host")) {
			return hostHeader
		}
	case 6:
		if bytes.EqualFold(name, []byte("expect")) {
			return expectHeader
		}
	case 10:
		if bytes.EqualFold(name, []byte("connection")) {
			return connectionHeader
		}
	case 12:
		if bytes.EqualFold(name, []byte("git-protocol")) {
			return protocolHeader
		}
	case 13:
		if bytes.EqualFold(name, []byte("authorization")) {
			return authHeader
		}
	case 14:
		if bytes.EqualFold(name, []byte("content-length")) {
			return lengthHeader
		}
	case 17:
		if bytes.EqualFold(name, []byte("transfer-encoding")) {
			return transferHeader
		}
	}
	return headerSlots
}

// queryValue answers the first value of a query parameter.
func (q *request) queryValue(name string) string {
	for pair := range strings.SplitSeq(q.query, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if unescape(k) == name {
			return unescape(v)
		}
	}
	return ""
}

// queryInt answers a query parameter as an integer, or def when absent or bad.
func (q *request) queryInt(name string, def int) int {
	text := q.queryValue(name)
	if text == "" {
		return def
	}
	if n, err := strconv.Atoi(text); err == nil {
		return n
	}
	return def
}

// segments splits the path into decoded segments. The split is memoized:
// dispatch and the handler both ask for the segments of the same request.
func (q *request) segments() []string {
	if q.segsDone {
		return q.segs
	}
	q.segsDone = true
	trimmed := strings.Trim(q.path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	for i, p := range parts {
		parts[i] = unescape(p)
	}
	q.segs = parts
	return parts
}

// response is one HTTP response.
type response struct {
	status      int
	contentType string
	requestID   uint64
	oauthScopes bool
	nextLink    string
	body        []byte
}

func jsonResponse(status int, body []byte) response {
	return response{status: status, contentType: "application/json", body: body}
}

func textResponse(status int, body string) response {
	return response{status: status, contentType: "text/plain", body: []byte(body)}
}

// server holds the store, the expected bearer token and the listener.
type server struct {
	st    *store
	token string // expected bearer token; empty means any non-empty token passes
}

func newServer(st *store, token string) *server {
	return &server{st: st, token: token}
}

// authorized answers whether the request carries the expected bearer token.
func (s *server) authorized(q *request) bool {
	auth := q.headers.values[authHeader]
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimSpace(auth[len("Bearer "):])
	if got == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return got == s.token
}

// serve accepts connections until the listener closes.
func (s *server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Disable Nagle: the workloads are small request/response exchanges,
		// and a delayed ACK interacting with Nagle stalls each one. The
		// net/http server does the same for its connections.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go s.serveConn(conn)
	}
}

type connBuffers struct {
	reader *bufio.Reader
	writer *bufio.Writer
}

var connBufferPool = sync.Pool{New: func() any {
	return &connBuffers{bufio.NewReaderSize(nil, 8192), bufio.NewWriterSize(nil, 16384)}
}}

func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	buffers := connBufferPool.Get().(*connBuffers)
	br, bw := buffers.reader, buffers.writer
	br.Reset(conn)
	bw.Reset(conn)
	defer func() {
		br.Reset(nil)
		bw.Reset(nil)
		connBufferPool.Put(buffers)
	}()
	for {
		req, keepAlive, err := readRequest(br, bw)
		if err != nil {
			return
		}
		resp := s.dispatch(req)
		writeResponse(bw, resp, keepAlive)
		if err := bw.Flush(); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

const maxHeaderBytes = 64 * 1024
const maxBodyBytes = 64 * 1024 * 1024

// readRequest parses one request from the buffered reader. It answers the
// request, whether the connection stays open, and an error when the peer went
// away or the request is unusable.
func readRequest(br *bufio.Reader, bw *bufio.Writer) (*request, bool, error) {
	line, err := readLineCapped(br, maxHeaderBytes)
	if err != nil {
		return nil, false, err
	}
	if len(line) == 0 {
		// A stray blank line before the request: tolerate one, then retry.
		line, err = readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return nil, false, err
		}
	}
	method, target, version, err := parseStartLine(string(line))
	if err != nil {
		return nil, false, err
	}
	req := &request{method: method}
	req.path, req.query = splitTarget(target)
	if err := readHeaders(br, &req.headers); err != nil {
		return nil, false, err
	}
	req.host = req.headers.values[hostHeader]
	if strings.EqualFold(req.headers.values[expectHeader], "100-continue") {
		if _, err := bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return nil, false, err
		}
		if err := bw.Flush(); err != nil {
			return nil, false, err
		}
	}
	if req.body, err = readRequestBody(br, &req.headers); err != nil {
		return nil, false, err
	}
	connection := strings.ToLower(req.headers.values[connectionHeader])
	keepAlive := !strings.Contains(connection, "close") &&
		(version != "HTTP/1.0" || strings.Contains(connection, "keep-alive"))
	return req, keepAlive, nil
}

func parseStartLine(line string) (method, target, version string, err error) {
	method, tail, space := strings.Cut(line, " ")
	target, version, protocol := strings.Cut(tail, " ")
	if !space || !protocol {
		return "", "", "", fmt.Errorf("bad request line %q", line)
	}
	return method, target, version, nil
}

func splitTarget(target string) (path, query string) {
	path, query, _ = strings.Cut(target, "?")
	if scheme, authority, absolute := strings.Cut(path, "://"); absolute && (scheme == "http" || scheme == "https") {
		if slash := strings.IndexByte(authority, '/'); slash >= 0 {
			path = authority[slash:]
		} else {
			path = "/"
		}
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path, query
}

func readHeaders(br *bufio.Reader, headers *requestHeaders) error {
	remaining := maxHeaderBytes
	for {
		line, err := readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return err
		}
		remaining -= len(line) + 2
		if remaining < 0 {
			return fmt.Errorf("headers too large")
		}
		if len(line) == 0 {
			return nil
		}
		if err := headers.read(line); err != nil {
			return err
		}
	}
}

func readRequestBody(br *bufio.Reader, headers *requestHeaders) ([]byte, error) {
	if strings.Contains(strings.ToLower(headers.values[transferHeader]), "chunked") {
		return readChunked(br)
	}
	length := headers.values[lengthHeader]
	if length == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(length)
	if err != nil || n < 0 || n > maxBodyBytes {
		return nil, fmt.Errorf("bad content-length %q", length)
	}
	if n == 0 {
		return nil, nil
	}
	body := make([]byte, n)
	_, err = io.ReadFull(br, body)
	return body, err
}

// The common path aliases the reader buffer until the next read. Only lines
// spanning buffers require accumulation, bounded by limit at each step.
func readLineCapped(br *bufio.Reader, limit int) ([]byte, error) {
	var prefix []byte
	for {
		part, err := br.ReadSlice('\n')
		if len(prefix)+len(part) > limit {
			return nil, fmt.Errorf("line too long")
		}
		if err == bufio.ErrBufferFull {
			prefix = append(prefix, part...)
			continue
		}
		if len(prefix) != 0 {
			part = append(prefix, part...)
		}
		if len(part) == 0 && err != nil {
			return nil, err
		}
		return bytes.TrimRight(part, "\r\n"), nil
	}
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return nil, err
		}
		sizeText, _, _ := bytes.Cut(line, []byte{';'})
		n, err := strconv.ParseInt(string(bytes.TrimSpace(sizeText)), 16, 64)
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, fmt.Errorf("bad chunk size %q", line)
		}
		if n == 0 {
			// Trailer headers until a blank line.
			for {
				t, err := readLineCapped(br, maxHeaderBytes)
				if err != nil {
					return nil, err
				}
				if len(t) == 0 {
					break
				}
			}
			return out, nil
		}
		if n > int64(maxBodyBytes-len(out)) {
			return nil, fmt.Errorf("chunked body too large")
		}
		start := len(out)
		out = append(out, make([]byte, int(n))...)
		if _, err := io.ReadFull(br, out[start:]); err != nil {
			return nil, err
		}
		// The CRLF after the chunk.
		if _, err := readLineCapped(br, 16); err != nil {
			return nil, err
		}
	}
}

var statusReasons = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func writeResponse(bw *bufio.Writer, resp response, keepAlive bool) {
	reason := statusReasons[resp.status]
	if reason == "" {
		reason = "Status"
	}
	var local [256]byte
	h := local[:0]
	h = append(h, "HTTP/1.1 "...)
	h = strconv.AppendInt(h, int64(resp.status), 10)
	h = append(h, ' ')
	h = append(h, reason...)
	h = append(h, "\r\n"...)
	if resp.contentType != "" {
		h = append(h, "Content-Type: "...)
		h = append(h, resp.contentType...)
		h = append(h, "\r\n"...)
	}
	if resp.status != 204 {
		h = append(h, "Content-Length: "...)
		h = strconv.AppendInt(h, int64(len(resp.body)), 10)
		h = append(h, "\r\n"...)
	}
	if keepAlive {
		h = append(h, "Connection: keep-alive\r\n"...)
	} else {
		h = append(h, "Connection: close\r\n"...)
	}
	if resp.nextLink != "" {
		h = append(h, "Link: "...)
		h = append(h, resp.nextLink...)
		h = append(h, "\r\n"...)
	}
	if resp.requestID != 0 {
		h = append(h, "X-GitHub-Request-Id: req-"...)
		h = strconv.AppendUint(h, resp.requestID, 10)
		h = append(h, "\r\n"...)
	}
	if resp.oauthScopes {
		h = append(h, "X-OAuth-Scopes: repo, read:org, user\r\n"...)
	}
	h = append(h, "\r\n"...)
	bw.Write(h)
	if resp.status != 204 {
		bw.Write(resp.body)
	}
}

// unescape decodes %XX sequences and plus signs in one path or query segment.
func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s):
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
			} else {
				b.WriteByte(c)
			}
		case c == '+':
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
