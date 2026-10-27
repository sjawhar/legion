package pmdoc

import (
	"strings"
	"testing"
)

// Go reads an email in each of these shapes as the browser editor's engine reads it - from its
// tokenizer, or from its transform over the decoded text after an escape, a character reference, a
// quote, a letter outside ASCII or other punctuation - and keeps it through a write: the rendering
// of Go's reading reads back as that reading in Go and in the engine. The first five Go read
// otherwise than the engine before INFRA-531, and the first two it read back from its own rendering
// as a link it had not read. Go writes an email's href bare, the engine `mailto:`, so a reading is
// compared with the engine's with `mailto:` taken off every href on both sides (withoutMailtoHrefs):
// the text, the marks, and where each link is and the address it holds. INFRA-530 makes the hrefs
// equal; these rows then move into its email table, which compares whole trees.
func TestEmailShapesReadAsTheEngineReadsThemAndSurviveARender(t *testing.T) {
	markdowns := []string{
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
	}
	docs := make([]*Node, len(markdowns))
	rendered := make([]string, len(markdowns))
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		if rendered[index], err = Render(doc); err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
		}
		docs[index] = doc
	}
	readings := engineReadings(t, append(append([]string(nil), markdowns...), rendered...))
	for index, markdown := range markdowns {
		doc := withoutMailtoHrefs(t, docs[index])
		if engine := readings[index]; engine == nil || !withoutMailtoHrefs(t, engine).Equal(doc) {
			t.Errorf("%q: Go reads %s, the engine %s", markdown, readingSummary(docs[index]), readingSummary(engine))
		}
		if back, err := Parse(rendered[index]); err != nil || !back.Equal(docs[index]) {
			t.Errorf("%q is written %q, which Go reads back as %s (%v), not %s", markdown, rendered[index], readingSummary(back), err, readingSummary(docs[index]))
		}
		if engine := readings[len(markdowns)+index]; engine == nil || !withoutMailtoHrefs(t, engine).Equal(doc) {
			t.Errorf("%q is written %q, which the engine reads back as %s, not %s", markdown, rendered[index], readingSummary(engine), readingSummary(docs[index]))
		}
	}
}

// withoutMailtoHrefs is a copy of doc with `mailto:` taken off the front of every link's href.
func withoutMailtoHrefs(t *testing.T, doc *Node) *Node {
	t.Helper()
	raw, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}
	copied, err := FromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	Walk(copied, func(node *Node) bool {
		for _, mark := range node.Marks {
			if href, ok := mark.Attrs["href"].(string); ok {
				mark.Attrs["href"] = strings.TrimPrefix(href, "mailto:")
			}
		}
		return true
	})
	return copied
}
