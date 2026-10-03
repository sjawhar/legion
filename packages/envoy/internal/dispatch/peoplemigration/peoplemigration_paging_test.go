package peoplemigration

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Run reads each table with JSON person columns a page at a time. A table holds more rows than one
// page, keyed by numbers whose digits sort differently from their values ("10000" before "9"), and
// every row has to move whichever way its key's digits fall.
func TestRunMovesEveryEventPastTheFirstPage(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	rows := 3 * actorPage
	if _, err := database.Pool.Exec(ctx, `
		insert into events (seq, type, actor, payload, notify)
		select n, 'user_state.updated', '{"kind":"user","id":"Ada-Example"}', '{"login":"ada-example","state":{"pinned":true}}', false
		from generate_series(1, $1::int) n
	`, rows); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	out, err := migrate(t, database, examplePeople)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from events where lower(actor->>'id') = 'ada-example' or payload->>'login' = 'ada-example'
	`).Scan(&left); err != nil {
		t.Fatalf("count events left: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d of %d events still name ada-example after a run that exited 0 and reported:\n%s", left, rows, out)
	}
}
