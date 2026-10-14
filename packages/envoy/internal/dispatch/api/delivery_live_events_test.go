package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

const (
	liveUser    = `{"kind":"user","id":"alice"}`
	liveSession = `{"kind":"session","id":"session-0123456789abcdef","origin":{"session_title":"Plan the widgets"}}`
)

// liveEventsSince carries a payload for a type the hub reads or an event a session wrote, and
// none for any other: an unwanted type by a user comes back with no payload, and through/at name
// the last row scanned, whether or not it came back.
func TestLiveEventsSinceCarriesOnlyTheWantedPayloads(t *testing.T) {
	database := storetest.Open(t)
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.updated", actor: liveUser, payload: `{"issue":{"key":"LIVE-1","priority":0}}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{"version":2}`, at: base.Add(time.Second)},
		{kind: "comment.created", actor: liveSession, payload: `{"body":"hello"}`, at: base.Add(2 * time.Second)},
		{kind: "architecture.synced", actor: `{"kind":"system","id":"architecture-importer"}`, payload: `{"commit":"abc"}`, at: base.Add(3 * time.Second)},
		{kind: "comment.created", actor: liveUser, payload: `{"body":"a human's"}`, at: base.Add(4 * time.Second)},
	})

	events, through, at, err := liveEventsSince(context.Background(), database.Pool, 0, 100)
	if err != nil {
		t.Fatalf("liveEventsSince: %v", err)
	}
	if through != ids[4] || !at.Equal(base.Add(4*time.Second)) {
		t.Fatalf("through, at = %d, %v; want the last row scanned, %d at %v", through, at, ids[4], base.Add(4*time.Second))
	}
	var got []int64
	for _, event := range events {
		got = append(got, event.id)
	}
	if want := []int64{ids[0], ids[2], ids[3]}; !slices.Equal(got, want) {
		t.Fatalf("events returned = %v, want the issue type, the session's and the architecture sync, %v", got, want)
	}
	updated, session, synced := events[0], events[1], events[2]
	if updated.kind != "issue.updated" || updated.issueKey != "LIVE-1" || updated.actor != (model.Actor{Kind: "user", ID: "alice"}) ||
		!updated.at.Equal(base) || !jsonEqual(t, updated.payload, `{"issue":{"key":"LIVE-1","priority":0}}`) {
		t.Fatalf("issue.updated = %+v, want its type, issue, actor, time and payload", updated)
	}
	if session.kind != "comment.created" || session.actor.Kind != "session" || session.actor.ID != "session-0123456789abcdef" ||
		session.actor.Origin == nil || session.actor.Origin.SessionTitle != "Plan the widgets" || !jsonEqual(t, session.payload, `{"body":"hello"}`) {
		t.Fatalf("a session's comment.created = %+v (origin %+v), want its actor with its session title and its payload", session, session.actor.Origin)
	}
	if synced.kind != "architecture.synced" || !jsonEqual(t, synced.payload, `{"commit":"abc"}`) {
		t.Fatalf("architecture.synced = %+v, want its payload", synced)
	}
}

// A page whose rows are all unwanted returns none, and through/at still move to its last row; past
// the newest event the read returns nothing with through at the cursor and at zero.
func TestLiveEventsSinceMovesPastARunItIgnores(t *testing.T) {
	database := storetest.Open(t)
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base},
		{kind: "ask.answered", actor: liveUser, payload: `{}`, at: base.Add(time.Second)},
		{kind: "comment.created", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Second)},
	})

	events, through, at, err := liveEventsSince(context.Background(), database.Pool, 0, 100)
	if err != nil {
		t.Fatalf("liveEventsSince: %v", err)
	}
	if len(events) != 0 || events == nil {
		t.Fatalf("events = %#v, want an empty, non-nil list", events)
	}
	if through != ids[2] || !at.Equal(base.Add(2*time.Second)) {
		t.Fatalf("through, at = %d, %v; want the last ignored row, %d at %v", through, at, ids[2], base.Add(2*time.Second))
	}

	events, through, at, err = liveEventsSince(context.Background(), database.Pool, ids[2], 100)
	if err != nil {
		t.Fatalf("liveEventsSince past the newest: %v", err)
	}
	if len(events) != 0 || through != ids[2] || !at.IsZero() {
		t.Fatalf("past the newest event: events %v, through %d, at %v; want none, the cursor %d, and zero", events, through, at, ids[2])
	}
}

// limit bounds the rows scanned, not the rows returned, and the next read starts after through.
func TestLiveEventsSinceScansAtMostLimitRows(t *testing.T) {
	database := storetest.Open(t)
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{"n":0}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base.Add(time.Second)},
		{kind: "issue.claimed", actor: liveUser, payload: `{"n":2}`, at: base.Add(2 * time.Second)},
		{kind: "issue.released", actor: liveUser, payload: `{"n":3}`, at: base.Add(3 * time.Second)},
		{kind: "issue.closed", actor: liveUser, payload: `{"n":4}`, at: base.Add(4 * time.Second)},
	})

	var pages [][]int64
	for cursor := ids[0] - 1; ; {
		events, through, _, err := liveEventsSince(context.Background(), database.Pool, cursor, 2)
		if err != nil {
			t.Fatalf("liveEventsSince(%d, 2): %v", cursor, err)
		}
		if through == cursor {
			break
		}
		page := []int64{}
		for _, event := range events {
			page = append(page, event.id)
		}
		pages = append(pages, page)
		cursor = through
	}
	want := [][]int64{{ids[0]}, {ids[2], ids[3]}, {ids[4]}}
	if !slices.EqualFunc(pages, want, slices.Equal[[]int64]) {
		t.Fatalf("pages of two rows = %v, want %v", pages, want)
	}
}

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

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("payload %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want %q is not JSON: %v", want, err)
	}
	gotJSON, _ := json.Marshal(a)
	wantJSON, _ := json.Marshal(b)
	return string(gotJSON) == string(wantJSON)
}
