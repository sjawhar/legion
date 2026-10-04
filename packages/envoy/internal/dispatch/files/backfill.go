package files

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the slice of a database pool the backfill runs on: what *store.Pool offers, named
// here so this package depends on no store.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// BackfillReport counts what one BackfillRows pass did.
type BackfillReport struct {
	// Moved is the rows whose bytes now live in the store alone.
	Moved int
	// Bytes is the size of those rows' files, summed.
	Bytes int64
}

// BackfillRows moves every file version that still holds its bytes in Postgres into store, oldest
// first, one row at a time (moveRow). A run stopped anywhere leaves each row either done or
// untouched, so the next run picks up where it stopped; the server keeps serving a row from its
// bytes until they are cleared. Progress is written to out as it goes; a store or database
// failure stops the pass with that row's error.
func BackfillRows(ctx context.Context, pool Querier, store Store, out io.Writer) (BackfillReport, error) {
	var report BackfillReport
	for {
		// sha256 and mime are nullable in the schema though every upload writes them. A row without
		// a type is stored under the upload route's own default (the object's type is advisory:
		// the version route serves the row's); a row without a hash stops the pass naming it,
		// where a scan into a string would stop it naming no row.
		var id, mime string
		var sha *string
		var body []byte
		err := pool.QueryRow(ctx, `
			select v.id::text, v.sha256, coalesce(v.mime, 'application/octet-stream'), v.content
			from artifact_versions v
			join artifacts a on a.id = v.artifact_id
			where a.kind <> 'doc' and v.content is not null
			order by v.created_at, v.id
			limit 1
		`).Scan(&id, &sha, &mime, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return report, nil
		}
		if err != nil {
			return report, fmt.Errorf("read the next version holding bytes: %w", err)
		}
		if sha == nil {
			return report, fmt.Errorf("version %s: the row records no sha256 to store its bytes under", id)
		}
		cleared, err := moveRow(ctx, pool, store, id, *sha, mime, body)
		if err != nil {
			return report, fmt.Errorf("version %s: %w", id, err)
		}
		if !cleared {
			// Another run cleared it between the read and the update; the object is in the store
			// either way, and nothing here counts it twice.
			continue
		}
		report.Moved++
		report.Bytes += int64(len(body))
		fmt.Fprintf(out, "moved version %s (%s, %d bytes)\n", id, *sha, len(body))
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

// VerifyReport counts what one VerifyRows pass found.
type VerifyReport struct {
	// Checked is the rows whose object read back with the recorded hash.
	Checked int
	// Failed names each row whose object is missing or reads back wrong, with its error.
	Failed []string
}

// VerifyRows reads back every file version whose bytes have left Postgres and checks each object
// against the row's hash, changing nothing. A row still holding its bytes is not checked here;
// BackfillRows checks it when it moves it.
func VerifyRows(ctx context.Context, pool Querier, store Store, out io.Writer) (VerifyReport, error) {
	rows, err := pool.Query(ctx, `
		select v.id::text, v.sha256
		from artifact_versions v
		join artifacts a on a.id = v.artifact_id
		where a.kind <> 'doc' and v.content is null and v.sha256 is not null
		order by v.created_at, v.id
	`)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("list the versions to verify: %w", err)
	}
	// Collected before any object is read, so no connection is held while the store answers.
	type version struct{ id, sha string }
	pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (version, error) {
		var v version
		return v, row.Scan(&v.id, &v.sha)
	})
	if err != nil {
		return VerifyReport{}, fmt.Errorf("list the versions to verify: %w", err)
	}
	var report VerifyReport
	for _, r := range pending {
		if err := Verify(ctx, store, r.sha); err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.Failed = append(report.Failed, fmt.Sprintf("version %s: %v", r.id, err))
			fmt.Fprintf(out, "FAILED version %s: %v\n", r.id, err)
			continue
		}
		report.Checked++
	}
	return report, nil
}
