package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/searchtest"
)

// A write whose text search_vector cuts to fit logs one WARN line naming the cut: the text's
// length, how much of it the vector holds and the limit, never the text. A write that fits logs
// nothing. Before 0090 the cut was silent, and before store.Open logged notices Postgres's warning
// went nowhere.
func TestATruncatingWriteLogsOneWarning(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedSearchDocument(t, ctx, store)
	logs := captureLogs(t)

	text := searchtest.DistinctWords(100_000, "\n")
	if _, err := store.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, $1, '[]' from artifacts`, text); err != nil {
		t.Fatalf("write text past the search vector limit: %v", err)
	}
	var warnings []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level=WARN") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("the truncating write logged %d WARN lines, want 1:\n%s", len(warnings), logs.String())
	}
	warning := warnings[0]
	for _, want := range []string{"dispatch: postgres notice", "severity=WARNING", "a text of 800000 characters to its first", "1048575-byte limit"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the warning %q lacks %q", warning, want)
		}
	}
	if strings.Contains(warning, "w000001") {
		t.Errorf("the warning %q holds the text", warning)
	}

	logs.Reset()
	if _, err := store.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) select id, 2, 'Calibrate the astrolabes.', '[]' from artifacts`); err != nil {
		t.Fatalf("write text that fits: %v", err)
	}
	if logged := logs.String(); logged != "" {
		t.Errorf("a write that fits logged:\n%s", logged)
	}
}
