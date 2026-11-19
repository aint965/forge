// Git smart HTTP: protocol v2 (upload-pack) with a v0 fallback, and v0
// receive-pack. pkt-line framing, advertisements, ls-refs, fetch with shallow
// and deepen, and receive-pack with fast-forward-only ref updates and
// report-status over side-band-64k.

package main

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type gitRoute uint8

const (
	gitNoRoute gitRoute = iota
	gitInfoRefs
	gitUpload
	gitReceive
)

const (
	uploadService  = "git-upload-pack"
	receiveService = "git-receive-pack"
)

func matchGitRoute(segments []string) (repoRoute, gitRoute) {
	if len(segments) < 3 {
		return repoRoute{}, gitNoRoute
	}
	repo, eligible := strings.CutSuffix(segments[1], ".git")
	if !eligible {
		return repoRoute{}, gitNoRoute
	}
	key := repoRoute{owner: segments[0], name: repo}
	switch {
	case len(segments) == 4 && segments[2] == "info" && segments[3] == "refs":
		return key, gitInfoRefs
	case len(segments) == 3 && segments[2] == uploadService:
		return key, gitUpload
	case len(segments) == 3 && segments[2] == receiveService:
		return key, gitReceive
	}
	return repoRoute{}, gitNoRoute
}

func (s *server) routeGit(q *request) (response, bool) {
	key, route := matchGitRoute(q.segments())
	if route == gitNoRoute {
		return response{}, false
	}
	if !s.authorized(q) {
		return jsonError(401, "Requires authentication"), true
	}
	if route == gitInfoRefs {
		if q.method != "GET" {
			return restNotFound(), true
		}
		service := q.queryValue("service")
		v2 := q.headers.values[protocolHeader] == "version=2"
		return s.withRepo(key, false, func(r *repository) response {
			return r.gitAdvertisement(service, v2)
		}), true
	}
	if q.method != "POST" {
		return restNotFound(), true
	}
	return s.withRepo(key, route == gitReceive, func(r *repository) response {
		serve := handleUploadPackPOST
		if route == gitReceive {
			serve = handleReceivePackPOST
		}
		body, contentType, status := serve(r, q.body)
		return response{status: status, contentType: contentType, body: body}
	}), true
}

var uploadAdvertisementV2 = advertiseV2(uploadService)

func (r *repository) gitAdvertisement(service string, v2 bool) response {
	var body []byte
	switch service {
	case uploadService:
		if v2 {
			body = uploadAdvertisementV2
		} else {
			body = advertiseV0(r, service, uploadPackCapsV0)
		}
	case receiveService:
		body = advertiseV0(r, service, receivePackCapsV0)
	default:
		return jsonError(400, "unknown service")
	}
	return response{status: 200, contentType: "application/x-" + service + "-advertisement", body: body}
}

// ---------------------------------------------------------------------------
// pkt-line framing

const (
	pktFlush = "0000"
	pktDelim = "0001"
	pktEnd   = "0002"
)

// appendPktLen appends the 4-hex-digit pkt-line length prefix for a payload
// of n bytes (length counts the 4 prefix bytes themselves).
func appendPktLen(out []byte, n int) []byte {
	const hexdigits = "0123456789abcdef"
	total := n + 4
	if total > 0xffff {
		// Unreachable for the short text lines pktLine frames; keep the
		// old formatting for absurd lengths rather than truncating.
		return append(out, []byte(fmt.Sprintf("%04x", total))...)
	}
	return append(out,
		hexdigits[(total>>12)&0xf],
		hexdigits[(total>>8)&0xf],
		hexdigits[(total>>4)&0xf],
		hexdigits[total&0xf])
}

func appendPktText(dst []byte, payload string) []byte {
	return append(appendPktLen(dst, len(payload)), payload...)
}

// parsePktLen parses a 4-byte hex pkt-line length without allocating. A
// leading sign falls back to ParseInt to preserve its exact acceptance.
func parsePktLen(hdr []byte) (int, bool) {
	if len(hdr) != 4 {
		return 0, false
	}
	if hdr[0] == '+' || hdr[0] == '-' {
		n, err := strconv.ParseInt(string(hdr), 16, 32)
		return int(n), err == nil
	}
	n := 0
	for _, c := range hdr {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}

// pktPacket is one decoded packet: a flush, a delim, a response-end, or data.
type pktPacket struct {
	kind    byte // 'f' flush, 'd' delim, 'e' end, 'D' data
	payload []byte
}

// decodePktLines parses a pkt-line stream. Trailing bytes that do not form a
// complete packet stop the parse; the caller decides whether that is an error.
func decodePktLines(b []byte) ([]pktPacket, int, error) {
	var out []pktPacket
	offset := 0
	for len(b)-offset >= 4 {
		packet, consumed, err := readPacket(b[offset:])
		if err != nil {
			return out, offset, err
		}
		out = append(out, packet)
		offset += consumed
	}
	return out, offset, nil
}

// readPacket requires a complete four-byte header. Keeping the boundary
// calculation here also lets receive-pack switch to raw pack bytes at flush.
func readPacket(input []byte) (pktPacket, int, error) {
	header := input[:4]
	switch string(header) {
	case pktFlush:
		return pktPacket{kind: 'f'}, 4, nil
	case pktDelim:
		return pktPacket{kind: 'd'}, 4, nil
	case pktEnd:
		return pktPacket{kind: 'e'}, 4, nil
	}
	length, valid := parsePktLen(header)
	if !valid || length < 4 {
		return pktPacket{}, 0, fmt.Errorf("bad pkt-line length %q", string(header))
	}
	if length > len(input) {
		return pktPacket{}, 0, fmt.Errorf("pkt-line of %d bytes overruns the stream", length)
	}
	return pktPacket{kind: 'D', payload: input[4:length]}, length, nil
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

// appendSideband frames at most 65515 payload bytes in each packet.
func appendSideband(dst []byte, band byte, data []byte) []byte {
	const maxData = 65515
	frames := max(1, (len(data)+maxData-1)/maxData)
	dst = slices.Grow(dst, len(data)+5*frames)
	for {
		n := min(len(data), maxData)
		dst = appendPktLen(dst, n+1)
		dst = append(dst, band)
		dst = append(dst, data[:n]...)
		data = data[n:]
		if len(data) == 0 {
			return dst
		}
	}
}

// ---------------------------------------------------------------------------
// Advertisements

const agentCap = "agent=forge/1.0"
const headRef = "refs/heads/main"

const uploadPackCapsV0 = "side-band-64k shallow thin-pack ofs-delta multi_ack symref=HEAD:refs/heads/main " + agentCap
const receivePackCapsV0 = "report-status side-band-64k delete-refs ofs-delta " + agentCap

// advertiseV0 writes a v0 ref advertisement for a service.
func advertiseV0(r *repository, service string, caps string) []byte {
	out := serviceIntro(service)
	names := r.refNames()
	if len(names) == 0 {
		return appendRefPacket(out, ZeroOid, "capabilities^{}", "\x00"+caps)
	}
	out = appendRefPacket(out, r.refs[headRef], "HEAD", "\x00"+caps)
	for _, name := range names {
		out = appendRefPacket(out, r.refs[name], name, "")
	}
	for _, name := range names {
		if target, tagged := r.advertisedTarget(r.refs[name]); tagged {
			out = appendRefPacket(out, target, name, "^{}")
		}
	}
	return out
}

func (r *repository) advertisedTarget(oid Oid) (Oid, bool) {
	if obj := r.objects[oid]; obj != nil && obj.typ == objTag {
		return r.peelTarget(oid), true
	}
	return ZeroOid, false
}

func appendRefPacket(dst []byte, oid Oid, name, attrs string) []byte {
	dst = appendPktLen(dst, 2*len(oid)+1+len(name)+len(attrs)+1)
	dst = oid.appendHex(dst)
	dst = append(dst, ' ')
	dst = append(dst, name...)
	dst = append(dst, attrs...)
	return append(dst, '\n')
}

func serviceIntro(service string) []byte {
	return append(appendPktText(nil, "# service="+service+"\n"), pktFlush...)
}

// advertiseV2 writes the protocol v2 capability advertisement.
func advertiseV2(service string) []byte {
	out := serviceIntro(service)
	for _, capability := range [...]string{
		"version 2", "fetch=shallow", "ls-refs=unborn", "object-format=sha1", agentCap,
	} {
		out = appendPktText(out, capability+"\n")
	}
	return append(out, pktFlush...)
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
			out = appendPktText(out, "unborn HEAD symref-target:"+headRef+"\n")
		}
		return append(out, pktFlush...)
	}
	if oid, present := r.refs[headRef]; present && args.symrefs && args.matches("HEAD") {
		out = appendRefPacket(out, oid, "HEAD", " symref-target:"+headRef)
	}
	for _, name := range names {
		if !args.matches(name) {
			continue
		}
		oid := r.refs[name]
		var attrs string
		if args.peel {
			if target, tagged := r.advertisedTarget(oid); tagged {
				attrs = " peeled:" + target.String()
			}
		}
		out = appendRefPacket(out, oid, name, attrs)
	}
	return append(out, pktFlush...)
}

// ---------------------------------------------------------------------------
// Fetch object selection

type fetchRequest struct {
	wants   []Oid
	haves   []Oid
	shallow []Oid // the client's existing shallow boundaries
	deepen  int   // 0 means no depth limit
}

type fetchObjectSet map[Oid]struct{}

// add reports whether this is the first visit to the object.
func (set fetchObjectSet) add(oid Oid) bool {
	if set.has(oid) {
		return false
	}
	set[oid] = struct{}{}
	return true
}

func (set fetchObjectSet) has(oid Oid) bool {
	_, exists := set[oid]
	return exists
}

type fetchPlan struct {
	objects   fetchObjectSet
	shallow   []Oid
	unshallow []Oid
}

type fetchVisit struct {
	oid   Oid
	depth int
}

func (r *repository) planFetch(req *fetchRequest) fetchPlan {
	boundary := make(fetchObjectSet, len(req.shallow))
	for _, oid := range req.shallow {
		boundary.add(oid)
	}
	held, heldTrees := r.fetchHaves(req.haves, boundary)
	plan := fetchPlan{objects: make(fetchObjectSet)}
	visited := make(fetchObjectSet)
	queue := make([]fetchVisit, 0, len(req.wants))
	for _, oid := range req.wants {
		queue = append(queue, fetchVisit{oid: oid})
	}
	for len(queue) != 0 {
		visit := queue[0]
		queue = queue[1:]
		if !visited.add(visit.oid) {
			continue
		}
		object := r.objects[visit.oid]
		if object == nil {
			continue
		}
		if object.typ != objCommit {
			if !held.has(visit.oid) {
				// A direct non-commit want contributes the requested object.
				plan.objects.add(visit.oid)
			}
			if object.typ == objTag {
				if target := r.peelTarget(visit.oid); target != visit.oid {
					queue = append(queue, fetchVisit{oid: target, depth: visit.depth})
				}
			}
			continue
		}
		if held.has(visit.oid) && !boundary.has(visit.oid) {
			continue
		}
		commit := object.commit
		if commit == nil || req.deepen > 0 && visit.depth >= req.deepen {
			continue
		}
		if !held.has(visit.oid) {
			plan.objects.add(visit.oid)
			r.addFetchTree(commit.tree, plan.objects)
		}
		if len(commit.parents) != 0 && visit.depth+1 == req.deepen {
			plan.shallow = append(plan.shallow, visit.oid)
		}
		for _, parent := range commit.parents {
			queue = append(queue, fetchVisit{oid: parent, depth: visit.depth + 1})
		}
	}
	for oid := range heldTrees {
		delete(plan.objects, oid)
	}
	for _, oid := range req.shallow {
		if c := r.commitOf(oid); c != nil && allParentsAvailable(c.parents, held, plan.objects) {
			plan.unshallow = append(plan.unshallow, oid)
		}
	}
	slices.SortFunc(plan.shallow, compareOids)
	slices.SortFunc(plan.unshallow, compareOids)
	return plan
}

func compareOids(a, b Oid) int { return bytes.Compare(a[:], b[:]) }

func allParentsAvailable(parents []Oid, held, sending fetchObjectSet) bool {
	for _, oid := range parents {
		if !held.has(oid) && !sending.has(oid) {
			return false
		}
	}
	return true
}

// fetchHaves keeps ancestry distinct from the full set of held objects. A
// shallow commit's tree is held even when none of its parents are held.
func (r *repository) fetchHaves(haves []Oid, boundary fetchObjectSet) (fetchObjectSet, fetchObjectSet) {
	if len(haves) == 0 {
		return nil, nil
	}
	held := make(fetchObjectSet, len(haves))
	stack := slices.Clone(haves)
	for len(stack) != 0 {
		last := len(stack) - 1
		oid := stack[last]
		stack = stack[:last]
		if held.add(oid) && !boundary.has(oid) {
			if commit := r.commitOf(oid); commit != nil {
				stack = append(stack, commit.parents...)
			}
		}
	}
	objects := make(fetchObjectSet, len(held))
	for oid := range held {
		if commit := r.commitOf(oid); commit != nil {
			r.addFetchTree(commit.tree, objects)
		}
	}
	// Add held roots after tree traversal, so a tree listed explicitly in a
	// have cannot suppress its own children based on map iteration order.
	for oid := range held {
		objects.add(oid)
	}
	return held, objects
}

func (r *repository) addFetchTree(root Oid, objects fetchObjectSet) {
	tree := r.objects[root]
	if tree == nil || tree.typ != objTree || !objects.add(root) {
		return
	}
	for _, entry := range tree.tree {
		if entry.isDir() {
			r.addFetchTree(entry.oid, objects)
		} else {
			objects.add(entry.oid)
		}
	}
}

// ---------------------------------------------------------------------------
// Fetch responses

// fetchResponseV2 builds the protocol v2 fetch answer.
func (r *repository) fetchResponseV2(req *fetchRequest) []byte {
	if req.deepen == 1 && len(req.wants) == 1 && len(req.haves) == 0 && len(req.shallow) == 0 {
		tip := req.wants[0]
		if o := r.objects[tip]; o != nil && o.typ == objCommit {
			return r.clones.get(tip, func() []byte { return r.renderFetch(r.planFetch(req), true) })
		}
	}
	return r.renderFetch(r.planFetch(req), true)
}

func (r *repository) renderFetch(plan fetchPlan, version2 bool) []byte {
	var out []byte
	withBoundaries := len(plan.shallow)+len(plan.unshallow) != 0
	if version2 {
		out = appendPktText(out, "acknowledgments\n")
		out = appendPktText(out, "NAK\n")
		out = append(out, pktFlush...)
		if withBoundaries {
			out = appendPktText(out, "shallow-info\n")
		}
	}
	out = appendBoundaries(out, "shallow ", plan.shallow)
	out = appendBoundaries(out, "unshallow ", plan.unshallow)
	if version2 {
		if withBoundaries {
			out = append(out, pktFlush...)
		}
		out = appendPktText(out, "packfile\n")
	} else {
		out = appendPktText(out, "NAK\n")
	}
	pack := writeSelectedPack(r.objects, plan.objects)
	return append(appendSideband(out, 1, pack), pktFlush...)
}

func appendBoundaries(dst []byte, prefix string, oids []Oid) []byte {
	for _, oid := range oids {
		dst = appendPktLen(dst, len(prefix)+2*len(oid)+1)
		dst = append(dst, prefix...)
		dst = oid.appendHex(dst)
		dst = append(dst, '\n')
	}
	return dst
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
	return r.renderFetch(r.planFetch(req), false), "application/x-git-upload-pack-result", 200
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
	push, problem := parsePush(body)
	if problem != "" {
		return []byte(problem), "text/plain", 400
	}
	return r.applyPush(push), "application/x-git-receive-pack-result", 200
}

type pushRequest struct {
	commands []pushCommand
	pack     []byte
	sideband bool
}

func parsePush(input []byte) (pushRequest, string) {
	var push pushRequest
	for len(input) >= 4 {
		packet, n, err := readPacket(input)
		if err != nil || packet.kind != 'D' && packet.kind != 'f' {
			return pushRequest{}, "bad pkt-line stream"
		}
		input = input[n:]
		if packet.kind == 'f' {
			if len(push.commands) == 0 {
				break
			}
			push.pack = input
			return push, ""
		}
		line := bytes.TrimRight(packet.payload, "\n")
		var caps []byte
		line, caps, _ = bytes.Cut(line, []byte{0})
		command, valid, fast := parsePushCommandFast(line)
		if !fast {
			command, valid = parsePushCommand(string(line))
		}
		if !valid {
			return pushRequest{}, "bad command line"
		}
		push.sideband = push.sideband || bytes.Contains(caps, []byte("side-band-64k"))
		push.commands = append(push.commands, command)
	}
	return pushRequest{}, "no command list"
}

// parsePushCommandFast handles the common protocol form without tokenizing or
// converting the packet line to a string. The general parser below preserves
// the broader whitespace behavior for non-canonical inputs.
func parsePushCommandFast(line []byte) (pushCommand, bool, bool) {
	if len(line) < 87 || line[40] != ' ' || line[81] != ' ' || !bytes.HasPrefix(line[82:], []byte("refs/")) {
		return pushCommand{}, false, false
	}
	for _, c := range line[82:] {
		if c <= ' ' || c >= 0x7f {
			return pushCommand{}, false, false
		}
	}
	oldOid, oldOK := parseOidBytes(line[:40])
	newOid, newOK := parseOidBytes(line[41:81])
	return pushCommand{oldOid: oldOid, newOid: newOid, ref: string(line[82:])}, oldOK && newOK, true
}

func parsePushCommand(line string) (pushCommand, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "refs/") {
		return pushCommand{}, false
	}
	oldOid, oldOK := parseOid(fields[0])
	newOid, newOK := parseOid(fields[1])
	return pushCommand{oldOid: oldOid, newOid: newOid, ref: fields[2]}, oldOK && newOK
}

func (r *repository) applyPush(push pushRequest) []byte {
	// Publish objects before considering refs. Ref failures are reported
	// individually in command order and never roll back a successful unpack.
	unpackErr := r.importPushPack(push.pack)
	detail := "ok"
	if unpackErr != nil {
		detail = unpackErr.Error()
	}
	report := appendPktText(nil, "unpack "+detail+"\n")
	for _, command := range push.commands {
		reason := "unpack failed"
		if unpackErr == nil {
			reason = r.updatePushedRef(command)
		}
		if reason != "" {
			report = appendPktText(report, "ng "+command.ref+" "+reason+"\n")
		} else {
			report = appendPktText(report, "ok "+command.ref+"\n")
		}
	}
	report = append(report, pktFlush...)
	if push.sideband {
		report = appendSideband(nil, 1, report)
	}
	return append(report, pktFlush...)
}

func (r *repository) importPushPack(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	objects, err := parsePack(data, r.object)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		r.putObject(obj.typ, obj.data)
	}
	return nil
}

func (r *repository) updatePushedRef(cmd pushCommand) string {
	current, exists := r.refs[cmd.ref]
	creating := !cmd.newOid.IsZero() && cmd.oldOid.IsZero()
	switch {
	case creating && exists:
		return "already exists"
	case !creating && !exists:
		return "remote ref does not exist"
	case !cmd.oldOid.IsZero() && current != cmd.oldOid:
		return "non-fast-forward"
	}
	if cmd.newOid.IsZero() {
		delete(r.refs, cmd.ref)
		return ""
	}
	if r.objects[cmd.newOid] == nil {
		return "object not found"
	}
	if !creating && !r.fastForward(cmd.oldOid, cmd.newOid) {
		return "non-fast-forward"
	}
	r.refs[cmd.ref] = cmd.newOid
	return ""
}

// fastForward answers whether newObj descends from oldObj (or is equal). Tag
// refs compare the peeled commits.
func (r *repository) fastForward(oldObj, newObj Oid) bool {
	oldCommit, okOld := r.peelToCommit(oldObj)
	newCommit, okNew := r.peelToCommit(newObj)
	if !okOld || !okNew {
		return oldObj == newObj
	}
	// Receive-pack usually advances the ref by one commit. Check this edge
	// before allocating the ancestry walk; non-adjacent updates still use the
	// full graph check below.
	if object := r.objects[newCommit]; object != nil && object.typ == objCommit && object.commit != nil {
		for _, parent := range object.commit.parents {
			if parent == oldCommit {
				return true
			}
		}
	}
	return r.isAncestor(oldCommit, newCommit)
}
