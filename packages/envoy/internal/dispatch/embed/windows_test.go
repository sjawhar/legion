package embed

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// fixtureBlocks are one document's blocks as an indexer holds them: two headings, short
// paragraphs that pack together, and one block far longer than a window's budget.
func fixtureBlocks() []Block {
	return []Block{
		{ID: "b1", Type: "heading", Text: "What a window is"},
		{ID: "b2", Type: "paragraph", Text: strings.Repeat("a window holds neighbouring blocks. ", 10)},
		{ID: "b3", Type: "paragraph", Text: strings.Repeat("the next paragraph packs in beside it. ", 10)},
		{ID: "b4", Type: "heading", Text: "What a piece is"},
		{ID: "b5", Type: "paragraph", Text: strings.Repeat("a long block becomes pieces. ", 70)},
		{ID: "b6", Type: "paragraph", Text: "The last word."},
	}
}

// No window passes what the embedder reads whole, which is the property the body cap used to
// hold: over the fixture, and over a worst case built to break it - a context as long as a
// whole item, a heading as long as a block may be, and blocks of every length around the
// budget, in runes of two, three and four bytes.
func TestEveryWindowFitsTheEmbedder(t *testing.T) {
	worst := []Block{{ID: "h", Type: "heading", Text: strings.Repeat("漢", 32000)}}
	for _, chars := range []int{0, 1, windowOverlapChars, windowTextChars - 1, windowTextChars, windowTextChars + 1, 4 * windowTextChars} {
		for _, letter := range []string{"é", "漢", "🙂"} {
			worst = append(worst, Block{ID: "b" + strconv.Itoa(len(worst)), Type: "paragraph", Text: strings.Repeat(letter, chars)})
		}
	}
	for name, blocks := range map[string][]Block{"the fixture": fixtureBlocks(), "the worst case": worst} {
		for _, window := range Windows(strings.Repeat("the owner's title and the heading above. ", 1000), blocks) {
			if runes := utf8.RuneCountInString(window.Text); runes > MaxInputRunes {
				t.Errorf("%s: window %s holds %d runes, want at most MaxInputRunes (%d)", name, window.IDSuffix, runes, MaxInputRunes)
			}
			if !utf8.ValidString(window.Text) {
				t.Errorf("%s: window %s was cut inside a rune", name, window.IDSuffix)
			}
		}
	}
}

// A block longer than one window's budget becomes pieces: the pieces cover the whole block,
// each one after the first repeats the windowOverlapChars characters before it, and each is
// named "<the block>/p<n>", n from 1.
func TestWindowsCutALongBlockIntoOverlappingPieces(t *testing.T) {
	// Three and a half windows of text, so the last piece is a short one, in runes of three
	// bytes so a cut inside a rune would show.
	body := []rune(strings.Repeat("漢", 7*windowTextChars/2))
	windows := Windows("", []Block{{ID: "long", Type: "paragraph", Text: string(body)}})
	if len(windows) != 4 {
		t.Fatalf("%d windows for a block of %d characters, want 4", len(windows), len(body))
	}
	var covered []rune
	for n, window := range windows {
		if want := "long/p" + strconv.Itoa(n+1); window.IDSuffix != want {
			t.Errorf("piece %d is named %q, want %q", n+1, window.IDSuffix, want)
		}
		if !slices.Equal(window.Covers, []string{"long"}) {
			t.Errorf("piece %d covers %v, want the block alone", n+1, window.Covers)
		}
		// The piece holds the next windowTextChars characters of the block and the
		// windowOverlapChars before them.
		from, to := max(n*windowTextChars-windowOverlapChars, 0), min((n+1)*windowTextChars, len(body))
		if want := string(body[from:to]); window.Text != want {
			t.Errorf("piece %d holds %d characters, want the block's characters %d to %d", n+1, utf8.RuneCountInString(window.Text), from, to)
		}
		covered = append(covered, body[max(n*windowTextChars, 0):to]...)
	}
	if string(covered) != string(body) {
		t.Errorf("the pieces, each less its overlap, hold %d characters of the block's %d", len(covered), len(body))
	}
}

// A heading starts a window, neighbouring short blocks pack into one, a window records the
// blocks it covers in order, and the context leads the text of every window.
func TestWindowsPackNeighboursAndStartAtEveryHeading(t *testing.T) {
	const context = "A document / Its heading"
	blocks := fixtureBlocks()
	windows := Windows(context, blocks)
	covers := make([][]string, len(windows))
	for i, window := range windows {
		covers[i] = window.Covers
		if !strings.HasPrefix(window.Text, context+"\n\n") {
			t.Errorf("window %s opens with %q, want the context first", window.IDSuffix, opening(window.Text))
		}
		if window.IDSuffix != window.Covers[0] && !strings.HasPrefix(window.IDSuffix, window.Covers[0]+"/p") {
			t.Errorf("window %s is named for no block it covers (%v)", window.IDSuffix, window.Covers)
		}
	}
	want := [][]string{{"b1", "b2", "b3"}, {"b4"}, {"b5"}, {"b5"}, {"b6"}}
	if !slices.EqualFunc(covers, want, slices.Equal) {
		t.Errorf("the windows cover %v, want %v: the heading b4 starts a window, b5 is too long to share one, and b6 follows its pieces", covers, want)
	}
	if want := context + "\n\n" + blocks[0].Text + "\n\n" + blocks[1].Text + "\n\n" + blocks[2].Text; windows[0].Text != want {
		t.Errorf("the first window holds %q, want the context and then its three blocks", opening(windows[0].Text))
	}
}

// An edited heading moves the hash of every window under it, because the heading is the context
// those windows embed, and renames none of them.
func TestTheContextIsPartOfEveryWindowsText(t *testing.T) {
	blocks := fixtureBlocks()
	before := Windows("A document / What a window is", blocks)
	after := Windows("A document / What a window holds", blocks)
	if len(before) != len(after) {
		t.Fatalf("%d windows before the heading changed, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i].IDSuffix != after[i].IDSuffix {
			t.Errorf("window %d is named %q, was %q: an edited heading renames no window", i, after[i].IDSuffix, before[i].IDSuffix)
		}
		if before[i].Text == after[i].Text {
			t.Errorf("window %s embeds the same text under both headings, so its hash would not move", before[i].IDSuffix)
		}
	}
}

// opening is the start of a text, for a failure message.
func opening(value string) string {
	runes := []rune(value)
	return strings.ReplaceAll(string(runes[:min(len(runes), 80)]), "\n", "\\n")
}
