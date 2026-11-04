package dispatch

import (
	"reflect"
	"strings"
	"testing"
)

func assertBlocks(t *testing.T, text string, want []string) {
	t.Helper()
	if got := Blocks(text); !reflect.DeepEqual(got, want) {
		t.Errorf("Blocks(%q)\n got: %q\nwant: %q", text, got, want)
	}
}

// The shapes this reader and the Dispatch server's (itemblocks.Split) read the same way, each
// checked against that reader's own answer: paragraphs, headings, tables and thematic breaks
// split at blank lines; a fenced code block holding blank lines is one block, as are a loose
// list, an indented code block holding a blank line and a list item's later paragraph; and a body
// the server's parser refuses falls back to the same blank-line split on both sides.
func TestBlocksReadsTheShapesTheServerReadsTheSameWay(t *testing.T) {
	assertBlocks(t, "# Plan\n\nFirst.\n\n- a\n- b\n\n```\ncode\n```",
		[]string{"# Plan", "First.", "- a\n- b", "```\ncode\n```"})
	assertBlocks(t, "intro\n\n```\na\n\nb\n```\n\ntail",
		[]string{"intro", "```\na\n\nb\n```", "tail"})
	assertBlocks(t, "~~~\nx\n\ny\n~~~", []string{"~~~\nx\n\ny\n~~~"})
	assertBlocks(t, "- a\n\n- b\n\ntail", []string{"- a\n\n- b", "tail"})
	assertBlocks(t, "1. a\n\n2. b", []string{"1. a\n\n2. b"})
	assertBlocks(t, "- a\n\n  more of a\n\ntail", []string{"- a\n\n  more of a", "tail"})
	assertBlocks(t, "    code\n\n    more\n\ntail", []string{"    code\n\n    more", "tail"})
	assertBlocks(t, "| a | b |\n| - | - |\n| 1 | 2 |\n\n---\n\ntail",
		[]string{"| a | b |\n| - | - |\n| 1 | 2 |", "---", "tail"})
	assertBlocks(t, "A\n\n<div>\nx\n\ny\n</div>\n\nB", []string{"A", "<div>\nx", "y\n</div>", "B"})
	assertBlocks(t, "First.\r\n\r\n- a\r\n- b\r\n\r\nLast.", []string{"First.", "- a\r\n- b", "Last."})
	assertBlocks(t, "", nil)
	assertBlocks(t, " \n\t\n\n", nil)
	assertBlocks(t, "\n\nonly.\n\n", []string{"only."})
}

// Where the two readers disagree, this one reads the larger block, which refuses more rather than
// less. Two quotes with a blank line between them are two blocks to the server's parser (a
// blockquote each) and one block here, and a paragraph followed by an indented code block is a
// paragraph and a code block there and one block here. A pair whose texts are each under the
// bound and together over it is refused at the call that carries them rather than posted and
// refused by Dispatch.
func TestBlocksReadsBlocksTheServerSeparatesAsOne(t *testing.T) {
	assertBlocks(t, "> a\n\n> b\n\ntail", []string{"> a\n\n> b", "tail"})
	assertBlocks(t, "para\n\n    code\n\n    more\n\ntail", []string{"para\n\n    code\n\n    more", "tail"})

	half := "> " + strings.Repeat("q", BlockUnits-2)
	pair := half + "\n\n" + half
	if err := CheckBody(pair); err == nil {
		t.Fatalf("two quotes of %d units each pass CheckBody, want the pair refused as one block", BlockUnits)
	}
}

// A fenced code block holding blank lines is one block to the server, so it is one block here:
// read as the blank lines' separate blocks it would pass the bound this way and be refused by
// Dispatch on the post.
func TestBlocksReadsAFenceHoldingBlankLinesAsOneBlock(t *testing.T) {
	fenced := "```\n" + strings.Repeat(strings.Repeat("x", 999)+"\n\n", 40) + "```"
	blocks := Blocks(fenced)
	if len(blocks) != 1 || blocks[0] != fenced {
		t.Fatalf("a fence of 40 paragraphs read as %d blocks, want the fence whole as one", len(blocks))
	}
	// An unclosed fence runs to the end of the text, as the server's parser reads one.
	assertBlocks(t, "```\na\n\nb", []string{"```\na\n\nb"})
	// A fence inside a list item is indented, and its blank lines are inside it too.
	assertBlocks(t, "- item\n\n  ```\n  a\n\n  b\n  ```\n\ntail", []string{"- item\n\n  ```\n  a\n\n  b\n  ```", "tail"})
}

func TestUnits16CountsAsDispatchCounts(t *testing.T) {
	for text, want := range map[string]int{"": 0, "abc": 3, "é": 1, "\U0001F600": 2, "a\U0001F600b": 4} {
		if got := Units16(text); got != want {
			t.Errorf("Units16(%q) = %d, want %d", text, got, want)
		}
	}
}

// CheckBody answers the first bound a text passes, in the server's order: the block count first,
// then the first block over the unit bound, each refusal naming how far over it is.
func TestCheckBodyNamesTheBoundATextPasses(t *testing.T) {
	if err := CheckBody("one paragraph.\n\n" + strings.Repeat("x", BlockUnits)); err != nil {
		t.Fatalf("a body whose longest block is exactly the bound = %v, want it taken", err)
	}
	over := CheckBody("one paragraph.\n\n" + "\U0001F600" + strings.Repeat("x", BlockUnits-1))
	long, ok := over.(*BlockTooLong)
	if !ok || long.Position != 2 || long.Units != BlockUnits+1 {
		t.Fatalf("a second block one unit over = %#v, want block 2 at %d units", over, BlockUnits+1)
	}
	if want := "block 2 is 1 characters over the 32000-character block limit Dispatch holds every block to (32001/32000)"; long.Error() != want {
		t.Fatalf("the refusal reads %q, want %q", long.Error(), want)
	}
	crowded := CheckBody(strings.Repeat("x\n\n", ItemBlocks+2))
	many, ok := crowded.(*TooManyBlocks)
	if !ok || many.Blocks != ItemBlocks+2 {
		t.Fatalf("a body of %d blocks = %#v, want %d blocks named", ItemBlocks+2, crowded, ItemBlocks+2)
	}
	if want := "it is 2 blocks over the 32768-block limit Dispatch holds every body to (32770/32768)"; many.Error() != want {
		t.Fatalf("the refusal reads %q, want %q", many.Error(), want)
	}
	// The count is read first, as the server reads it first: a body that passes both bounds is
	// refused for how many blocks it holds.
	both := CheckBody(strings.Repeat("x\n\n", ItemBlocks) + strings.Repeat("y", BlockUnits+1))
	if _, ok := both.(*TooManyBlocks); !ok {
		t.Fatalf("a body over both bounds = %#v, want the block count named", both)
	}
}
