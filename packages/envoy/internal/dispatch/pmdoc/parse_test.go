package pmdoc

import (
	"strings"
	"testing"
)

func TestParseMatchesMilkdownForFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			got, err := Parse(fx.Markdown)
			if err != nil {
				t.Fatal(err)
			}
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				g, _ := got.JSON()
				t.Fatalf("Parse() differs from Milkdown\n got: %s\nwant: %s", g, fx.PMJSON)
			}
		})
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			md, _, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(md)
			if err != nil {
				t.Fatalf("re-parse canonical markdown: %v\n%s", err, md)
			}
			if !stripAnchorMarks(doc).Equal(back) {
				g, _ := back.JSON()
				w, _ := stripAnchorMarks(doc).JSON()
				t.Fatalf("round trip differs\n got: %s\nwant: %s\nmd:\n%s", g, w, md)
			}
		})
	}
}

func stripAnchorMarks(node *Node) *Node {
	if node == nil {
		return nil
	}
	out := &Node{Type: node.Type, Attrs: node.Attrs, Text: node.Text}
	for _, mark := range node.Marks {
		if !strings.HasPrefix(mark.Type, "proof") && mark.Type != "dispatchAsk" {
			out.Marks = append(out.Marks, mark)
		}
	}
	for _, child := range node.Children {
		clean := stripAnchorMarks(child)
		if len(out.Children) > 0 {
			previous := out.Children[len(out.Children)-1]
			if previous.Type == "text" && clean.Type == "text" && marksEqual(previous.Marks, clean.Marks) {
				previous.Text += clean.Text
				continue
			}
		}
		out.Children = append(out.Children, clean)
	}
	return out
}
