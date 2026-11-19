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
	resp.requestID = s.st.reqSeq.Add(1)
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
		resp.oauthScopes = true
		return resp
	}
	if resp, ok := s.routeGit(q); ok {
		return resp
	}
	return jsonError(404, "Not Found")
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

// issueResponse caches the immutable JSON body used by the hot issue GET path.
// Callers hold the repository lock; mutating paths clear viewCache before the
// next read observes the changed issue.
func (s *server) issueResponse(r *repository, it *issue) response {
	it.viewMu.Lock()
	defer it.viewMu.Unlock()
	if it.viewCache == nil {
		it.viewCache = mustJSON(s.issueView(r, it))
	}
	return jsonResponse(200, it.viewCache)
}

func (it *issue) invalidateView() {
	it.viewMu.Lock()
	it.viewCache = nil
	it.viewMu.Unlock()
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
			return restPage(q, s.st.reposOf(s.st.viewerLogin), s.repoView)
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

type repoRoute struct {
	owner string
	name  string
}

// Callbacks inspect the repository and serialize a snapshot before unlocking.
// Body parsing stays at the endpoint so validation precedence is explicit.
func (s *server) withRepo(key repoRoute, write bool, handler func(*repository) response) response {
	if write {
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
	} else {
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
	}
	r := s.st.repo(key.owner, key.name)
	if r == nil {
		return restNotFound()
	}
	return handler(r)
}

func restNotFound() response { return jsonError(404, "Not Found") }

func restBadJSON() response { return jsonError(400, "Problems parsing JSON") }

func restJSON(status int, value any) response { return jsonResponse(status, mustJSON(value)) }

func restPage[T, V any](q *request, ordered []T, view func(T) V) response {
	page, size := q.queryInt("page", 1), q.queryInt("per_page", 30)
	start, end, next := paginate(len(ordered), page, size)
	result := make([]V, end-start)
	for i, item := range ordered[start:end] {
		result[i] = view(item)
	}
	resp := restJSON(200, result)
	if next {
		resp.nextLink = q.linkNext(page+1, size)
	}
	return resp
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
	key := repoRoute{owner, name}
	switch q.method {
	case "GET":
		return s.withRepo(key, false, func(r *repository) response {
			if tip, ok := s.resolveRef(r, q.queryValue("ref")); ok {
				if commit := r.commitOf(tip); commit != nil {
					return s.contentsGet(r, commit.tree, path)
				}
			}
			return restNotFound()
		})
	case "PUT", "DELETE":
		body, ok := parseBody(q)
		if !ok {
			return restBadJSON()
		}
		return s.withRepo(key, true, func(r *repository) response {
			if q.method == "PUT" {
				return s.contentsPut(r, path, body)
			}
			return s.contentsDelete(r, path, body)
		})
	}
	return restNotFound()
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
		return restJSON(200, fileContentView(path, oid, len(data), base64.StdEncoding.EncodeToString(data)))
	}
	// A directory?
	if dirOid := r.treeAtPath(treeOid, path); !dirOid.IsZero() {
		return s.contentsDir(r, dirOid, path)
	}
	return jsonError(404, "Not Found")
}

func (s *server) contentsDir(r *repository, treeOid Oid, prefix string) response {
	var tree []treeEntry
	if object := r.objects[treeOid]; object != nil && object.typ == objTree {
		tree = object.tree
	}
	if prefix != "" {
		prefix += "/"
	}
	entries := make([]contentEntryJSON, len(tree))
	for i, entry := range tree {
		item := contentEntryJSON{Type: "file", Name: entry.name, Path: prefix + entry.name, SHA: entry.oid.String()}
		if entry.isDir() {
			item.Type = "dir"
		} else if object := r.objects[entry.oid]; object != nil {
			item.Size = len(object.data)
		}
		entries[i] = item
	}
	return restJSON(200, entries)
}

func fileContentView(path string, oid Oid, size int, encoded string) contentFileJSON {
	return contentFileJSON{
		Type: "file", Encoding: "base64", Size: size,
		Name: path[strings.LastIndexByte(path, '/')+1:], Path: path,
		SHA: oid.String(), Content: encoded,
	}
}

type contentsEdit struct {
	ref      string
	message  string
	sha      Oid
	hasSHA   bool
	validSHA bool
}

func readContentsEdit(r *repository, body map[string]any) contentsEdit {
	branch, _ := strField(body, "branch")
	if branch == "" {
		branch = r.defaultBranch
	}
	message, _ := strField(body, "message")
	text, supplied := strField(body, "sha")
	oid, valid := parseOid(text)
	return contentsEdit{ref: branchRef(branch), message: message, sha: oid, hasSHA: supplied, validSHA: valid}
}

type contentSnapshot struct {
	files  map[string]flatFile
	parent Oid
	hasTip bool
}

func (r *repository) contentSnapshot(ref string, fallback bool) contentSnapshot {
	tip, exists := r.refs[ref]
	if !exists && fallback {
		tip, exists = r.refs[branchRef(r.defaultBranch)]
	}
	if exists {
		if commit := r.commitOf(tip); commit != nil {
			return contentSnapshot{files: r.flattenTree(commit.tree), parent: tip, hasTip: true}
		}
	}
	return contentSnapshot{files: r.flattenTree(ZeroOid)}
}

func (s *server) commitContent(r *repository, edit contentsEdit, snapshot contentSnapshot) Oid {
	var parents []Oid
	if snapshot.hasTip {
		parents = []Oid{snapshot.parent}
	}
	commit, _ := s.st.commitAsViewer(r, r.treeFromFlat(snapshot.files), parents, edit.message)
	r.refs[edit.ref] = commit
	return commit
}

func contentsResult(status int, file *contentFileJSON, commit Oid) response {
	return restJSON(status, map[string]any{
		"content": file,
		"commit":  map[string]string{"sha": commit.String()},
	})
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
	edit := readContentsEdit(r, body)
	snapshot := r.contentSnapshot(edit.ref, true)
	status := 201
	if cur, exists := snapshot.files[path]; exists {
		status = 200
		if !edit.hasSHA {
			return jsonError(422, "sha is required to update a file")
		}
		if !edit.validSHA || edit.sha != cur.oid {
			return contentsConflict()
		}
	}
	blob := r.putObject(objBlob, content)
	snapshot.files[path] = flatFile{mode: "100644", oid: blob}
	file := fileContentView(path, blob, len(content), contentB64)
	return contentsResult(status, &file, s.commitContent(r, edit, snapshot))
}

// contentsDelete removes a file, committing on the named branch.
func (s *server) contentsDelete(r *repository, path string, body map[string]any) response {
	edit := readContentsEdit(r, body)
	snapshot := r.contentSnapshot(edit.ref, false)
	current, exists := snapshot.files[path]
	if !snapshot.hasTip || !exists {
		return restNotFound()
	}
	if edit.hasSHA && (!edit.validSHA || current.oid != edit.sha) {
		return contentsConflict()
	}
	delete(snapshot.files, path)
	return contentsResult(200, nil, s.commitContent(r, edit, snapshot))
}

func contentsConflict() response {
	return jsonError(409, "the file changed since the given sha")
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
	key := repoRoute{owner, name}
	if len(tail) == 0 {
		return s.issueCollection(q, key)
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		return restNotFound()
	}
	if len(tail) == 2 && tail[1] == "comments" {
		return s.handleComments(q, key, number)
	}
	if len(tail) != 1 {
		return restNotFound()
	}
	write := q.method == "PATCH"
	if !write && q.method != "GET" {
		return restNotFound()
	}
	var patch map[string]any
	if write {
		var ok bool
		patch, ok = parseBody(q)
		if !ok {
			return restBadJSON()
		}
	}
	return s.withIssue(key, number, write, func(r *repository, it *issue) response {
		if write {
			if missing, invalid := applyIssuePatch(r, it, patch); invalid {
				return missingLabel(missing)
			}
			it.invalidateView()
		}
		if !write {
			return s.issueResponse(r, it)
		}
		return restJSON(200, s.issueView(r, it))
	})
}

func (s *server) issueCollection(q *request, key repoRoute) response {
	switch q.method {
	case "GET":
		return s.withRepo(key, false, func(r *repository) response {
			state, direction := restState(q), q.queryValue("direction")
			issues := selectInOrder(r.issues, direction == "" || direction == "desc", func(it *issue) bool {
				return state == "all" || it.state == state
			})
			return restPage(q, issues, func(it *issue) issueJSON { return s.issueView(r, it) })
		})
	case "POST":
		body, ok := parseBody(q)
		if !ok {
			return restBadJSON()
		}
		title, _ := strField(body, "title")
		if title == "" {
			return jsonError(422, "title is required")
		}
		labels := strListField(body, "labels")
		text, _ := strField(body, "body")
		return s.withRepo(key, true, func(r *repository) response {
			if missing, invalid := r.unknownLabel(labels); invalid {
				return missingLabel(missing)
			}
			return restJSON(201, s.issueView(r, s.st.createIssue(r, title, text, labels)))
		})
	}
	return restNotFound()
}

func restState(q *request) string {
	if state := q.queryValue("state"); state != "" {
		return state
	}
	return "open"
}

func missingLabel(label string) response { return jsonError(422, "the label "+label+" does not exist") }

func applyIssuePatch(r *repository, it *issue, body map[string]any) (string, bool) {
	if title, ok := strField(body, "title"); ok {
		it.title = title
	}
	if text, ok := strField(body, "body"); ok {
		it.body = text
	}
	state, _ := strField(body, "state")
	switch lower := strings.ToLower(state); lower {
	case "open", "closed":
		it.state = lower
	}
	// Label validation comes last: the existing API applies textual changes
	// even when the label replacement is rejected.
	if _, replacingLabels := body["labels"]; replacingLabels {
		labels := strListField(body, "labels")
		if name, invalid := r.unknownLabel(labels); invalid {
			return name, true
		}
		it.labels = labels
	}
	return "", false
}

func (s *server) withIssue(key repoRoute, number int, write bool, handler func(*repository, *issue) response) response {
	return s.withRepo(key, write, func(r *repository) response {
		it := r.issuesByNum[number]
		if it == nil {
			return restNotFound()
		}
		return handler(r, it)
	})
}

func (s *server) handleComments(q *request, key repoRoute, number int) response {
	switch q.method {
	case "GET":
		return s.withIssue(key, number, false, func(_ *repository, it *issue) response {
			views := make([]commentJSON, len(it.comments))
			viewer := s.viewerJSON()
			for i, c := range it.comments {
				views[i] = commentView(viewer, c)
			}
			return restJSON(200, views)
		})
	case "POST":
		body, ok := parseBody(q)
		if !ok {
			return restBadJSON()
		}
		text, _ := strField(body, "body")
		return s.withIssue(key, number, true, func(r *repository, it *issue) response {
			it.invalidateView()
			return restJSON(201, commentView(s.viewerJSON(), s.st.addComment(r, it, text)))
		})
	}
	return restNotFound()
}

func commentView(viewer userJSON, c *comment) commentJSON {
	return commentJSON{ID: c.id, Body: c.body, User: viewer, CreatedAt: fixedTime, UpdatedAt: fixedTime}
}

// ---------------------------------------------------------------------------
// Pull requests

func (s *server) handlePulls(q *request, owner, name string, tail []string) response {
	key := repoRoute{owner, name}
	if len(tail) == 0 {
		return s.pullCollection(q, key)
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		return restNotFound()
	}
	switch {
	case q.method == "GET" && len(tail) == 1:
		return s.withPull(key, number, false, func(r *repository, pr *pullRequest) response {
			return restJSON(200, s.pullView(r, pr))
		})
	case q.method == "GET" && len(tail) == 2 && tail[1] == "files":
		return s.withPull(key, number, false, func(r *repository, pr *pullRequest) response {
			return restJSON(200, s.pullFilesView(r, pr))
		})
	case q.method == "PUT" && len(tail) == 2 && tail[1] == "merge":
		if _, ok := parseBody(q); !ok {
			return restBadJSON()
		}
		return s.withPull(key, number, true, s.mergePullResponse)
	}
	return restNotFound()
}

func (s *server) withPull(key repoRoute, number int, write bool, handler func(*repository, *pullRequest) response) response {
	return s.withRepo(key, write, func(r *repository) response {
		pr := r.pullsByNum[number]
		if pr == nil {
			return restNotFound()
		}
		return handler(r, pr)
	})
}

func (s *server) mergePullResponse(r *repository, pr *pullRequest) response {
	sha, conflict, already := s.mergePullRequest(r, pr)
	switch {
	case already:
		return jsonError(405, "Pull request is already merged")
	case conflict:
		return jsonError(405, "Merge conflict")
	default:
		return restJSON(200, map[string]any{
			"sha": sha.String(), "merged": true, "message": "Pull request successfully merged",
		})
	}
}

func (s *server) pullCollection(q *request, key repoRoute) response {
	switch q.method {
	case "GET":
		return s.withRepo(key, false, func(r *repository) response {
			state := restState(q)
			pulls := selectInOrder(r.pulls, true, func(pr *pullRequest) bool {
				return state == "all" || pr.state == state
			})
			return restPage(q, pulls, func(pr *pullRequest) pullJSON { return s.pullView(r, pr) })
		})
	case "POST":
		return s.openPull(q, key)
	}
	return restNotFound()
}

func (s *server) openPull(q *request, key repoRoute) response {
	body, ok := parseBody(q)
	if !ok {
		return restBadJSON()
	}
	title, _ := strField(body, "title")
	headName, hasHead := strField(body, "head")
	baseName, hasBase := strField(body, "base")
	if title == "" || !hasHead || !hasBase {
		return jsonError(422, "title, head and base are required")
	}
	head, base := stripOwnerPrefix(headName), stripOwnerPrefix(baseName)
	text, _ := strField(body, "body")
	return s.withRepo(key, true, func(r *repository) response {
		headTip, ok := r.refs[branchRef(head)]
		if !ok {
			return jsonError(422, "the head branch does not exist")
		}
		baseTip, ok := r.refs[branchRef(base)]
		if !ok {
			return jsonError(422, "the base branch does not exist")
		}
		pr := s.st.createPull(r, title, text, head, base, headTip, baseTip)
		return restJSON(201, s.pullView(r, pr))
	})
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
	headTip := r.refs[branchRef(pr.headRef)]
	baseTip := r.refs[branchRef(pr.baseRef)]
	changes := r.diffTrees(r.commitTree(r.mergeBase(baseTip, headTip)), r.commitTree(headTip))
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
	baseRef := branchRef(pr.baseRef)
	baseTip := r.refs[baseRef]
	headTip := r.refs[branchRef(pr.headRef)]
	merged, ok := r.mergeCommits(baseTip, headTip)
	if !ok {
		return ZeroOid, true, false
	}
	mergeSha, ts := s.st.commitAsViewer(r, r.treeFromFlat(merged), []Oid{baseTip}, pr.title)
	r.refs[baseRef] = mergeSha
	pr.state = "closed"
	pr.merged = true
	pr.mergedAt = formatTime(ts)
	pr.mergeSha = mergeSha
	return mergeSha, false, false
}
