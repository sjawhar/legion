package store

import (
	"context"
	"testing"
)

func TestInboxGraphMigrationLeavesOldGraphAndThreadIndexUnchanged(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261104090130)
	const definitions = `select pg_get_viewdef('graph_edges'),pg_get_viewdef('graph_person_edges'),pg_get_indexdef('user_threads_unread'::regclass)`
	var graph, person, index string
	if err := database.Pool.QueryRow(ctx, definitions).Scan(&graph, &person, &index); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var nextGraph, nextPerson, nextIndex string
	if err := database.Pool.QueryRow(ctx, definitions).Scan(&nextGraph, &nextPerson, &nextIndex); err != nil {
		t.Fatal(err)
	}
	if graph != nextGraph || person != nextPerson || index != nextIndex {
		t.Fatal("inbox migration changed an old reader's relation or unread index")
	}
	var observations int
	if err := database.Pool.QueryRow(ctx, `select (select count(*) from inbox_shadow_latest)+(select count(*) from inbox_shadow_days)`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observations != 0 {
		t.Fatalf("migration invented shadow evidence: %d", observations)
	}
}
