package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// PgSessionStore records the generation used to revoke signed browser sessions.
type PgSessionStore struct {
	pool *pgxpool.Pool
}

// NewPgSessionStore returns a SessionStore backed by pool.
func NewPgSessionStore(pool *pgxpool.Pool) *PgSessionStore {
	return &PgSessionStore{pool: pool}
}

// EnsureSession creates the initial generation for a newly authenticated login
// and returns the current generation.
func (s *PgSessionStore) EnsureSession(ctx context.Context, login string) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("ensure session: Postgres pool required")
	}
	var generation int64
	err := s.pool.QueryRow(ctx, `
		insert into user_sessions (login) values ($1)
		on conflict (login) do nothing
		returning generation
	`, login).Scan(&generation)
	if err == nil {
		return generation, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("ensure session for %q: %w", login, err)
	}
	generation, found, err := s.CurrentSessionGeneration(ctx, login)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("ensure session for %q: session row disappeared", login)
	}
	return generation, nil
}

// CurrentSessionGeneration reads the current generation. A missing row does
// not authenticate a signed browser cookie.
func (s *PgSessionStore) CurrentSessionGeneration(ctx context.Context, login string) (int64, bool, error) {
	if s == nil || s.pool == nil {
		return 0, false, errors.New("read session generation: Postgres pool required")
	}
	var generation int64
	if err := s.pool.QueryRow(ctx, `select generation from user_sessions where login = $1`, login).Scan(&generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read session generation for %q: %w", login, err)
	}
	return generation, true, nil
}

// RevokeSessions advances the user's generation, invalidating every previously
// issued signed browser cookie for that login.
func (s *PgSessionStore) RevokeSessions(ctx context.Context, login string) error {
	if s == nil || s.pool == nil {
		return errors.New("revoke sessions: Postgres pool required")
	}
	if _, err := s.pool.Exec(ctx, `
		insert into user_sessions (login, generation) values ($1, 1)
		on conflict (login) do update set generation = user_sessions.generation + 1
	`, login); err != nil {
		return fmt.Errorf("revoke sessions for %q: %w", login, err)
	}
	return nil
}

var _ auth.SessionStore = (*PgSessionStore)(nil)
