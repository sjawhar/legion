package pmdoc

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

// DisplayCacheBytes is how much of the display trees' JSON one process keeps: the bytes of the
// encodings themselves, not of the markdown they were read from or of the keys that find them.
// 64 MiB holds the stored text a busy dashboard reads over and over - an issue's comment list
// re-read on every event, an inbox's asks - and is a bound a server can carry beside its pools,
// not a target to fill.
const DisplayCacheBytes = 64 << 20

// DisplayJSON is the ProseMirror tree markdown is shown as (ReadForDisplay), encoded as the JSON
// a response carries beside the stored text, or nil for text with nothing in it. The read never
// refuses, so neither does this; an encoding that fails - which only an attribute no JSON holds
// could cause - is logged and answered with nil, since a reader shown no tree falls back to the
// markdown beside it.
//
// The answer is shared, never copied: a caller marshals it into a response and must not write to
// it.
func DisplayJSON(markdown string, mode DisplayMode) json.RawMessage {
	if markdown == "" {
		return nil
	}
	key := displayCacheKey{sum: sha256.Sum256([]byte(markdown)), mode: mode}
	if tree, held := displayCache.Get(key); held {
		return tree
	}
	tree, err := json.Marshal(ReadForDisplay(markdown, mode).toDisplayJSON())
	if err != nil {
		slog.Warn("dispatch: display tree encoding failed", "error", err)
		return nil
	}
	displayCache.Put(key, tree)
	return tree
}

// DisplayHighlightMark is the mark DisplaySnippetJSON puts over what a search matched. It is a
// mark of the display tree alone: no document stores it, no write accepts it, and it is not in
// markTypes, which is the schema of what Dispatch keeps.
const DisplayHighlightMark = "searchHighlight"

// DisplaySnippetJSON is the display tree of a search hit's snippet: the headline balanced as the
// cut left it (SearchSnippetMarkdown), read for display, with DisplayHighlightMark over every
// range the sentinels start and stop named. The sentinels ride through the read rather than being
// taken out before it, because they are private-use characters, which the parser reads as
// ordinary word characters and leaves in the text nodes they fall in: a tree walk then has their
// exact places, where an offset into the markdown would have had to survive both the balancing
// and the parse.
//
// The snippet is read fresh rather than from the cache: every query cuts its own headline, so a
// cached snippet would be a tree nothing asks for again, taking the room of a body many readers
// do share.
func DisplaySnippetJSON(headline, start, stop string) json.RawMessage {
	if headline == "" {
		return nil
	}
	tree := ReadForDisplay(SearchSnippetMarkdown(headline), DisplayLatest)
	highlightSnippet(tree, start, stop, false)
	encoded, err := json.Marshal(tree.toDisplayJSON())
	if err != nil {
		slog.Warn("dispatch: search snippet tree encoding failed", "error", err)
		return nil
	}
	return encoded
}

// highlightSnippet rewrites node's text children into the runs the sentinels cut them into, the
// runs between a start and a stop carrying DisplayHighlightMark, and answers whether the walk is
// inside a highlight when it leaves. The state carries across nodes in document order, since a
// headline may start a highlight in one text node and stop it in the next; a sentinel the parse
// put somewhere no text node holds - a link's destination the query matched - leaves the state
// where it was, which shows the reader one highlight too many or too few and never a refusal.
func highlightSnippet(node *Node, start, stop string, inside bool) bool {
	if node == nil {
		return inside
	}
	if node.Type == "text" {
		return inside
	}
	var rewritten []*Node
	for _, child := range node.Children {
		if child == nil {
			continue
		}
		if child.Type != "text" {
			inside = highlightSnippet(child, start, stop, inside)
			rewritten = append(rewritten, child)
			continue
		}
		rewritten, inside = appendHighlightRuns(rewritten, child, start, stop, inside)
	}
	node.Children = rewritten
	return inside
}

// appendHighlightRuns appends the runs text is cut into at its sentinels, each carrying the text
// node's own marks and, while the walk is inside a highlight, DisplayHighlightMark. The sentinels
// themselves are dropped, and a run with nothing in it is never appended, so a node whose whole
// text is one sentinel leaves nothing behind.
func appendHighlightRuns(into []*Node, text *Node, start, stop string, inside bool) ([]*Node, bool) {
	if !strings.Contains(text.Text, start) && !strings.Contains(text.Text, stop) {
		if inside {
			text.Marks = highlighted(text.Marks)
		}
		return append(into, text), inside
	}
	rest := text.Text
	for rest != "" {
		next, sentinel := len(rest), ""
		if at := strings.Index(rest, start); at >= 0 {
			next, sentinel = at, start
		}
		if at := strings.Index(rest, stop); at >= 0 && at < next {
			next, sentinel = at, stop
		}
		if next > 0 {
			run := &Node{Type: "text", Attrs: text.Attrs, Text: rest[:next], Marks: text.Marks}
			if inside {
				run.Marks = highlighted(text.Marks)
			}
			into = append(into, run)
		}
		if sentinel == "" {
			break
		}
		inside = sentinel == start
		rest = rest[next+len(sentinel):]
	}
	return into, inside
}

// highlighted is marks with DisplayHighlightMark beside them, in a slice of its own: the runs a
// text node is cut into share the node's marks, so none of them may be appended to in place.
func highlighted(marks []Mark) []Mark {
	out := make([]Mark, len(marks), len(marks)+1)
	copy(out, marks)
	return append(out, Mark{Type: DisplayHighlightMark})
}

// displayCacheKey finds one reading of one text: the SHA-256 of the markdown and the mode it was
// read in, since the two modes read the same text into different trees.
type displayCacheKey struct {
	sum  [sha256.Size]byte
	mode DisplayMode
}

// displayCache is the process's one display-tree cache, held for the process's life: a tree is
// the reading of the text its key hashes, so nothing in it ever goes stale, and an entry leaves
// only to make room for another (DisplayCacheBytes). It counts the bytes of the encodings it
// holds and nothing else, which is what a reader of DisplayCacheBytes is promised.
var displayCache = bytelru.New[displayCacheKey, json.RawMessage](DisplayCacheBytes)
