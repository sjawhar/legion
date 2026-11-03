package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The idle view is one read per issue and it reads the four activity columns from the store, not
// from a machine: a report that arrived between a machine's writes is exactly the fact the rule
// turns on, and putClaim never carries it. It also reads the pending delivery's turn marks, the
// claims' states and contracts, the rows queued for a role of the pod, and the issue's place in
// the workflow beside its root's.
func TestTheIdleViewReadsTheStoredActivityReportAndTheQueuedRows(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	now := time.Now().UTC().Truncate(time.Millisecond)

	token := claim.Token("legion-legion-legion-208-implementer")
	c := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, PluginContract: 47}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into pending_task_deliveries
		(claim_token, delivery_id, task, queued_at, delivered_at, confirmed_at, phase, generation)
		values ($1, 'd-1', 'implement it', $2, $3, $4, 'implementing', 1)`,
		string(token), now.Add(-time.Hour), now.Add(-50*time.Minute), now.Add(-45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The report arrives the way the route sends it, after the machine's own last write.
	reminder := now.Add(-5 * time.Minute)
	reported := claim.SessionActivity{
		At:             now.Add(-40 * time.Minute),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job", "subagent"}},
		Phase:          claim.PhaseActivity{NextReminderAt: &reminder},
	}
	if wrote, err := st.PutClaimActivity(ctx, token, reported); err != nil || !wrote {
		t.Fatalf("PutClaimActivity = %t, %v", wrote, err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error) values
		(31, 'supervise', $1, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now(), ''),
		(32, 'dispatch_status', $1, '{"status":"testing","observedStatus":"in_progress"}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}

	view := idleViewOf(t, st, rootIssue)
	if !view.Found || view.Tree != rootIssue || view.Phase != phase.Implementing || view.Generation != 1 || view.Status != "in_progress" {
		t.Fatalf("view = %+v, want the running issue", view)
	}
	if !view.IsRoot() || view.Root.AwaitingPerson || view.Root.LingerUntil != nil {
		t.Fatalf("root = %+v, want the running root", view.Root)
	}
	if len(view.Claims) != 1 {
		t.Fatalf("claims = %+v, want the one claim", view.Claims)
	}
	got := view.Claims[0]
	if got.Role != claim.RoleImplementer || got.State != supervise.StateIdle || got.PluginContract != 47 || got.ServingGeneration != 1 {
		t.Fatalf("claim = %+v, want the stored implementer", got)
	}
	if !got.Live() {
		t.Fatalf("claim %+v reads as not live", got)
	}
	if !got.ActivityAt.Equal(reported.At) || !got.ActivityBackground ||
		len(got.ActivityKinds) != 2 || got.ActivityKinds[0] != "async-job" || got.ActivityKinds[1] != "subagent" ||
		!got.ActivityNextReminderAt.Equal(reminder) {
		t.Fatalf("activity = %+v, want the session's own report", got)
	}
	if !got.LastTurnAt.Equal(now.Add(-45 * time.Minute)) {
		t.Fatalf("last turn = %s, want the delivery's confirmation %s", got.LastTurnAt, now.Add(-45*time.Minute))
	}
	if !got.Reported() {
		t.Fatalf("claim %+v reads as unreported, though its report is newer than its turn", got)
	}
	if since, ok := got.ActivityAt, got.UpdatedAt.IsZero(); ok || got.UpdatedAt.Before(since) {
		t.Fatalf("updated_at = %s, want it at or after the report at %s", got.UpdatedAt, since)
	}
	// The start waits for a role of the pod; the Dispatch status write reaches no session, so
	// the view would read nothing waiting without it.
	if !view.Waiting {
		t.Fatal("the view reads nothing waiting, though a start of the pod's tester is queued")
	}
	if view.Releasing {
		t.Fatal("the view reports a release in progress with no mark written")
	}

	markRelease(t, st, 30, 31, 32, 0)
	if marked := idleViewOf(t, st, rootIssue); !marked.Releasing {
		t.Fatalf("view after the mark = %+v, want it releasing", marked.Releasing)
	}
}

// The view carries two facts beside the rule's own conditions: whether the issue holds a bundle
// no pod has restored, which keeps the pod from being released over work that never came back,
// and whether a hold streak of the pod is running, which is what the daemon's pass reads before
// it ends one. The second is in the view because the pass already takes this read, and ending
// streaks a pod does not have would cost a write transaction per pod per tick.
func TestTheIdleViewReadsTheUnrestoredBundleAndTheRunningHoldStreak(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	now := time.Now().UTC()

	if view := idleViewOf(t, st, rootIssue); view.BundleUnrestored || view.Holding {
		t.Fatalf("view = %+v, want no stored bundle and no running streak", view)
	}

	if err := st.PutWorkspaceBundle(ctx, rootIssue, 7,
		runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "abc", At: now, PodUID: "pod-being-released"}); err != nil {
		t.Fatalf("save a bundle for %s: %v", rootIssue, err)
	}
	if err := st.RecordPodHold(ctx, PodHold{Project: "legion", Issue: rootIssue, Reason: "image", At: now}); err != nil {
		t.Fatalf("record a hold of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); !view.BundleUnrestored || !view.Holding {
		t.Fatalf("view = %+v, want the unrestored bundle and the running streak", view)
	}

	if err := st.EndPodHoldStreaks(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("end the hold streaks of %s: %v", rootIssue, err)
	}
	if err := st.DeleteWorkspaceBundle(ctx, rootIssue); err != nil {
		t.Fatalf("delete the bundle of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); view.BundleUnrestored || view.Holding {
		t.Fatalf("view = %+v, want the restored bundle and the ended streak gone from it", view)
	}
}

// idleViewOf is the pod's view as the release's own transaction reads it (readIdleView, which
// ReleaseIdleIssue hands to the rule): the store serves no view of its own, because nothing but
// that decision reads one.
func idleViewOf(t *testing.T, st *Store, issue string) pods.IdleView {
	t.Helper()
	ctx := context.Background()
	var view pods.IdleView
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		view, err = readIdleView(ctx, tx, "legion", issue)
		return err
	}); err != nil {
		t.Fatalf("read the idle view of %s: %v", issue, err)
	}
	return view
}

// A machine's own claim write never carries the activity report, which is why an idle decision
// reads it from the store: this pins that a PutClaim after a report leaves the four columns
// standing, so a view taken later still sees what the session said.
func TestAClaimWriteAfterAReportLeavesTheReportStanding(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	c := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateWorking}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	reported := claim.SessionActivity{At: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)}
	if _, err := st.PutClaimActivity(ctx, token, reported); err != nil {
		t.Fatal(err)
	}
	// The machine, which never read the report, persists its next state.
	c.State = supervise.StateIdle
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	view := idleViewOf(t, st, rootIssue)
	if got := view.Claims[0]; !got.ActivityAt.Equal(reported.At) {
		t.Fatalf("activity_at = %s after the machine's write, want the report's %s", got.ActivityAt, reported.At)
	}
}

// The release's one transaction: the view it decides on, the rows it queues and the mark that
// records it commit together, and the mark's boundary is the first row's id. A decision that
// releases nothing writes nothing, and a second release never starts behind the first.
func TestReleaseIdleIssueWritesItsRowsAndItsMarkTogether(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}

	kept, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{}, nil
	})
	if err != nil || kept {
		t.Fatalf("a decision that releases nothing = %t, %v; want no release", kept, err)
	}
	var rows int
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("outbox rows = %d, want none written by a decision that released nothing", rows)
	}

	var seen pods.IdleView
	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		seen = view
		return IdleReleaseMark{Generation: view.Generation, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"implementer"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	if len(seen.Claims) != 1 || seen.Claims[0].Role != claim.RoleImplementer {
		t.Fatalf("the decision saw %+v, want the issue's one claim", seen.Claims)
	}

	var ids []int64
	var kinds []string
	queued, err := st.Pool().Query(ctx, `select id, kind from outbox where issue = $1 order by id`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	for queued.Next() {
		var id int64
		var kind string
		if err := queued.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		ids, kinds = append(ids, id), append(kinds, kind)
	}
	queued.Close()
	if len(kinds) != 3 || kinds[0] != "issue_bundle" || kinds[1] != "supervise" || kinds[2] != "issue_release" ||
		ids[1] <= ids[0] || ids[2] <= ids[1] {
		t.Fatalf("queued rows = %v (%v), want the save, then the role stop, then the teardown", kinds, ids)
	}
	var first, boundary, last, generation int64
	var role string
	if err := st.Pool().QueryRow(ctx, `select first_row, boundary_row, last_row, generation, role from issue_releases
		where project='legion' and issue=$1`, rootIssue).Scan(&first, &boundary, &last, &generation, &role); err != nil {
		t.Fatal(err)
	}
	// The mark names its own rows: the save, the role stop that is its boundary, and the
	// teardown, which is how a row of this release is told from a row of one that is over.
	if first != ids[0] || boundary != ids[1] || last != ids[2] || generation != 1 || role != "implementer" {
		t.Fatalf("mark = rows %d..%d boundary %d, generation %d, role %s; want the rows %v", first, last, boundary, generation, role, ids)
	}

	// A second release never starts behind the first: the decision is asked, sees the mark, and
	// nothing more is written.
	again, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		if !view.Releasing {
			t.Error("the second decision was handed a view that does not report the release in progress")
		}
		return IdleReleaseMark{Generation: 1, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`), Boundary: true},
		}}, nil
	})
	if err != nil || again {
		t.Fatalf("a second release = %t, %v; want none", again, err)
	}
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("outbox rows = %d, want the first release's three alone", rows)
	}

	if err := st.ClearIssueRelease(ctx, "legion", rootIssue); err != nil {
		t.Fatal(err)
	}
	if cleared := idleViewOf(t, st, rootIssue); cleared.Releasing {
		t.Fatalf("view after the mark was cleared = %t, want no release in progress", cleared.Releasing)
	}
}

// IdlePods is the tick's candidate list: every issue of the project with a live claim, and no
// issue whose roles have all stopped — there is no role there to ask for a bundle and no claim to
// suspend.
func TestIdlePodsListsOnlyIssuesWithALiveClaim(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	for _, c := range []supervise.Claim{
		{Token: claim.Token("legion-legion-legion-208-implementer"), Issue: rootIssue, Role: claim.RoleImplementer, State: supervise.StateIdle},
		{Token: claim.Token("legion-legion-legion-208-tester"), Issue: rootIssue, Role: claim.RoleTester, State: supervise.StateSuspended},
		{Token: claim.Token("legion-legion-legion-209-implementer"), Issue: childIssue, Role: claim.RoleImplementer, State: supervise.StateRetired},
	} {
		c.Project, c.Tree, c.TreeEpoch, c.Generation = "legion", rootIssue, 1, 1
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := st.IdlePods(ctx, "legion")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0] != rootIssue {
		t.Fatalf("pods = %v, want %s alone", pods, rootIssue)
	}
}

// A cancel and a park of the same issue in flight together cannot end with a resume standing
// above the close. The park is the act production runs: one fact transaction that takes the
// issue out of the workflow and queues the close's own row together, under the shared
// serializer. The cancel reads the issue's standing and takes its resume's id from the same
// outbox sequence, so with no lock of its own it can read the status from before that
// transaction and still insert above the row it queued. The resume then supersedes the close:
// the close's suspension finishes without acting, the issue's Sandbox is never deleted, and the
// roles of a parked issue run on in it with their launcher tokens and `gh` credentials.
//
// The interleaving is driven, not hoped for: the park holds its transaction open with its row
// already queued while the cancel runs, which is the whole window. Without
// treelifecycle.Serialize at the head of the cancel's transaction the cancel runs straight
// through it — nothing it reads or writes meets a row the park holds — and the close stops
// acting; with it the cancel waits for the park's commit and reads the issue as one that left.
// Both outcomes are read here rather than only the wait, so the failure the lock prevents is
// reported as the end state it leaves.
func TestACancelRacingAParkLeavesNoResumeAboveTheClose(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	markRelease(t, st, first, boundary, last, 0, claim.RoleImplementer)

	park, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = park.Rollback(ctx) }()
	if err := treelifecycle.Serialize(ctx, park); err != nil {
		t.Fatal(err)
	}
	if _, err := park.Exec(ctx, `update issues set phase = 'done', status = 'backlog',
		linger_until = now() + interval '1 hour' where key = $1`, rootIssue); err != nil {
		t.Fatalf("park the root: %v", err)
	}
	var closeRow int64
	if err := park.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('issue_suspend', $1, '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), '', '')
		returning id`, rootIssue).Scan(&closeRow); err != nil {
		t.Fatalf("queue the close's own row: %v", err)
	}

	type cancelled struct {
		roles []claim.Role
		err   error
	}
	done := make(chan cancelled, 1)
	go func() {
		roles, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow)
		done <- cancelled{roles: roles, err: err}
	}()

	var got cancelled
	var finished bool
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got = <-done:
			finished = true
		default:
		}
		if finished || waitingOnTheSerializer(t, st) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancel neither finished nor waited for the park's serializer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := park.Commit(ctx); err != nil {
		t.Fatalf("commit the park: %v", err)
	}
	if !finished {
		got = <-done
	}
	if got.err != nil {
		t.Fatalf("carry the cancel out: %v", got.err)
	}

	if len(got.roles) != 0 {
		t.Fatalf("resumed = %v, want none: the claims of an issue a park took out of the workflow are its close's", got.roles)
	}
	var above int
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and id > $2`, rootIssue, closeRow).Scan(&above); err != nil {
		t.Fatal(err)
	}
	if above != 0 {
		t.Fatalf("%d resume rows stand above the close's row %d; the close is superseded, its Sandbox is never deleted, and the parked issue's roles run on in it", above, closeRow)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel left its mark behind; every later decision would read a release that is over")
	}

	// The consequence itself: the close's own suspension acts, which is what deletes the issue's
	// Sandbox and takes the pod's role Secrets with it.
	act, err := st.IssueSuspension(ctx, "legion",
		IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: closeRow},
		record.OutOfWorkflow)
	if err != nil || !act {
		t.Fatalf("the close after the race = %t, %v; want it to act", act, err)
	}
}

// waitingOnTheSerializer is whether a backend of this test's database other than the reader waits
// on an advisory lock: the shared serializer (treelifecycle.Serialize) is the only one the store
// takes by that name outside a migration, so a waiter here is a transaction held at its head.
func waitingOnTheSerializer(t *testing.T, st *Store) bool {
	t.Helper()
	var waiting bool
	if err := st.Pool().QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid()
		and wait_event_type = 'Lock' and wait_event = 'advisory')`).Scan(&waiting); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting
}
