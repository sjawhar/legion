package cireconcile

import "testing"

// absentRow is a repository the App is installed on that GitHub then answers 404 for. It sorts
// before the mapped repository, so a pass reads it first and has to go on from it.
const absentRow = "acme/absent"

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

// No bucket yet, no pass: a pass that runs before the listener has made envoy_ci_state binds no
// writer, says so and reads GitHub nothing -- there is nowhere to put what it would read -- and
// the pass after the listener has made the bucket records the head and settles it.
func TestNoBucketYetNoPass(t *testing.T) {
	h := newHarnessBeforeTheListener(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

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
}
