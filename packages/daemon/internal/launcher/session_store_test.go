package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Under SQL session storage (the runtime's session_store postgres: OMP_SESSION_STORAGE=sql and
// OMP_SESSION_SQL_DSN_FILE in the generation's environment) a resume is held to the session table,
// where the child's Oh My Pi will read it, never to the volume: a session the table does not hold is
// refused before anything starts, even when a file of that name is on disk, and a session the table
// holds starts with no file on disk at all.
func TestStartUnderSQLStorageResumesOnlyASessionTheTableHolds(t *testing.T) {
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_test")
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0001.jsonl")
	start := func(generation uint64) (string, bool) {
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
		command.ResumeFile = sessionFile
		got := g.start(command)
		return got.Error, got.OK
	}

	refusal, ok := start(5)
	if ok || !strings.Contains(refusal, "resume session "+sessionFile+": the session table holds no such session") {
		t.Fatalf("start of a session the table does not hold = %q (ok %t), want the refusal naming it", refusal, ok)
	}
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(6); ok || !strings.Contains(refusal, "the session table holds no such session") {
		t.Fatalf("start with the file on disk but not in the table = %q (ok %t), want the table's refusal", refusal, ok)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child")
	}

	if err := os.Remove(sessionFile); err != nil {
		t.Fatal(err)
	}
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(7); !ok {
		t.Fatalf("start of a session the table holds = %q, want generation 7 to run", refusal)
	}
	published(t, marker)
	g.stop(7)
}

// A generation told SQL storage with no URL file is refused before it starts, naming the variable:
// the launcher cannot look where Oh My Pi would.
func TestStartUnderSQLStorageRefusesAResumeWithNoURLFile(t *testing.T) {
	g := newRig(t)
	command := g.childStart(3, filepath.Join(t.TempDir(), "marker"), nil)
	command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage)
	command.ResumeFile = "/sessions/absent.jsonl"
	if got := g.start(command); got.OK || !strings.Contains(got.Error, "OMP_SESSION_STORAGE is sql and OMP_SESSION_SQL_DSN_FILE names no file") {
		t.Fatalf("start = %#v, want the refusal naming OMP_SESSION_SQL_DSN_FILE", got)
	}
}

// Under SQL storage a session's last write is the table's mtime_ms, which Oh My Pi moves on every
// write and `legion sessions import` sets to the copied file's: a resumed generation whose workspace
// provisioning recorded creating after that is told so, and one whose workspace is older is not.
func TestAResumeUnderSQLStorageIsToldWhetherItsWorkspaceWasRecreatedSinceTheTableWroteItsSession(t *testing.T) {
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_recreated_test")
	g := newRig(t)
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0002.jsonl")
	written := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), written); err != nil {
		t.Fatal(err)
	}
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir, ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + dsnFile}
	if err := workspace.RecordCreated(dir, written.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(1, env, sessionFile); got != "false" {
		t.Errorf("a resume in a workspace older than the table's last write was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(2, env, sessionFile); got != "true" {
		t.Errorf("a resume in a workspace recreated after the table's last write was told %q, want true", got)
	}
}
