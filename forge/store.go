// The in-memory store: repositories, git objects, issues, pull requests and
// labels, all in one process. Every surface (git smart HTTP, REST, GraphQL,
// MCP) reads and writes this same store, so a change through one is visible
// through the others at once.
//
// Determinism: no wall clock and no randomness anywhere in the store. Server
// created commits and timestamps come from a monotonic counter seeded at the
// scenario epoch, and iteration order is always fixed (sorted refs, creation
// order for issues and pulls).

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var sha1Pool = sync.Pool{New: func() any { return sha1.New() }}

// ---------------------------------------------------------------------------
// Object ids and object types

// Oid is a 20-byte SHA-1 object id.
type Oid [20]byte

// ZeroOid is the all-zero id: an unborn ref's value.
var ZeroOid Oid

func (o Oid) String() string { return hex.EncodeToString(o[:]) }

func (o Oid) IsZero() bool { return o == ZeroOid }

// parseOid decodes 40 lowercase or uppercase hex characters.
func parseOid(s string) (Oid, bool) {
	var o Oid
	if len(s) != 40 {
		return o, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, false
	}
	copy(o[:], b)
	return o, true
}

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

func typeName(t byte) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

// hashObjectId is SHA-1 over "<type> <size>\0" + data, the git object id.
// The header is built in a stack buffer and the hash comes from a pool: the
// fmt.Fprintf form allocated on every object stored.
func hashObjectId(typ byte, data []byte) Oid {
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	var hdr [32]byte
	n := copy(hdr[:], typeName(typ))
	hdr[n] = ' '
	n++
	n += len(strconv.AppendInt(hdr[n:n], int64(len(data)), 10))
	hdr[n] = 0
	n++
	h.Write(hdr[:n])
	h.Write(data)
	var o Oid
	copy(o[:], h.Sum(nil))
	sha1Pool.Put(h)
	return o
}

// ---------------------------------------------------------------------------
// Parsed object metadata, computed once when an object enters the store.

type parsedCommit struct {
	tree      Oid
	parents   []Oid
	author    string // raw "Name <email> ts tz" value
	committer string
	message   string // raw message bytes after the header blank line
}

type treeEntry struct {
	mode string // "100644", "40000", "100755", "120000", "160000"
	name string
	oid  Oid
}

func (e treeEntry) isDir() bool { return e.mode == "40000" }

type parsedTag struct {
	object  Oid
	objType string
	tag     string
	message string
}

// gitObject is one stored object with its parsed form for commits, trees and tags.
type gitObject struct {
	typ    byte
	data   []byte
	commit *parsedCommit
	tree   []treeEntry
	tag    *parsedTag
}

// parseCommitHeader reads a commit object's tree, parents, author, committer and
// message. A malformed object yields an empty commit rather than an error: the
// object is still stored and hashed, it just carries no ancestry.
func parseCommitHeader(data []byte) *parsedCommit {
	c := &parsedCommit{}
	rest := data
	for len(rest) > 0 {
		line, next := splitLine(rest)
		rest = next
		if len(line) == 0 {
			break
		}
		text := string(line)
		switch {
		case strings.HasPrefix(text, "tree "):
			if oid, ok := parseOid(strings.TrimSpace(text[5:])); ok {
				c.tree = oid
			}
		case strings.HasPrefix(text, "parent "):
			if oid, ok := parseOid(strings.TrimSpace(text[7:])); ok {
				c.parents = append(c.parents, oid)
			}
		case strings.HasPrefix(text, "author "):
			c.author = text[7:]
		case strings.HasPrefix(text, "committer "):
			c.committer = text[10:]
		}
	}
	c.message = string(rest)
	return c
}

// parseTreeEntries reads a tree object's entries.
func parseTreeEntries(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := indexByte(data, ' ')
		if sp < 0 {
			break
		}
		nul := indexByte(data[sp+1:], 0)
		if nul < 0 || sp+1+nul+20 > len(data) {
			break
		}
		var oid Oid
		copy(oid[:], data[sp+1+nul+1:sp+1+nul+21])
		out = append(out, treeEntry{mode: string(data[:sp]), name: string(data[sp+1 : sp+1+nul]), oid: oid})
		data = data[sp+1+nul+21:]
	}
	return out
}

// parseTagHeader reads a tag object's target, type and message.
func parseTagHeader(data []byte) *parsedTag {
	t := &parsedTag{}
	rest := data
	for len(rest) > 0 {
		line, next := splitLine(rest)
		rest = next
		if len(line) == 0 {
			break
		}
		text := string(line)
		switch {
		case strings.HasPrefix(text, "object "):
			if oid, ok := parseOid(strings.TrimSpace(text[7:])); ok {
				t.object = oid
			}
		case strings.HasPrefix(text, "type "):
			t.objType = strings.TrimSpace(text[5:])
		case strings.HasPrefix(text, "tag "):
			t.tag = strings.TrimSpace(text[4:])
		}
	}
	t.message = string(rest)
	return t
}

func splitLine(b []byte) (line, rest []byte) {
	if i := indexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Issues, comments, pull requests, labels

type comment struct {
	id   int
	body string
	seq  int
}

type issue struct {
	number   int
	id       int // globally unique REST id
	title    string
	body     string
	state    string // "open" or "closed"
	labels   []string
	comments []*comment
	seq      int // creation order within the repository
}

type pullRequest struct {
	number   int
	id       int
	title    string
	body     string
	headRef  string
	baseRef  string
	headSha  Oid    // head branch tip at creation
	baseSha  Oid    // base branch tip at creation
	state    string // "open" or "closed"
	merged   bool
	mergedAt string
	mergeSha Oid
	seq      int
}

type label struct {
	name  string
	color string
}

// ---------------------------------------------------------------------------
// Repository

type repository struct {
	id            int // numeric database id, assigned in creation order
	owner         string
	name          string
	description   string
	hasDesc       bool
	private       bool
	defaultBranch string

	objects map[Oid]*gitObject
	refs    map[string]Oid // full ref name -> oid

	issues      []*issue
	issuesByNum map[int]*issue
	pulls       []*pullRequest
	pullsByNum  map[int]*pullRequest
	labels      map[string]*label

	seq        int // shared issue/pull-request number counter
	commentSeq int
	createdSeq int
}

func (r *repository) fullName() string { return r.owner + "/" + r.name }

// putObject stores an object, parsing commits, trees and tags. The returned id
// is the object's hash. Storing the same object twice is a no-op.
func (r *repository) putObject(typ byte, data []byte) Oid {
	oid := hashObjectId(typ, data)
	if _, ok := r.objects[oid]; ok {
		return oid
	}
	o := &gitObject{typ: typ, data: data}
	switch typ {
	case objCommit:
		o.commit = parseCommitHeader(data)
	case objTree:
		o.tree = parseTreeEntries(data)
	case objTag:
		o.tag = parseTagHeader(data)
	}
	r.objects[oid] = o
	return oid
}

func (r *repository) object(oid Oid) *gitObject { return r.objects[oid] }

// peelToCommit follows tag objects until a commit (or a non-tag object) is
// reached. It answers the peeled oid and whether it is a commit.
func (r *repository) peelToCommit(oid Oid) (Oid, bool) {
	for i := 0; i < 64; i++ {
		o := r.objects[oid]
		if o == nil {
			return oid, false
		}
		if o.typ == objCommit {
			return oid, true
		}
		if o.typ == objTag && o.tag != nil {
			oid = o.tag.object
			continue
		}
		return oid, false
	}
	return oid, false
}

// peelTarget follows tag objects to the first non-tag object.
func (r *repository) peelTarget(oid Oid) Oid {
	for i := 0; i < 64; i++ {
		o := r.objects[oid]
		if o == nil || o.typ != objTag || o.tag == nil {
			return oid
		}
		oid = o.tag.object
	}
	return oid
}

// commitOf answers the parsed commit for an oid, following tags.
func (r *repository) commitOf(oid Oid) *parsedCommit {
	peeled, ok := r.peelToCommit(oid)
	if !ok {
		return nil
	}
	o := r.objects[peeled]
	if o == nil {
		return nil
	}
	return o.commit
}

// refNames returns all ref names in sorted order.
func (r *repository) refNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// The store

type store struct {
	mu sync.RWMutex

	repos map[string]*repository // key: owner + "\x00" + name
	order []*repository          // creation order

	repoSeq    int // next database id
	issueIDSeq int // next global issue REST id
	pullIDSeq  int // next global pull REST id
	commentSeq int // next global comment REST id
	reqSeq     atomic.Uint64
	timeSeq    int64 // deterministic timestamp counter

	viewerLogin string
	viewerName  string
	viewerEmail string
	viewerID    int
}

func repoKey(owner, name string) string { return owner + "\x00" + name }

// newStore builds an empty store and derives the token owner's login from the
// contest's one fixed scenario seed (see scenario.go).
func newStore() *store {
	viewer := deriveViewer()
	return &store{
		repos:       map[string]*repository{},
		viewerLogin: viewer.login,
		viewerName:  viewer.name,
		viewerEmail: viewer.email,
		viewerID:    viewer.id,
		timeSeq:     1735689600, // 2025-01-01T00:00:00Z
	}
}

// nextTime advances the deterministic clock and answers the new epoch second.
func (s *store) nextTime() int64 {
	s.timeSeq++
	return s.timeSeq
}

// nextRequestID answers a fresh X-GitHub-Request-Id value.
func (s *store) nextRequestID() string {
	var buf [24]byte
	b := append(buf[:0], "req-"...)
	b = strconv.AppendUint(b, s.reqSeq.Add(1), 10)
	return string(b)
}

// createRepo makes a repository owned by owner. It answers nil when a
// repository of that name already exists under that owner.
func (s *store) createRepo(owner, name, description string, hasDesc, private bool) *repository {
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; ok {
		return nil
	}
	s.repoSeq++
	r := &repository{
		id:            s.repoSeq,
		owner:         owner,
		name:          name,
		description:   description,
		hasDesc:       hasDesc,
		private:       private,
		defaultBranch: "main",
		objects:       map[Oid]*gitObject{},
		refs:          map[string]Oid{},
		issuesByNum:   map[int]*issue{},
		pullsByNum:    map[int]*pullRequest{},
		labels:        map[string]*label{},
		createdSeq:    len(s.order),
	}
	s.repos[key] = r
	s.order = append(s.order, r)
	return r
}

func (s *store) repo(owner, name string) *repository {
	return s.repos[repoKey(owner, name)]
}

func (s *store) deleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	for i, cand := range s.order {
		if cand == r {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

// reposOf lists the repositories owned by owner, in creation order.
func (s *store) reposOf(owner string) []*repository {
	var out []*repository
	for _, r := range s.order {
		if r.owner == owner {
			out = append(out, r)
		}
	}
	return out
}

// nextNumber answers the next shared issue/pull-request number for a repo.
func (r *repository) nextNumber() int {
	r.seq++
	return r.seq
}

// createIssue appends an issue. Labels must already exist; the caller checks.
func (s *store) createIssue(r *repository, title, body string, labels []string) *issue {
	s.issueIDSeq++
	it := &issue{
		number: r.nextNumber(),
		id:     s.issueIDSeq,
		title:  title,
		body:   body,
		state:  "open",
		labels: append([]string(nil), labels...),
		seq:    len(r.issues) + len(r.pulls),
	}
	r.issues = append(r.issues, it)
	r.issuesByNum[it.number] = it
	return it
}

// createPull appends a pull request, capturing the branch tips at creation.
func (s *store) createPull(r *repository, title, body, headRef, baseRef string, headSha, baseSha Oid) *pullRequest {
	s.pullIDSeq++
	pr := &pullRequest{
		number:  r.nextNumber(),
		id:      s.pullIDSeq,
		title:   title,
		body:    body,
		headRef: headRef,
		baseRef: baseRef,
		headSha: headSha,
		baseSha: baseSha,
		state:   "open",
		seq:     len(r.issues) + len(r.pulls),
	}
	r.pulls = append(r.pulls, pr)
	r.pullsByNum[pr.number] = pr
	return pr
}

// addComment appends a comment to an issue.
func (s *store) addComment(r *repository, it *issue, body string) *comment {
	s.commentSeq++
	r.commentSeq++
	c := &comment{id: s.commentSeq, body: body, seq: r.commentSeq}
	it.comments = append(it.comments, c)
	return c
}
