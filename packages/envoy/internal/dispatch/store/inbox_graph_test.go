package store

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

func TestInboxGraphMigrationLeavesOldGraphAndThreadIndexUnchanged(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261104090130)
	if _, err := database.Pool.Exec(ctx, `insert into messages(author,body,target) values('{"kind":"user","id":"alice"}','Existing message','session:agent')`); err != nil {
		t.Fatal(err)
	}
	report, err := census(ctx, database.Pool.Config().ConnString(), migrationsThrough(t, 20261104090131), pgmigrate.CensusOptions{})
	if err != nil || report.Refused() {
		t.Fatalf("inbox census over existing messages: %v\n%s", err, reportText(report))
	}
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
