// Git smart HTTP: protocol v2 (upload-pack) with a v0 fallback, and v0
// receive-pack. pkt-line framing, advertisements, ls-refs, fetch with shallow
// and deepen, and receive-pack with fast-forward-only ref updates and
// report-status over side-band-64k.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line framing

const (
	pktFlush = "0000"
	pktDelim = "0001"
	pktEnd   = "0002"
)

// pktLine frames one payload (which should end with "\n" for text lines).
func pktLine(payload string) []byte {
	n := len(payload) + 4
	return []byte(fmt.Sprintf("%04x%s", n, payload))
}

func pktFlushBytes() []byte { return []byte(pktFlush) }
func pktDelimBytes() []byte { return []byte(pktDelim) }

// pktPacket is one decoded packet: a flush, a delim, a response-end, or data.
type pktPacket struct {
	kind    byte // 'f' flush, 'd' delim, 'e' end, 'D' data
	payload []byte
}

// decodePktLines parses a pkt-line stream. Trailing bytes that do not form a
// complete packet stop the parse; the caller decides whether that is an error.
func decodePktLines(b []byte) ([]pktPacket, int, error) {
	var out []pktPacket
	off := 0
	for off+4 <= len(b) {
		hdr := string(b[off : off+4])
		if hdr == pktFlush {
			out = append(out, pktPacket{kind: 'f'})
			off += 4
			continue
		}
		if hdr == pktDelim {
			out = append(out, pktPacket{kind: 'd'})
			off += 4
			continue
		}
		if hdr == pktEnd {
			out = append(out, pktPacket{kind: 'e'})
			off += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 {
			return out, off, fmt.Errorf("bad pkt-line length %q", hdr)
		}
		if off+int(n) > len(b) {
			return out, off, fmt.Errorf("pkt-line of %d bytes overruns the stream", n)
		}
		out = append(out, pktPacket{kind: 'D', payload: b[off+4 : off+int(n)]})
		off += int(n)
	}
	return out, off, nil
}

// decodePktLinesAll parses a whole stream; any leftover bytes are an error.
func decodePktLinesAll(b []byte) ([]pktPacket, error) {
	packets, off, err := decodePktLines(b)
	if err != nil {
		return nil, err
	}
	if off != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after the pkt-line stream", len(b)-off)
	}
	return packets, nil
}

// pktLinesText joins the data packets' payloads as text lines.
func pktLinesText(packets []pktPacket) []string {
	var out []string
	for _, p := range packets {
		if p.kind == 'D' {
			out = append(out, string(p.payload))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Sideband framing

// sidebandWrap splits data into sideband packets on the given band, each
// packet at most 65520 bytes total.
func sidebandWrap(band byte, data []byte) []byte {
	var out []byte
	const maxData = 65520 - 4 - 1
	for len(data) > 0 {
		n := len(data)
		if n > maxData {
			n = maxData
		}
		chunk := data[:n]
		data = data[n:]
		pkt := make([]byte, 0, n+5)
		pkt = append(pkt, []byte(fmt.Sprintf("%04x", n+5))...)
		pkt = append(pkt, band)
		pkt = append(pkt, chunk...)
		out = append(out, pkt...)
	}
	if len(out) == 0 {
		// Even an empty payload gets one sideband packet.
		out = append(out, []byte("0005")...)
		out = append(out, band)
	}
	return out
}

// ---------------------------------------------------------------------------
// Advertisements

const agentCap = "agent=forge/1.0"
const headRef = "refs/heads/main"

const uploadPackCapsV0 = "side-band-64k shallow thin-pack ofs-delta multi_ack symref=HEAD:refs/heads/main " + agentCap
const receivePackCapsV0 = "report-status side-band-64k delete-refs ofs-delta " + agentCap

// advertiseV0 writes a v0 ref advertisement for a service.
func advertiseV0(r *repository, service string, caps string) []byte {
	var out []byte
	out = append(out, pktLine("# service="+service+"\n")...)
	out = append(out, pktFlushBytes()...)
	names := r.refNames()
	if len(names) == 0 {
		out = append(out, pktLine(ZeroOid.String()+" capabilities^{}\x00"+caps+"\n")...)
		return out
	}
	headOid := r.refs[headRef]
	first := true
	emit := func(oid Oid, name string) {
		line := oid.String() + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		out = append(out, pktLine(line+"\n")...)
	}
	emit(headOid, "HEAD")
	for _, name := range names {
		emit(r.refs[name], name)
	}
	// Peeled lines for annotated tags.
	for _, name := range names {
		oid := r.refs[name]
		if o := r.objects[oid]; o != nil && o.typ == objTag {
			peeled := r.peelTarget(oid)
			out = append(out, pktLine(peeled.String()+" "+name+"^{}\n")...)
		}
	}
	return out
}

// advertiseV2 writes the protocol v2 capability advertisement.
func advertiseV2(service string) []byte {
	var out []byte
	out = append(out, pktLine("# service="+service+"\n")...)
	out = append(out, pktFlushBytes()...)
	out = append(out, pktLine("version 2\n")...)
	out = append(out, pktLine("fetch=shallow\n")...)
	out = append(out, pktLine("ls-refs=unborn\n")...)
	out = append(out, pktLine("object-format=sha1\n")...)
	out = append(out, pktLine(agentCap+"\n")...)
	out = append(out, pktFlushBytes()...)
	return out
}

// ---------------------------------------------------------------------------
// ls-refs (protocol v2)

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func (a *lsRefsArgs) matches(name string) bool {
	if len(a.prefixes) == 0 {
		return true
	}
	for _, p := range a.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// handleLsRefs answers a protocol v2 ls-refs command.
func handleLsRefs(r *repository, args *lsRefsArgs) []byte {
	var out []byte
	names := r.refNames()
	if len(names) == 0 {
		if args.unborn {
			out = append(out, pktLine("unborn HEAD symref-target:"+headRef+"\n")...)
		}
		out = append(out, pktFlushBytes()...)
		return out
	}
	if args.symrefs {
		if headOid, ok := r.refs[headRef]; ok && args.matches("HEAD") {
			out = append(out, pktLine(headOid.String()+" HEAD symref-target:"+headRef+"\n")...)
		}
	}
	for _, name := range names {
		if !args.matches(name) {
			continue
		}
		oid := r.refs[name]
		line := oid.String() + " " + name
		if args.peel {
			if o := r.objects[oid]; o != nil && o.typ == objTag {
				line += " peeled:" + r.peelTarget(oid).String()
			}
		}
		out = append(out, pktLine(line+"\n")...)
	}
	out = append(out, pktFlushBytes()...)
	return out
}

// ---------------------------------------------------------------------------
// Fetch object selection

type fetchRequest struct {
	wants   []Oid
	haves   []Oid
	shallow []Oid // the client's existing shallow boundaries
	deepen  int   // 0 means no depth limit
}

// fetchSelection computes the objects to send, the new shallow boundaries and
// the boundaries that cease to be shallow.
//
//   - held is the closure of the haves, stopping at the client's shallow
//     boundaries: their parents are not held.
//   - the send walk starts at the wants and stops at held commits, except that
//     it walks through shallow boundaries (their parents are not held).
//   - with a depth limit D, commits at distance < D are sent with their full
//     tree closure; commits at distance D-1 that have parents become shallow
//     boundaries.
func (r *repository) fetchSelection(req *fetchRequest) (send map[Oid]bool, shallowOut, unshallow []Oid) {
	shallowSet := map[Oid]bool{}
	for _, s := range req.shallow {
		shallowSet[s] = true
	}
	// The held set: walk from the haves, not crossing shallow boundaries.
	held := map[Oid]bool{}
	{
		stack := append([]Oid(nil), req.haves...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if held[cur] {
				continue
			}
			held[cur] = true
			if shallowSet[cur] {
				continue
			}
			if c := r.commitOf(cur); c != nil {
				stack = append(stack, c.parents...)
			}
		}
	}
	// The held objects: every held commit plus its tree and blob closure.
	// A shared blob or subtree must never be re-sent to a client that holds
	// it through an older tip.
	heldObjects := map[Oid]bool{}
	for oid := range held {
		heldObjects[oid] = true
		if c := r.commitOf(oid); c != nil {
			r.collectTreeObjects(c.tree, heldObjects)
		}
	}

	send = map[Oid]bool{}
	shallowMap := map[Oid]bool{}
	type item struct {
		oid   Oid
		depth int
	}
	visited := map[Oid]bool{}
	queue := []item{}
	for _, w := range req.wants {
		queue = append(queue, item{oid: w, depth: 0})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.oid] {
			continue
		}
		visited[cur.oid] = true
		o := r.objects[cur.oid]
		if o == nil {
			continue
		}
		// A tag want contributes the tag object and walks its target.
		if o.typ == objTag {
			if !held[cur.oid] {
				send[cur.oid] = true
			}
			target := r.peelTarget(cur.oid)
			if target != cur.oid {
				queue = append(queue, item{oid: target, depth: cur.depth})
			}
			continue
		}
		if o.typ != objCommit {
			// A tree or blob want: send it (and a tree's closure).
			if !held[cur.oid] {
				send[cur.oid] = true
				if o.typ == objTree {
					r.collectTreeObjects(cur.oid, send)
				}
			}
			continue
		}
		if held[cur.oid] && !shallowSet[cur.oid] {
			continue
		}
		if req.deepen > 0 && cur.depth >= req.deepen {
			continue
		}
		c := o.commit
		if c == nil {
			continue
		}
		if !held[cur.oid] {
			send[cur.oid] = true
			r.collectTreeObjects(c.tree, send)
		}
		if req.deepen > 0 && cur.depth == req.deepen-1 && len(c.parents) > 0 {
			shallowMap[cur.oid] = true
		}
		for _, p := range c.parents {
			queue = append(queue, item{oid: p, depth: cur.depth + 1})
		}
	}

	// Nothing the client already holds goes out, even when a new commit's
	// tree shares objects with an older tip's tree.
	for oid := range heldObjects {
		delete(send, oid)
	}

	// Unshallow: an old boundary whose parents are all in send or held.
	for _, s := range req.shallow {
		c := r.commitOf(s)
		if c == nil {
			continue
		}
		all := true
		for _, p := range c.parents {
			if !send[p] && !held[p] {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, s)
		}
	}
	for oid := range shallowMap {
		shallowOut = append(shallowOut, oid)
	}
	sort.Slice(shallowOut, func(i, j int) bool { return bytes.Compare(shallowOut[i][:], shallowOut[j][:]) < 0 })
	sort.Slice(unshallow, func(i, j int) bool { return bytes.Compare(unshallow[i][:], unshallow[j][:]) < 0 })
	return send, shallowOut, unshallow
}

// collectTreeObjects adds a tree and everything under it to the set.
func (r *repository) collectTreeObjects(treeOid Oid, set map[Oid]bool) {
	o := r.objects[treeOid]
	if o == nil || o.typ != objTree || set[treeOid] {
		return
	}
	set[treeOid] = true
	for _, e := range o.tree {
		if e.isDir() {
			r.collectTreeObjects(e.oid, set)
		} else {
			set[e.oid] = true
		}
	}
}

// ---------------------------------------------------------------------------
// Fetch responses

// fetchResponseV2 builds the protocol v2 fetch answer.
func (r *repository) fetchResponseV2(req *fetchRequest) []byte {
	send, shallowOut, unshallow := r.fetchSelection(req)
	var out []byte
	out = append(out, pktLine("acknowledgments\n")...)
	out = append(out, pktLine("NAK\n")...)
	out = append(out, pktFlushBytes()...)
	if len(shallowOut) > 0 || len(unshallow) > 0 {
		out = append(out, pktLine("shallow-info\n")...)
		for _, s := range shallowOut {
			out = append(out, pktLine("shallow "+s.String()+"\n")...)
		}
		for _, s := range unshallow {
			out = append(out, pktLine("unshallow "+s.String()+"\n")...)
		}
		out = append(out, pktFlushBytes()...)
	}
	out = append(out, pktLine("packfile\n")...)
	pack := writePack(r.objectsSubset(send))
	out = append(out, sidebandWrap(1, pack)...)
	out = append(out, pktFlushBytes()...)
	return out
}

// fetchResponseV0 builds the protocol v0 fetch answer.
func (r *repository) fetchResponseV0(req *fetchRequest) []byte {
	send, shallowOut, unshallow := r.fetchSelection(req)
	var out []byte
	for _, s := range shallowOut {
		out = append(out, pktLine("shallow "+s.String()+"\n")...)
	}
	for _, s := range unshallow {
		out = append(out, pktLine("unshallow "+s.String()+"\n")...)
	}
	out = append(out, pktLine("NAK\n")...)
	pack := writePack(r.objectsSubset(send))
	out = append(out, sidebandWrap(1, pack)...)
	out = append(out, pktFlushBytes()...)
	return out
}

func (r *repository) objectsSubset(set map[Oid]bool) map[Oid]*gitObject {
	out := make(map[Oid]*gitObject, len(set))
	for oid := range set {
		if o := r.objects[oid]; o != nil {
			out[oid] = o
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Request parsing for upload-pack POSTs

// parseFetchArgs reads want/have/shallow/deepen lines from pkt-line payloads.
func parseFetchArgs(lines []string, req *fetchRequest) {
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(line[5:])
			if len(fields) > 0 {
				if oid, ok := parseOid(fields[0]); ok {
					req.wants = append(req.wants, oid)
				}
			}
		case strings.HasPrefix(line, "have "):
			if oid, ok := parseOid(strings.TrimSpace(line[5:])); ok {
				req.haves = append(req.haves, oid)
			}
		case strings.HasPrefix(line, "shallow "):
			if oid, ok := parseOid(strings.TrimSpace(line[8:])); ok {
				req.shallow = append(req.shallow, oid)
			}
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil && n > 0 {
				req.deepen = n
			}
		}
	}
}

// parseV2Command splits a protocol v2 request into its command and argument
// lines (everything after the delim, or everything after the command when the
// client sent no delim).
func parseV2Command(packets []pktPacket) (command string, args []string) {
	var lines []string
	seenDelim := false
	for _, p := range packets {
		switch p.kind {
		case 'd':
			seenDelim = true
		case 'D':
			text := strings.TrimRight(string(p.payload), "\n")
			if command == "" && strings.HasPrefix(text, "command=") {
				command = text[len("command="):]
			} else if seenDelim {
				lines = append(lines, text)
			} else if command != "" {
				// Lines before the delim that are not the command: agent and
				// object-format declarations, ignored here.
			}
		}
	}
	return command, lines
}

// handleUploadPackPOST answers POST git-upload-pack: a v2 command request or
// a v0 want/have exchange.
func handleUploadPackPOST(r *repository, body []byte) ([]byte, string, int) {
	packets, err := decodePktLinesAll(body)
	if err != nil {
		return []byte("bad pkt-line stream"), "text/plain", 400
	}
	if len(packets) > 0 && packets[0].kind == 'D' && strings.HasPrefix(string(packets[0].payload), "command=") {
		command, args := parseV2Command(packets)
		switch command {
		case "ls-refs":
			lsArgs := &lsRefsArgs{}
			for _, a := range args {
				switch {
				case a == "peel":
					lsArgs.peel = true
				case a == "symrefs":
					lsArgs.symrefs = true
				case a == "unborn":
					lsArgs.unborn = true
				case strings.HasPrefix(a, "ref-prefix "):
					lsArgs.prefixes = append(lsArgs.prefixes, a[len("ref-prefix "):])
				}
			}
			return handleLsRefs(r, lsArgs), "application/x-git-upload-pack-result", 200
		case "fetch":
			req := &fetchRequest{}
			parseFetchArgs(args, req)
			return r.fetchResponseV2(req), "application/x-git-upload-pack-result", 200
		default:
			return []byte("unknown command"), "text/plain", 400
		}
	}
	// Protocol v0: wants + shallow/deepen, flush, haves + done, flush.
	req := &fetchRequest{}
	var lines []string
	for _, p := range packets {
		if p.kind == 'D' {
			lines = append(lines, string(p.payload))
		}
	}
	parseFetchArgs(lines, req)
	return r.fetchResponseV0(req), "application/x-git-upload-pack-result", 200
}

// ---------------------------------------------------------------------------
// Receive-pack

type pushCommand struct {
	oldOid Oid
	newOid Oid
	ref    string
}

// handleReceivePackPOST applies a push: commands, then a pack. It answers the
// report-status stream (sideband band 1 when the client asked for it).
func handleReceivePackPOST(r *repository, body []byte) ([]byte, string, int) {
	// Parse the command list pkt by pkt, stopping at the flush: everything
	// after it is the pack, which is not pkt-line framed.
	var commands []pushCommand
	sideband := false
	sawFlush := false
	off := 0
	for off+4 <= len(body) {
		hdr := string(body[off : off+4])
		if hdr == pktFlush {
			off += 4
			sawFlush = true
			break
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || off+int(n) > len(body) {
			return []byte("bad pkt-line stream"), "text/plain", 400
		}
		raw := strings.TrimRight(string(body[off+4:off+int(n)]), "\n")
		off += int(n)
		line := raw
		if i := strings.IndexByte(raw, 0); i >= 0 {
			line = raw[:i]
			caps := raw[i+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return []byte("bad command line"), "text/plain", 400
		}
		oldOid, ok1 := parseOid(fields[0])
		newOid, ok2 := parseOid(fields[1])
		if !ok1 || !ok2 || !strings.HasPrefix(fields[2], "refs/") {
			return []byte("bad command line"), "text/plain", 400
		}
		commands = append(commands, pushCommand{oldOid: oldOid, newOid: newOid, ref: fields[2]})
	}
	if !sawFlush || len(commands) == 0 {
		return []byte("no command list"), "text/plain", 400
	}
	packBytes := body[off:]

	// Unpack first: objects land in the store even when a ref update is
	// refused.
	unpackOk := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objects, err := parsePack(packBytes, func(oid Oid) *gitObject { return r.objects[oid] })
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else {
			for _, o := range objects {
				r.putObject(o.typ, o.data)
			}
		}
	}

	// Apply the ref updates in order.
	type outcome struct {
		ref    string
		ok     bool
		reason string
	}
	outcomes := make([]outcome, 0, len(commands))
	for _, cmd := range commands {
		if !unpackOk {
			outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "unpack failed"})
			continue
		}
		current, exists := r.refs[cmd.ref]
		switch {
		case cmd.newOid.IsZero():
			// Delete.
			if !exists {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if !cmd.oldOid.IsZero() && current != cmd.oldOid {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(r.refs, cmd.ref)
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: true})
			}
		case cmd.oldOid.IsZero():
			// Create.
			if exists {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "already exists"})
			} else if r.objects[cmd.newOid] == nil {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "object not found"})
			} else {
				r.refs[cmd.ref] = cmd.newOid
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: true})
			}
		default:
			// Update: the ref must be at old, and new must descend from old.
			if !exists {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if current != cmd.oldOid {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else if r.objects[cmd.newOid] == nil {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "object not found"})
			} else if !r.fastForward(cmd.oldOid, cmd.newOid) {
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				r.refs[cmd.ref] = cmd.newOid
				outcomes = append(outcomes, outcome{ref: cmd.ref, ok: true})
			}
		}
	}

	// The report-status stream, pkt-lined inside sideband band 1.
	var inner []byte
	if unpackOk {
		inner = append(inner, pktLine("unpack ok\n")...)
	} else {
		inner = append(inner, pktLine("unpack "+unpackDetail+"\n")...)
	}
	for _, o := range outcomes {
		if o.ok {
			inner = append(inner, pktLine("ok "+o.ref+"\n")...)
		} else {
			inner = append(inner, pktLine("ng "+o.ref+" "+o.reason+"\n")...)
		}
	}
	inner = append(inner, pktFlushBytes()...)
	var out []byte
	if sideband {
		out = append(out, sidebandWrap(1, inner)...)
	} else {
		out = append(out, inner...)
	}
	out = append(out, pktFlushBytes()...)
	return out, "application/x-git-receive-pack-result", 200
}

// fastForward answers whether newObj descends from oldObj (or is equal). Tag
// refs compare the peeled commits.
func (r *repository) fastForward(oldObj, newObj Oid) bool {
	oldCommit, okOld := r.peelToCommit(oldObj)
	newCommit, okNew := r.peelToCommit(newObj)
	if !okOld || !okNew {
		return oldObj == newObj
	}
	return r.isAncestor(oldCommit, newCommit)
}
