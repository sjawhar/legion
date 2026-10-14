package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

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

// liveEventsSince reads up to limit events after cursor, oldest first, carrying the payload only of
// an event the hub reads (deliveryLiveEventTypes) or one a session wrote (its actor's title can move
// a view's session titles). through and at are the last id it scanned and that row's created_at, so
// the hub moves its cursor past a run of events it ignores and never scans them again; through is
// cursor, and at zero, when nothing lies past it.
func liveEventsSince(ctx context.Context, q queryer, cursor int64, limit int) (events []liveEvent, through int64, at time.Time, err error) {
	rows, err := q.Query(ctx, `
		select id, created_at, type, coalesce(issue_key, ''), actor,
		       case when (type = any($2) or actor->>'kind' = 'session') then payload end
		from events
		where id > $1
		order by id
		limit $3
	`, cursor, deliveryLiveEventTypes, limit)
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

// eventCursorCheck reads the newest event's id and created_at (0 and zero when the log is empty),
// and the created_at of the event at cursor (nil when no row holds it): the hub compares them with
// what it read for its cursor to tell an event log that was reset.
func eventCursorCheck(ctx context.Context, q queryer, cursor int64) (maxID int64, maxAt time.Time, cursorAt *time.Time, err error) {
	var newestAt *time.Time
	if err := q.QueryRow(ctx, `select id, created_at from events order by id desc limit 1`).Scan(&maxID, &newestAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil, err
	}
	if newestAt != nil {
		maxAt = *newestAt
	}
	var rowAt time.Time
	switch err := q.QueryRow(ctx, `select created_at from events where id = $1`, cursor).Scan(&rowAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return maxID, maxAt, nil, nil
	case err != nil:
		return 0, time.Time{}, nil, err
	}
	return maxID, maxAt, &rowAt, nil
}
