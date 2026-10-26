package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// writeSession writes a session file at path, its parents made, last modified at.
func writeSession(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// A resume continues the recorded session where it is: the recorded path itself when it is there,
// else the same session in another working directory's folder of the same sessions directory — a
// session recorded in a pod keeps the folder named for the pod's working directory when it is
// carried onto the host — found by the recorded file name, or, for Oh My Pi's
// `<timestamp>_<session id>.jsonl`, by the session id under any timestamp. Of several, the one
// written last; a directory of the right name is no session.
func TestSessionFileOfFindsTheRecordedSessionWhereItWasCarried(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	recorded := filepath.Join(sessions, "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	old := time.Now().Add(-time.Hour)

	t.Run("at its recorded path", func(t *testing.T) {
		here := filepath.Join(t.TempDir(), "--work--", "2026-10-20T10-00-00-000Z_ses-here.jsonl")
		writeSession(t, here, old)
		if got, err := sessionFileOf(here, "OMP"); err != nil || got != here {
			t.Fatalf("sessionFileOf = %q, %v; want the recorded %s", got, err, here)
		}
	})

	carried := filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, carried, old)
	if got, err := sessionFileOf(recorded, "OMP"); err != nil || got != carried {
		t.Fatalf("sessionFileOf with the session in another folder = %q, %v; want %s", got, err, carried)
	}

	renamed := filepath.Join(sessions, "--state-work--", "2026-10-21T09-00-00-000Z_ses-abc.jsonl")
	writeSession(t, renamed, time.Now())
	if got, err := sessionFileOf(recorded, "OMP"); err != nil || got != renamed {
		t.Fatalf("sessionFileOf with two copies = %q, %v; want the one written last, %s", got, err, renamed)
	}

	if err := os.MkdirAll(filepath.Join(sessions, "--newer--", "2026-10-22T00-00-00-000Z_ses-abc.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-abcd.jsonl")
	writeSession(t, other, time.Now().Add(time.Hour))
	if got, err := sessionFileOf(recorded, "OMP"); err != nil || got != renamed {
		t.Fatalf("sessionFileOf beside a directory of the name and another session's file = %q, %v; want %s", got, err, renamed)
	}
}

// A Claude Code session, `<session id>.jsonl`, holds no `_`: it is found by its name alone.
func TestSessionFileOfFindsAClaudeSessionByItsName(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	recorded := filepath.Join(projects, "-legion-workspace", "5f0c2a.jsonl")
	carried := filepath.Join(projects, "-pod-work", "5f0c2a.jsonl")
	writeSession(t, carried, time.Now())
	if got, err := sessionFileOf(recorded, "Claude Code"); err != nil || got != carried {
		t.Fatalf("sessionFileOf = %q, %v; want %s", got, err, carried)
	}
}

// A session found nowhere is runtime.ErrSessionFileMissing, naming the harness, the recorded path
// and the sessions directory searched; a recorded name holding a glob character is searched for
// nowhere, since it would match other sessions.
func TestSessionFileOfFindingNothingIsTheMissingSession(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	recorded := filepath.Join(sessions, "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-xyz.jsonl"), time.Now())
	_, err := sessionFileOf(recorded, "OMP")
	want := "the recorded session file is missing: no OMP session at " + recorded + ", nor of that session in another folder of " + sessions
	if !errors.Is(err, runtime.ErrSessionFileMissing) || err.Error() != want {
		t.Fatalf("sessionFileOf = %v, want runtime.ErrSessionFileMissing: %q", err, want)
	}

	globbed := filepath.Join(sessions, "--legion-workspace--", "*_ses-xyz.jsonl")
	if _, err := sessionFileOf(globbed, "OMP"); !errors.Is(err, runtime.ErrSessionFileMissing) || !strings.Contains(err.Error(), globbed) {
		t.Fatalf("sessionFileOf of a name holding a glob = %v, want the missing session", err)
	}
}
