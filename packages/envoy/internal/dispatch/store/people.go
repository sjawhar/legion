package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

// RefreshTokenRevoker revokes a refresh token at the sign-in pool that issued it; *oidc.CodeFlow
// is the one Dispatch uses. Its error never carries the token.
type RefreshTokenRevoker interface {
	Revoke(ctx context.Context, refreshToken string) error
}

// PgPeopleStore records the people who have signed in to Dispatch and each pool sign-in's
// membership (migration 0068), its refresh token sealed (refreshTokenSeal).
type PgPeopleStore struct {
	pool    *Pool
	seal    refreshTokenSeal
	revoker RefreshTokenRevoker
}

// NewPgPeopleStore returns a PeopleStore backed by pool that seals each refresh token under a key
// derived from signingKey, the key session cookies are signed with (DISPATCH_SIGNING_KEY), and
// retires one stored in plain text by revoking it through revoker, the sign-in pool's code flow; a
// nil revoker (no sign-in pool) revokes nothing, so such a token stays.
func NewPgPeopleStore(pool *Pool, signingKey string, revoker RefreshTokenRevoker) *PgPeopleStore {
	return &PgPeopleStore{pool: pool, seal: newRefreshTokenSeal(signingKey), revoker: revoker}
}

// Record notes that email has signed in, keeping any membership it already holds.
func (s *PgPeopleStore) Record(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email); err != nil {
		return fmt.Errorf("record person %q: %w", email, err)
	}
	return nil
}

// SignIn records a sign-in through the pool, replacing any membership email held before. A refresh
// token the row holds in plain text is retired first, so the replacement never forgets one the pool
// still accepts; when the pool does not revoke it, retire has logged that, and the sign-in proceeds.
func (s *PgPeopleStore) SignIn(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	var stored *string
	err := s.pool.QueryRow(ctx, `select refresh_token from people where email = $1`, email).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the refresh token the sign-in of %q replaces: %w", email, err)
	}
	if stored != nil && !strings.HasPrefix(*stored, sealedRefreshTokenPrefix) {
		if err := s.retire(ctx, email, *stored); err != nil && !errors.Is(err, errNotRevoked) {
			return err
		}
	}
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
// token that does not open (sealed under another signing key, for another person, or stored in
// plain text) is no refresh token: the read answers none, so the person signs in again, and logs
// that without the value. The read writes nothing: the next boot retires a token stored in plain
// text (RetirePlainRefreshTokens), and the person's next sign-in replaces whatever the row holds.
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
		slog.Warn("dispatch: a stored refresh token did not open; the person signs in again", "email", email, "reason", err.Error())
		return auth.PersonMembership{}, true, nil
	}
	return auth.PersonMembership{RefreshToken: refreshToken, ConfirmedAt: *confirmedAt}, true, nil
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

// RetirePlainRefreshTokens retires every refresh token people holds in plain text (retire): one
// stored before Dispatch sealed them, or by a task of that earlier release since, during a roll or
// after a rollback. Dispatch runs it at every boot. It returns how many the pool revoked and how
// many it did not, each of those left in its row for the next boot; once ctx is done, each token
// not yet revoked fails at once. The error is the database's, and stops the sweep.
func (s *PgPeopleStore) RetirePlainRefreshTokens(ctx context.Context) (retired, failed int, err error) {
	rows, err := s.pool.Query(ctx, `select email, refresh_token from people where refresh_token is not null and refresh_token not like '`+sealedRefreshTokenPrefix+`%' order by email`)
	if err != nil {
		return 0, 0, fmt.Errorf("read the refresh tokens stored in plain text: %w", err)
	}
	type plain struct{ email, token string }
	tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (plain, error) {
		var p plain
		return p, row.Scan(&p.email, &p.token)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("read the refresh tokens stored in plain text: %w", err)
	}
	for _, p := range tokens {
		switch err := s.retire(ctx, p.email, p.token); {
		case errors.Is(err, errNotRevoked):
			failed++
		case err != nil:
			return retired, failed, err
		default:
			retired++
		}
	}
	return retired, failed, nil
}

// errNotRevoked is a refresh token stored in plain text that the sign-in pool did not revoke;
// retire has logged why.
var errNotRevoked = errors.New("the sign-in pool did not revoke the refresh token stored in plain text")

// errNoSignInPool is a revocation asked of a store with no sign-in pool to revoke at.
var errNoSignInPool = errors.New("no sign-in pool is configured (DISPATCH_SIGNIN_*) to revoke it at")

// retire revokes plain, a refresh token email's row holds in plain text, at the sign-in pool, then
// forgets it with its confirmation while the row still holds it, so a sign-in recorded since keeps
// its own. Anyone holding a copy of a plain token (a database backup, say) can redeem it with the
// client's secret until the pool revokes it, which forgetting alone never does. A revocation that
// fails leaves the row as it is and returns errNotRevoked, logged once at ERROR naming the person
// and the pool's HTTP status, never the token. Only a value without the sealed format's prefix
// reaches it: a sealed one is never sent to the pool.
func (s *PgPeopleStore) retire(ctx context.Context, email, plain string) error {
	if err := s.revoke(ctx, plain); err != nil {
		attrs := []any{"email", email}
		var refused *oidc.RevocationError
		if errors.As(err, &refused) {
			attrs = append(attrs, "status", refused.Status)
		}
		slog.Error("dispatch: the sign-in pool did not revoke a refresh token stored in plain text", append(attrs, "error", err.Error())...)
		return errNotRevoked
	}
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1 and refresh_token = $2`, email, plain); err != nil {
		return fmt.Errorf("forget the revoked refresh token of %q: %w", email, err)
	}
	return nil
}

func (s *PgPeopleStore) revoke(ctx context.Context, plain string) error {
	if s.revoker == nil {
		return errNoSignInPool
	}
	return s.revoker.Revoke(ctx, plain)
}

var _ auth.PeopleStore = (*PgPeopleStore)(nil)
