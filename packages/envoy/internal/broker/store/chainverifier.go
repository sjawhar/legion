package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// ChainVerifier builds the record.ChainVerifier that re-verifies records of one kind
// ('agent_secret' for requests.Machine's grants, 'launcher_credential' for
// enroll.Service.AuthenticateLauncher) straight from this store: FetchRecord finds a record only
// of that kind, so a record of one kind never backs the other's credential, and FetchDecisions
// reads every terminal decision event the record carries, oldest first.
func (s *Store) ChainVerifier(kind, audience string, skew time.Duration) *record.ChainVerifier {
	return &record.ChainVerifier{
		Audience: audience,
		Skew:     skew,
		FetchRecord: func(ctx context.Context, recordID string) (string, time.Time, bool, error) {
			var body string
			var createdAt time.Time
			err := s.Pool.QueryRow(ctx, `select body, created_at from credential_requests where id=$1 and kind=$2`, recordID, kind).Scan(&body, &createdAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", time.Time{}, false, nil
			}
			if err != nil {
				return "", time.Time{}, false, err
			}
			return body, createdAt, true, nil
		},
		FetchDecisions: func(ctx context.Context, recordID string) ([]record.TerminalEvent, error) {
			rows, err := s.Pool.Query(ctx, `select event, coalesce(login, '') from credential_request_events
				where record_id=$1 and event = any($2) order by id`, recordID, record.TerminalEventNames())
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowToStructByPos[record.TerminalEvent])
		},
	}
}
