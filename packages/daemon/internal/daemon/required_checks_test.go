package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "handoff", CheckedHead: "code", Verdict: "red",
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
// to log.
func requiredRuntime(pool *pgxpool.Pool, api string, log *slog.Logger) *workflowRuntime {
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
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
