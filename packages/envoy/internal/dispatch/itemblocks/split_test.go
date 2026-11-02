package itemblocks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// typed is a block's type and text, what most cases compare.
type typed struct{ Type, Text string }

func typesAndTexts(t *testing.T, body string, blocks []Block) []typed {
	t.Helper()
	out := make([]typed, len(blocks))
	for index, block := range blocks {
		if block.Text != body[block.From:block.To] {
			t.Errorf("block %d text %q, want body[%d:%d] %q", index, block.Text, block.From, block.To, body[block.From:block.To])
		}
		out[index] = typed{block.Type, block.Text}
	}
	return out
}

func assertSplit(t *testing.T, body string, want []typed) {
	t.Helper()
	if got := typesAndTexts(t, body, Split(body)); !reflect.DeepEqual(got, want) {
		t.Errorf("Split(%q)\n got: %q\nwant: %q", body, got, want)
	}
}

// The types are the Proof schema's node names, which pmdoc uses.
func TestSplitReadsTopLevelBlocks(t *testing.T) {
	assertSplit(t, "# Plan\n\nFirst.\n\n- a\n- b\n\n```\ncode\n```", []typed{
		{"heading", "# Plan"}, {"paragraph", "First."}, {"bullet_list", "- a\n- b"}, {"code_block", "```\ncode\n```"},
	})
	// A block the parser places inside its line, after a tab, starts at its line's start.
	assertSplit(t, "First.\n\n\tcode\n\nLast.\n", []typed{
		{"paragraph", "First."}, {"code_block", "\tcode"}, {"paragraph", "Last."},
	})
}

func TestSplitFallsBackToBlankLines(t *testing.T) {
	body := "<div>\nx\n</div>\n\nNext."
	if _, err := pmdoc.Parse(body); err == nil {
		t.Fatalf("pmdoc.Parse(%q) accepted the body this test needs refused", body)
	}
	assertSplit(t, body, []typed{{"text", "<div>\nx\n</div>"}, {"text", "Next."}})
	assertSplit(t, "\n\n<div>x</div>\n \t\n\n  indented\nline  \n\n", []typed{{"text", "<div>x</div>"}, {"text", "  indented\nline  "}})
}

func TestSplitReadsCRLFBodies(t *testing.T) {
	assertSplit(t, "First.\r\n\r\n- a\r\n- b\r\n\r\nLast.", []typed{
		{"paragraph", "First."}, {"bullet_list", "- a\r\n- b"}, {"paragraph", "Last."},
	})
	assertSplit(t, "<div>\r\nx\r\n</div>\r\n\r\nNext.", []typed{{"text", "<div>\r\nx\r\n</div>"}, {"text", "Next."}})
}

// pmdoc.Parse answers each of these with one empty paragraph, so Split checks for whitespace
// before it parses.
func TestSplitOfAnEmptyBodyIsNoBlocks(t *testing.T) {
	for _, body := range []string{"", " \n\n\t", "\r\n\r\n"} {
		if blocks := Split(body); blocks != nil {
			t.Errorf("Split(%q) = %+v, want no blocks", body, blocks)
		}
	}
}

// pmdoc.Render pads the short row to two cells and writes the item's `2.` as `2\.`; a block keeps
// the body's own bytes.
func TestSplitKeepsTheTextAsWritten(t *testing.T) {
	table := "| a | b |\n|---|---|\n| 1 |"
	list := "- x\n  2. y"
	body := table + "\n\n" + list
	doc, err := pmdoc.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := pmdoc.Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, table) || strings.Contains(rendered, list) {
		t.Fatalf("pmdoc.Render(%q) = %q, which no longer rewrites the table and the list this test needs", body, rendered)
	}
	assertSplit(t, body, []typed{{"table", table}, {"bullet_list", list}})
}

func TestHashReadsCRLFAsLF(t *testing.T) {
	if Hash("- a\r\n- b") != Hash("- a\n- b") {
		t.Error("a CR LF block hashes apart from its LF twin")
	}
	if Hash("a\rb") == Hash("a\nb") {
		t.Error("a lone carriage return hashes as a line feed")
	}
	if got, want := Hash(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; got != want {
		t.Errorf("Hash(\"\") = %s, want the SHA-256 of nothing, %s", got, want)
	}
}

// FuzzSplit runs its seeds, the parser's corpus, as a unit test: Split accepts every body, and its
// blocks are in order, each the body's own bytes, and none empty.
func FuzzSplit(f *testing.F) {
	paths, err := filepath.Glob("../pmdoc/testdata/corpus/*.md")
	if err != nil {
		f.Fatal(err)
	}
	if len(paths) == 0 {
		f.Fatal("no corpus under ../pmdoc/testdata/corpus")
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(body))
		f.Add(strings.ReplaceAll(string(body), "\n", "\r\n"))
	}
	f.Add("")
	f.Add("<div>\n\n</div>\r\n \t\r\n x")
	f.Fuzz(func(t *testing.T, body string) {
		to := 0
		for index, block := range Split(body) {
			if block.From < to || block.To <= block.From || block.To > len(body) {
				t.Fatalf("block %d spans [%d, %d) after %d in a body of %d bytes", index, block.From, block.To, to, len(body))
			}
			if block.Text != body[block.From:block.To] {
				t.Fatalf("block %d text %q, want body[%d:%d] %q", index, block.Text, block.From, block.To, body[block.From:block.To])
			}
			to = block.To
		}
	})
}
