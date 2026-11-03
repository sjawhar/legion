// Package settings keeps the settings changes the daemon took from its route, and is where the
// daemon's two settings arms — the file under it, and the route — reach the one settings holder
// (config.Live).
//
// The arms are Keeper's: Route for a caller's change, File for the file changing on disk, and
// Restore for the boot that lays the kept route changes back over the file. Each is one call of
// config.Live.Apply and there is no second resolution path anywhere in the daemon, which
// TestEachSettingsArmHasOneApplyCallSite holds the module's sources to.
package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Change is one stored settings change: the dotted `legion.yaml` path, the value the route asked
// for, the digest of what the file itself said for that path when the change was applied, where
// the change came from, who asked, and when the daemon applied it.
type Change struct {
	Path  string
	Value json.RawMessage
	// FileDigest is Digest of the file's own value for Path at the moment of the change
	// (config.Live.FileValues). A file whose value for Path has moved since has taken the key
	// back, and this row goes with it.
	FileDigest string
	Source     string
	By         string
	At         time.Time
}

// Digest is how a row records a value of the file for comparison: the sha256 of the value as
// config.Live.FileValues encodes it, since equality is all the comparison needs and the file's
// own value may be a credential or an address carrying one.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Store is the daemon's table of route changes, scoped to one project: one database serves every
// project's daemon, and each reads and writes its own project's rows alone.
type Store struct {
	pool    *pgxpool.Pool
	project string
}

// NewStore is the store of project's route changes over pool.
func NewStore(pool *pgxpool.Pool, project string) *Store {
	return &Store{pool: pool, project: project}
}

// undefinedTable is Postgres's code for a table that does not exist (42P01).
const undefinedTable = "42P01"

// Rows is every stored change of this project, by path. A database this daemon has not migrated
// yet carries no route change — the table arrives with the migration of this very release, and
// the boot reads the rows before it migrates — so a missing table answers none rather than
// refusing the boot.
func (s *Store) Rows(ctx context.Context) ([]Change, error) {
	rows, err := s.pool.Query(ctx, `select path, value, file_digest, source, changed_by, changed_at
		from settings_overlay where project = $1 order by path`, s.project)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return nil, nil
		}
		return nil, fmt.Errorf("read the settings overlay of %s: %w", s.project, err)
	}
	defer rows.Close()
	var stored []Change
	for rows.Next() {
		var change Change
		if err := rows.Scan(&change.Path, &change.Value, &change.FileDigest, &change.Source, &change.By, &change.At); err != nil {
			return nil, fmt.Errorf("read the settings overlay of %s: %w", s.project, err)
		}
		change.At = change.At.UTC()
		stored = append(stored, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the settings overlay of %s: %w", s.project, err)
	}
	return stored, nil
}

// Record writes each change, replacing the row that path already holds: the latest change of a
// key wins, and the row carries who made that latest one and when.
func (s *Store) Record(ctx context.Context, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, change := range changes {
		batch.Queue(`insert into settings_overlay (project, path, value, file_digest, source, changed_by, changed_at)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (project, path) do update set value = excluded.value, file_digest = excluded.file_digest,
				source = excluded.source, changed_by = excluded.changed_by, changed_at = excluded.changed_at`,
			s.project, change.Path, []byte(change.Value), change.FileDigest, change.Source, change.By, change.At.UTC())
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("record the settings change of %s: %w", s.project, err)
	}
	return nil
}

// Forget deletes the rows of paths: the file has changed those keys, so the route change that
// stood above each is over.
func (s *Store) Forget(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `delete from settings_overlay where project = $1 and path = any($2)`,
		s.project, paths); err != nil {
		return fmt.Errorf("forget the settings changes of %s: %w", s.project, err)
	}
	return nil
}
