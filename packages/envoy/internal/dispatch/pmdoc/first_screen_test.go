package pmdoc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The first screen's two renderers are held to one record of blocks: this row draws each fixture
// document through the server's renderer, and `web/src/features/doc/first-screen-fixtures.test.ts`
// draws the same documents through the page's own preview and writes what it drew into
// testdata/first-screen.json. A document the first screen carries whole must match that record
// exactly; one it cuts must match its opening (dispatch://INFRA-341, R11, acceptance A11).
//
// What is compared is the text of each top-level block and the order they stand in, not their
// ids: the page's preview mints ids of its own as it parses, while this renderer draws the ids
// the live tree holds, so the two can never spell one id and must spell one document. That the
// server's own blocks each carry a live id is this package's to prove, and the row below does.

// firstScreenBlock is one drawn block as a test reads it back out of the HTML.
type firstScreenBlock struct {
	ID   string
	Text string
}

type firstScreenFixture struct {
	Name     string          `json:"name"`
	Markdown string          `json:"markdown"`
	PMJSON   json.RawMessage `json:"pm_json"`
	Blocks   []string        `json:"blocks"`
}

func loadFirstScreenFixtures(t *testing.T) []firstScreenFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "first-screen.json"))
	if err != nil {
		t.Fatalf("read first-screen fixtures: %v", err)
	}
	var fixtures []firstScreenFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode first-screen fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("first-screen fixtures are empty")
	}
	return fixtures
}

// blocksOfHTML is every identified block of rendered, in document order, with the text its
// element holds: what `document.querySelectorAll("[data-block-id]")` and `textContent` answer on
// the page.
func blocksOfHTML(t *testing.T, rendered string) []firstScreenBlock {
	t.Helper()
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(rendered), context)
	if err != nil {
		t.Fatalf("parse rendered first screen: %v", err)
	}
	blocks := make([]firstScreenBlock, 0, FirstScreenBlocks)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			for _, attribute := range node.Attr {
				if attribute.Key == "data-block-id" {
					blocks = append(blocks, firstScreenBlock{ID: attribute.Val, Text: textOf(node)})
					break
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	return blocks
}

func textOf(node *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			out.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return out.String()
}

// elementsOfHTML is every `tag` element of rendered, in document order.
func elementsOfHTML(t *testing.T, rendered, tag string) []*html.Node {
	t.Helper()
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(rendered), context)
	if err != nil {
		t.Fatalf("parse rendered first screen: %v", err)
	}
	found := []*html.Node{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == tag {
			found = append(found, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	return found
}

// topLevelCount is how many top-level elements rendered holds, which is what the first screen's
// block budget counts.
func topLevelCount(t *testing.T, rendered string) int {
	t.Helper()
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(rendered), context)
	if err != nil {
		t.Fatalf("parse rendered first screen: %v", err)
	}
	count := 0
	for _, node := range nodes {
		if node.Type == html.ElementNode {
			count++
		}
	}
	return count
}

// topLevelBlocksOfHTML is each top-level element of rendered, in order, with the live block id it
// carries and the text it holds.
func topLevelBlocksOfHTML(t *testing.T, rendered string) []firstScreenBlock {
	t.Helper()
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(rendered), context)
	if err != nil {
		t.Fatalf("parse rendered first screen: %v", err)
	}
	blocks := []firstScreenBlock{}
	for _, node := range nodes {
		if node.Type != html.ElementNode {
			continue
		}
		block := firstScreenBlock{Text: textOf(node)}
		for _, attribute := range node.Attr {
			if attribute.Key == "data-block-id" {
				block.ID = attribute.Val
			}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func TestFirstScreenDrawsTheBlocksThePageDraws(t *testing.T) {
	for _, fixture := range loadFirstScreenFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			tree, err := FromJSON(fixture.PMJSON)
			if err != nil {
				t.Fatalf("decode fixture tree: %v", err)
			}
			rendered, complete := FirstScreenHTML(tree)
			drawn := topLevelBlocksOfHTML(t, rendered)
			if complete {
				if len(drawn) != len(fixture.Blocks) {
					t.Fatalf("a whole document drew %d blocks, the page draws %d", len(drawn), len(fixture.Blocks))
				}
			} else if len(drawn) == 0 || len(drawn) >= len(fixture.Blocks) {
				t.Fatalf("a cut document drew %d blocks of the page's %d", len(drawn), len(fixture.Blocks))
			}
			for index, block := range drawn {
				if block.Text != fixture.Blocks[index] {
					t.Fatalf("block %d reads %q, the page draws %q", index, block.Text, fixture.Blocks[index])
				}
				if block.ID == "" {
					t.Fatalf("block %d carries no live block id: %q", index, block.Text)
				}
			}
		})
	}
}

func TestFirstScreenCutsAtItsBudget(t *testing.T) {
	fixtures := loadFirstScreenFixtures(t)
	var many firstScreenFixture
	for _, fixture := range fixtures {
		if fixture.Name == "many-blocks" {
			many = fixture
		}
	}
	if many.Name == "" {
		t.Fatal("no many-blocks fixture")
	}
	tree, err := FromJSON(many.PMJSON)
	if err != nil {
		t.Fatalf("decode fixture tree: %v", err)
	}
	rendered, complete := FirstScreenHTML(tree)
	if complete {
		t.Fatal("a document of 41 top-level blocks was drawn whole")
	}
	if count := topLevelCount(t, rendered); count != FirstScreenBlocks {
		t.Fatalf("the first screen drew %d top-level blocks, not %d", count, FirstScreenBlocks)
	}
	if len(rendered) > FirstScreenBytes {
		t.Fatalf("the first screen is %d bytes, over the %d it may carry", len(rendered), FirstScreenBytes)
	}
}

func TestFirstScreenCutsAtItsByteBudget(t *testing.T) {
	doc := &Node{Type: "doc"}
	for index := range 20 {
		doc.Children = append(doc.Children, &Node{
			Type:     "paragraph",
			Attrs:    Attrs{BlockIDAttr: fmt.Sprintf("b-%03d", index)},
			Children: []*Node{{Type: "text", Text: strings.Repeat("x", 2_000)}},
		})
	}
	rendered, complete := FirstScreenHTML(doc)
	if complete {
		t.Fatal("40,000 characters of paragraphs were drawn whole")
	}
	if len(rendered) > FirstScreenBytes {
		t.Fatalf("the first screen is %d bytes, over the %d it may carry", len(rendered), FirstScreenBytes)
	}
	if count := topLevelCount(t, rendered); count == 0 || count >= 20 {
		t.Fatalf("the byte budget drew %d of 20 paragraphs", count)
	}
}

// A document whose opening block alone is longer than the whole budget still draws it: a reader
// with no first block has no first screen at all.
func TestFirstScreenDrawsAnOversizedOpeningBlock(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{
		{
			Type:     "paragraph",
			Attrs:    Attrs{BlockIDAttr: "b-001"},
			Children: []*Node{{Type: "text", Text: strings.Repeat("x", FirstScreenBytes*2)}},
		},
		{
			Type:     "paragraph",
			Attrs:    Attrs{BlockIDAttr: "b-002"},
			Children: []*Node{{Type: "text", Text: "the second"}},
		},
	}}
	rendered, complete := FirstScreenHTML(doc)
	if complete {
		t.Fatal("a document of two paragraphs was drawn whole")
	}
	if blocks := blocksOfHTML(t, rendered); len(blocks) != 1 || blocks[0].ID != "b-001" {
		t.Fatalf("the oversized opening block was not drawn alone: %+v", blocks)
	}
}

// Everything the HTML carries is a person's document, so nothing a person writes may close an
// element, open an attribute of its own, or reach the page as a scheme a browser runs.
func TestFirstScreenEscapesWhatAPersonWrote(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{
		{
			Type:  "paragraph",
			Attrs: Attrs{BlockIDAttr: `"><script>alert(1)</script>`},
			Children: []*Node{
				{Type: "text", Text: `</p><script>alert("x")</script> & <b>`},
				{
					Type:  "text",
					Text:  "a link",
					Marks: []Mark{{Type: "link", Attrs: Attrs{"href": `" onmouseover="alert(1)`}}},
				},
			},
		},
	}}
	rendered, _ := FirstScreenHTML(doc)
	if strings.Contains(rendered, "<script") {
		t.Fatalf("the first screen carries a script tag: %s", rendered)
	}
	for _, anchor := range elementsOfHTML(t, rendered, "a") {
		for _, attribute := range anchor.Attr {
			if attribute.Key != "href" {
				t.Fatalf("the anchor carries %q=%q, which the document wrote", attribute.Key, attribute.Val)
			}
		}
	}
	blocks := blocksOfHTML(t, rendered)
	if len(blocks) != 1 {
		t.Fatalf("expected one block, got %+v", blocks)
	}
	if blocks[0].ID != `"><script>alert(1)</script>` {
		t.Fatalf("the block id did not survive escaping: %q", blocks[0].ID)
	}
	if want := `</p><script>alert("x")</script> & <b>a link`; blocks[0].Text != want {
		t.Fatalf("the text is %q, want %q", blocks[0].Text, want)
	}
}

// The first screen keeps the link and picture schemes the page keeps (`sanitizeLinkHref`) and
// drops every other, so a `javascript:` address reaches the reader as no address at all.
func TestFirstScreenKeepsOnlyTheSchemesThePageKeeps(t *testing.T) {
	for _, test := range []struct{ href, want string }{
		{"https://example.test/a", "https://example.test/a"},
		{"http://example.test/a", "http://example.test/a"},
		{"mailto:someone@example.test", "mailto:someone@example.test"},
		{"tel:+15550100", "tel:+15550100"},
		{"ftp://example.test/a", "ftp://example.test/a"},
		{"/issues/CORE-1", "/issues/CORE-1"},
		{"../a/relative/path", "../a/relative/path"},
		{"javascript:alert(1)", ""},
		{"JaVaScRiPt:alert(1)", ""},
		{"  java\tscript:alert(1)  ", ""},
		{"data:text/html;base64,PHNjcmlwdD4=", ""},
		{"vbscript:msgbox(1)", ""},
	} {
		doc := &Node{Type: "doc", Children: []*Node{{
			Type:  "paragraph",
			Attrs: Attrs{BlockIDAttr: "b-001"},
			Children: []*Node{{
				Type:  "text",
				Text:  "a link",
				Marks: []Mark{{Type: "link", Attrs: Attrs{"href": test.href}}},
			}},
		}}}
		rendered, _ := FirstScreenHTML(doc)
		anchors := elementsOfHTML(t, rendered, "a")
		if len(anchors) != 1 {
			t.Fatalf("%q drew %d anchors", test.href, len(anchors))
		}
		got := ""
		for _, attribute := range anchors[0].Attr {
			if attribute.Key == "href" {
				got = attribute.Val
			}
		}
		if got != test.want {
			t.Fatalf("%q reached the page as %q, want %q", test.href, got, test.want)
		}
	}
}

// A picture under a scheme the page refuses reaches it with no source, and a `dispatch://`
// picture as the empty anchor the page's own serializer draws in its place.
func TestFirstScreenPictureSources(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type:  "paragraph",
		Attrs: Attrs{BlockIDAttr: "b-001"},
		Children: []*Node{
			{Type: "image", Attrs: Attrs{"src": "javascript:alert(1)", "alt": "", "title": ""}},
			{Type: "image", Attrs: Attrs{"src": "dispatch://CORE-1/artifact/a-png", "alt": "", "title": ""}},
			{Type: "image", Attrs: Attrs{"src": "https://example.test/a.png", "alt": "a", "title": ""}},
		},
	}}}
	rendered, _ := FirstScreenHTML(doc)
	pictures := elementsOfHTML(t, rendered, "img")
	if len(pictures) != 2 {
		t.Fatalf("expected two pictures, got %d: %s", len(pictures), rendered)
	}
	sources := make([]string, 0, 2)
	for _, picture := range pictures {
		for _, attribute := range picture.Attr {
			if attribute.Key == "src" {
				sources = append(sources, attribute.Val)
			}
		}
	}
	if sources[0] != "" || sources[1] != "https://example.test/a.png" {
		t.Fatalf("the pictures carry %q", sources)
	}
	if anchors := elementsOfHTML(t, rendered, "a"); len(anchors) != 1 {
		t.Fatalf("the dispatch picture drew %d anchors: %s", len(anchors), rendered)
	}
}

func TestFirstScreenOfNoDocument(t *testing.T) {
	rendered, complete := FirstScreenHTML(nil)
	if rendered != "" || complete {
		t.Fatalf("a nil document drew %q (complete %v)", rendered, complete)
	}
}
