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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The whole directory, not *.up.sql alone, and with all:, which keeps names beginning with _ or .
// that a bare directory pattern leaves out: pgmigrate.Load then sees every file in the tree and
// refuses one named any other way, rather than a migration going unembedded and unapplied while
// its file sits there.
//
//go:embed all:migrations
var migrationFiles embed.FS

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
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
		if err := pgmigrate.Exec(ctx, tx, migration); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into broker_schema_migrations (version) values ($1)`, migration.Version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
