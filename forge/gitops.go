// Git object operations on the store: building commits and trees, walking
// ancestry, merge-base, three-way merges and diffs. The contents API, the
// merge endpoints and the fetch selection all share these.

package main

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Building commits and trees

// buildCommitObject serialises a commit and stores it, answering its id.
func (r *repository) buildCommitObject(tree Oid, parents []Oid, message, author, committer string, timestamp int64) Oid {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree.String())
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p.String())
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", author, timestamp)
	fmt.Fprintf(&b, "committer %s %d +0000\n", committer, timestamp)
	b.WriteString("\n")
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteString("\n")
	}
	return r.putObject(objCommit, []byte(b.String()))
}

// buildTreeObject serialises a tree from sorted entries and stores it.
func (r *repository) buildTreeObject(entries []treeEntry) Oid {
	sortTreeEntries(entries)
	var b []byte
	for _, e := range entries {
		b = append(b, []byte(e.mode+" "+e.name+"\x00")...)
		b = append(b, e.oid[:]...)
	}
	return r.putObject(objTree, b)
}

// sortTreeEntries orders entries the way git does: by name, with directories
// compared as if their name had a trailing slash.
func sortTreeEntries(entries []treeEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		an, bn := a.name, b.name
		if a.isDir() {
			an += "/"
		}
		if b.isDir() {
			bn += "/"
		}
		return an < bn
	})
}

// flatFile is one file in a flattened tree.
type flatFile struct {
	mode string
	oid  Oid
}

// flattenTree walks a tree recursively into path -> file. Missing trees yield
// an empty map.
func (r *repository) flattenTree(treeOid Oid) map[string]flatFile {
	out := map[string]flatFile{}
	var walk func(oid Oid, prefix string)
	walk = func(oid Oid, prefix string) {
		o := r.objects[oid]
		if o == nil || o.typ != objTree {
			return
		}
		for _, e := range o.tree {
			path := prefix + e.name
			if e.isDir() {
				walk(e.oid, path+"/")
			} else {
				out[path] = flatFile{mode: e.mode, oid: e.oid}
			}
		}
	}
	walk(treeOid, "")
	return out
}

// treeFromFlat builds the nested trees for a flat path map and answers the
// root tree id.
func (r *repository) treeFromFlat(flat map[string]flatFile) Oid {
	// Group paths by their first segment, recursively.
	var build func(paths map[string]flatFile, prefix string) Oid
	build = func(paths map[string]flatFile, prefix string) Oid {
		dirs := map[string]map[string]flatFile{}
		var entries []treeEntry
		for path, f := range paths {
			rel := strings.TrimPrefix(path, prefix)
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				dir := rel[:i]
				if dirs[dir] == nil {
					dirs[dir] = map[string]flatFile{}
				}
				dirs[dir][path] = f
			} else {
				entries = append(entries, treeEntry{mode: f.mode, name: rel, oid: f.oid})
			}
		}
		for dir, sub := range dirs {
			entries = append(entries, treeEntry{mode: "40000", name: dir, oid: build(sub, prefix+dir+"/")})
		}
		return r.buildTreeObject(entries)
	}
	return build(flat, "")
}

// treeAtPath reads the tree object at a directory path within a tree, or the
// zero id when the path is absent or not a directory.
func (r *repository) treeAtPath(treeOid Oid, dirPath string) Oid {
	if dirPath == "" {
		return treeOid
	}
	cur := treeOid
	for _, seg := range strings.Split(dirPath, "/") {
		if seg == "" {
			continue
		}
		o := r.objects[cur]
		if o == nil || o.typ != objTree {
			return ZeroOid
		}
		found := ZeroOid
		for _, e := range o.tree {
			if e.name == seg && e.isDir() {
				found = e.oid
				break
			}
		}
		if found.IsZero() {
			return ZeroOid
		}
		cur = found
	}
	return cur
}

// blobAtPath reads the blob id at a file path within a tree.
func (r *repository) blobAtPath(treeOid Oid, filePath string) (Oid, string, bool) {
	segments := strings.Split(filePath, "/")
	cur := treeOid
	for i, seg := range segments {
		o := r.objects[cur]
		if o == nil || o.typ != objTree {
			return ZeroOid, "", false
		}
		var found *treeEntry
		for j := range o.tree {
			if o.tree[j].name == seg {
				found = &o.tree[j]
				break
			}
		}
		if found == nil {
			return ZeroOid, "", false
		}
		if i == len(segments)-1 {
			if found.isDir() {
				return ZeroOid, "", false
			}
			return found.oid, found.mode, true
		}
		if !found.isDir() {
			return ZeroOid, "", false
		}
		cur = found.oid
	}
	return ZeroOid, "", false
}

// ---------------------------------------------------------------------------
// Ancestry

// isAncestor answers whether ancestor is reachable from descendant (or equal),
// following first and later parents alike.
func (r *repository) isAncestor(ancestor, descendant Oid) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[Oid]bool{}
	stack := []Oid{descendant}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		c := r.commitOf(cur)
		if c == nil {
			continue
		}
		for _, p := range c.parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// ancestorsOf returns the set of commits reachable from tip, including tip.
func (r *repository) ancestorsOf(tip Oid) map[Oid]bool {
	seen := map[Oid]bool{}
	stack := []Oid{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c := r.commitOf(cur); c != nil {
			stack = append(stack, c.parents...)
		}
	}
	return seen
}

// mergeBase finds a common ancestor of a and b: the first ancestor of a (in
// breadth-first order from a) that is also reachable from b. The zero id
// means the histories are unrelated.
func (r *repository) mergeBase(a, b Oid) Oid {
	if a.IsZero() || b.IsZero() {
		return ZeroOid
	}
	fromB := r.ancestorsOf(b)
	seen := map[Oid]bool{}
	queue := []Oid{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if fromB[cur] {
			return cur
		}
		if c := r.commitOf(cur); c != nil {
			queue = append(queue, c.parents...)
		}
	}
	return ZeroOid
}

// historyOf lists the commits reachable from tip, children before parents,
// each commit once, in breadth-first order.
func (r *repository) historyOf(tip Oid) []Oid {
	var out []Oid
	seen := map[Oid]bool{}
	queue := []Oid{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		if c := r.commitOf(cur); c != nil {
			queue = append(queue, c.parents...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Three-way merge and diff

// mergeTrees merges theirs into ours against base. It answers the merged flat
// map, or ok=false when the two sides changed the same path differently.
func (r *repository) mergeTrees(base, ours, theirs map[string]flatFile) (map[string]flatFile, bool) {
	merged := map[string]flatFile{}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	for p := range paths {
		b, inB := base[p]
		o, inO := ours[p]
		t, inT := theirs[p]
		switch {
		case inO && inT && o == t:
			merged[p] = o
		case inB && inO && inT && o == b && t != b:
			merged[p] = t
		case inB && inO && inT && t == b && o != b:
			merged[p] = o
		case !inB && inO && inT && o == t:
			merged[p] = o
		case !inB && inO && !inT:
			merged[p] = o
		case !inB && !inO && inT:
			merged[p] = t
		case inB && inO && !inT && o == b:
			// Deleted by theirs, unchanged by ours: deleted.
		case inB && !inO && inT && t == b:
			// Deleted by ours, unchanged by theirs: deleted.
		case inB && inO && !inT:
			return nil, false // deleted by theirs, modified by ours
		case inB && !inO && inT:
			return nil, false // deleted by ours, modified by theirs
		case inB && inO && inT:
			return nil, false // both modified differently
		case !inB && inO && inT:
			return nil, false // both added differently
		case inB && !inO && !inT:
			// Deleted by both.
		}
	}
	return merged, true
}

// mergeable answers whether merging headTip into baseTip would be clean.
func (r *repository) mergeable(baseTip, headTip Oid) bool {
	if baseTip.IsZero() || headTip.IsZero() {
		return true
	}
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
	_, ok := r.mergeTrees(r.flattenTree(baseTree), r.flattenTree(oursTree), r.flattenTree(theirsTree))
	return ok
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
	paths := map[string]bool{}
	for p := range oldFlat {
		paths[p] = true
	}
	for p := range newFlat {
		paths[p] = true
	}
	var names []string
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []fileChange
	for _, p := range names {
		o, inO := oldFlat[p]
		n, inN := newFlat[p]
		switch {
		case inO && inN && o.oid == n.oid:
			// Unchanged.
		case !inO && inN:
			out = append(out, fileChange{path: p, status: "added", additions: r.lineCount(n.oid), blobOid: n.oid})
		case inO && !inN:
			out = append(out, fileChange{path: p, status: "removed", deletions: r.lineCount(o.oid)})
		default:
			add, del := r.lineDiff(o.oid, n.oid)
			out = append(out, fileChange{path: p, status: "modified", additions: add, deletions: del, blobOid: n.oid})
		}
	}
	return out
}

// lineCount counts the lines of a blob: newlines plus one for a trailing
// partial line.
func (r *repository) lineCount(oid Oid) int {
	o := r.objects[oid]
	if o == nil {
		return 0
	}
	return countLines(o.data)
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff estimates added and removed lines between two blobs by stripping
// the common prefix and suffix lines.
func (r *repository) lineDiff(oldOid, newOid Oid) (add, del int) {
	var oldLines, newLines []string
	if o := r.objects[oldOid]; o != nil {
		oldLines = splitLines(o.data)
	}
	if o := r.objects[newOid]; o != nil {
		newLines = splitLines(o.data)
	}
	pre := 0
	for pre < len(oldLines) && pre < len(newLines) && oldLines[pre] == newLines[pre] {
		pre++
	}
	suf := 0
	for suf < len(oldLines)-pre && suf < len(newLines)-pre && oldLines[len(oldLines)-1-suf] == newLines[len(newLines)-1-suf] {
		suf++
	}
	return len(newLines) - pre - suf, len(oldLines) - pre - suf
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
