package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixtureName is a check name as packages/contracts/fixtures/checks-settlement writes it: a string,
// or {repeat, times, then}, repeat written times over and then then.
type fixtureName string

func (n *fixtureName) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*n = fixtureName(name)
		return nil
	}
	var built struct {
		Repeat string `json:"repeat"`
		Times  int    `json:"times"`
		Then   string `json:"then"`
	}
	if err := json.Unmarshal(data, &built); err != nil {
		return err
	}
	*n = fixtureName(strings.Repeat(built.Repeat, built.Times) + built.Then)
	return nil
}

// The pass settles a head as Envoy's listener would have from the same check runs: one fixture,
// packages/contracts/fixtures/checks-settlement/listener-rules.json, holds GitHub's runs on a head
// and the settlement the listener publishes for them, and Envoy's own test
// (packages/envoy/internal/cistore, TestTheListenerSettlesCheckRunsAsTheSharedFixtureSays) drives
// the listener's webhook handler, CI store and summary loop with the same runs, so a rule changed
// on one side and not the other fails one of the two. Each run is decoded as githubrest decodes
// GitHub's check-runs answer.
func TestThePassSettlesCheckRunsAsEnvoysListenerDoes(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", "listener-rules.json"))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fixture struct {
		ReviewerAppID string                       `json:"reviewerAppId"`
		CheckRuns     []map[string]json.RawMessage `json:"checkRuns"`
		Settlement    struct {
			CheckRuns []struct {
				Name fixtureName `json:"name"`
				ID   int64       `json:"id"`
			} `json:"check_runs"`
			Failed    []fixtureName `json:"failed"`
			Cancelled []fixtureName `json:"cancelled"`
		} `json:"settlement"`
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the shared fixture: %v", err)
	}
	runs := make([]githubrest.CheckRun, 0, len(fixture.CheckRuns))
	var newest time.Time
	for _, raw := range fixture.CheckRuns {
		var name fixtureName
		if err := json.Unmarshal(raw["name"], &name); err != nil {
			t.Fatalf("decode a run's name: %v", err)
		}
		raw["name"], _ = json.Marshal(string(name))
		answered, _ := json.Marshal(raw)
		var run githubrest.CheckRun
		if err := json.Unmarshal(answered, &run); err != nil {
			t.Fatalf("decode a run as GitHub answers it: %v", err)
		}
		if run.CompletedAt.After(newest) {
			newest = *run.CompletedAt
		}
		runs = append(runs, run)
	}
	want := intake.PullRequestChecks{Repo: "acme/widgets", Number: 86, HeadSHA: "code", Generation: classify.PassGeneration, Failing: []string{}, Cancelled: []string{}}
	for _, run := range fixture.Settlement.CheckRuns {
		want.CheckRuns = append(want.CheckRuns, record.AttemptRun{Name: string(run.Name), ID: run.ID})
	}
	for _, name := range fixture.Settlement.Failed {
		want.Failing = append(want.Failing, string(name))
	}
	for _, name := range fixture.Settlement.Cancelled {
		want.Cancelled = append(want.Cancelled, string(name))
	}

	got, completed, ok := listenerSettlement("acme/widgets", 86, "code", runs, fixture.ReviewerAppID)
	if !ok || !completed.Equal(newest) {
		t.Fatalf("listenerSettlement = ok %t, completed %s; want a settlement, completed at the newest run's %s", ok, completed, newest)
	}
	if !slices.Equal(got.CheckRuns, want.CheckRuns) || !slices.Equal(got.Failing, want.Failing) || !slices.Equal(got.Cancelled, want.Cancelled) ||
		got.Generation != want.Generation || got.HeadSHA != want.HeadSHA || got.Repo != want.Repo || got.Number != want.Number {
		t.Fatalf("the pass's settlement of the fixture's runs\n%s\nwant the listener's\n%s", shortChecks(got), shortChecks(want))
	}
	if got.Snapshot == "" {
		t.Fatal("the pass's settlement has no snapshot")
	}
	// Listed in any order, the same runs are the same settlement, under the same snapshot.
	slices.Reverse(runs)
	if again, _, _ := listenerSettlement("acme/widgets", 86, "code", runs, fixture.ReviewerAppID); again.Snapshot != got.Snapshot || !slices.Equal(again.CheckRuns, got.CheckRuns) {
		t.Fatalf("the runs listed backwards settle as %s, want %s", shortChecks(again), shortChecks(got))
	}
}

// shortChecks is a settlement for a failure message, each long name cut to its last 24 bytes.
func shortChecks(fact intake.PullRequestChecks) string {
	short := func(name string) string {
		if len(name) > 40 {
			return "…" + name[len(name)-24:]
		}
		return name
	}
	var runs, failing, cancelled []string
	for _, run := range fact.CheckRuns {
		runs = append(runs, fmt.Sprintf("%s=%d", short(run.Name), run.ID))
	}
	for _, name := range fact.Failing {
		failing = append(failing, short(name))
	}
	for _, name := range fact.Cancelled {
		cancelled = append(cancelled, short(name))
	}
	return fmt.Sprintf("runs %v failing %v cancelled %v generation %d", runs, failing, cancelled, fact.Generation)
}

// checkRunJSON is one check run as GitHub's check-runs list of a commit answers it, created by the
// App app; a zero completed is a run still going (GitHub's null completed_at and conclusion).
func checkRunJSON(id int64, name, conclusion string, completed time.Time, app string) string {
	status, completedAt, concluded := "in_progress", "null", "null"
	if !completed.IsZero() {
		status, completedAt, concluded = "completed", `"`+completed.UTC().Format(time.RFC3339)+`"`, `"`+conclusion+`"`
	}
	return fmt.Sprintf(`{"id":%d,"name":%q,"status":%q,"conclusion":%s,"started_at":"2026-10-28T08:00:00Z","completed_at":%s,"app":{"id":%s},"pull_requests":[]}`,
		id, name, status, concluded, completedAt, app)
}

// settlementStandIn is GitHub's REST API for acme/widgets as a pass reads it when the settlement
// of pull request 86's head was lost: 86 open on legion/CAPTURE-1 at head code, with no reviews,
// listed open, on base main, whose rulesets require the checks named in requires and which is
// unprotected; the check runs on code are runs, a JSON array of checkRunJSON. Every request is
// counted in reads.
func settlementStandIn(t *testing.T, requires []string, runs []string, reads *pathReads) *httptest.Server {
	t.Helper()
	pull := passPull(86, "legion/CAPTURE-1", "code", "open", time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC), "")
	contexts := make([]string, 0, len(requires))
	for _, name := range requires {
		contexts = append(contexts, `{"context":"`+name+`"}`)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.add(r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls":
			w.Write([]byte("[" + pull + "]"))
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			w.Write([]byte(`[]`))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[` + strings.Join(contexts, ",") + `]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		case "/repos/acme/widgets/commits/code/check-runs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("read the check runs with %q, want every run (filter=all)", r.URL.RawQuery)
			}
			fmt.Fprintf(w, `{"total_count":%d,"check_runs":[%s]}`, len(runs), strings.Join(runs, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// seedLostSettlement records CAPTURE-1 in testing with its tester at work, and its pull request 86
// at head code, whose base requires requires, with no settlement: every check_run webhook of the
// head was lost.
func seedLostSettlement(t *testing.T, pool *pgxpool.Pool, requires ...string) {
	t.Helper()
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	setCapturePullRequest(t, pool, func(pr *record.PullRequest) {
		pr.CheckedHead, pr.CheckRuns, pr.Failing, pr.Generation, pr.Snapshot, pr.Required = "", nil, []string{}, 0, "", requires
	})
}

// lateSettlementChangesOnlyTheFence applies fact, the listener's settlement of the runs the pass
// settled, arriving afterwards, and fails t unless it changed nothing but the pull request's
// generation and snapshot, which become the listener's: no phase, run, outcome or notice.
func lateSettlementChangesOnlyTheFence(t *testing.T, pool *pgxpool.Pool, w *workflowRuntime, fact intake.PullRequestChecks) {
	t.Helper()
	before := recordSnapshot(t, pool)
	_, pr := capturePhase(t, pool)
	if _, err := intake.ApplyFact(context.Background(), pool, "github", "late-settlement", fact, w.handlers...); err != nil {
		t.Fatalf("apply the listener's late settlement: %v", err)
	}
	_, adopted := capturePhase(t, pool)
	if adopted.Generation != fact.Generation || adopted.Snapshot != fact.Snapshot {
		t.Fatalf("after the listener's settlement the fence is %d %q, want its %d %q", adopted.Generation, adopted.Snapshot, fact.Generation, fact.Snapshot)
	}
	if _, err := pool.Exec(context.Background(), "update pull_requests set generation = $1, snapshot = $2 where issue = 'CAPTURE-1'", pr.Generation, pr.Snapshot); err != nil {
		t.Fatalf("put the pass's fence back to compare: %v", err)
	}
	if after := recordSnapshot(t, pool); after != before {
		t.Fatalf("the listener's late settlement changed more than the fence:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := pool.Exec(context.Background(), "update pull_requests set generation = $1, snapshot = $2 where issue = 'CAPTURE-1'", fact.Generation, fact.Snapshot); err != nil {
		t.Fatalf("restore the listener's fence: %v", err)
	}
}

// A head whose every check_run webhook was lost has no settlement and the issue no CI verdict. The
// pass reads the head's check runs, and once every one completed more than an interval ago it
// applies the settlement the listener would have: a red required check sends the issue back from
// testing with the architect told, and a green one leaves it to the tester, whose passing verdict
// then moves it on. A second pass reads the settled head's runs no more and applies nothing again,
// and the listener's own settlement of the same runs arriving afterwards changes nothing but the
// fence.
func TestThePassSettlesAHeadWhoseSettlementWasLost(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		conclusion string
		phase      phase.Phase
		verdict    string
		failing    []string
	}{
		{"a red check sends the issue back from testing", "failure", phase.Implementing, "red", []string{"pr-checks-result"}},
		{"a green check lets the tester's verdict move it", "success", phase.Testing, "green", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedLostSettlement(t, pool, "pr-checks-result")
			runs := []string{
				checkRunJSON(5, "pr-checks-result", "success", long.Add(-time.Hour), "15368"),
				checkRunJSON(7, "pr-checks-result", tc.conclusion, long, "15368"),
				checkRunJSON(8, "lint", "success", long, "15368"),
			}
			var reads pathReads
			w := requiredRuntime(pool, settlementStandIn(t, []string{"pr-checks-result"}, runs, &reads).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.phase || pr.CheckedHead != "code" || pr.Generation != classify.PassGeneration || classify.HeadVerdict(*pr) != tc.verdict || !slices.Equal(pr.Failing, tc.failing) {
				t.Fatalf("after the pass the issue is in %s with %+v (verdict %q), want %s with code settled %s by the pass", got, pr, classify.HeadVerdict(*pr), tc.phase, tc.verdict)
			}
			if want := []record.AttemptRun{{Name: "lint", ID: 8}, {Name: "pr-checks-result", ID: 7}}; !slices.Equal(pr.CheckRuns, want) {
				t.Fatalf("the settlement's runs are %v, want each check's latest %v", pr.CheckRuns, want)
			}
			reasons := noticeReasons(t, pool, "checks-red")
			if tc.verdict == "red" && (len(reasons) != 1 || reasons[0] != "CI is red at code: pr-checks-result") {
				t.Fatalf("checks-red notices %q, want one naming pr-checks-result at code", reasons)
			}
			if tc.verdict == "green" && len(reasons) != 0 {
				t.Fatalf("checks-red notices %q, want none on a green head", reasons)
			}
			w.readRequiredChecks(context.Background())
			if n := reads.counts()["/repos/acme/widgets/commits/code/check-runs"]; n != 1 || headReads(t, pool, "checks-read") != 1 {
				t.Fatalf("two passes read the head's check runs %d times and applied %d settlements, want one of each", n, headReads(t, pool, "checks-read"))
			}
			lateSettlementChangesOnlyTheFence(t, pool, w, intake.PullRequestChecks{Repo: "acme/widgets", Number: 86, HeadSHA: "code",
				CheckRuns: []record.AttemptRun{{Name: "lint", ID: 8}, {Name: "pr-checks-result", ID: 7}}, Generation: 3, Snapshot: "listener-3",
				Failing: tc.failing, Cancelled: []string{}})
			if tc.verdict != "green" {
				return
			}
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "tester-pass", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleTester,
				Claim: "test-claim", Summary: "tested", Verdict: "pass", HandoffRecord: "test-record", HandoffVersion: 1}, w.handlers...); err != nil {
				t.Fatalf("apply the tester's pass: %v", err)
			}
			if got, _ := capturePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the tester's pass the issue is in %s, want reviewing", got)
			}
		})
	}
}

// The listener settles a head only once every check on it is done and the head has been quiet, so
// the pass waits as long: a run still going, or a newest run that completed less than an interval
// ago, applies nothing, and the issue keeps no verdict until a later pass or the listener settles it.
func TestThePassAppliesNoSettlementWhileAChecksRunIsGoingOrJustCompleted(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		runs []string
	}{
		{"a run still going", []string{checkRunJSON(7, "pr-checks-result", "failure", long, "15368"), checkRunJSON(8, "lint", "", time.Time{}, "15368")}},
		{"a run completed less than an interval ago", []string{checkRunJSON(7, "pr-checks-result", "failure", long, "15368"),
			checkRunJSON(8, "lint", "success", time.Now().Add(-requiredChecksInterval/4), "15368")}},
		{"no run at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedLostSettlement(t, pool, "pr-checks-result")
			var reads pathReads
			w := requiredRuntime(pool, settlementStandIn(t, []string{"pr-checks-result"}, tc.runs, &reads).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != phase.Testing || pr.CheckedHead != "" || pr.CheckRuns != nil || headReads(t, pool, "checks-read") != 0 {
				t.Fatalf("after the pass the issue is in %s with %+v, want testing with no settlement", got, pr)
			}
			if n := reads.counts()["/repos/acme/widgets/commits/code/check-runs"]; n != 1 {
				t.Fatalf("the pass read the head's check runs %d times, want once", n)
			}
		})
	}
}

// A run named tester or architect is a Legion role's verdict only when the review App created it,
// as the listener counts it: the same names from any other App are left out of the pass's
// settlement, so another App's failing tester run on a base that requires tester makes nothing red,
// and the review App's own passing one stands.
func TestThePassDropsTesterAndArchitectChecksRunsFromAnotherApp(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	pool := isolatedOutboxPool(t)
	seedLostSettlement(t, pool, "pr-checks-result", "tester")
	runs := []string{
		checkRunJSON(6, "tester", "success", long, testReviewAppID),
		checkRunJSON(7, "pr-checks-result", "success", long, "15368"),
		checkRunJSON(8, "tester", "failure", long, "98765"),
		checkRunJSON(9, "architect", "failure", long, "98765"),
	}
	w := requiredRuntime(pool, settlementStandIn(t, []string{"pr-checks-result", "tester"}, runs, &pathReads{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	got, pr := capturePhase(t, pool)
	if want := []record.AttemptRun{{Name: "pr-checks-result", ID: 7}, {Name: "tester", ID: 6}}; got != phase.Testing || !slices.Equal(pr.CheckRuns, want) ||
		len(pr.Failing) != 0 || classify.HeadVerdict(*pr) != "green" {
		t.Fatalf("after the pass the issue is in %s with runs %v failing %v (verdict %q), want testing, green on %v", got, pr.CheckRuns, pr.Failing, classify.HeadVerdict(*pr), want)
	}
}
