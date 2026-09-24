package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every agent of a tree writes the shared clone, so everything below is what a tree agent can plant
// there for the next provisioning of the tree, and what provisioning must never obey: its
// credentialed clone and fetch name the token file in their environment, and in a pod every other
// step runs where the mounted provisioning Secret is readable.

// gitHooks is every hook githooks(5) names; a planted script under each name records whether git
// ran it.
var gitHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// plantHooks writes every hook into dir; each one that runs appends its name, and the token file
// its environment named, to sink.
func plantHooks(t *testing.T, dir, sink string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s token-file=%s\\n' \"${0##*/}\" \"$LEGION_PROVISIONING_TOKEN_FILE\" >> '" + sink + "'\nexit 0\n"
	for _, hook := range gitHooks {
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// advanceRemote puts a new commit on the remote's main, so the next fetch updates a ref.
func advanceRemote(t *testing.T, remote string) {
	t.Helper()
	pusher := filepath.Join(t.TempDir(), "pusher")
	runSetup(t, filepath.Dir(pusher), "git", "clone", "--quiet", remote, pusher)
	runSetup(t, pusher, "git", "-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "advance")
	runSetup(t, pusher, "git", "push", "--quiet", "origin", "HEAD:main")
}

// provisionTwice provisions WIDGETS-42, lets plant tamper with the shared clone, advances the
// remote, and provisions WIDGETS-43 on the same clone: a credentialed fetch that moves a ref, a
// workspace add, and the configuration writes, all against what plant left.
func provisionTwice(t *testing.T, plant func(t *testing.T, clone string)) {
	t.Helper()
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	plant(t, filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets"))
	advanceRemote(t, run.remote)
	req.Issue = "WIDGETS-43"
	second, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if second.Dir == first.Dir {
		t.Fatalf("both provisions landed in %s", second.Dir)
	}
}

// requireAbsent fails naming what a planted script recorded, when it recorded anything.
func requireAbsent(t *testing.T, sink, what string) {
	t.Helper()
	if body, err := os.ReadFile(sink); err == nil {
		t.Fatalf("provisioning ran %s:\n%s", what, strings.TrimSpace(string(body)))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// A hook in the shared clone's hooks directory, or in a directory its core.hooksPath names, never
// runs: not in the credentialed fetch (reference-transaction), not in the workspace add
// (post-index-change), not in any other step.
func TestProvisionRunsNoHookTheTreePlanted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, clone, sink string)
	}{
		{"in .git/hooks", func(t *testing.T, clone, sink string) {
			plantHooks(t, filepath.Join(clone, ".git", "hooks"), sink)
		}},
		{"under core.hooksPath", func(t *testing.T, clone, sink string) {
			hooks := filepath.Join(t.TempDir(), "hooks")
			plantHooks(t, hooks, sink)
			runSetup(t, clone, "git", "--git-dir="+filepath.Join(clone, ".git"), "config", "core.hooksPath", hooks)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := filepath.Join(t.TempDir(), "hooks-ran")
			provisionTwice(t, func(t *testing.T, clone string) { tc.plant(t, clone, sink) })
			requireAbsent(t, sink, "hooks the tree planted")
		})
	}
}
