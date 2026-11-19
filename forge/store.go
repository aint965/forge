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
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"hash"
	"maps"
	"slices"
	"strconv"
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

func (o Oid) appendHex(dst []byte) []byte { return hex.AppendEncode(dst, o[:]) }

func (o Oid) IsZero() bool { return o == ZeroOid }

// parseOid decodes 40 lowercase or uppercase hex characters.
func parseOid(s string) (Oid, bool) {
	if len(s) != hex.EncodedLen(len(ZeroOid)) {
		return ZeroOid, false
	}
	var oid Oid
	_, err := hex.Decode(oid[:], []byte(s))
	if err != nil {
		return ZeroOid, false
	}
	return oid, true
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
	var storage [32]byte
	header := append(storage[:0], typeName(typ)...)
	header = strconv.AppendInt(append(header, ' '), int64(len(data)), 10)
	h.Write(append(header, 0))
	h.Write(data)
	var oid Oid
	h.Sum(oid[:0])
	sha1Pool.Put(h)
	return oid
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
	var commit parsedCommit
	header := gitHeader{remaining: data}
	for key, value := range header.fields {
		switch string(key) {
		case "tree", "parent":
			oid, valid := parseOid(string(bytes.TrimSpace(value)))
			if !valid {
				continue
			}
			if string(key) == "tree" {
				commit.tree = oid
			} else {
				commit.parents = append(commit.parents, oid)
			}
		case "author":
			commit.author = string(value)
		case "committer":
			commit.committer = string(value)
		}
	}
	commit.message = string(header.remaining)
	return &commit
}

// parseTreeEntries reads a tree object's entries.
func parseTreeEntries(data []byte) []treeEntry {
	var entries []treeEntry
	for len(data) != 0 {
		entry, remaining, ok := readTreeEntry(data)
		if !ok {
			return entries
		}
		entries = append(entries, entry)
		data = remaining
	}
	return entries
}

func readTreeEntry(data []byte) (treeEntry, []byte, bool) {
	mode, afterMode, separated := bytes.Cut(data, []byte{' '})
	if !separated {
		return treeEntry{}, nil, false
	}
	name, afterName, terminated := bytes.Cut(afterMode, []byte{0})
	if !terminated || len(afterName) < len(ZeroOid) {
		return treeEntry{}, nil, false
	}
	entry := treeEntry{mode: string(mode), name: string(name), oid: Oid(afterName[:len(ZeroOid)])}
	return entry, afterName[len(ZeroOid):], true
}

// parseTagHeader reads a tag object's target, type and message.
func parseTagHeader(data []byte) *parsedTag {
	var tag parsedTag
	header := gitHeader{remaining: data}
	for key, value := range header.fields {
		switch string(key) {
		case "object":
			if target, valid := parseOid(string(bytes.TrimSpace(value))); valid {
				tag.object = target
			}
		case "type":
			tag.objType = string(bytes.TrimSpace(value))
		case "tag":
			tag.tag = string(bytes.TrimSpace(value))
		}
	}
	tag.message = string(header.remaining)
	return &tag
}

// gitHeader traverses the key/value lines shared by commits and tags. After
// iteration, remaining contains the message, including its original newlines.
type gitHeader struct {
	remaining []byte
}

func (h *gitHeader) fields(yield func(key, value []byte) bool) {
	for len(h.remaining) > 0 {
		line, tail, _ := bytes.Cut(h.remaining, []byte{'\n'})
		h.remaining = tail
		if len(line) == 0 {
			return
		}
		key, value, present := bytes.Cut(line, []byte{' '})
		if present && !yield(key, value) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Issues, comments, pull requests, labels

type comment struct {
	id   int
	body string
	seq  int
}

type issue struct {
	number    int
	id        int // globally unique REST id
	title     string
	body      string
	state     string // "open" or "closed"
	labels    []string
	comments  []*comment
	seq       int // creation order within the repository
	viewMu    sync.Mutex
	viewCache []byte // cached JSON for the hot GET issue representation
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
	clones  shallowReplyCache

	issues      []*issue
	issuesByNum map[int]*issue
	pulls       []*pullRequest
	pullsByNum  map[int]*pullRequest
	labels      map[string]*label

	seq        int // shared issue/pull-request number counter
	commentSeq int
}

func (r *repository) fullName() string { return r.owner + "/" + r.name }

// putObject stores an object, parsing commits, trees and tags. The returned id
// is the object's hash. Storing the same object twice is a no-op.
func (r *repository) putObject(typ byte, data []byte) Oid {
	oid := hashObjectId(typ, data)
	r.insertObject(oid, typ, data)
	return oid
}

func (r *repository) insertObject(oid Oid, typ byte, data []byte) {
	if r.objects[oid] != nil {
		return
	}
	r.objects[oid] = newGitObject(typ, data)
	// A new object can complete a tree or tag in a cached reply.
	r.clones.clear()
}

func newGitObject(typ byte, data []byte) *gitObject {
	object := &gitObject{typ: typ, data: data}
	switch typ {
	case objCommit:
		object.commit = parseCommitHeader(data)
	case objTree:
		object.tree = parseTreeEntries(data)
	case objTag:
		object.tag = parseTagHeader(data)
	}
	return object
}

func (r *repository) object(oid Oid) *gitObject { return r.objects[oid] }

// peelToCommit follows tag objects until a commit (or a non-tag object) is
// reached. It answers the peeled oid and whether it is a commit.
func (r *repository) peelToCommit(oid Oid) (Oid, bool) {
	target, object := r.followTags(oid)
	return target, object != nil && object.typ == objCommit
}

// peelTarget follows tag objects to the first non-tag object.
func (r *repository) peelTarget(oid Oid) Oid {
	target, _ := r.followTags(oid)
	return target
}

// At the traversal limit the final oid is still reportable, but it has not
// been inspected and does not qualify as a peeled commit.
func (r *repository) followTags(oid Oid) (Oid, *gitObject) {
	for range 64 {
		object := r.objects[oid]
		if object == nil || object.typ != objTag || object.tag == nil {
			return oid, object
		}
		oid = object.tag.object
	}
	return oid, nil
}

// commitOf answers the parsed commit for an oid, following tags.
func (r *repository) commitOf(oid Oid) *parsedCommit {
	_, target := r.followTags(oid)
	if target != nil && target.typ == objCommit {
		return target.commit
	}
	return nil
}

// refNames returns all ref names in sorted order.
func (r *repository) refNames() []string {
	return sortedKeys(r.refs)
}

func sortedKeys[V any](values map[string]V) []string { return slices.Sorted(maps.Keys(values)) }

func selectInOrder[T any](source []T, newestFirst bool, accept func(T) bool) []T {
	var selected []T
	for i := range source {
		if newestFirst {
			i = len(source) - 1 - i
		}
		if value := source[i]; accept(value) {
			selected = append(selected, value)
		}
	}
	return selected
}

func (r *repository) unknownLabel(names []string) (string, bool) {
	for _, name := range names {
		if r.labels[name] == nil {
			return name, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The store

type store struct {
	mu sync.RWMutex

	repos     map[string]*repository // key: owner + "\x00" + name
	reposByID map[int]*repository
	byOwner   map[string][]*repository // creation order within each owner

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
		reposByID:   map[int]*repository{},
		byOwner:     map[string][]*repository{},
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

func branchRef(name string) string { return "refs/heads/" + name }

func (s *store) commitAsViewer(r *repository, tree Oid, parents []Oid, message string) (Oid, int64) {
	ts := s.nextTime()
	identity := s.viewerName + " <" + s.viewerEmail + ">"
	return r.buildCommitObject(tree, parents, message, identity, identity, ts), ts
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
	}
	s.repos[key] = r
	s.reposByID[r.id] = r
	s.byOwner[owner] = append(s.byOwner[owner], r)
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
	delete(s.reposByID, r.id)
	owned := s.byOwner[r.owner]
	if index := slices.Index(owned, r); index >= 0 {
		owned = slices.Delete(owned, index, index+1)
	}
	if len(owned) == 0 {
		delete(s.byOwner, r.owner)
	} else {
		s.byOwner[r.owner] = owned
	}
	return true
}

// reposOf exposes a read-only view of an owner's creation order under the lock.
func (s *store) reposOf(owner string) []*repository {
	return s.byOwner[owner]
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
