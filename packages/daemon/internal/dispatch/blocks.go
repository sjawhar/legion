package dispatch

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// Dispatch bounds a body block by block rather than as a whole: each block to BlockUnits UTF-16
// code units and the body to ItemBlocks blocks (generated.go, the values GET /api/v1/limits
// publishes). The server splits a body with its document parser and falls back to splitting at
// blank lines (packages/envoy/internal/dispatch/itemblocks/split.go); this module's reader splits
// the same text without that parser, which the daemon cannot import, so a call that carries a text
// the server would refuse is refused where the caller stands rather than in the outbox, where the
// post is lost and only a notice is left.
//
// The rule where the two readers could disagree: refuse what the server would refuse. A text this
// reader refuses and the server would have taken costs its caller a shorter retry, which the
// refusal names; a text this reader takes and the server refuses is a post on no issue, which is
// what this check exists to prevent. The two bounds pull that rule in opposite directions - one
// is passed by holding fewer, longer blocks and the other by holding more, shorter ones - so each
// bound is measured its own way, and each measure errs toward refusing.
//
// The block bound reads the text as the largest blocks it could hold (Blocks): every blank line
// that could stand inside one of the server's blocks joins what is around it into one block here:
//
//   - a blank line inside a fenced code block (``` or ~~~), which the server reads as one block;
//   - a blank line inside a typed block, the container directive the parser reads as one block
//     from its opener (:::ask{...}) through the line of colons that closes it;
//   - a blank line inside the frontmatter a body opens with, which the parser reads as one block
//     from the line of dashes that opens it through the line of dashes that closes it;
//   - a blank line before a line that is indented at all, which in the server's parser continues
//     a list item or an indented code block (and which joins a paragraph to an indented code
//     block below it, where the parser reads two blocks);
//   - a blank line between two list items, which the server reads as one loose list, and one
//     between two quoted lines, which the server reads as two quotes and this reader as one.
//
// Everything else splits where the server's fallback splits, at a blank line, so the two agree on
// paragraphs, headings, tables, thematic breaks, and lists and quotes written without blank lines.
// Joining only ever makes a block longer, but the shapes it joins are a list, not a proof: a shape
// the parser holds in one block and this list misses reads as shorter blocks here. So the two
// readings are compared document by document over the parser's own corpus (corpus_test.go, over
// the table packages/envoy/internal/dispatch/itemblocks writes from its splitter), and no document
// of that corpus holds a block longer than the block this reader holds it in.
//
// The block count reads the text as the most blocks it could hold (MostBlocks): its lines that are
// not blank. Every block of the server's holds at least one line with text in it and no two of its
// blocks share a line, so that count is never below the server's, whatever the parser makes of the
// text - the places the server starts a block with no blank line before it included (a heading, a
// fence opener, a list item, a thematic break after a paragraph), which a reader that splits at
// blank lines alone can never see. Counting the joined runs instead would take a body of more
// blocks than Dispatch holds a body to and lose the post that carried it.

// Blocks is text's blocks, in order, each as its author wrote it: from the start of the line the
// block begins on through its last line that is not blank, without the blank lines that end it. A
// text of only whitespace has no blocks, as the server's reader has none for it.
func Blocks(text string) []string {
	var blocks []string
	start, end := -1, 0 // where the block being read begins, and the end of its last full line
	opener := ""        // that block's first line, which says whether a later line continues it
	gap := false        // a blank line stands between that block's last line and the next one
	fence := ""         // the run of backticks, tildes or colons an open block was opened with
	offset := 0
	// Frontmatter stands at the start of a body and nowhere else, so it is read here rather than
	// by opensFence, and as one block, blank lines and all, as the parser reads it.
	if closed := frontmatter(text); closed > 0 {
		blocks = append(blocks, strings.TrimSuffix(text[:closed], "\r"))
		offset = closed
	}
	for offset < len(text) {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += offset
		}
		line := text[offset:lineEnd]
		switch {
		case fence != "":
			// Every line of an open fence belongs to its block, blank ones included; a blank line
			// is interior, so only a line with text in it carries the block's end forward.
			if strings.Trim(line, " \t\r") != "" {
				end = lineEnd
			}
			if closesFence(line, fence) {
				fence = ""
			}
		case strings.Trim(line, " \t\r") == "":
			gap = true
		default:
			if start >= 0 && gap && !continues(opener, line) {
				blocks = append(blocks, strings.TrimSuffix(text[start:end], "\r"))
				start = -1
			}
			if start < 0 {
				start, opener = offset, line
			}
			end, gap = lineEnd, false
			fence = opensFence(line)
		}
		offset = lineEnd + 1
	}
	if start >= 0 {
		blocks = append(blocks, strings.TrimSuffix(text[start:end], "\r"))
	}
	return blocks
}

// MostBlocks is the most blocks the server could read in text: its lines that are not blank,
// counted. Every block of the server's holds at least one line with text in it and no two of its
// blocks share a line, so no reading of the text holds more blocks than it holds such lines, and
// this count is never below the server's. It is the finest split, where Blocks is the coarsest,
// and it is what CheckBody holds the block count to.
func MostBlocks(text string) int {
	count := 0
	for offset := 0; offset < len(text); {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += offset
		}
		if strings.Trim(text[offset:lineEnd], " \t\r") != "" {
			count++
		}
		offset = lineEnd + 1
	}
	return count
}

// continues says whether line, the first line with text in it after a blank line, goes on with the
// block opener began rather than starting a block of its own: it is indented, so the server's
// parser reads it as a list item's later paragraph or as an indented code block; or both lines
// are list items, which the server reads as one loose list; or both are quoted.
func continues(opener, line string) bool {
	switch {
	case strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t"):
		return true
	case listMarker(opener) && listMarker(line):
		return true
	case strings.HasPrefix(opener, ">") && strings.HasPrefix(line, ">"):
		return true
	}
	return false
}

// listMarker says whether line opens a list item: a bullet (-, * or +) or a number followed by .
// or ), then a space or tab, or nothing more on the line.
func listMarker(line string) bool {
	rest := line
	if digits := len(rest) - len(strings.TrimLeft(rest, "0123456789")); digits > 0 {
		rest = rest[digits:]
		if !strings.HasPrefix(rest, ".") && !strings.HasPrefix(rest, ")") {
			return false
		}
		rest = rest[1:]
	} else {
		if !strings.HasPrefix(rest, "-") && !strings.HasPrefix(rest, "*") && !strings.HasPrefix(rest, "+") {
			return false
		}
		rest = rest[1:]
	}
	return rest == "" || strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t")
}

// opensFence is the run of backticks, tildes or colons line opens a block that holds its blank
// lines with, empty when it opens none: a fenced code block (``` or ~~~), or a typed block, the
// container directive the parser reads as one block from its opener (:::ask{...}) through the
// line of colons that closes it. Any indentation counts: a fence inside a list item is indented,
// and a line that only looks like one inside an indented code block joins what follows it into
// one block, which is the reading that refuses more rather than less.
func opensFence(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	for _, mark := range []string{"`", "~", ":"} {
		run := len(trimmed) - len(strings.TrimLeft(trimmed, mark))
		if run >= 3 {
			return trimmed[:run]
		}
	}
	return ""
}

// closesFence says whether line closes a block opened with fence: the same character, at least as
// many of them, and nothing else on the line. A typed block's colons must stand at the left margin
// as well, since a typed block written inside another one closes the inner one: reading the outer
// one on to the line that closes it at the margin keeps the whole of it in one block, which is the
// reading that refuses more rather than less.
func closesFence(line, fence string) bool {
	mark := fence[:1]
	if mark == ":" && !strings.HasPrefix(line, ":") {
		return false
	}
	trimmed := strings.TrimRight(strings.TrimLeft(line, " \t"), " \t\r")
	return len(trimmed) >= len(fence) && strings.Trim(trimmed, mark) == ""
}

// frontmatter is where the frontmatter text opens with ends: the offset just past the line of
// dashes that closes it, and 0 when text opens with no line of dashes or no later line closes
// one. The parser reads frontmatter as one block, blank lines and all. A body that opens with a
// thematic break and holds a second one is read as one block to that second line here, where the
// parser reads two breaks and what stands between them, which is the reading that refuses more
// rather than less.
func frontmatter(text string) int {
	offset, first := 0, true
	for offset < len(text) {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += offset
		}
		line := strings.TrimRight(text[offset:lineEnd], " \t\r")
		dashes := len(line) >= 3 && strings.Trim(line, "-") == ""
		switch {
		case first && !dashes:
			return 0
		case !first && dashes:
			return lineEnd
		}
		offset, first = lineEnd+1, false
	}
	return 0
}

// Units16 is text's length in UTF-16 code units, how Dispatch counts a text: a rune outside the
// Basic Multilingual Plane counts as the two units its surrogate pair takes. A byte that is not
// valid UTF-8 reads as U+FFFD, one unit, as the JSON encoder writes it.
func Units16(text string) int {
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	return units
}

// BlockTooLong is one block of a text over BlockUnits: Position is the block's place in the text
// counted from 1, Units its length and Limit the bound it passed. The wording lives in Error() and
// nowhere else; a caller adds the subject and its own code.
type BlockTooLong struct {
	Position int
	Units    int
	Limit    int
}

func (e *BlockTooLong) Error() string {
	return fmt.Sprintf("block %d is %d characters over the %d-character block limit Dispatch holds every block to (%d/%d)",
		e.Position, e.Units-e.Limit, e.Limit, e.Units, e.Limit)
}

// TooManyBlocks is a text of more blocks than ItemBlocks: Blocks is how many it holds and Limit
// the bound it passed. Its wording lives in Error() too.
type TooManyBlocks struct {
	Blocks int
	Limit  int
}

func (e *TooManyBlocks) Error() string {
	return fmt.Sprintf("it is %d blocks over the %d-block limit Dispatch holds every body to (%d/%d)",
		e.Blocks-e.Limit, e.Limit, e.Blocks, e.Limit)
}

// CheckBody answers the first bound text passes, or nil when it passes none: a *TooManyBlocks when
// it holds more than ItemBlocks blocks, else a *BlockTooLong naming the first block over
// BlockUnits. The count is checked first, as the server checks it first. Each bound reads the text
// the way that cannot take what the server would refuse: the count over the finest split
// (MostBlocks), the unit bound over the largest blocks (Blocks).
func CheckBody(text string) error {
	if count := MostBlocks(text); count > ItemBlocks {
		return &TooManyBlocks{Blocks: count, Limit: ItemBlocks}
	}
	for index, block := range Blocks(text) {
		if units := Units16(block); units > BlockUnits {
			return &BlockTooLong{Position: index + 1, Units: units, Limit: BlockUnits}
		}
	}
	return nil
}
