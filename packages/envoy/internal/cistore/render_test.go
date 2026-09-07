package cistore

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mkChecks(spec map[string][2]string) map[string]Check {
	out := make(map[string]Check, len(spec))
	for name, sc := range spec {
		out[name] = Check{Status: sc[0], Conclusion: sc[1]}
	}
	return out
}

func renderOrFail(t *testing.T, s State, head string) (string, Summary) {
	t.Helper()
	raw, err := RenderSummary(s, head)
	if err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	var sum Summary
	if err := json.Unmarshal([]byte(raw), &sum); err != nil {
		t.Fatalf("summary is not valid JSON: %v\n%s", err, raw)
	}
	return raw, sum
}

func assertGroup(t *testing.T, label string, g StatusGroup, want []string) {
	t.Helper()
	if g.Count != len(want) {
		t.Errorf("%s.count = %d, want %d", label, g.Count, len(want))
	}
	if strings.Join(g.Checks, ",") != strings.Join(want, ",") {
		t.Errorf("%s.checks = %v, want %v", label, g.Checks, want)
	}
}

func TestRenderSummaryJSON(t *testing.T) {
	s := State{
		Owner: "sjawhar", Repo: "legion", Number: "13728", SHA: "a1b2c3d9999999",
		Checks: mkChecks(map[string][2]string{
			"infra-tests": {"completed", "failure"},
			"build-image": {"in_progress", ""},
			"snapshots":   {"in_progress", ""},
			"classify":    {"completed", "success"},
			"review":      {"completed", "neutral"},
			"task-tests":  {"queued", ""},
			"docs":        {"completed", "skipped"},
			"lint":        {"completed", "skipped"},
		}),
	}
	_, sum := renderOrFail(t, s, s.SHA)

	if sum.Kind != "checks" || sum.Repo != "sjawhar/legion" || sum.Number != "13728" || sum.SHA != "a1b2c3d9999999" {
		t.Fatalf("identity wrong: %+v", sum)
	}
	assertGroup(t, "failed", sum.Failed, []string{"infra-tests"})
	assertGroup(t, "running", sum.Running, []string{"build-image", "snapshots"})
	assertGroup(t, "passed", sum.Passed, []string{"classify", "review"})
	assertGroup(t, "queued", sum.Queued, []string{"task-tests"})
	assertGroup(t, "skipped", sum.Skipped, []string{"docs", "lint"})
}

func TestRenderSummarySkippedKeepsAllNames(t *testing.T) {
	// Regression: skipped must list every name, never collapse to a bare count.
	spec := map[string][2]string{}
	for i := 0; i < 12; i++ {
		spec["skip-"+string(rune('a'+i))] = [2]string{"completed", "skipped"}
	}
	_, sum := renderOrFail(t, State{Owner: "o", Repo: "r", Number: "9", SHA: "sha", Checks: mkChecks(spec)}, "sha")
	if sum.Skipped.Count != 12 || len(sum.Skipped.Checks) != 12 {
		t.Fatalf("skipped should keep all 12 names, got count=%d names=%d", sum.Skipped.Count, len(sum.Skipped.Checks))
	}
}

func TestRenderSummaryEmptyGroupsArePresent(t *testing.T) {
	raw, sum := renderOrFail(t, State{
		Owner: "o", Repo: "r", Number: "1", SHA: "sha",
		Checks: mkChecks(map[string][2]string{"a": {"completed", "success"}, "b": {"completed", "success"}}),
	}, "sha")
	assertGroup(t, "passed", sum.Passed, []string{"a", "b"})
	for _, empty := range []StatusGroup{sum.Failed, sum.Running, sum.Queued, sum.Skipped} {
		if empty.Count != 0 || len(empty.Checks) != 0 {
			t.Errorf("absent group should be count 0 / empty checks, got %+v", empty)
		}
	}
	// Absent groups serialize with count + [] (stable schema), never null.
	if !strings.Contains(raw, `"failed":{"count":0,"checks":[]}`) {
		t.Errorf("expected empty failed group as {count:0,checks:[]} in %s", raw)
	}
}

func TestRenderSummaryIncludesReviewerVerdicts(t *testing.T) {
	_, sum := renderOrFail(t, State{
		Owner: "sjawhar", Repo: "legion", Number: "42", SHA: "deadbeef",
		Checks: mkChecks(map[string][2]string{
			"tester":     {"completed", "success"},
			"architect":  {"completed", "failure"},
			"unit-tests": {"completed", "success"},
		}),
	}, "deadbeef")
	assertGroup(t, "passed", sum.Passed, []string{"tester", "unit-tests"})
	assertGroup(t, "failed", sum.Failed, []string{"architect"})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status, conclusion string
		want               category
	}{
		{"completed", "failure", catFailed},
		{"completed", "timed_out", catFailed},
		{"completed", "cancelled", catCancelled},
		{"completed", "action_required", catFailed},
		{"completed", "startup_failure", catFailed},
		{"completed", "stale", catFailed},
		{"completed", "success", catPassed},
		{"completed", "neutral", catPassed},
		{"completed", "skipped", catSkipped},
		{"completed", "some_future_conclusion", catFailed},
		{"in_progress", "", catRunning},
		{"queued", "", catQueued},
		{"waiting", "", catQueued},
		{"requested", "", catQueued},
		{"weird_status", "", catQueued},
	}
	for _, c := range cases {
		if got := classify(Check{Status: c.status, Conclusion: c.conclusion}); got != c.want {
			t.Errorf("classify(%q,%q) = %v, want %v", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestRenderSummaryCancelledAndHead(t *testing.T) {
	state := State{
		Owner:  "example-org",
		Repo:   "example-repo",
		Number: "42",
		SHA:    "abcdef1234567",
		Checks: mkChecks(map[string][2]string{
			"cancelled-check": {"completed", "cancelled"},
			"failed-check":    {"completed", "failure"},
		}),
	}

	_, head := renderOrFail(t, state, state.SHA)
	assertGroup(t, "cancelled", head.Cancelled, []string{"cancelled-check"})
	if !head.IsHead {
		t.Fatal("summary for the recorded head must set is_head")
	}

	_, stale := renderOrFail(t, state, "newer-head")
	if stale.IsHead {
		t.Fatal("summary for a stale SHA must clear is_head")
	}
}

// TestRenderSummaryExample prints the JSON for a realistic full CI run so the
// exact checks notification shape is visible in test output (go test -run Example -v).
func TestRenderSummaryExample(t *testing.T) {
	spec := map[string][2]string{
		"infra-tests":      {"completed", "failure"},
		"build-image":      {"in_progress", ""},
		"snapshots":        {"in_progress", ""},
		"classify":         {"completed", "success"},
		"detect-changes":   {"completed", "success"},
		"review":           {"completed", "success"},
		"auto-approve":     {"completed", "success"},
		"vercel":           {"completed", "success"},
		"pr-checks-result": {"completed", "success"},
		"task-tests":       {"queued", ""},
	}
	for i := 1; i <= 12; i++ {
		spec["skip-"+string(rune('a'+i-1))] = [2]string{"completed", "skipped"}
	}
	raw, _ := renderOrFail(t, State{Owner: "citest", Repo: "citest", Number: "13728", SHA: "a1b2c3d9999999", Checks: mkChecks(spec)}, "a1b2c3d9999999")
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, []byte(raw), "", "  ")
	t.Logf("EXAMPLE checks notification:\n%s", pretty.String())
}
