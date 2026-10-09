package ompsessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

const sessionPath = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces--/2026-10-08T12-00-00-000Z_0001.jsonl"

// session is a two-entry session file, with a multi-byte character so its byte length and its
// character count differ.
const session = `{"type":"session","version":3,"id":"0001","cwd":"/legion/workspaces/sjawhar/legion/legion-654"}
{"type":"message","id":"a1","message":{"role":"user","content":"remember the nonce — BETA"}}
`

func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	_, file := testpg.DSNFile(t, "legion_ompsessions_test")
	conn, err := Connect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// A copied session is the row Oh My Pi reads as one an older release of it wrote: the whole file as
// the row's content, byte_len its size in bytes (not characters), mtime_ms the file's, no title and
// no parts, in a database no Oh My Pi has opened yet, whose two tables the copy makes.
func TestImportWritesTheRowOhMyPiReadsAsAnOlderReleases(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	mtime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	outcome, err := Import(ctx, conn, sessionPath, []byte(session), mtime)
	if err != nil || outcome != Copied {
		t.Fatalf("Import = %q, %v; want copied", outcome, err)
	}
	var content string
	var byteLen, mtimeMs int64
	var title, titleSource, titleUpdatedAt *string
	if err := conn.QueryRow(ctx, "SELECT content, byte_len, mtime_ms, title, title_source, title_updated_at FROM omp_session_files WHERE path = $1",
		sessionPath).Scan(&content, &byteLen, &mtimeMs, &title, &titleSource, &titleUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if content != session || byteLen != int64(len(session)) || byteLen == int64(len([]rune(session))) || mtimeMs != mtime.UnixMilli() {
		t.Errorf("row = content %q, byte_len %d, mtime_ms %d; want the file, %d bytes, %d", content, byteLen, mtimeMs, len(session), mtime.UnixMilli())
	}
	if title != nil || titleSource != nil || titleUpdatedAt != nil {
		t.Errorf("title columns = %v, %v, %v; want none", title, titleSource, titleUpdatedAt)
	}
	var parts int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM omp_session_files_parts WHERE path = $1", sessionPath).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if parts != 0 {
		t.Errorf("%d parts, want none", parts)
	}
	if read, ok, err := Read(ctx, conn, sessionPath); err != nil || !ok || string(read) != session {
		t.Errorf("Read = %q, %t, %v; want the session", read, ok, err)
	}
}

// A copy run again copies nothing twice: a session the table holds byte for byte is identical, and
// one it holds with other content, an agent's entries appended under SQL storage as parts or another
// file, is refused and left as it was, naming both sizes and digests.
func TestImportIsIdempotentAndRefusesARowWithOtherContent(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if _, err := Import(ctx, conn, sessionPath, []byte(session), time.Now()); err != nil {
		t.Fatal(err)
	}
	if outcome, err := Import(ctx, conn, sessionPath, []byte(session), time.Now()); err != nil || outcome != Identical {
		t.Fatalf("Import again = %q, %v; want identical", outcome, err)
	}
	appended := `{"type":"message","id":"a2","message":{"role":"assistant","content":"BETA noted"}}` + "\n"
	if _, err := conn.Exec(ctx, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
		sessionPath, len(session), appended); err != nil {
		t.Fatal(err)
	}
	held := session + appended
	if read, _, err := Read(ctx, conn, sessionPath); err != nil || string(read) != held {
		t.Fatalf("Read = %q, %v; want the row then its part", read, err)
	}
	_, err := Import(ctx, conn, sessionPath, []byte(session), time.Now())
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Import over a row an agent appended to = %v, want a ConflictError", err)
	}
	if conflict.Held != Summarize([]byte(held)) || conflict.Copied != Summarize([]byte(session)) {
		t.Errorf("conflict = %+v, want the row's %v and the file's %v", conflict, Summarize([]byte(held)), Summarize([]byte(session)))
	}
	if !strings.Contains(err.Error(), "already holds "+sessionPath+" with other content") {
		t.Errorf("refusal %q does not name the session", err)
	}
	if read, _, _ := Read(ctx, conn, sessionPath); string(read) != held {
		t.Errorf("after the refusal the session reads %q, want it left as it was", read)
	}
}

// A part left under a path with no row (an earlier session's, deleted) would follow the copied
// content when Oh My Pi reads the session, so the copy deletes it in the same transaction.
func TestImportDropsPartsAPathWithNoRowLeft(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	for _, statement := range createTables {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, 0, 'orphan')", sessionPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, conn, sessionPath, []byte(session), time.Now()); err != nil {
		t.Fatal(err)
	}
	if read, _, err := Read(ctx, conn, sessionPath); err != nil || string(read) != session {
		t.Errorf("Read = %q, %v; want the copied session alone", read, err)
	}
}

// Written is the launcher's check: no session in a database no Oh My Pi has opened, the copied one
// once copied, at the mtime it was copied with to the millisecond mtime_ms keeps, and no other.
func TestWrittenFindsOnlyASessionTheTableHoldsAndWhenItWasWritten(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if _, found, err := Written(ctx, conn, sessionPath); err != nil || found {
		t.Fatalf("Written before any table = %t, %v; want false", found, err)
	}
	mtime := time.Date(2026, 10, 8, 12, 0, 0, 123456789, time.UTC)
	if _, err := Import(ctx, conn, sessionPath, []byte(session), mtime); err != nil {
		t.Fatal(err)
	}
	if at, found, err := Written(ctx, conn, sessionPath); err != nil || !found || !at.Equal(mtime.Truncate(time.Millisecond)) {
		t.Errorf("Written of the copied session = %v, %t, %v; want %v", at, found, err, mtime.Truncate(time.Millisecond))
	}
	if _, found, err := Written(ctx, conn, sessionPath+".other"); err != nil || found {
		t.Errorf("Written of another session = %t, %v; want false", found, err)
	}
}

// Content Postgres cannot keep as text is refused before anything is written.
func TestImportRefusesContentNoTextValueHolds(t *testing.T) {
	conn := connect(t)
	for name, content := range map[string][]byte{"not UTF-8": {0xff, 0xfe, '\n'}, "a NUL byte": []byte("{}\x00\n")} {
		t.Run(name, func(t *testing.T) {
			if _, err := Import(context.Background(), conn, sessionPath, content, time.Now()); err == nil {
				t.Fatal("Import succeeded, want a refusal")
			}
			if _, found, _ := Written(context.Background(), conn, sessionPath); found {
				t.Error("a refused copy wrote the row")
			}
		})
	}
}

// Connect's refusals name the URL file or the database's address, never the URL's password.
func TestConnectNamesTheFileOrTheAddressAndNeverThePassword(t *testing.T) {
	dir := t.TempDir()
	if _, err := Connect(context.Background(), filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "absent")) {
		t.Errorf("Connect of a missing file = %v, want it named", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("Connect of an empty file = %v, want it refused as empty", err)
	}
	unreachable := filepath.Join(dir, "unreachable")
	if err := os.WriteFile(unreachable, []byte("postgres://legion:hunter2@127.0.0.1:1/sessions"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Connect(context.Background(), unreachable)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Connect of an unreachable database = %v, want its address and not its password", err)
	}
}
