package store

import (
	"context"
	"crypto/sha256"
	"time"
)

// A browser session is a person signed in to the broker's approval page (0019's browser_sessions).
// Each function takes the SHA-256 of the session's cookie secret, never the secret itself, and
// the caller's now: a session is live while its expires_at is after now, so the lookup and the
// sweep draw the same line, as the sweeper's other expiries do on the time it passes them.

// CreateBrowserSession writes the session whose secret hashes to hash, signed in as login until
// expires.
func (s *Store) CreateBrowserSession(ctx context.Context, hash [sha256.Size]byte, login string, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into browser_sessions (secret_hash, login, expires_at) values ($1, $2, $3)`,
		hash[:], login, expires)
	return err
}

// BrowserSession answers the login and expiry of the session whose secret hashes to hash while it
// is live at now. A session past its expiry is absent, as one never made or logged out is: each
// answers pgx.ErrNoRows.
func (s *Store) BrowserSession(ctx context.Context, hash [sha256.Size]byte, now time.Time) (login string, expires time.Time, err error) {
	err = s.Pool.QueryRow(ctx, `select login, expires_at from browser_sessions where secret_hash = $1 and expires_at > $2`,
		hash[:], now).Scan(&login, &expires)
	return login, expires, err
}

// DeleteBrowserSession deletes the session whose secret hashes to hash, its logout. A session
// already gone is no error.
func (s *Store) DeleteBrowserSession(ctx context.Context, hash [sha256.Size]byte) error {
	_, err := s.Pool.Exec(ctx, `delete from browser_sessions where secret_hash = $1`, hash[:])
	return err
}

// DeleteExpiredBrowserSessions deletes every session no longer live at now and answers how many
// it deleted.
func (s *Store) DeleteExpiredBrowserSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `delete from browser_sessions where expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
