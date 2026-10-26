package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// A transcript's title is its last custom-title line's, as Claude Code reads it; its session and
// working directory are the first conversation line's. A line that is not JSON is skipped. The
// file and in-memory forms agree.
func TestDescribeReadsTheTitleRenameRecords(t *testing.T) {
	f := &fixture{}
	f.raw(`{"type":"custom-title","customTitle":"first title","sessionId":"s-1"}`)
	f.add(prompt("One."))
	f.raw(`not json`)
	f.raw(`{"type":"custom-title","customTitle":"LEGION-1-implementer","sessionId":"s-1"}`)
	f.raw(`{"type":"agent-name","agentName":"LEGION-1-implementer","sessionId":"s-1"}`)
	want := Description{Title: "LEGION-1-implementer", SessionID: "s-1", Cwd: "/work"}
	if got := Describe(f.bytes()); got != want {
		t.Errorf("Describe = %+v, want %+v", got, want)
	}
	path := filepath.Join(t.TempDir(), "s-1.jsonl")
	if err := os.WriteFile(path, f.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DescribeFile(path)
	if err != nil || got != want {
		t.Errorf("DescribeFile = %+v, %v; want %+v", got, err, want)
	}
	if s := read(t, f.bytes()); s.Title != want.Title {
		t.Errorf("the reader's title %q, want %q", s.Title, want.Title)
	}
}
