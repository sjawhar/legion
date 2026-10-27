package pmdoc

import (
	"strings"
	"testing"
)

// inlineSummaries is inlineSummary of each block in doc that holds inline content, in document
// order, so a reading in a table cell shows as one in a paragraph does.
func inlineSummaries(doc *Node) string {
	if doc == nil {
		return "nothing"
	}
	var summaries []string
	Walk(doc, func(node *Node) bool {
		if len(node.Children) > 0 && isInlineNodeType(node.Children[0].Type) {
			summaries = append(summaries, inlineSummary(node))
		}
		return true
	})
	return strings.Join(summaries, " || ")
}

// An email autolink - a GFM literal address or a CommonMark `<address>` - is a link to the
// address's mailto: URL, as the browser editor's engine reads it, so Go's reading of a document
// holding one is the engine's. Each row is markdown holding an email autolink in a context that
// could change where it is read: a paragraph, angle brackets, a table cell, the inside of a mark,
// parentheses, uppercase letters, and a local part with punctuation before a trailing dot. Go's
// reading equals the engine's, every link in it goes to "mailto:" and its own text, and the
// rendering writes each address as a link to its mailto: URL, which reads back, in Go and in the
// engine alike, as the markdown's reading.
func TestAnEmailAutolinkIsAMailLinkInGoAndTheEngine(t *testing.T) {
	rows := []string{
		"Mail a@b.test now.\n",
		"Mail <a@b.test> now.\n",
		"| h |\n| --- |\n| a@b.test |\n",
		"Mail *a@b.test* end\n",
		"Mail (a@b.test) end\n",
		"Mail A@B.TEST end\n",
		"Mail a.b-c+d@x-y.test. end\n",
	}
	docs := make([]*Node, len(rows))
	renderings := make([]string, len(rows))
	for index, markdown := range rows {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		docs[index] = doc
		renderings[index] = mustRender(t, doc)
	}
	readings := engineReadings(t, append(append([]string{}, rows...), renderings...))
	for index, markdown := range rows {
		doc, rendering := docs[index], renderings[index]
		if reading := readings[index]; reading == nil || !doc.Equal(reading) {
			t.Errorf("%q: Go reads %s, the engine %s", markdown, inlineSummaries(doc), inlineSummaries(reading))
		}
		var addresses []string
		Walk(doc, func(node *Node) bool {
			for _, mark := range node.Marks {
				if mark.Type != "link" {
					continue
				}
				addresses = append(addresses, node.Text)
				if href := mark.Attrs["href"]; href != "mailto:"+node.Text {
					t.Errorf("%q: the link on %q goes to %v, want %q", markdown, node.Text, href, "mailto:"+node.Text)
				}
			}
			return true
		})
		if len(addresses) == 0 {
			t.Errorf("%q: Go reads no link: %s", markdown, inlineSummaries(doc))
		}
		// The label carries the address's other marks, so the rendering is checked for the
		// destination; the reading back below checks the label.
		for _, address := range addresses {
			if want := "](mailto:" + address + ")"; !strings.Contains(rendering, want) {
				t.Errorf("%q: Render = %q, which does not link to %q", markdown, rendering, "mailto:"+address)
			}
		}
		back, err := Parse(rendering)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", rendering, err)
		}
		if !back.Equal(doc) {
			t.Errorf("%q: its rendering %q reads in Go as %s, not as %s", markdown, rendering, inlineSummaries(back), inlineSummaries(doc))
		}
		if reading := readings[len(rows)+index]; reading == nil || !reading.Equal(doc) {
			t.Errorf("%q: its rendering %q reads in the engine as %s, not as %s", markdown, rendering, inlineSummaries(reading), inlineSummaries(doc))
		}
	}
}

// Go and the browser editor's engine still disagree on where an email address starts or ends
// beside some characters: after a scheme or a `;`, at a `www.` opening the local part, at a `'` or
// `!` in the local part, and in a domain ending in a digit. Each row is markdown and whether Go
// and the engine read it alike, so a change to either reading shows here. The rows recorded as
// differing are left to dispatch://INFRA-531 by the architect's decision D1 on INFRA-530
// (dispatch://INFRA-530/comment/92c28ada-e835-4632-acdf-554c395b8d9f), which turns them to agree.
func TestEmailReadingsThatDifferAreRecorded(t *testing.T) {
	tests := []struct {
		markdown string
		agree    bool
	}{
		{"Mail mailto:a@b.test now.\n", false},
		{"Mail xmpp:a@b.test now.\n", false},
		{"x;a@b.test end\n", false},
		{"www.a@b.test\n", false},
		{"Mail a'b@c.test now.\n", false},
		{"Mail a!b@c.test now.\n", false},
		{"Mail a@b.test1 now.\n", false},
		{"Mail a@b.test_ end\n", true},
		{"Mail a@b end\n", true},
	}
	markdowns := make([]string, 0, len(tests))
	for _, test := range tests {
		markdowns = append(markdowns, test.markdown)
	}
	readings := engineReadings(t, markdowns)
	for index, test := range tests {
		doc, err := Parse(test.markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", test.markdown, err)
		}
		if got := readings[index] != nil && doc.Equal(readings[index]); got != test.agree {
			t.Errorf("%q: Go reads %s, the engine %s; recorded as agreeing: %v", test.markdown, inlineSummaries(doc), inlineSummaries(readings[index]), test.agree)
		}
	}
}
