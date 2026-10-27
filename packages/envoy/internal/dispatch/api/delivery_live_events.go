package api

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// eventCursorCheck reads the newest event's id and created_at (0 and zero when the log is empty),
// and the created_at of the event at cursor (nil when no row holds it): the event stream relay
// starts a generation's cursor from the first two, and compares them with what it read for its
// cursor to tell an event log that was reset (eventStreamRelay.resetCursor).
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
