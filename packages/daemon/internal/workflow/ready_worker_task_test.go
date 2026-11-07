package workflow

// A worker that holds its issue's phase always has its task: dispatch://INFRA-714 R7. After a
// restart — the process relaunched, the claim retried, the session resumed — the start row that
// moved the issue into this phase ran long ago, and nothing else sends the worker anything. 48
// waits totalling 301 hours followed a restart after which the daemon sent nothing.

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedPhaseWorker is a claim of role on issue in the state the ready fact finds it: ready, with or
// without a task pending, as a worker whose process has just come back up.
func seedPhaseWorker(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role, pending string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $4, $5, 2, 'ses', '/sessions/ses.jsonl', 'ready', 0, 0, 0, 0)`,
		string(token), project, issue, issue, string(role)); err != nil {
		t.Fatalf("seed the %s %s: %v", issue, role, err)
	}
	if pending != "" {
		if _, err := pool.Exec(t.Context(), `insert into pending_task_deliveries (claim_token, delivery_id, task, phase,
			generation, queued_at) values ($1, 'd-1', $2, 'testing', 1, now())`, string(token), pending); err != nil {
			t.Fatalf("seed the %s %s's task: %v", issue, role, err)
		}
	}
	return token
}

// A tester whose claim becomes ready while its issue stands in testing with nothing pending is
// queued its phase's resume task; one that already holds a task is left alone, since a second task
// would be a second prompt for work under way.
func TestAReadyWorkerHoldingItsPhaseWithNothingPendingIsGivenItsPhaseTask(t *testing.T) {
	for name, pending := range map[string]string{
		"with nothing pending": "",
		"holding its task":     "test the branch",
	} {
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "LEGION-209", Project: "LEGION", Title: "a child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
			seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0})
			seedPhaseWorker(t, pool, "LEGION-209", claim.RoleTester, pending)

			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:tester:2:ready",
				intake.ClaimReady{Issue: "LEGION-209", Role: claim.RoleTester, Launch: 2},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the ready: %v", err)
			}

			want := map[string]int{"LEGION-209/tester": 1}
			if pending != "" {
				want = map[string]int{}
			}
			if got := superviseRequests(t, pool, "start"); !sameCounts(got, want) {
				t.Fatalf("the ready queued starts %v, want %v", got, want)
			}
		})
	}
}

// A role that does not hold the issue's phase is given nothing: its phase is over, and the phase
// that is running belongs to another claim.
func TestAReadyWorkerThatDoesNotHoldTheIssuesPhaseIsGivenNothing(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-209", Project: "LEGION", Title: "a child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0})
	seedPhaseWorker(t, pool, "LEGION-209", claim.RoleImplementer, "")

	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:implementer:2:ready",
		intake.ClaimReady{Issue: "LEGION-209", Role: claim.RoleImplementer, Launch: 2},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the ready: %v", err)
	}

	if got := superviseRequests(t, pool, "start"); !sameCounts(got, map[string]int{}) {
		t.Fatalf("the ready queued starts %v, want none: the implementer works no phase of this issue", got)
	}
}

// A failed claim's notices name both of the causes the claim recorded — why its process last died
// and why its last start failed — since the architect reading them has no other way to learn
// either (dispatch://INFRA-714 R6).
func TestAFailedClaimsNoticesNameTheDeathAndTheFailedStart(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "a root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})

	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:tester:3:failed", intake.ClaimFailed{
		Issue: "LEGION-208", Role: claim.RoleTester, Reason: "deaths with work outstanding ran out",
		Death:        "gone: pod legion-legion-208 (uid 8a1f) ended: OOMKilled",
		StartFailure: "launch legion-LEGION-208-tester: its init container cloned no workspace",
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the failure: %v", err)
	}

	reasons := noticeKindReasons(t, pool, "LEGION-208")
	for _, kind := range []string{"held", "worker-died"} {
		reason, told := reasons[kind]
		if !told {
			t.Fatalf("no %s notice was written; notices: %v", kind, reasons)
		}
		for _, want := range []string{"OOMKilled", "its init container cloned no workspace", "deaths with work outstanding ran out"} {
			if !strings.Contains(reason, want) {
				t.Errorf("the %s notice reads %q, want it to name %q", kind, reason, want)
			}
		}
	}
}

// noticeKindReasons is the reason of each kind of notice the outbox holds for issue.
func noticeKindReasons(t *testing.T, pool *pgxpool.Pool, issue string) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'kind', coalesce(payload->>'reason', '') from outbox where kind = 'notice' and issue = $1", issue)
	if err != nil {
		t.Fatalf("read the notices of %s: %v", issue, err)
	}
	defer rows.Close()
	reasons := map[string]string{}
	for rows.Next() {
		var kind, reason string
		if err := rows.Scan(&kind, &reason); err != nil {
			t.Fatalf("scan a notice of %s: %v", issue, err)
		}
		reasons[kind] = reason
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the notices of %s: %v", issue, err)
	}
	return reasons
}
