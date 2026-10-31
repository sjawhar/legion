// Package ompsessions is Oh My Pi's SQL session table as Legion uses it, and the settled
// runtime.kubernetes.session_store the daemon reads it under (store.go: the union, the file name
// DSNFileVariable points at, libpq's own name for a unix socket, and the two URL spellings of one
// unix-socket endpoint). The table is Oh My Pi's own (sql-session-storage.ts at
// the release .omp-pin names): it creates and migrates omp_session_files and
// omp_session_files_parts when it starts, keys a session by the path its file would have had (the
// session file a claim records), and reads a session as the row's content followed by its parts in
// offset order. Legion reads it to hold a resume to a session the table holds (Written, the role
// launcher's check), seeds it once from the files sessions were kept in before (Import,
// `legion sessions import`), and keeps a Claude Code session's files in it as Oh My Pi's own writes
// would (Append and Replace, the role launcher's, which no Oh My Pi runs beside).
package ompsessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// Oh My Pi's two session-storage variables (session-storage-config.ts): StorageVariable set to
// SQLStorage keeps every session in the database whose postgres:// URL the file DSNFileVariable
// names holds. Any other storage keeps each session a file.
const (
	StorageVariable = "OMP_SESSION_STORAGE"
	DSNFileVariable = "OMP_SESSION_SQL_DSN_FILE"
	SQLStorage      = "sql"
)

// createTables are Oh My Pi's own Postgres statements for its two tables at the pinned release
// (buildQueries' createTable and createPartsTable). Each is IF NOT EXISTS, so they change nothing
// Oh My Pi already made, which it migrates itself when it starts.
var createTables = []string{
	"CREATE TABLE IF NOT EXISTS omp_session_files (path TEXT PRIMARY KEY, content TEXT NOT NULL, mtime_ms BIGINT NOT NULL, " +
		"title TEXT, title_source TEXT, title_updated_at TEXT, byte_len BIGINT)",
	"CREATE TABLE IF NOT EXISTS omp_session_files_parts (path TEXT NOT NULL, start_offset BIGINT NOT NULL, content TEXT NOT NULL, " +
		"PRIMARY KEY (path, start_offset))",
}

// CreateTables makes Oh My Pi's two tables where the database has none (createTables), so a
// database no Oh My Pi has opened yet takes a session. Import runs it before its write, and a
// writer of Append and Replace once per connection before its first.
func CreateTables(ctx context.Context, conn *pgx.Conn) error {
	for _, statement := range createTables {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create Oh My Pi's session tables: %w", err)
		}
	}
	return nil
}

// undefinedTable is Postgres's SQLSTATE for a relation that does not exist.
const undefinedTable = "42P01"

// ReadDSN is the connection URL a DSN file holds, trimmed as Oh My Pi trims it. Every caller reads
// the session database's: Connect, which opens it, and the launcher's own resume lookup
// (launcher.sessionWritten), which reads the file afresh before each attempt so a URL rewritten
// under it is picked up. Its refusals name the file, never what it holds.
func ReadDSN(file string) (string, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read the database URL file %s: %w", file, err)
	}
	dsn := strings.TrimSpace(string(body))
	if dsn == "" {
		return "", fmt.Errorf("the database URL file %s is empty", file)
	}
	return dsn, nil
}

// Connect opens the session database whose URL dsnFile holds. A refusal names the database's
// address, never the URL's password (store.ConnectError).
func Connect(ctx context.Context, dsnFile string) (*pgx.Conn, error) {
	dsn, err := ReadDSN(dsnFile)
	if err != nil {
		return nil, err
	}
	return ConnectURL(ctx, dsn)
}

// ConnectURL opens the session database at dsn, a URL ReadDSN read. A refusal names the
// database's address, never the URL's password (store.ConnectError).
func ConnectURL(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, store.ConnectError(dsn, err)
	}
	return conn, nil
}

// Written is when the session the table holds at path was last written: the row's mtime_ms, which
// Oh My Pi sets on every write, an append included, and Import sets to the copied file's. ok is false
// when the table holds no such session; a database no Oh My Pi has opened has no table yet, and so
// holds none.
func Written(ctx context.Context, conn *pgx.Conn, path string) (at time.Time, ok bool, err error) {
	var mtime int64
	err = conn.QueryRow(ctx, "SELECT mtime_ms FROM omp_session_files WHERE path = $1", path).Scan(&mtime)
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("look up session %s: %w", path, err)
	}
	return time.UnixMilli(mtime), true, nil
}

// Read is the session the table holds at path as Oh My Pi reads it (readFull): the row's content,
// then each of its parts in offset order, in one statement so both come from one snapshot. ok is
// false when the table holds no such session.
func Read(ctx context.Context, conn *pgx.Conn, path string) (content []byte, ok bool, err error) {
	rows, err := conn.Query(ctx,
		"SELECT 0 AS kind, 0::bigint AS start_offset, content FROM omp_session_files WHERE path = $1 "+
			"UNION ALL SELECT 1, start_offset, content FROM omp_session_files_parts WHERE path = $1 "+
			"ORDER BY kind, start_offset", path)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read session %s: %w", path, err)
	}
	defer rows.Close()
	var buffer bytes.Buffer
	for rows.Next() {
		var kind int
		var offset int64
		var piece string
		if err := rows.Scan(&kind, &offset, &piece); err != nil {
			return nil, false, fmt.Errorf("read session %s: %w", path, err)
		}
		if kind == 0 {
			ok = true
		}
		buffer.WriteString(piece)
	}
	if err := rows.Err(); err != nil {
		if isUndefinedTable(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read session %s: %w", path, err)
	}
	if !ok {
		return nil, false, nil
	}
	return buffer.Bytes(), true, nil
}

// Outcome is what Import did with a session.
type Outcome string

const (
	// Copied is a session Import wrote.
	Copied Outcome = "copied"
	// Identical is a session the table already held, byte for byte.
	Identical Outcome = "identical"
)

// Summary names a session's content without carrying it: its size in bytes and its sha256.
type Summary struct {
	Bytes  int
	SHA256 string
}

func (s Summary) String() string { return fmt.Sprintf("%d bytes, sha256 %s", s.Bytes, s.SHA256) }

// Summarize is content's Summary.
func Summarize(content []byte) Summary {
	sum := sha256.Sum256(content)
	return Summary{Bytes: len(content), SHA256: hex.EncodeToString(sum[:])}
}

// ConflictError is Import's refusal of a session the table already holds with other content: an
// agent has written it under SQL storage since, or another file was copied there.
type ConflictError struct {
	Path         string
	Held, Copied Summary
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the session table already holds %s with other content (%s; the file is %s): it is left as it is",
		e.Path, e.Held, e.Copied)
}

// Import writes content as the session at path the way Oh My Pi reads a row an older release of it
// wrote: the whole of it as the row's content, byte_len its size in bytes, mtime_ms mtime, no title
// and no parts, with any part left under path by an earlier session deleted in the same
// transaction, since a session reads as its row followed by its parts. It first creates the two
// tables when the database has none (createTables). A session the table already holds is left as
// it is: Identical when it is content byte for byte, a *ConflictError otherwise. Content Postgres
// cannot keep as text, a byte sequence that is not UTF-8 or a NUL byte, Postgres refuses itself, and
// the transaction writes nothing.
func Import(ctx context.Context, conn *pgx.Conn, path string, content []byte, mtime time.Time) (Outcome, error) {
	if err := CreateTables(ctx, conn); err != nil {
		return "", err
	}
	inserted := false
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "INSERT INTO omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4) "+
			"ON CONFLICT (path) DO NOTHING", path, string(content), mtime.UnixMilli(), len(content))
		if err != nil {
			return err
		}
		if inserted = tag.RowsAffected() == 1; !inserted {
			return nil
		}
		_, err = tx.Exec(ctx, "DELETE FROM omp_session_files_parts WHERE path = $1", path)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("write session %s: %w", path, err)
	}
	if inserted {
		return Copied, nil
	}
	held, ok, err := Read(ctx, conn, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("session %s was in the table when the copy was written and gone when it was compared: copy it again", path)
	}
	if !bytes.Equal(held, content) {
		return "", &ConflictError{Path: path, Held: Summarize(held), Copied: Summarize(content)}
	}
	return Identical, nil
}

// Absent is the byte_len a writer holds for a session the table has no row for.
const Absent int64 = -1

// partBytes is the largest part Replace stores, Oh My Pi's PART_BYTES: a full write is split into
// parts of at most 1 MiB, where an append stores what it adds as one part.
const partBytes = 1024 * 1024

// MovedError is Append's and Replace's refusal of a session whose row is not the one its writer
// last saved: another writer moved it since, and nothing was written. Saved is the byte_len the
// writer last saved and Held the one the table holds, either Absent for no row.
type MovedError struct {
	Path        string
	Saved, Held int64
}

func (e *MovedError) Error() string {
	return fmt.Sprintf("session %s moved under its writer: the table holds %s where the writer last saved %s",
		e.Path, describeLength(e.Held), describeLength(e.Saved))
}

func describeLength(n int64) string {
	if n == Absent {
		return "no row"
	}
	return fmt.Sprintf("%d bytes", n)
}

// Append adds content to the end of the session at path as Oh My Pi's own append does
// (sql-session-storage.ts's append): one part at the row's old byte_len holding all of content, the
// row's byte_len grown by content's size in bytes and its mtime_ms set to mtime, in one
// transaction. It is a compare-and-set on the row: the row is locked first (lockSize's SELECT …
// FOR UPDATE) and written only when its byte_len is saved, what the writer last saved, so a row
// another writer moved, or none, is a *MovedError and nothing is written. The tables must exist
// (CreateTables). Content Postgres cannot keep as text, a byte sequence that is not UTF-8 or a NUL
// byte, Postgres refuses itself, naming the session, and the transaction writes nothing.
func Append(ctx context.Context, conn *pgx.Conn, path string, saved int64, content []byte, mtime time.Time) error {
	if saved == Absent {
		return fmt.Errorf("write session %s: the table holds no row to append to", path)
	}
	return write(ctx, conn, path, saved, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
			path, saved, string(content)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE omp_session_files SET byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, saved+int64(len(content)), mtime.UnixMilli())
		return err
	})
}

// Replace writes content as the whole session at path as Oh My Pi's own full write does
// (writeFull): the row's content empty, its byte_len content's size in bytes and its mtime_ms
// mtime, every part under path deleted, and content stored as parts of at most partBytes, each
// ending on a UTF-8 character boundary, in one transaction. It is a compare-and-set as Append is:
// with saved Absent the row is written only when path has none (insertRowIfMissing), and
// otherwise only when the locked row's byte_len is saved; any other row is a *MovedError and
// nothing is written. The tables must exist (CreateTables), and content Postgres cannot keep as
// text is refused as Append's is.
func Replace(ctx context.Context, conn *pgx.Conn, path string, saved int64, content []byte, mtime time.Time) error {
	return write(ctx, conn, path, saved, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE omp_session_files SET content = '', byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, len(content), mtime.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM omp_session_files_parts WHERE path = $1", path); err != nil {
			return err
		}
		for start := 0; start < len(content); {
			end := min(start+partBytes, len(content))
			// Back off continuation bytes (0b10xxxxxx), so no character straddles two parts.
			for end < len(content) && content[end]&0xc0 == 0x80 {
				end--
			}
			if _, err := tx.Exec(ctx, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
				path, start, string(content[start:end])); err != nil {
				return err
			}
			start = end
		}
		return nil
	})
}

// write runs apply in one transaction once the row at path is the one its writer last saved:
// saved Absent inserts an empty row in its place when path has none, and otherwise the row is
// locked and its byte_len compared. A row it is not is a *MovedError.
func write(ctx context.Context, conn *pgx.Conn, path string, saved int64, apply func(pgx.Tx) error) error {
	var moved *MovedError
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if saved == Absent {
			var inserted string
			err := tx.QueryRow(ctx, "INSERT INTO omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, '', 0, 0) "+
				"ON CONFLICT (path) DO NOTHING RETURNING path", path).Scan(&inserted)
			if err == nil {
				return apply(tx)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		held := Absent
		err := tx.QueryRow(ctx, "SELECT byte_len FROM omp_session_files WHERE path = $1 FOR UPDATE", path).Scan(&held)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if held != saved {
			moved = &MovedError{Path: path, Saved: saved, Held: held}
			return moved
		}
		return apply(tx)
	})
	if moved != nil {
		return moved
	}
	if err != nil {
		return fmt.Errorf("write session %s: %w", path, err)
	}
	return nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}
