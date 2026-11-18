// The HTTP/1.1 server, hand-rolled over raw TCP: one goroutine per
// connection, sequential requests on keep-alive connections, Content-Length
// and chunked request bodies, 100-continue. No net/http: the surface is small
// and the benchmark's read workload is 2,000 sequential requests on one warm
// connection, so the parse/write path stays allocation-light.

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// request is one parsed HTTP request.
type request struct {
	method  string
	target  string // raw request target
	path    string // decoded path, no query, no trailing slash (except root)
	query   string
	headers map[string]string // lowercased names, first value wins
	body    []byte
	host    string
}

func (q *request) header(name string) string { return q.headers[strings.ToLower(name)] }

// queryValue answers the first value of a query parameter.
func (q *request) queryValue(name string) string {
	for _, pair := range strings.Split(q.query, "&") {
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

// segments splits the path into decoded segments.
func (q *request) segments() []string {
	trimmed := strings.Trim(q.path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	for i, p := range parts {
		parts[i] = unescape(p)
	}
	return parts
}

// response is one HTTP response.
type response struct {
	status      int
	contentType string
	headers     map[string]string
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
	auth := q.header("authorization")
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
		go s.serveConn(conn)
	}
}

func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	bw := bufio.NewWriterSize(conn, 65536)
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
	// Request line.
	line, err := readLineCapped(br, maxHeaderBytes)
	if err != nil {
		return nil, false, err
	}
	if line == "" {
		// A stray blank line before the request: tolerate one, then retry.
		line, err = readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return nil, false, err
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, fmt.Errorf("bad request line %q", line)
	}
	method, target, version := parts[0], parts[1], parts[2]
	req := &request{method: method, target: target, headers: map[string]string{}}
	// Headers.
	headerBytes := 0
	for {
		h, err := readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return nil, false, err
		}
		headerBytes += len(h) + 2
		if headerBytes > maxHeaderBytes {
			return nil, false, fmt.Errorf("headers too large")
		}
		if h == "" {
			break
		}
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return nil, false, fmt.Errorf("bad header %q", h)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if _, seen := req.headers[name]; !seen {
			req.headers[name] = value
		}
	}
	// Path and query.
	path := target
	if i := strings.IndexByte(target, '?'); i >= 0 {
		path = target[:i]
		req.query = target[i+1:]
	}
	// Absolute-form targets (http://host/path) are reduced to their path.
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if i := strings.Index(path[8:], "/"); i >= 0 {
			path = path[8+i:]
		} else {
			path = "/"
		}
	}
	req.path = path
	if len(req.path) > 1 {
		req.path = strings.TrimRight(req.path, "/")
	}
	req.host = req.header("host")

	// 100-continue: answer before reading the body.
	if strings.EqualFold(req.header("expect"), "100-continue") {
		bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		bw.Flush()
	}

	// Body.
	if strings.Contains(strings.ToLower(req.header("transfer-encoding")), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, false, err
		}
		req.body = body
	} else if cl := req.header("content-length"); cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, false, fmt.Errorf("bad content-length %q", cl)
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, false, err
			}
			req.body = body
		}
	}

	keepAlive := true
	conn := strings.ToLower(req.header("connection"))
	if strings.Contains(conn, "close") {
		keepAlive = false
	} else if version == "HTTP/1.0" && !strings.Contains(conn, "keep-alive") {
		keepAlive = false
	}
	return req, keepAlive, nil
}

func readLineCapped(br *bufio.Reader, cap int) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if len(line) > cap {
		return "", fmt.Errorf("line too long")
	}
	line = strings.TrimRight(line, "\r\n")
	return line, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := readLineCapped(br, maxHeaderBytes)
		if err != nil {
			return nil, err
		}
		sizeText, _, _ := strings.Cut(line, ";")
		n, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
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
				if t == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > maxBodyBytes {
			return nil, fmt.Errorf("chunked body too large")
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
	var h strings.Builder
	fmt.Fprintf(&h, "HTTP/1.1 %d %s\r\n", resp.status, reason)
	if resp.contentType != "" {
		fmt.Fprintf(&h, "Content-Type: %s\r\n", resp.contentType)
	}
	if resp.status != 204 {
		fmt.Fprintf(&h, "Content-Length: %d\r\n", len(resp.body))
	}
	if keepAlive {
		h.WriteString("Connection: keep-alive\r\n")
	} else {
		h.WriteString("Connection: close\r\n")
	}
	for k, v := range resp.headers {
		fmt.Fprintf(&h, "%s: %s\r\n", k, v)
	}
	h.WriteString("\r\n")
	bw.WriteString(h.String())
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
