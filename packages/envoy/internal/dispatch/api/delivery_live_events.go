package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// deliveryLiveEventTypes are the event types whose payload the delivery live hub reads: an issue's
// own writes (a title, a priority, a parent, a component choice, a status, a route), its claim and
// release, and an architecture import. Task 0 Step 18 found no other writer of a field the Delivery
// page shows (route has one writer, PATCH /api/v1/issues/{key}, whose event is issue.updated or
// issue.closed).
var deliveryLiveEventTypes = []string{
	"issue.created", "issue.updated", "issue.closed", "issue.claimed", "issue.released",
	"architecture.synced",
}

// liveEvent is one event the hub may act on: its id, type, issue, actor and, for a type the hub
// reads or an event a session wrote, its payload (nil otherwise).
type liveEvent struct {
	id       int64
	at       time.Time
	kind     string
	issueKey string
	actor    model.Actor
	payload  json.RawMessage
}

// liveEventsSince reads up to limit events after cursor and at or below safe, oldest first,
// carrying the payload only of an event the hub reads (deliveryLiveEventTypes) or one a session
// wrote (its actor's title can move a view's session titles). through and at are the last id it
// scanned and that row's created_at, so the hub moves its cursor past a run of events it ignores
// and never scans them again; through is cursor, and at zero, when nothing lies past it.
//
// safe is the safe horizon the caller read (events.Horizon), at or below which every event is
// final. Event ids are not commit order, so a lower id can still be open after a higher one has
// committed; bounded by safe, a read that moves the cursor to through skips no id that can still
// commit, and through is always an existing row at or below an H, which the reset check needs.
func liveEventsSince(ctx context.Context, q queryer, cursor, safe int64, limit int) (events []liveEvent, through int64, at time.Time, err error) {
	rows, err := q.Query(ctx, `
		select id, created_at, type, coalesce(issue_key, ''), actor,
		       case when (type = any($3) or actor->>'kind' = 'session') then payload end
		from events
		where id > $1 and id <= $2
		order by id
		limit $4
	`, cursor, safe, deliveryLiveEventTypes, limit)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	defer rows.Close()
	through = cursor
	events = []liveEvent{}
	for rows.Next() {
		var event liveEvent
		var actor, payload []byte
		if err := rows.Scan(&event.id, &event.at, &event.kind, &event.issueKey, &actor, &payload); err != nil {
			return nil, 0, time.Time{}, err
		}
		through, at = event.id, event.at
		if len(payload) == 0 {
			continue
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			return nil, 0, time.Time{}, err
		}
		event.payload = payload
		events = append(events, event)
	}
	return events, through, at, rows.Err()
}

// eventCursorCheck reads the newest event's id (0 when the log is empty) and the created_at of the
// event at cursor (nil when no row holds it): the delivery hub (catchUpEvents) and the event stream
// relay (resetCursor) compare them with what they read for their cursor to tell an event log that
// was reset (eventLogWasReset).
//
// Both come from one statement, so they describe one log. A `truncate events restart identity`
// (e2e/seed.ts) takes an ACCESS EXCLUSIVE lock, and a statement holds its ACCESS SHARE lock on
// events until it ends, so the reset lands wholly before or wholly after this read. Read as two
// statements, a reset committing between them answered with the old log's newest id and no row at
// the cursor: the relay then took the old log's head for the new log's and closed the next
// subscription opened after the reset as a second reset.
func eventCursorCheck(ctx context.Context, q queryer, cursor int64) (maxID int64, cursorAt *time.Time, err error) {
	if err := q.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), (select created_at from events where id = $1)
	`, cursor).Scan(&maxID, &cursorAt); err != nil {
		return 0, nil, err
	}
	return maxID, cursorAt, nil
}

// eventLogWasReset is whether the event log was reset under a reader's cursor, from what
// eventCursorCheck read for it (maxID, cursorAt) and what the reader read when it moved there (at):
// a log whose newest id is below the cursor, or whose row at the cursor is gone or carries another
// created_at — a `truncate events restart identity` refilled past it, which is what e2e/seed.ts:20
// and :150 do between browser scenarios. The cursor is always an existing row's id, or 0 before the
// first, so until a reset the row at it carries the created_at the reader read.
func eventLogWasReset(cursor int64, at time.Time, maxID int64, cursorAt *time.Time) bool {
	return maxID < cursor || (cursor > 0 && (cursorAt == nil || !cursorAt.Equal(at)))
}
