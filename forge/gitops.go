// Git object operations on the store: building commits and trees, walking
// ancestry, merge-base, three-way merges and diffs. The contents API, the
// merge endpoints and the fetch selection all share these.

package main

import (
	"bytes"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Building commits and trees

// buildCommitObject serialises a commit and stores it, answering its id.
func (r *repository) buildCommitObject(tree Oid, parents []Oid, message, author, committer string, timestamp int64) Oid {
	data := make([]byte, 0, 96+48*len(parents)+len(author)+len(committer)+len(message))
	data = appendOidHeader(data, "tree ", tree)
	for _, parent := range parents {
		data = appendOidHeader(data, "parent ", parent)
	}
	data = appendIdentityHeader(data, "author ", author, timestamp)
	data = appendIdentityHeader(data, "committer ", committer, timestamp)
	data = append(append(data, '\n'), message...)
	if !strings.HasSuffix(message, "\n") {
		data = append(data, '\n')
	}
	return r.putObject(objCommit, data)
}

func appendOidHeader(dst []byte, key string, oid Oid) []byte {
	return append(oid.appendHex(append(dst, key...)), '\n')
}

func appendIdentityHeader(dst []byte, key, identity string, timestamp int64) []byte {
	dst = append(dst, key...)
	dst = append(dst, identity...)
	dst = strconv.AppendInt(append(dst, ' '), timestamp, 10)
	return append(dst, " +0000\n"...)
}

// buildTreeObject serialises a tree from sorted entries and stores it.
func (r *repository) buildTreeObject(entries []treeEntry) Oid {
	slices.SortStableFunc(entries, compareTreeEntries)
	var size int
	for _, entry := range entries {
		size += len(entry.mode) + len(entry.name) + 2 + len(entry.oid)
	}
	data := make([]byte, 0, size)
	for _, entry := range entries {
		data = append(data, entry.mode...)
		data = append(data, ' ')
		data = append(data, entry.name...)
		data = append(data, 0)
		data = append(data, entry.oid[:]...)
	}
	return r.putObject(objTree, data)
}

// Compare the serialized name order, treating directory names as if they
// include a trailing slash. The slash is virtual to avoid an allocation.
func compareTreeEntries(a, b treeEntry) int {
	aLen, bLen := len(a.name), len(b.name)
	common := min(aLen, bLen)
	if order := strings.Compare(a.name[:common], b.name[:common]); order != 0 {
		return order
	}
	if aLen == bLen {
		switch {
		case a.isDir() && !b.isDir():
			return 1
		case b.isDir() && !a.isDir():
			return -1
		default:
			return 0
		}
	}
	if aLen > bLen {
		return -compareTreeEntries(b, a)
	}
	if !a.isDir() {
		return -1
	}
	if next := b.name[common]; next != '/' {
		return int('/') - int(next)
	}
	if bLen == aLen+1 && !b.isDir() {
		return 0
	}
	return -1
}

// flatFile is one file in a flattened tree.
type flatFile struct {
	mode string
	oid  Oid
}

// flattenTree walks a tree recursively into path -> file. Missing trees yield
// an empty map.
func (r *repository) flattenTree(treeOid Oid) map[string]flatFile {
	files := make(map[string]flatFile)
	r.collectFiles(files, treeOid, "")
	return files
}

func (r *repository) collectFiles(files map[string]flatFile, tree Oid, prefix string) {
	for _, entry := range r.treeEntries(tree) {
		path := prefix + entry.name
		if entry.isDir() {
			r.collectFiles(files, entry.oid, path+"/")
		} else {
			files[path] = flatFile{mode: entry.mode, oid: entry.oid}
		}
	}
}

func (r *repository) treeEntries(oid Oid) []treeEntry {
	if tree := r.objects[oid]; tree != nil && tree.typ == objTree {
		return tree.tree
	}
	return nil
}

// treeFromFlat builds the nested trees for a flat path map and answers the
// root tree id.
func (r *repository) treeFromFlat(flat map[string]flatFile) Oid {
	var root treeBuilder
	for path, file := range flat {
		root.add(path, file)
	}
	return root.store(r)
}

// Insert each path once and write children before parents. Files and subtrees
// remain separate here so the same names are accepted as in the flat map.
type treeBuilder struct {
	files []treeEntry
	dirs  map[string]*treeBuilder
}

func (b *treeBuilder) add(path string, file flatFile) {
	for {
		name, rest, nested := strings.Cut(path, "/")
		if !nested {
			b.files = append(b.files, treeEntry{name: name, mode: file.mode, oid: file.oid})
			return
		}
		if b.dirs == nil {
			b.dirs = make(map[string]*treeBuilder)
		}
		child := b.dirs[name]
		if child == nil {
			child = &treeBuilder{}
			b.dirs[name] = child
		}
		b, path = child, rest
	}
}

func (b *treeBuilder) store(r *repository) Oid {
	entries := slices.Grow(b.files, len(b.dirs))
	for name, dir := range b.dirs {
		entries = append(entries, treeEntry{name: name, mode: "40000", oid: dir.store(r)})
	}
	return r.buildTreeObject(entries)
}

// treeAtPath reads the tree object at a directory path within a tree, or the
// zero id when the path is absent or not a directory.
func (r *repository) treeAtPath(treeOid Oid, dirPath string) Oid {
	for segment := range strings.SplitSeq(dirPath, "/") {
		if segment == "" {
			continue
		}
		entry := r.findTreeEntry(treeOid, segment, true)
		if entry == nil || entry.oid.IsZero() {
			return ZeroOid
		}
		treeOid = entry.oid
	}
	return treeOid
}

// blobAtPath reads the blob id at a file path within a tree.
func (r *repository) blobAtPath(treeOid Oid, filePath string) (Oid, string, bool) {
	for {
		name, rest, nested := strings.Cut(filePath, "/")
		entry := r.findTreeEntry(treeOid, name, false)
		if entry == nil || entry.isDir() != nested {
			return ZeroOid, "", false
		}
		if !nested {
			return entry.oid, entry.mode, true
		}
		treeOid, filePath = entry.oid, rest
	}
}

func (r *repository) findTreeEntry(oid Oid, name string, directoriesOnly bool) *treeEntry {
	entries := r.treeEntries(oid)
	for i := range entries {
		entry := &entries[i]
		if entry.name == name && (!directoriesOnly || entry.isDir()) {
			return entry
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ancestry

// isAncestor answers whether ancestor is reachable from descendant (or equal),
// following first and later parents alike.
func (r *repository) isAncestor(ancestor, descendant Oid) bool {
	for oid := range r.commitHistory(descendant) {
		if oid == ancestor {
			return true
		}
	}
	return false
}

// ancestorsOf returns the set of commits reachable from tip, including tip.
func (r *repository) ancestorsOf(tip Oid) fetchObjectSet {
	ancestors := make(fetchObjectSet)
	for oid := range r.commitHistory(tip) {
		ancestors.add(oid)
	}
	return ancestors
}

// mergeBase finds a common ancestor of a and b: the first ancestor of a (in
// breadth-first order from a) that is also reachable from b. The zero id
// means the histories are unrelated.
func (r *repository) mergeBase(a, b Oid) Oid {
	if a.IsZero() || b.IsZero() {
		return ZeroOid
	}
	fromB := r.ancestorsOf(b)
	for oid := range r.commitHistory(a) {
		if fromB.has(oid) {
			return oid
		}
	}
	return ZeroOid
}

// historyOf lists the commits reachable from tip, children before parents,
// each commit once, in breadth-first order.
func (r *repository) historyOf(tip Oid) []Oid {
	return slices.Collect(r.commitHistory(tip))
}

func (r *repository) commitHistory(tip Oid) func(func(Oid) bool) {
	return func(yield func(Oid) bool) { r.walkCommitHistory(tip, yield) }
}

func (r *repository) walkCommitHistory(tip Oid, yield func(Oid) bool) {
	seen := fetchObjectSet{tip: {}}
	queue := []Oid{tip}
	for next := 0; next < len(queue); next++ {
		oid := queue[next]
		if !yield(oid) {
			return
		}
		if commit := r.commitOf(oid); commit != nil {
			for _, parent := range commit.parents {
				if seen.add(parent) {
					queue = append(queue, parent)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Three-way merge and diff

// mergeTrees merges theirs into ours against base. It answers the merged flat
// map, or ok=false when the two sides changed the same path differently.
func (r *repository) mergeTrees(base, ours, theirs map[string]flatFile) (map[string]flatFile, bool) {
	merged := make(map[string]flatFile, len(ours))
	for path := range filePaths(ours, theirs) {
		oursAtPath := fileVersionAt(ours, path)
		theirsAtPath := fileVersionAt(theirs, path)
		chosen, ok := mergeFileVersions(fileVersionAt(base, path), oursAtPath, theirsAtPath)
		if !ok {
			return nil, false
		}
		if chosen.present {
			merged[path] = chosen.file
		}
	}
	return merged, true
}

type fileVersion struct {
	file    flatFile
	present bool
}

func fileVersionAt(files map[string]flatFile, path string) fileVersion {
	file, exists := files[path]
	return fileVersion{file: file, present: exists}
}

func mergeFileVersions(base, ours, theirs fileVersion) (fileVersion, bool) {
	switch {
	case ours == theirs, theirs == base:
		return ours, true
	case ours == base:
		return theirs, true
	default:
		return fileVersion{}, false
	}
}

func filePaths(a, b map[string]flatFile) map[string]struct{} {
	paths := make(map[string]struct{}, max(len(a), len(b)))
	for name := range maps.Keys(a) {
		paths[name] = struct{}{}
	}
	for name := range maps.Keys(b) {
		paths[name] = struct{}{}
	}
	return paths
}

// mergeable answers whether merging headTip into baseTip would be clean.
func (r *repository) mergeable(baseTip, headTip Oid) bool {
	if baseTip.IsZero() || headTip.IsZero() {
		return true
	}
	_, ok := r.mergeCommits(baseTip, headTip)
	return ok
}

func (r *repository) mergeCommits(ours, theirs Oid) (map[string]flatFile, bool) {
	base := r.mergeBase(ours, theirs)
	return r.mergeTrees(r.flattenTree(r.commitTree(base)), r.flattenTree(r.commitTree(ours)), r.flattenTree(r.commitTree(theirs)))
}

func (r *repository) commitTree(oid Oid) Oid {
	if commit := r.commitOf(oid); commit != nil {
		return commit.tree
	}
	return ZeroOid
}

// fileChange is one path's change between two trees.
type fileChange struct {
	path      string
	status    string // "added", "removed", "modified"
	additions int
	deletions int
	blobOid   Oid // the new blob, when there is one
}

// diffTrees compares the old tree with the new tree and lists every changed
// path, sorted by path. Line counts come from a prefix/suffix comparison, so
// an unchanged file never counts as changed.
func (r *repository) diffTrees(oldTree, newTree Oid) []fileChange {
	oldFlat := r.flattenTree(oldTree)
	newFlat := r.flattenTree(newTree)
	var changes []fileChange
	for _, path := range sortedKeys(filePaths(oldFlat, newFlat)) {
		old, current := fileVersionAt(oldFlat, path), fileVersionAt(newFlat, path)
		change := fileChange{path: path, blobOid: current.file.oid}
		switch {
		case !old.present:
			change.status = "added"
			change.additions = countLines(r.objectData(change.blobOid))
		case !current.present:
			change.status = "removed"
			change.deletions = countLines(r.objectData(old.file.oid))
		case old.file.oid == current.file.oid:
			continue
		default:
			change.status = "modified"
			change.additions, change.deletions = lineDiff(r.objectData(old.file.oid), r.objectData(change.blobOid))
		}
		changes = append(changes, change)
	}
	return changes
}

// lineCount counts the lines of a blob: newlines plus one for a trailing
// partial line.
func (r *repository) objectData(oid Oid) []byte {
	if object := r.objects[oid]; object != nil {
		return object.data
	}
	return nil
}

func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff estimates added and removed lines between two blobs by stripping
// the common prefix and suffix lines.
func lineDiff(before, after []byte) (add, del int) {
	for len(before) != 0 && len(after) != 0 {
		old, oldTail, _ := bytes.Cut(before, []byte{'\n'})
		current, currentTail, _ := bytes.Cut(after, []byte{'\n'})
		if !bytes.Equal(old, current) {
			break
		}
		before, after = oldTail, currentTail
	}
	for len(before) != 0 && len(after) != 0 {
		old, oldHead := lastLine(before)
		current, currentHead := lastLine(after)
		if !bytes.Equal(old, current) {
			break
		}
		before, after = oldHead, currentHead
	}
	return countLines(after), countLines(before)
}

func lastLine(data []byte) (line, rest []byte) {
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	start := bytes.LastIndexByte(data[:end], '\n') + 1
	return data[start:end], data[:start]
}
