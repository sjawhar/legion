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
)

// RefreshTokenRevoker revokes a refresh token at the sign-in pool that issued it; *oidc.CodeFlow
// is the one Dispatch uses. Its error never carries the token.
type RefreshTokenRevoker interface {
	Revoke(ctx context.Context, refreshToken string) error
}

// PgPeopleStore records the people who have signed in to Dispatch and each sign-in's membership
// (migration 0068), its refresh token sealed (refreshTokenSeal), with the Google account that last
// signed in to the address for a Workspace domain (migration 0112).
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

// retirePlainBeforeSignIn retires email's refresh token when the row holds it in plain text, so a
// sign-in that replaces it (SignIn or SignInSubject) never forgets one the pool still accepts.
// When the pool does not revoke it, this logs that at ERROR, naming the person to sign out at the
// pool since no later boot will see the token, and proceeds.
func (s *PgPeopleStore) retirePlainBeforeSignIn(ctx context.Context, email string) error {
	var stored *string
	err := s.pool.QueryRow(ctx, `select refresh_token from people where email = $1`, email).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the refresh token the sign-in of %q replaces: %w", email, err)
	}
	if stored != nil && !strings.HasPrefix(*stored, sealedRefreshTokenPrefix) {
		switch err := s.retire(ctx, email, *stored); {
		case errors.Is(err, errNotRevoked):
			slog.Error("dispatch: a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool", "email", email, "error", err)
		case err != nil:
			return err
		}
	}
	return nil
}

// SignIn records a sign-in through a Cognito sign-in pool, replacing any membership email held
// before (auth.PeopleStore.SignIn); the stored subject is neither read nor written. A refresh
// token the row holds in plain text is retired first (retirePlainBeforeSignIn), so the
// replacement never forgets one the pool still accepts.
func (s *PgPeopleStore) SignIn(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	if err := s.retirePlainBeforeSignIn(ctx, email); err != nil {
		return err
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

// SignInSubject records a sign-in for a Workspace domain in one transaction
// (auth.PeopleStore.SignInSubject), replacing any membership email held before; a refresh token
// the row holds in plain text is retired first (retirePlainBeforeSignIn), as SignIn's. It creates
// email's row when there is none and then locks it, so two sign-ins to one address take turns, two
// first sign-ins included: the second waits on the first's insert, then reads the subject it
// committed. A row this sign-in's own insert just created has no session to end, however its
// subject reads. For a row that already existed, when its recorded subject is not subject (Google
// gave the address to another account, or the row recorded no Google account at all: a
// Cognito-era, header- or dev-recorded row, which may hold a cookie from before a switch to
// domain mode) it ends every session of email (revokeSessionsSQL) before it records subject with
// the refresh token. A revoke that fails rolls the sign-in back, leaving the old subject for the
// next sign-in to revoke again; one that commits is logged at WARN with the address.
func (s *PgPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken string, confirmedAt time.Time) error {
	if err := s.retirePlainBeforeSignIn(ctx, email); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	inserted, err := tx.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email)
	if err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	var stored string
	if err := tx.QueryRow(ctx, `select coalesce(sub, '') from people where email = $1 for update`, email).Scan(&stored); err != nil {
		return fmt.Errorf("read the subject the sign-in of %q replaces: %w", email, err)
	}
	revoked := inserted.RowsAffected() == 0 && stored != subject
	if revoked {
		if _, err := tx.Exec(ctx, revokeSessionsSQL, email); err != nil {
			return fmt.Errorf("end the sessions of %q, which another Google account signs in to now: %w", email, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		update people
		set signed_in_at = now(), refresh_token = $2, confirmed_at = $3, sub = $4
		where email = $1
	`, email, s.seal.seal(email, refreshToken), confirmedAt, subject); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if revoked {
		slog.Warn("dispatch: a sign-in under another Google account ended every session of the address", "email", email)
	}
	return nil
}

// Membership reads email's membership; found is false for a person who never signed in. A refresh
// token that does not open (sealed under another signing key, for another person, or stored in
// plain text) is no refresh token: the read answers none, so the person signs in again, and logs
// that without the value. The read writes nothing: the next boot retires a token stored in plain
// text (RetirePlainRefreshTokens), and the person's next sign-in replaces whatever the row holds.
// The subject is read whatever the refresh token holds.
func (s *PgPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	var sealed *string
	var confirmedAt *time.Time
	membership := auth.PersonMembership{}
	err := s.pool.QueryRow(ctx, `select refresh_token, confirmed_at, coalesce(sub, '') from people where email = $1`, email).Scan(&sealed, &confirmedAt, &membership.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.PersonMembership{}, false, nil
	}
	if err != nil {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, err)
	}
	// people's check pairs the two: a row holds both or neither.
	if sealed == nil {
		return membership, true, nil
	}
	refreshToken, err := s.seal.open(email, *sealed)
	if err != nil {
		slog.Warn("dispatch: a stored refresh token did not open; the person signs in again", "email", email, "reason", err.Error())
		return membership, true, nil
	}
	membership.RefreshToken, membership.ConfirmedAt = refreshToken, *confirmedAt
	return membership, true, nil
}

// Confirm records a refresh that confirmed email's Cognito-pool membership, with the refresh
// token to use next (auth.PeopleStore.Confirm).
func (s *PgPeopleStore) Confirm(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = $2, confirmed_at = $3 where email = $1`, email, s.seal.seal(email, refreshToken), confirmedAt); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	return nil
}

// ConfirmSubject records a refresh that confirmed email's domain-mode membership
// (auth.PeopleStore.ConfirmSubject). It writes only while the row still records subject, the
// Google account the refresh read; a row another account signed in to since is left as it is, and
// the confirmation still returns nil. That is safe: the one request whose refresh it was passed
// its generation check before the refresh, and the other account's sign-in advanced the
// generation, so that browser's next request fails the check.
func (s *PgPeopleStore) ConfirmSubject(ctx context.Context, email, subject, refreshToken string, confirmedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = $2, confirmed_at = $3 where email = $1 and sub is not distinct from $4`, email, s.seal.seal(email, refreshToken), confirmedAt, subject); err != nil {
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
// many it did not, each of those logged at ERROR naming the person and left in its row for the next
// boot; once ctx is done, each token not yet revoked fails at once. The error is the database's,
// and stops the sweep.
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
			slog.Error("dispatch: the sign-in pool did not revoke a refresh token stored in plain text", "email", p.email, "error", err)
			failed++
		case err != nil:
			return retired, failed, err
		default:
			retired++
		}
	}
	return retired, failed, nil
}

// errNotRevoked is a refresh token stored in plain text that the sign-in pool did not revoke.
var errNotRevoked = errors.New("the sign-in pool did not revoke the refresh token stored in plain text")

// errNoSignInPool is a revocation asked of a store with no sign-in pool to revoke at.
var errNoSignInPool = errors.New("no sign-in pool is configured (DISPATCH_SIGNIN_*) to revoke it at")

// retire revokes plain, a refresh token email's row holds in plain text, at the sign-in pool, then
// forgets it with its confirmation while the row still holds it, so a sign-in recorded since keeps
// its own. Anyone holding a copy of a plain token (a database backup, say) can redeem it with the
// client's secret until the pool revokes it, which forgetting alone never does. A revocation that
// fails leaves the row as it is and returns errNotRevoked wrapping the pool's error, which never
// carries the token; the caller logs it. Only a value without the sealed format's prefix reaches
// it: a sealed one is never sent to the pool.
func (s *PgPeopleStore) retire(ctx context.Context, email, plain string) error {
	if s.revoker == nil {
		return fmt.Errorf("%w: %w", errNotRevoked, errNoSignInPool)
	}
	if err := s.revoker.Revoke(ctx, plain); err != nil {
		return fmt.Errorf("%w: %w", errNotRevoked, err)
	}
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1 and refresh_token = $2`, email, plain); err != nil {
		return fmt.Errorf("forget the revoked refresh token of %q: %w", email, err)
	}
	return nil
}

var _ auth.PeopleStore = (*PgPeopleStore)(nil)

// EndSubject ends email's domain-mode sign-in only while its row records subject, empty meaning
// none (auth.PeopleStore.EndSubject). It locks the row, compares its subject, and only then
// advances the session generation (revokeSessionsSQL) and forgets the refresh token and
// confirmation, all in one transaction, so a sign-in by another account cannot land between the
// check and the writes.
func (s *PgPeopleStore) EndSubject(ctx context.Context, email, subject string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("end the sign-in of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	var stored string
	err = tx.QueryRow(ctx, `select coalesce(sub, '') from people where email = $1 for update`, email).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the subject the end of %q expects: %w", email, err)
	}
	if stored != subject {
		return false, nil
	}
	if _, err := tx.Exec(ctx, revokeSessionsSQL, email); err != nil {
		return false, fmt.Errorf("end the sessions of %q: %w", email, err)
	}
	if _, err := tx.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1`, email); err != nil {
		return false, fmt.Errorf("end membership of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("end the sign-in of %q: %w", email, err)
	}
	return true, nil
}
