package pmdoc

import "testing"

func TestFixturesAreWithinSchema(t *testing.T) {
	fixtures := loadFixtures(t)
	if len(fixtures) < 10 {
		t.Fatalf("expected the interim corpus, got %d fixtures", len(fixtures))
	}
	for _, fx := range fixtures {
		if _, err := FromJSON(fx.PMJSON); err != nil {
			t.Errorf("%s: %v", fx.Name, err)
		}
	}
}
