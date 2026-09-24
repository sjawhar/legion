package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

type admissionStub struct{}

func (admissionStub) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	return intake.Result{}, nil
}

func TestGateRegistrationWithDesignGateOffStartsPlanningAndSeedsApprovalRead(t *testing.T) {
	pool := migratedPool(t)
	store := record.NewStore()
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{
		Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "Workflow table", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "U",
	})

	engine := New(store, Config{
		Project:        "LEGION",
		DesignGate:     config.DesignGateOff,
		ReviewRoundCap: 3,
		MaxFixAttempts: 3,
		LingerHours:    time.Hour,
		Clock:          func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, nil)
	if _, err := intake.ApplyFact(ctx, pool, "api", "gate-registration", intake.GateRegistered{
		Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 4,
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}

	var gotPhase string
	if err := pool.QueryRow(ctx, "select phase from issues where key = $1", "LEGION-208").Scan(&gotPhase); err != nil {
		t.Fatalf("read issue phase: %v", err)
	}
	if gotPhase != string(phase.Planning) {
		t.Fatalf("issue phase = %q, want %q", gotPhase, phase.Planning)
	}

	var latest int
	var approved *int
	if err := pool.QueryRow(ctx, "select latest_version, approved_version from design_gates where issue = $1", "LEGION-208").Scan(&latest, &approved); err != nil {
		t.Fatalf("read gate: %v", err)
	}
	if latest != 4 || approved == nil || *approved != 4 {
		t.Fatalf("gate = latest %d approved %v, want version 4 open", latest, approved)
	}

	rows, err := pool.Query(ctx, "select kind, payload from outbox order by id")
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		var payload []byte
		if err := rows.Scan(&kind, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	if want := []string{"gate_seed", "notice", "supervise", "notice"}; !sameStrings(kinds, want) {
		t.Fatalf("outbox kinds = %v, want %v", kinds, want)
	}
}

func TestRegisteredOpenGateRecordsChildAndStartsPlanningInTheSameFact(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})

	engine := testEngine()
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-todo", intake.DispatchIssue{
		Key: "LEGION-209", Seq: 1, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V",
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}

	var tree, gotPhase string
	if err := pool.QueryRow(ctx, "select tree, phase from issues where key = $1", "LEGION-209").Scan(&tree, &gotPhase); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if tree != "LEGION-208" || gotPhase != string(phase.Planning) {
		t.Fatalf("child tree/phase = %q/%q, want LEGION-208/planning", tree, gotPhase)
	}
	assertOutboxKinds(t, pool, []string{"supervise", "notice"})
}

func TestRefusedReadyIsCommittedAndApprovalAdvancesWithoutSecondReady(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := testEngine()

	result, err := intake.ApplyFact(ctx, pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Ready: true}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("ApplyFact READY: %v", err)
	}
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Message != "READY refused: approve design version 4 before requesting READY." {
		t.Fatalf("READY refusal = %#v, want committed version-4 refusal", result.Refusal)
	}
	var pending int
	if err := pool.QueryRow(ctx, "select ready_pending_version from issues where key = $1", "LEGION-208").Scan(&pending); err != nil {
		t.Fatalf("read committed ready pending version: %v", err)
	}
	if pending != 4 {
		t.Fatalf("ReadyPendingVersion = %d, want 4", pending)
	}

	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact approval: %v", err)
	}
	var gotPhase string
	var readyPending *int
	if err := pool.QueryRow(ctx, "select phase, ready_pending_version from issues where key = $1", "LEGION-208").Scan(&gotPhase, &readyPending); err != nil {
		t.Fatalf("read advanced issue: %v", err)
	}
	if gotPhase != string(phase.AwaitingMerge) || readyPending != nil {
		t.Fatalf("advanced issue = %q pending %v, want awaiting_merge with no pending version", gotPhase, readyPending)
	}
}

// A READY refused at one version stands: the human may revise the spec again before approving,
// and the approval that reopens the gate at the later version advances the merge. A retried READY
// from the same merger phase is one fact, so nothing else could.
func TestRefusedReadyAdvancesWhenTheGateReopensAtALaterVersion(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := testEngine()

	if _, err := intake.ApplyFact(ctx, pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Ready: true}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact READY: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "version-5", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 5}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact version 5: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "approval-5", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 5}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact approval 5: %v", err)
	}
	var gotPhase string
	var readyPending *int
	if err := pool.QueryRow(ctx, "select phase, ready_pending_version from issues where key = $1", "LEGION-208").Scan(&gotPhase, &readyPending); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if gotPhase != string(phase.AwaitingMerge) || readyPending != nil {
		t.Fatalf("issue = %q pending %v, want awaiting_merge with no pending version", gotPhase, readyPending)
	}
}

func TestImplementationReachesTestingWhenHandoffAndPullRequestArriveInEitherOrder(t *testing.T) {
	for _, handoffFirst := range []bool{true, false} {
		name := "pull-request-first"
		if handoffFirst {
			name = "handoff-first"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine()
			handoff := func() {
				if _, err := intake.ApplyFact(ctx, pool, "api", "handoff", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "abc123", Verdict: "pass"}, engine, admissionStub{}); err != nil {
					t.Fatalf("ApplyFact handoff: %v", err)
				}
			}
			pullRequest := func() {
				if _, err := intake.ApplyFact(ctx, pool, "github", "opened", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"}, engine, admissionStub{}); err != nil {
					t.Fatalf("ApplyFact pull request: %v", err)
				}
			}
			if handoffFirst {
				handoff()
				pullRequest()
			} else {
				pullRequest()
				handoff()
			}
			var gotPhase string
			if err := pool.QueryRow(ctx, "select phase from issues where key = $1", "LEGION-208").Scan(&gotPhase); err != nil {
				t.Fatalf("read issue: %v", err)
			}
			if gotPhase != string(phase.Testing) {
				t.Fatalf("issue phase = %q, want testing", gotPhase)
			}
			assertOutboxKinds(t, pool, []string{"dispatch_status", "supervise", "supervise", "notice"})
		})
	}
}

func TestCapturedArtifactVersionAndApprovalFlowThroughConsume(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "CAPTURE-4", Tree: "CAPTURE-4", Project: "CAPTURE", Title: "captured gate", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	engine := New(record.NewStore(), Config{Project: "CAPTURE", ReviewRoundCap: 3, MaxFixAttempts: 3, LingerHours: time.Hour, Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }}, nil)
	if _, err := intake.ApplyFact(ctx, pool, "api", "register-captured-gate", intake.GateRegistered{Issue: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Version: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("register gate: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "approve-v1", intake.DispatchArtifact{Key: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Kind: intake.DispatchArtifactApproved, Version: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("approve v1: %v", err)
	}

	js := testJetStream(t)
	stop := startConsume(t, js, pool, engine)
	defer stop()
	publishCaptured(t, js, "notifications.dispatch.issue.CAPTURE-4.artifact.version", "../intake/testdata/dispatch/artifact-version.json")
	eventually(t, "captured version closes gate", func() bool {
		gate := gateState(t, pool, "CAPTURE-4")
		return gate.LatestVersion == 2 && gate.ApprovedVersion != nil && *gate.ApprovedVersion == 1
	})
	publishCaptured(t, js, "notifications.dispatch.issue.CAPTURE-4.artifact.approved", "../intake/testdata/dispatch/artifact-approved.json")
	eventually(t, "captured approval opens gate", func() bool {
		gate := gateState(t, pool, "CAPTURE-4")
		return gate.LatestVersion == 2 && gate.ApprovedVersion != nil && *gate.ApprovedVersion == 2
	})
}

func TestCapturedApprovedReviewFlowsThroughConsumeToRetro(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "captured review", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-captured", Verdict: "green", Failing: []string{}, FailingStatuses: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	js := testJetStream(t)
	stop := startConsume(t, js, pool, testEngine())
	defer stop()
	publishCaptured(t, js, "notifications.github.sjawhar.legion.pr.42.review", "../intake/testdata/github/review.json")
	eventually(t, "captured approval reaches retro", func() bool {
		var gotPhase, status string
		if err := pool.QueryRow(ctx, "select phase, status from issues where key = $1", "LEGION-208").Scan(&gotPhase, &status); err != nil {
			return false
		}
		return gotPhase == string(phase.Retro) && status == "retro"
	})
	assertOutboxKinds(t, pool, []string{"dispatch_status", "supervise", "supervise", "notice"})
}

func TestReviewRoundCapPostsOneMessageAndNoticeForTheThirdRound(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Verdict: "green", Failing: []string{}, FailingStatuses: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Rounds: 2})
	engine := testEngine()

	if _, err := intake.ApplyFact(ctx, pool, "github", "changes-requested", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", HeadSHA: "head"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact review: %v", err)
	}
	assertOutboxKinds(t, pool, []string{"dispatch_message", "notice", "dispatch_status", "supervise", "supervise", "notice"})
	var rounds int
	if err := pool.QueryRow(ctx, "select rounds from phases where issue = $1 and role = $2", "LEGION-208", "implementer").Scan(&rounds); err != nil {
		t.Fatalf("read implementer rounds: %v", err)
	}
	if rounds != 3 {
		t.Fatalf("implementer rounds = %d, want 3", rounds)
	}
}

// The production check is the implementer's last phase. Its completion moves no phase — the
// architect's sign-off does — so the completion is what tells the architect the check is done.
func TestProductionCheckCompletionTellsTheArchitectAndAwaitsSignOff(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.ProductionCheck, Generation: 1, Status: "retro", Rank: "U"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "retro"})

	if _, err := intake.ApplyFact(ctx, pool, "api", "production-check", intake.HandoffComplete{
		Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "the merged change serves", Commit: "retro",
	}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact production check: %v", err)
	}
	var gotPhase string
	if err := pool.QueryRow(ctx, "select phase from issues where key = $1", "LEGION-208").Scan(&gotPhase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if gotPhase != string(phase.ProductionCheck) {
		t.Fatalf("phase = %q, want production_check until the architect signs off", gotPhase)
	}
	var notice record.Notice
	var payload []byte
	if err := pool.QueryRow(ctx, "select payload from outbox where kind = 'notice'").Scan(&payload); err != nil {
		t.Fatalf("read the one notice: %v", err)
	}
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode notice %s: %v", payload, err)
	}
	if want := (record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.ProductionCheck, Summary: "the merged change serves"}); notice != want {
		t.Fatalf("notice = %+v, want %+v", notice, want)
	}
	assertOutboxKinds(t, pool, []string{"notice"})
}

// Linger suspends, and its expiry stops, every claim the tree holds — the root architect admission
// started and a worker that never reported a handoff included, neither of which has a phase row.
func TestSignOffLingersOnceAndExpiryStopsTreeAndRemovesEveryWorkspace(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.ProductionCheck, Generation: 7, Status: "retro", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer", HandoffCommit: "production-check"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer"})
	engine := New(record.NewStore(), Config{Project: "LEGION", LingerHours: time.Hour, Clock: func() time.Time { return now }}, nil)

	if _, err := intake.ApplyFact(ctx, pool, "api", "signoff", intake.SignOff{Issue: "LEGION-208"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact signoff: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "api", "signoff-echo", intake.SignOff{Issue: "LEGION-208"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact signoff echo: %v", err)
	}
	var lingerUntil time.Time
	if err := pool.QueryRow(ctx, "select linger_until from issues where key = $1", "LEGION-208").Scan(&lingerUntil); err != nil {
		t.Fatalf("read linger deadline: %v", err)
	}
	if !lingerUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("LingerUntil = %s, want %s", lingerUntil, now.Add(time.Hour))
	}
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	// The sign-off's own transition suspends the implementer it moves off, before linger does.
	suspended := superviseRequests(t, pool, "suspend")
	suspended["LEGION-208/implementer"]--
	if !sameCounts(suspended, everyClaim) {
		t.Fatalf("linger suspended %v, want each tree claim once: %v", suspended, everyClaim)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
	assertOutboxCount(t, pool, "dispatch_status", 1)

	if _, err := intake.ApplyFact(ctx, pool, "timer", "linger-current", intake.LingerExpired{Issue: "LEGION-208", Generation: 7}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact current linger expiry: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "timer", "linger-stale", intake.LingerExpired{Issue: "LEGION-208", Generation: 6}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact stale linger expiry: %v", err)
	}
	if stopped := superviseRequests(t, pool, "stop"); !sameCounts(stopped, everyClaim) {
		t.Fatalf("linger expiry stopped %v, want each tree claim once: %v", stopped, everyClaim)
	}
	assertOutboxCount(t, pool, "workspace_remove", 2)
}

// superviseRequests counts the enqueued supervise requests of one operation by issue/role.
func superviseRequests(t *testing.T, pool *pgxpool.Pool, op string) map[string]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue, payload->>'role' from outbox where kind = 'supervise' and payload->>'op' = $1", op)
	if err != nil {
		t.Fatalf("read %s requests: %v", op, err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var issue, role string
		if err := rows.Scan(&issue, &role); err != nil {
			t.Fatalf("scan %s request: %v", op, err)
		}
		counts[issue+"/"+role]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s requests: %v", op, err)
	}
	return counts
}

func sameCounts(got, want map[string]int) bool {
	for key, n := range got {
		if n != want[key] {
			return false
		}
	}
	for key, n := range want {
		if n != got[key] {
			return false
		}
	}
	return true
}

func assertOutboxCount(t *testing.T, pool *pgxpool.Pool, kind string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), "select count(*) from outbox where kind = $1", kind).Scan(&got); err != nil {
		t.Fatalf("count %s rows: %v", kind, err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", kind, got, want)
	}
}

func TestRemainingForwardRowsApplyThroughIntake(t *testing.T) {
	cases := []struct {
		name       string
		current    phase.Phase
		role       claim.Role
		setup      func(t *testing.T, pool *pgxpool.Pool)
		fact       func() intake.Fact
		wantPhase  phase.Phase
		wantStatus string
		wantOutbox []string
	}{
		{
			name: "planner completion", current: phase.Planning, role: claim.RolePlanner,
			fact: func() intake.Fact {
				return intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RolePlanner, Claim: "claim", Commit: "plan"}
			},
			wantPhase: phase.Implementing, wantStatus: "in_progress", wantOutbox: []string{"supervise", "supervise", "notice"},
		},
		{
			name: "tester pass", current: phase.Testing, role: claim.RoleTester,
			fact: func() intake.Fact {
				return intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "claim", Verdict: "pass", Commit: "test"}
			},
			wantPhase: phase.Reviewing, wantStatus: "needs_review", wantOutbox: []string{"dispatch_status", "supervise", "supervise", "notice"},
		},
		{
			name: "tester fail", current: phase.Testing, role: claim.RoleTester,
			fact: func() intake.Fact {
				return intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "claim", Verdict: "fail", Commit: "test"}
			},
			wantPhase: phase.Implementing, wantStatus: "in_progress", wantOutbox: []string{"dispatch_status", "supervise", "supervise", "notice"},
		},
		{
			name: "approved review", current: phase.Reviewing, role: claim.RoleReviewer,
			setup: func(t *testing.T, pool *pgxpool.Pool) {
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Verdict: "green", Failing: []string{}, FailingStatuses: []string{}})
			},
			fact: func() intake.Fact {
				return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", HeadSHA: "head"}
			},
			wantPhase: phase.Retro, wantStatus: "retro", wantOutbox: []string{"dispatch_status", "supervise", "supervise", "notice"},
		},
		{
			name: "retro completion", current: phase.Retro, role: claim.RoleImplementer,
			fact: func() intake.Fact {
				return intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "claim", Commit: "retro"}
			},
			wantPhase: phase.Merging, wantStatus: "retro", wantOutbox: []string{"supervise", "supervise", "notice"},
		},
		{
			name: "approved merger ready", current: phase.Merging, role: claim.RoleMerger,
			setup: func(t *testing.T, pool *pgxpool.Pool) {
				seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 1, ApprovedVersion: new(1)})
			},
			fact: func() intake.Fact {
				return intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "claim", Ready: true}
			},
			wantPhase: phase.AwaitingMerge, wantStatus: "retro", wantOutbox: []string{"supervise", "notice"},
		},
		{
			name: "merged pull request", current: phase.AwaitingMerge,
			setup: func(t *testing.T, pool *pgxpool.Pool) {
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}, FailingStatuses: []string{}})
			},
			fact: func() intake.Fact {
				return intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}
			},
			wantPhase: phase.ProductionCheck, wantStatus: "retro", wantOutbox: []string{"supervise", "notice"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "row", Phase: tc.current, Generation: 1, Status: fixtureStatus(tc.current), Rank: "U"})
			if tc.role != "" {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.role, Claim: "claim"})
			}
			if tc.setup != nil {
				tc.setup(t, pool)
			}
			if _, err := intake.ApplyFact(context.Background(), pool, "fact", tc.name, tc.fact(), testEngine(), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			var gotPhase, gotStatus string
			if err := pool.QueryRow(t.Context(), "select phase, status from issues where key = $1", "LEGION-208").Scan(&gotPhase, &gotStatus); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if gotPhase != string(tc.wantPhase) || gotStatus != tc.wantStatus {
				t.Fatalf("phase/status = %q/%q, want %q/%q", gotPhase, gotStatus, tc.wantPhase, tc.wantStatus)
			}
			assertOutboxKinds(t, pool, tc.wantOutbox)
			assertStartTasksNamePhase(t, pool, tc.wantPhase)
		})
	}
}

// assertStartTasksNamePhase requires every worker start a fact enqueued to name the phase it starts:
// the implementer runs implementing, retro, and production_check under one role, and the task is
// all that tells a resumed implementer which one it is in.
func assertStartTasksNamePhase(t *testing.T, pool *pgxpool.Pool, want phase.Phase) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start'")
	if err != nil {
		t.Fatalf("read start requests: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var task string
		if err := rows.Scan(&task); err != nil {
			t.Fatalf("scan start task: %v", err)
		}
		if !strings.Contains(task, "Phase: "+string(want)+".") {
			t.Fatalf("start task %q does not name phase %s", task, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate start requests: %v", err)
	}
}

func fixtureStatus(current phase.Phase) string {
	switch current {
	case phase.Testing:
		return "testing"
	case phase.Reviewing:
		return "needs_review"
	case phase.Retro, phase.Merging, phase.AwaitingMerge:
		return "retro"
	default:
		return "in_progress"
	}
}

func TestEveryBackwardEdgeAppliesThroughIntake(t *testing.T) {
	for _, from := range []phase.Phase{phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro, phase.Merging, phase.ProductionCheck} {
		for _, to := range requiredBackwardTargets(from) {
			t.Run(string(from)+"-to-"+string(to), func(t *testing.T) {
				pool := migratedPool(t)
				role := roleFor(from)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "backward", Phase: from, Generation: 1, Status: "in_progress", Rank: "U"})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: "claim"})
				if _, err := intake.ApplyFact(context.Background(), pool, "api", string(from)+"-"+string(to), intake.BackwardMove{Issue: "LEGION-208", Requester: role, To: to, Reason: "correct"}, testEngine(), admissionStub{}); err != nil {
					t.Fatalf("ApplyFact backward: %v", err)
				}
				var got string
				if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", "LEGION-208").Scan(&got); err != nil {
					t.Fatalf("read phase: %v", err)
				}
				if got != string(to) {
					t.Fatalf("phase = %q, want %q", got, to)
				}
			})
		}
	}
}

func seedIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer tx.Rollback(t.Context())
	if err := record.NewStore().PutIssue(t.Context(), tx, issue); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit seed issue: %v", err)
	}
}

func seedGate(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutGate(t.Context(), tx, gate) })
}

func seedSlot(t *testing.T, pool *pgxpool.Pool, slot record.Slot) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutSlot(t.Context(), tx, slot) })
}

func seedPhase(t *testing.T, pool *pgxpool.Pool, row record.PhaseRow) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutPhase(t.Context(), tx, row) })
}

func seedPR(t *testing.T, pool *pgxpool.Pool, pr record.PullRequest) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutPullRequest(t.Context(), tx, pr) })
}

func seedRecord(t *testing.T, pool *pgxpool.Pool, put func(pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer tx.Rollback(t.Context())
	if err := put(tx); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit seed record: %v", err)
	}
}

func testEngine() *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, MaxFixAttempts: 3, LingerHours: time.Hour,
		Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

func assertOutboxKinds(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select kind from outbox order by id")
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan outbox kind: %v", err)
		}
		got = append(got, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox kinds: %v", err)
	}
	if !sameStrings(got, want) {
		t.Fatalf("outbox kinds = %v, want %v", got, want)
	}
}

func gateState(t *testing.T, pool *pgxpool.Pool, issue string) record.DesignGate {
	t.Helper()
	var gate *record.DesignGate
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		gate, err = record.NewStore().Gate(t.Context(), tx, issue)
		return err
	})
	if gate == nil {
		t.Fatalf("gate %s is missing", issue)
	}
	return *gate
}

func testJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js := testnats.JetStream(t)
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	return js
}

func startConsume(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, engine *Engine) func() {
	t.Helper()
	consumers, err := intake.OpenConsumers(t.Context(), js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []string{"sjawhar/legion"}, AckWait: time.Second, NakDelay: time.Millisecond,
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumers.Run(ctx, pool, engine, admissionStub{}) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
}

func publishCaptured(t *testing.T, js jetstream.JetStream, subject, path string) {
	t.Helper()
	captured, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured event %s: %v", path, err)
	}
	if _, err := js.Publish(t.Context(), subject, captured); err != nil {
		t.Fatalf("publish captured event %s: %v", path, err)
	}
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so workflow tests need no database")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(t.Context(), adminURL.String())
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_workflow_test_" + randomSuffix(t)
	if _, err := admin.Exec(t.Context(), "create database "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	testURL := *base
	testURL.Path = "/" + name
	st, err := legionstore.Open(t.Context(), testURL.String())
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if _, err := st.Migrate(t.Context()); err != nil {
		st.Close()
		t.Fatalf("migrate test store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(t.Context(), testURL.String())
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}
