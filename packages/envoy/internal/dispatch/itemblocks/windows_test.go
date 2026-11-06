package itemblocks

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// windowFixtureBlocks are one document's blocks as an indexer holds them: two headings, short
// paragraphs that pack together, and one block far longer than a window's budget.
func windowFixtureBlocks() []WindowBlock {
	return []WindowBlock{
		{ID: "b1", Type: "heading", Text: "What a window is"},
		{ID: "b2", Type: "paragraph", Text: strings.Repeat("a window holds neighbouring blocks. ", 10)},
		{ID: "b3", Type: "paragraph", Text: strings.Repeat("the next paragraph packs in beside it. ", 10)},
		{ID: "b4", Type: "heading", Text: "What a piece is"},
		{ID: "b5", Type: "paragraph", Text: strings.Repeat("a long block becomes pieces. ", 70)},
		{ID: "b6", Type: "paragraph", Text: "The last word."},
	}
}

// A block longer than one window's budget becomes pieces: the pieces cover the whole block,
// each one after the first repeats the WindowOverlapChars characters before it, and each is
// named "<the block>/p<n>", n from 1.
func TestWindowsCutALongBlockIntoOverlappingPieces(t *testing.T) {
	// Three and a half windows of text, so the last piece is a short one, in runes of three
	// bytes so a cut inside a rune would show.
	body := []rune(strings.Repeat("漢", 7*WindowTextChars/2))
	windows := Windows("", []WindowBlock{{ID: "long", Type: "paragraph", Text: string(body)}})
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
		// The piece holds the next WindowTextChars characters of the block and the
		// WindowOverlapChars before them.
		from, to := max(n*WindowTextChars-WindowOverlapChars, 0), min((n+1)*WindowTextChars, len(body))
		if want := string(body[from:to]); window.Text != want {
			t.Errorf("piece %d holds %d characters, want the block's characters %d to %d", n+1, utf8.RuneCountInString(window.Text), from, to)
		}
		covered = append(covered, body[max(n*WindowTextChars, 0):to]...)
	}
	if string(covered) != string(body) {
		t.Errorf("the pieces, each less its overlap, hold %d characters of the block's %d", len(covered), len(body))
	}
}

// A heading starts a window, neighbouring short blocks pack into one, a window records the
// blocks it covers in order, and the context leads the text of every window.
func TestWindowsPackNeighboursAndStartAtEveryHeading(t *testing.T) {
	const context = "A document / Its heading"
	blocks := windowFixtureBlocks()
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
	blocks := windowFixtureBlocks()
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

// DocumentWindows gives each window the title and the heading it stands under: a window before
// the first heading carries the title alone, every window of a section carries that section's
// heading, and an edit of one heading moves the text of its own section's windows and of no
// other.
func TestDocumentWindowsCarryTheHeadingAboveThem(t *testing.T) {
	const title = "notes.md"
	blocks := append([]WindowBlock{{ID: "b0", Type: "paragraph", Text: "Before any heading."}}, windowFixtureBlocks()...)
	windows := DocumentWindows(title, blocks)
	first, second := title+"\n\nWhat a window is", title+"\n\nWhat a piece is"
	contexts := []string{title, first, second, second, second, second}
	if len(windows) != len(contexts) {
		t.Fatalf("%d windows, want %d: the opening paragraph, the first section, b4, b5's two pieces and b6", len(windows), len(contexts))
	}
	for i, window := range windows {
		if !strings.HasPrefix(window.Text, contexts[i]+"\n\n") {
			t.Errorf("window %s (covering %v) opens with %q, want the context %q", window.IDSuffix, window.Covers, opening(window.Text), contexts[i])
		}
	}
	edited := slices.Clone(blocks)
	edited[1] = WindowBlock{ID: "b1", Type: "heading", Text: "What a window holds"}
	after := DocumentWindows(title, edited)
	for i := range windows {
		if moved, under := windows[i].Text != after[i].Text, i == 1; moved != under {
			t.Errorf("window %s moved = %t, want %t: the edited heading opens the windows covering %v and no other",
				windows[i].IDSuffix, moved, under, windows[i].Covers)
		}
	}
}

// opening is the start of a text, for a failure message.
func opening(value string) string {
	runes := []rune(value)
	return strings.ReplaceAll(string(runes[:min(len(runes), 80)]), "\n", "\\n")
}
