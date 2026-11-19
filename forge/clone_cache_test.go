package main

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
)

func TestShallowCloneCacheTracksObjectAvailability(t *testing.T) {
	st := newStore()
	r := st.createRepo("alice", "repo", "", false, false)
	missing := []byte("fetched after the original commit\n")
	blob := hashObjectId(objBlob, missing)
	tree := r.buildTreeObject([]treeEntry{{mode: "100644", name: "file", oid: blob}})
	oldTip, _ := st.commitAsViewer(r, tree, nil, "root")
	tip, _ := st.commitAsViewer(r, tree, []Oid{oldTip}, "tip")
	req := &fetchRequest{wants: []Oid{tip}, deepen: 1}
	before := r.fetchResponseV2(req)
	if has := clonePackIDs(t, before); len(has) != 2 || !has.has(tip) || !has.has(tree) {
		t.Fatalf("before adding blob: wrong shallow pack: %v", has)
	}
	r.putObject(objBlob, missing)
	after := r.fetchResponseV2(req)
	if has := clonePackIDs(t, after); len(has) != 3 || !has.has(blob) || has.has(oldTip) {
		t.Fatalf("after adding blob: wrong shallow pack: %v", has)
	}
	if !bytes.Equal(after, r.fetchResponseV2(req)) {
		t.Fatal("cache hit changed the response bytes")
	}
	wantBoundary := []byte("shallow " + tip.String() + "\n")
	if !bytes.Contains(after, wantBoundary) {
		t.Fatalf("missing shallow boundary %q", wantBoundary)
	}
}

func clonePackIDs(t *testing.T, reply []byte) fetchObjectSet {
	t.Helper()
	packets, err := decodePktLinesAll(reply)
	if err != nil {
		t.Fatal(err)
	}
	var pack []byte
	var inPack bool
	for _, pkt := range packets {
		if string(pkt.payload) == "packfile\n" {
			inPack = true
		} else if inPack && pkt.kind == 'D' {
			if len(pkt.payload) == 0 || pkt.payload[0] != 1 {
				t.Fatalf("invalid pack sideband: %v", pkt.payload)
			}
			pack = append(pack, pkt.payload[1:]...)
		}
	}
	objects, err := parsePack(pack, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(fetchObjectSet, len(objects))
	for _, object := range objects {
		ids.add(hashObjectId(object.typ, object.data))
	}
	return ids
}

func TestConcurrentShallowRepliesShareCompression(t *testing.T) {
	var cache shallowReplyCache
	var builds atomic.Int32
	var clients sync.WaitGroup
	start := make(chan struct{})
	results := make(chan []byte, 50)
	for range cap(results) {
		clients.Add(1)
		go func() {
			defer clients.Done()
			<-start
			results <- cache.get(Oid{1}, func() []byte {
				builds.Add(1)
				return []byte("compressed bytes")
			})
		}()
	}
	close(start)
	clients.Wait()
	close(results)
	shared := <-results
	for reply := range results {
		if len(reply) == 0 || &reply[0] != &shared[0] {
			t.Fatal("callers did not receive the same immutable reply")
		}
	}
	if actual := builds.Load(); actual != 1 {
		t.Fatalf("built %d times, want once", actual)
	}
}
