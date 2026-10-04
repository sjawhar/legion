package files

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
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
// first, one row at a time: it writes the object, reads it back and checks the hash, and only then
// clears the row's bytes, in a transaction that re-checks the row still holds them. A run stopped
// anywhere leaves each row either done or untouched, so the next run picks up where it stopped;
// the server keeps serving a row from its bytes until they are cleared. Progress is written to
// out as it goes; a store or database failure stops the pass with that row's error.
func BackfillRows(ctx context.Context, pool Querier, store Store, out io.Writer) (BackfillReport, error) {
	var report BackfillReport
	for {
		var id, sha, mime string
		var body []byte
		err := pool.QueryRow(ctx, `
			select v.id::text, v.sha256, v.mime, v.content
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
		if got := SHA256(body); got != sha {
			return report, fmt.Errorf("version %s: row records sha256 %s but its bytes hash to %s", id, sha, got)
		}
		if err := store.Put(ctx, sha, mime, body); err != nil {
			return report, fmt.Errorf("version %s: %w", id, err)
		}
		if err := Verify(ctx, store, sha); err != nil {
			return report, fmt.Errorf("version %s: read back: %w", id, err)
		}
		tag, err := pool.Exec(ctx, `update artifact_versions set content = null where id = $1 and content is not null`, id)
		if err != nil {
			return report, fmt.Errorf("version %s: clear bytes: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			// Another run cleared it between the read and the update; the object is in the store
			// either way, and nothing here counts it twice.
			continue
		}
		report.Moved++
		report.Bytes += int64(len(body))
		fmt.Fprintf(out, "moved version %s (%s, %d bytes)\n", id, sha, len(body))
	}
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
	defer rows.Close()
	type row struct{ id, sha string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.sha); err != nil {
			return VerifyReport{}, fmt.Errorf("read a version to verify: %w", err)
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
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
