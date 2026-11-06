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

// Content Postgres cannot keep as text is refused, by Postgres itself, naming the session, and
// nothing is written.
func TestImportRefusesContentNoTextValueHolds(t *testing.T) {
	conn := connect(t)
	for name, content := range map[string][]byte{"not UTF-8": {0xff, 0xfe, '\n'}, "a NUL byte": []byte("{}\x00\n")} {
		t.Run(name, func(t *testing.T) {
			_, err := Import(context.Background(), conn, sessionPath, content, time.Now())
			if err == nil || !strings.Contains(err.Error(), "write session "+sessionPath) || !strings.Contains(err.Error(), "invalid byte sequence") {
				t.Fatalf("Import = %v, want the write of %s refused for its invalid byte sequence", err, sessionPath)
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

// claudeTranscript is a Claude Code transcript's path, the key its role launcher keeps it under,
// and claudeRecord its phase-reminder record's.
const (
	claudeTranscript = "/legion/scratch/state/implementer/claude/projects/-legion-workspaces-legion-208/0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c.jsonl"
	claudeRecord     = "/legion/scratch/state/implementer/claude/legion-phase-stall-0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c.json"
)

// connectWithTables is a connection to a fresh database that holds Oh My Pi's two tables, as a
// writer of Append and Replace makes them once it connects.
func connectWithTables(t *testing.T) *pgx.Conn {
	t.Helper()
	conn := connect(t)
	if err := CreateTables(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

// partsOf is the parts the table holds under path, by offset.
func partsOf(t *testing.T, conn *pgx.Conn, path string) map[int64]string {
	t.Helper()
	rows, err := conn.Query(context.Background(), "SELECT start_offset, content FROM omp_session_files_parts WHERE path = $1", path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	parts := map[int64]string{}
	for rows.Next() {
		var offset int64
		var content string
		if err := rows.Scan(&offset, &content); err != nil {
			t.Fatal(err)
		}
		parts[offset] = content
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return parts
}

// A Claude Code session is written in Oh My Pi's own form, so Read and Oh My Pi read it back as
// written: a whole write is the row with no content of its own, byte_len its size in bytes and
// mtime_ms the file's, and the bytes as parts of at most 1 MiB, none splitting a character; an
// append is one part at the old byte_len, which grows by the part's bytes, mtime_ms moving with it.
func TestAClaudeSaveWritesOhMyPisOwnForm(t *testing.T) {
	conn := connectWithTables(t)
	ctx := context.Background()
	// A multi-byte character straddles the 1 MiB mark, so the split backs off to before it.
	line := strings.Repeat("a", partBytes-1) + "—" + "\n"
	written := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := Replace(ctx, conn, claudeTranscript, Absent, []byte(line), written); err != nil {
		t.Fatal(err)
	}
	var content string
	var byteLen, mtimeMs int64
	row := "SELECT content, byte_len, mtime_ms FROM omp_session_files WHERE path = $1"
	if err := conn.QueryRow(ctx, row, claudeTranscript).Scan(&content, &byteLen, &mtimeMs); err != nil {
		t.Fatal(err)
	}
	if content != "" || byteLen != int64(len(line)) || mtimeMs != written.UnixMilli() {
		t.Errorf("row = content %q, byte_len %d, mtime_ms %d; want none of its own, %d, %d", content, byteLen, mtimeMs, len(line), written.UnixMilli())
	}
	if parts := partsOf(t, conn, claudeTranscript); len(parts) != 2 || parts[0] != line[:partBytes-1] || parts[partBytes-1] != "—\n" {
		t.Errorf("a whole write of %d bytes left %d parts, want the bytes before the character at 0 and the rest at %d", len(line), len(parts), partBytes-1)
	}
	appended := `{"type":"assistant","message":"noted"}` + "\n"
	later := written.Add(time.Minute)
	if err := Append(ctx, conn, claudeTranscript, int64(len(line)), []byte(appended), later); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, row, claudeTranscript).Scan(&content, &byteLen, &mtimeMs); err != nil {
		t.Fatal(err)
	}
	if byteLen != int64(len(line)+len(appended)) || mtimeMs != later.UnixMilli() {
		t.Errorf("after an append the row has byte_len %d, mtime_ms %d; want %d, %d", byteLen, mtimeMs, len(line)+len(appended), later.UnixMilli())
	}
	if parts := partsOf(t, conn, claudeTranscript); parts[int64(len(line))] != appended {
		t.Errorf("the append's part at %d is %q, want %q", len(line), parts[int64(len(line))], appended)
	}
	if read, ok, err := Read(ctx, conn, claudeTranscript); err != nil || !ok || string(read) != line+appended {
		t.Errorf("Read = %d bytes, %t, %v; want the %d bytes written", len(read), ok, err, len(line+appended))
	}
	if at, ok, err := Written(ctx, conn, claudeTranscript); err != nil || !ok || !at.Equal(later) {
		t.Errorf("Written = %v, %t, %v; want the append's %v", at, ok, err, later)
	}
}

// A writer saves only over what it last saved: a writer whose view is stale, of a row another
// writer made or grew since, is refused naming both lengths, and the row reads as the other writer
// left it.
func TestAClaudeSaveRefusesAStaleWriter(t *testing.T) {
	conn := connectWithTables(t)
	ctx := context.Background()
	first, second := "{\"n\":1}\n", "{\"n\":2}\n"
	if err := Replace(ctx, conn, claudeTranscript, Absent, []byte(first), time.Now()); err != nil {
		t.Fatal(err)
	}
	held := func() string {
		read, _, err := Read(ctx, conn, claudeTranscript)
		if err != nil {
			t.Fatal(err)
		}
		return string(read)
	}
	stale := map[string]func() error{
		"a whole write of a row it saw none of": func() error {
			return Replace(ctx, conn, claudeTranscript, Absent, []byte("{\"stale\":1}\n"), time.Now())
		},
		"an append at a length the row has left": func() error {
			return Append(ctx, conn, claudeTranscript, 3, []byte("{\"stale\":2}\n"), time.Now())
		},
		"a whole write over a length the row has left": func() error {
			return Replace(ctx, conn, claudeTranscript, 3, []byte("{\"stale\":3}\n"), time.Now())
		},
	}
	for name, write := range stale {
		var moved *MovedError
		if err := write(); !errors.As(err, &moved) || moved.Held != int64(len(first)) || !strings.Contains(err.Error(), claudeTranscript) {
			t.Errorf("%s = %v, want a MovedError naming the session and the %d bytes it holds", name, err, len(first))
		}
		if got := held(); got != first {
			t.Errorf("after %s the session reads %q, want %q as its writer left it", name, got, first)
		}
	}
	if err := Append(ctx, conn, claudeTranscript, int64(len(first)), []byte(second), time.Now()); err != nil {
		t.Fatalf("an append by the writer that saved the row = %v", err)
	}
	if got := held(); got != first+second {
		t.Errorf("the session reads %q, want %q", got, first+second)
	}
	if err := Append(ctx, conn, claudeTranscript+".absent", Absent, []byte(second), time.Now()); err == nil {
		t.Error("an append to a session the table holds no row for was taken")
	}
}

// A Claude Code row beside an Oh My Pi one never leaves a NULL byte_len, which would stop every Oh
// My Pi process at its index load, and every row's byte_len is its bytes as Oh My Pi measures
// them; the Oh My Pi session reads, and was written, exactly as before.
func TestAClaudeRowHasAByteLenBesideAnOhMyPiRowLeftAsItWas(t *testing.T) {
	conn := connectWithTables(t)
	ctx := context.Background()
	imported := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, err := Import(ctx, conn, sessionPath, []byte(session), imported); err != nil {
		t.Fatal(err)
	}
	transcript := "{\"type\":\"user\",\"message\":\"remember the nonce — BETA\"}\n"
	if err := Replace(ctx, conn, claudeTranscript, Absent, []byte(transcript), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, conn, claudeTranscript, int64(len(transcript)), []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Replace(ctx, conn, claudeRecord, Absent, []byte(`{"state":"open","count":2}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "SELECT f.path, f.byte_len, octet_length(f.content) + "+
		"coalesce((SELECT sum(octet_length(p.content)) FROM omp_session_files_parts p WHERE p.path = f.path), 0) FROM omp_session_files f")
	if err != nil {
		t.Fatal(err)
	}
	measured := 0
	for rows.Next() {
		var path string
		var byteLen *int64
		var bytes int64
		if err := rows.Scan(&path, &byteLen, &bytes); err != nil {
			t.Fatal(err)
		}
		measured++
		if byteLen == nil || *byteLen != bytes {
			t.Errorf("row %s has byte_len %v, want its %d bytes", path, byteLen, bytes)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if measured != 3 {
		t.Errorf("the table holds %d rows, want the Oh My Pi session and the two Claude Code files", measured)
	}
	if read, ok, err := Read(ctx, conn, sessionPath); err != nil || !ok || string(read) != session {
		t.Errorf("the Oh My Pi session reads %q, %t, %v; want it as imported", read, ok, err)
	}
	if at, ok, err := Written(ctx, conn, sessionPath); err != nil || !ok || !at.Equal(imported) {
		t.Errorf("the Oh My Pi session was written %v, %t, %v; want %v", at, ok, err, imported)
	}
}

// Content Postgres cannot keep as text is refused by Postgres itself, naming the session, as
// Import's is, and nothing is written: neither a first whole write nor an append to a saved row.
func TestAClaudeSaveRefusesContentNoTextValueHolds(t *testing.T) {
	conn := connectWithTables(t)
	ctx := context.Background()
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "write session "+claudeTranscript) || !strings.Contains(err.Error(), "invalid byte sequence") {
			t.Errorf("%s = %v, want the write of %s refused for its invalid byte sequence", what, err, claudeTranscript)
		}
	}
	refused("a whole write holding a NUL byte", Replace(ctx, conn, claudeTranscript, Absent, []byte("{}\x00\n"), time.Now()))
	if _, found, _ := Written(ctx, conn, claudeTranscript); found {
		t.Fatal("a refused whole write wrote the row")
	}
	if err := Replace(ctx, conn, claudeTranscript, Absent, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	refused("an append holding a NUL byte", Append(ctx, conn, claudeTranscript, 3, []byte("{\x00}\n"), time.Now()))
	if read, _, _ := Read(ctx, conn, claudeTranscript); string(read) != "{}\n" {
		t.Errorf("after a refused append the session reads %q, want it as saved", read)
	}
}
