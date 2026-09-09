package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// PgUserStore stores refreshable GitHub OAuth tokens by GitHub login.
type PgUserStore struct {
	pool *pgxpool.Pool
}

// NewPgUserStore returns a UserStore backed by pool.
func NewPgUserStore(pool *pgxpool.Pool) *PgUserStore {
	return &PgUserStore{pool: pool}
}

// Read returns nil, nil when login has no stored token pair.
func (s *PgUserStore) Read(login string) (*auth.User, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("read user: Postgres pool required")
	}
	var user auth.User
	var accessExpiresAt, refreshExpiresAt *time.Time
	err := s.pool.QueryRow(context.Background(), `
		select login, access_token, refresh_token, access_expires_at, refresh_expires_at
		from users
		where login = $1
	`, login).Scan(
		&user.Login,
		&user.Tokens.AccessToken,
		&user.Tokens.RefreshToken,
		&accessExpiresAt,
		&refreshExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user %q: %w", login, err)
	}
	user.Tokens.GithubLogin = user.Login
	user.Tokens.AccessExpiresAt = unixMilli(accessExpiresAt)
	user.Tokens.RefreshExpiresAt = unixMilli(refreshExpiresAt)
	return &user, nil
}

// Write upserts a user's current GitHub OAuth token pair.
func (s *PgUserStore) Write(user *auth.User) error {
	if s == nil || s.pool == nil {
		return errors.New("write user: Postgres pool required")
	}
	if user == nil {
		return errors.New("write user: nil user")
	}
	if user.Login == "" {
		return errors.New("write user: login required")
	}
	_, err := s.pool.Exec(context.Background(), `
		insert into users (
			login, access_token, refresh_token, access_expires_at, refresh_expires_at
		) values ($1, $2, $3, $4, $5)
		on conflict (login) do update set
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			access_expires_at = excluded.access_expires_at,
			refresh_expires_at = excluded.refresh_expires_at,
			updated_at = now()
	`,
		user.Login,
		user.Tokens.AccessToken,
		user.Tokens.RefreshToken,
		asTime(user.Tokens.AccessExpiresAt),
		asTime(user.Tokens.RefreshExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("write user %q: %w", user.Login, err)
	}
	return nil
}

// Remove deletes a user's stored OAuth tokens. A missing user is a no-op.
func (s *PgUserStore) Remove(login string) error {
	if s == nil || s.pool == nil {
		return errors.New("remove user: Postgres pool required")
	}
	if _, err := s.pool.Exec(context.Background(), "delete from users where login = $1", login); err != nil {
		return fmt.Errorf("remove user %q: %w", login, err)
	}
	return nil
}

func asTime(milliseconds int64) any {
	if milliseconds == 0 {
		return nil
	}
	return time.UnixMilli(milliseconds).UTC()
}

func unixMilli(value *time.Time) int64 {
	if value == nil {
		return 0
	}
	return value.UnixMilli()
}

var _ auth.UserStore = (*PgUserStore)(nil)
