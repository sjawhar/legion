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
// that daemon's own (`legion credential` by the pane launcher's path, or `gh auth git-credential`,
// which answers from one owner's gh files alone), to `legion git-credential`, the helper that
// answers each pane's git from the gh files of the owner the repository's path names: the clone's
// credential.helper and its github.com one each read as the empty reset and then the new helper,
// with credential.useHttpPath true so git passes that path, as a clone provisioned now does, and
// the boot log names the clone. The next boot finds the helper current and writes and logs
// nothing; a clone with the current helper and no useHttpPath is rewritten. A state directory with
// no clone is nothing to do, and so is a configuration with no repository.
func TestTmuxBootRewritesAnOlderClonesCredentialHelper(t *testing.T) {
	if _, err := os.Stat(realGit); err != nil {
		t.Skipf("no git at %s: %v", realGit, err)
	}
	// The clone's helper keys are read with `git config --get-all`, which lists every scope, and the
	// host's own config can carry the very helper asserted here (`gh auth setup-git` writes the
	// github.com one to ~/.gitconfig). Neither the global nor the system file may reach the test:
	// its own git, its `git init`, and the fix-up's runner all inherit this process's environment.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
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
	old := "!gh auth git-credential"
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
	if got := strings.TrimSpace(git(t, located.Clone, "config", "credential.useHttpPath")); got != "true" {
		t.Errorf("credential.useHttpPath = %q, want true", got)
	}
	if lines := strings.Count(logs.String(), "set the shared clone's git credential helper"); lines != 1 || !strings.Contains(logs.String(), located.Clone) {
		t.Errorf("the fix-up logged %q, want one line naming %s", logs.String(), located.Clone)
	}

	// The next boot: the helper is current, so nothing is written and nothing logged.
	logs.Reset()
	before := git(t, located.Clone, "config", "--list", "--local")
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential on a current clone: %v", err)
	}
	if after := git(t, located.Clone, "config", "--list", "--local"); after != before || logs.Len() != 0 {
		t.Errorf("a current clone was rewritten or logged: config before %q, after %q, log %q", before, after, logs.String())
	}

	// A clone with the current helper and no useHttpPath answers every repository with one
	// owner's token, so it is rewritten too.
	git(t, located.Clone, "config", "--unset", "credential.useHttpPath")
	logs.Reset()
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential on a clone without useHttpPath: %v", err)
	}
	if got := strings.TrimSpace(git(t, located.Clone, "config", "credential.useHttpPath")); got != "true" || !strings.Contains(logs.String(), located.Clone) {
		t.Errorf("credential.useHttpPath = %q after the fix-up, log %q; want true and a line naming %s", got, logs.String(), located.Clone)
	}

	// A configuration with no repository has no clone to look for, whatever the state directory
	// holds.
	cfg.Projects = nil
	logs.Reset()
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil || logs.Len() != 0 {
		t.Fatalf("reconfigureCloneCredential with no repository = %v, logged %q; want nothing to do", err, logs.String())
	}
}
