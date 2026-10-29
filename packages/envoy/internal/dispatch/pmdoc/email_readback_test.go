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

// Go reads an email in each of these shapes as the browser editor's engine reads it, whole tree
// for whole tree, and keeps it through a write: the rendering of Go's reading reads back as that
// reading in Go and in the engine. The shapes are where an address could start or end otherwise:
// after a scheme, a `;`, a `^`, a `_`, a quote, a letter outside ASCII, a character reference or an
// escape; at a `www.` opening the local part, at a `'` or `!` in the local part, in a domain
// ending in a digit or an underscore, and inside or after a mark - read from the engine's
// tokenizer or from its transform over the decoded text.
//
// The transform splits each text of the `.@y.zz` rows at an email, in a paragraph and in each
// block that holds one, and searches each text once, so the text beside the email is not searched
// again as a text of its own. Written as an explicit link, the email ends the text before it and
// begins the one after it, and both readers link an address that text opens with alone (`.@y.zz`),
// so the writer writes the email as its address there (runSpelling.bareEmails).
func TestEmailReadingsAgreeWithTheEngine(t *testing.T) {
	markdowns := []string{
		"Mail mailto:a@b.test now.\n",
		"Mail xmpp:a@b.test now.\n",
		"x;a@b.test end\n",
		"www.a@b.test\n",
		"Mail a'b@c.test now.\n",
		"Mail a!b@c.test now.\n",
		"Mail a@b.test1 now.\n",
		"Mail a@b.test_ end\n",
		"Mail a@b end\n",
		"_a@b.test_\n",
		"a\\@b.test\n",
		"\"a@b.test\"\n",
		"a@-b.test\n",
		"\\<a@b.test>\n",
		"\\*a@b.test c\n",
		"\"a@b.test\" c\n",
		"éa@b.test c\n",
		"x&amp;a@b.test c\n",
		"see www\\.a@b.test\n",
		"x;mailto:a@b.test end\n",
		"x^a@b.test end\n",
		"x ^a@b.test end\n",
		"x_a@b.test end\n",
		"x _a@b.test end\n",
		"*a@b.test*\n",
		"x *a@b.test* y\n",
		"a~b~x@y.co\n",
		"~~a c~~x@y.co\n",
		"a\\_b@c.dd\\.@y.zz\n",
		"- a\\_b@c.dd\\.@y.zz\n",
		"> a\\_b@c.dd\\.@y.zz\n",
		"| a |\n| - |\n| a\\_b@c.dd\\.@y.zz |\n",
		"x /www.x.test.a_b@c.dd y\n",
	}
	docs := make([]*Node, len(markdowns))
	renderings := make([]string, len(markdowns))
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		docs[index] = doc
		renderings[index] = mustRender(t, doc)
	}
	readings := engineReadings(t, append(append([]string{}, markdowns...), renderings...))
	for index, markdown := range markdowns {
		doc, rendering := docs[index], renderings[index]
		if reading := readings[index]; reading == nil || !doc.Equal(reading) {
			t.Errorf("%q: Go reads %s, the engine %s", markdown, readingSummary(doc), readingSummary(reading))
		}
		if back, err := Parse(rendering); err != nil || !back.Equal(doc) {
			t.Errorf("%q is written %q, which Go reads back as %s (%v), not %s", markdown, rendering, readingSummary(back), err, readingSummary(doc))
		}
		if reading := readings[len(markdowns)+index]; reading == nil || !reading.Equal(doc) {
			t.Errorf("%q is written %q, which the engine reads back as %s, not %s", markdown, rendering, readingSummary(reading), readingSummary(doc))
		}
	}
}
