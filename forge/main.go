// forge: a GitHub-compatible git forge in one process. It binds an ephemeral
// port, registers it with the grading environment, serves git smart HTTP,
// REST v3, GraphQL and MCP from one in-memory store, and exits 0 once the
// environment reports the suite is done.
//
// Local runs set FORGE_PORT to bind a fixed port and skip the register/done
// dance; MAKEFUN_SHOP_TOKEN names the bearer token every API call presents.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}

func run() error {
	// The contest runs the entry under a 2-CPU limit, but Go 1.24 sizes
	// GOMAXPROCS from the host's CPU count, which the sandbox does not
	// restrict: on a many-core host the scheduler and the GC would run
	// dozens of threads on 2 real CPUs. Cap it at the contest's limit.
	if runtime.NumCPU() > 2 {
		runtime.GOMAXPROCS(2)
	}
	st := newStore()
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	srv := newServer(st, token)

	if port := os.Getenv("FORGE_PORT"); port != "" {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "forge: listening on %s (viewer %s)\n", ln.Addr(), st.viewerLogin)
		srv.serve(ln)
		return nil
	}

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set (set FORGE_PORT for a local run)")
	}
	hostPort, err := envHostPort(envURL)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stderr, "forge: listening on %s (viewer %s)\n", ln.Addr(), st.viewerLogin)
	go srv.serve(ln)

	// Register once: POST {"port": N} to the environment.
	if err := registerWithEnv(hostPort, addr.Port); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "forge: registered")

	// Poll /done until the suite is over, then exit 0.
	pollDone(hostPort)
	fmt.Fprintln(os.Stderr, "forge: done, exiting")
	return nil
}

// envHostPort reduces an env base URL (http://host:port) to its host:port.
func envHostPort(envURL string) (string, error) {
	rest := strings.TrimSuffix(envURL, "/")
	rest = strings.TrimPrefix(rest, "http://")
	rest = strings.TrimPrefix(rest, "https://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", fmt.Errorf("MAKEFUN_ENV_URL %q names no host", envURL)
	}
	return rest, nil
}

// httpExchange performs one small HTTP/1.1 request over a fresh connection
// and answers the status and body. The environment's answers are tiny JSON.
func httpExchange(hostPort, method, path, body, contentType string) (int, []byte, error) {
	conn, err := net.DialTimeout("tcp", hostPort, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, path, hostPort)
	if contentType != "" {
		fmt.Fprintf(&req, "Content-Type: %s\r\n", contentType)
	}
	fmt.Fprintf(&req, "Content-Length: %d\r\nConnection: close\r\n\r\n", len(body))
	req.WriteString(body)
	if _, err := conn.Write([]byte(req.String())); err != nil {
		return 0, nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, err
	}
	length := -1
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, _ := strings.Cut(line, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		case "transfer-encoding":
			chunked = strings.Contains(strings.ToLower(value), "chunked")
		}
	}
	if chunked {
		var out []byte
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return 0, nil, err
			}
			sizeText, _, _ := strings.Cut(strings.TrimSpace(line), ";")
			n, err := strconv.ParseInt(sizeText, 16, 64)
			if err != nil {
				return 0, nil, err
			}
			if n == 0 {
				break
			}
			chunk := make([]byte, n)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return 0, nil, err
			}
			out = append(out, chunk...)
			if _, err := br.ReadString('\n'); err != nil {
				return 0, nil, err
			}
		}
		return status, out, nil
	}
	if length < 0 {
		rest, err := io.ReadAll(br)
		return status, rest, err
	}
	respBody := make([]byte, length)
	if _, err := io.ReadFull(br, respBody); err != nil {
		return 0, nil, err
	}
	return status, respBody, nil
}

// registerWithEnv POSTs the bound port, retrying transport failures briefly.
// A 409 (already registered) is accepted: the first registration wins, and it
// is this process's.
func registerWithEnv(hostPort string, port int) error {
	body := fmt.Sprintf(`{"port":%d}`, port)
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		status, _, err := httpExchange(hostPort, "POST", "/register", body, "application/json")
		if err == nil {
			if status == 200 || status == 201 || status == 409 {
				return nil
			}
			return fmt.Errorf("register answered HTTP %d", status)
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("register failed: %w", lastErr)
}

// pollDone GETs /done with backoff until the environment answers done=true.
func pollDone(hostPort string) {
	delay := 250 * time.Millisecond
	for {
		status, body, err := httpExchange(hostPort, "GET", "/done", "", "")
		if err == nil && status == 200 {
			var done struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(body, &done) == nil && done.Done {
				return
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}
