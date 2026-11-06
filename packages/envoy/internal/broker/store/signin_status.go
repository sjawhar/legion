package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker's sign-in status is one row (0023's signin_status), the facts GET /signin/status
// answers. It is in Postgres rather than in each process so that every replica and every restart
// answers the same thing, and it is one row because none of these facts belongs to a pod: the
// client check any replica took is the check they all report.
//
// Each writer upserts that one row and sets only its own columns. Two replicas writing different
// facts in the same moment therefore keep both, where a writer that wrote the whole row would
// blank whatever it did not hold. A fact that has never happened is absent: its columns are
// null, never a zero time, so "no sign-in has failed" and "one failed at the zero time" cannot
// be confused.
//
// The two vocabularies - the client check's state and the failed sign-in's reason - are closed
// by the migration's own check constraints, which are the one place they are written down. A
// word outside them is refused by Postgres, so no provider's message can reach a public answer
// through this row.

// SignInStatus is the signin_status row. Every field is nil for a fact that has never happened.
type SignInStatus struct {
	// ClientCheckState is what the last client check found: "ready", "client_rejected" or
	// "issuer_unreachable" (oidc.ClientCheck's words).
	ClientCheckState *string
	// ClientCheckedAt is when that check was taken.
	ClientCheckedAt *time.Time
	// LastSignInAt is when a sign-in last completed.
	LastSignInAt *time.Time
	// LastFailedSignInAt is when a sign-in last failed at the token exchange or the ID-token
	// check, and LastFailedSignInReason the coded reason it failed with.
	LastFailedSignInAt     *time.Time
	LastFailedSignInReason *string
	// LastDecisionAt is when a decision was last made on the approval page.
	LastDecisionAt *time.Time
}

// SignInStatus reads the whole row. A broker that has written none of these facts yet has no row
// at all, which is the zero SignInStatus and no error: nothing has happened is what it means.
func (s *Store) SignInStatus(ctx context.Context) (SignInStatus, error) {
	var status SignInStatus
	err := s.Pool.QueryRow(ctx, `select client_check_state, client_checked_at, last_sign_in_at,
		last_failed_sign_in_at, last_failed_sign_in_reason, last_decision_at from signin_status`).
		Scan(&status.ClientCheckState, &status.ClientCheckedAt, &status.LastSignInAt,
			&status.LastFailedSignInAt, &status.LastFailedSignInReason, &status.LastDecisionAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignInStatus{}, nil
	}
	if err != nil {
		return SignInStatus{}, err
	}
	return status, nil
}

// RecordClientCheck writes what the client check found at at: state is "ready" when the issuer
// accepted the broker's client, "client_rejected" when it did not, and "issuer_unreachable" when
// it could not be asked. Postgres refuses any other word.
func (s *Store) RecordClientCheck(ctx context.Context, state string, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into signin_status (only_row, client_check_state, client_checked_at)
		values (true, $1, $2)
		on conflict (only_row) do update set client_check_state = excluded.client_check_state,
			client_checked_at = excluded.client_checked_at`, state, at)
	return err
}

// RecordSignIn writes the time of a sign-in that completed.
func (s *Store) RecordSignIn(ctx context.Context, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into signin_status (only_row, last_sign_in_at) values (true, $1)
		on conflict (only_row) do update set last_sign_in_at = excluded.last_sign_in_at`, at)
	return err
}

// RecordFailedSignIn writes the time of a sign-in that failed and the coded reason it failed
// with, one of the eleven the migration lists. Postgres refuses any other word, so no provider's
// own message can be written here.
func (s *Store) RecordFailedSignIn(ctx context.Context, reason string, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into signin_status (only_row, last_failed_sign_in_at, last_failed_sign_in_reason)
		values (true, $1, $2)
		on conflict (only_row) do update set last_failed_sign_in_at = excluded.last_failed_sign_in_at,
			last_failed_sign_in_reason = excluded.last_failed_sign_in_reason`, at, reason)
	return err
}

// RecordDecision writes the time of a decision made on the approval page.
func (s *Store) RecordDecision(ctx context.Context, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into signin_status (only_row, last_decision_at) values (true, $1)
		on conflict (only_row) do update set last_decision_at = excluded.last_decision_at`, at)
	return err
}
