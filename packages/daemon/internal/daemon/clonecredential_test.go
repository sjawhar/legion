package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// realGit is the host's git, which the fix-up runs through the provisioning runner: a fake would
// not read or write a repository's configuration as git does.
const realGit = "/usr/bin/git"

// git runs the real git over the clone's own repository and returns its stdout.
func git(t *testing.T, clone string, args ...string) string {
	t.Helper()
	// --git-dir is not a working directory: the helper runs in the clone it reads, never in the
	// directory the test process happens to have, and under an environment of its own, so no
	// GIT_ variable of this process can send it to another repository or configuration.
	argv := append([]string{realGit, "--git-dir=" + filepath.Join(clone, ".git")}, args...)
	return fixturegit.Isolated(t, t.TempDir()).Out(clone, argv...)
}

// A tmux daemon's boot rewrites every shared clone an older daemon provisioned, one per repository,
// whose helper names that daemon's own (`legion credential` by the pane launcher's path, or `gh
// auth git-credential`, which answers from one owner's gh files alone), to `legion git-credential`,
// the helper that answers each pane's git from the gh files of the owner the repository's path
// names: each clone's credential.helper and its github.com one read as the empty reset and then the
// new helper, with credential.useHttpPath true so git passes that path, as a clone provisioned now
// does, and the boot log names each clone. The next boot finds the helpers current and writes and
// logs nothing; a clone with the current helper and no useHttpPath is rewritten. A state directory
// with no clone is nothing to do.
func TestTmuxBootRewritesEveryOlderClonesCredentialHelper(t *testing.T) {
	if _, err := os.Stat(realGit); err != nil {
		t.Skipf("no git at %s: %v", realGit, err)
	}
	// The clone's helper keys are read with `git config --get-all`, which lists every scope, and the
	// host's own config can carry the very helper asserted here (`gh auth setup-git` writes the
	// github.com one to ~/.gitconfig). Neither the global nor the system file may reach the test,
	// and each reader is shut out of them by the measure that fits it. The fix-up runs its git
	// through workspace.execRunner.Run, which builds that child's environment out of this
	// process's own, so the two t.Setenv lines below are the measure for it. That child needs no
	// measure against the environment's own scope: the runner drops the pod's GIT_CONFIG_* block
	// itself (withoutGitConfigPairs, internal/workspace/config.go:256 and :577-581). The test's
	// own git helper and its `git init` below read nothing of this process's environment: each
	// carries fixtureenv.Isolated, which shuts out the files and the environment block alike.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	cfg := config.Config{Project: "WIDGETS", StateDir: t.TempDir(), Runtime: config.Runtime{Name: "tmux"}}
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

	var clones []string
	for _, repo := range []string{"acme/widgets", "zeta/tools"} {
		located, err := workspace.Location(cfg.StateDir, ghrepo.MustParse(repo), "WIDGETS-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(located.Clone, 0o700); err != nil {
			t.Fatal(err)
		}
		// The clone's directory is made just above, so the init runs inside it rather than in
		// whatever directory the test process has, and under an environment of its own, as the
		// git helper above does.
		fixturegit.Isolated(t, t.TempDir()).Run(located.Clone, append([]string{realGit}, fixturegit.InitWorkTree(located.Clone, "")[1:]...)...)
		old := "!gh auth git-credential"
		git(t, located.Clone, "config", "--add", "credential.helper", old)
		git(t, located.Clone, "config", "--add", "credential.https://github.com.helper", old)
		clones = append(clones, located.Clone)
	}

	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential: %v", err)
	}
	want := "\n" + workspace.GitHubCredentialHelper + "\n"
	for _, clone := range clones {
		for _, key := range []string{"credential.helper", "credential.https://github.com.helper"} {
			if got := git(t, clone, "config", "--get-all", key); got != want {
				t.Errorf("%s of %s reads %q after the fix-up, want the empty reset and then %q", key, clone, got, workspace.GitHubCredentialHelper)
			}
		}
		if got := strings.TrimSpace(git(t, clone, "config", "credential.interactive")); got != "false" {
			t.Errorf("credential.interactive of %s = %q, want false", clone, got)
		}
		if got := strings.TrimSpace(git(t, clone, "config", "credential.useHttpPath")); got != "true" {
			t.Errorf("credential.useHttpPath of %s = %q, want true", clone, got)
		}
		if !strings.Contains(logs.String(), clone) {
			t.Errorf("the fix-up logged %q, want a line naming %s", logs.String(), clone)
		}
	}
	if lines := strings.Count(logs.String(), "set the shared clone's git credential helper"); lines != 2 {
		t.Errorf("the fix-up logged %d lines, want one per clone:\n%s", lines, logs.String())
	}

	// The next boot: the helpers are current, so nothing is written and nothing logged.
	logs.Reset()
	var before []string
	for _, clone := range clones {
		before = append(before, git(t, clone, "config", "--list", "--local"))
	}
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential on current clones: %v", err)
	}
	for i, clone := range clones {
		if after := git(t, clone, "config", "--list", "--local"); after != before[i] {
			t.Errorf("current clone %s was rewritten: config before %q, after %q", clone, before[i], after)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("current clones logged %q", logs.String())
	}

	// A clone with the current helper and no useHttpPath answers every repository with one
	// owner's token, so it is rewritten too.
	git(t, clones[1], "config", "--unset", "credential.useHttpPath")
	logs.Reset()
	if err := reconfigureCloneCredential(context.Background(), cfg, tools, log); err != nil {
		t.Fatalf("reconfigureCloneCredential on a clone without useHttpPath: %v", err)
	}
	if got := strings.TrimSpace(git(t, clones[1], "config", "credential.useHttpPath")); got != "true" || !strings.Contains(logs.String(), clones[1]) {
		t.Errorf("credential.useHttpPath = %q after the fix-up, log %q; want true and a line naming %s", got, logs.String(), clones[1])
	}
}
