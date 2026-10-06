package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A finished child whose every commit reached origin — parked after its own push — is removed.
func TestRemoveFinishedRemovesAWorkspaceWithEveryCommitPushed(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains after RemoveFinished: %v", err)
	}
	if want := "removed WIDGETS-42's workspace"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}

// A finished child whose last commit is only the merged pull request's head is removed: GitHub
// deletes a squash merge's branch, so that commit carries no remote bookmark of its own once the
// clone's next fetch prunes it — and the daemon's recorded merged head is what tells that commit
// from one that was never pushed at all.
func TestRemoveFinishedRemovesAWorkspaceWhoseLastCommitIsOnlyTheMergedHead(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	mergedHead := strings.TrimSpace(runSetup(t, ws.Dir, "jj", "log", "-r", ws.Bookmark, "--no-graph", "-T", "commit_id"))
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")
	runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "branch", "-D", ws.Bookmark)
	// The next pod's own provisioning fetch is what prunes the now-deleted branch on the shared
	// clone; RemoveFinished is never the one that fetches.
	runSetup(t, ws.Clone, "jj", "git", "fetch")
	if listed := strings.TrimSpace(runSetup(t, ws.Clone, "jj", "log", "-r", "bookmarks(exact:"+ws.Bookmark+")", "--no-graph", "-T", "commit_id")); listed != "" {
		t.Fatalf("test setup: bookmark remains after branch deletion: %s", listed)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", mergedHead, time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains after RemoveFinished: %v", err)
	}
	if want := "removed WIDGETS-42's workspace"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}

// A child with a commit that reached neither a remote bookmark nor the merged head — local-only
// work no push ever carried — is kept, and the kept commit is named in the one line RemoveFinished
// logs, never deleted.
func TestRemoveFinishedKeepsAWorkspaceWithAnUnpushedCommit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "unpushed.txt"), []byte("never pushed\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	commit := strings.TrimSpace(runSetup(t, ws.Dir, "jj", "log", "-r", "@", "--no-graph", "-T", "commit_id"))

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], commit) {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept and commit %s", logged, commit)
	}
}

// A pending edit the workspace's own agent never ran a jj command over since is still counted as
// unpushed work: RemoveFinished snapshots ws.Dir itself before judging it, so a file dropped on
// disk and never committed does not slip past the push-safety check (dispatch://LEGION-583's
// correction: a parked child's un-snapshotted edit must keep its workspace).
func TestRemoveFinishedSnapshotsTheWorkspaceBeforeJudgingAnUncommittedEdit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "pending.txt"), []byte("never snapshotted\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	// No manual `jj status` here, unlike the other tests above: the edit above is on disk only,
	// untracked by any jj operation, until RemoveFinished itself snapshots it.

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
}

// RemoveFinished's snapshot (unlike every other command this package runs, which pass
// --ignore-working-copy) runs a real `jj status` over a workspace a tree agent writes to
// directly. jj configuration naming a working-copy filter and a signing backend, behavior, key
// and program, set as that workspace's own (plantWorkspaceConfig: on the tmux runtime a pane
// shares the config home it lives in, and in a pod it is what a legacy file written in the
// instant after the runner's disarmLegacyConfig check would become), runs neither program on
// RemoveFinished's snapshot: snapshotOverrides outranks it. A pending edit untouched by any jj
// command still keeps the workspace, proving the snapshot itself still ran.
func TestRemoveFinishedSnapshotRunsNoHostileFilterOrSigningProgram(t *testing.T) {
	requireFilters(t)
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "pending.txt"), []byte("never snapshotted\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}

	filterSink := filepath.Join(t.TempDir(), "filter-ran")
	filter := recordingScript(t, "filter", filterSink, "exec cat")
	signSink := filepath.Join(t.TempDir(), "sign-ran")
	signProgram := recordingScript(t, "sign", signSink, "exit 1")
	signKey := filepath.Join(t.TempDir(), "signing-key.pub")
	if err := os.WriteFile(signKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop planted\n"), 0o644); err != nil {
		t.Fatalf("write planted signing key: %v", err)
	}
	// Set as ws.Dir's own configuration: a worker's own pane, in this same workspace, before it
	// was ever parked.
	plantWorkspaceConfig(t, ws.Dir,
		"[git.filter]\nenabled = true\n[git.filter.drivers.planted]\nclean = ["+tomlString(filter)+"]\nsmudge = [\"cat\"]\nrequired = false\n"+
			"[fsmonitor]\nbackend = \"watchman\"\n"+
			"[signing]\nbackend = \"ssh\"\nbehavior = \"own\"\nkey = "+tomlString(signKey)+"\n[signing.backends.ssh]\nprogram = "+tomlString(signProgram)+"\n")
	if err := os.WriteFile(filepath.Join(ws.Dir, ".gitattributes"), []byte("*.txt filter=planted\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
	if _, err := os.Stat(filterSink); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the planted working-copy filter ran during RemoveFinished's snapshot (sink present: %v)", err)
	}
	if _, err := os.Stat(signSink); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the planted signing program ran during RemoveFinished's snapshot (sink present: %v)", err)
	}
}

// jj refuses to snapshot a new file over snapshot.max-new-file-size (1MiB by default), leaving it
// untracked with exit 0 and a warning rather than an error: @ would stay empty of a 2MiB
// uncommitted file and the push-safety check would read the workspace clean and remove it.
// snapshotOverrides deliberately leaves the size limit at its default (raising or disabling it
// would let this very pass write a worker's oversized file into the shared clone's object store
// it exists to shrink), so untrackedPaths, reading jj's own "Untracked paths:" section off the
// real `jj status` this runs — never a hand-written stdout string, so a jj release that changes
// that section's wording or shape turns this test red rather than leaving the parser's own test
// passing against a string the real tool no longer prints — is the one thing that catches it.
func TestRemoveFinishedKeepsAWorkspaceWhoseSnapshotLeftALargeFileUntracked(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	large := bytes.Repeat([]byte("x"), 2<<20) // 2MiB, over the 1MiB default limit
	if err := os.WriteFile(filepath.Join(ws.Dir, "large.bin"), large, 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], "large.bin") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept and large.bin untracked", logged)
	}
}

// A prior pass's Remove renames the workspace directory aside before deleting it (bookmark.go's
// removingSuffix), so a kill partway through the delete — simulated here by renaming the whole
// directory aside and stopping, the earliest point a kill could land — leaves ws.Dir absent and
// the renamed-aside directory in its place. RemoveFinished finds no workspace at ws.Dir, but
// recognizes the renamed-aside name and finishes the delete rather than reporting nothing to
// remove and leaving it an orphan forever: this candidate was already found safe to remove by
// whichever earlier pass started it.
func TestRemoveFinishedFinishesARemovalAnEarlierPassWasInterruptedMidDelete(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.Rename(ws.Dir, removingPath(ws.Dir)); err != nil {
		t.Fatalf("simulate an interrupted removal: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ws.Dir exists after RemoveFinished: %v", err)
	}
	if _, err := os.Stat(removingPath(ws.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the renamed-aside directory remains: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "finished a removal an earlier pass was interrupted mid-delete") {
		t.Errorf("logged %v, want one line naming the finished resume", logged)
	}
	workspaceName := filepath.Base(ws.Dir)
	listed := runSetup(t, ws.Clone, "jj", "workspace", "list", "-T", `name ++ "\n"`)
	if strings.Contains(listed, workspaceName) {
		t.Errorf("jj workspace list %q still names %s, want it forgotten", listed, workspaceName)
	}
}

// A candidate with no workspace on this volume — already removed, or never provisioned here — is
// left alone without error: the daemon's candidate list is computed from issue lifecycle alone and
// may still name one nothing on this volume ever created.
func TestRemoveFinishedOfAWorkspaceAlreadyGoneIsANoop(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if want := "has no workspace on this volume"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}

// jj silently skips a directory holding its own .git or .jj: not tracked, not listed as untracked,
// no warning anywhere. A workspace otherwise fully pushed, holding a second repository an agent
// cloned to patch a dependency and committed to locally, is kept: that local commit is invisible
// to unpushedRevset's `::workspaceName@`, and without nestedRepositories the snapshot would read
// the workspace clean and RemoveFinished would delete the nested repository's own unpushed commit
// along with it.
func TestRemoveFinishedKeepsAWorkspaceHoldingANestedRepositoryWithALocalCommit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	nested := filepath.Join(ws.Dir, "vendor", "dep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("make the nested repository's directory: %v", err)
	}
	gitEnv := []string{"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid"}
	runSetupWith(t, nested, gitEnv, "git", "init")
	if err := os.WriteFile(filepath.Join(nested, "patch.txt"), []byte("a local fix\n"), 0o644); err != nil {
		t.Fatalf("write the nested repository's own file: %v", err)
	}
	runSetupWith(t, nested, gitEnv, "git", "add", "patch.txt")
	runSetupWith(t, nested, gitEnv, "git", "commit", "-m", "a local fix")

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], nested) {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept and the nested repository %s", logged, nested)
	}
}

// jj's own nested-repository skip (lib/src/local_working_copy.rs, the pinned fork) is
// `disk_dir.join(name).symlink_metadata().is_ok()` for both ".git" and ".jj" — existence alone,
// no type check at all — so a plain file or a symlink (even a dangling one) of either name hides
// the whole directory from jj's snapshot exactly as a real nested repository's directory does:
// not tracked, not listed as untracked, no warning anywhere. A workspace otherwise fully pushed,
// holding one of these beside an unpushed file, is kept: without nestedRepositories matching every
// entry type, the snapshot would read the workspace clean and RemoveFinished would delete the
// unpushed file along with it.
func TestRemoveFinishedKeepsAWorkspaceHoldingAFileOrSymlinkNamedGitOrJJ(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, path string){
		"a file named .git": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a real git directory\n"), 0o644); err != nil {
				t.Fatalf("plant a file named .git: %v", err)
			}
		},
		"a symlink named .git": func(t *testing.T, path string) {
			if err := os.Symlink(os.TempDir(), path); err != nil {
				t.Fatalf("plant a symlink named .git: %v", err)
			}
		},
		"a dangling symlink named .git": func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(path, "..", "nowhere-at-all"), path); err != nil {
				t.Fatalf("plant a dangling symlink named .git: %v", err)
			}
		},
		"a file named .jj": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a real jj directory\n"), 0o644); err != nil {
				t.Fatalf("plant a file named .jj: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := provisionRequest(t)
			ws, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
				t.Fatalf("write worker change: %v", err)
			}
			runSetup(t, ws.Dir, "jj", "status")
			runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

			nested := filepath.Join(ws.Dir, "vendor", "dep")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatalf("make the nested directory: %v", err)
			}
			secret := filepath.Join(nested, "secret.txt")
			if err := os.WriteFile(secret, []byte("unpushed, never committed anywhere\n"), 0o644); err != nil {
				t.Fatalf("write the hidden unpushed file: %v", err)
			}
			entry := ".git"
			if strings.HasSuffix(name, ".jj") {
				entry = ".jj"
			}
			plant(t, filepath.Join(nested, entry))

			var logged []string
			if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
				t.Fatalf("RemoveFinished: %v", err)
			}
			if _, err := os.Stat(secret); err != nil {
				t.Fatalf("the unpushed file is gone, want it (and the whole workspace) kept: %v", err)
			}
			if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], nested) {
				t.Errorf("logged %v, want one line naming WIDGETS-42 kept and the nested path %s", logged, nested)
			}
		})
	}
}

// A git submodule's own repository lives under the workspace's git worktree admin entry
// (<clone>/.git/worktrees/<ws>/modules/<name>), and the submodule's own checkout, inside the
// workspace, has a `.git` *file* pointing there — exactly the shape a plain file named .git
// produces, confirmed against the real git and jj: an unpushed commit inside a real submodule is
// invisible to the snapshot the same way, and removeGitWorktree deletes the whole worktree admin
// entry (modules included) along with the workspace, so without nestedRepositories matching a
// file too, that commit would exist nowhere on the volume.
func TestRemoveFinishedKeepsAWorkspaceHoldingASubmoduleWithAnUnpushedCommit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	gitEnv := []string{"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid", "GIT_ALLOW_PROTOCOL=file"}
	subSource := filepath.Join(t.TempDir(), "sub-source.git")
	runSetupWith(t, "", gitEnv, "git", "init", "--bare", subSource)
	subClone := t.TempDir()
	runSetupWith(t, subClone, gitEnv, "git", "clone", subSource, ".")
	if err := os.WriteFile(filepath.Join(subClone, "initial.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write the submodule's own initial file: %v", err)
	}
	runSetupWith(t, subClone, gitEnv, "git", "add", "initial.txt")
	runSetupWith(t, subClone, gitEnv, "git", "commit", "-m", "initial")
	runSetupWith(t, subClone, gitEnv, "git", "push", "origin", "HEAD")

	runSetupWith(t, ws.Dir, gitEnv, "git", "-c", "protocol.file.allow=always", "submodule", "add", subSource, "vendor/dep")
	sub := filepath.Join(ws.Dir, "vendor", "dep")
	if err := os.WriteFile(filepath.Join(sub, "local.txt"), []byte("an unpushed local fix\n"), 0o644); err != nil {
		t.Fatalf("write the submodule's own unpushed file: %v", err)
	}
	runSetupWith(t, sub, gitEnv, "git", "add", "local.txt")
	runSetupWith(t, sub, gitEnv, "git", "commit", "-m", "an unpushed local fix")

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "local.txt")); err != nil {
		t.Fatalf("the submodule's unpushed commit is gone, want the workspace kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
}

// A linked git worktree's own checkout, inside the workspace, has a `.git` *file* pointing back
// at the real repository's worktree admin entry — the same shape as the submodule case above. An
// uncommitted edit there is kept, never deleted along with the workspace.
func TestRemoveFinishedKeepsAWorkspaceHoldingALinkedWorktreeWithAnUncommittedEdit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	gitEnv := []string{"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid"}
	source := t.TempDir()
	runSetupWith(t, source, gitEnv, "git", "init")
	if err := os.WriteFile(filepath.Join(source, "initial.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write the source repo's own initial file: %v", err)
	}
	runSetupWith(t, source, gitEnv, "git", "add", "initial.txt")
	runSetupWith(t, source, gitEnv, "git", "commit", "-m", "initial")

	linked := filepath.Join(ws.Dir, "vendor", "dep")
	runSetupWith(t, source, gitEnv, "git", "worktree", "add", "-b", "linked", linked)
	uncommitted := filepath.Join(linked, "uncommitted.txt")
	if err := os.WriteFile(uncommitted, []byte("an uncommitted edit\n"), 0o644); err != nil {
		t.Fatalf("write the linked worktree's own uncommitted file: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(uncommitted); err != nil {
		t.Fatalf("the linked worktree's uncommitted edit is gone, want the workspace kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
}

// jj writes "Warning: Skipped some paths because they are not valid UTF-8" to stderr only, with
// stdout claiming "The working copy has no changes." A workspace otherwise fully pushed, holding
// such a file, is kept: nothing about it reaches untrackedPaths (which reads only stdout), so the
// stderr check is the only thing that catches it.
func TestRemoveFinishedKeepsAWorkspaceHoldingAFileWhoseNameIsNotValidUTF8(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	name := "notes-" + string([]byte{0xff}) + ".md"
	if err := os.WriteFile(filepath.Join(ws.Dir, name), []byte("not valid utf-8 in the name\n"), 0o644); err != nil {
		t.Fatalf("write the invalid-UTF-8-named file: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], "stderr") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept for the snapshot's stderr", logged)
	}
}
