package pmdoc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	Name               string          `json:"name"`
	Markdown           string          `json:"markdown"`
	PMJSON             json.RawMessage `json:"pm_json"`
	YjsUpdateV1B64     string          `json:"yjs_update_v1_b64"`
	RenderedByMilkdown string          `json:"rendered_by_milkdown"`
	YjsUpdateV1        []byte          `json:"-"`
}

type spliceFixture struct {
	Name        string          `json:"name"`
	Markdown    string          `json:"markdown"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	At          string          `json:"at"`
	Point       string          `json:"point"`
	Replacement string          `json:"replacement"`
	PMJSON      json.RawMessage `json:"pm_json"`
}

type spliceFixtureFile struct {
	Schema string          `json:"schema"`
	Cases  []spliceFixture `json:"cases"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v (run `bun run gen` in pmdoc/gen)", err)
	}

	var fixtures []fixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	for i := range fixtures {
		b, err := base64.StdEncoding.DecodeString(fixtures[i].YjsUpdateV1B64)
		if err != nil {
			t.Fatalf("fixture %s: decode update: %v", fixtures[i].Name, err)
		}
		fixtures[i].YjsUpdateV1 = b
	}
	return fixtures
}

// FromJSON decodes the ProseMirror JSON the TypeScript fixture generator emits
// (the inverse of Node.JSON); only tests read documents in that shape.
func FromJSON(b []byte) (*Node, error) {
	var j jsonNode
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("pmdoc: decode json: %w", err)
	}
	n := j.toNode()
	sortNodeMarks(n)
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return n, nil
}

func (j *jsonNode) toNode() *Node {
	n := &Node{Type: j.Type, Attrs: j.Attrs, Text: j.Text}
	for _, m := range j.Marks {
		n.Marks = append(n.Marks, Mark{Type: m.Type, Attrs: m.Attrs})
	}
	for _, c := range j.Content {
		n.Children = append(n.Children, c.toNode())
	}
	return n
}

func loadSpliceFixtures(t *testing.T) []spliceFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "splices.json"))
	if err != nil {
		t.Fatalf("read splice fixtures: %v (run `bun run gen` in pmdoc/gen)", err)
	}
	var fixtureFile spliceFixtureFile
	if err := json.Unmarshal(raw, &fixtureFile); err != nil {
		t.Fatalf("decode splice fixtures: %v", err)
	}
	if fixtureFile.Schema != "pmdoc/replace-range-oracle/v1" {
		t.Fatalf("splice fixture schema = %q", fixtureFile.Schema)
	}
	return fixtureFile.Cases
}

func TestFixturesAreWithinSchema(t *testing.T) {
	fixtures := loadFixtures(t)
	wantNames := []string{
		"adjacent-lists", "ask-answered", "ask", "bare-address-after-punctuation", "bare-url-trailing-punctuation", "bare-url", "blockquote",
		"browser-list-spacing", "callout-blocks", "callout-nested", "callout", "code-directive-example", "code-fence", "code-span-line-ends",
		"code-span-tag-line-in-containers", "code", "definition-callout-holding-an-unclosed-fence",
		"definition-items-spaced-around-callouts-holding-an-unclosed-fence", "delimiter-rows-on-partly-lazy-lines", "directive-like-text",
		"emphasis-beside-another-marker", "emphasis-rule-of-three",
		"emphasis", "empty-containers", "empty-footnote-definitions-in-quoted-items", "empty-items-before-a-block-outside-their-item",
		"empty-items-holding-the-next-line", "empty-items-in-a-quote-after-indented-code", "empty", "escaped-block-markers", "escapes", "explicit-url-space",
		"explicit-url-title", "fence-blank-before-the-block-after-its-container", "fence-in-a-quote-unclosed-over-a-tab-line",
		"fence-indented-in-a-quote-over-shorter-whitespace-lines", "fence-indented-over-shorter-whitespace-lines",
		"fenced-code-whitespace-lines-short-of-the-fence-indent", "footnote-block-indents", "footnote-ending-in-a-block-in-an-item",
		"footnote-ending-in-a-block",
		"footnote-label-escapes", "footnote-opening-on-a-later-line", "footnote-placement", "footnote-spread-inside-a-quoted-item", "footnote-unreferenced",
		"footnote", "frontmatter-empty", "frontmatter-fence-whitespace", "frontmatter-long-fence", "frontmatter-opener-unclosed", "frontmatter",
		"generated-indented-code-g21-g10436-round6", "generated-indented-code-g21-g11819-round6", "generated-indented-code-g21-g1560-round6",
		"generated-indented-code-g21-g217-round6",
		"generated-indented-code-g21-g5616-round6", "generated-indented-code-g21-g6243-round6", "generated-indented-code-g21-g754-round6",
		"generated-indented-code-g21-g7991-round6", "generated-indented-code-g21-g8243-round6", "generated-indented-code-g21-g9089-round6",
		"generated-indented-code-g21-g9305-round6", "generated-indented-code-g21-g9745-round6", "generated-indented-code-g22-g11512-round6",
		"generated-indented-code-g22-g1471-round6",
		"generated-indented-code-g22-g4026-round6", "generated-indented-code-g22-g6351-round6", "generated-indented-code-g22-g6373-round6",
		"generated-indented-code-g22-g6776-round6", "generated-indented-code-g22-g6846-round6", "generated-indented-code-g22-g7356-round6",
		"generated-indented-code-g22-g7786-round6", "generated-indented-code-g22-g7832-round6", "generated-indented-code-g22-g9407-round6",
		"generated-indented-code-g22-g9654-round6",
		"generated-indented-code-g22-g9908-round6", "heading-line-breaks", "headings", "hr", "html-declarations", "html", "image-alt-line-ends",
		"image-delimiters", "image", "indented-code-after-a-closed-callout", "indented-code-tab-lines", "indented-code-trailing-whitespace-lines",
		"indented-delimiter-rows-before-setext-underlines", "inline-code-backticks", "inline-code-spaces",
		"item-callout-code-ending-in-a-short-whitespace-line", "item-callout-holding-two-paragraphs-after-an-empty-item",
		"item-callout-holding-two-paragraphs-beside-an-unreferenced-definition", "item-callout-list-ending-in-a-short-whitespace-line",
		"item-callout-paragraph-ending-in-a-blank-line", "item-callout-quote-ending-in-a-blank-line",
		"item-callouts-holding-an-unclosed-fence-before-a-lazy-line", "item-callouts-holding-an-unclosed-fence-before-flow-content",
		"item-fence-before-a-callout-after-blank-lines", "item-nested-callouts-ending-in-a-blank-line",
		"item-opening-with-an-empty-definition-line", "item-unclosed-callout-code-ending-in-a-short-whitespace-line",
		"item-unclosed-callout-paragraph-ending-in-a-blank-line", "items-spaced-around-callouts-holding-an-unclosed-fence",
		"lazy-lines-holding-only-an-html-tag", "link-delimiters", "links-holding-an-autolink", "links", "list-item-code-blank-lines",
		"list-spacing-in-containers", "lists-after-code-after-a-list", "lists-after-code-after-a-quote", "lists-opening-in-a-container-after-code",
		"lists-spaced-by-indented-code-whitespace-lines", "lists", "long",
		"marks-on-addresses-before-inline-html", "marks-on-footnote-references", "marks-on-hard-breaks", "marks-on-images", "marks-on-inline-html", "marks",
		"nested-code", "nested-empty-items", "nested-marks", "nul-characters", "ordered-list-prefixes", "outer-fence-closing-unclosed-inner-blocks",
		"overlapping-marks", "paragraphs", "quoted-callouts-holding-an-unclosed-fence", "quoted-definition-first-items", "quoted-definition-items",
		"quoted-definitions-ending-in-lists",
		"quoted-item-callout-holding-an-unclosed-fence-at-the-document-end", "quoted-item-callout-paragraph-ending-in-a-blank-line",
		"quoted-item-nested-callout-list-ending-in-a-blank-line", "quoted-item-typed-block-code-ending-in-a-blank-line",
		"quoted-item-typed-blocks-ending-in-blank-lines", "quoted-item-unclosed-callout-paragraph-ending-in-two-blank-lines",
		"quoted-items-holding-a-closed-callout", "quoted-items-spaced-around-callouts-holding-an-unclosed-fence",
		"quoted-lists-before-a-container-in-an-outer-quote", "quoted-lists-in-quoted-typed-blocks", "quoted-typed-block-list-ends",
		"quoted-unclosed-fence-at-the-document-end", "setext-underlines-after-delimiter-like-lines", "softbreak", "strikethrough-run-lengths",
		"tab-indented-list-markers", "table-cell-pipes", "table-header-rows-of-other-cell-counts", "table-then-setext-underline", "table",
		"tables-then-empty-items", "task-markers", "tasks",
		"twice-quoted-item-callout-fence-ending-in-a-blank-line", "typed-block-definitions-before-a-lone-tag-line",
		"typed-block-in-a-quote-ending-in-blank-lines", "typed-block-list-ending-in-a-line-of-spaces", "typed-block-opening-a-list-item-then-blank-lines",
		"typed-blocks-nested-32-deep", "unclosed-fences-before-the-next-block", "unicode", "writer-spells-a-flanked-letter-after-a-backslash",
	}
	if len(fixtures) != len(wantNames) {
		t.Fatalf("fixture count = %d, want %d", len(fixtures), len(wantNames))
	}
	for i, fx := range fixtures {
		if fx.Name != wantNames[i] {
			t.Fatalf("fixture %d = %q, want %q", i, fx.Name, wantNames[i])
		}
		if _, err := FromJSON(fx.PMJSON); err != nil {
			t.Errorf("%s: %v", fx.Name, err)
		}
	}
}

func TestSpliceFixturesHaveRequiredCoverage(t *testing.T) {
	fixtures := loadSpliceFixtures(t)
	wantNames := []string{
		"paragraph-inline", "paragraph-multiblock", "paragraph-list", "list-item-code",
		"list-items-paragraph", "task-list-open-checked", "task-list-open-unchecked",
		"list-items-join-inline", "table-cells-join-inline", "nested-list-item-to-parent-next-inline",
		"heading-into-paragraph-inline", "blockquote-last-paragraph-to-following-paragraph-inline",
		"open-side-deeper-than-close-side-inline", "close-side-deeper-than-open-side-inline",
		"task-list-replacement-in-plain-bullet", "heading-into-following-list-item-inline",
		"list-item-into-following-heading-inline", "blockquote-into-following-list-inline",
		"blockquote-last-paragraph-into-following-list-inline", "blockquote-list",
		"headings-paragraph", "code-blocks-paragraph", "heading-code", "code-list", "table-cell-code",
		"table-cell-inline", "table-header-into-first-body-cell-inline",
		"insert-inline-after-quote", "insert-inline-at-textblock-start", "insert-inline-at-textblock-end",
		"insert-blocks-after-quote-splits-paragraph", "insert-block-at-doc-start", "insert-block-at-doc-end",
		"insert-block-after-heading-textblock", "insert-block-before-heading-textblock",
		"insert-paragraph-after-list-item-textblock", "callout-paragraph-and-code", "callout-paragraph-and-list", "callout-paragraph-and-heading", "callout-in-list-item-code", "callout-in-blockquote-code", "nested-callout-code", "callout-across-paragraphs-code",
	}
	if len(fixtures) != len(wantNames) {
		t.Fatalf("splice fixture count = %d, want %d", len(fixtures), len(wantNames))
	}
	for index, fixture := range fixtures {
		if fixture.Name != wantNames[index] {
			t.Fatalf("splice fixture %d = %q, want %q", index, fixture.Name, wantNames[index])
		}
	}
}
