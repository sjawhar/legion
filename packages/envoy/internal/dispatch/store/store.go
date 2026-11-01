// Package store owns Dispatch's Postgres connection and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Store is the shared Dispatch database handle.
type Store struct {
	Pool *Pool
}

// Every file in migrations, so pgmigrate.Load judges each one; pgmigratetest.CheckEmbedsEveryFile
// states why the pattern is all:migrations and holds the tree to it.
//
//go:embed all:migrations
var migrationFiles embed.FS

// poolSizeParam is the connection-string parameter Open refuses. Open fixes MaxConns at
// SharedPoolSize, so a connection string that asks for a pool size is asking for something
// that will not happen, and ignoring it silently would leave the operator tuning a number
// nobody reads.
//
// The other pool parameters are not refused because they are not ignored: the shared pool
// keeps whatever minimums the connection string sets, and only the rooms and health pools
// override them, deliberately and per pool (Pool.separate).
const poolSizeParam = "pool_max_conns"

// Open connects to databaseURL and verifies the database is reachable. The pool's size is
// SharedPoolSize: how many connections Dispatch may hold is a property of Dispatch, not of the
// string that names its database. It is the request pool (RequestsPool): its connections carry
// dispatch-requests in application_name, and its acquires are bounded at requestAcquireBound.
// Every connection it opens scans timestamptz in UTC (scanTimestampsInUTC), as do the pools
// Pool.separate copies from its configuration.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	declared, err := declaresPoolSize(databaseURL)
	if err != nil {
		return nil, err
	}
	if declared {
		return nil, fmt.Errorf(
			"DATABASE_URL sets %s, which Dispatch does not honour: the pool is fixed at %d in code (store.SharedPoolSize). Remove the parameter",
			poolSizeParam, SharedPoolSize)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.MaxConns = SharedPoolSize
	configurePool(config, RequestsPool, requestAcquireBound)
	config.AfterConnect = scanTimestampsInUTC
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &Store{Pool: NewPool(pool)}, nil
}

// scanTimestampsInUTC makes every timestamptz a connection scans come back in UTC, whatever zone
// the host runs in. pgx builds a scanned timestamptz with time.Unix, which carries time.Local,
// and time.Time marshals JSON in the zone it carries - so one instant left Dispatch as
// "2026-10-02T00:00:01.12Z" from a UTC host and "2026-10-01T17:00:01.12-07:00" from a Pacific
// one, and every reader that compares a timestamp as a string (the dashboard's read-through
// mark, a client ordering replies written in the same second) read two different servers. Only
// the zone the value carries changes; the instant does not. The connection's own session
// timezone is Postgres's and is untouched, so what a timestamptz is compared with and stored as
// is unchanged too.
//
// Registered per connection rather than in pgtype's default map, which is shared with every
// other pgx user in the process, and for the array and range types over timestamptz as well:
// each holds the element type as it was registered when the map was built, so re-registering the
// element alone would leave a timestamptz[] scanning in the host's zone.
func scanTimestampsInUTC(_ context.Context, conn *pgx.Conn) error {
	types := conn.TypeMap()
	timestamptz := &pgtype.Type{
		Name: "timestamptz", OID: pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	}
	tstzrange := &pgtype.Type{
		Name: "tstzrange", OID: pgtype.TstzrangeOID, Codec: &pgtype.RangeCodec{ElementType: timestamptz},
	}
	types.RegisterTypes([]*pgtype.Type{
		timestamptz,
		tstzrange,
		{Name: "_timestamptz", OID: pgtype.TimestamptzArrayOID, Codec: &pgtype.ArrayCodec{ElementType: timestamptz}},
		{Name: "_tstzrange", OID: pgtype.TstzrangeArrayOID, Codec: &pgtype.ArrayCodec{ElementType: tstzrange}},
		{Name: "tstzmultirange", OID: pgtype.TstzmultirangeOID, Codec: &pgtype.MultirangeCodec{ElementType: tstzrange}},
	})
	return nil
}

// declaresPoolSize reports whether databaseURL carries poolSizeParam. It asks pgx.ParseConfig
// rather than pgxpool.ParseConfig or the raw string. pgxpool is the layer that consumes the
// parameter - it folds it into MaxConns and deletes it from RuntimeParams - so its parse
// cannot answer the question, while the layer underneath it leaves the parameter there.
// Scanning the string instead would answer a different question: keyword/value values may be
// quoted or backslash-escaped and may contain the parameter's own name, and a keyword may be
// separated from its "=" by whitespace, so a scanner refuses valid passwords and misses
// "pool_max_conns = 4", which pgx honours.
func declaresPoolSize(databaseURL string) (bool, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return false, fmt.Errorf("parse Postgres URL: %w", err)
	}
	_, declared := config.RuntimeParams[poolSizeParam]
	return declared, nil
}

// Migrate applies the embedded migrations in version order, each committed with its version
// record in one transaction. It refuses a set pgmigrate.Load refuses before it touches the
// database, so a set it cannot apply as written applies nothing, and it bounds every lock wait a
// migration makes by pgmigrate.LockTimeout.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	if s == nil || s.Pool == nil {
		return fmt.Errorf("migrate: store pool required")
	}
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	// Only a set holding a version past an integer widens the column: see createVersionTable.
	widen := slices.ContainsFunc(migrations, func(m pgmigrate.Migration) bool { return m.Version > math.MaxInt32 })
	if err := s.createVersionTable(ctx, widen); err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := s.applyMigration(ctx, migration); err != nil {
			return fmt.Errorf("apply migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

// migrationLockKey is the pg_advisory_xact_lock every Dispatch process's runner takes, so two
// processes booting at once migrate one after the other. The auto-deployer
// (deploy/scripts/autodeploy.sh, schema_version) takes it too before it reads schema_migrations, so
// a migration in flight ends first; a change of key changes both.
const migrationLockKey int64 = 8150001

// createVersionTable creates schema_migrations under the runner's advisory lock, as the broker's
// runner creates its own, and, when widen is set, widens the version column of a database that has
// the table from before the versions became timestamps. Created outside the lock, two processes
// booting at once against an empty database race `create table if not exists` on the catalog, and
// the loser exits on pg_type's unique index.
//
// The column is a bigint because a migration added since the set stopped being numbered is named
// for the UTC moment it was created, 14 digits, which is past the 2147483647 an integer column
// holds. A database created before this change has the integer the statement below used to create.
// It is widened only by a boot whose set holds such a version (widen), as the daemon widens its
// own in the first timestamped migration's transaction, and not by a boot that records nothing past
// an integer: the release before this change reads `max(version)` uncast, so a widening while its
// processes still serve fails that read once on every connection that prepared it ("cached plan
// must not change result type"). A release whose set first holds a timestamped migration is rolled
// out over processes of this one, which read the version cast. The type is read before the ALTER
// TABLE, which on a column already bigint rewrites nothing but still takes ACCESS EXCLUSIVE.
func (s *Store) createVersionTable(ctx context.Context, widen bool) error {
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		create table if not exists schema_migrations (
			version bigint primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	if !widen {
		return tx.Commit(ctx)
	}
	var dataType string
	if err := tx.QueryRow(ctx, `select data_type from information_schema.columns
		where table_schema = current_schema() and table_name = 'schema_migrations' and column_name = 'version'`).Scan(&dataType); err != nil {
		return fmt.Errorf("read the type of schema_migrations.version: %w", err)
	}
	if dataType == "integer" {
		if _, err := tx.Exec(ctx, "alter table schema_migrations alter column version type bigint"); err != nil {
			return fmt.Errorf("widen schema_migrations.version to bigint: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// applyMigration applies one migration in a transaction of its own, trying it again while its lock
// wait outlasts its lock_timeout, up to pgmigrate.LockTimeoutAttempts times, holding nothing between
// attempts (pgmigrate.RetryLockTimeouts). A migration that times out on every attempt fails the
// runner, and boot with it.
func (s *Store) applyMigration(ctx context.Context, migration pgmigrate.Migration) error {
	return pgmigrate.RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		return s.applyMigrationOnce(ctx, migration)
	})
}

func (s *Store) applyMigrationOnce(ctx context.Context, migration pgmigrate.Migration) error {
	// A migration is a transaction like any other, so it is marked like any other: nothing it
	// runs may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	var applied bool
	// The parameter is cast, here and in the insert below, because pgx encodes it for the column
	// it finds: a database whose column this boot is about to widen, or one widened by a boot
	// whose statement this connection has not prepared again, would otherwise refuse a 14-digit
	// version as too large for an int4.
	if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1::bigint)", migration.Version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	if applied {
		return tx.Commit(ctx)
	}
	if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
		return fmt.Errorf("execute migration: %w", err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1::bigint)", migration.Version); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}
