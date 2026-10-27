package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// liveEventSeed is one event a test writes directly, oldest first.
type liveEventSeed struct {
	kind    string
	actor   string
	payload string
	at      time.Time
}

// seedLiveEvents writes each event on issue LIVE-1, oldest first, and returns the ids they took.
func seedLiveEvents(t *testing.T, database *store.Store, seeds []liveEventSeed) []int64 {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name) values ('LIVE', 'Live') on conflict do nothing`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U') on conflict do nothing`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	ids := make([]int64, 0, len(seeds))
	for _, seed := range seeds {
		var id int64
		if err := database.Pool.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select 'LIVE-1', coalesce(max(seq), 0) + 1, $1, $2::jsonb, $3::jsonb, false, $4
			from events where issue_key = 'LIVE-1'
			returning id
		`, seed.kind, seed.actor, seed.payload, seed.at).Scan(&id); err != nil {
			t.Fatalf("insert %s event: %v", seed.kind, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveUser is the actor of the events a test writes.
const liveUser = `{"kind":"user","id":"alice"}`

// eventCursorCheck reads the newest event's id and time, and the time of the event at the cursor;
// once the cursor's row is gone (a reset of the log), cursorAt is nil.
func TestEventCursorCheckReadsTheNewestEventAndTheCursorsRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	maxID, maxAt, cursorAt, err := eventCursorCheck(ctx, database.Pool, 0)
	if err != nil {
		t.Fatalf("eventCursorCheck on an empty log: %v", err)
	}
	if maxID != 0 || !maxAt.IsZero() || cursorAt != nil {
		t.Fatalf("an empty log = %d, %v, %v; want 0, zero, nil", maxID, maxAt, cursorAt)
	}

	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	maxID, maxAt, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck: %v", err)
	}
	if maxID != ids[2] || !maxAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("newest event = %d at %v, want %d at %v", maxID, maxAt, ids[2], base.Add(2*time.Minute))
	}
	if cursorAt == nil || !cursorAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("the cursor's row time = %v, want %v", cursorAt, base.Add(time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `delete from events where id = $1`, ids[1]); err != nil {
		t.Fatalf("delete the cursor's row: %v", err)
	}
	maxID, maxAt, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck once the cursor's row is gone: %v", err)
	}
	if maxID != ids[2] || !maxAt.Equal(base.Add(2*time.Minute)) || cursorAt != nil {
		t.Fatalf("with the cursor's row gone = %d, %v, %v; want %d, %v, nil", maxID, maxAt, cursorAt, ids[2], base.Add(2*time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	reset := seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	maxID, _, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[2])
	if err != nil {
		t.Fatalf("eventCursorCheck after a reset: %v", err)
	}
	if maxID != reset[0] || maxID >= ids[2] || cursorAt != nil {
		t.Fatalf("after a reset = %d, %v; want the new newest id %d below the cursor %d, and nil", maxID, cursorAt, reset[0], ids[2])
	}
}
