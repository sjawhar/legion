package pmdoc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuin/goldmark/util"
)

// The guard's local-part table is goldmark's: a byte the guard reads as a local-part character is
// one util.FindEmailIndex reads as one, and no other.
func TestLinkifyGuardReadsGoldmarksLocalPartCharacters(t *testing.T) {
	for c := range 256 {
		goldmarks := util.FindEmailIndex([]byte{byte(c), '@', 'x', '.', 'y'}) > 0
		if goldmarks != emailLocal[c] {
			t.Errorf("byte %q: goldmark reads it as a local-part character: %v; the guard: %v", byte(c), goldmarks, emailLocal[c])
		}
	}
}

// The guard links exactly what goldmark's linkify links, and leaves text where goldmark did.
func TestLinkifyGuardLinksWhatGoldmarkLinks(t *testing.T) {
	for _, test := range []struct{ markdown, want string }{
		{"mail alice@example.com now", `text "mail " | text "alice@example.com" link mailto:alice@example.com | text " now"`},
		{"(bob@example.org)", `text "(" | text "bob@example.org" link mailto:bob@example.org | text ")"`},
		{"a_a_a@b.c", `text "a_a_a@b.c" link mailto:a_a_a@b.c`},
		{"a_a_a@b.c_", `text "a_a_a@b.c_"`},
		{"x@y and a@@b.c", `text "x@y and a@@b.c"`},
		{"see https://example.com/a_b and www.example.org/x", `text "see " | text "https://example.com/a_b" link https://example.com/a_b | text " and " | text "www.example.org/x" link http://www.example.org/x`},
		{"@start of a line", `text "@start of a line"`},
		{"end.@dot", `text "end.@dot"`},
	} {
		doc, err := Parse(test.markdown)
		if err != nil {
			t.Fatalf("%q: %v", test.markdown, err)
		}
		if got := inlineSummary(doc.Children[0]); got != test.want {
			t.Errorf("%q:\n got %s\nwant %s", test.markdown, got, test.want)
		}
	}
}

// literalURL answers a URL at each offset of a line as a fresh read would, through the two records
// that keep a line linear: the domain read from an earlier trigger (literalDomains), which a www
// literal starting inside it reads again with its own underscores and dots, and a trail run
// already found to continue the URL (literalTrail).
func TestLiteralURLReadsEachOffsetAsAFreshRead(t *testing.T) {
	for _, test := range []struct {
		line string
		want map[int]int // offset → the URL's length, 0 for none; offsets not listed are checked against a fresh read only
	}{
		{"_www.a_www.a_www.ab", map[int]int{1: 0, 7: 0, 13: 6}},
		{"www.a_b.c.d www.d", map[int]int{0: 11, 12: 5}},
		{"www.x_y.z.com/q", map[int]int{0: 15}},
		{"www.a.b_c.d", map[int]int{0: 0}},
		{"https://x.test/a)\"; b", map[int]int{0: 16}},
		{"https://x.test/(a)) b", map[int]int{0: 18}},
		{"https://x.test/a&amp;&amp;) b", map[int]int{0: 16}},
		{"https://x.test/a&amp;x b", map[int]int{0: 22}},
		{"https://x.test/a.,;:!?'\"_~*)x b", map[int]int{0: 29}},
		{"https://x.test/a.,;:!?'\"_~*) b", map[int]int{0: 16}},
		{"https://x.test/a](b) https://x.test/c]x", map[int]int{0: 16, 21: 18}},
		{"HTTPS://X.TEST/Y www.", map[int]int{0: 16, 17: 0}},
	} {
		line := []byte(test.line)
		shared := &literalDomains{from: -1, end: -1}
		for offset := range len(line) {
			if !literalURLPrefix(line[offset:]) {
				continue
			}
			shared.base = offset
			got, _, _ := literalURL(line[offset:], shared)
			fresh, _, _ := literalURL(line[offset:], &literalDomains{from: -1, end: -1, base: offset})
			if got != fresh {
				t.Errorf("%q at %d: %d with the line's record, %d read fresh", test.line, offset, got, fresh)
			}
			if want, ok := test.want[offset]; ok && got != want {
				t.Errorf("%q at %d: length %d, want %d", test.line, offset, got, want)
			}
		}
	}
}

func inlineSummary(paragraph *Node) string {
	var parts []string
	for _, node := range paragraph.Children {
		part := fmt.Sprintf("%s %q", node.Type, node.Text)
		for _, mark := range node.Marks {
			if mark.Type == "link" {
				part += " link " + mark.Attrs["href"].(string)
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
