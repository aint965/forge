// Packfiles: parsing (including OFS_DELTA and REF_DELTA) and generation.
// Packs this server writes carry plain objects only, in oid order — no deltas.
// The client's reader verifies hashes, sizes and the trailer checksum, and
// accepts any object order.

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	packObjCommit   = 1
	packObjTree     = 2
	packObjBlob     = 3
	packObjTag      = 4
	packObjOfsDelta = 6
	packObjRefDelta = 7
)

// packObject is one object decoded from a pack.
type packObject struct {
	typ  byte
	data []byte
}

// parsePack decodes a whole packfile. resolve looks up objects already in the
// store, for REF_DELTA bases that are not in the pack itself. The trailer
// checksum is verified.
func parsePack(pack []byte, resolve func(Oid) *gitObject) ([]packObject, error) {
	if len(pack) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack has a bad signature")
	}
	version := uint32(pack[4])<<24 | uint32(pack[5])<<16 | uint32(pack[6])<<8 | uint32(pack[7])
	if version != 2 {
		return nil, fmt.Errorf("pack version %d, expected 2", version)
	}
	count := int(uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11]))

	// Verify the trailer: SHA-1 of everything before the last 20 bytes.
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}
	body := pack[12 : len(pack)-20]

	type entry struct {
		typ      byte
		size     int64
		data     []byte // inflated payload (for deltas: the delta instructions)
		ofsBase  int    // pack offset of the base, for OFS_DELTA
		refBase  Oid    // base oid, for REF_DELTA
		offset   int    // this entry's offset in body
		resolved *packObject
	}
	entries := make([]*entry, 0, count)
	off := 0
	for i := 0; i < count; i++ {
		if off >= len(body) {
			return nil, errors.New("pack ends before its object count")
		}
		start := off
		c := body[off]
		off++
		typ := (c >> 4) & 0x7
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if off >= len(body) {
				return nil, errors.New("pack header overruns the pack")
			}
			c = body[off]
			off++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		e := &entry{typ: typ, size: size, offset: start}
		switch typ {
		case packObjOfsDelta:
			if off >= len(body) {
				return nil, errors.New("ofs-delta overruns the pack")
			}
			c = body[off]
			off++
			ofs := int64(c & 0x7f)
			for c&0x80 != 0 {
				if off >= len(body) {
					return nil, errors.New("ofs-delta offset overruns the pack")
				}
				c = body[off]
				off++
				ofs = ((ofs + 1) << 7) | int64(c&0x7f)
			}
			e.ofsBase = start - int(ofs)
			if e.ofsBase < 0 {
				return nil, errors.New("ofs-delta base before the pack start")
			}
		case packObjRefDelta:
			if off+20 > len(body) {
				return nil, errors.New("ref-delta overruns the pack")
			}
			copy(e.refBase[:], body[off:off+20])
			off += 20
		case packObjCommit, packObjTree, packObjBlob, packObjTag:
		default:
			return nil, fmt.Errorf("pack object type %d is unknown", typ)
		}
		// Inflate one zlib stream starting at off. bytes.Reader implements
		// io.ByteReader, so flate reads exactly the stream's bytes and the
		// remaining length tells us where the next entry starts.
		br := bytes.NewReader(body[off:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack object zlib header: %w", err)
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, size+1))
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("pack object inflate: %w", err)
		}
		if int64(len(inflated)) != size {
			return nil, fmt.Errorf("pack object size %d, declared %d", len(inflated), size)
		}
		off += len(body[off:]) - br.Len()
		e.data = inflated
		entries = append(entries, e)
	}

	byOffset := make(map[int]*entry, len(entries))
	for _, e := range entries {
		byOffset[e.offset] = e
	}

	// Resolve deltas, memoised; delta chains resolve recursively.
	var resolveEntry func(e *entry, depth int) (*packObject, error)
	resolveEntry = func(e *entry, depth int) (*packObject, error) {
		if depth > 64 {
			return nil, errors.New("delta chain too deep")
		}
		if e.resolved != nil {
			return e.resolved, nil
		}
		switch e.typ {
		case packObjCommit, packObjTree, packObjBlob, packObjTag:
			e.resolved = &packObject{typ: e.typ, data: e.data}
			return e.resolved, nil
		}
		var base *packObject
		if e.typ == packObjOfsDelta {
			be, ok := byOffset[e.ofsBase]
			if !ok {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			b, err := resolveEntry(be, depth+1)
			if err != nil {
				return nil, err
			}
			base = b
		} else {
			// REF_DELTA: the base may be in the pack or already in the store.
			var found *packObject
			for _, cand := range entries {
				if cand.typ == packObjCommit || cand.typ == packObjTree || cand.typ == packObjBlob || cand.typ == packObjTag {
					if hashObjectId(cand.typ, cand.data) == e.refBase {
						found = &packObject{typ: cand.typ, data: cand.data}
						break
					}
				}
			}
			if found == nil && resolve != nil {
				if stored := resolve(e.refBase); stored != nil {
					found = &packObject{typ: stored.typ, data: stored.data}
				}
			}
			if found == nil {
				// The base may itself be a delta in this pack: resolve every
				// entry and compare hashes.
				for _, cand := range entries {
					if cand == e {
						continue
					}
					r, err := resolveEntry(cand, depth+1)
					if err != nil {
						continue
					}
					if hashObjectId(r.typ, r.data) == e.refBase {
						found = r
						break
					}
				}
			}
			if found == nil {
				return nil, fmt.Errorf("ref-delta base %s is missing", e.refBase.String())
			}
			base = found
		}
		applied, err := applyDelta(base.data, e.data)
		if err != nil {
			return nil, err
		}
		e.resolved = &packObject{typ: base.typ, data: applied}
		return e.resolved, nil
	}

	out := make([]packObject, 0, len(entries))
	for _, e := range entries {
		r, err := resolveEntry(e, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// applyDelta applies git delta instructions to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta varint overruns the delta")
			}
			c := delta[off]
			off++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size %d, base is %d", srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			// Copy from the base: offset and size follow as flagged bytes.
			var cpOff, cpSize int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy overruns the delta")
					}
					cpOff |= int64(delta[off]) << uint(8*i)
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy overruns the delta")
					}
					cpSize |= int64(delta[off]) << uint(8*i)
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff < 0 || cpSize < 0 || cpOff+cpSize > int64(len(base)) {
				return nil, errors.New("delta copy is outside the base")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			// Insert the next op bytes literally.
			if off+int(op) > len(delta) {
				return nil, errors.New("delta insert overruns the delta")
			}
			out = append(out, delta[off:off+int(op)]...)
			off += int(op)
		} else {
			return nil, errors.New("delta opcode 0 is reserved")
		}
	}
	if int64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta result size %d, declared %d", len(out), dstSize)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pack generation

// writePack serialises objects as a packfile with no deltas, in oid order.
func writePack(objs map[Oid]*gitObject) []byte {
	oids := make([]Oid, 0, len(objs))
	for oid := range objs {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return bytes.Compare(oids[i][:], oids[j][:]) < 0 })

	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	n := len(oids)
	buf.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	for _, oid := range oids {
		o := objs[oid]
		writePackEntry(&buf, o.typ, o.data)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// writePackEntry writes one undeltified object: a type/size varint header
// followed by the zlib stream.
func writePackEntry(buf *bytes.Buffer, typ byte, data []byte) {
	size := len(data)
	first := (typ << 4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	buf.WriteByte(first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	zw, _ := zlib.NewWriterLevel(buf, flate.BestSpeed)
	zw.Write(data)
	zw.Close()
}
