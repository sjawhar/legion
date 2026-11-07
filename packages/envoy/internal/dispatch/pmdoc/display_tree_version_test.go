package pmdoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// displayTreeVersionGolden is testdata/display-tree-version.json: the display-tree version this
// build must stand at, and the hash of every display corpus document's tree in both read modes.
// A change to that hash is a change to what a stored tree holds, so the version a reader
// compares with (DisplayTreeVersion) must move with it.
type displayTreeVersionGolden struct {
	DisplayTreeVersion int    `json:"display_tree_version"`
	TreesSHA256        string `json:"trees_sha256"`
}

// corpusDisplayTreeHash is the hash of the display corpus's trees, over every document
// displayCorpusDocuments names and, within a document, DisplayLatest before DisplayVersion, each
// entry framed by its name and its length so no two corpora hash alike. The trees are encoded as
// a response carries them (displayReading), since the encoding is as much of what a stored tree
// holds as the read is.
//
// No block id is minted to be held still here, as the renderer's hashes must do
// (corpusRenderingAndTokenHashes): the display read names a block only where the markdown does
// (ReadForDisplay), so nothing random reaches a tree and the hash stands on what the read and
// the encoding write, which is what DisplayTreeVersion numbers.
func corpusDisplayTreeHash(t *testing.T) string {
	t.Helper()
	sum := sha256.New()
	frame := func(name string, payload []byte) {
		t.Helper()
		if _, err := fmt.Fprintf(sum, "%s %d\n", name, len(payload)); err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		if _, err := sum.Write(payload); err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
	}
	for _, path := range displayCorpusDocuments(t) {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		for _, mode := range []struct {
			name string
			mode DisplayMode
		}{{"latest", DisplayLatest}, {"version", DisplayVersion}} {
			frame(name+" "+mode.name, displayReading(t, string(markdown), mode.mode))
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// A build whose display trees of the corpus differ from the golden's stands at a display-tree
// version of its own: a stored tree is served in place of reading its text again only while the
// version that stored it stands at DisplayTreeVersion, so a change to a tree that left the
// number alone would serve readers a tree this build no longer makes.
func TestDisplayTreeVersionGoldenHoldsTheCorpus(t *testing.T) {
	path := filepath.Join("testdata", "display-tree-version.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the display tree version golden: %v", err)
	}
	var golden displayTreeVersionGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if golden.DisplayTreeVersion != DisplayTreeVersion {
		t.Fatalf("%s holds display tree version %d, want DisplayTreeVersion %d", path, golden.DisplayTreeVersion, DisplayTreeVersion)
	}
	trees := corpusDisplayTreeHash(t)
	if trees != golden.TreesSHA256 {
		t.Errorf("the display corpus reads to %s, and %s holds %s: raise DisplayTreeVersion past %d and write the new hash",
			trees, path, golden.TreesSHA256, DisplayTreeVersion)
	}
}

// The version a stored tree is keyed by is the constant until a test moves it, and the move is
// put back by the function the setter answers, so a test that raises the version leaves the ones
// after it reading at DisplayTreeVersion.
func TestSetDisplayTreeVersionForTestMovesTheVersionAndPutsItBack(t *testing.T) {
	if now := DisplayTreeVersionNow(); now != DisplayTreeVersion {
		t.Fatalf("DisplayTreeVersionNow() = %d before any test moved it, want DisplayTreeVersion %d", now, DisplayTreeVersion)
	}
	restore := SetDisplayTreeVersionForTest(DisplayTreeVersion + 7)
	if now := DisplayTreeVersionNow(); now != DisplayTreeVersion+7 {
		t.Errorf("DisplayTreeVersionNow() = %d while the test stands at %d", now, DisplayTreeVersion+7)
	}
	restore()
	if now := DisplayTreeVersionNow(); now != DisplayTreeVersion {
		t.Errorf("DisplayTreeVersionNow() = %d after the restore, want DisplayTreeVersion %d", now, DisplayTreeVersion)
	}
}

// DisplayTreeBuilds counts the reads a response paid for: one for a text no cache holds, none
// for the same text read again, and none for a snippet, whose tree is cut per query and stored
// by nobody.
func TestDisplayTreeBuildsCountsTheReadsAndNotTheSnippets(t *testing.T) {
	const markdown = "A line counted by the display tree build counter and read nowhere else.\n"
	before := DisplayTreeBuilds()
	DisplayJSON(markdown, DisplayLatest)
	if built := DisplayTreeBuilds() - before; built != 1 {
		t.Errorf("a text no cache held was built %d times, want 1", built)
	}
	DisplayJSON(markdown, DisplayLatest)
	if built := DisplayTreeBuilds() - before; built != 1 {
		t.Errorf("a second read of one text left the count at %d builds, want 1", built)
	}
	DisplaySnippetJSON("A snippet of the same kind of line", "\uE000", "\uE001")
	if built := DisplayTreeBuilds() - before; built != 1 {
		t.Errorf("a snippet left the count at %d builds, want the 1 the text cost", built)
	}
}
