package events

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx, so one query
// serves both the plain-read and in-transaction paths.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// OpenedEventIDs resolves each ask's identity-establishing event id: its
// ask.opened event, falling back to the earliest ask.answered/ask.resolved/
// ask.edited event for a legacy row whose opening event has since been
// pruned. This is the one place an ask's identity event is resolved, shared
// by the read API (api.attachOpenedEventIDs) and the anchor-refresh cascade
// (docs.refreshAnchors) — a second, stricter fallback ladder here would make
// a legacy ask readable but unrefreshable, aborting an unrelated document
// write for reasons the read API already tolerates.
//
// OpenedEventIDsStatement and ScanOpenedEventIDs are the statement and the scan it runs, so a
// caller that queues the read beside its others (the issue read's batch, api.getIssue) resolves
// an ask's identity event exactly as this does.
func OpenedEventIDs(ctx context.Context, q Queryer, askIDs []string) (map[string]int64, error) {
	if len(askIDs) == 0 {
		return map[string]int64{}, nil
	}
	rows, err := q.Query(ctx, OpenedEventIDsStatement, askIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOpenedEventIDs(rows, len(askIDs))
}

// OpenedEventIDsStatement is the query OpenedEventIDs runs, $1 the ask ids. The partial index
// events_ask_payload_id (migration 0030) serves it, and that index's `type in (...)` list
// mirrors this one.
const OpenedEventIDsStatement = `
		select payload->>'id', min(id)
		from events
		where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
		  and payload->>'id' = any($1)
		group by payload->>'id'
	`

// ScanOpenedEventIDs reads the rows OpenedEventIDsStatement returns, keyed by ask id; asks is
// how many asks were asked for, which sizes the result.
func ScanOpenedEventIDs(rows pgx.Rows, asks int) (map[string]int64, error) {
	result := make(map[string]int64, asks)
	for rows.Next() {
		var askID string
		var eventID int64
		if err := rows.Scan(&askID, &eventID); err != nil {
			return nil, err
		}
		result[askID] = eventID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
