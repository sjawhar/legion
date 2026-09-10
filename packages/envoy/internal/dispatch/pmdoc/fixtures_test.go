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

func TestFixturesAreWithinSchema(t *testing.T) {
	fixtures := loadFixtures(t)
	wantNames := []string{
		"bare-url", "blockquote", "code-fence", "code", "emphasis", "empty", "escapes",
		"explicit-url-space", "explicit-url-title", "footnote", "frontmatter", "headings", "hr",
		"html", "image-delimiters", "image", "inline-code-backticks", "inline-code-spaces",
		"link-delimiters", "links", "lists", "long", "marks", "nested-code", "ordered-list-prefixes",
		"paragraphs", "softbreak", "table", "tasks", "unicode",
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
