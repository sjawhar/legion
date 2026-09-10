package pmdoc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type Fixture struct {
	Name               string          `json:"name"`
	Markdown           string          `json:"markdown"`
	PMJSON             json.RawMessage `json:"pm_json"`
	YjsUpdateV1B64     string          `json:"yjs_update_v1_b64"`
	RenderedByMilkdown string          `json:"rendered_by_milkdown"`
	YjsUpdateV1        []byte          `json:"-"`
}

type fixtureFile struct {
	Fixtures []Fixture `json:"fixtures"`
}

func loadFixtures(t *testing.T) []Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v (run `bun run gen` in pmdoc/gen)", err)
	}

	var fixtures []Fixture
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(trimmed, &fixtures)
	} else {
		var interim fixtureFile
		err = json.Unmarshal(trimmed, &interim)
		fixtures = interim.Fixtures
	}
	if err != nil {
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
