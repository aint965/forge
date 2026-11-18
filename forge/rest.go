// REST v3: repositories, branches, commits, contents, labels, issues,
// comments and pull requests, plus the merge endpoint. Every reply is JSON,
// carries a non-empty X-GitHub-Request-Id, and — on this surface —
// X-OAuth-Scopes. Errors are JSON with a string message; unknown routes are
// 404, malformed bodies are 400, and a missing token is 401.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Dispatch

// dispatch routes one request and stamps every response with a request id.
func (s *server) dispatch(q *request) response {
	resp := s.route(q)
	if resp.headers == nil {
		resp.headers = map[string]string{}
	}
	resp.headers["X-GitHub-Request-Id"] = s.st.nextRequestID()
	return resp
}

func (s *server) route(q *request) response {
	if q.path == "/healthz" {
		if q.method != "GET" {
			return jsonError(404, "Not Found")
		}
		return jsonResponse(200, []byte(`{"ok":true}`))
	}
	if q.path == "/api/graphql" || q.path == "/api/graphql/" {
		if !s.authorized(q) {
			return jsonError(401, "Requires authentication")
		}
		if q.method != "POST" {
			return jsonError(404, "Not Found")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		return s.handleGraphQL(q)
	}
	if q.path == "/mcp" {
		if !s.authorized(q) {
			return jsonError(401, "Requires authentication")
		}
		if q.method != "POST" {
			return jsonError(404, "Not Found")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		return s.handleMCP(q)
	}
	if q.path == "/api/v3" || q.path == "/api/v3/" || strings.HasPrefix(q.path, "/api/v3/") {
		if !s.authorized(q) {
			return jsonError(401, "Requires authentication")
		}
		resp := s.handleREST(q)
		if resp.headers == nil {
			resp.headers = map[string]string{}
		}
		resp.headers["X-OAuth-Scopes"] = "repo, read:org, user"
		return resp
	}
	if resp, ok := s.routeGit(q); ok {
		return resp
	}
	return jsonError(404, "Not Found")
}

// routeGit handles /{owner}/{repo}.git/info/refs and the two smart-HTTP
// POST endpoints. It answers ok=false when the path is not a git path.
func (s *server) routeGit(q *request) (response, bool) {
	segs := q.segments()
	if len(segs) < 3 || !strings.HasSuffix(segs[1], ".git") {
		return response{}, false
	}
	owner, repoName := segs[0], strings.TrimSuffix(segs[1], ".git")
	isInfoRefs := len(segs) == 4 && segs[2] == "info" && segs[3] == "refs"
	isUpload := len(segs) == 3 && segs[2] == "git-upload-pack"
	isReceive := len(segs) == 3 && segs[2] == "git-receive-pack"
	if !isInfoRefs && !isUpload && !isReceive {
		return response{}, false
	}
	if !s.authorized(q) {
		return jsonError(401, "Requires authentication"), true
	}
	if isInfoRefs {
		if q.method != "GET" {
			return jsonError(404, "Not Found"), true
		}
		service := q.queryValue("service")
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.st.repo(owner, repoName)
		if r == nil {
			return jsonError(404, "Not Found"), true
		}
		switch service {
		case "git-upload-pack":
			if q.header("git-protocol") == "version=2" {
				return response{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: advertiseV2(service)}, true
			}
			return response{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: advertiseV0(r, service, uploadPackCapsV0)}, true
		case "git-receive-pack":
			return response{status: 200, contentType: "application/x-git-receive-pack-advertisement", body: advertiseV0(r, service, receivePackCapsV0)}, true
		}
		return jsonError(400, "unknown service"), true
	}
	if q.method != "POST" {
		return jsonError(404, "Not Found"), true
	}
	if isUpload {
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.st.repo(owner, repoName)
		if r == nil {
			return jsonError(404, "Not Found"), true
		}
		body, contentType, status := handleUploadPackPOST(r, q.body)
		return response{status: status, contentType: contentType, body: body}, true
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	r := s.st.repo(owner, repoName)
	if r == nil {
		return jsonError(404, "Not Found"), true
	}
	body, contentType, status := handleReceivePackPOST(r, q.body)
	return response{status: status, contentType: contentType, body: body}, true
}

// ---------------------------------------------------------------------------
// JSON helpers

func jsonError(status int, message string) response {
	body, _ := json.Marshal(map[string]string{"message": message})
	return jsonResponse(status, body)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"message":"internal error"}`)
	}
	return b
}

// parseBody decodes a JSON request body. An empty body decodes as an empty
// object; malformed JSON answers ok=false (the caller sends 400).
func parseBody(q *request) (map[string]any, bool) {
	if len(q.body) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(q.body, &m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(m map[string]any, key string, def bool) bool {
	v, ok := m[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// strListField reads a string list; objects contribute their "name" field.
func strListField(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		switch t := e.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if name, ok := strField(t, "name"); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// JSON shapes

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func (s *server) viewerJSON() userJSON {
	return userJSON{Login: s.st.viewerLogin, ID: s.st.viewerID, Type: "User", Name: s.st.viewerName, Email: s.st.viewerEmail}
}

type repoJSON struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   *string  `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	HasIssues     bool     `json:"has_issues"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	URL           string   `json:"url"`
	HTMLURL       string   `json:"html_url"`
}

func (s *server) repoView(r *repository) repoJSON {
	var desc *string
	if r.hasDesc {
		d := r.description
		desc = &d
	}
	return repoJSON{
		ID:            r.id,
		Name:          r.name,
		FullName:      r.fullName(),
		Owner:         s.viewerJSON(),
		Private:       r.private,
		Description:   desc,
		DefaultBranch: r.defaultBranch,
		HasIssues:     true,
		CreatedAt:     fixedTime,
		UpdatedAt:     fixedTime,
		URL:           "/api/v3/repos/" + r.fullName(),
		HTMLURL:       "/" + r.fullName(),
	}
}

const fixedTime = "2025-01-01T00:00:00Z"

func formatTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueJSON struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	Comments  int         `json:"comments"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
}

func (s *server) issueView(r *repository, it *issue) issueJSON {
	labels := make([]labelJSON, 0, len(it.labels))
	for _, name := range it.labels {
		color := "ededed"
		if l, ok := r.labels[name]; ok {
			color = l.color
		}
		labels = append(labels, labelJSON{Name: name, Color: color})
	}
	return issueJSON{
		ID: it.id, Number: it.number, Title: it.title, Body: it.body, State: it.state,
		Labels: labels, User: s.viewerJSON(), Comments: len(it.comments),
		CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
}

type commentJSON struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type refJSON struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type pullJSON struct {
	ID             int      `json:"id"`
	Number         int      `json:"number"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	State          string   `json:"state"`
	Head           refJSON  `json:"head"`
	Base           refJSON  `json:"base"`
	Merged         bool     `json:"merged"`
	MergedAt       *string  `json:"merged_at"`
	MergeCommitSHA *string  `json:"merge_commit_sha"`
	Mergeable      bool     `json:"mergeable"`
	User           userJSON `json:"user"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

func (s *server) pullView(r *repository, pr *pullRequest) pullJSON {
	headSha, _ := r.refs["refs/heads/"+pr.headRef]
	baseSha, _ := r.refs["refs/heads/"+pr.baseRef]
	// The head/base shas answer the branch tips at creation when the branches
	// still point there; live tips otherwise keep the view current.
	_ = headSha
	_ = baseSha
	var mergedAt, mergeSha *string
	if pr.merged {
		mergedAt = &pr.mergedAt
		m := pr.mergeSha.String()
		mergeSha = &m
	}
	mergeable := false
	if pr.state == "open" {
		mergeable = r.mergeable(r.refs["refs/heads/"+pr.baseRef], r.refs["refs/heads/"+pr.headRef])
	}
	return pullJSON{
		ID: pr.id, Number: pr.number, Title: pr.title, Body: pr.body, State: pr.state,
		Head:   refJSON{Ref: pr.headRef, SHA: pr.headSha.String(), Label: r.owner + ":" + pr.headRef},
		Base:   refJSON{Ref: pr.baseRef, SHA: pr.baseSha.String(), Label: r.owner + ":" + pr.baseRef},
		Merged: pr.merged, MergedAt: mergedAt, MergeCommitSHA: mergeSha, Mergeable: mergeable,
		User: s.viewerJSON(), CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
}

type branchJSON struct {
	Name      string  `json:"name"`
	Commit    shaJSON `json:"commit"`
	Protected bool    `json:"protected"`
}

type shaJSON struct {
	SHA string `json:"sha"`
}

type commitJSON struct {
	SHA    string           `json:"sha"`
	Commit commitDetailJSON `json:"commit"`
}

type commitDetailJSON struct {
	Message   string     `json:"message"`
	Author    personJSON `json:"author"`
	Committer personJSON `json:"committer"`
}

type personJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// parsePerson splits a "Name <email> ts tz" identity line.
func parsePerson(raw string) personJSON {
	name, email, date := "", "", fixedTime
	if i := strings.IndexByte(raw, '<'); i >= 0 {
		name = strings.TrimSpace(raw[:i])
		rest := raw[i+1:]
		if j := strings.IndexByte(rest, '>'); j >= 0 {
			email = rest[:j]
			fields := strings.Fields(rest[j+1:])
			if len(fields) > 0 {
				if ts, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					date = formatTime(ts)
				}
			}
		}
	}
	return personJSON{Name: name, Email: email, Date: date}
}

func (s *server) commitView(r *repository, oid Oid) commitJSON {
	c := r.commitOf(oid)
	msg := ""
	var author, committer personJSON
	if c != nil {
		msg = strings.TrimSuffix(c.message, "\n")
		author = parsePerson(c.author)
		committer = parsePerson(c.committer)
	}
	return commitJSON{SHA: oid.String(), Commit: commitDetailJSON{Message: msg, Author: author, Committer: committer}}
}

// ---------------------------------------------------------------------------
// Pagination

// paginate slices a list and answers the page plus whether more pages follow.
func paginate(total, page, perPage int) (start, end int, hasNext bool) {
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	if page <= 0 {
		page = 1
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	hasNext = end < total
	return start, end, hasNext
}

// linkNext builds a Link header for the next page, preserving the query.
func (q *request) linkNext(page, perPage int) string {
	values, _ := url.ParseQuery(q.query)
	values.Set("page", strconv.Itoa(page))
	values.Set("per_page", strconv.Itoa(perPage))
	host := q.host
	if host == "" {
		host = "127.0.0.1"
	}
	next := "http://" + host + q.path + "?" + values.Encode()
	return "<" + next + `>; rel="next"`
}

// ---------------------------------------------------------------------------
// REST routing

func (s *server) handleREST(q *request) response {
	segs := q.segments()
	// segs[0] == "api", segs[1] == "v3" (the dispatcher already matched).
	rest := segs[2:]
	if len(rest) == 0 {
		if q.method == "GET" {
			return jsonResponse(200, mustJSON(map[string]string{
				"current_user_url": "/api/v3/user",
				"repository_url":   "/api/v3/repos/{owner}/{repo}",
			}))
		}
		return jsonError(404, "Not Found")
	}
	switch rest[0] {
	case "meta":
		if q.method == "GET" && len(rest) == 1 {
			return jsonResponse(200, mustJSON(map[string]any{
				"installed_version":                  "1.0.0",
				"verifiable_password_authentication": true,
				"ssh_key_fingerprints":               map[string]string{},
				"hooks":                              []string{},
				"packages":                           []string{},
				"actions":                            []string{},
				"dependabot":                         []string{},
			}))
		}
	case "user":
		return s.handleUser(q, rest[1:])
	case "repos":
		return s.handleRepos(q, rest[1:])
	}
	return jsonError(404, "Not Found")
}

func (s *server) handleUser(q *request, rest []string) response {
	if len(rest) == 0 {
		if q.method == "GET" {
			s.st.mu.RLock()
			defer s.st.mu.RUnlock()
			return jsonResponse(200, mustJSON(s.viewerJSON()))
		}
		return jsonError(404, "Not Found")
	}
	if len(rest) == 1 && rest[0] == "repos" {
		switch q.method {
		case "GET":
			s.st.mu.RLock()
			defer s.st.mu.RUnlock()
			repos := s.st.reposOf(s.st.viewerLogin)
			page := q.queryInt("page", 1)
			perPage := q.queryInt("per_page", 30)
			start, end, hasNext := paginate(len(repos), page, perPage)
			views := make([]repoJSON, 0, end-start)
			for _, r := range repos[start:end] {
				views = append(views, s.repoView(r))
			}
			resp := jsonResponse(200, mustJSON(views))
			if hasNext {
				resp.headers = map[string]string{"Link": q.linkNext(page+1, perPage)}
			}
			return resp
		case "POST":
			body, ok := parseBody(q)
			if !ok {
				return jsonError(400, "Problems parsing JSON")
			}
			name, hasName := strField(body, "name")
			if !hasName || strings.TrimSpace(name) == "" {
				return jsonError(422, "name is required")
			}
			description, hasDesc := strField(body, "description")
			private := boolField(body, "private", false)
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			r := s.st.createRepo(s.st.viewerLogin, name, description, hasDesc, private)
			if r == nil {
				return jsonError(422, "name already exists on this account")
			}
			return jsonResponse(201, mustJSON(s.repoView(r)))
		}
	}
	return jsonError(404, "Not Found")
}

// lookupRepo resolves /repos/{owner}/{repo} under the caller's lock.
func (s *server) lookupRepo(owner, name string) *repository {
	return s.st.repo(owner, name)
}

func (s *server) handleRepos(q *request, rest []string) response {
	if len(rest) < 2 {
		return jsonError(404, "Not Found")
	}
	owner, name := rest[0], rest[1]
	tail := rest[2:]

	// The repository itself.
	if len(tail) == 0 {
		switch q.method {
		case "GET":
			s.st.mu.RLock()
			defer s.st.mu.RUnlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			return jsonResponse(200, mustJSON(s.repoView(r)))
		case "PATCH":
			body, ok := parseBody(q)
			if !ok {
				return jsonError(400, "Problems parsing JSON")
			}
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			if desc, has := strField(body, "description"); has {
				r.description = desc
				r.hasDesc = true
			}
			if _, has := body["private"]; has {
				r.private = boolField(body, "private", r.private)
			}
			return jsonResponse(200, mustJSON(s.repoView(r)))
		case "DELETE":
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			if !s.st.deleteRepo(owner, name) {
				return jsonError(404, "Not Found")
			}
			return response{status: 204}
		}
		return jsonError(404, "Not Found")
	}

	switch tail[0] {
	case "branches":
		return s.handleBranches(q, owner, name, tail[1:])
	case "commits":
		return s.handleCommits(q, owner, name, tail[1:])
	case "contents":
		return s.handleContents(q, owner, name, tail[1:])
	case "git":
		return s.handleGitData(q, owner, name, tail[1:])
	case "labels":
		return s.handleLabels(q, owner, name, tail[1:])
	case "issues":
		return s.handleIssues(q, owner, name, tail[1:])
	case "pulls":
		return s.handlePulls(q, owner, name, tail[1:])
	}
	return jsonError(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Branches and commits

func (s *server) handleBranches(q *request, owner, name string, tail []string) response {
	if q.method != "GET" {
		return jsonError(404, "Not Found")
	}
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	r := s.lookupRepo(owner, name)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	if len(tail) == 0 {
		views := []branchJSON{}
		for _, refName := range r.refNames() {
			if !strings.HasPrefix(refName, "refs/heads/") {
				continue
			}
			views = append(views, branchJSON{
				Name:   strings.TrimPrefix(refName, "refs/heads/"),
				Commit: shaJSON{SHA: r.refs[refName].String()},
			})
		}
		return jsonResponse(200, mustJSON(views))
	}
	branch := strings.Join(tail, "/")
	oid, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return jsonError(404, "Not Found")
	}
	return jsonResponse(200, mustJSON(branchJSON{Name: branch, Commit: shaJSON{SHA: oid.String()}}))
}

func (s *server) handleCommits(q *request, owner, name string, tail []string) response {
	if q.method != "GET" || len(tail) != 0 {
		return jsonError(404, "Not Found")
	}
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	r := s.lookupRepo(owner, name)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	tip, ok := s.resolveRef(r, q.queryValue("sha"))
	if !ok {
		return jsonResponse(200, []byte("[]"))
	}
	views := []commitJSON{}
	for _, oid := range r.historyOf(tip) {
		views = append(views, s.commitView(r, oid))
	}
	return jsonResponse(200, mustJSON(views))
}

// resolveRef answers the tip a ref query names: a branch name, a full ref, or
// a commit id. An empty query means the default branch.
func (s *server) resolveRef(r *repository, ref string) (Oid, bool) {
	if ref == "" {
		oid, ok := r.refs["refs/heads/"+r.defaultBranch]
		return oid, ok
	}
	if oid, ok := r.refs[ref]; ok {
		return oid, true
	}
	if oid, ok := r.refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if oid, ok := parseOid(ref); ok {
		if r.objects[oid] != nil {
			return oid, true
		}
	}
	return ZeroOid, false
}

// ---------------------------------------------------------------------------
// Contents

type contentFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Content  string `json:"content"`
}

type contentEntryJSON struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
}

func (s *server) handleContents(q *request, owner, name string, tail []string) response {
	path := strings.Join(tail, "/")
	switch q.method {
	case "GET":
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		tip, ok := s.resolveRef(r, q.queryValue("ref"))
		if !ok {
			return jsonError(404, "Not Found")
		}
		c := r.commitOf(tip)
		if c == nil {
			return jsonError(404, "Not Found")
		}
		return s.contentsGet(r, c.tree, path)
	case "PUT":
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		return s.contentsPut(r, path, body)
	case "DELETE":
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		return s.contentsDelete(r, path, body)
	}
	return jsonError(404, "Not Found")
}

// contentsGet answers a file read or a directory listing.
func (s *server) contentsGet(r *repository, treeOid Oid, path string) response {
	if path == "" {
		return s.contentsDir(r, treeOid, "")
	}
	// A file?
	if oid, _, ok := r.blobAtPath(treeOid, path); ok {
		o := r.objects[oid]
		data := []byte{}
		if o != nil {
			data = o.data
		}
		name := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			name = path[i+1:]
		}
		return jsonResponse(200, mustJSON(contentFileJSON{
			Type: "file", Encoding: "base64", Size: len(data), Name: name,
			Path: path, SHA: oid.String(), Content: base64.StdEncoding.EncodeToString(data),
		}))
	}
	// A directory?
	if dirOid := r.treeAtPath(treeOid, path); !dirOid.IsZero() {
		return s.contentsDir(r, dirOid, path)
	}
	return jsonError(404, "Not Found")
}

func (s *server) contentsDir(r *repository, treeOid Oid, prefix string) response {
	o := r.objects[treeOid]
	entries := []contentEntryJSON{}
	if o != nil && o.typ == objTree {
		for _, e := range o.tree {
			typ := "file"
			size := 0
			if e.isDir() {
				typ = "dir"
			} else if blob := r.objects[e.oid]; blob != nil {
				size = len(blob.data)
			}
			p := e.name
			if prefix != "" {
				p = prefix + "/" + e.name
			}
			entries = append(entries, contentEntryJSON{Type: typ, Name: e.name, Path: p, SHA: e.oid.String(), Size: size})
		}
	}
	return jsonResponse(200, mustJSON(entries))
}

// contentsPut creates or updates a file, committing on the named branch.
func (s *server) contentsPut(r *repository, path string, body map[string]any) response {
	if path == "" || strings.Contains(path, "..") {
		return jsonError(422, "invalid path")
	}
	contentB64, hasContent := strField(body, "content")
	if !hasContent {
		return jsonError(422, "content is required")
	}
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return jsonError(422, "content is not valid base64")
	}
	message, _ := strField(body, "message")
	branch, hasBranch := strField(body, "branch")
	if !hasBranch || branch == "" {
		branch = r.defaultBranch
	}
	shaText, hasSha := strField(body, "sha")

	refName := "refs/heads/" + branch
	tip, exists := r.refs[refName]
	var parentTree Oid
	var parents []Oid
	if exists {
		if c := r.commitOf(tip); c != nil {
			parentTree = c.tree
			parents = []Oid{tip}
		}
	} else {
		// A branch that does not exist yet starts from the default branch's
		// tip, or from nothing on an unborn repository.
		if defTip, ok := r.refs["refs/heads/"+r.defaultBranch]; ok && branch != r.defaultBranch {
			if c := r.commitOf(defTip); c != nil {
				parentTree = c.tree
				parents = []Oid{defTip}
			}
		}
	}
	flat := r.flattenTree(parentTree)
	creating := true
	if cur, ok := flat[path]; ok {
		creating = false
		if !hasSha {
			return jsonError(422, "sha is required to update a file")
		}
		if want, ok := parseOid(shaText); !ok || want != cur.oid {
			return jsonError(409, "the file changed since the given sha")
		}
	}
	blobOid := r.putObject(objBlob, content)
	flat[path] = flatFile{mode: "100644", oid: blobOid}
	newTree := r.treeFromFlat(flat)
	ts := s.st.nextTime()
	identity := s.st.viewerName + " <" + s.st.viewerEmail + ">"
	commitOid := r.buildCommitObject(newTree, parents, message, identity, identity, ts)
	r.refs[refName] = commitOid

	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	status := 201
	if !creating {
		status = 200
	}
	return jsonResponse(status, mustJSON(map[string]any{
		"content": contentFileJSON{
			Type: "file", Encoding: "base64", Size: len(content), Name: name,
			Path: path, SHA: blobOid.String(), Content: contentB64,
		},
		"commit": map[string]string{"sha": commitOid.String()},
	}))
}

// contentsDelete removes a file, committing on the named branch.
func (s *server) contentsDelete(r *repository, path string, body map[string]any) response {
	branch, hasBranch := strField(body, "branch")
	if !hasBranch || branch == "" {
		branch = r.defaultBranch
	}
	message, _ := strField(body, "message")
	shaText, hasSha := strField(body, "sha")
	refName := "refs/heads/" + branch
	tip, exists := r.refs[refName]
	if !exists {
		return jsonError(404, "Not Found")
	}
	c := r.commitOf(tip)
	if c == nil {
		return jsonError(404, "Not Found")
	}
	flat := r.flattenTree(c.tree)
	cur, ok := flat[path]
	if !ok {
		return jsonError(404, "Not Found")
	}
	if hasSha {
		if want, ok := parseOid(shaText); !ok || want != cur.oid {
			return jsonError(409, "the file changed since the given sha")
		}
	}
	delete(flat, path)
	newTree := r.treeFromFlat(flat)
	ts := s.st.nextTime()
	identity := s.st.viewerName + " <" + s.st.viewerEmail + ">"
	commitOid := r.buildCommitObject(newTree, []Oid{tip}, message, identity, identity, ts)
	r.refs[refName] = commitOid
	return jsonResponse(200, mustJSON(map[string]any{
		"content": nil,
		"commit":  map[string]string{"sha": commitOid.String()},
	}))
}

// ---------------------------------------------------------------------------
// Git Data API

type refObjectJSON struct {
	Ref    string      `json:"ref"`
	Object shaTypeJSON `json:"object"`
}

type shaTypeJSON struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

func (s *server) handleGitData(q *request, owner, name string, tail []string) response {
	if len(tail) == 0 || tail[0] != "refs" {
		return jsonError(404, "Not Found")
	}
	rest := tail[1:]
	switch q.method {
	case "POST":
		if len(rest) != 0 {
			return jsonError(404, "Not Found")
		}
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		ref, hasRef := strField(body, "ref")
		shaText, hasSha := strField(body, "sha")
		if !hasRef || !hasSha || !strings.HasPrefix(ref, "refs/") {
			return jsonError(422, "ref and sha are required")
		}
		oid, ok := parseOid(shaText)
		if !ok {
			return jsonError(422, "sha is not an object id")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		if r.objects[oid] == nil {
			return jsonError(422, "the object does not exist")
		}
		if _, exists := r.refs[ref]; exists {
			return jsonError(422, "Reference already exists")
		}
		r.refs[ref] = oid
		return jsonResponse(201, mustJSON(refObjectJSON{Ref: ref, Object: shaTypeJSON{SHA: oid.String(), Type: typeName(r.objects[oid].typ)}}))
	case "GET":
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		if len(rest) == 0 {
			views := []refObjectJSON{}
			for _, refName := range r.refNames() {
				oid := r.refs[refName]
				views = append(views, refObjectJSON{Ref: refName, Object: shaTypeJSON{SHA: oid.String(), Type: typeName(r.objects[oid].typ)}})
			}
			return jsonResponse(200, mustJSON(views))
		}
		want := "refs/" + strings.Join(rest, "/")
		if oid, ok := r.refs[want]; ok {
			return jsonResponse(200, mustJSON(refObjectJSON{Ref: want, Object: shaTypeJSON{SHA: oid.String(), Type: typeName(r.objects[oid].typ)}}))
		}
		// A prefix: every ref under it.
		views := []refObjectJSON{}
		for _, refName := range r.refNames() {
			if strings.HasPrefix(refName, want) {
				oid := r.refs[refName]
				views = append(views, refObjectJSON{Ref: refName, Object: shaTypeJSON{SHA: oid.String(), Type: typeName(r.objects[oid].typ)}})
			}
		}
		if len(views) == 0 {
			return jsonError(404, "Not Found")
		}
		return jsonResponse(200, mustJSON(views))
	}
	return jsonError(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Labels

func (s *server) handleLabels(q *request, owner, name string, tail []string) response {
	if len(tail) != 0 {
		return jsonError(404, "Not Found")
	}
	switch q.method {
	case "GET":
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		views := []labelJSON{}
		names := make([]string, 0, len(r.labels))
		for n := range r.labels {
			names = append(names, n)
		}
		sortStrings(names)
		for _, n := range names {
			views = append(views, labelJSON{Name: n, Color: r.labels[n].color})
		}
		return jsonResponse(200, mustJSON(views))
	case "POST":
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		labelName, has := strField(body, "name")
		if !has || labelName == "" {
			return jsonError(422, "name is required")
		}
		color, _ := strField(body, "color")
		if color == "" {
			color = "ededed"
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		if existing, ok := r.labels[labelName]; ok {
			return jsonResponse(201, mustJSON(labelJSON{Name: existing.name, Color: existing.color}))
		}
		r.labels[labelName] = &label{name: labelName, color: color}
		return jsonResponse(201, mustJSON(labelJSON{Name: labelName, Color: color}))
	}
	return jsonError(404, "Not Found")
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Issues and comments

func (s *server) handleIssues(q *request, owner, name string, tail []string) response {
	// /issues
	if len(tail) == 0 {
		switch q.method {
		case "GET":
			s.st.mu.RLock()
			defer s.st.mu.RUnlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			return s.listIssues(q, r)
		case "POST":
			body, ok := parseBody(q)
			if !ok {
				return jsonError(400, "Problems parsing JSON")
			}
			title, hasTitle := strField(body, "title")
			if !hasTitle || title == "" {
				return jsonError(422, "title is required")
			}
			bodyText, _ := strField(body, "body")
			labels := strListField(body, "labels")
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			for _, l := range labels {
				if _, ok := r.labels[l]; !ok {
					return jsonError(422, "the label "+l+" does not exist")
				}
			}
			it := s.st.createIssue(r, title, bodyText, labels)
			return jsonResponse(201, mustJSON(s.issueView(r, it)))
		}
		return jsonError(404, "Not Found")
	}
	// /issues/{n}[/comments]
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		return jsonError(404, "Not Found")
	}
	if len(tail) == 2 && tail[1] == "comments" {
		return s.handleComments(q, owner, name, number)
	}
	if len(tail) != 1 {
		return jsonError(404, "Not Found")
	}
	switch q.method {
	case "GET":
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		it := r.issuesByNum[number]
		if it == nil {
			return jsonError(404, "Not Found")
		}
		return jsonResponse(200, mustJSON(s.issueView(r, it)))
	case "PATCH":
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		it := r.issuesByNum[number]
		if it == nil {
			return jsonError(404, "Not Found")
		}
		if title, has := strField(body, "title"); has {
			it.title = title
		}
		if text, has := strField(body, "body"); has {
			it.body = text
		}
		if state, has := strField(body, "state"); has {
			switch strings.ToLower(state) {
			case "open", "closed":
				it.state = strings.ToLower(state)
			}
		}
		if _, has := body["labels"]; has {
			labels := strListField(body, "labels")
			for _, l := range labels {
				if _, ok := r.labels[l]; !ok {
					return jsonError(422, "the label "+l+" does not exist")
				}
			}
			it.labels = labels
		}
		return jsonResponse(200, mustJSON(s.issueView(r, it)))
	}
	return jsonError(404, "Not Found")
}

// listIssues filters, sorts and pages a repository's issues.
func (s *server) listIssues(q *request, r *repository) response {
	state := q.queryValue("state")
	if state == "" {
		state = "open"
	}
	direction := q.queryValue("direction")
	if direction == "" {
		direction = "desc"
	}
	var filtered []*issue
	for _, it := range r.issues {
		if state != "all" && it.state != state {
			continue
		}
		filtered = append(filtered, it)
	}
	// Creation order; desc reverses it.
	if direction == "desc" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	page := q.queryInt("page", 1)
	perPage := q.queryInt("per_page", 30)
	start, end, hasNext := paginate(len(filtered), page, perPage)
	views := make([]issueJSON, 0, end-start)
	for _, it := range filtered[start:end] {
		views = append(views, s.issueView(r, it))
	}
	resp := jsonResponse(200, mustJSON(views))
	if hasNext {
		resp.headers = map[string]string{"Link": q.linkNext(page+1, perPage)}
	}
	return resp
}

func (s *server) handleComments(q *request, owner, name string, number int) response {
	switch q.method {
	case "GET":
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		it := r.issuesByNum[number]
		if it == nil {
			return jsonError(404, "Not Found")
		}
		views := []commentJSON{}
		for _, c := range it.comments {
			views = append(views, commentJSON{ID: c.id, Body: c.body, User: s.viewerJSON(), CreatedAt: fixedTime, UpdatedAt: fixedTime})
		}
		return jsonResponse(200, mustJSON(views))
	case "POST":
		body, ok := parseBody(q)
		if !ok {
			return jsonError(400, "Problems parsing JSON")
		}
		text, _ := strField(body, "body")
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		it := r.issuesByNum[number]
		if it == nil {
			return jsonError(404, "Not Found")
		}
		c := s.st.addComment(r, it, text)
		return jsonResponse(201, mustJSON(commentJSON{ID: c.id, Body: c.body, User: s.viewerJSON(), CreatedAt: fixedTime, UpdatedAt: fixedTime}))
	}
	return jsonError(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Pull requests

func (s *server) handlePulls(q *request, owner, name string, tail []string) response {
	if len(tail) == 0 {
		switch q.method {
		case "GET":
			s.st.mu.RLock()
			defer s.st.mu.RUnlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			state := q.queryValue("state")
			if state == "" {
				state = "open"
			}
			var filtered []*pullRequest
			for _, pr := range r.pulls {
				if state != "all" && pr.state != state {
					continue
				}
				filtered = append(filtered, pr)
			}
			// Newest first.
			for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
			page := q.queryInt("page", 1)
			perPage := q.queryInt("per_page", 30)
			start, end, hasNext := paginate(len(filtered), page, perPage)
			views := make([]pullJSON, 0, end-start)
			for _, pr := range filtered[start:end] {
				views = append(views, s.pullView(r, pr))
			}
			resp := jsonResponse(200, mustJSON(views))
			if hasNext {
				resp.headers = map[string]string{"Link": q.linkNext(page+1, perPage)}
			}
			return resp
		case "POST":
			body, ok := parseBody(q)
			if !ok {
				return jsonError(400, "Problems parsing JSON")
			}
			title, hasTitle := strField(body, "title")
			head, hasHead := strField(body, "head")
			base, hasBase := strField(body, "base")
			if !hasTitle || title == "" || !hasHead || !hasBase {
				return jsonError(422, "title, head and base are required")
			}
			bodyText, _ := strField(body, "body")
			head = stripOwnerPrefix(head)
			base = stripOwnerPrefix(base)
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			r := s.lookupRepo(owner, name)
			if r == nil {
				return jsonError(404, "Not Found")
			}
			headSha, okHead := r.refs["refs/heads/"+head]
			baseSha, okBase := r.refs["refs/heads/"+base]
			if !okHead {
				return jsonError(422, "the head branch does not exist")
			}
			if !okBase {
				return jsonError(422, "the base branch does not exist")
			}
			pr := s.st.createPull(r, title, bodyText, head, base, headSha, baseSha)
			return jsonResponse(201, mustJSON(s.pullView(r, pr)))
		}
		return jsonError(404, "Not Found")
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		return jsonError(404, "Not Found")
	}
	if len(tail) == 2 && tail[1] == "files" {
		if q.method != "GET" {
			return jsonError(404, "Not Found")
		}
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		pr := r.pullsByNum[number]
		if pr == nil {
			return jsonError(404, "Not Found")
		}
		return jsonResponse(200, mustJSON(s.pullFilesView(r, pr)))
	}
	if len(tail) == 2 && tail[1] == "merge" {
		if q.method != "PUT" {
			return jsonError(404, "Not Found")
		}
		if len(q.body) > 0 {
			if _, ok := parseBody(q); !ok {
				return jsonError(400, "Problems parsing JSON")
			}
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		r := s.lookupRepo(owner, name)
		if r == nil {
			return jsonError(404, "Not Found")
		}
		pr := r.pullsByNum[number]
		if pr == nil {
			return jsonError(404, "Not Found")
		}
		sha, conflict, already := s.mergePullRequest(r, pr)
		if already {
			return jsonError(405, "Pull request is already merged")
		}
		if conflict {
			return jsonError(405, "Merge conflict")
		}
		return jsonResponse(200, mustJSON(map[string]any{
			"sha": sha.String(), "merged": true, "message": "Pull request successfully merged",
		}))
	}
	if len(tail) != 1 || q.method != "GET" {
		return jsonError(404, "Not Found")
	}
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	r := s.lookupRepo(owner, name)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	pr := r.pullsByNum[number]
	if pr == nil {
		return jsonError(404, "Not Found")
	}
	return jsonResponse(200, mustJSON(s.pullView(r, pr)))
}

// stripOwnerPrefix turns "owner:branch" into "branch".
func stripOwnerPrefix(ref string) string {
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

type pullFileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	SHA       string `json:"sha"`
}

// pullFilesView diffs the merge-base tree against the head tip's tree.
func (s *server) pullFilesView(r *repository, pr *pullRequest) []pullFileJSON {
	headTip := r.refs["refs/heads/"+pr.headRef]
	baseTip := r.refs["refs/heads/"+pr.baseRef]
	base := r.mergeBase(baseTip, headTip)
	var baseTree, headTree Oid
	if c := r.commitOf(base); c != nil {
		baseTree = c.tree
	}
	if c := r.commitOf(headTip); c != nil {
		headTree = c.tree
	}
	changes := r.diffTrees(baseTree, headTree)
	views := make([]pullFileJSON, 0, len(changes))
	for _, ch := range changes {
		sha := ""
		if !ch.blobOid.IsZero() {
			sha = ch.blobOid.String()
		}
		views = append(views, pullFileJSON{
			Filename: ch.path, Status: ch.status, Additions: ch.additions,
			Deletions: ch.deletions, Changes: ch.additions + ch.deletions, SHA: sha,
		})
	}
	return views
}

// mergePullRequest squash-merges a pull request onto its base branch. It
// answers the merge commit, whether the merge conflicted, and whether the
// pull request was already closed. The caller holds the store lock.
func (s *server) mergePullRequest(r *repository, pr *pullRequest) (Oid, bool, bool) {
	if pr.state != "open" {
		return ZeroOid, false, true
	}
	baseTip := r.refs["refs/heads/"+pr.baseRef]
	headTip := r.refs["refs/heads/"+pr.headRef]
	base := r.mergeBase(baseTip, headTip)
	var baseTree, oursTree, theirsTree Oid
	if c := r.commitOf(base); c != nil {
		baseTree = c.tree
	}
	if c := r.commitOf(baseTip); c != nil {
		oursTree = c.tree
	}
	if c := r.commitOf(headTip); c != nil {
		theirsTree = c.tree
	}
	merged, ok := r.mergeTrees(r.flattenTree(baseTree), r.flattenTree(oursTree), r.flattenTree(theirsTree))
	if !ok {
		return ZeroOid, true, false
	}
	newTree := r.treeFromFlat(merged)
	ts := s.st.nextTime()
	identity := s.st.viewerName + " <" + s.st.viewerEmail + ">"
	mergeSha := r.buildCommitObject(newTree, []Oid{baseTip}, pr.title, identity, identity, ts)
	r.refs["refs/heads/"+pr.baseRef] = mergeSha
	pr.state = "closed"
	pr.merged = true
	pr.mergedAt = formatTime(ts)
	pr.mergeSha = mergeSha
	return mergeSha, false, false
}
