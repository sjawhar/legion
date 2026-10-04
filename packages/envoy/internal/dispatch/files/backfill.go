package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the slice of a database pool the backfill runs on: what *store.Pool offers, named
// here so this package depends on no store.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	// Acquire takes one connection the caller holds until it releases it: where a pass keeps
	// its advisory lock, which Postgres scopes to the session that took it.
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// backfillLock is the advisory lock one pass of BackfillRows or RestoreRows holds for its run, so
// two passes started together (a one-off task run twice) do not both move or restore the same
// rows in lockstep. Session-scoped, so a pass that dies releases it with its connection.
const backfillLock = 0x4c45_4749_4f4e_3532 // "LEGION52"

// ErrAnotherPassRunning is the refusal when the advisory lock is held by another run.
var ErrAnotherPassRunning = errors.New("another backfill-files pass holds the lock; wait for it to finish")

// acquire takes backfillLock on a connection of pool it holds for the pass, or answers
// ErrAnotherPassRunning. The lock is the session's, so it lives on that one connection, which the
// returned release unlocks and gives back.
func acquire(ctx context.Context, pool Querier) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("take a connection for the backfill lock: %w", err)
	}
	var held bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, backfillLock).Scan(&held); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take the backfill lock: %w", err)
	}
	if !held {
		conn.Release()
		return nil, ErrAnotherPassRunning
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, backfillLock)
		conn.Release()
	}, nil
}

// fileVersion is one row of artifact_versions the passes walk: a file's version, never a
// document's.
type fileVersion struct {
	id        string
	createdAt time.Time
	// sha is nil where the row records no hash, which no upload has ever written but the schema
	// allows; a pass names such a row rather than scanning it into a string and naming nothing.
	sha  *string
	mime string
}

// nextVersion is the oldest file version after (createdAt, id) matching where, with its bytes when
// withBytes. Keyset paging, so a row a pass fails on is passed over on the next step rather than
// returned again by `limit 1` for the rest of the run.
func nextVersion(ctx context.Context, pool Querier, after *fileVersion, where string, withBytes bool) (*fileVersion, []byte, error) {
	content := "null::bytea"
	if withBytes {
		content = "v.content"
	}
	var afterAt time.Time
	var afterID string
	if after != nil {
		afterAt, afterID = after.createdAt, after.id
	}
	row := pool.QueryRow(ctx, `
		select v.id::text, v.created_at, v.sha256, coalesce(v.mime, 'application/octet-stream'), `+content+`
		from artifact_versions v
		join artifacts a on a.id = v.artifact_id
		where a.kind <> 'doc' and (`+where+`) and (v.created_at, v.id::text) > ($1, $2)
		order by v.created_at, v.id
		limit 1
	`, afterAt, afterID)
	var version fileVersion
	var body []byte
	err := row.Scan(&version.id, &version.createdAt, &version.sha, &version.mime, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the next file version: %w", err)
	}
	return &version, body, nil
}

// PassReport counts what one pass did and names each row it could not do.
type PassReport struct {
	// Done is the rows the pass moved, restored or verified.
	Done int
	// Bytes is the size of those rows' files, summed; VerifyRows reads none and leaves it zero.
	Bytes int64
	// Failed names each row the pass could not finish, with its error. The pass goes on past it.
	Failed []string
}

func (r *PassReport) fail(out io.Writer, id string, err error) {
	r.Failed = append(r.Failed, fmt.Sprintf("version %s: %v", id, err))
	fmt.Fprintf(out, "FAILED version %s: %v\n", id, err)
}

// BackfillRows moves every file version that still holds its bytes in Postgres into store, oldest
// first, one row at a time (moveRow): it writes the object, reads it back against the hash, and
// only then clears the row, in one update that clears only a row still holding them. A row it
// cannot move (a store failure, a row whose bytes do not hash to its hash) is named in the report
// and passed over, so one bad row does not hold every newer one; a run stopped anywhere leaves
// each row either done or untouched, and the next run picks up what is left. The server keeps
// serving a row from its bytes until they are cleared. A database failure, or a cancelled context,
// stops the pass with that error.
func BackfillRows(ctx context.Context, pool Querier, store Store, out io.Writer) (PassReport, error) {
	release, err := acquire(ctx, pool)
	if err != nil {
		return PassReport{}, err
	}
	defer release()
	var report PassReport
	var cursor *fileVersion
	for {
		version, body, err := nextVersion(ctx, pool, cursor, "v.content is not null", true)
		if err != nil {
			return report, err
		}
		if version == nil {
			return report, nil
		}
		cursor = version
		if version.sha == nil {
			report.fail(out, version.id, errors.New("the row records no sha256 to store its bytes under"))
			continue
		}
		cleared, err := moveRow(ctx, pool, store, version.id, *version.sha, version.mime, body)
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.fail(out, version.id, err)
			continue
		}
		if !cleared {
			// Cleared between the read and the update; the object is in the store either way, and
			// nothing here counts it twice.
			continue
		}
		report.Done++
		report.Bytes += int64(len(body))
		fmt.Fprintf(out, "moved version %s (%s, %d bytes)\n", version.id, *version.sha, len(body))
	}
}

// moveRow writes one row's bytes to the store, reads them back and checks the hash, and only then
// clears the row, in one update that clears only a row still holding them. It reports whether
// this call cleared the row.
func moveRow(ctx context.Context, pool Querier, store Store, id, sha, mime string, body []byte) (bool, error) {
	if got := SHA256(body); got != sha {
		return false, fmt.Errorf("row records sha256 %s but its bytes hash to %s", sha, got)
	}
	if err := store.Put(ctx, sha, mime, body); err != nil {
		return false, err
	}
	if err := Verify(ctx, store, sha); err != nil {
		return false, fmt.Errorf("read back: %w", err)
	}
	tag, err := pool.Exec(ctx, `update artifact_versions set content = null where id = $1 and content is not null`, id)
	if err != nil {
		return false, fmt.Errorf("clear bytes: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RestoreRows is the rollback: it reads every file version whose bytes have left Postgres back
// from store, verified against the row's hash, and writes the bytes into the row, in one update
// that fills only a row still empty. Once it has run, a server with no store configured, or an
// image from before the store existed, serves every file from its row again; the objects stay in
// the bucket, and a later BackfillRows moves the rows out once more. A row whose object is missing
// or wrong is named and passed over.
func RestoreRows(ctx context.Context, pool Querier, store Store, out io.Writer) (PassReport, error) {
	release, err := acquire(ctx, pool)
	if err != nil {
		return PassReport{}, err
	}
	defer release()
	var report PassReport
	var cursor *fileVersion
	for {
		version, _, err := nextVersion(ctx, pool, cursor, "v.content is null and v.sha256 is not null", false)
		if err != nil {
			return report, err
		}
		if version == nil {
			return report, nil
		}
		cursor = version
		body, err := ReadAll(ctx, store, *version.sha)
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.fail(out, version.id, err)
			continue
		}
		tag, err := pool.Exec(ctx, `update artifact_versions set content = $2 where id = $1 and content is null`, version.id, body)
		if err != nil {
			return report, fmt.Errorf("version %s: write bytes back: %w", version.id, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		report.Done++
		report.Bytes += int64(len(body))
		fmt.Fprintf(out, "restored version %s (%s, %d bytes)\n", version.id, *version.sha, len(body))
	}
}

// VerifyRows reads back every file version whose bytes have left Postgres and checks each object
// against the row's hash, changing nothing. A row still holding its bytes is not checked here;
// BackfillRows checks it when it moves it.
func VerifyRows(ctx context.Context, pool Querier, store Store, out io.Writer) (PassReport, error) {
	rows, err := pool.Query(ctx, `
		select v.id::text, v.sha256
		from artifact_versions v
		join artifacts a on a.id = v.artifact_id
		where a.kind <> 'doc' and v.content is null and v.sha256 is not null
		order by v.created_at, v.id
	`)
	if err != nil {
		return PassReport{}, fmt.Errorf("list the versions to verify: %w", err)
	}
	// Collected before any object is read, so no connection is held while the store answers.
	type version struct{ id, sha string }
	pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (version, error) {
		var v version
		return v, row.Scan(&v.id, &v.sha)
	})
	if err != nil {
		return PassReport{}, fmt.Errorf("list the versions to verify: %w", err)
	}
	var report PassReport
	for _, r := range pending {
		if err := Verify(ctx, store, r.sha); err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.fail(out, r.id, err)
			continue
		}
		report.Done++
	}
	return report, nil
}
