package workflow

// A worker that lost its session, and a worker no start can reach because the session store
// cannot be asked, are both reported to the people who can act on them: dispatch://INFRA-714 R8.

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

func seedTestingIssue(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
}

// controllerNoticeKinds is the kind of every notice the outbox holds for the project's controller.
func controllerNoticeKinds(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return outboxStrings(t, pool, "select payload->>'kind' from outbox where kind = 'controller_notice' and issue = $1 order by id", "LEGION-209")
}

// A lost session is posted on the issue and told to its architect and to the controller: the
// worker is running, so nothing is held, and the report is the whole point.
func TestALostSessionIsToldOnTheIssueToItsArchitectAndToTheController(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")

	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "session-lost:legion-legion-209-tester:3", intake.SessionLost{
		Issue: "LEGION-209", Role: claim.RoleTester,
		Detail: "the session ses-abc this claim recorded is in no session store the runtime could resume it from",
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the lost session: %v", err)
	}

	posted := messageBodies(t, pool)
	if len(posted) != 1 || !strings.Contains(posted[0], "ses-abc") {
		t.Fatalf("the issue was told %v, want one message naming the lost session", posted)
	}
	if reason, told := noticeKindReasons(t, pool, "LEGION-209")["session-lost"]; !told || !strings.Contains(reason, "fresh agent") {
		t.Fatalf("the architect was told %q, want a session-lost notice saying a fresh agent runs", reason)
	}
	if got := controllerNoticeKinds(t, pool); len(got) != 1 || got[0] != "session-lost" {
		t.Fatalf("the controller was told %v, want the lost session", got)
	}
}

// A session store that cannot be asked is told the same way, and holds no phase: the claim keeps
// its session and the daemon keeps trying.
func TestASessionStoreFaultIsToldOnTheIssueAndToTheController(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")

	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "session-store-fault:legion-legion-209-tester:1", intake.SessionStoreFault{
		Issue: "LEGION-209", Role: claim.RoleTester, Detail: "dial tcp 10.0.0.3:5432: connection refused",
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the store fault: %v", err)
	}

	posted := messageBodies(t, pool)
	if len(posted) != 1 || !strings.Contains(posted[0], "connection refused") || !strings.Contains(posted[0], "every minute") {
		t.Fatalf("the issue was told %v, want one message naming the fault and the retry", posted)
	}
	if got := controllerNoticeKinds(t, pool); len(got) != 1 || got[0] != "session-store-fault" {
		t.Fatalf("the controller was told %v, want the store fault", got)
	}
	var got phase.Phase
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = 'LEGION-209'").Scan(&got); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if got != phase.Testing {
		t.Fatalf("the issue stands in %s, want testing: a store fault holds no phase", got)
	}
}
