package workflow

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// strandedStore is the issue record, the admission slots and the queued supervise rows Stranded
// reads; every claim's stored row is the one its role run reads back.
type strandedStore struct {
	record.Store
	issues        []record.Issue
	slots         []record.Slot
	queued        map[claim.Token][]record.QueuedSupervise
	claims        map[claim.Token]supervise.Claim
	roleRunCalls  int
	roleRunsCalls int
}

func (s *strandedStore) Issues(context.Context, pgx.Tx) ([]record.Issue, error) { return s.issues, nil }

func (s *strandedStore) Slots(context.Context, pgx.Tx) ([]record.Slot, error) { return s.slots, nil }

// slotsOf is one admission slot for each of keys.
func slotsOf(keys ...string) []record.Slot {
	slots := make([]record.Slot, len(keys))
	for i, key := range keys {
		slots[i] = record.Slot{Issue: key, Index: i}
	}
	return slots
}

func (s *strandedStore) RoleRun(_ context.Context, _ pgx.Tx, token claim.Token, _ string, _ claim.Role, _ uint64) (record.RoleRun, error) {
	s.roleRunCalls++
	return s.roleRun(token), nil
}

func (s *strandedStore) RoleRuns(_ context.Context, _ pgx.Tx, keys []record.RoleRunKey) (map[claim.Token]record.RoleRun, error) {
	s.roleRunsCalls++
	runs := make(map[claim.Token]record.RoleRun, len(keys))
	for _, key := range keys {
		runs[key.Token] = s.roleRun(key.Token)
	}
	return runs, nil
}

func (s *strandedStore) roleRun(token claim.Token) record.RoleRun {
	run := record.RoleRun{Queued: s.queued[token]}
	if c, ok := s.claims[token]; ok {
		run.Claim = &record.RoleClaim{State: c.State, Tree: c.Tree, LastStartRow: c.LastStartRow}
	}
	return run
}

func strandedClaim(t *testing.T, tree, issue string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role, Generation: 1, State: state}
}

func strandedIssue(key, tree string, p phase.Phase) record.Issue {
	issue := record.Issue{Key: key, Tree: tree, Project: "LEGION", Phase: p, Generation: 2, Status: "in_progress"}
	if key != tree {
		issue.Parent = &tree
	}
	return issue
}

// Each claim nothing will wake is named, and every claim something will still wake, or that
// waits correctly, is not.
func TestStrandedNamesTheClaimsNothingWillWake(t *testing.T) {
	until := time.Now().Add(time.Hour)
	held := strandedIssue("LEGION-3", "LEGION-3", phase.Held)
	held.Hold = &record.Hold{From: phase.Reviewing}
	lingering := strandedIssue("LEGION-7", "LEGION-7", phase.Done)
	lingering.LingerUntil = &until
	// A lingering root re-admitted while every slot is taken: readmit has cleared its linger and
	// recorded a new generation waiting for a slot, and the architect the linger suspended stays
	// suspended with no start queued. Admission starts it once a slot frees; resumed now, it would
	// run outside the cap.
	waiting := strandedIssue("LEGION-10", "LEGION-10", phase.Admitted)
	waiting.Generation, waiting.Status, waiting.HandedOver = 3, "todo", true
	issues := []record.Issue{
		strandedIssue("LEGION-1", "LEGION-1", phase.Implementing),
		strandedIssue("LEGION-2", "LEGION-1", phase.Retro),
		held,
		strandedIssue("LEGION-4", "LEGION-4", phase.Testing),
		strandedIssue("LEGION-5", "LEGION-5", phase.Merging),
		strandedIssue("LEGION-6", "LEGION-6", phase.Planning),
		lingering,
		strandedIssue("LEGION-8", "LEGION-7", phase.ProductionCheck),
		strandedIssue("LEGION-9", "LEGION-404", phase.Planning),
		waiting,
		strandedIssue("LEGION-11", "LEGION-11", phase.Testing),
	}

	implementer := strandedClaim(t, "LEGION-1", "LEGION-1", claim.RoleImplementer, supervise.StateSuspended)
	retro := strandedClaim(t, "LEGION-1", "LEGION-2", claim.RoleImplementer, supervise.StateSuspended)
	failedReviewer := strandedClaim(t, "LEGION-3", "LEGION-3", claim.RoleReviewer, supervise.StateFailed)
	failedArchitect := strandedClaim(t, "LEGION-6", "LEGION-6", claim.RoleArchitect, supervise.StateFailed)
	// A notice to a suspended tree architect waits for it, and nothing in the daemon resumes it.
	suspendedArchitect := strandedClaim(t, "LEGION-1", "LEGION-1", claim.RoleArchitect, supervise.StateSuspended)
	// The failed tester's start for testing is queued, and its retry relaunches it.
	startQueued := strandedClaim(t, "LEGION-4", "LEGION-4", claim.RoleTester, supervise.StateFailed)
	startQueued.LastStartRow = 10
	// The architect the daemon suspended has its admission start queued, which names no phase and
	// resumes it by rule.
	architectStartQueued := strandedClaim(t, "LEGION-4", "LEGION-4", claim.RoleArchitect, supervise.StateSuspended)
	architectStartQueued.SuspendedBy = supervise.SuspendedByDaemon
	// The failed merger's start is queued, but a suspend written after it still acts and undoes it.
	startUndone := strandedClaim(t, "LEGION-5", "LEGION-5", claim.RoleMerger, supervise.StateFailed)
	startUndone.LastStartRow = 10
	// An operator's suspension holds until the operator resumes the claim: a start queued for it
	// waits (supervise's RuleHolds), so it wakes neither the architect nor the tester.
	operatorArchitect := strandedClaim(t, "LEGION-5", "LEGION-5", claim.RoleArchitect, supervise.StateSuspended)
	operatorArchitect.SuspendedBy = supervise.SuspendedByOperator
	operatorTester := strandedClaim(t, "LEGION-11", "LEGION-11", claim.RoleTester, supervise.StateSuspended)
	operatorTester.SuspendedBy = supervise.SuspendedByOperator
	claims := []supervise.Claim{
		failedArchitect,
		implementer,
		// A finished phase's worker is started again only when its phase comes.
		strandedClaim(t, "LEGION-1", "LEGION-1", claim.RolePlanner, supervise.StateSuspended),
		suspendedArchitect,
		// A failed sub-architect the operator started.
		strandedClaim(t, "LEGION-1", "LEGION-2", claim.RoleArchitect, supervise.StateFailed),
		retro,
		failedReviewer,
		startQueued,
		architectStartQueued,
		startUndone,
		operatorArchitect,
		operatorTester,
		// A tree that lingers holds its members, and its re-admission starts them again.
		strandedClaim(t, "LEGION-7", "LEGION-7", claim.RoleArchitect, supervise.StateFailed),
		strandedClaim(t, "LEGION-7", "LEGION-8", claim.RoleImplementer, supervise.StateSuspended),
		// An issue whose tree root is not recorded, and an issue not recorded at all.
		strandedClaim(t, "LEGION-404", "LEGION-9", claim.RolePlanner, supervise.StateSuspended),
		strandedClaim(t, "S2-1", "S2-1", claim.RoleImplementer, supervise.StateSuspended),
		// A worker of its issue's phase that runs, or is launching, is not stranded.
		strandedClaim(t, "LEGION-6", "LEGION-6", claim.RolePlanner, supervise.StateIdle),
		// The architect of a root waiting for a slot.
		strandedClaim(t, "LEGION-10", "LEGION-10", claim.RoleArchitect, supervise.StateSuspended),
	}
	stored := map[claim.Token]supervise.Claim{}
	for _, c := range claims {
		stored[c.Token] = c
	}
	s := &strandedStore{
		issues: issues,
		slots:  slotsOf("LEGION-1", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6", "LEGION-11"),
		claims: stored,
		queued: map[claim.Token][]record.QueuedSupervise{
			startQueued.Token:          {{ID: 11, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-4", Role: claim.RoleTester, Generation: 2, Phase: phase.Testing}}},
			architectStartQueued.Token: {{ID: 13, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-4", Role: claim.RoleArchitect, Generation: 2}}},
			startUndone.Token: {
				{ID: 11, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-5", Role: claim.RoleMerger, Generation: 2, Phase: phase.Merging}},
				{ID: 12, Request: record.SuperviseRequest{Op: "suspend", Tree: "LEGION-5", Role: claim.RoleMerger, Generation: 2}},
			},
			operatorArchitect.Token: {{ID: 14, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-5", Role: claim.RoleArchitect, Generation: 2}}},
			operatorTester.Token:    {{ID: 15, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-11", Role: claim.RoleTester, Generation: 2, Phase: phase.Testing}}},
		},
	}

	got, err := Stranded(context.Background(), nil, s, claims)
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	want := []supervise.Claim{suspendedArchitect, implementer, operatorTester, retro, failedReviewer, operatorArchitect, startUndone, failedArchitect}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stranded = %v, want %v", names(got), names(want))
	}
}

// A phase worker the daemon or its own exit suspended is not stranded: the daemon resumes it by rule,
// at boot (DaemonBooted) and at its phase's next start. One an operator suspended is: only the
// operator's resume runs it again, a start queued for it included. One whose suspension predates
// migration 0038 (unknown) is stranded while no start is queued for it, since the boot resumes no
// such claim; a start queued for it resumes it, as on main. A tree architect is stranded whoever
// suspended it, since no rule resumes an architect.
func TestStrandedLeavesOutTheWorkersTheRuleResumes(t *testing.T) {
	issues := []record.Issue{
		strandedIssue("LEGION-1", "LEGION-1", phase.Implementing),
		strandedIssue("LEGION-2", "LEGION-2", phase.Testing),
		strandedIssue("LEGION-3", "LEGION-3", phase.Reviewing),
		strandedIssue("LEGION-4", "LEGION-4", phase.Merging),
		strandedIssue("LEGION-5", "LEGION-5", phase.Testing),
	}
	by := func(c supervise.Claim, source supervise.SuspendSource) supervise.Claim {
		c.SuspendedBy = source
		return c
	}
	daemonWorker := by(strandedClaim(t, "LEGION-1", "LEGION-1", claim.RoleImplementer, supervise.StateSuspended), supervise.SuspendedByDaemon)
	operatorWorker := by(strandedClaim(t, "LEGION-2", "LEGION-2", claim.RoleTester, supervise.StateSuspended), supervise.SuspendedByOperator)
	unknownWorker := by(strandedClaim(t, "LEGION-3", "LEGION-3", claim.RoleReviewer, supervise.StateSuspended), supervise.SuspendedByUnknown)
	exitWorker := by(strandedClaim(t, "LEGION-4", "LEGION-4", claim.RoleMerger, supervise.StateSuspended), supervise.SuspendedByExit)
	daemonArchitect := by(strandedClaim(t, "LEGION-1", "LEGION-1", claim.RoleArchitect, supervise.StateSuspended), supervise.SuspendedByDaemon)
	unknownStarted := by(strandedClaim(t, "LEGION-5", "LEGION-5", claim.RoleTester, supervise.StateSuspended), supervise.SuspendedByUnknown)
	claims := []supervise.Claim{daemonWorker, operatorWorker, unknownWorker, exitWorker, daemonArchitect, unknownStarted}
	stored := map[claim.Token]supervise.Claim{}
	for _, c := range claims {
		stored[c.Token] = c
	}

	store := &strandedStore{issues: issues, slots: slotsOf("LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5"), claims: stored,
		queued: map[claim.Token][]record.QueuedSupervise{
			unknownStarted.Token: {{ID: 21, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-5", Role: claim.RoleTester, Generation: 2, Phase: phase.Testing}}},
		}}
	got, err := Stranded(context.Background(), nil, store, claims)
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	want := []supervise.Claim{daemonArchitect, operatorWorker, unknownWorker}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stranded = %v, want %v", names(got), names(want))
	}
}

func names(claims []supervise.Claim) []string {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = c.Issue + " " + string(c.Role) + " " + string(c.State)
	}
	return out
}

// A large fleet has hundreds of suspended claims after an operator action. Stranded reads their
// queued starts in bounded batches rather than issuing one outbox and one claims query per role.
func TestStrandedReadsRoleRunsInBatches(t *testing.T) {
	const claimsCount = 401
	store := &strandedStore{claims: map[claim.Token]supervise.Claim{}}
	claims := make([]supervise.Claim, 0, claimsCount)
	for i := range claimsCount {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		store.issues = append(store.issues, strandedIssue(issue, issue, phase.Planning))
		store.slots = append(store.slots, record.Slot{Issue: issue, Index: i})
		current := strandedClaim(t, issue, issue, claim.RolePlanner, supervise.StateSuspended)
		store.claims[current.Token] = current
		claims = append(claims, current)
	}

	got, err := Stranded(context.Background(), nil, store, claims)
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}
	if len(got) != claimsCount {
		t.Fatalf("stranded claims = %d, want %d", len(got), claimsCount)
	}
	if store.roleRunCalls != 0 || store.roleRunsCalls != 3 {
		t.Fatalf("role-run reads = %d single and %d batches, want 0 single and 3 bounded batches", store.roleRunCalls, store.roleRunsCalls)
	}
}
