package ompsessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// appendStatement is Oh My Pi's own PostgreSQL append at the pinned release (sql-session-storage.ts
// buildQueries' append): grow the session row's byte_len by the line's length, taking its row
// lock, and insert the line as a part at the old length. Its parameters are the path, mtime_ms,
// the length in bytes and the content.
const appendStatement = "WITH s AS (INSERT INTO omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, '', $2, $3) " +
	"ON CONFLICT (path) DO UPDATE SET byte_len = omp_session_files.byte_len + excluded.byte_len, " +
	"mtime_ms = excluded.mtime_ms RETURNING byte_len) " +
	"INSERT INTO omp_session_files_parts (path, start_offset, content) " +
	"SELECT $1, s.byte_len - $3, $4 FROM s WHERE $3 > 0"

// MovedError is Append's refusal of a session that is not the one its caller read: the table holds
// no such session, or its byte_len is not the length the caller read. It Is conversation.ErrMoved.
type MovedError struct {
	Path string
	// Read is the length the caller read; Now the row's byte_len, -1 when the table holds no such
	// session or the row has none.
	Read, Now int64
}

func (e *MovedError) Error() string {
	if e.Now < 0 {
		return fmt.Sprintf("session %s: %s: the table holds no length for it (read %d bytes)", e.Path, conversation.ErrMoved, e.Read)
	}
	return fmt.Sprintf("session %s: %s: read %d bytes, now %d", e.Path, conversation.ErrMoved, e.Read, e.Now)
}

// Is makes a MovedError match conversation.ErrMoved.
func (e *MovedError) Is(target error) bool { return target == conversation.ErrMoved }

// Append writes data after the session the table holds at path, as Oh My Pi appends a line, in one
// transaction that first reads the row's byte_len FOR UPDATE and refuses with a *MovedError when it
// is not readLen, the length of the content the caller built data after: Oh My Pi or another
// appender wrote since. mtime is the row's new mtime_ms. data must be text Postgres keeps (UTF-8
// with no NUL), as every line Oh My Pi writes is.
func Append(ctx context.Context, conn *pgx.Conn, path string, readLen int64, data []byte, mtime time.Time) error {
	if len(data) == 0 {
		return fmt.Errorf("append to session %s: nothing to append", path)
	}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var byteLen *int64
		err := tx.QueryRow(ctx, "SELECT byte_len FROM omp_session_files WHERE path = $1 FOR UPDATE", path).Scan(&byteLen)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || isUndefinedTable(err):
			return &MovedError{Path: path, Read: readLen, Now: -1}
		case err != nil:
			return err
		case byteLen == nil:
			return &MovedError{Path: path, Read: readLen, Now: -1}
		case *byteLen != readLen:
			return &MovedError{Path: path, Read: readLen, Now: *byteLen}
		}
		_, err = tx.Exec(ctx, appendStatement, path, mtime.UnixMilli(), int64(len(data)), string(data))
		return err
	})
	var moved *MovedError
	if errors.As(err, &moved) {
		return moved
	}
	if err != nil {
		return fmt.Errorf("append to session %s: %w", path, err)
	}
	return nil
}
