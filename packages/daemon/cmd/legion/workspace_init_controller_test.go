package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The controller's pod (`controller: daemon`) has one init container, `workspace-init controller`,
// on the volume its Sandbox owns: it makes the sessions directory Oh My Pi's sessions are mounted
// from, provisions no workspace, and installs no gh shim, since the controller holds no GitHub
// credential.
func TestWorkspaceInitControllerMakesTheSessionsDirectoryAndNothingElse(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LEGION_RESUME_SESSION_FILE", "")
	os.Unsetenv("LEGION_RESUME_SESSION_FILE")
	code, stdout, stderr := runWorkspaceInitHere([]string{"controller", "--root", root})
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
	}
	info, err := os.Stat(filepath.Join(root, "sessions"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("sessions: %v (%v), want a 0700 directory", info, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the controller's volume holds %v, want the sessions directory alone", names)
	}
}

// A resume of the controller whose recorded session is gone from its volume means the volume was
// lost: exit 3, which the runtime reads as a lost volume, so the daemon relaunches a fresh
// controller rather than charging launch failures for a session no attempt can find. A session
// that is present lets the same invocation through.
func TestWorkspaceInitControllerReportsALostVolumeWhenTheResumedSessionIsGone(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "sessions", "--legion--", "2026-10-06T12-00-00-000Z_0001.jsonl")
	t.Setenv("LEGION_RESUME_SESSION_FILE", session)
	code, stdout, stderr := runWorkspaceInitHere([]string{"controller", "--root", root})
	want := "The controller's volume holds no recorded OMP session file (" + session + "): the volume was lost"
	if code != 3 || !strings.Contains(stderr, want) || stdout != "" {
		t.Fatalf("lost session: exit %d, stdout %q, stderr %q; want 3 naming %q", code, stdout, stderr, want)
	}

	if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runWorkspaceInitHere([]string{"controller", "--root", root}); code != 0 || stderr != "" {
		t.Fatalf("present session: exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
	}
}

// The controller's init container takes only its volume's root, an absolute path, and refuses the
// provisioning token outright, as `workspace-init provision` does: nothing on the controller's pod
// ever holds it.
func TestWorkspaceInitControllerRefusesARelativeRootAndTheProvisioningToken(t *testing.T) {
	if code, _, stderr := runWorkspaceInitHere([]string{"controller", "--root", "legion"}); code != 1 || !strings.Contains(stderr, `--root must be an absolute path (got "legion")`) {
		t.Fatalf("relative root: exit %d, stderr %q", code, stderr)
	}
	root := t.TempDir()
	t.Setenv(provisionTokenFileEnv, "/var/run/legion/provision/LEGION_PROVISION_TOKEN")
	code, _, stderr := runWorkspaceInitHere([]string{"controller", "--root", root})
	if code != 1 || !strings.Contains(stderr, provisionTokenFileEnv+" is set") {
		t.Fatalf("provisioning token: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused run made the sessions directory (%v)", err)
	}
}
