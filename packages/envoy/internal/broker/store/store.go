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
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The whole directory, not *.up.sql alone, so pgmigrate.Load sees a file named any other way and
// refuses it rather than a migration going unembedded and unapplied while its file sits in the tree.
//
//go:embed migrations
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
