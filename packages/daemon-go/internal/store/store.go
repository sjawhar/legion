// Package store owns the Legion daemon's Postgres: its connection pool, its
// forward-only schema, and the durable facts the daemon keeps there. These
// tables are the daemon's alone — it reads no other component's, and none reads
// these.
package store

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// migrateLock is the advisory lock two daemons migrating the same database take
// turns on. The number is arbitrary and fixed: only agreement matters.
const migrateLock int64 = 1208001

// Store is the daemon's handle on its database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to dsn and waits for the server to answer, so a daemon that
// gets a Store has a database and one that does not is told where it failed.
func Open(ctx context.Context, dsn string) (*Store, error) {
	address, password := target(dsn)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, &connectError{address: address, password: password, err: err}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &connectError{address: address, password: password, err: err}
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool's connections.
func (s *Store) Close() {
	s.pool.Close()
}

// Tx runs fn inside one transaction: committed when fn returns nil, rolled back
// when it does not, and fn's own error is what the caller gets back.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// Migrate applies every embedded migration this database has not recorded, in
// version order, and reports how many it applied. A migration and its
// schema_version row commit together, so a half-applied schema is not a state
// the daemon can boot into.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	all, err := migrations.All()
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, migration := range all {
		ran, err := s.apply(ctx, migration)
		if err != nil {
			return applied, fmt.Errorf("apply migration %s: %w", migration.Name, err)
		}
		if ran {
			applied++
		}
	}
	return applied, nil
}

// SchemaVersion is the highest migration this database has recorded, and 0 for
// a database no daemon has migrated yet.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	recorded, err := schemaVersionExists(ctx, s.pool)
	if err != nil {
		return 0, err
	}
	if !recorded {
		return 0, nil
	}
	var version int
	if err := s.pool.QueryRow(ctx, "select coalesce(max(version), 0) from schema_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	return version, nil
}

// RecordBoot writes the row that says this daemon started and returns its id.
// The daemon holds the id for the life of the process and stamps it on the way
// out; a row with no stopped_at is a boot that never shut down.
func (s *Store) RecordBoot(ctx context.Context, project string, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		"insert into daemon_boot (project, started_at) values ($1, $2) returning id",
		project, at,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record the boot of %s: %w", project, err)
	}
	return id, nil
}

// StopBoot stamps the end of one boot. An id no row carries is refused rather
// than ignored: the caller is stamping a boot that was never recorded.
func (s *Store) StopBoot(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, "update daemon_boot set stopped_at = $2 where id = $1", id, at)
	if err != nil {
		return fmt.Errorf("stop boot %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("stop boot %d: no boot has that id", id)
	}
	return nil
}

// Boots counts a project's recorded boots and reads the earliest start among
// them — the two durable facts a restart is proved by. A project that never
// booted reads 0 and the zero time.
func (s *Store) Boots(ctx context.Context, project string) (int, time.Time, error) {
	var count int
	var firstBootAt *time.Time
	err := s.pool.QueryRow(ctx,
		"select count(*), min(started_at) from daemon_boot where project = $1", project,
	).Scan(&count, &firstBootAt)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read the boots of %s: %w", project, err)
	}
	if firstBootAt == nil {
		return count, time.Time{}, nil
	}
	return count, *firstBootAt, nil
}

// apply runs one migration unless the database already records it, and reports
// whether it ran. The check, the migration and the record share a transaction,
// and the advisory lock is held to its end.
func (s *Store) apply(ctx context.Context, migration migrations.Migration) (bool, error) {
	ran := false
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", migrateLock); err != nil {
			return fmt.Errorf("lock the schema: %w", err)
		}
		recorded, err := versionRecorded(ctx, tx, migration.Version)
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
		if _, err := tx.Exec(ctx, migration.SQL); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", migration.Version); err != nil {
			return fmt.Errorf("record version %d: %w", migration.Version, err)
		}
		ran = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return ran, nil
}

// versionRecorded answers whether this database has already taken a migration.
// A database without the schema_version table has taken none: migration 1 is
// what creates it.
func versionRecorded(ctx context.Context, tx pgx.Tx, version int) (bool, error) {
	exists, err := schemaVersionExists(ctx, tx)
	if err != nil || !exists {
		return false, err
	}
	var recorded bool
	if err := tx.QueryRow(ctx,
		"select exists (select 1 from schema_version where version = $1)", version,
	).Scan(&recorded); err != nil {
		return false, fmt.Errorf("read schema_version: %w", err)
	}
	return recorded, nil
}

// queryRower is the part of a pool and of a transaction this package reads
// through.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func schemaVersionExists(ctx context.Context, db queryRower) (bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, "select to_regclass('schema_version') is not null").Scan(&exists); err != nil {
		return false, fmt.Errorf("look for schema_version: %w", err)
	}
	return exists, nil
}

// connectError says where the daemon could not reach Postgres without saying
// how it would have got in: an operator reads this in a boot refusal, and a
// DSN's password has no business in a log line.
type connectError struct {
	address  string
	password string
	err      error
}

func (e *connectError) Error() string {
	return fmt.Sprintf("connect to Postgres at %s: %s", e.address, redactPassword(e.err.Error(), e.password))
}

// redactPassword hides a password where a driver would print one: after the
// password= of a keyword DSN, and between the : and @ of a URL. Only in those
// positions — a password equal to the user or the database name is the
// ordinary devbox DSN (postgres://legion:legion@…), and blanking every
// occurrence would hide the fields an operator reads to diagnose a refusal.
func redactPassword(detail, password string) string {
	if password == "" {
		return detail
	}
	detail = strings.ReplaceAll(detail, "password="+password, "password=xxxxx")
	return strings.ReplaceAll(detail, ":"+password+"@", ":xxxxx@")
}

func (e *connectError) Unwrap() error { return e.err }

// target reads the address a DSN points at and the password it carries. pgx
// understands both the URL and the keyword/value forms; url.Parse answers for a
// DSN pgx itself rejects, so even then the operator learns the host.
func target(dsn string) (string, string) {
	if config, err := pgconn.ParseConfig(dsn); err == nil && config.Host != "" {
		return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), config.Password
	}
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		password, _ := parsed.User.Password()
		return parsed.Host, password
	}
	return "the configured Postgres", ""
}
