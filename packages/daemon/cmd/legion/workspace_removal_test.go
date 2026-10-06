package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// jjPush pushes bookmark from dir's workspace to origin, whose https://github.com/acme/widgets
// URL is redirected to the volume's local bare remote (withRemote's WINIT_REMOTE) for the one
// invocation, the same insteadOf override the production runner's recordingRunner applies to
// provisioning's own clone and fetch: a shared clone's origin is left at the real GitHub URL for
// ordinary operation, so a test driving jj directly (not through the PATH stub, which never
// rewrites a push's URL) must redirect it itself.
func (v *treeVolume) jjPush(t *testing.T, dir, bookmark string) {
	t.Helper()
	command := exec.Command(v.realJJ, "git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description", "-R", dir)
	command.Env = append(os.Environ(),
		"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
		"GIT_ALLOW_PROTOCOL=https:file",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+v.env["WINIT_REMOTE"]+".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/acme/widgets",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("jj git push --bookmark %s: %v\n%s", bookmark, err, output)
	}
}

// jjFetchBranch fetches exactly bookmark from the real remote (jjPush's own redirect) into dir, a
// workspace of the shared clone: a targeted, unrestricted-by-feed fetch that asks the real remote
// whether bookmark still exists, so jj prunes its local remote-tracking record of it when the
// remote answers that it no longer does (as a squash merge's branch deletion leaves it).
func (v *treeVolume) jjFetchBranch(t *testing.T, dir, bookmark string) {
	t.Helper()
	command := exec.Command(v.realJJ, "git", "fetch", "--remote", "origin", "--branch", "exact:"+bookmark, "--ignore-working-copy", "-R", dir)
	command.Env = append(os.Environ(),
		"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
		"GIT_ALLOW_PROTOCOL=https:file",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+v.env["WINIT_REMOTE"]+".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/acme/widgets",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("jj git fetch --branch exact:%s: %v\n%s", bookmark, err, output)
	}
}

// removableEnv JSON-encodes candidates into LEGION_REMOVABLE_WORKSPACES's combined shape, with a
// notAfter generous enough that no ordinary test call needs its own.
func removableEnv(t *testing.T, candidates []runtime.RemovableWorkspace) string {
	t.Helper()
	return removableEnvWithNotAfter(t, candidates, time.Now().Add(time.Hour))
}

// removableEnvWithNotAfter JSON-encodes candidates and notAfter into LEGION_REMOVABLE_WORKSPACES's
// combined shape, for a test that needs its own notAfter rather than removableEnv's generous
// default.
func removableEnvWithNotAfter(t *testing.T, candidates []runtime.RemovableWorkspace, notAfter time.Time) string {
	t.Helper()
	encoded, err := json.Marshal(removableWorkspacesPayload{NotAfter: notAfter, Workspaces: candidates})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// panicRunner fails the test if ever asked to run a command: a candidate this test names has no
// workspace on this volume, so RemoveFinished's "nothing to remove" path never reaches the
// runner at all, budgeted out or not.
type panicRunner struct{ t *testing.T }

func (p panicRunner) Timeout() time.Duration { return winitWait }
func (p panicRunner) Run(context.Context, workspace.Command) (workspace.Result, error) {
	p.t.Fatal("a candidate with no workspace on this volume ran a command")
	return workspace.Result{}, nil
}

// A malformed candidate never reaches workspace.Location or RemoveFinished's revset: an issue key
// that is not the Dispatch form, and a mergedHead that is not 40 hex characters, are each refused
// and logged, never acted on — panicRunner would fail the test the moment either one tried to run
// a command. A well-formed candidate alongside them still runs normally.
func TestRemoveFinishedWorkspacesRefusesAMalformedCandidate(t *testing.T) {
	root := t.TempDir()
	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []runtime.RemovableWorkspace{
		{Issue: "not an issue key"},
		{Issue: "LEGION-101", MergedHead: "not 40 hex characters"},
		{Issue: "LEGION-102", MergedHead: "deadbeef"}, // 8 hex characters, still not 40
		{Issue: "LEGION-103"},
	}
	t.Setenv(removableWorkspacesEnv, removableEnv(t, candidates))

	var stdout bytes.Buffer
	removeFinishedWorkspaces(context.Background(), panicRunner{t}, root, repository, "LEGION-200", &stdout, time.Now, removalBudget, time.Now())

	output := stdout.String()
	for _, want := range []string{
		`"not an issue key" is not a Dispatch issue key`,
		`LEGION-101's mergedHead "not 40 hex characters" is not 40 hex characters`,
		`LEGION-102's mergedHead "deadbeef" is not 40 hex characters`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("stdout %q, want it to contain %q", output, want)
		}
	}
	if want := "LEGION-103 has no workspace on this volume; nothing to remove"; !strings.Contains(output, want) {
		t.Errorf("stdout %q, want the well-formed candidate LEGION-103 to still run: %q", output, want)
	}
}

// A removal pass whose own workspace-fetch started after the removable-workspaces list's notAfter
// removes nothing at all and logs why, never reaching a single candidate: a pod the Sandbox
// controller recreated on its own gets a fresh workspace-fetch, with its own later start time,
// whatever its clone then takes — comparing that start, not wall-clock time at removal, is what
// keeps this bound independent of the clone's own duration (LEGION-585).
func TestRemoveFinishedWorkspacesRemovesNothingPastItsNotAfter(t *testing.T) {
	root := t.TempDir()
	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}
	notAfter := time.Now().Add(-time.Minute)
	t.Setenv(removableWorkspacesEnv, removableEnvWithNotAfter(t, candidates, notAfter))

	var stdout bytes.Buffer
	removeFinishedWorkspaces(context.Background(), panicRunner{t}, root, repository, "LEGION-200", &stdout, time.Now, removalBudget, time.Now())

	output := stdout.String()
	if !strings.Contains(output, "past the removable-workspaces list's notAfter") {
		t.Errorf("stdout %q, want it to name notAfter", output)
	}
	if strings.Contains(output, "LEGION-100") {
		t.Errorf("stdout %q names LEGION-100 at all, want the pass to stop before reaching any candidate", output)
	}
}

// The rotation seed changes with LEGION_GENERATION, not only the pod's own issue: ten relaunches
// of the same issue, each spending its whole budget on the first candidate it starts, started
// more than one of four candidates across those ten relaunches rather than always the same one —
// seeding by issue alone starved the same candidates on every relaunch of one issue, since the
// issue never changes between them while the generation always does.
func TestRemoveFinishedWorkspacesRotatesByGenerationNotJustIssue(t *testing.T) {
	root := t.TempDir()
	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []runtime.RemovableWorkspace{{Issue: "LEGION-100"}, {Issue: "LEGION-101"}, {Issue: "LEGION-102"}, {Issue: "LEGION-103"}}
	t.Setenv(removableWorkspacesEnv, removableEnv(t, candidates))

	started := map[string]bool{}
	for generation := 1; generation <= 10; generation++ {
		t.Setenv("LEGION_GENERATION", strconv.Itoa(generation))
		t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
		const budget = 30 * time.Second
		calls := 0
		now := func() time.Time {
			calls++
			if calls <= 2 {
				return t0
			}
			return t0.Add(budget + time.Second)
		}
		var stdout bytes.Buffer
		removeFinishedWorkspaces(context.Background(), panicRunner{t}, root, repository, "LEGION-200", &stdout, now, budget, time.Now())
		for _, candidate := range candidates {
			if strings.Contains(stdout.String(), candidate.Issue+" has no workspace on this volume") {
				started[candidate.Issue] = true
			}
		}
	}
	if len(started) < 2 {
		t.Fatalf("ten relaunches of LEGION-200 started only %v, want more than one candidate across them", started)
	}
}

// One issue's own phase workers on their first launches (planner, implementer, tester, reviewer,
// merger, all at generation 1) must not share a rotation: a generation-only seed starts the same
// candidate for every one of them. Adding LEGION_ROLE to the seed is what closes that.
func TestRemoveFinishedWorkspacesRotatesByRoleNotJustIssueAndGeneration(t *testing.T) {
	root := t.TempDir()
	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []runtime.RemovableWorkspace{{Issue: "LEGION-100"}, {Issue: "LEGION-101"}, {Issue: "LEGION-102"}, {Issue: "LEGION-103"}}
	t.Setenv(removableWorkspacesEnv, removableEnv(t, candidates))
	t.Setenv("LEGION_GENERATION", "1")

	started := map[string]bool{}
	for _, role := range []string{"planner", "implementer", "tester", "reviewer", "merger"} {
		t.Setenv("LEGION_ROLE", role)
		t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
		const budget = 30 * time.Second
		calls := 0
		now := func() time.Time {
			calls++
			if calls <= 2 {
				return t0
			}
			return t0.Add(budget + time.Second)
		}
		var stdout bytes.Buffer
		removeFinishedWorkspaces(context.Background(), panicRunner{t}, root, repository, "LEGION-200", &stdout, now, budget, time.Now())
		for _, candidate := range candidates {
			if strings.Contains(stdout.String(), candidate.Issue+" has no workspace on this volume") {
				started[candidate.Issue] = true
			}
		}
	}
	if len(started) < 2 {
		t.Fatalf("all five roles' first launches of LEGION-200 started only %v, want more than one candidate across them", started)
	}
}

// rotateCandidates starts a different pass at a different point in the candidate list,
// deterministically from the pod's own issue: two different issues produce two different
// rotations of the same four candidates (every element still present, in the same cyclic order),
// and the same issue always produces the same rotation.
func TestRotateCandidatesStartsAtADifferentPointPerIssue(t *testing.T) {
	candidates := []runtime.RemovableWorkspace{{Issue: "LEGION-100"}, {Issue: "LEGION-101"}, {Issue: "LEGION-102"}, {Issue: "LEGION-103"}}
	issues := func(rotated []runtime.RemovableWorkspace) []string {
		names := make([]string, len(rotated))
		for i, c := range rotated {
			names[i] = c.Issue
		}
		return names
	}
	byIssue := map[string][]string{}
	for _, seed := range []string{"LEGION-200", "LEGION-201", "LEGION-202", "LEGION-203", "LEGION-204"} {
		rotated := rotateCandidates(candidates, seed)
		if len(rotated) != len(candidates) {
			t.Fatalf("rotateCandidates(%q) = %v, want all %d candidates present", seed, issues(rotated), len(candidates))
		}
		seen := map[string]bool{}
		for _, c := range rotated {
			seen[c.Issue] = true
		}
		if len(seen) != len(candidates) {
			t.Fatalf("rotateCandidates(%q) = %v, want every candidate exactly once", seed, issues(rotated))
		}
		byIssue[seed] = issues(rotated)
		// Deterministic: the same seed rotates the same way every time.
		if again := issues(rotateCandidates(candidates, seed)); !slices.Equal(again, byIssue[seed]) {
			t.Errorf("rotateCandidates(%q) = %v, then %v: not deterministic", seed, byIssue[seed], again)
		}
	}
	distinct := map[string]bool{}
	for _, order := range byIssue {
		distinct[strings.Join(order, ",")] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("every seed in %v produced the same order %v, want at least two different rotations", byIssue, distinct)
	}
}

// removeFinishedWorkspaces stops starting new candidates once its budget is spent, measured from
// its own first check, and logs the rest as deferred to the tree's next launch, rather than run a
// long candidate list past the registration deadline this init container shares with the
// provisioning it still has to report done. It never interrupts a candidate already started (the
// first two here, named before the fake clock crosses the budget, both still run; only the
// third, and everything after it, is deferred).
func TestRemoveFinishedWorkspacesDefersCandidatesPastItsBudget(t *testing.T) {
	root := t.TempDir()
	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	const budget = 30 * time.Second
	calls := 0
	now := func() time.Time {
		calls++
		// The budget-establishing call and the first two in-loop checks (one per candidate
		// started) land before the budget is spent; every later call is well past it, as if
		// the first two candidates together had spent the whole budget.
		if calls <= 3 {
			return t0
		}
		return t0.Add(budget + time.Second)
	}
	candidates := []runtime.RemovableWorkspace{{Issue: "LEGION-100"}, {Issue: "LEGION-101"}, {Issue: "LEGION-102"}, {Issue: "LEGION-103"}}
	t.Setenv(removableWorkspacesEnv, removableEnv(t, candidates))

	var stdout bytes.Buffer
	removeFinishedWorkspaces(context.Background(), panicRunner{t}, root, repository, "LEGION-200", &stdout, now, budget, time.Now())

	output := stdout.String()
	all := []string{"LEGION-100", "LEGION-101", "LEGION-102", "LEGION-103"}
	var started, deferred []string
	for _, name := range all {
		if strings.Contains(output, name+" has no workspace on this volume; nothing to remove") {
			started = append(started, name)
		} else {
			deferred = append(deferred, name)
		}
	}
	// Rotated by issue (rotateCandidates), so which two start and which two defer depends on the
	// seed, not the candidates' own order; exactly two of the four start before the fake clock
	// crosses the budget, and the other two are the ones the deferral line names.
	if len(started) != 2 {
		t.Fatalf("stdout %q, started %v, want exactly 2 candidates started", output, started)
	}
	if len(deferred) != 2 {
		t.Fatalf("stdout %q, deferred %v, want exactly 2 candidates deferred", output, deferred)
	}
	for _, name := range deferred {
		if !strings.Contains(output, name) {
			t.Errorf("stdout %q does not name deferred candidate %s", output, name)
		}
	}
	if want := fmt.Sprintf("removal budget (%s) spent; deferring %d candidate(s) to the tree's next launch:", budget, len(deferred)); !strings.Contains(output, want) {
		t.Errorf("stdout %q, want it to contain %q", output, want)
	}
}

// A done child with every commit pushed — parked after its own push — is removed at the next pod
// launch of its tree: the daemon names it in LEGION_REMOVABLE_WORKSPACES, and workspace-init,
// provisioning a different issue's pod, removes it.
func TestWorkspaceInitRemovesAFinishedChildWithEveryCommitPushed(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the finished child: exit %d, stderr %q", code, stderr)
	}
	finished := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(finished, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", finished)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", finished)
	v.jjPush(t, finished, bookmark)

	t.Setenv("LEGION_REMOVABLE_WORKSPACES", removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}))
	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LEGION-100's workspace remains: %v", err)
	}
	if want := "removed LEGION-100's workspace"; !strings.Contains(stdout, want) {
		t.Errorf("stdout %q, want it to contain %q", stdout, want)
	}
}

// A done child with every commit pushed is removed even when the pod that provisions LEGION-200
// and runs the removal pass has never touched this repository from any config home before: every
// committed test above shares one XDG_CONFIG_HOME across both pods (newTreeVolume's own), which
// production never does (every init container starts with an empty one of its own). Giving
// LEGION-200's own pod a second, genuinely empty XDG_CONFIG_HOME/JJ_CONFIG pair still removes the
// candidate: provisioning LEGION-200 runs jj against the shared clone first (workspace_init.go's
// own call-site comment explains why that is what this relies on), migrating this pod's own copy
// of the clone's per-repo config before the removal pass's candidate snapshot ever runs.
func TestWorkspaceInitRemovesAFinishedChildEvenWithAFreshConfigHomeForTheSecondPod(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the finished child: exit %d, stderr %q", code, stderr)
	}
	finished := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(finished, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", finished)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", finished)
	v.jjPush(t, finished, bookmark)

	// LEGION-200's own pod: a config home this repository's per-repo config has never been
	// migrated into, unlike v.env's (which provisioned LEGION-100 and pushed it above).
	fresh := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fresh, "config"))
	t.Setenv("JJ_CONFIG", filepath.Join(fresh, "no-user-config.toml"))

	t.Setenv("LEGION_REMOVABLE_WORKSPACES", removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}))
	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LEGION-100's workspace remains: %v", err)
	}
	if want := "removed LEGION-100's workspace"; !strings.Contains(stdout, want) {
		t.Errorf("stdout %q, want it to contain %q", stdout, want)
	}
}

// A clean, pushed candidate is still removed even when this pod's own clone — workspace-fetch's,
// which LEGION-585 bounds at up to 30 minutes rather than the 5-minute command cap — takes long
// enough that wall-clock time at removal is already past the list's notAfter: what the fetch-start
// comparison checks is this pod's own fetch, recorded once before the clone ever started, not how
// long the clone (or anything after it) then took. Before this fix, comparing wall-clock time at
// removal instead meant a slow clone on exactly the large repositories LEGION-585 exists for could
// make every launch skip removal, never once failing safe into actually removing anything.
func TestWorkspaceInitRemovesACleanCandidateEvenWhenItsCloneOutlastsTheWindow(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the candidate: exit %d, stderr %q", code, stderr)
	}
	candidate := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(candidate, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", candidate)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", candidate)
	v.jjPush(t, candidate, bookmark)

	fetchStart := readFetchStartedForTest(t, v.feed)
	// notAfter just past this pod's own real fetch start: whatever the clone (simulated by the
	// sleep below) then takes is irrelevant to the comparison this exercises.
	notAfter := fetchStart.Add(50 * time.Millisecond)
	t.Setenv(removableWorkspacesEnv, removableEnvWithNotAfter(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}, notAfter))
	time.Sleep(150 * time.Millisecond) // wall-clock time at removal is now already past notAfter

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LEGION-100's workspace remains: %v", err)
	}
	if want := "removed LEGION-100's workspace"; !strings.Contains(stdout, want) {
		t.Errorf("stdout %q, want it to contain %q", stdout, want)
	}
}

// A pod the Sandbox controller recreates on its own gets a fresh workspace-fetch, whose own start
// time is long after the removable-workspaces list's notAfter was stamped: removal removes
// nothing at all, logging why, rather than act on a list that may by then be hours old.
func TestWorkspaceInitRemovesNothingWhenThisPodsFetchStartedLongAfterTheStamp(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the candidate: exit %d, stderr %q", code, stderr)
	}
	candidate := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(candidate, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", candidate)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", candidate)
	v.jjPush(t, candidate, bookmark)

	fetchStart := readFetchStartedForTest(t, v.feed)
	// notAfter stamped an hour before this pod's own real fetch start: as if the daemon computed
	// the list for a pod the controller only recreated an hour later.
	notAfter := fetchStart.Add(-time.Hour)
	t.Setenv(removableWorkspacesEnv, removableEnvWithNotAfter(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}, notAfter))

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("LEGION-100's workspace was removed, want it kept: %v", err)
	}
	if !strings.Contains(stdout, "past the removable-workspaces list's notAfter") {
		t.Errorf("stdout %q, want it to name notAfter", stdout)
	}
	if strings.Contains(stdout, "LEGION-100 has no workspace") || strings.Contains(stdout, "removed LEGION-100") {
		t.Errorf("stdout %q names LEGION-100 reached at all, want the pass to stop before any candidate", stdout)
	}
}

// readFetchStartedForTest reads fetchStartedFile straight from feed, as workspace-init provision
// itself does, for a test that needs to compute its own notAfter relative to it.
func readFetchStartedForTest(t *testing.T, feed string) time.Time {
	t.Helper()
	started, err := readFetchStarted(feed)
	if err != nil {
		t.Fatalf("read this test's own fetch-started file: %v", err)
	}
	return started
}

// A done child whose last commit is only the merged pull request's head — GitHub deletes a squash
// merge's branch, leaving that commit with no remote bookmark of its own — is removed: the
// daemon's recorded merged head tells it from a commit that was never pushed at all.
func TestWorkspaceInitRemovesADoneChildWhoseLastCommitIsTheMergedHead(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the merged child: exit %d, stderr %q", code, stderr)
	}
	merged := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(merged, "feature.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", merged)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", merged)
	mergedHead := v.jj(t, "log", "-r", bookmark, "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never", "-R", merged)
	v.jjPush(t, merged, bookmark)
	v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "branch", "-D", bookmark)
	// LEGION-200's own provisioning fetch never asks about legion/LEGION-100 (ensureFetchConfiguration
	// restricts every fetch to main plus the issue's own bookmark), so the shared clone's record of
	// legion/LEGION-100@origin, set by the push above, would otherwise stay frozen at the merged
	// head forever and satisfy the push-safety check by a live remote bookmark alone, never
	// exercising MergedHead. A direct, targeted fetch of that one bookmark against the real remote
	// (jjPush's own redirect, restricted to just it) is what a daemon-driven resync would also do,
	// and is what makes jj prune the now-deleted branch from the shared clone's local view.
	v.jjFetchBranch(t, v.clone(), bookmark)

	t.Setenv("LEGION_REMOVABLE_WORKSPACES", removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100", MergedHead: mergedHead}}))
	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(merged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LEGION-100's workspace remains: %v", err)
	}
	if want := "removed LEGION-100's workspace"; !strings.Contains(stdout, want) {
		t.Errorf("stdout %q, want it to contain %q", stdout, want)
	}
}

// A child with a commit that reached neither a remote bookmark nor a recorded merged head — local
// work no push ever carried — is kept and named, never deleted.
func TestWorkspaceInitKeepsAChildWithAnUnpushedCommit(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the unpushed child: exit %d, stderr %q", code, stderr)
	}
	unpushed := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(unpushed, "unpushed.txt"), []byte("never pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", unpushed)
	commit := v.jj(t, "log", "-r", "@", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never", "-R", unpushed)

	t.Setenv("LEGION_REMOVABLE_WORKSPACES", removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}))
	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-200"))
	if code != 0 {
		t.Fatalf("provision LEGION-200: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(unpushed); err != nil {
		t.Fatalf("LEGION-100's workspace was removed, want it kept: %v", err)
	}
	if !strings.Contains(stdout, "kept LEGION-100's workspace") || !strings.Contains(stdout, commit) {
		t.Errorf("stdout %q, want it to name LEGION-100 kept and commit %s", stdout, commit)
	}
}

// The issue this pod provisions is live by definition and is never touched even if the daemon's
// candidate list names it — a defensive floor workspace-init holds on its own, never trusting the
// list alone to keep a live child's workspace untouched.
func TestWorkspaceInitNeverRemovesTheIssueItIsProvisioning(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	t.Setenv("LEGION_REMOVABLE_WORKSPACES", removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}))
	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-100"))
	if code != 0 {
		t.Fatalf("provision LEGION-100: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(v.workspace("LEGION-100")); err != nil {
		t.Fatalf("the pod's own workspace is gone: %v", err)
	}
	if strings.Contains(stdout, "LEGION-100's workspace") {
		t.Errorf("stdout %q named LEGION-100's own workspace for removal", stdout)
	}
}

// RemoveFinished's own nested-repository walk is bounded by its own walkTimeout parameter alone,
// never by how much of removeFinishedWorkspaces' own removal budget happens to be left (a slow
// snapshot must not leave the walk nothing, dispatch://LEGION-583). A walk timeout of zero keeps
// a clean, pushed candidate deterministically — no file needed to make the walk itself slow, and
// no dependence on how long this host's own `jj status` happens to take.
func TestWorkspaceInitKeepsAChildWhenTheNestedRepositoryWalkTimesOut(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the candidate: exit %d, stderr %q", code, stderr)
	}
	candidate := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(candidate, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", candidate)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", candidate)
	v.jjPush(t, candidate, bookmark)

	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	located, err := workspace.Location(v.root, repository, "LEGION-100")
	if err != nil {
		t.Fatal(err)
	}
	run := workspace.NewRunner(workspace.CommandTimeout, map[string]string{"jj": v.realJJ, "git": v.env["WINIT_REAL_GIT"]})

	var logged []string
	if err := workspace.RemoveFinished(context.Background(), run, located, "LEGION-100", "", 0, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("LEGION-100's workspace was removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept LEGION-100's workspace") || !strings.Contains(logged[0], "ran out of its own") {
		t.Errorf("logged %v, want one line naming LEGION-100 kept for running out of its own time limit", logged)
	}
}

// A candidate whose snapshot alone outlasts the removal pass's own budget is still fully judged
// and removed when it is in fact clean: the walk's own fixed timeout, counted from when the walk
// itself starts rather than from the pass's own deadline, is what removalBudget's doc comment
// means by "never interrupts one already running" (dispatch://LEGION-583) — the budget bounds
// only when a new candidate may start, never how much of its own already-running check it gets,
// so a slow snapshot alone cannot exhaust what the walk has left before the walk even begins.
func TestWorkspaceInitRemovesACleanChildEvenWhenItsSnapshotOutlastsTheRemovalBudget(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	if code, _, stderr := runWorkspaceInitHere(v.args("LEGION-100")); code != 0 {
		t.Fatalf("provision the candidate: exit %d, stderr %q", code, stderr)
	}
	candidate := v.workspace("LEGION-100")
	if err := os.WriteFile(filepath.Join(candidate, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "status", "-R", candidate)
	bookmark := "legion/LEGION-100"
	v.jj(t, "bookmark", "set", bookmark, "-r", "@", "--allow-backwards", "-R", candidate)
	v.jjPush(t, candidate, bookmark)

	repository, err := ghrepo.Parse("--repo", winitRepo)
	if err != nil {
		t.Fatal(err)
	}
	real := workspace.NewRunner(workspace.CommandTimeout, map[string]string{"jj": v.realJJ, "git": v.env["WINIT_REAL_GIT"]})
	run := slowSnapshotRunner{Runner: real, sleep: 2500 * time.Millisecond}
	t.Setenv(removableWorkspacesEnv, removableEnv(t, []runtime.RemovableWorkspace{{Issue: "LEGION-100"}}))

	var stdout bytes.Buffer
	removeFinishedWorkspaces(context.Background(), run, v.root, repository, "LEGION-200", &stdout, time.Now, 2*time.Second, time.Now())

	output := stdout.String()
	if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LEGION-100's workspace remains (err=%v), want removed: it was clean despite its snapshot outlasting the 2s removal budget", err)
	}
	if want := "removed LEGION-100's workspace"; !strings.Contains(output, want) {
		t.Errorf("stdout %q, want it to contain %q", output, want)
	}
}

// slowSnapshotRunner sleeps after the push-safety snapshot command (`jj status`) returns,
// simulating a snapshot that takes real wall-clock time, as a near-full tree volume's does.
type slowSnapshotRunner struct {
	workspace.Runner
	sleep time.Duration
}

func (r slowSnapshotRunner) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	result, err := r.Runner.Run(ctx, command)
	if len(command.Argv) >= 2 && command.Argv[0] == "jj" && command.Argv[1] == "status" {
		time.Sleep(r.sleep)
	}
	return result, err
}
