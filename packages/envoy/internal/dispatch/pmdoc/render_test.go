package pmdoc

import (
	"regexp"
	"strings"
	"testing"
)

var orderedMarker = regexp.MustCompile(`(?m)^\d+\.\s*`)
var spanTag = regexp.MustCompile(`</?span(?:\s[^>]*)?>`)
var linkDestination = regexp.MustCompile(`\]\([^)]*\)`)

func TestRenderMatchesMilkdownForFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if plain(got) != plain(fx.RenderedByMilkdown) {
				t.Fatalf("text content differs\n got: %q\nwant: %q", got, fx.RenderedByMilkdown)
			}
		})
	}
}

func TestPositionMapRoundTripsTextRuns(t *testing.T) {
	doc, err := FromJSON([]byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Some "},{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" 😀 end"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	md, pm, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if md != "Some **bold** 😀 end\n" {
		t.Fatalf("md = %q", md)
	}
	if got := pm.ToPM(7); got != 6 {
		t.Fatalf("ToPM(7) = %d, want 6", got)
	}
	if got := pm.ToPM(len16("Some **bold** 😀")); got != 1+len16("Some bold 😀") {
		t.Fatalf("emoji offset mismatch: %d", got)
	}
	if got := pm.ToPM(5); got != 6 {
		t.Fatalf("ToPM(5) = %d, want 6", got)
	}
	if md0, ok := pm.ToMd(6); !ok || md0 != 7 {
		t.Fatalf("ToMd(6) = %d,%v", md0, ok)
	}
}

func plain(markdown string) string {
	markdown = orderedMarker.ReplaceAllString(markdown, "")
	markdown = spanTag.ReplaceAllString(markdown, "")
	markdown = linkDestination.ReplaceAllString(markdown, "]")
	markdown = strings.NewReplacer("*", "", "_", "", "~", "", "`", "", "#", "", ">", "", "<", "", "-", "", "|", "", "[", "", "]", "", "(", "", ")", "").Replace(markdown)
	return strings.Join(strings.Fields(markdown), " ")
}
