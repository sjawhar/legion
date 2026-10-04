package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// PgPeopleStore records the people who have signed in to Dispatch and each pool sign-in's
// membership (migration 0068), its refresh token sealed (refreshTokenSeal).
type PgPeopleStore struct {
	pool *Pool
	seal refreshTokenSeal
}

// NewPgPeopleStore returns a PeopleStore backed by pool that seals each refresh token under a key
// derived from signingKey, the key session cookies are signed with (DISPATCH_SIGNING_KEY): a new
// signing key opens none of the refresh tokens stored under the old one.
func NewPgPeopleStore(pool *Pool, signingKey string) *PgPeopleStore {
	return &PgPeopleStore{pool: pool, seal: newRefreshTokenSeal(signingKey)}
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
	`, email, s.seal.seal(email, refreshToken), confirmedAt); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	return nil
}

// Membership reads email's membership; found is false for a person who never signed in. A refresh
// token that does not open (sealed under another signing key, for another person, or stored before
// Dispatch sealed them) is no refresh token: Membership forgets it with its confirmation, as End
// does, so the person signs in again, and logs that once, without the value.
func (s *PgPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	var sealed *string
	var confirmedAt *time.Time
	err := s.pool.QueryRow(ctx, `select refresh_token, confirmed_at from people where email = $1`, email).Scan(&sealed, &confirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.PersonMembership{}, false, nil
	}
	if err != nil {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, err)
	}
	// people's check pairs the two: a row holds both or neither.
	if sealed == nil {
		return auth.PersonMembership{}, true, nil
	}
	refreshToken, err := s.seal.open(email, *sealed)
	if err != nil {
		return auth.PersonMembership{}, true, s.forgetUnopened(ctx, email, *sealed, err)
	}
	return auth.PersonMembership{RefreshToken: refreshToken, ConfirmedAt: *confirmedAt}, true, nil
}

// forgetUnopened forgets email's refresh token and confirmation while the row still holds sealed,
// the value that did not open, so a sign-in recorded since keeps its own. Only the read that
// forgets it logs, so the line appears once however many requests read the row at once.
func (s *PgPeopleStore) forgetUnopened(ctx context.Context, email, sealed string, reason error) error {
	tag, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1 and refresh_token = $2`, email, sealed)
	if err != nil {
		return fmt.Errorf("forget the refresh token of %q that did not open: %w", email, err)
	}
	if tag.RowsAffected() == 1 {
		slog.Warn("dispatch: a stored refresh token did not open; the person signs in again", "email", email, "reason", reason.Error())
	}
	return nil
}

// Confirm records a refresh that confirmed email's membership.
func (s *PgPeopleStore) Confirm(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = $2, confirmed_at = $3 where email = $1`, email, s.seal.seal(email, refreshToken), confirmedAt); err != nil {
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
