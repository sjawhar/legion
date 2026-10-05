package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// rulesetsStandIn is GitHub's REST API for acme/widgets as the required-checks read sees it: pull
// request 86 on base main, whose rulesets require pr-checks-result and whose branch protection
// requires nothing. While failing is set, the rulesets read answers 502.
func rulesetsStandIn(t *testing.T, failing *atomic.Bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(`{"number":86,"base":{"ref":"main"}}`))
		case "/repos/acme/widgets/rules/branches/main":
			if failing.Load() {
				http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
				return
			}
			w.Write([]byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// mergeableStandIn is GitHub's REST API for acme/widgets as the required-checks read sees it: pull
// request 86 on base main, whose GitHub `mergeable` field is the literal JSON state, whose
// rulesets require pr-checks-result and whose branch protection requires nothing.
func mergeableStandIn(t *testing.T, state string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(`{"number":86,"base":{"ref":"main"},"mergeable":` + state + `}`))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// seedStuckRound records the round a live tree got stuck in before the daemon read required sets:
// the reviewer approved its own handoff head and completed, and the code head's settlement that
// stands for it is red only on two workflow_dispatch lanes and an advisory review check, beside a
// required gate that passed. No required set is recorded.
func seedStuckRound(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets",
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "handoff", CheckedHead: "code",
			Failing:    []string{"dev-apply / dev-chain-tripwire", "dev-apply / staging-e2e / staging-e2e", "review"},
			CheckRuns:  []record.AttemptRun{{Name: "dev-apply / dev-chain-tripwire", ID: 2}, {Name: "pr-checks-result", ID: 1}, {Name: "review", ID: 3}},
			Generation: 1, Snapshot: "settled", Pushes: []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim"}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "handoff",
			Summary: "approved", Decision: &record.ReviewDecision{State: "approved", Body: "looks right", Head: "handoff"}})
	}); err != nil {
		t.Fatalf("seed the stuck round: %v", err)
	}
}

// requiredRuntime is a workflow runtime over pool whose required-checks read goes to api, logging
// to log, for a project that declares reviewWorkflows.
func requiredRuntime(pool *pgxpool.Pool, api string, log *slog.Logger, reviewWorkflows ...string) *workflowRuntime {
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", ReviewWorkflows: reviewWorkflows}, log)
	return &workflowRuntime{
		pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"}, handlers: []intake.Handler{engine},
		tokens: outboxTokens{}, dispatchProject: "CAPTURE", bootID: "test-boot", log: log, githubAPI: api,
	}
}

func capturePhase(t *testing.T, pool *pgxpool.Pool) (phase.Phase, *record.PullRequest) {
	t.Helper()
	var issue *record.Issue
	var pr *record.PullRequest
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = record.NewStore().Issue(context.Background(), tx, "CAPTURE-1"); err != nil {
			return err
		}
		pr, err = record.NewStore().PullRequest(context.Background(), tx, "CAPTURE-1")
		return err
	}); err != nil {
		t.Fatalf("read CAPTURE-1: %v", err)
	}
	return issue.Phase, pr
}

// The daemon's first read of required sets, at boot, ends a round a red the base branch never
// required left stuck, with no new push: it reads the pull request's base, then the base's rulesets
// and protection, with the implement App's token, records the set, and the approved round moves on.
func TestTheBootReadOfRequiredChecksEndsARoundStuckOnRedsTheBaseDoesNotRequire(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedStuckRound(t, pool)
	w := requiredRuntime(pool, rulesetsStandIn(t, &atomic.Bool{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	got, pr := capturePhase(t, pool)
	if got != phase.Retro {
		t.Fatalf("after the boot read the issue is in %s, want retro", got)
	}
	if strings.Join(pr.Required, ",") != "pr-checks-result" {
		t.Fatalf("the recorded required set = %#v, want the rulesets' pr-checks-result", pr.Required)
	}
}

// A read GitHub fails counts as neither green nor red: the pull request keeps no required set, the
// round stays where it was, and the failure is logged with the repository and GitHub's HTTP
// status. The next pass that reads the set decides the round.
func TestAnUnreadableRulesetLeavesTheRoundUndecided(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedStuckRound(t, pool)
	var logged bytes.Buffer
	failing := &atomic.Bool{}
	failing.Store(true)
	w := requiredRuntime(pool, rulesetsStandIn(t, failing).URL, slog.New(slog.NewJSONHandler(&logged, nil)))
	w.readRequiredChecks(context.Background())
	got, pr := capturePhase(t, pool)
	if got != phase.Reviewing || pr.Required != nil {
		t.Fatalf("after a failed read the issue is in %s with required set %#v, want reviewing with none", got, pr.Required)
	}
	var notices int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'notice'").Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 0 {
		t.Fatalf("a failed read wrote %d notices, want none", notices)
	}
	if line := logged.String(); !strings.Contains(line, `"repo":"acme/widgets"`) || !strings.Contains(line, `"status":502`) || !strings.Contains(line, `"pullRequest":86`) {
		t.Fatalf("log = %s, want the failure with the repository, the pull request and HTTP status 502", line)
	}
	failing.Store(false)
	w.readRequiredChecks(context.Background())
	if got, _ := capturePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the next pass read the set the issue is in %s, want retro", got)
	}
}

// A rate-limit answer ends the pass: every further read on the installation meets the same limit,
// and the panes share it, so the pass reads no other pull request until the next one. Any other
// refusal skips only the pull request it answered. The stand-in refuses every read; two open pull
// requests are recorded.
func TestARateLimitAnswerEndsTheRequiredChecksPass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		reads   int64
	}{
		{"429", http.StatusTooManyRequests, nil, "", 1},
		{"403 out of rate limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "", 1},
		{"403 with retry-after", http.StatusForbidden, map[string]string{"Retry-After": "60"}, "", 1},
		{"403 for a secondary rate limit named only by its message", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4321"},
			`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, 1},
		{"403 that is not a rate limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4999"}, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedStuckRound(t, pool)
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				records := record.NewStore()
				if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "CAPTURE-2", Tree: "CAPTURE-2", Project: "CAPTURE", Title: "second",
					Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V"}); err != nil {
					return err
				}
				return records.PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-2",
					Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-2", HeadSHA: "second"})
			}); err != nil {
				t.Fatalf("seed a second open pull request: %v", err)
			}
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				for name, value := range tc.headers {
					w.Header().Set(name, value)
				}
				body := tc.body
				if body == "" {
					body = `{"message":"refused"}`
				}
				http.Error(w, body, tc.status)
			}))
			defer server.Close()
			requiredRuntime(pool, server.URL, quietLogger()).readRequiredChecks(context.Background())
			if got := reads.Load(); got != tc.reads {
				t.Fatalf("the pass read GitHub %d times, want %d", got, tc.reads)
			}
		})
	}
}

// requiredWorkflowStandIn is GitHub's REST API for acme/widgets with a live repository's rule
// shapes: pull request 86 on base main, whose rulesets require the workflow
// .github/workflows/claude-pr-review.yml (a workflows rule) and the check pr-checks-result (a
// required_status_checks rule), and whose branch protection requires nothing. The head code's
// workflow runs are what runs answers when asked, a JSON array.
func requiredWorkflowStandIn(t *testing.T, runs func() string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(`{"number":86,"base":{"ref":"main"}}`))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"workflows","parameters":{"do_not_enforce_on_create":true,"workflows":[{"repository_id":4242,"path":".github/workflows/claude-pr-review.yml","ref":"refs/heads/main"}]}},` +
				`{"type":"deletion","parameters":null},{"type":"pull_request","parameters":{"required_approving_review_count":0}},` +
				`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"pr-checks-result","integration_id":15368}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		case "/repos/acme/widgets/actions/runs":
			if r.URL.Query().Get("head_sha") != "code" {
				http.NotFound(w, r)
				return
			}
			answer := runs()
			w.Write([]byte(`{"total_count":` + strconv.Itoa(strings.Count(answer, `"path"`)) + `,"workflow_runs":` + answer + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// reviewRuns is the head's workflow runs: the required review workflow's run 37 at attempt, with
// status and conclusion, beside a passing run of a workflow the base does not require.
func reviewRuns(attempt int, status, conclusion string) string {
	return `[{"id":37,"run_attempt":` + strconv.Itoa(attempt) + `,"name":"Review PR #86","path":".github/workflows/claude-pr-review.yml","event":"pull_request","status":"` + status +
		`","conclusion":` + conclusion + `,"repository":{"id":4242}},` +
		`{"id":38,"run_attempt":1,"name":"PR Checks","path":".github/workflows/pr-checks.yml","event":"pull_request","status":"completed","conclusion":"success","repository":{"id":4242}}]`
}

// seedCapture records CAPTURE-1 in from with its pull request 86 at head code, whose required check
// passed there, and its phases.
func seedCapture(t *testing.T, pool *pgxpool.Pool, from phase.Phase, status string, phases ...record.PhaseRow) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: from, Generation: 1, Status: status, Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets",
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code", CheckedHead: "code", Failing: []string{},
			CheckRuns:  []record.AttemptRun{{Name: "pr-checks-result", ID: 1}, {Name: "review", ID: 2}},
			Generation: 1, Snapshot: "settled", Required: []string{"pr-checks-result"}}); err != nil {
			return err
		}
		for _, row := range append([]record.PhaseRow{{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim"}}, phases...) {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
}

// noticeReasons is the reason of every notice of kind in the outbox.
func noticeReasons(t *testing.T, pool *pgxpool.Pool, kind string) []string {
	t.Helper()
	var reasons []string
	rows, err := pool.Query(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = $1", kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reasons
}

// The daemon's pass judges a ruleset's required workflow by its latest run on the head, as it
// judges a required check by the settlement: a failed run is CI red at the head. In testing, where
// the required checks passed, a red only a review workflow the project declares makes is the
// reviewer's round's to decide, so the tester goes on; the same workflow undeclared is a failing
// check, and sends the work back to implementing. In awaiting_merge, where every worker is
// suspended, a failed run sends the work back to implementing whether declared or not, so a READY
// head that turns red wakes the implementer instead of stranding the tree. A run still going, or one
// that succeeded, moves nothing. The pull request's required checks were read before and passed at
// the head, so only the workflow can decide anything.
func TestAFailedRequiredWorkflowSendsTheWorkBackUnlessTheReviewRoundDecidesIt(t *testing.T) {
	const declared = ".github/workflows/claude-pr-review.yml"
	for _, tc := range []struct {
		name     string
		from     phase.Phase
		status   string
		runs     string
		declared []string
		want     phase.Phase
	}{
		{"failed, declared, in testing", phase.Testing, "testing", reviewRuns(1, "completed", `"failure"`), []string{declared}, phase.Testing},
		{"failed, undeclared, in testing", phase.Testing, "testing", reviewRuns(1, "completed", `"failure"`), nil, phase.Implementing},
		{"failed, declared, in awaiting_merge", phase.AwaitingMerge, "retro", reviewRuns(1, "completed", `"failure"`), []string{declared}, phase.Implementing},
		{"still running, in awaiting_merge", phase.AwaitingMerge, "retro", reviewRuns(1, "in_progress", "null"), []string{declared}, phase.AwaitingMerge},
		{"succeeded, in awaiting_merge", phase.AwaitingMerge, "retro", reviewRuns(1, "completed", `"success"`), []string{declared}, phase.AwaitingMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, tc.from, tc.status)
			requiredRuntime(pool, requiredWorkflowStandIn(t, func() string { return tc.runs }).URL, quietLogger(), tc.declared...).readRequiredChecks(context.Background())
			if got, _ := capturePhase(t, pool); got != tc.want {
				t.Fatalf("after the pass the issue is in %s, want %s", got, tc.want)
			}
			reasons := noticeReasons(t, pool, "checks-red")
			if want := "CI is red at code: .github/workflows/claude-pr-review.yml"; tc.want == phase.Implementing && (len(reasons) != 1 || reasons[0] != want) {
				t.Fatalf("checks-red notices %q, want one saying %q", reasons, want)
			}
			if tc.want != phase.Implementing && len(reasons) != 0 {
				t.Fatalf("checks-red notices %q, want none", reasons)
			}
		})
	}
}

// A re-run of a declared review workflow keeps its run's id and raises its attempt, so the pass
// reads a re-run that ended red as a change even when no pass read it running: the approved round
// stuck on the first red is told again, naming the re-run's red, and the architect sees the re-run
// that stayed red. A pass that finds the same attempt again tells nothing more.
func TestARerunThatEndsRedBetweenTwoPassesIsToldAgain(t *testing.T) {
	const declared = ".github/workflows/claude-pr-review.yml"
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim",
		HandoffCommit: "code", Summary: "approved", Decision: &record.ReviewDecision{State: "approved", Body: "looks right", Head: "code"}})
	attempt := 1
	w := requiredRuntime(pool, requiredWorkflowStandIn(t, func() string { return reviewRuns(attempt, "completed", `"failure"`) }).URL, quietLogger(), declared)
	for _, pass := range []struct {
		attempt, told int
	}{{1, 1}, {2, 2}, {2, 2}} {
		attempt = pass.attempt
		w.readRequiredChecks(context.Background())
		stuck := noticeReasons(t, pool, "review-stuck")
		if len(stuck) != pass.told || !strings.Contains(stuck[len(stuck)-1], "CI is red at code: "+declared+"; only declared review workflows are red") {
			t.Fatalf("after the pass that read attempt %d, review-stuck notices %q, want %d naming the review workflow", pass.attempt, stuck, pass.told)
		}
	}
	if got, _ := capturePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("the issue is in %s, want reviewing", got)
	}
}

// The required-checks pass's one read of a pull request already carries GitHub's `mergeable`
// field, so a head that starts conflicting with its base is caught without a further GitHub call:
// in awaiting_merge the daemon sends the tree back to implementing exactly as a red CI verdict
// does, naming the base to merge forward, since GitHub computes no merge ref for a conflicting
// head and runs no checks on it at all. A read that finds `mergeable` still null - GitHub has not
// computed it yet - or true moves nothing.
func TestTheRequiredChecksPassCatchesAHeadThatStartsConflictingWithItsBase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  phase.Phase
	}{
		{"conflicting", "false", phase.Implementing},
		{"not yet computed", "null", phase.AwaitingMerge},
		{"mergeable", "true", phase.AwaitingMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.AwaitingMerge, "retro",
				record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code", Summary: "READY #86 at code"})
			w := requiredRuntime(pool, mergeableStandIn(t, tc.state).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.want {
				t.Fatalf("after the pass the issue is in %s, want %s", got, tc.want)
			}
			if pr.Base != "main" {
				t.Fatalf("recorded base = %q, want main", pr.Base)
			}
			if tc.want != phase.Implementing {
				return
			}
			want := "the head conflicts with main: GitHub runs no checks on it; merge main forward"
			reasons := noticeReasons(t, pool, "checks-red")
			if len(reasons) != 1 || !strings.Contains(reasons[0], want) {
				t.Fatalf("checks-red notices %q, want one saying %q", reasons, want)
			}
		})
	}
}
