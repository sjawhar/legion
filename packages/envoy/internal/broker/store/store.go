// Package store owns the broker's Postgres pool and schema migrations. Its runner shares
// internal/dispatch/store's rules through internal/pgmigrate: the embedded set is refused whole,
// before anything is applied, when pgmigrate.Load refuses it, and every lock wait a migration
// makes is bounded by pgmigrate.LockTimeout. Migrate holds a broker-specific
// pg_advisory_xact_lock for the whole run, so concurrent callers serialize instead of racing on
// `create table if not exists`, and applies every pending migration inside one shared
// transaction, recording each in broker_schema_migrations.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Every file in migrations, so pgmigrate.Load judges each one; pgmigratetest.CheckEmbedsEveryFile
// states why the pattern is all:migrations and holds the tree to it.
//
//go:embed all:migrations
var migrationFiles embed.FS

type Store struct {
	Pool *pgxpool.Pool

	// mu guards listen, the one-connection pool Listener opens on first use, so two callers
	// cannot open two of them.
	mu     sync.Mutex
	listen *pgxpool.Pool
}

// TokenMinter mints the password one new connection signs in with, for the host:port it dials
// and the database user it signs in as.
type TokenMinter func(ctx context.Context, endpoint, user string) (string, error)

// Option configures the pool Open makes.
type Option func(*pgxpool.Config)

// WithTokenMinter signs every new connection in with a password mint makes for it, set in the
// pool's BeforeConnect, so a connection the pool opens an hour after startup brings a token as
// fresh as the first one's: an RDS IAM token is good for 15 minutes from its mint and is checked
// only at sign-in, so a connection outlives the token it signed in with but a new one cannot
// reuse it. The migration lock watch's own connection signs in the same way (Migrate).
func WithTokenMinter(mint TokenMinter) Option {
	return func(config *pgxpool.Config) {
		config.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			token, err := mint(ctx, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))), cc.User)
			if err != nil {
				return fmt.Errorf("mint the sign-in token for database user %s at %s: %w", cc.User, cc.Host, err)
			}
			cc.Password = token
			return nil
		}
	}
}

// Open opens the pool databaseURL names, configured by opts, and pings it.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	for _, opt := range opts {
		opt(config)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// IsUniqueViolation reports whether err is Postgres's unique_violation (23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// listenerPoolSize bounds the pool Listener takes its connection from. One listener dials
// through it - the cross-pod secret-reread fan-out (internal/broker/reread) - and the connection
// is hijacked as soon as it is opened, so it leaves with its caller and the pool holds none
// between dials: what this bounds is dials in flight, not connections held.
const listenerPoolSize = 1

// Listener returns one connection of its own for LISTEN, from a one-connection pool opened from
// the shared pool's own configuration and hijacked, so it holds no connection of the shared pool
// and the caller owns, and closes, what it gets. The pool is opened on first use and kept, so
// every redial of a listener mints its own connection through it.
//
// Opening it from s.Pool.Config() rather than from the database URL is what keeps BeforeConnect -
// the RDS IAM token minter WithTokenMinter installs - minting a fresh token for every redial; a
// pgxpool.New on the raw URL would carry no minter and sign in with no password at all once the
// first token lapsed.
//
// What it costs: one extra Postgres connection per broker pod, outside the count pool_max_conns
// sizes, held for the life of the process, since a LISTEN lives on its connection and a pooled
// one would end the subscription when the pool recycled it. That connection also needs a
// session-mode path to Postgres: a transaction pooler hands each transaction whichever backend
// is free, which can carry no LISTEN. The deployment's path is a TLS tunnel that keeps each
// client connection its own session, not a pooler, so this holds there.
func (s *Store) Listener(ctx context.Context) (*pgx.Conn, error) {
	pool, err := s.listenerPool()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Hijack(), nil
}

// listenerPool opens the listener pool on first use, under mu for the whole check-and-open.
func (s *Store) listenerPool() (*pgxpool.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listen != nil {
		return s.listen, nil
	}
	config := s.Pool.Config().Copy()
	config.MaxConns = listenerPoolSize
	// Both minimums, not just MinConns: a floor the shared pool's DSN set would be a target one
	// connection cannot reach, which pgxpool re-attempts every health-check period.
	config.MinConns = 0
	config.MinIdleConns = 0
	// Not the caller's context: this pool outlives the dial that opened it, and pgxpool's own
	// background work would end with that context.
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("open the listener connection pool: %w", err)
	}
	s.listen = opened
	return opened, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(8330001)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `create table if not exists broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	for _, migration := range migrations {
		var applied bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from broker_schema_migrations where version=$1)`, migration.Version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into broker_schema_migrations (version) values ($1)`, migration.Version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
