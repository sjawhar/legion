package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// dedupeKeyNamesItsEvent's test in packages/contracts/src/envelope.test.ts reads the same rows: the
// stream's MsgId and every host's dedupe answer one question, so a key either side misjudges is a
// repeat one of them drops and the other does not, or a distinct event both lose.
func TestDedupeKeyNamesTheUpstreamEvent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "dedupe-keys.json"))
	if err != nil {
		t.Fatalf("read the dedupe-key fixture: %v", err)
	}
	var fixture struct {
		Keys []struct {
			Name     string   `json:"name"`
			Envelope Envelope `json:"envelope"`
			Names    bool     `json:"names"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the dedupe-key fixture: %v", err)
	}
	if len(fixture.Keys) == 0 {
		t.Fatal("the dedupe-key fixture holds no rows")
	}
	for _, row := range fixture.Keys {
		if got := DedupeKeyNamesTheUpstreamEvent(row.Envelope); got != row.Names {
			t.Errorf("%s: DedupeKeyNamesTheUpstreamEvent = %v, want %v", row.Name, got, row.Names)
		}
	}
}
