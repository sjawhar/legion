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
