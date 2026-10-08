package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// realGit is the host's git, which the fix-up runs through the provisioning runner: a fake would
// not read or write a repository's configuration as git does.
const realGit = "/usr/bin/git"

// git runs the real git over the clone's own repository and returns its stdout.
func git(t *testing.T, clone string, args ...string) string {
	t.Helper()
	command := exec.Command(realGit, append([]string{"--git-dir=" + filepath.Join(clone, ".git")}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// A tmux daemon's boot rewrites the shared clone an older daemon provisioned, whose helper names
// that daemon's `legion credential` by the pane launcher's path, to `gh auth git-credential`, the
// helper every pane's git answers from its own gh files through: the clone's credential.helper and
// its github.com one each read as the empty reset and then the new helper, as a clone provisioned
// now does, and the boot log names the clone. A state directory with no clone is nothing to do,
// and so is a configuration with no repository.
func TestTmuxBootRewritesAnOlderClonesCredentialHelper(t *testing.T) {
	if _, err := os.Stat(realGit); err != nil {
		t.Skipf("no git at %s: %v", realGit, err)
	}
	cfg := config.Config{Project: "WIDGETS", StateDir: t.TempDir(), Runtime: config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"WIDGETS": {Repo: ghrepo.MustParse("acme/widgets")}}}
	tools := map[string]string{"git": realGit}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// No clone yet: a daemon that has provisioned nothing skips the fix-up without a word.
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential with no clone = %v, want nothing to do", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("the fix-up logged %q with no clone", logs.String())
	}

	located, err := workspace.Location(cfg.StateDir, cfg.Projects["WIDGETS"].Repo, "WIDGETS-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(located.Clone, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(realGit, "init", "-q", located.Clone).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	old := "!/old/legion credential"
	git(t, located.Clone, "config", "--add", "credential.helper", old)
	git(t, located.Clone, "config", "--add", "credential.https://github.com.helper", old)

	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential: %v", err)
	}
	want := "\n" + workspace.GitHubCredentialHelper + "\n"
	for _, key := range []string{"credential.helper", "credential.https://github.com.helper"} {
		if got := git(t, located.Clone, "config", "--get-all", key); got != want {
			t.Errorf("%s reads %q after the fix-up, want the empty reset and then %q", key, got, workspace.GitHubCredentialHelper)
		}
	}
	if got := strings.TrimSpace(git(t, located.Clone, "config", "credential.interactive")); got != "false" {
		t.Errorf("credential.interactive = %q, want false", got)
	}
	if lines := strings.Count(logs.String(), "set the shared clone's git credential helper"); lines != 1 || !strings.Contains(logs.String(), located.Clone) {
		t.Errorf("the fix-up logged %q, want one line naming %s", logs.String(), located.Clone)
	}

	// A configuration with no repository has no clone to look for, whatever the state directory
	// holds.
	cfg.Projects = nil
	logs.Reset()
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil || logs.Len() != 0 {
		t.Fatalf("reconfigureCloneCredential with no repository = %v, logged %q; want nothing to do", err, logs.String())
	}
}
