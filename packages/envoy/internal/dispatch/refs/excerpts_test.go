package refs

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A document's mentions are owed an excerpt until one is recorded at its latest version; once
// recorded, the reconciliation's drift query finds nothing, and it reads refs only through the
// partial index that holds the owed rows, never every document's mentions. A version a binary
// predating the excerpts wrote leaves its mentions' excerpts behind, and MarkStaleExcerpts makes
// them owed again without dropping the excerpt the route still serves.
func TestOwedExcerptsAreFoundThroughTheDriftIndexAndRecordedOnce(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	createReferenceProject(t, tx, "CORE")
	document, _ := createReferenceArtifact(t, tx, "", "CORE", "design")
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, 'v1', '[]')`, document); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	if _, err := replace(ctx, tx, "artifact", document, "see dispatch://CORE-1 and dispatch://CORE-2", "https://dispatch.example"); err != nil {
		t.Fatalf("index mentions: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	drifted, err := DriftedExcerpts(ctx, database.Pool, 20)
	if err != nil || len(drifted) != 1 || drifted[0] != (ExcerptDrift{FromID: document, Version: 1}) {
		t.Fatalf("drift before recording = %#v, %v; want the document at version 1", drifted, err)
	}
	excerpt := model.GraphExcerpt{BlockID: "b1", Text: "see dispatch://CORE-1"}
	if err := RecordExcerpts(ctx, database.Pool, document, 1, map[Target]model.GraphExcerpt{{Kind: "issue", ID: "CORE-1"}: excerpt}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 0 {
		t.Fatalf("drift after recording = %#v, %v; want none", drifted, err)
	}
	if stored, err := StoredExcerpts(ctx, database.Pool, "issue", "CORE-1"); err != nil || stored[document] != excerpt {
		t.Fatalf("stored = %#v, %v; want %#v", stored, err, excerpt)
	}
	if stored, _ := StoredExcerpts(ctx, database.Pool, "issue", "CORE-2"); len(stored) != 0 {
		t.Fatalf("a mention in no block stored %#v; want none", stored)
	}

	// With sequential scans priced out, the drift query must still have a plan, and it must read
	// refs through refs_excerpt_drift alone.
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `set enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "explain (costs off) "+strings.Replace(driftedExcerptsQuery, "$1", "20", 1))
	if err != nil {
		t.Fatalf("explain drift query: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if _, err := conn.Exec(ctx, `reset enable_seqscan`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if text := plan.String(); strings.Count(text, " on refs") == 0 || strings.Count(text, " on refs") != strings.Count(text, "refs_excerpt_drift on refs") {
		t.Fatalf("drift plan does not read refs through refs_excerpt_drift alone:\n%s", text)
	}

	if _, err := database.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, 'v2', '[]')`, document); err != nil {
		t.Fatalf("write version 2 as an older binary would: %v", err)
	}
	if marked, err := MarkStaleExcerpts(ctx, database.Pool); err != nil || marked != 2 {
		t.Fatalf("marked stale = %d, %v; want both mentions", marked, err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 1 || drifted[0].Version != 2 {
		t.Fatalf("drift after an older binary's version = %#v, %v; want the document at version 2", drifted, err)
	}
	if stored, _ := StoredExcerpts(ctx, database.Pool, "issue", "CORE-1"); len(stored) != 0 {
		t.Fatalf("a mention owed an excerpt serves %#v; want no stored excerpt", stored)
	}
	if marked, err := MarkStaleExcerpts(ctx, database.Pool); err != nil || marked != 0 {
		t.Fatalf("marking again = %d, %v; want none, owed rows are not stale", marked, err)
	}
}
