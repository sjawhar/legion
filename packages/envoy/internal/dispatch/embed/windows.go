package embed

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	// windowTextChars is the most characters of block text one window holds: about 300 tokens at
	// the 4-characters-a-token rate EstimateTokens uses, which is a passage a reader would call
	// one point rather than one document. Small enough that a hit names a place in a long item,
	// large enough that a paragraph is rarely cut.
	windowTextChars = 1200
	// windowOverlapChars is how much of a long block's previous text each of its pieces after
	// the first carries in front of it, so a sentence a piece boundary cuts stands whole in one
	// of the two pieces.
	windowOverlapChars = 200
	// headingType is the node type of a heading, which always starts a window: a heading opens a
	// section, and a window that began under the heading before it would carry text from two.
	headingType = "heading"
)

// Block is one block of one owner, as the indexer that has just written it holds it.
type Block struct {
	// ID is the block's id, stable across edits, which is what a window's id is anchored on.
	ID string
	// Type is the block's node type, as the document parser names it ("heading", "paragraph",
	// "bullet_list", a table): Windows reads it only to find a heading.
	Type string
	// Text is the block's whole text.
	Text string
}

// Window is one vector's worth of an owner's text.
type Window struct {
	// IDSuffix is what follows the owner key in the window's embeddings id, which the indexer
	// writes as "<owner key>#<IDSuffix>": the id of the first block the window covers, and
	// "<block id>/p<n>", n from 1, for each piece of a block too long to hold in one window. An
	// id anchored on a block rather than on a position is why inserting a paragraph re-embeds
	// its own window alone, where "window 7" would rename every window after it.
	IDSuffix string
	// Covers is the ids of the blocks whose text this window holds, in the order it holds them;
	// one id for each piece of a long block. A meaning hit on the window names one of them.
	Covers []string
	// Text is what is embedded: the context, then the blocks' text.
	Text string
}

// Windows builds the windows one owner's blocks are embedded in, in the order the blocks are
// given. blocks are the owner's blocks in order, each with its id, its node type and its text,
// as the indexer that has just written them holds them; context is what gives those blocks
// their meaning (the owner's title and the nearest heading above for a document, the issue's
// title for a comment or a message, the question for an ask's parts).
//
// A window is a run of neighbouring blocks holding at most windowTextChars characters of block
// text; a heading always starts a new one; and a block longer than that budget on its own
// becomes pieces of at most windowTextChars characters, each after the first carrying the
// previous windowOverlapChars characters of the block in front of it. Characters are runes, as
// the rest of this package counts them, and no cut falls inside one.
//
// Each window's text is the context, then its blocks' text, a blank line between each. The
// context is part of what is hashed, which is how a window notices that its context changed: an
// edited heading moves the hash of every window under it and of no other. It is cut to
// windowTextChars runes, so a window of a heading as long as a block may be holds the blocks
// whole all the same; with that cut no window passes 4,800 runes, far inside MaxInputRunes.
func Windows(context string, blocks []Block) []Window {
	var windows []Window
	var run []Block
	runChars := 0
	flush := func() {
		if len(run) > 0 {
			windows = append(windows, wholeBlocks(context, run))
			run, runChars = nil, 0
		}
	}
	for _, block := range blocks {
		chars := utf8.RuneCountInString(block.Text)
		if chars > windowTextChars {
			flush()
			windows = append(windows, pieces(context, block)...)
			continue
		}
		if block.Type == headingType || runChars+chars > windowTextChars {
			flush()
		}
		run = append(run, block)
		runChars += chars
	}
	flush()
	return windows
}

// DocumentWindows is Windows for a document, whose context is its title and the nearest heading
// above the window, where Windows takes one context for every block it is given. It calls
// Windows once for each heading section - the heading and the blocks under it, up to the next
// heading - with that section's own context, which is what gives each window the heading it
// stands under. A heading always starts a window, so a section's windows are the windows of a
// run of blocks and the sections' windows together are the ones one call would make, each with
// the heading above it as context.
//
// The heading's own text is in the context of its section and in the first window of it, which
// covers the heading block: a query matching the heading's words finds that window, and an edit
// of the heading moves the content hash of every window of its section and of no other.
func DocumentWindows(title string, blocks []Block) []Window {
	var windows []Window
	section, context := []Block(nil), title
	flush := func() {
		if len(section) > 0 {
			windows = append(windows, Windows(context, section)...)
			section = nil
		}
	}
	for _, block := range blocks {
		if block.Type == headingType {
			flush()
			context = title
			if block.Text != "" {
				context = strings.TrimPrefix(title+"\n\n"+block.Text, "\n\n")
			}
		}
		section = append(section, block)
	}
	flush()
	return windows
}

// wholeBlocks is the window a run of neighbouring blocks makes, named for the first of them.
func wholeBlocks(context string, run []Block) Window {
	covers := make([]string, len(run))
	texts := make([]string, len(run))
	for i, block := range run {
		covers[i], texts[i] = block.ID, block.Text
	}
	return Window{IDSuffix: run[0].ID, Covers: covers, Text: windowText(context, texts...)}
}

// pieces cuts a block longer than one window's budget into the windows that cover it: each
// piece holds the next windowTextChars characters of the block and repeats the
// windowOverlapChars before them, so the pieces together hold every character of the block and
// a sentence a boundary cuts is whole in the piece after it.
func pieces(context string, block Block) []Window {
	runes := []rune(block.Text)
	windows := make([]Window, 0, (len(runes)+windowTextChars-1)/windowTextChars)
	for start := 0; start < len(runes); start += windowTextChars {
		piece := string(runes[max(start-windowOverlapChars, 0):min(start+windowTextChars, len(runes))])
		windows = append(windows, Window{
			IDSuffix: block.ID + "/p" + strconv.Itoa(len(windows)+1),
			Covers:   []string{block.ID},
			Text:     windowText(context, piece),
		})
	}
	return windows
}

// windowText is what one window embeds: the context, cut to windowTextChars runes, and then
// each text that holds anything, a blank line between them. A block holding no text is covered
// by its window and adds nothing to what is embedded.
func windowText(context string, texts ...string) string {
	var built strings.Builder
	write := func(part string) {
		if part == "" {
			return
		}
		if built.Len() > 0 {
			built.WriteString("\n\n")
		}
		built.WriteString(part)
	}
	write(text.HeadRunes(context, windowTextChars))
	for _, part := range texts {
		write(part)
	}
	return built.String()
}
