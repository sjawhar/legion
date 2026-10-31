package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pullStandIn is GitHub's REST API for acme/widgets as the per-issue read sees it: pull request 86
// answered with pull, and no review on it.
func pullStandIn(t *testing.T, pull string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// mergeableStandIn answers pull request 86 on base main with state as the literal JSON of GitHub's
// `mergeable` field, and no merge state at all.
func mergeableStandIn(t *testing.T, state string) *httptest.Server {
	t.Helper()
	return pullStandIn(t, `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":`+state+`}`)
}

// mergeStateStandIn answers pull request 86 on base main with state as GitHub's `mergeable_state`,
// beside the `mergeable: true` GitHub computes with it: GitHub answers true for every state its
// own rules leave rather than a conflict, a head it blocks on a required check included, which is
// what makes the merge state the only answer that tells those apart.
func mergeStateStandIn(t *testing.T, state string) *httptest.Server {
	t.Helper()
	return pullStandIn(t, `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"`+state+`"}`)
}

// readRuntime is a workflow runtime over pool whose GitHub reads go to api, logging to log. Its
// engine stands in for the whole workflow and is given the runtime's own merge-state queue, so a
// fact a test applies asks for the read it asks for in production; the queue is empty and
// nothing reads it until a test starts readMergeStates.
func readRuntime(pool *pgxpool.Pool, api string, log *slog.Logger) *workflowRuntime {
	w := &workflowRuntime{
		pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"},
		tokens: outboxTokens{}, dispatchProject: "CAPTURE", bootID: "test-boot", log: log, githubAPI: api,
		mergeStates: newWakeSet[string](), mergeStateRunning: map[string]bool{},
	}
	w.engine = workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, ReadMergeState: w.queueMergeState}, log)
	w.handlers = []intake.Handler{w.engine}
	return w
}

// servedAcme is the repository mapping of a project that serves acme/widgets alone, the one a
// read's own spelling of a repository is resolved against.
func servedAcme() *repositories {
	served := newRepositories("CAPTURE")
	served.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	return served
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

// readCapture runs the per-issue read of CAPTURE-1's pull request, as the daemon's start and each
// handoff completion do, failing the test on an error.
func readCapture(t *testing.T, w *workflowRuntime) {
	t.Helper()
	if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
		t.Fatalf("read CAPTURE-1's pull request: %v", err)
	}
}

// seedCapture records CAPTURE-1 in from with its pull request 86 at head code, whose required
// check passed there and which GitHub's last read found clean, and its phases. A test about what
// an unread or another merge state leaves undone sets its own (clearCaptureMergeState).
func seedCapture(t *testing.T, pool *pgxpool.Pool, from phase.Phase, status string, phases ...record.PhaseRow) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: from, Generation: 1, Status: status, Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets",
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code", CheckedHead: "code", Failing: []string{},
			CheckRuns:  []record.AttemptRun{{Name: "pr-checks-result", ID: 1}, {Name: "review", ID: 2}},
			Generation: 1, Snapshot: "settled", Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
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

// GitHub sends no event when it computes a pull request's mergeability, so a head that starts
// conflicting with its base - someone merged another pull request into it - is learned only from
// the daemon's own read of /pulls/{number}. A conflicting read sends the tree back to implementing
// from every phase after the implementer's: GitHub computes no merge
// ref for a conflicting head and runs no checks on it at all. A read that finds `mergeable` still
// null - GitHub has not computed it yet - or true moves nothing. The read must apply the fact and
// decide even when it only confirms what is already stored: a conflict can be recorded while the
// issue is still in testing or reviewing, and the first
// read after the issue reaches merging has to catch it then, with no change in the GitHub
// answer to key off.
func TestThePerIssueReadCatchesAHeadThatStartsConflictingWithItsBase(t *testing.T) {
	for _, tc := range []struct {
		name         string
		state        string
		stored       record.Mergeability
		mergeability record.Mergeability
		want         phase.Phase
	}{
		{"conflicting", "false", "", record.MergeabilityConflicting, phase.Implementing},
		{"not yet computed", "null", "", record.MergeabilityUnknown, phase.Merging},
		{"mergeable", "true", "", record.MergeabilityMergeable, phase.Merging},
		{"conflict already recorded", "false", record.MergeabilityConflicting, record.MergeabilityConflicting, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Merging, "retro",
				record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code", Summary: "READY #86 at code"})
			if tc.stored != "" {
				if _, err := pool.Exec(context.Background(), "update pull_requests set mergeability = $1 where issue = 'CAPTURE-1'", string(tc.stored)); err != nil {
					t.Fatalf("seed the stored mergeability: %v", err)
				}
			}
			w := readRuntime(pool, mergeableStandIn(t, tc.state).URL, quietLogger())
			if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request: %v", err)
			}
			got, pr := capturePhase(t, pool)
			if got != tc.want {
				t.Fatalf("after the read the issue is in %s, want %s", got, tc.want)
			}
			if pr.Mergeability != tc.mergeability {
				t.Fatalf("recorded mergeability = %q, want %q", pr.Mergeability, tc.mergeability)
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

// The same one read carries GitHub's merge state for the head, which is the answer the spec's
// mergeable is written in: clean, unstable or has_hooks, and never blocked. The daemon records
// GitHub's own spelling, since the tri-state mergeability it decodes from the same response
// cannot tell the two apart - a head GitHub holds on a check or a review the repository requires
// answers `mergeable: true` exactly as a clean one does - and recording it moves no issue: what
// the merge state decides is a later change's.
func TestThePerIssueReadRecordsGitHubsMergeStateForTheHead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		want      record.MergeState
		mergeable bool
	}{
		{"blocked on what the repository requires", "blocked", record.MergeStateBlocked, false},
		{"clean", "clean", record.MergeStateClean, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Merging, "retro",
				record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code", Summary: "READY #86 at code"})
			w := readRuntime(pool, mergeStateStandIn(t, tc.state).URL, quietLogger())
			if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request: %v", err)
			}
			got, pr := capturePhase(t, pool)
			if pr.MergeState != tc.want {
				t.Fatalf("recorded merge state = %q, want %q", pr.MergeState, tc.want)
			}
			if pr.MergeState.Mergeable() != tc.mergeable {
				t.Fatalf("%q reads as mergeable = %t, want %t", pr.MergeState, pr.MergeState.Mergeable(), tc.mergeable)
			}
			if got != phase.Merging {
				t.Fatalf("after the read the issue is in %s, want merging: recording the merge state moves nothing", got)
			}
		})
	}
}
