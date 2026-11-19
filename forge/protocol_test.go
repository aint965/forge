package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestHTTPChunkedThenKeepAlive(t *testing.T) {
	const exchange = "POST http://localhost/mcp?x=1 HTTP/1.1\r\n" +
		"Host: localhost\r\nAUTHORIZATION: Bearer first\r\nAuthorization: Bearer second\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n3;ignored=yes\r\nfoo\r\n3\r\nbar\r\n0\r\nTrailer: test\r\n\r\n" +
		"GET /healthz/ HTTP/1.0\r\nConnection: close\r\n\r\n"
	reader := bufio.NewReaderSize(strings.NewReader(exchange), 17)
	writer := bufio.NewWriter(io.Discard)
	first, keep, err := readRequest(reader, writer)
	if err != nil || !keep {
		t.Fatalf("first request: keep=%v err=%v", keep, err)
	}
	if first.path != "/mcp" || first.queryValue("x") != "1" || string(first.body) != "foobar" {
		t.Fatalf("wrong path, query or body: %q %q %q", first.path, first.query, first.body)
	}
	second, keep, err := readRequest(reader, writer)
	if err != nil || keep || second.path != "/healthz" {
		t.Fatalf("second request: request=%v keep=%v err=%v", second, keep, err)
	}
	if first.headers.values[authHeader] != "Bearer first" || second.headers.values[authHeader] != "" {
		t.Fatal("header was overwritten or leaked into the next request")
	}
}

func TestHTTPResponseHeaderOrder(t *testing.T) {
	var wire bytes.Buffer
	writer := bufio.NewWriter(&wire)
	writeResponse(writer, response{
		status: 200, contentType: "application/json", requestID: 123,
		oauthScopes: true, nextLink: "next", body: []byte("{}"),
	}, true)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	const want = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n" +
		"Connection: keep-alive\r\nLink: next\r\nX-GitHub-Request-Id: req-123\r\n" +
		"X-OAuth-Scopes: repo, read:org, user\r\n\r\n{}"
	if wire.String() != want {
		t.Fatalf("wire = %q", wire.String())
	}
}

func TestGraphQLBatchProjectsFragmentsAndVariables(t *testing.T) {
	st := newStore()
	s := newServer(st, "")
	r := st.createRepo("alice", "sample", "", false, false)
	st.createIssue(r, "first", "", nil)
	st.createIssue(r, "second", "", nil)
	query := `query Other { viewer { login } }
		query Page($repo: String!, $cursor: String!) {
		  repo: repository(owner: "al\u0069ce", name: $repo) { ...FirstIssue }
		}
		fragment FirstIssue on Repository {
		  issues(first: 1, after: $cursor, orderBy: {direction: ASC}, states: [OPEN]) {
		    nodes { ... on Issue { title } }
		    pageInfo { hasNextPage endCursor }
		  }
		}`
	body, err := json.Marshal([]map[string]any{
		{"query": query, "operationName": "Page", "variables": map[string]any{"repo": "sample", "cursor": encodeCursor(1)}},
		{"query": "{ viewer { unknown } }"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.handleGraphQL(&request{body: body})
	const want = `[{"data":{"repo":{"issues":{"nodes":[{"title":"second"}],"pageInfo":{"endCursor":"Y3Vyc29yOjI=","hasNextPage":false}}}}},{"data":{"viewer":{"unknown":null}},"errors":[{"message":"Cannot query field \"unknown\" on type \"User\"."}]}]`
	if got.status != 200 || string(got.body) != want {
		t.Fatalf("status %d, body %s", got.status, got.body)
	}
}
