package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", mergedHead, func(line string) { logged = append(logged, line) }); err != nil {
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
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
// directly. `jj config set --repo` (config.go's own `ensureFetchConfiguration` sets
// git.abandon-unreachable-commits this way) writes every workspace of the repository's one shared
// repo config, wherever it is run from: a tree agent's own pane, in its own issue's workspace, can
// run it once and reach every later provisioning of the tree, on the tree volume every pod of the
// tree mounts read-write — confirmed directly: a `jj config set --repo` run against one jj
// workspace of a repo is read by a plain `jj status` run against a different workspace of the same
// repo, with no migration or cache file involved (unlike the legacy per-workspace
// `.jj/workspace-config.toml` isolation_test.go's filter test plants, which this is not). Repo
// config naming the working-copy filter and a signing backend, behavior, key and program runs
// neither program on RemoveFinished's snapshot. A pending edit untouched by any jj command still
// keeps the workspace, proving the snapshot itself still ran.
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
	// Planted against the shared clone with --ignore-working-copy, as every other command this
	// package runs against it does: --repo config is the repository's own, not this one
	// invocation's workspace, and reaches ws.Dir's later snapshot exactly as it would reach any
	// other workspace of the tree a phase worker's own pane set it from.
	for _, setting := range [][2]string{
		{"git.filter.enabled", "true"},
		{"git.filter.drivers.planted.required", "false"},
		{"git.filter.drivers.planted.clean", `["` + filter + `"]`},
		{"git.filter.drivers.planted.smudge", `["cat"]`},
		{"fsmonitor.backend", `"watchman"`},
		{"signing.backend", `"ssh"`},
		{"signing.behavior", `"own"`},
		{"signing.key", `"` + signKey + `"`},
		{"signing.backends.ssh.program", `"` + signProgram + `"`},
	} {
		runSetup(t, ws.Clone, "jj", "config", "set", "--repo", "--ignore-working-copy", setting[0], setting[1])
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, ".gitattributes"), []byte("*.txt filter=planted\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
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
// untracked with exit 0 and a warning rather than an error, under its default
// snapshot.auto-track = "all()" (jj help -k config): every other new file is auto-tracked, so
// "Untracked paths:" on `jj status`'s own stdout is never printed except for a refusal. Without
// snapshotOverrides' snapshot.max-new-file-size=0 and the untrackedPaths check that follows it, @
// would stay empty of a 2MiB uncommitted file and the push-safety check would read the workspace
// clean and remove it; either defense alone keeps it (the override commits the file, so
// unpushedRevset's own reachability check then keeps it; untrackedPaths would keep it directly if
// jj ever left it untracked despite the override), so this proves the end-to-end outcome without
// binding the test to which one engaged. TestUntrackedPathsReadsJJsOwnSection below proves the
// parser itself against jj's documented output shape, independent of this override.
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
}

// untrackedPaths reads exactly the shape jj status --color=never prints (confirmed against the
// pinned jj, 0.45.1-sami): the literal line "Untracked paths:", then one "? <path>" per path,
// ending at the first line that is not one (jj follows the section with the working-copy summary
// lines). A status with nothing left untracked prints no such header at all.
func TestUntrackedPathsReadsJJsOwnSection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   []string
	}{
		{"no untracked section", "Working copy changes:\nA tracked.txt\nWorking copy  (@) : abc def\n", nil},
		{"one untracked path", "Working copy changes:\nA tracked.txt\nUntracked paths:\n? large.bin\nWorking copy  (@) : abc def\n", []string{"large.bin"}},
		{"more than one, nothing else tracked", "Untracked paths:\n? a.bin\n? b.bin\nWorking copy  (@) : abc def\n", []string{"a.bin", "b.bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := untrackedPaths(tc.stdout)
			if len(got) != len(tc.want) {
				t.Fatalf("untrackedPaths = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("untrackedPaths = %v, want %v", got, tc.want)
				}
			}
		})
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
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
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if want := "has no workspace on this volume"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}
