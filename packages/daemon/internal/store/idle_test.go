package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
	reported := c
	reported.ActivityAt = now.Add(-40 * time.Minute)
	reported.ActivityBackground = true
	reported.ActivityKinds = []string{"async-job", "subagent"}
	reported.ActivityNextReminderAt = now.Add(-5 * time.Minute)
	if wrote, err := PutClaimActivity(ctx, st.Pool(), reported); err != nil || !wrote {
		t.Fatalf("PutClaimActivity = %t, %v", wrote, err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error) values
		(31, 'supervise', $1, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now(), ''),
		(32, 'dispatch_status', $1, '{"status":"testing","observedStatus":"in_progress"}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}

	view, err := st.IdleView(ctx, "legion", rootIssue)
	if err != nil {
		t.Fatalf("IdleView: %v", err)
	}
	if !view.Found || view.Tree != rootIssue || view.Phase != phase.Implementing || view.Generation != 1 || view.Status != "in_progress" {
		t.Fatalf("view = %+v, want the running issue", view)
	}
	if !view.IsRoot() || view.Root.Key != rootIssue || view.Root.AwaitingPerson || view.Root.Lingering {
		t.Fatalf("root = %+v, want the running root", view.Root)
	}
	if len(view.Claims) != 1 {
		t.Fatalf("claims = %+v, want the one claim", view.Claims)
	}
	got := view.Claims[0]
	if got.Token != token || got.Role != claim.RoleImplementer || got.State != supervise.StateIdle || got.PluginContract != 47 || got.ServingGeneration != 1 {
		t.Fatalf("claim = %+v, want the stored implementer", got)
	}
	if !got.Live() {
		t.Fatalf("claim %+v reads as not live", got)
	}
	if !got.ActivityAt.Equal(reported.ActivityAt) || !got.ActivityBackground ||
		len(got.ActivityKinds) != 2 || got.ActivityKinds[0] != "async-job" || got.ActivityKinds[1] != "subagent" ||
		!got.ActivityNextReminderAt.Equal(reported.ActivityNextReminderAt) {
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
	// The start waits for a role of the pod; the Dispatch status write reaches no session.
	if len(view.Waiting) != 1 || view.Waiting[0].ID != 31 || view.Waiting[0].Kind != "supervise" || view.Waiting[0].Role != claim.RoleTester {
		t.Fatalf("waiting = %+v, want the queued start alone", view.Waiting)
	}
	if view.Releasing {
		t.Fatal("the view reports a release in progress with no mark written")
	}

	markRelease(t, st, 31, 0)
	marked, err := st.IdleView(ctx, "legion", rootIssue)
	if err != nil || !marked.Releasing {
		t.Fatalf("view after the mark = %+v, %v; want it releasing", marked.Releasing, err)
	}
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
	reported := c
	reported.ActivityAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := PutClaimActivity(ctx, st.Pool(), reported); err != nil {
		t.Fatal(err)
	}
	// The machine, which never read the report, persists its next state.
	c.State = supervise.StateIdle
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	view, err := st.IdleView(ctx, "legion", rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Claims[0]; !got.ActivityAt.Equal(reported.ActivityAt) {
		t.Fatalf("activity_at = %s after the machine's write, want the report's %s", got.ActivityAt, reported.ActivityAt)
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

	kept, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(IdleView) (IdleReleaseMark, error) {
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

	var seen IdleView
	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view IdleView) (IdleReleaseMark, error) {
		seen = view
		return IdleReleaseMark{Generation: view.Generation, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`)},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"implementer"}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	if len(seen.Claims) != 1 || seen.Claims[0].Token != token {
		t.Fatalf("the decision saw %+v, want the issue's one claim", seen.Claims)
	}

	var first, second int64
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
		if first == 0 {
			first = id
		} else {
			second = id
		}
		kinds = append(kinds, kind)
	}
	queued.Close()
	if len(kinds) != 2 || kinds[0] != "supervise" || kinds[1] != "issue_release" || second <= first {
		t.Fatalf("queued rows = %v (%d, %d), want the role stop then the teardown", kinds, first, second)
	}
	var boundary, generation int64
	var role string
	if err := st.Pool().QueryRow(ctx, `select boundary_row, generation, role from issue_releases where project='legion' and issue=$1`,
		rootIssue).Scan(&boundary, &generation, &role); err != nil {
		t.Fatal(err)
	}
	if boundary != first || generation != 1 || role != "implementer" {
		t.Fatalf("mark = row %d, generation %d, role %s; want the first row %d", boundary, generation, role, first)
	}

	// A second release never starts behind the first: the decision is asked, sees the mark, and
	// nothing more is written.
	again, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view IdleView) (IdleReleaseMark, error) {
		if !view.Releasing {
			t.Error("the second decision was handed a view that does not report the release in progress")
		}
		return IdleReleaseMark{Generation: 1, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`)},
		}}, nil
	})
	if err != nil || again {
		t.Fatalf("a second release = %t, %v; want none", again, err)
	}
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("outbox rows = %d, want the first release's two alone", rows)
	}

	if err := st.ClearIssueRelease(ctx, "legion", rootIssue); err != nil {
		t.Fatal(err)
	}
	cleared, err := st.IdleView(ctx, "legion", rootIssue)
	if err != nil || cleared.Releasing {
		t.Fatalf("view after the mark was cleared = %t, %v; want no release in progress", cleared.Releasing, err)
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
