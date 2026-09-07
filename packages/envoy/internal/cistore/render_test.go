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
		out[name] = Check{CheckRunID: 1, Status: sc[0], Conclusion: sc[1]}
	}
	return out
}

func renderOrFail(t *testing.T, s State) (string, Summary) {
	t.Helper()
	sum := renderSummary(s)
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	return string(raw), sum
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
	raw, sum := renderOrFail(t, s)

	if sum.Kind != "checks" || sum.Repo != "sjawhar/legion" || sum.Number != "13728" || sum.SHA != "a1b2c3d9999999" {
		t.Fatalf("identity wrong: %+v", sum)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode rendered summary: %v", err)
	}
	if got := string(payload["generation"]); got != "0" {
		t.Fatalf("generation = %s, want 0", got)
	}
	if got := string(payload["latest_check_run_id"]); got != "1" {
		t.Fatalf("latest_check_run_id = %s, want 1", got)
	}
	assertGroup(t, "failed", sum.Failed, []string{"infra-tests"})
	assertGroup(t, "running", sum.Running, []string{"build-image", "snapshots"})
	assertGroup(t, "passed", sum.Passed, []string{"classify", "review"})
	assertGroup(t, "queued", sum.Queued, []string{"task-tests"})
	assertGroup(t, "skipped", sum.Skipped, []string{"docs", "lint"})
}

func TestRenderSummaryCarriesLatestCheckRunID(t *testing.T) {
	_, sum := renderOrFail(t, State{
		Owner: "example-org", Repo: "example-repo", Number: "42", SHA: "abcdef",
		Checks: map[string]Check{
			"build": {CheckRunID: 900, Status: "completed", Conclusion: "success"},
			"lint":  {CheckRunID: 901, Status: "completed", Conclusion: "success"},
			"test":  {CheckRunID: 1024, Status: "completed", Conclusion: "success"},
		},
	})
	if sum.LatestCheckRunID != 1024 {
		t.Fatalf("latest_check_run_id = %d, want 1024", sum.LatestCheckRunID)
	}
}

func TestRenderSummaryCarriesSnapshotAndLatestCompletedAt(t *testing.T) {
	state := State{
		Owner:  "example-org",
		Repo:   "example-repo",
		Number: "42",
		SHA:    "abcdef",
		Checks: map[string]Check{
			"build": {CheckRunID: 900, Status: "completed", Conclusion: "success", ObservedAt: "2026-09-07T03:00:00Z"},
			"lint":  {CheckRunID: 901, Status: "completed", Conclusion: "failure", ObservedAt: "2026-09-07T03:01:00Z"},
			"test":  {CheckRunID: 902, Status: "in_progress", ObservedAt: "2026-09-07T03:02:00Z"},
		},
	}
	raw, _ := renderOrFail(t, state)
	var payload struct {
		Snapshot          string `json:"snapshot"`
		LatestCompletedAt string `json:"latest_completed_at"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode rendered summary: %v", err)
	}
	if payload.Snapshot != state.Hash() {
		t.Fatalf("snapshot = %q, want state hash %q", payload.Snapshot, state.Hash())
	}
	if payload.LatestCompletedAt != "2026-09-07T03:01:00Z" {
		t.Fatalf("latest_completed_at = %q, want latest completed GitHub timestamp", payload.LatestCompletedAt)
	}
}

func TestRenderSummaryMarksOnlyPreviouslyEmittedSettlementSuperseded(t *testing.T) {
	state := State{
		Owner:      "example-org",
		Repo:       "example-repo",
		Number:     "42",
		SHA:        "abcdef",
		Generation: 7,
		Checks:     mkChecks(map[string][2]string{"build": {"completed", "success"}}),
	}
	_, neverEmittedSummary := renderOrFail(t, state)
	if neverEmittedSummary.SupersededSettlement != "" {
		t.Fatalf("never-emitted superseded_settlement = %q, want empty", neverEmittedSummary.SupersededSettlement)
	}

	state.Generation++
	_, changedButNeverEmitted := renderOrFail(t, state)
	if changedButNeverEmitted.SupersededSettlement != "" {
		t.Fatalf("generation-only superseded_settlement = %q, want empty", changedButNeverEmitted.SupersededSettlement)
	}

	if err := json.Unmarshal([]byte(`{"owner":"example-org","repo":"example-repo","number":"42","sha":"abcdef","generation":9,"emitted_count":1,"checks":{"build":{"check_run_id":900,"status":"completed","conclusion":"success"}}}`), &state); err != nil {
		t.Fatalf("decode previously emitted state: %v", err)
	}
	_, previouslyEmitted := renderOrFail(t, state)
	if previouslyEmitted.SupersededSettlement != "true" {
		t.Fatalf("previously emitted superseded_settlement = %q, want true", previouslyEmitted.SupersededSettlement)
	}
}

func TestRenderSummarySkippedKeepsAllNames(t *testing.T) {
	// Regression: skipped must list every name, never collapse to a bare count.
	spec := map[string][2]string{}
	for i := 0; i < 12; i++ {
		spec["skip-"+string(rune('a'+i))] = [2]string{"completed", "skipped"}
	}
	_, sum := renderOrFail(t, State{Owner: "o", Repo: "r", Number: "9", SHA: "sha", Checks: mkChecks(spec)})
	if sum.Skipped.Count != 12 || len(sum.Skipped.Checks) != 12 {
		t.Fatalf("skipped should keep all 12 names, got count=%d names=%d", sum.Skipped.Count, len(sum.Skipped.Checks))
	}
}

func TestRenderSummaryEmptyGroupsArePresent(t *testing.T) {
	raw, sum := renderOrFail(t, State{
		Owner: "o", Repo: "r", Number: "1", SHA: "sha",
		Checks: mkChecks(map[string][2]string{"a": {"completed", "success"}, "b": {"completed", "success"}}),
	})
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
	})
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

func TestRenderSummaryCancelledAndFailingChecks(t *testing.T) {
	state := State{
		Owner:  "example-org",
		Repo:   "example-repo",
		Number: "42",
		SHA:    "abcdef1234567",
		Checks: map[string]Check{
			"cancelled-check": {CheckRunID: 1, Status: "completed", Conclusion: "cancelled"},
			"failed-check":    {CheckRunID: 2, Status: "completed", Conclusion: "failure", URL: "https://example.test/checks/failed"},
		},
	}

	raw, sum := renderOrFail(t, state)
	assertGroup(t, "cancelled", sum.Cancelled, []string{"cancelled-check"})
	_, passedSummary := renderOrFail(t, State{Checks: mkChecks(map[string][2]string{"passed": {"completed", "success"}})})
	assertGroup(t, "cancelled empty", passedSummary.Cancelled, []string{})
	if got := sum.FailingChecks; len(got) != 1 || got[0].Name != "failed-check" || got[0].URL != "https://example.test/checks/failed" {
		t.Fatalf("failing_checks = %+v", got)
	}
	if !strings.Contains(raw, `"cancelled":{"count":1,"checks":["cancelled-check"]}`) {
		t.Fatalf("cancelled group is not serialized: %s", raw)
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
	raw, _ := renderOrFail(t, State{Owner: "citest", Repo: "citest", Number: "13728", SHA: "a1b2c3d9999999", Checks: mkChecks(spec)})
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, []byte(raw), "", "  ")
	t.Logf("EXAMPLE checks notification:\n%s", pretty.String())
}
