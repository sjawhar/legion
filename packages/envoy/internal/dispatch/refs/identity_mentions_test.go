package refs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func TestEditingABlockKeepsItsIdentityMentionsDateAndSource(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	id, key := createReferenceArtifact(t, tx, "", "CORE", "spec")
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, body := range []string{"See dispatch://people/alice", "Edited around dispatch://people/alice"} {
		at := first.Add(time.Duration(i) * time.Hour)
		if _, err := tx.Exec(ctx, `insert into artifact_versions(artifact_id,number,markdown,created_at) values ($1,$2,$3,$4)`, id, i+1, body, at); err != nil {
			t.Fatal(err)
		}
		if _, err := IndexBlocks(ctx, tx, id, i+1, at, body, []model.ArtifactBlock{{ID: "b1", Type: "paragraph", From: 0, To: len(body)}}, text.ExtractAt(body, ""), nil); err != nil {
			t.Fatal(err)
		}
		if err := Stamp(ctx, tx, "artifact", id, int64(50+i)); err != nil {
			t.Fatal(err)
		}
	}
	var date time.Time
	var source int64
	if err := tx.QueryRow(ctx, `select created_at,source_seq from graph_mention_edges where from_kind='block' and from_id=$1 and to_kind='person' and to_id='alice'`, key+"#b1").Scan(&date, &source); err != nil {
		t.Fatal(err)
	}
	if !date.Equal(first) || source != 50 {
		t.Fatalf("mention moved to %s / %d, want %s / 50", date, source, first)
	}
}
