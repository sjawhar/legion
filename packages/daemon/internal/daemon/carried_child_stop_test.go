package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A finished root's close suspends every member of its tree, and the suspension of a role in a
// turn is held for that turn's end. A child carried into a tree of its own before that turn ends —
// a person's label on it (admission's orphan carry) or the root's hand-over at its linger's expiry
// (admission's consumption of the hand-over) — moves to a new generation, and the close's suspend
// row, of the earlier generation, finishes without acting. The new tree's starts then stop that
// role themselves: they hold its suspension, as the daemon and keeping its session, and launch
// nothing of the new tree while it runs in the old one. They do so with the close's hold still in
// memory, and with nothing held, as after a restart that forgot it. The hold is bounded where the
// old tree's close would have ended the turn, at the old root's linger expiry, not at the stop
// timeout. Once the turn ends, or that expiry ends a turn that never does, the new tree's architect
// launches and the role resumes its session in the new tree.
func TestACarriedChildsRoleStillInATurnStopsBeforeItsNewTreeLaunches(t *testing.T) {
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	carries := []struct {
		name string
		// carry moves the child into a tree of its own, through admission.
		carry func(apply func(id string, fact intake.Fact))
	}{
		{"a person's label on the child", func(apply func(string, intake.Fact)) {
			apply("person-labels-the-child", intake.DispatchIssue{Key: childKey, Seq: 2, Type: "issue.updated", Status: "todo",
				Title: "child", Parent: rootKey, Rank: "B", HandedOver: true})
		}},
		{"the root's hand-over at its linger's expiry", func(apply func(string, intake.Fact)) {
			apply("handover:"+childKey, intake.Handover{Issue: childKey, Tree: rootKey, Generation: 1})
		}},
	}
	stops := []struct {
		name string
		// closeHeld is whether the close's suspension is held in memory when the child is carried.
		closeHeld bool
		// end is how the role's turn in the old tree ends.
		end turnEnd
	}{
		{"with the close's suspension held", true, turnEnds},
		{"with nothing held, as after a restart", false, turnEnds},
		{"with the close's suspension held and a turn that never ends", true, lingerExpires},
		{"with nothing held and a turn that never ends", false, lingerExpires},
		{"with the close's suspension held and the operator stopping a hung turn", true, operatorStops},
	}
	for _, cc := range carries {
		for _, sc := range stops {
			t.Run(cc.name+"/"+sc.name, func(t *testing.T) {
				carriedChildStops(t, rootKey, childKey, cc.carry, sc.closeHeld, sc.end)
			})
		}
	}
}

// turnEnd is how a carried role's turn in its old tree ends in carriedChildStops.
type turnEnd string

const (
	// turnEnds is the turn's own end.
	turnEnds turnEnd = "turn"
	// lingerExpires is a turn that never ends, stopped at the old root's linger expiry.
	lingerExpires turnEnd = "expiry"
	// operatorStops is a hung turn the operator suspends, stopped at the stop timeout from then.
	operatorStops turnEnd = "operator"
)

// carriedChildStops runs one case of TestACarriedChildsRoleStillInATurnStopsBeforeItsNewTreeLaunches:
// carry moves the child, closeHeld holds the close's suspension in memory first, and end says how
// the role's turn in the old tree ends.
func carriedChildStops(t *testing.T, rootKey, childKey string, carry func(apply func(id string, fact intake.Fact)), closeHeld bool, end turnEnd) {
	// stopTimeout is the machines' stop timeout, and linger the old root's time left to linger, each a
	// wait no other timer of the test is armed for, so firing one fires only the held suspension's.
	const stopTimeout, linger = 7 * time.Second, time.Hour
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	// Whole seconds, so the linger expiry Postgres stores (to the microsecond) reads back as set, and
	// the deadline the daemon names from it is armed as exactly linger from the machines' start.
	start := time.Now().UTC().Truncate(time.Second)
	until := start.Add(linger)
	parent := rootKey
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: rootKey, Tree: rootKey, Project: "LEGION", Title: "root", Phase: phase.Done,
				Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}),
			records.PutIssue(ctx, tx, record.Issue{Key: childKey, Tree: rootKey, Project: "LEGION", Title: "child", Parent: &parent,
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
			records.PutHandover(ctx, tx, record.Handover{Issue: childKey, Tree: rootKey, Generation: 1}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the finished root and its child: %v", err)
	}
	// The claims live in the daemon's real store, as in production, so admission's promotion reads
	// the implementer's stored claim as it stands: working, in the old tree.
	trees := outboxTreeStore(t, pool, rootKey)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machineClock := &heldClock{at: start}
	sup.deps.Clock, sup.deps.Timeouts.Stop, sup.deps.Store = machineClock, stopTimeout, trees
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: time.Hour}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	var logs bytes.Buffer
	clock := start
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		githubAPI: newBranchGitHub(t, nil, branchExists).url, notices: &outboxPublisher{},
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: childKey, Status: "todo", HandedOver: true, Labels: []string{"legion"}}},
		handlers: []intake.Handler{engine, admission}, now: func() time.Time { return clock },
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + childKey}, nil
		},
	}
	run := func(what string) {
		t.Helper()
		for range 3 {
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			clock = clock.Add(5 * time.Minute)
		}
	}

	// The child's implementer works in its old tree, in a turn when the root's close suspends it.
	implementer := launchedClaim(t, sup, rootKey, childKey, claim.RoleImplementer)
	machine, _ := sup.Machine(implementer)
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: implementer}); err != nil {
		t.Fatalf("the implementer's turn: %v", err)
	}
	if closeHeld {
		enqueueOutbox(t, pool, records, mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "suspend", Tree: rootKey,
			Role: claim.RoleImplementer, Generation: 1, Reason: "the tree of LEGION-207 lingers"}, clock))
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the close's suspend: %v", err)
		}
		if got := machine.Claim(); got.State != supervise.StateWorking || !got.SuspensionHeld {
			t.Fatalf("the implementer after the close's suspend = %s, held %t; want working with the suspension held", got.State, got.SuspensionHeld)
		}
	}

	carry(func(id string, fact intake.Fact) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, engine, admission); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	})
	var carried *record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		carried, err = records.Issue(ctx, tx, childKey)
		return err
	}); err != nil || carried == nil || carried.Tree != childKey || carried.Generation != 2 || carried.Phase != phase.Implementing {
		t.Fatalf("the carried child = %+v, %v; want its own tree at generation 2 in implementing", carried, err)
	}
	// Promotion read the implementer's stored claim working in the old tree, and still queued its
	// start in the new one: that claim was started by the old tree, not by this run.
	var state, tree string
	if err := pool.QueryRow(ctx, `select state, tree from claims where token = $1`, string(implementer)).Scan(&state, &tree); err != nil ||
		state != string(supervise.StateWorking) || tree != rootKey {
		t.Fatalf("the implementer's stored claim at the carry = %s in %s, %v; want working in %s", state, tree, err, rootKey)
	}
	var starts int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'implementer' and payload->>'generation' = '2'`, childKey).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("the carry's promotion queued %d starts of the implementer at generation 2, %v; want one", starts, err)
	}

	run("the carry's rows while the implementer's turn runs")
	if closeHeld && !strings.Contains(logs.String(), `"msg":"outbox supervise row serves an earlier generation; finished without acting"`) {
		t.Fatalf("the close's suspend row did not finish as one of an earlier generation; log:\n%s", logs.String())
	}
	architect := mustClaimToken(t, childKey, claim.RoleArchitect)
	launchesOf := func(method string, token claim.Token) []fake.Call {
		var of []fake.Call
		for _, call := range rt.CallsOf(method) {
			if call.Spec.Claim == token {
				of = append(of, call)
			}
		}
		return of
	}
	if spawns := launchesOf("Spawn", architect); len(spawns) != 0 {
		t.Fatalf("the new tree's architect launched %d times while the implementer still ran its turn in tree %s; want none", len(spawns), rootKey)
	}
	if got := machine.Claim(); got.State != supervise.StateWorking || !got.SuspensionHeld || got.SuspensionHeldBy != supervise.SuspendedByDaemon || got.Tree != rootKey {
		t.Fatalf("the implementer while its turn runs = %s in tree %s, held %t by %q; want working in %s with the daemon's suspension held",
			got.State, got.Tree, got.SuspensionHeld, got.SuspensionHeldBy, rootKey)
	}

	wantBy := supervise.SuspendedByDaemon
	switch end {
	case lingerExpires:
		if fired := machineClock.fire(linger); fired != 1 {
			t.Fatalf("fired %d deadlines at the old root's linger expiry, want the held suspension's one", fired)
		}
	case operatorStops:
		wantBy = supervise.SuspendedByOperator
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: implementer, By: supervise.SuspendedByOperator,
			Reason: "the turn hangs inside the linger"}); !errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("the operator's suspend of the hung turn = %v, want it held for the turn", err)
		}
		if fired := machineClock.fire(stopTimeout); fired != 1 {
			t.Fatalf("fired %d stop timeouts after the operator's suspend, want the held suspension's one", fired)
		}
	default:
		if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: implementer}); err != nil {
			t.Fatalf("end the implementer's turn: %v", err)
		}
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != wantBy ||
		got.Session != "ses-implementer" || got.SessionFile != "/tmp/implementer.jsonl" {
		t.Fatalf("the implementer after its turn = %+v; want suspended by %q with its session kept", got, wantBy)
	}
	if end != lingerExpires {
		if fired := machineClock.fire(linger); fired != 0 {
			t.Fatalf("the deadline at the old root's linger expiry fired %d times after the suspension ran; want it stopped with the hold", fired)
		}
	}
	// The work the old generation did — its pushed branch, its handoff, a completion refused as
	// a stale generation — is the resumed session's own: the start that resumes it delivers
	// its phase's resume task at the new generation.
	var task string
	if err := pool.QueryRow(ctx, `select payload->>'task' from outbox where kind = 'supervise' and issue = $1
				and payload->>'op' = 'start' and payload->>'role' = 'implementer' and (payload->>'generation')::int = 2`, childKey).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start at generation 2: %v", err)
	}
	if want := "Issue: " + childKey + ". Phase: implementing. Resume the existing phase work."; !strings.Contains(task, want) {
		t.Fatalf("the implementer's start task = %q, want it to say %q", task, want)
	}
	run("the carry's rows once the implementer has stopped")
	if spawns := launchesOf("Spawn", architect); len(spawns) != 1 || spawns[0].Spec.Tree != childKey {
		t.Fatalf("the architect's launches = %+v; want one, in tree %s", spawns, childKey)
	}
	if end == operatorStops {
		// The operator's suspension holds the claim, so its start waits for the operator; the claim
		// is re-pointed to its new tree while it waits, so the operator's resume relaunches it there.
		if got := machine.Claim(); got.Tree != childKey || got.State != supervise.StateSuspended || len(launchesOf("Resume", implementer)) != 0 {
			t.Fatalf("the operator-held implementer while its start waits = %s in tree %s, resumed %d times; want suspended, re-pointed to %s and not resumed",
				got.State, got.Tree, len(launchesOf("Resume", implementer)), childKey)
		}
		if err := machine.Handle(ctx, supervise.RequestResume{Claim: implementer}); err != nil {
			t.Fatalf("the operator's resume: %v", err)
		}
	}
	resumes := launchesOf("Resume", implementer)
	if len(resumes) != 1 || resumes[0].Spec.Tree != childKey || resumes[0].Spec.ResumeSessionFile != "/tmp/implementer.jsonl" {
		t.Fatalf("the implementer's resumes = %+v; want its session resumed once, in tree %s", resumes, childKey)
	}
	if got := machine.Claim(); got.Tree != childKey {
		t.Fatalf("the implementer's claim names tree %s, want %s", got.Tree, childKey)
	}
}

// A start whose issue's claims all name its own tree waits on none of them: the outbox runs one row
// at a time, and a machine holds its lock through the runtime call its decision makes, so reading a
// sibling claim that way would hold every row behind it for as long as a Sandbox relaunch waits on
// the cluster. The check before a start reads each claim's view, which only Retree changes, and
// goes on while the issue's architect launches.
func TestAStartOfAnIssueInItsOwnTreeWaitsOnNoSiblingsLaunch(t *testing.T) {
	sup, fakeRuntime := newOutboxSupervisor(t, "legion", t.TempDir())
	rt := newStallingRuntime(t, fakeRuntime, false)
	sup.deps.Runtime = rt
	architect := architectClaim(t, sup, "LEGION-1", supervise.StateQueued)
	machine, _ := sup.Machine(architect)
	rt.stalling.Store(true)
	go func() { _ = machine.Handle(context.Background(), supervise.RequestSpawn{Claim: architect}) }()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the architect's launch never reached the runtime")
	}
	runner := &outbox{log: quietLogger(), supervisor: sup, project: "legion"}
	checked := make(chan error, 1)
	go func() {
		checked <- runner.stopElsewhere(context.Background(), record.Issue{Key: "LEGION-1", Tree: "LEGION-1"})
	}()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatalf("the check before a start of LEGION-1 in its own tree = %v, want nothing to wait for", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check before a start waited on its architect's launch in the runtime")
	}
}

// A carried child's role still running in its old tree is stopped where that tree's close would
// have ended it: at the old root's linger expiry while the root lingers. A root that no longer
// lingers, or none recorded, leaves no close to wait for, so the stop is bounded at the stop timeout
// from now, as an operator's is.
func TestACarriedRolesStopDeadlineIsItsOldRootsLingerExpiry(t *testing.T) {
	now := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	until := now.Add(17 * time.Minute)
	const stop = 10 * time.Second
	for _, tc := range []struct {
		name string
		root *record.Issue
		want time.Time
	}{
		{"an old root that lingers", &record.Issue{Key: "LEGION-207", LingerUntil: &until}, until},
		{"an old root whose linger expired before the start asked", &record.Issue{Key: "LEGION-207", LingerUntil: &now}, now},
		{"an old root that no longer lingers", &record.Issue{Key: "LEGION-207"}, now.Add(stop)},
		{"no old root recorded", nil, now.Add(stop)},
	} {
		if got := carriedStopDeadline(tc.root, now, stop); !got.Equal(tc.want) {
			t.Errorf("%s: carriedStopDeadline = %s, want %s", tc.name, got, tc.want)
		}
	}
}
