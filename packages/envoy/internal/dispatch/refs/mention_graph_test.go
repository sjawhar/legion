package refs

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestBodyIdentityMentionsSurviveOldReconciliationWithProvenance(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `insert into projects(key,name) values ('CORE','Core');
		insert into issues(key,project_key,number,title,rank,created_by) values ('CORE-1','CORE',1,'Mentions','U','{"kind":"user","id":"alice"}');
		insert into comments(id,issue_key,author,body) values ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"user","id":"bob"}','See dispatch://people/ALICE and dispatch://sessions/reader and dispatch://roles/reviewer');`)
	if err != nil {
		t.Fatal(err)
	}
	const id = "00000000-0000-0000-0000-000000000001"
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceCountedItem(ctx, tx, ItemComment, id, "See dispatch://people/ALICE and dispatch://sessions/reader and dispatch://roles/reviewer", "https://dispatch.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Stamp(ctx, tx, "comment", id, 42); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	read := func() []string {
		rows, err := database.Pool.Query(ctx, `select to_kind || ':' || to_id from `+AllEdgesSQL+` where from_kind='comment' and from_id=$1 and kind='mentions'`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			got = append(got, value)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	want := []string{"person:alice", "role:reviewer", "session:reader"}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("identity edges = %v, want %v", got, want)
	}
	// The old parser finds no identity address. Its exact reconciliation deletes all refs
	// absent from that extraction; it does not mark an unchanged item owed again.
	tx, err = database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `delete from refs where from_kind='comment' and from_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("old reconciliation lost identity mentions: %v", got)
	}
	if _, err := RebuildAll(ctx, database.Pool, "https://dispatch.test"); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuild restored %v, want %v", got, want)
	}
	var unstamped int
	if err := database.Pool.QueryRow(ctx, `select count(*) from identity_mentions where owner_kind='comment' and owner_id=$1 and source_seq is distinct from 42`, id).Scan(&unstamped); err != nil || unstamped != 0 {
		t.Fatalf("old reconciliation/rebuild lost provenance: %d, %v", unstamped, err)
	}
}
