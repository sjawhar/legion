package pmdoc

import (
	"encoding/base64"
	"encoding/json"
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
		"ask-answered", "ask", "bare-url", "blockquote", "callout-blocks", "callout", "code-directive-example", "code-fence",
		"code", "emphasis", "empty", "escapes", "explicit-url-space", "explicit-url-title",
		"footnote", "frontmatter", "headings", "hr", "html", "image-delimiters", "image",
		"inline-code-backticks", "inline-code-spaces", "link-delimiters", "links", "lists", "long",
		"marks", "nested-code", "ordered-list-prefixes", "paragraphs", "softbreak", "table", "tasks",
		"unicode",
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
		"insert-paragraph-after-list-item-textblock",
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
