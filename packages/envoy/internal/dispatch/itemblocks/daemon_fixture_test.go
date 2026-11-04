package itemblocks

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateFixture = flag.Bool("update", false, "rewrite the fixture this package pins for the daemon's block reader")

// daemonFixture is where this splitter's reading of the corpus is kept for a module that cannot
// import it: the Legion daemon refuses a body Dispatch would refuse at the call it stands in, and
// reads the body's blocks with a reader of its own (packages/daemon/internal/dispatch/blocks.go),
// since internal/dispatch/itemblocks is another module's internal package. That reader's test
// holds itself to this table, and this test is what keeps the table honest when Split changes.
const daemonFixture = "../../../../daemon/internal/dispatch/testdata/server-blocks.json"

// corpusDocuments is every document of the parser's corpus, the shared surface the two readers are
// compared over, as FuzzSplit's seeds read it.
const corpusDocuments = "../pmdoc/testdata/corpus/*.md"

// serverReading is one corpus document as Split reads it: the document's file name, how many
// blocks it holds and its longest block in UTF-16 code units (Units16), the two measures Dispatch
// bounds a body by.
type serverReading struct {
	Document string `json:"document"`
	Blocks   int    `json:"blocks"`
	Longest  int    `json:"longest"`
}

// readCorpus is Split's reading of every corpus document, in the order the glob names them, which
// is sorted.
func readCorpus(t *testing.T) []serverReading {
	t.Helper()
	paths, err := filepath.Glob(corpusDocuments)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no corpus under %s", corpusDocuments)
	}
	readings := make([]serverReading, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		reading := serverReading{Document: filepath.Base(path)}
		for _, block := range Split(string(body)) {
			reading.Blocks++
			if units := Units16(block.Text); units > reading.Longest {
				reading.Longest = units
			}
		}
		readings = append(readings, reading)
	}
	return readings
}

// The fixture the daemon's block reader is held to holds what Split reads of the corpus today:
// `-update` writes it, and without it this test fails naming every document whose block count or
// longest block differs from the committed table, every corpus document the table misses and
// every row of the table whose document is gone. A change to this splitter that moves either
// measure fails here, where the fixture is rewritten, rather than leaving the daemon's reader
// held to a reading the server no longer has.
func TestTheDaemonsFixtureHoldsWhatSplitReadsOfTheCorpus(t *testing.T) {
	readings := readCorpus(t)
	encoded, err := json.MarshalIndent(readings, "", "  ")
	if err != nil {
		t.Fatalf("marshal the corpus reading: %v", err)
	}
	encoded = append(encoded, '\n')

	if *updateFixture {
		if err := os.MkdirAll(filepath.Dir(daemonFixture), 0o755); err != nil {
			t.Fatalf("create the fixture's directory: %v", err)
		}
		if err := os.WriteFile(daemonFixture, encoded, 0o644); err != nil {
			t.Fatalf("write the fixture: %v", err)
		}
		return
	}

	committed, err := os.ReadFile(daemonFixture)
	if err != nil {
		t.Fatalf("read the fixture (run `go test ./internal/dispatch/itemblocks/ -update`): %v", err)
	}
	var want []serverReading
	if err := json.Unmarshal(committed, &want); err != nil {
		t.Fatalf("read the fixture %s: %v", daemonFixture, err)
	}
	held := make(map[string]serverReading, len(want))
	for _, row := range want {
		held[row.Document] = row
	}
	for _, reading := range readings {
		row, ok := held[reading.Document]
		delete(held, reading.Document)
		switch {
		case !ok:
			t.Errorf("%s is in the corpus and in no row of the fixture: Split reads it as %d blocks, longest %d units",
				reading.Document, reading.Blocks, reading.Longest)
		case row != reading:
			t.Errorf("%s: Split reads %d blocks, longest %d units; the fixture holds %d blocks, longest %d",
				reading.Document, reading.Blocks, reading.Longest, row.Blocks, row.Longest)
		}
	}
	for document := range held {
		t.Errorf("the fixture holds a row for %s, which is no longer in the corpus", document)
	}
	if !t.Failed() && !bytes.Equal(encoded, committed) {
		t.Errorf("the fixture reads the same as the corpus does and is written otherwise than this test writes it")
	}
	if t.Failed() {
		t.Log("rewrite it: go test ./internal/dispatch/itemblocks/ -update")
	}
}
