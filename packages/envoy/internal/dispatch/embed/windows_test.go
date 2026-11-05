package embed

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
)

// No window the indexers build (itemblocks.Windows) passes what this embedder reads whole,
// which is the property the body cap used to hold: over one document's blocks - two headings,
// short paragraphs that pack together and one block far longer than a window's budget - and
// over a worst case built to break it: a context as long as a whole item, a heading as long as
// a block may be, and blocks of every length around the budget, in runes of two, three and four
// bytes. The window builder is a leaf package with no embedder in it, and this is the test that
// holds the two together.
func TestEveryWindowFitsTheEmbedder(t *testing.T) {
	fixture := []itemblocks.WindowBlock{
		{ID: "b1", Type: "heading", Text: "What a window is"},
		{ID: "b2", Type: "paragraph", Text: strings.Repeat("a window holds neighbouring blocks. ", 10)},
		{ID: "b3", Type: "paragraph", Text: strings.Repeat("the next paragraph packs in beside it. ", 10)},
		{ID: "b4", Type: "heading", Text: "What a piece is"},
		{ID: "b5", Type: "paragraph", Text: strings.Repeat("a long block becomes pieces. ", 70)},
		{ID: "b6", Type: "paragraph", Text: "The last word."},
	}
	worst := []itemblocks.WindowBlock{{ID: "h", Type: "heading", Text: strings.Repeat("漢", 32000)}}
	for _, chars := range []int{0, 1, itemblocks.WindowOverlapChars, itemblocks.WindowTextChars - 1, itemblocks.WindowTextChars, itemblocks.WindowTextChars + 1, 4 * itemblocks.WindowTextChars} {
		for _, letter := range []string{"é", "漢", "🙂"} {
			worst = append(worst, itemblocks.WindowBlock{ID: "b" + strconv.Itoa(len(worst)), Type: "paragraph", Text: strings.Repeat(letter, chars)})
		}
	}
	for name, blocks := range map[string][]itemblocks.WindowBlock{"the fixture": fixture, "the worst case": worst} {
		for _, window := range itemblocks.Windows(strings.Repeat("the owner's title and the heading above. ", 1000), blocks) {
			if runes := utf8.RuneCountInString(window.Text); runes > MaxInputRunes {
				t.Errorf("%s: window %s holds %d runes, want at most MaxInputRunes (%d)", name, window.IDSuffix, runes, MaxInputRunes)
			}
			if !utf8.ValidString(window.Text) {
				t.Errorf("%s: window %s was cut inside a rune", name, window.IDSuffix)
			}
		}
	}
}
