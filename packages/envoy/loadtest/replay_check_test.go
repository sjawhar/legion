package main

import (
	"crypto/md5"
	"testing"
)

func TestMaskTextOrdinals(t *testing.T) {
	o := ordinals{}
	s := maskText("a 0bd2f0d8-1c2e-4c47-9a39-0c1a2b3c4d5e b 2026-10-26T22:55:00.123Z c 0BD2F0D8-1C2E-4C47-9A39-0C1A2B3C4D5E", o)
	if s != "a <id1> b <ts> c <id1>" {
		t.Fatalf("masked %q", s)
	}
}

func TestMD5FieldsKeepsBoundaries(t *testing.T) {
	a, b := md5.New(), md5.New()
	md5Fields(a, "ab", "c")
	md5Fields(b, "a", "bc")
	if string(a.Sum(nil)) == string(b.Sum(nil)) {
		t.Fatal("moving a field boundary must change the digest")
	}
}

func TestDumpAnchorOrdinals(t *testing.T) {
	docOrd := map[string]int{"doc-uuid": 2}
	scopes := map[string]ordinals{}
	scope := func(k string) ordinals {
		if scopes[k] == nil {
			scopes[k] = ordinals{}
		}
		return scopes[k]
	}
	first := dumpAnchorOf([]byte(`{"artifact_id":"doc-uuid","block_id":"B1","mark_id":"m-uuid-1","version":4,"quote":"q","orphaned":false}`), docOrd, scope)
	second := dumpAnchorOf([]byte(`{"artifact_id":"doc-uuid","block_id":"b1","mark_id":"m-uuid-2","version":5,"quote":"q","orphaned":true}`), docOrd, scope)
	if first.Doc != 2 || first.Block != second.Block || first.Mark == second.Mark || second.Version != 5 || !second.Orphaned {
		t.Fatalf("anchors %+v %+v", *first, *second)
	}
	if dumpAnchorOf(nil, docOrd, scope) != nil {
		t.Fatal("no anchor, no dump")
	}
}
