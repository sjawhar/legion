package pmdoc

import (
	"bytes"
	"container/list"
	"encoding/json"
	"strings"
	"testing"
)

// The display tree a response carries is ReadForDisplay's own tree, encoded once and kept by the
// text it was read from, so two readers of one comment are served the same bytes.
func TestDisplayJSONIsTheDisplayReadsTreeAndIsKept(t *testing.T) {
	// One id for every block the read mints, so the kept encoding and a second read of the same
	// text are the same bytes: a minted id would differ between the two.
	SetBlockIDGenerator(func() string { return "f0f0f0f0-0000-4000-8000-000000000001" })
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
	const markdown = "A **bold** line nothing else in this package reads.\n"
	tree := DisplayJSON(markdown, DisplayLatest)
	want, err := json.Marshal(ReadForDisplay(markdown, DisplayLatest).toJSON())
	if err != nil {
		t.Fatalf("encode the display read: %v", err)
	}
	if !bytes.Equal(tree, want) {
		t.Errorf("DisplayJSON =\n\t%s\nwant ReadForDisplay's\n\t%s", tree, want)
	}
	if again := DisplayJSON(markdown, DisplayLatest); &again[0] != &tree[0] {
		t.Errorf("a second read of the same text built the tree again")
	}
	if DisplayJSON("", DisplayLatest) != nil {
		t.Errorf("an empty text was read as a tree")
	}
}

// The cache holds at most what it is given room for, counted as the bytes of the JSON it holds,
// and the least recently read entry is the one that leaves.
func TestTheDisplayCacheHoldsItsBound(t *testing.T) {
	cache := &displayTreeCache{limit: 64, items: map[displayCacheKey]*list.Element{}, order: list.New()}
	first := displayCacheKey{sum: [32]byte{1}}
	second := displayCacheKey{sum: [32]byte{2}}
	third := displayCacheKey{sum: [32]byte{3}}
	cache.put(first, make(json.RawMessage, 32))
	cache.put(second, make(json.RawMessage, 32))
	if _, held := cache.get(first); !held {
		t.Fatalf("the first tree left while the cache still had room")
	}
	// Reading the first put the second at the back, so the third evicts it.
	cache.put(third, make(json.RawMessage, 32))
	if _, held := cache.get(second); held {
		t.Errorf("the least recently read tree stayed")
	}
	if _, held := cache.get(first); !held {
		t.Errorf("the most recently read tree left")
	}
	if cache.held > cache.limit {
		t.Errorf("the cache holds %d bytes, over its %d bound", cache.held, cache.limit)
	}
	cache.put(displayCacheKey{sum: [32]byte{4}}, make(json.RawMessage, 65))
	if _, held := cache.get(displayCacheKey{sum: [32]byte{4}}); held {
		t.Errorf("a tree larger than the whole cache was kept")
	}
}

// A search snippet's tree is the headline read for display with the match marked: the sentinels
// are gone from the text, the words between them carry DisplayHighlightMark, and the cut's
// unmatched delimiters were balanced away before the read.
func TestDisplaySnippetJSONMarksWhatTheQueryMatched(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	tree := DisplaySnippetJSON("Keep the "+start+"astrolabe"+stop+" here", start, stop)
	var root struct {
		Content []struct {
			Content []struct {
				Text  string `json:"text"`
				Marks []struct {
					Type string `json:"type"`
				} `json:"marks"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	if len(root.Content) != 1 {
		t.Fatalf("the snippet read as %d blocks, want one paragraph: %s", len(root.Content), tree)
	}
	var text, highlighted strings.Builder
	for _, piece := range root.Content[0].Content {
		text.WriteString(piece.Text)
		for _, mark := range piece.Marks {
			if mark.Type == DisplayHighlightMark {
				highlighted.WriteString(piece.Text)
			}
		}
	}
	if text.String() != "Keep the astrolabe here" {
		t.Errorf("the snippet's text is %q, want it without the sentinels", text.String())
	}
	if highlighted.String() != "astrolabe" {
		t.Errorf("the snippet highlights %q, want the matched word", highlighted.String())
	}
}

// A headline the window cut is balanced before it is read, so the palette is shown no delimiter
// the parser would have paired across the cut.
func TestDisplaySnippetJSONBalancesACutHeadline(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	window := strings.Repeat("word ", 21) + start + "anchor" + stop + "** after the"
	tree := DisplaySnippetJSON(window, start, stop)
	if bytes.Contains(tree, []byte("**")) {
		t.Errorf("the cut's unmatched delimiter reached the tree: %s", tree)
	}
	if bytes.Contains(tree, []byte("strong")) {
		t.Errorf("the cut's unmatched delimiter opened a mark: %s", tree)
	}
}
