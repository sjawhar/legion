package pmdoc

import "testing"

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
			if !StripAnchorMarks(doc).Equal(back) {
				g, _ := back.JSON()
				w, _ := StripAnchorMarks(doc).JSON()
				t.Fatalf("round trip differs\n got: %s\nwant: %s\nmd:\n%s", g, w, md)
			}
		})
	}
}
