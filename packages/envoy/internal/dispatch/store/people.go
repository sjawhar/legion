package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// PgPeopleStore records the people who have signed in to Dispatch and each pool sign-in's
// membership (migration 0067).
type PgPeopleStore struct {
	pool *Pool
}

// NewPgPeopleStore returns a PeopleStore backed by pool.
func NewPgPeopleStore(pool *Pool) *PgPeopleStore {
	return &PgPeopleStore{pool: pool}
}

// Record notes that email has signed in, keeping any membership it already holds.
func (s *PgPeopleStore) Record(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email); err != nil {
		return fmt.Errorf("record person %q: %w", email, err)
	}
	return nil
}

// SignIn records a sign-in through the pool, replacing any membership email held before.
func (s *PgPeopleStore) SignIn(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		insert into people (email, refresh_token, confirmed_at) values ($1, $2, $3)
		on conflict (email) do update
		set signed_in_at = now(), refresh_token = excluded.refresh_token, confirmed_at = excluded.confirmed_at
	`, email, refreshToken, confirmedAt); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	return nil
}

// Membership reads email's membership; found is false for a person who never signed in.
func (s *PgPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	var refreshToken *string
	var confirmedAt *time.Time
	err := s.pool.QueryRow(ctx, `select refresh_token, confirmed_at from people where email = $1`, email).Scan(&refreshToken, &confirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.PersonMembership{}, false, nil
	}
	if err != nil {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, err)
	}
	var membership auth.PersonMembership
	if refreshToken != nil {
		membership.RefreshToken = *refreshToken
	}
	if confirmedAt != nil {
		membership.ConfirmedAt = *confirmedAt
	}
	return membership, true, nil
}

// Confirm records a refresh that confirmed email's membership.
func (s *PgPeopleStore) Confirm(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = $2, confirmed_at = $3 where email = $1`, email, refreshToken, confirmedAt); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	return nil
}

// End forgets email's refresh token and confirmation, keeping the person.
func (s *PgPeopleStore) End(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1`, email); err != nil {
		return fmt.Errorf("end membership of %q: %w", email, err)
	}
	return nil
}

var _ auth.PeopleStore = (*PgPeopleStore)(nil)
