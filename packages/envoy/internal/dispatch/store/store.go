// Package store owns Dispatch's Postgres connection and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the shared Dispatch database handle.
type Store struct {
	Pool *pgxpool.Pool
}

//go:embed migrations/*.up.sql
var migrationFiles embed.FS

// Open connects to databaseURL and verifies the database is reachable.
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

// Migrate applies embedded migrations in filename order. Every migration and
// its durable version record are committed together.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.Pool == nil {
		return fmt.Errorf("migrate: store pool required")
	}
	if _, err := s.Pool.Exec(ctx, `
		create table if not exists schema_migrations (
			version integer primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	files, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, filename := range files {
		version, err := migrationVersion(filename)
		if err != nil {
			return err
		}
		contents, err := migrationFiles.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", filename, err)
		}
		if err := s.applyMigration(ctx, version, string(contents)); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version int, sql string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(8150001)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	var applied bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1)", version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	if applied {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("execute migration: %w", err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1)", version); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}

func migrationVersion(filename string) (int, error) {
	name := path.Base(filename)
	stem, ok := strings.CutSuffix(name, ".up.sql")
	if !ok {
		return 0, fmt.Errorf("invalid migration filename %q", filename)
	}
	versionText, _, ok := strings.Cut(stem, "_")
	if !ok {
		return 0, fmt.Errorf("invalid migration filename %q", filename)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("invalid migration version in %q", filename)
	}
	return version, nil
}
