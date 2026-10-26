package ompsessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// An append is Oh My Pi's own: the row's byte_len grows by the bytes appended and the bytes are a
// part at the old length, so the session reads as the content then the append. An append built on
// a length the row no longer has, or on a session the table does not hold, is refused with
// ErrMoved and writes nothing.
func TestAppendWritesOhMyPisPartAndRefusesASessionThatMoved(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if _, err := Import(ctx, conn, sessionPath, []byte(session), time.Now()); err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"type":"custom","customType":"legion-answer","data":{"questionId":"q — 1"},"id":"a2","parentId":"a1"}` + "\n")
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := Append(ctx, conn, sessionPath, int64(len(session)), line, at); err != nil {
		t.Fatal(err)
	}
	var byteLen, mtime, offset int64
	if err := conn.QueryRow(ctx, "SELECT f.byte_len, f.mtime_ms, p.start_offset FROM omp_session_files f JOIN omp_session_files_parts p USING (path) WHERE f.path = $1",
		sessionPath).Scan(&byteLen, &mtime, &offset); err != nil {
		t.Fatal(err)
	}
	if byteLen != int64(len(session)+len(line)) || offset != int64(len(session)) || mtime != at.UnixMilli() {
		t.Errorf("byte_len %d, part at %d, mtime_ms %d; want %d, %d, %d", byteLen, offset, mtime, len(session)+len(line), len(session), at.UnixMilli())
	}
	if content, ok, err := Read(ctx, conn, sessionPath); err != nil || !ok || string(content) != session+string(line) {
		t.Errorf("Read = %q, %t, %v; want the session then the append", content, ok, err)
	}

	err := Append(ctx, conn, sessionPath, int64(len(session)), line, at)
	var moved *MovedError
	if !errors.Is(err, conversation.ErrMoved) || !errors.As(err, &moved) || moved.Now != byteLen {
		t.Errorf("an append on the old length = %v, want ErrMoved naming byte_len %d", err, byteLen)
	}
	if err := Append(ctx, conn, "/no/such/session.jsonl", 0, line, at); !errors.Is(err, conversation.ErrMoved) {
		t.Errorf("an append to a session the table lacks = %v, want ErrMoved", err)
	}
	var parts int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM omp_session_files_parts").Scan(&parts); err != nil || parts != 1 {
		t.Errorf("%d parts, %v; want the one append", parts, err)
	}
}

const describedSession = `{"type":"title","v":1,"title":"Legion tester · INFRA-204","source":"auto","updatedAt":"2026-10-26T00:00:00.000Z","pad":"   "}
{"type":"session","version":3,"id":"0001","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work","title":"older title","parentSession":"/sessions/parent.jsonl"}
{"type":"custom","customType":"legion-managed-session","data":{"session_id":"0001"},"id":"a1","parentId":null}
not json
{"type":"custom","customType":"envoy-role-claim","data":{"role":"legion-infra-infra-204-tester"},"id":"a2","parentId":"a1"}
`

// A session describes itself by its title slot (over its header's title), its header, and the role
// its last envoy-role-claim entry names; a hand-back clears the role. A file read for its head only
// stops at the header.
func TestDescribeReadsTitleHeaderAndRoleClaim(t *testing.T) {
	d := Describe([]byte(describedSession))
	want := Description{
		Title: "Legion tester · INFRA-204", TitleSource: "auto", ID: "0001", Timestamp: "2026-10-26T00:00:00.000Z", Cwd: "/work",
		ParentSession: "/sessions/parent.jsonl", RoleClaim: "legion-infra-infra-204-tester", ManagedSession: "0001",
	}
	if d != want {
		t.Errorf("Describe = %+v, want %+v", d, want)
	}
	released := Describe([]byte(describedSession + `{"type":"custom","customType":"envoy-role-claim","data":{"role":null},"id":"a3","parentId":"a2"}` + "\n"))
	if released.RoleClaim != "" {
		t.Errorf("after a hand-back the role claim = %q, want none", released.RoleClaim)
	}
	if noSlot := Describe([]byte(describedSession[len(`{"type":"title","v":1,"title":"Legion tester · INFRA-204","source":"auto","updatedAt":"2026-10-26T00:00:00.000Z","pad":"   "}`)+1:])); noSlot.Title != "older title" {
		t.Errorf("with no title slot the title = %q, want the header's", noSlot.Title)
	}

	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(describedSession), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := DescribeFile(path, true); err != nil || got != want {
		t.Errorf("DescribeFile = %+v, %v; want %+v", got, err, want)
	}
	head, err := DescribeFile(path, false)
	if err != nil || head.Title != want.Title || head.ID != "0001" || head.RoleClaim != "" || head.ManagedSession != "" {
		t.Errorf("DescribeFile head only = %+v, %v; want the title and header alone", head, err)
	}
}

// The table's sessions list with the title column where Oh My Pi set one, and otherwise the title
// slot of the content Import copied.
func TestTitlesListsTheColumnOrTheContentsTitle(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if titled, err := Titles(ctx, conn); err != nil || len(titled) != 0 {
		t.Fatalf("Titles before any table = %v, %v; want none", titled, err)
	}
	if _, err := Import(ctx, conn, "/s/a.jsonl", []byte(describedSession), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, conn, "/s/b.jsonl", []byte(describedSession), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "UPDATE omp_session_files SET title = 'Renamed', title_source = 'user' WHERE path = '/s/b.jsonl'"); err != nil {
		t.Fatal(err)
	}
	titled, err := Titles(ctx, conn)
	if err != nil || len(titled) != 2 {
		t.Fatalf("Titles = %v, %v", titled, err)
	}
	if titled[0].Path != "/s/a.jsonl" || titled[0].Title != "Legion tester · INFRA-204" || titled[0].TitleSource != "auto" || titled[0].ParentSession != "/sessions/parent.jsonl" {
		t.Errorf("the imported row = %+v, want the content's title and header", titled[0])
	}
	if titled[1].Title != "Renamed" || titled[1].TitleSource != "user" || titled[1].ID != "0001" {
		t.Errorf("the titled row = %+v, want the column's title", titled[1])
	}
}
