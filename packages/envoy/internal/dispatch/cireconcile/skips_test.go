package cireconcile

import (
	"strings"
	"testing"
)

// absentRow is a repository the App is installed on that GitHub then answers 404 for. It sorts
// before the mapped repository, so a pass reads it first and has to go on from it.
const absentRow = "acme/absent"

// renamedAwayRow is the name the mapped repository was renamed away from, held by a mapping of
// its own as repo_projects holds both names of one repository. The GitHub stand-in knows no
// repository under it, so every read of it is a 404, as GitHub answers for a name a repository
// no longer has.
const renamedAwayRow = "acme/widgets-before"

// A mapping GitHub does not answer for is skipped, counted and named, and the pass goes on to
// the repositories after it: one repository GitHub has stopped answering about must not cost
// every other repository its pass.
func TestAMappingGitHubDoesNotAnswerIsSkipped(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(absentRow)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.hideRepository(absentRow)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over a repository GitHub did not answer for")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the second repository's head settled on its one passing check", settled)
	}
	line := h.passLine()
	if got := number(t, line, "mappings_skipped"); got != 1 {
		t.Errorf("the pass line reports %d mappings skipped, want 1: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass read %d heads, want the one head of the repository GitHub answered for: %v", got, line)
	}
	skipped := h.lines("ci reconcile skipped a repository GitHub did not answer for")
	if len(skipped) != 1 {
		t.Fatalf("the skipped repository was named %d times, want once:\n%s", len(skipped), h.logs.String())
	}
	if skipped[0]["repository"] != absentRow {
		t.Fatalf("the WARN names repository %v, want %s", skipped[0]["repository"], absentRow)
	}
}

// A mapping whose repository was renamed away is reconciled under the name the repository is
// called today, so the pass asks GitHub nothing about the old name and skips nothing for it.
// GitHub answers 404 for the installation of a name a repository was renamed away from, so a
// pass reading that row's own name holds no token for it and logs `ci reconcile skipped a
// repository it holds no token for` on every pass for ever, about a name no one can read by
// design. The mapping that names no rename is reconciled exactly as it was: its head settles on
// the checks GitHub holds.
func TestARenamedMappingIsReconciledUnderTheNameItHasNow(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over a mapping renamed away and the mapping it names")
	if settled.Number != pullNumber || settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head of %s settled on its one passing check", settled, gitHubRepository)
	}
	if skipped := h.lines("ci reconcile skipped a repository it holds no token for"); len(skipped) != 0 {
		t.Fatalf("the pass skipped %d repositories it holds no token for, want none:\n%s", len(skipped), h.logs.String())
	}
	for _, request := range h.github.received() {
		if strings.Contains(strings.ToLower(request.url), renamedAwayRow) {
			t.Errorf("the pass asked GitHub about the name the repository was renamed away from: %s %s", request.method, request.url)
		}
	}
	line := h.passLine()
	if got := number(t, line, "mappings_skipped"); got != 0 {
		t.Errorf("the pass line reports %d mappings skipped, want none: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass read %d heads, want the one head of the repository both rows name: %v", got, line)
	}
	if sent := h.github.countPath("/repos/" + gitHubRepository + "/pulls"); sent != 1 {
		t.Errorf("the open pull requests of %s were read %d times, want once for two rows naming it", gitHubRepository, sent)
	}
}

// No bucket yet, no pass: a pass that runs before the listener has made envoy_ci_state binds no
// writer, says so and reads GitHub nothing -- there is nowhere to put what it would read -- and
// the pass after the listener has made the bucket records the head and settles it.
//
// Each pass binds once and keeps nothing: the bus dials a replacement when nats.go gives up on
// a connection, and a writer held from an earlier pass would write through the connection it
// gave up on, so every later pass would read GitHub in full and record nothing until a restart.
func TestNoBucketYetNoPass(t *testing.T) {
	h := newHarnessBeforeTheListener(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	// A pass runs on the test's own goroutine, so a plain count is read and written by one.
	binds := 0
	bind := h.reconciler.Open
	h.reconciler.Open = func() (Writer, error) {
		binds++
		return bind()
	}

	h.pass()

	if bound := h.lines("ci reconcile pass bound no CI record writer"); len(bound) != 1 {
		t.Fatalf("the pass logged %d lines saying it bound no writer, want one:\n%s", len(bound), h.logs.String())
	}
	if lines := h.lines("ci reconcile pass"); len(lines) != 0 {
		t.Fatalf("a pass that bound no writer logged a pass line: %v", lines)
	}
	if sent := h.github.count(); sent != 0 {
		t.Fatalf("a pass that bound no writer sent GitHub %d requests: %+v", sent, h.github.received())
	}
	if binds != 1 {
		t.Fatalf("the pass called Open %d times, want once", binds)
	}

	// The listener boots and makes the bucket, as it does on its own clock.
	h.startListener()
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after the listener made the bucket")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass that bound the writer")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want ci-ok", state.Checks)
	}

	h.pass()

	if binds != 3 {
		t.Fatalf("Open was called %d times over three passes, want once a pass: a pass that kept the writer it bound would write through a connection the client may have replaced", binds)
	}
}
