package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTreeOrderAndNestedRoundTrip(t *testing.T) {
	entries := []treeEntry{
		{name: "src", mode: "40000"},
		{name: "src.c", mode: "100644"},
		{name: "src", mode: "100644"},
		{name: "src0", mode: "100644"},
		{name: "a", mode: "40000"},
	}
	slices.SortStableFunc(entries, compareTreeEntries)
	var names []string
	for _, entry := range entries {
		name := entry.name
		if entry.isDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if want := []string{"a/", "src", "src.c", "src/", "src0"}; !slices.Equal(names, want) {
		t.Fatalf("tree names: %v, want %v", names, want)
	}
	r := newStore().createRepo("owner", "tree", "", false, false)
	files := map[string]flatFile{
		"README":       {mode: "100644", oid: r.putObject(objBlob, []byte("readme\n"))},
		"src/main.go":  {mode: "100644", oid: r.putObject(objBlob, []byte("package main\n"))},
		"src/run":      {mode: "100755", oid: r.putObject(objBlob, []byte("#!/bin/sh\n"))},
		"src/deep/ref": {mode: "120000", oid: r.putObject(objBlob, []byte("../../README"))},
	}
	tree := r.treeFromFlat(files)
	if got := r.flattenTree(tree); !reflect.DeepEqual(files, got) {
		t.Fatalf("tree round trip: %v, want %v", got, files)
	}
	for range 20 {
		if got := r.treeFromFlat(files); got != tree {
			t.Fatalf("tree serialization changed from %s to %s", tree, got)
		}
	}
}

func TestMergeFilePresenceAndConflicts(t *testing.T) {
	a := fileVersion{present: true, file: flatFile{mode: "100644", oid: Oid{1}}}
	b := fileVersion{present: true, file: flatFile{mode: "100644", oid: Oid{2}}}
	missing := fileVersion{}
	for _, tc := range []struct {
		name               string
		base, ours, theirs fileVersion
		want               fileVersion
		clean              bool
	}{
		{"unchanged", a, a, a, a, true},
		{"their edit", a, a, b, b, true},
		{"our edit", a, b, a, b, true},
		{"same edit", a, b, b, b, true},
		{"their deletion", a, a, missing, missing, true},
		{"our deletion", a, missing, a, missing, true},
		{"same deletion", a, missing, missing, missing, true},
		{"our addition", missing, a, missing, a, true},
		{"same addition", missing, a, a, a, true},
		{"different additions", missing, a, b, missing, false},
		{"modify delete", a, b, missing, missing, false},
		{"delete modify", a, missing, b, missing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, clean := mergeFileVersions(tc.base, tc.ours, tc.theirs)
			if clean != tc.clean || clean && got != tc.want {
				t.Fatalf("merge = %v, %v; want %v, %v", got, clean, tc.want, tc.clean)
			}
		})
	}
}

func TestLineDiffPreservesPrefixSuffixLineSemantics(t *testing.T) {
	for _, tc := range []struct {
		old, current string
		add, del     int
	}{
		{"", "", 0, 0},
		{"", "\n", 1, 0},
		{"a\nb\nc\n", "a\nx\ny\nc\n", 2, 1},
		{"a\nb", "a\nb\n", 0, 0},
		{"a\n\nb\n", "a\nb\n", 0, 1},
		{"old\nshared", "shared", 0, 1},
	} {
		add, del := lineDiff([]byte(tc.old), []byte(tc.current))
		if add != tc.add || del != tc.del {
			t.Fatalf("%q -> %q: +%d -%d, want +%d -%d", tc.old, tc.current, add, del, tc.add, tc.del)
		}
	}
}

func TestPktLineFramingAcrossSidebandBoundary(t *testing.T) {
	for _, size := range []int{0, 1, 65514, 65515, 65516, 131030} {
		payload := bytes.Repeat([]byte{42}, size)
		framed := appendSideband(nil, 1, payload)
		packets, err := decodePktLinesAll(framed)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []byte
		for _, packet := range packets {
			if packet.kind != 'D' || len(packet.payload) < 1 || packet.payload[0] != 1 || len(packet.payload)+4 > 65520 {
				t.Fatalf("invalid sideband frame for %d bytes", size)
			}
			decoded = append(decoded, packet.payload[1:]...)
		}
		if !bytes.Equal(decoded, payload) {
			t.Fatalf("sideband payload changed at size %d", size)
		}
	}
	for _, stream := range []string{"0003", "zzzz", "0009abc", "0000x"} {
		if _, err := decodePktLinesAll([]byte(stream)); err == nil {
			t.Fatalf("accepted invalid stream %q", stream)
		}
	}
}

func TestPushCommandFastParserMatchesGeneralParser(t *testing.T) {
	for _, tc := range []struct {
		line      string
		valid     bool
		fastParse bool
	}{
		{strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main", true, true},
		{" " + strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main", true, false},
		{strings.Repeat("z", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main", false, true},
		{strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main extra", false, false},
	} {
		fast, valid, handled := parsePushCommandFast([]byte(tc.line))
		general, generalValid := parsePushCommand(tc.line)
		if generalValid != tc.valid || handled != tc.fastParse || handled && valid != generalValid {
			t.Fatalf("fast parser = (%+v, %v, %v), general parser = (%+v, %v) for %q", fast, valid, handled, general, generalValid, tc.line)
		}
		if handled && valid && fast != general {
			t.Fatalf("fast parser = %+v, general parser = %+v for %q", fast, general, tc.line)
		}
	}
}

func TestFastForwardDirectParentAndHistoryFallback(t *testing.T) {
	r := newStore().createRepo("owner", "history", "", false, false)
	root := r.buildCommitObject(ZeroOid, nil, "root\n", "author", "committer", 1)
	middle := r.buildCommitObject(ZeroOid, []Oid{root}, "middle\n", "author", "committer", 2)
	tip := r.buildCommitObject(ZeroOid, []Oid{middle}, "tip\n", "author", "committer", 3)
	unrelated := r.buildCommitObject(ZeroOid, nil, "unrelated\n", "author", "committer", 4)
	for _, tc := range []struct {
		old, next Oid
		want      bool
	}{
		{middle, tip, true},
		{root, tip, true},
		{unrelated, tip, false},
	} {
		if got := r.fastForward(tc.old, tc.next); got != tc.want {
			t.Fatalf("fastForward(%s, %s) = %v, want %v", tc.old, tc.next, got, tc.want)
		}
	}
}

func TestGraphQLTruncationAndFragmentCycleReturnErrors(t *testing.T) {
	for _, query := range []string{"query Q($a:", "{viewer", "{f(a:[1", "{f(a:{x:", "fragment F on User { login }"} {
		if _, err := parseGraphQLDocument(query); err == nil {
			t.Fatalf("accepted truncated document %q", query)
		}
	}
	s := newServer(newStore(), "")
	result := s.runGraphQLOperation(map[string]any{
		"query": "{ viewer { ...F } } fragment F on User { login ...F }",
	})
	if !strings.Contains(string(mustJSON(result)), "Cannot spread fragment") {
		t.Fatalf("cycle did not return a GraphQL error: %v", result)
	}
}
