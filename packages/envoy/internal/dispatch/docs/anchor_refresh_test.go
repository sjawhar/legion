package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// commitOrderLock is the key of the advisory lock events.Broker.Append takes before it writes an
// event and holds until its transaction ends (commitOrderAdvisoryLock, events/broker.go): every
// writer of an event on the database waits for it.
const commitOrderLock int64 = 0x4449535041544348

// commitOrderedTrips is how many round trips the first connection to take the events'
// commit-order lock since mark made from that lock statement to its transaction's commit, both
// included, and whether it was seen to commit.
func (t *statementTracer) commitOrderedTrips(since int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var holder *pgx.Conn
	trips := 0
	for _, trip := range t.trips[since:] {
		if holder == nil {
			for _, statement := range trip.statements {
				if strings.Contains(statement.sql, "pg_advisory_xact_lock") && statement.names(fmt.Sprint(commitOrderLock)) {
					holder = trip.conn
					break
				}
			}
			if holder == nil {
				continue
			}
		}
		if trip.conn != holder {
			continue
		}
		trips++
		for _, statement := range trip.statements {
			if strings.EqualFold(strings.TrimSpace(statement.sql), "commit") {
				return trips, true
			}
		}
	}
	return trips, false
}

// anchorRefreshMarks is how many open anchors each document of the test carries: one ask and the
// rest comments.
const anchorRefreshMarks = 20

// anchoredRow is one open ask or comment anchored on a mark of the test's document.
type anchoredRow struct {
	id, quote string
	ask       bool
}

func (row anchoredRow) table() string {
	if row.ask {
		return "asks"
	}
	return "comments"
}

func (row anchoredRow) eventType() string {
	if row.ask {
		return "ask.anchor_refreshed"
	}
	return "comment.anchor_refreshed"
}

// versionAnchorRefreshTrips writes a named version of a fresh document carrying anchorRefreshMarks
// open anchors, the first changed of which have stored quotes that no longer match their marks, and
// returns how many round trips the version's transaction made holding the events' commit-order lock.
// It requires what the version leaves to be what it is today: every anchor's row carries its mark's
// quote, and each changed anchor, and no other, has one refresh event of its kind, whose payload
// names that quote, at the next sequences of the issue's events.
func versionAnchorRefreshTrips(t *testing.T, changed int) int {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := tracedStore(t, database, tracer)
	artifactID := createDocument(t, traced, "# First")
	service, _ := newTestServiceInstance(t, traced)
	alice := model.Actor{Kind: "user", ID: "alice"}

	rows := make([]anchoredRow, anchorRefreshMarks)
	var text strings.Builder
	for index := range rows {
		rows[index] = anchoredRow{
			id:    fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1),
			quote: fmt.Sprintf("quote%02d", index),
			ask:   index == 0,
		}
		fmt.Fprintf(&text, "Paragraph %s here.\n\n", rows[index].quote)
	}
	seedServiceText(t, service, artifactID, text.String())
	for _, row := range rows {
		kind := MarkComment
		if row.ask {
			kind = MarkAsk
		}
		if _, err := joinedMarkQuote(service, artifactID, MarkSpec{Kind: kind, ID: row.id, By: alice}, row.quote, nil); err != nil {
			t.Fatalf("mark %q: %v", row.quote, err)
		}
	}

	tx, err := traced.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the anchored rows: %v", err)
	}
	defer tx.Rollback(ctx)
	for index, row := range rows {
		stored := model.Anchor{ArtifactID: artifactID, MarkID: row.id, Version: 1, Quote: row.quote}
		if index < changed {
			stored.Quote = "stale " + row.quote
		}
		anchor, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("encode the anchor of %q: %v", row.quote, err)
		}
		if row.ask {
			if _, err := tx.Exec(ctx, `
				insert into asks (id, issue_key, author, question, anchor)
				values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored ask', $2)
			`, row.id, anchor); err != nil {
				t.Fatalf("create the anchored ask: %v", err)
			}
			if _, err := service.events.Append(ctx, tx, model.Event{
				IssueKey: new("DOC-1"),
				Type:     "ask.opened",
				Actor:    alice,
				Payload:  model.NewAskEventPayload(model.Ask{ID: row.id}, model.ReferenceChanges{}),
			}); err != nil {
				t.Fatalf("record the anchored ask's opened event: %v", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, anchor)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $2)
		`, row.id, anchor); err != nil {
			t.Fatalf("create the anchored comment on %q: %v", row.quote, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the anchored rows: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	var lastEvent, lastSeq int64
	if err := traced.Pool.QueryRow(ctx, `
		select coalesce(max(id), 0), coalesce(max(seq), 0) from events where issue_key = 'DOC-1'
	`).Scan(&lastEvent, &lastSeq); err != nil {
		t.Fatalf("read the issue's last event: %v", err)
	}

	since := tracer.mark()
	if _, err := namedVersion(t, service, artifactID, "Anchors", alice); err != nil {
		t.Fatalf("write the named version: %v", err)
	}
	trips, committed := tracer.commitOrderedTrips(since)
	if !committed {
		t.Fatalf("the version's transaction was not seen taking the events' commit-order lock and committing (%d round trips seen), so nothing here counts what it says", trips)
	}

	for _, row := range rows {
		var encoded []byte
		if err := traced.Pool.QueryRow(ctx, fmt.Sprintf(`select anchor from %s where id = $1`, row.table()), row.id).Scan(&encoded); err != nil {
			t.Fatalf("read the anchor of %q: %v", row.quote, err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode the anchor of %q: %v", row.quote, err)
		}
		if anchor.Quote != row.quote || anchor.Orphaned {
			t.Errorf("the anchor of %s %s after the version quotes %q (orphaned %t), want its mark's %q", row.table(), row.id, anchor.Quote, anchor.Orphaned, row.quote)
		}
	}

	events, err := traced.Pool.Query(ctx, `
		select seq, type, payload from events where issue_key = 'DOC-1' and id > $1 order by seq
	`, lastEvent)
	if err != nil {
		t.Fatalf("read the version's events: %v", err)
	}
	defer events.Close()
	byID := make(map[string]anchoredRow, len(rows))
	for _, row := range rows {
		byID[row.id] = row
	}
	refreshed := make(map[string]int)
	wantSeq := lastSeq
	for events.Next() {
		var seq int64
		var eventType string
		var payload []byte
		if err := events.Scan(&seq, &eventType, &payload); err != nil {
			t.Fatalf("scan the version's events: %v", err)
		}
		wantSeq++
		if seq != wantSeq {
			t.Errorf("the issue's event %q has sequence %d, want %d: the version's events are not contiguous", eventType, seq, wantSeq)
			wantSeq = seq
		}
		if !strings.HasSuffix(eventType, ".anchor_refreshed") {
			continue
		}
		var body struct {
			ID     string        `json:"id"`
			Anchor *model.Anchor `json:"anchor"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatalf("decode the %s event's payload: %v", eventType, err)
		}
		row, known := byID[body.ID]
		switch {
		case !known:
			t.Errorf("a %s event names %q, none of the document's anchored rows", eventType, body.ID)
			continue
		case eventType != row.eventType():
			t.Errorf("the refresh of %s %s is a %s event, want %s", row.table(), row.id, eventType, row.eventType())
		case body.Anchor == nil || body.Anchor.Quote != row.quote || body.Anchor.Orphaned:
			t.Errorf("the %s event of %s carries anchor %+v, want its mark's quote %q", eventType, row.id, body.Anchor, row.quote)
		}
		refreshed[body.ID]++
	}
	if err := events.Err(); err != nil {
		t.Fatalf("read the version's events: %v", err)
	}
	for index, row := range rows {
		want := 0
		if index < changed {
			want = 1
		}
		if refreshed[row.id] != want {
			t.Errorf("the version appended %d refresh events for %s %s (quote %q), want %d", refreshed[row.id], row.table(), row.id, row.quote, want)
		}
	}
	return trips
}

// A version write refreshes every open anchor of its document against the version's tree, and
// each anchor whose quote changed is written back with an event of its own, all while the
// transaction holds the advisory lock every event writer on the database waits for (events/broker.go).
// One after another, each changed anchor costs an update, a reload of its row and an event append:
// about seven round trips under that lock for each. Written together, the round trips the lock is
// held for do not grow with the number of anchors that changed, and what the version leaves - the
// rows, the events, their order among the issue's events - is what it was.
func TestAVersionWritesChangedAnchorsInRoundTripsThatDoNotGrowWithTheirCount(t *testing.T) {
	one := versionAnchorRefreshTrips(t, 1)
	all := versionAnchorRefreshTrips(t, anchorRefreshMarks)
	t.Logf("a version holding the events' commit-order lock: %d round trips with 1 changed anchor, %d with %d", one, all, anchorRefreshMarks)
	if all > one+2 {
		t.Errorf("a version with %d changed anchors made %d round trips holding the events' commit-order lock, against %d with one changed anchor: want at most %d",
			anchorRefreshMarks, all, one, one+2)
	}
}
