package pmdoc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// displayFixture is one document's display read in both modes, as testdata/display.json holds it.
type displayFixture struct {
	Name     string          `json:"name"`
	Markdown string          `json:"markdown"`
	Latest   json.RawMessage `json:"latest"`
	Version  json.RawMessage `json:"version"`
}

// displayCorpusDocuments are the documents the display read is held to: every one of
// testdata/corpus-display, which holds the shapes a reader must still be shown and no writer
// stores, and the documents of testdata/corpus this change added. Their fixture is a golden of
// what Go reads, not of what a browser reads: that the two agree is what the differential's
// cross-reader gate answers, over the whole corpus (gen/differential.sh).
func displayCorpusDocuments(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "corpus-display", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("testdata/corpus-display: %d documents, %v", len(paths), err)
	}
	for _, name := range []string{
		"list-nested-33-deep", "mention", "reply-quoting-a-block",
		"typed-block-with-a-server-attribute", "typed-blocks-of-every-type",
	} {
		paths = append(paths, filepath.Join("testdata", "corpus", name+".md"))
	}
	return paths
}

// TestReadForDisplayMatchesItsFixtures reads each document twice and holds both readings to
// testdata/display.json. Run it with PMDOC_WRITE_DISPLAY=1 to write that file from what this
// build reads, and read the diff: every line of it is a change to what a reader is shown.
func TestReadForDisplayMatchesItsFixtures(t *testing.T) {
	path := filepath.Join("testdata", "display.json")
	fixtures := make([]displayFixture, 0, len(displayCorpusDocuments(t)))
	for _, document := range displayCorpusDocuments(t) {
		markdown, err := os.ReadFile(document)
		if err != nil {
			t.Fatal(err)
		}
		fixture := displayFixture{Name: strings.TrimSuffix(filepath.Base(document), ".md"), Markdown: string(markdown)}
		fixture.Latest = displayReading(t, string(markdown), DisplayLatest)
		fixture.Version = displayReading(t, string(markdown), DisplayVersion)
		fixtures = append(fixtures, fixture)
	}
	next, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	next = append(next, '\n')
	if os.Getenv("PMDOC_WRITE_DISPLAY") != "" {
		if err := os.WriteFile(path, next, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d documents)", path, len(fixtures))
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []displayFixture
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(want) != len(fixtures) {
		t.Fatalf("%s holds %d documents, the display corpus has %d: run with PMDOC_WRITE_DISPLAY=1", path, len(want), len(fixtures))
	}
	for index, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if want[index].Name != fixture.Name {
				t.Fatalf("%s holds %q where the corpus has %q", path, want[index].Name, fixture.Name)
			}
			for _, mode := range []struct {
				name        string
				got, golden json.RawMessage
			}{
				{"latest", fixture.Latest, want[index].Latest},
				{"version", fixture.Version, want[index].Version},
			} {
				if !sameDisplayJSON(t, mode.got, mode.golden) {
					t.Errorf("%s reading differs from %s:\n got: %s\nwant: %s", mode.name, path, mode.got, mode.golden)
				}
			}
		})
	}
}

// displayReading is the display read of markdown in mode, encoded as a response carries it
// (toDisplayJSON): the attributes the browser's schema supplies itself are no part of the
// golden, and neither is a block id the markdown did not name, since the read mints none.
func displayReading(t *testing.T, markdown string, mode DisplayMode) json.RawMessage {
	t.Helper()
	doc := ReadForDisplay(markdown, mode)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the display read is outside the schema: %v", err)
	}
	encoded, err := json.Marshal(doc.toDisplayJSON())
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// sameDisplayJSON compares two readings by what they hold rather than by their bytes, so the
// golden's indentation is no part of the comparison.
func sameDisplayJSON(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	left, err := FromJSON(got)
	if err != nil {
		t.Fatalf("the display read: %v", err)
	}
	right, err := FromJSON(want)
	if err != nil {
		t.Fatalf("the golden reading: %v", err)
	}
	return left.EqualWithBlockIDs(right)
}

// TestReadForDisplayRefusesNothingTheWriterRefuses reads every document no writer stores and
// answers a tree for each: the display read never refuses (ReadForDisplay).
func TestReadForDisplayRefusesNothingTheWriterRefuses(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "corpus-errors", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("testdata/corpus-errors: %d documents, %v", len(paths), err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			markdown, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(string(markdown)); err == nil {
				t.Fatalf("%s is stored by the writer, so it belongs in testdata/corpus", path)
			}
			doc := ReadForDisplay(string(markdown), DisplayLatest)
			if err := doc.Validate(); err != nil {
				t.Fatalf("the display read is outside the schema: %v", err)
			}
		})
	}
}

// TestReadForDisplayShowsADocumentPastTheElementLimitAsItsText reads markdown whose elements pass
// MaxDocumentElements as one paragraph of literal text per part between blank lines, the answer
// to a refusal no block can be named for (ReadForDisplay).
func TestReadForDisplayShowsADocumentPastTheElementLimitAsItsText(t *testing.T) {
	markdown := strings.Repeat(")_", MaxDocumentElements) + "\n\nthe second part\n"
	if _, err := Parse(markdown); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("Parse() = %v, want ErrTooManyElements", err)
	}
	doc := ReadForDisplay(markdown, DisplayLatest)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the display read is outside the schema: %v", err)
	}
	if len(doc.Children) != 2 {
		t.Fatalf("the display read holds %s, want two paragraphs", BlockNames(doc.Children))
	}
	for index, want := range []string{strings.Repeat(")_", MaxDocumentElements), "the second part"} {
		block := doc.Children[index]
		if block.Type != "paragraph" || len(block.Children) != 1 || block.Children[0].Text != want {
			t.Errorf("block %d is %s holding %d nodes, want one paragraph of the part's own text", index, block.Type, len(block.Children))
		}
	}
}

// TestReadForDisplayShowsARefusedBlockAsItsTextAndKeepsTheOthers holds the per-block answer: a
// fenced code block whose language holds a backslash escape, which the browser's parser decodes
// and goldmark keeps as written so no writer stores it, is shown as its own text, and the blocks
// around it keep their formatting.
func TestReadForDisplayShowsARefusedBlockAsItsTextAndKeepsTheOthers(t *testing.T) {
	fence := "```"
	markdown := "# A heading\n\n" + fence + "go&amp;x\nx := 1\n" + fence + "\n\n*emphasis* after\n"
	if _, err := Parse(markdown); err == nil {
		t.Fatal("Parse() stored the document; this test wants one it refuses")
	}
	doc := ReadForDisplay(markdown, DisplayLatest)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the display read is outside the schema: %v", err)
	}
	if got := BlockNames(doc.Children); got != "a heading and two paragraphs" {
		t.Fatalf("the display read holds %s", got)
	}
	code := doc.Children[1]
	want := []string{fence + "go&amp;x", "x := 1", fence}
	for index, line := range want {
		node := code.Children[2*index]
		if code.Type != "paragraph" || len(code.Children) != 5 || node.Text != line {
			t.Fatalf("the refused block reads as %s holding %d nodes, want one paragraph of its three lines", code.Type, len(code.Children))
		}
	}
	last := doc.Children[2]
	if len(last.Children) != 2 || len(last.Children[0].Marks) != 1 || last.Children[0].Marks[0].Type != "emphasis" {
		t.Errorf("the paragraph after it lost its formatting: %+v", last.Children)
	}
}

// TestReadForDisplayEndsAnUnknownBlocksLinesAtEveryLineEnding holds the unknown-directive rule
// to the line endings a stored text actually carries. A message is stored as it was sent, and a
// person or an agent writing through a Windows client sends CRLF, while a lone CR reaches the
// store the same way (e2e/unknown-directive.e2e.ts). The fence lines of an unknown directive
// read as paragraph text line by line, so each of its lines is its own paragraph whichever
// ending the text uses: with LF the read already does this, and the two others must agree with
// it, since a reader is shown one body and not three readings of it. A line ending left inside
// a text node is worse than a wrong block: HTML draws neither CR nor CRLF as a break, so the
// lines run together on the page.
func TestReadForDisplayEndsAnUnknownBlocksLinesAtEveryLineEnding(t *testing.T) {
	want := []string{":::a", "body words", ":::"}
	for _, ending := range []struct{ name, between string }{
		{"a line feed", "\n"},
		{"a carriage return and a line feed", "\r\n"},
		{"a lone carriage return", "\r"},
	} {
		t.Run(ending.name, func(t *testing.T) {
			markdown := "Lead **bold**\n\n" + strings.Join(want, ending.between) + "\n\nAfter"
			doc := ReadForDisplay(markdown, DisplayLatest)
			if err := doc.Validate(); err != nil {
				t.Fatalf("the display read is outside the schema: %v", err)
			}
			if got := len(doc.Children); got != len(want)+2 {
				t.Fatalf("the display read holds %s, want the lead, the block's %d lines and the line after it", BlockNames(doc.Children), len(want))
			}
			for index, line := range want {
				block := doc.Children[index+1]
				if block.Type != "paragraph" || len(block.Children) != 1 || block.Children[0].Text != line {
					t.Errorf("line %d reads as %s holding %d nodes, want one paragraph of %q", index, block.Type, len(block.Children), line)
				}
			}
			for _, block := range doc.Children {
				for _, node := range block.Children {
					if strings.ContainsAny(node.Text, "\r\n") {
						t.Errorf("a text node keeps a line ending, which draws as no break at all: %q", node.Text)
					}
				}
			}
		})
	}
}
