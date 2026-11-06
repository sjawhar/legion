package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
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

// podSession is a session file as a pod records it: under the worker image's home and profile
// (HOME=/home/legion, OMP_PROFILE=legion), in the folder named for the pod's working directory.
const podSession = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-acme-widgets-legion-43--/2026-10-20T10-00-00-000Z_ses-abc.jsonl"

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
		if got, _, err := sessionFileOf(here, nil); err != nil || got != here {
			t.Fatalf("sessionFileOf = %q, %v; want the recorded %s", got, err, here)
		}
	})

	carried := filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, carried, old)
	if got, _, err := sessionFileOf(recorded, nil); err != nil || got != carried {
		t.Fatalf("sessionFileOf with the session in another folder = %q, %v; want %s", got, err, carried)
	}

	renamed := filepath.Join(sessions, "--state-work--", "2026-10-21T09-00-00-000Z_ses-abc.jsonl")
	writeSession(t, renamed, time.Now())
	if got, _, err := sessionFileOf(recorded, nil); err != nil || got != renamed {
		t.Fatalf("sessionFileOf with two copies = %q, %v; want the one written last, %s", got, err, renamed)
	}

	if err := os.MkdirAll(filepath.Join(sessions, "--newer--", "2026-10-22T00-00-00-000Z_ses-abc.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-abcd.jsonl")
	writeSession(t, other, time.Now().Add(time.Hour))
	if got, _, err := sessionFileOf(recorded, nil); err != nil || got != renamed {
		t.Fatalf("sessionFileOf beside a directory of the name and another session's file = %q, %v; want %s", got, err, renamed)
	}
}

// A session a pod recorded names the image's home, which is not this host's: once its recorded
// sessions directory holds nothing, the pane's own sessions directory is searched, by the recorded
// file name in the pod's folder or another, and by the session id under any timestamp.
func TestSessionFileOfFindsAPodsSessionInThePanesSessionsDirectory(t *testing.T) {
	for name, carried := range map[string]string{
		"in the pod's folder, by its name":       filepath.Join("--legion-workspaces-acme-widgets-legion-43--", filepath.Base(podSession)),
		"in the pane's folder, by its name":      filepath.Join("--state-workspaces-acme-widgets-legion-43--", filepath.Base(podSession)),
		"under another timestamp, by session id": filepath.Join("--state-workspaces-acme-widgets-legion-43--", "2026-10-21T09-00-00-000Z_ses-abc.jsonl"),
	} {
		t.Run(name, func(t *testing.T) {
			pane := filepath.Join(t.TempDir(), "home", ".omp", "agent", "sessions")
			writeSession(t, filepath.Join(pane, carried), time.Now())
			got, searched, err := sessionFileOf(podSession, paneAt(pane))
			if err != nil || got != filepath.Join(pane, carried) {
				t.Fatalf("sessionFileOf = %q, %v; want %s", got, err, filepath.Join(pane, carried))
			}
			if want := []string{filepath.Dir(filepath.Dir(podSession)), pane}; !slices.Equal(searched, want) {
				t.Errorf("searched %v, want %v", searched, want)
			}
		})
	}
}

// The recorded path's own sessions directory comes first: a copy there wins over a newer one in the
// pane's sessions directory, and the pane's is searched only when the first holds none.
func TestSessionFileOfSearchesTheRecordedSessionsDirectoryBeforeThePanes(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	recorded := filepath.Join(sessions, "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	pane := filepath.Join(t.TempDir(), "pane-sessions")
	first := filepath.Join(sessions, "--pod-work--", filepath.Base(recorded))
	writeSession(t, first, time.Now().Add(-time.Hour))
	writeSession(t, filepath.Join(pane, "--pane-work--", filepath.Base(recorded)), time.Now())
	if got, _, err := sessionFileOf(recorded, paneAt(pane)); err != nil || got != first {
		t.Fatalf("sessionFileOf = %q, %v; want %s, in the recorded path's sessions directory", got, err, first)
	}
}

// A Claude Code session, `<session id>.jsonl`, holds no `_`: it is found by its name alone, in the
// recorded projects directory or the pane's own.
func TestSessionFileOfFindsAClaudeSessionByItsName(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	recorded := filepath.Join(projects, "-legion-workspace", "5f0c2a.jsonl")
	carried := filepath.Join(projects, "-pod-work", "5f0c2a.jsonl")
	writeSession(t, carried, time.Now())
	if got, _, err := sessionFileOf(recorded, nil); err != nil || got != carried {
		t.Fatalf("sessionFileOf = %q, %v; want %s", got, err, carried)
	}
	pane := filepath.Join(t.TempDir(), "claude", "projects")
	elsewhere := filepath.Join(pane, "-state-work", "7a1b3c.jsonl")
	writeSession(t, elsewhere, time.Now())
	if got, _, err := sessionFileOf(filepath.Join("/elsewhere/projects/-pod-work", "7a1b3c.jsonl"), paneAt(pane)); err != nil || got != elsewhere {
		t.Fatalf("sessionFileOf in the pane's projects directory = %q, %v; want %s", got, err, elsewhere)
	}
}

// A session found nowhere is no file and no error, with every sessions directory searched, each
// once; a recorded name holding a glob character is searched for nowhere, and a sessions directory
// holding one is skipped, since either would match other sessions.
func TestSessionFileOfFindingNothingNamesEverySessionsDirectorySearched(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	recorded := filepath.Join(sessions, "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-xyz.jsonl"), time.Now())
	pane := filepath.Join(t.TempDir(), "pane-sessions")
	got, searched, err := sessionFileOf(recorded, paneAt(pane))
	if err != nil || got != "" || !slices.Equal(searched, []string{sessions, pane}) {
		t.Fatalf("sessionFileOf = %q, %v, %v; want nothing found in %s and %s", got, searched, err, sessions, pane)
	}
	if want := sessions + " or of the pane's sessions directory " + pane; searchedIn(searched) != want {
		t.Errorf("searchedIn = %q, want %q", searchedIn(searched), want)
	}
	if _, searched, _ := sessionFileOf(recorded, paneAt(sessions)); !slices.Equal(searched, []string{sessions}) || searchedIn(searched) != sessions {
		t.Errorf("the recorded sessions directory as the pane's is searched %v, want once", searched)
	}

	globbed := filepath.Join(sessions, "--legion-workspace--", "*_ses-xyz.jsonl")
	writeSession(t, filepath.Join(pane, "--pane-work--", "2026-10-20T10-00-00-000Z_ses-xyz.jsonl"), time.Now())
	if got, _, err := sessionFileOf(globbed, paneAt(pane)); err != nil || got != "" {
		t.Fatalf("sessionFileOf of a name holding a glob = %q, %v; want nothing", got, err)
	}
	starred := filepath.Join(t.TempDir(), "pane*sessions")
	writeSession(t, filepath.Join(starred, "--pane-work--", filepath.Base(recorded)), time.Now())
	if got, _, err := sessionFileOf(recorded, paneAt(starred)); err != nil || got != "" {
		t.Fatalf("sessionFileOf in a pane sessions directory holding a glob = %q, %v; want nothing", got, err)
	}
}

// The pane's sessions directory is never resolved when the recorded path is there, and a failure to
// resolve it, once needed, is the search's error.
func TestSessionFileOfResolvesThePanesSessionsDirectoryOnlyWhenItSearches(t *testing.T) {
	here := filepath.Join(t.TempDir(), "--work--", "2026-10-20T10-00-00-000Z_ses-here.jsonl")
	writeSession(t, here, time.Now())
	refused := func() (string, error) { return "", errors.New("no home to resolve under") }
	if got, _, err := sessionFileOf(here, refused); err != nil || got != here {
		t.Fatalf("sessionFileOf of a session at its recorded path = %q, %v; want it, the pane's directory unresolved", got, err)
	}
	if _, _, err := sessionFileOf(filepath.Join(filepath.Dir(here), "gone_ses-gone.jsonl"), refused); err == nil || err.Error() != "no home to resolve under" {
		t.Fatalf("sessionFileOf with an unresolvable pane directory = %v, want its error", err)
	}
}

// paneAt is a pane sessions directory that resolves to dir.
func paneAt(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}
