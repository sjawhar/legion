package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusDir is the parser's corpus, the shared surface the two readers are compared over: the
// documents packages/envoy/internal/dispatch/pmdoc's own tests read.
const corpusDir = "../../../envoy/internal/dispatch/pmdoc/testdata/corpus"

// shapesDir is this reader's own shapes, the bodies its rules are written for: a fence closed on a
// line indented past three spaces, a fence opener indented past them, frontmatter closed by a
// longer line of dashes, and the text the outbox posts for a message — the body with the marker
// the runner appends (record.MessagePost.Posted), which is what the call sites measure. Each is
// written at the size that shows the reading, not at the size that passes a bound: the bounds are
// checked where the callers stand (internal/api), and what this table holds is the two readers'
// measures of the same bytes.
const shapesDir = "testdata/shapes"

// fixturePath and shapesFixturePath are what the Dispatch server's splitter reads of those
// documents, written by packages/envoy/internal/dispatch/itemblocks'
// TestTheDaemonsFixtureHoldsWhatSplitReadsOfTheCorpus with `-update` and kept honest by that test
// without it. The daemon module cannot import the splitter (itemblocks is another module's
// internal package), so its reading travels as these tables.
const (
	fixturePath       = "testdata/server-blocks.json"
	shapesFixturePath = "testdata/shape-blocks.json"
)

// serverReading is one corpus document as the server's splitter reads it: the document's file
// name, how many blocks it holds and its longest block in UTF-16 code units.
type serverReading struct {
	Document string `json:"document"`
	Blocks   int    `json:"blocks"`
	Longest  int    `json:"longest"`
}

// Both measures of this reader err toward refusing over the whole corpus and over its own shapes:
// its longest block is never shorter than the server's longest block, so no body the server
// refuses for a block over BlockUnits is taken here, and its block count (MostBlocks, the finest
// split) is never below the server's block count, so no body the server refuses for holding more
// than ItemBlocks blocks is taken here either. A body taken here and refused there is a post on no
// issue, which is what CheckBody exists to prevent.
func TestBothMeasuresAreNeverBelowTheServersOverTheCorpus(t *testing.T) {
	for _, table := range []struct{ name, dir, fixture string }{
		{"corpus", corpusDir, fixturePath},
		{"shapes", shapesDir, shapesFixturePath},
	} {
		t.Run(table.name, func(t *testing.T) {
			assertNeverBelowTheServer(t, table.dir, table.fixture)
		})
	}
}

// assertNeverBelowTheServer holds this reader to the server's reading of every document under dir,
// which fixture holds.
func assertNeverBelowTheServer(t *testing.T, dir, fixture string) {
	t.Helper()
	committed, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read the fixture (write it: cd ../../../envoy && go test ./internal/dispatch/itemblocks/ -update): %v", err)
	}
	var server []serverReading
	if err := json.Unmarshal(committed, &server); err != nil {
		t.Fatalf("read the fixture %s: %v", fixture, err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	if len(paths) == 0 || len(paths) != len(server) {
		t.Fatalf("%s holds %d documents and the fixture %d rows: rewrite it with cd ../../../envoy && go test ./internal/dispatch/itemblocks/ -update",
			dir, len(paths), len(server))
	}

	for _, document := range server {
		body, err := os.ReadFile(filepath.Join(dir, document.Document))
		if err != nil {
			t.Fatalf("read %s: %v", document.Document, err)
		}
		text := string(body)
		longest := 0
		for _, block := range Blocks(text) {
			if units := Units16(block); units > longest {
				longest = units
			}
		}
		if longest < document.Longest {
			t.Errorf("%s: this reader's longest block is %d UTF-16 units and the server's is %d; a body the server refuses for a block over the unit bound would be taken here",
				document.Document, longest, document.Longest)
		}
		if count := MostBlocks(text); count < document.Blocks {
			t.Errorf("%s: this reader counts %d blocks and the server holds %d; a body the server refuses for its block count would be taken here",
				document.Document, count, document.Blocks)
		}
	}
}
