package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// seedRunningRoot is the state an idle release binds to, and the opposite of a close's: the
// root's issue row in the workflow at generation, not lingering.
func seedRunningRoot(t *testing.T, store *Store, generation int) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'implementing', $2, 'in_progress', 'A', 1)
		on conflict (key) do update set generation = excluded.generation, status = excluded.status,
			phase = excluded.phase, linger_until = null`, rootIssue, generation)
	if err != nil {
		t.Fatalf("seed running root: %v", err)
	}
}

// openRunningTree is a workflow tree admitted at epoch one whose root is still running: the one
// shape a release authorizes and a close does not.
func openRunningTree(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.OpenTreeLifecycle(context.Background(), "legion", rootIssue, "workflow"); err != nil {
		t.Fatalf("open lifecycle of %s: %v", rootIssue, err)
	}
	seedRunningRoot(t, store, 1)
}

// markRelease is the mark ReleaseIdleIssue leaves, written by hand so a fence can be tested on
// its own: the ids of the release's first row, of its boundary and of its last, and the newest
// start row the pod's claims held when the release was marked.
func markRelease(t *testing.T, store *Store, first, boundary, last, started int64) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, marked_at)
		values ('legion', $1, 1, $2, $3, $4, $5, 'architect', now())`, rootIssue, first, boundary, last, started)
	if err != nil {
		t.Fatalf("mark the release of %s: %v", rootIssue, err)
	}
}

// IssueRelease authorizes the pod of an issue that is IN the workflow — the mirror of
// IssueSuspension, which authorizes one that has left it — and it refuses or waits on the same
// fences in the same order: generations unmoved, the pod not taken up again, the tree's cleanup
// unreserved, a wait while one of the release's own role stops is unrun or a stored claim may
// still hold a process.
func TestIssueReleaseAuthorizesAnIssueInTheWorkflowAndWaitsForAllStoredRoles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    string
		state     supervise.ClaimState
		lastStart int64
		noMark    bool
		want      ReleaseAct
		wantWait  string
	}{
		{name: "every role released", state: supervise.StateSuspended, want: ReleaseActs},
		{name: "no mark: the release was cancelled or is over", state: supervise.StateSuspended, noMark: true, want: ReleaseOver},
		// A claim holding a process is the pod being used again, which the teardown must not
		// take: it is cancelled rather than waited on, since the bundle the save stored is
		// already older than whatever that process is doing.
		{name: "held turn", state: supervise.StateWorking, want: ReleaseCancel},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, want: ReleaseCancel},
		{name: "a role resumed into the pod", state: supervise.StateIdle, wantWait: "stored claim"},
		{
			name: "the issue left the workflow", state: supervise.StateSuspended, want: ReleaseCancel,
			change: "update issues set status='backlog' where key='LEGION-208'",
		},
		{
			name: "the issue is done", state: supervise.StateSuspended, want: ReleaseCancel,
			change: "update issues set phase='done', status='done' where key='LEGION-208'",
		},
		{
			name: "the tree lingers", state: supervise.StateSuspended, want: ReleaseCancel,
			change: "update issues set linger_until = now() + interval '1 hour' where key='LEGION-208'",
		},
		{
			name: "a re-admission moved the generation", state: supervise.StateSuspended, want: ReleaseCancel,
			change: "update issues set generation=2 where key='LEGION-208'",
		},
		{
			name: "the tree's cleanup is reserved", state: supervise.StateSuspended, want: ReleaseCancel,
			change: "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'",
		},
		{
			// R4's boundary never saw this one: the start ran before the role stop that would
			// have caught it, so the claim is in the pod again and the teardown would take it.
			name: "a role started again since the mark", state: supervise.StateSuspended,
			lastStart: 7, want: ReleaseCancel,
		},
		{
			name: "a start of a role of the pod is queued above the release's rows", state: supervise.StateSuspended,
			want: ReleaseCancel,
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(30,'supervise','LEGION-208','{"op":"start","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
		{
			name: "a role stop of the release is still unrun", state: supervise.StateSuspended, wantWait: "role stop effects",
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(9,'supervise','LEGION-208','{"op":"release","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
		{
			name: "a close's suspend of the same run is still unrun", state: supervise.StateSuspended, wantWait: "role stop effects",
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(9,'supervise','LEGION-208','{"op":"suspend","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if !tc.noMark {
				markRelease(t, st, 5, 6, 10, 0)
			}
			c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
				Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: tc.state,
				LastStartRow: tc.lastStart, SuspendedBy: supervise.SuspendedByReleased}
			if err := st.PutClaim(ctx, c); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := st.Pool().Exec(ctx, tc.change); err != nil {
					t.Fatal(err)
				}
			}
			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}
			act, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if tc.wantWait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || act == ReleaseActs {
					t.Fatalf("release = %s, %v; want the wait %q", act, err, tc.wantWait)
				}
				return
			}
			if err != nil || act != tc.want {
				t.Fatalf("release = %s, %v; want %s", act, err, tc.want)
			}
		})
	}
}

// A close and a release order against each other: each waits on the other's role stops, which is
// what adding 'release' to both op fences buys. A release row below a close's own row makes the
// close wait, exactly as one of its own suspends would.
func TestACloseWaitsForAReleasesRoleStops(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, st)
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
		(9,'supervise','LEGION-208','{"op":"release","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`); err != nil {
		t.Fatal(err)
	}
	act, err := st.IssueSuspension(ctx, "legion", IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "role stop effects") || act {
		t.Fatalf("suspension = %t, %v; want it to wait for the release's role stop", act, err)
	}
}

// The release's boundary is the row that stops the first role process of the pod, and
// IssueReleaseStop is the one place it is read. Every row the release queued lies between the
// mark's first_row and its last_row, so a task, notice, message or start of the issue outside
// that range — queued by a transaction already in flight when the mark was written, or queued
// once the mark stood — reached a pod every role of which was still running: it cancels the
// release at the boundary and the mark goes. A claim started since the mark, or one holding a
// process, cancels it at any of the release's role stops. A row no mark holds does nothing.
func TestTheReleasesBoundaryCancelsOnWorkThePodWasGivenAfterTheMark(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	for _, tc := range []struct {
		name      string
		row       int64
		queued    int64
		kind      string
		state     supervise.ClaimState
		started   int64
		lastStart int64
		noMark    bool
		want      ReleaseAct
		wantMk    bool
	}{
		{name: "nothing queued", row: boundary, want: ReleaseActs, wantMk: true},
		{name: "a start queued below the release's rows", row: boundary, queued: first - 1, kind: "supervise", want: ReleaseCancel},
		{name: "a notice queued below them", row: boundary, queued: first - 1, kind: "notice", want: ReleaseCancel},
		{name: "a phase-ended message queued below them", row: boundary, queued: first - 1, kind: "phase_ended", want: ReleaseCancel},
		{name: "a board write queued below them", row: boundary, queued: first - 1, kind: "dispatch_status", want: ReleaseActs, wantMk: true},
		// The release's own rows are inside the range, so they are not work the pod was given.
		{name: "a role stop of the release itself", row: boundary, queued: boundary + 1, kind: "supervise", want: ReleaseActs, wantMk: true},
		{name: "a start queued above the release's rows", row: boundary, queued: last + 1, kind: "supervise", want: ReleaseCancel},
		{name: "a notice queued above them", row: boundary, queued: last + 1, kind: "notice", want: ReleaseCancel},
		// A later role stop reads the claims and not the queue: the first role process is
		// already stopped, so a delivery waits behind the release rather than cancelling it.
		{name: "a later role stop with a delivery queued", row: boundary + 1, queued: last + 1, kind: "notice", want: ReleaseActs, wantMk: true},
		{name: "a start the lane already ran", row: boundary, started: 5, lastStart: 19, want: ReleaseCancel},
		{name: "a start the lane already ran, at a later role stop", row: boundary + 1, started: 5, lastStart: 19, want: ReleaseCancel},
		{name: "the start the mark was taken at", row: boundary, started: 19, lastStart: 19, want: ReleaseActs, wantMk: true},
		{name: "a claim in a turn", row: boundary, state: supervise.StateWorking, want: ReleaseCancel},
		{name: "the release was already cancelled", row: boundary, noMark: true, want: ReleaseOver},
		{name: "a row of a release this mark is not", row: last + 5, want: ReleaseOver, wantMk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if !tc.noMark {
				markRelease(t, st, first, boundary, last, tc.started)
			}
			if tc.queued != 0 {
				payload := `{"op":"start","tree":"LEGION-208","role":"architect","generation":1}`
				if tc.kind != "supervise" {
					payload = `{"kind":"todo","reason":"r"}`
				}
				if tc.kind == "dispatch_status" {
					payload = `{"status":"in_progress","observedStatus":"todo"}`
				}
				if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
					values ($1,$2,'LEGION-208',$3,0,now(),'')`, tc.queued, tc.kind, payload); err != nil {
					t.Fatal(err)
				}
			}
			if tc.lastStart != 0 || tc.state != "" {
				state := tc.state
				if state == "" {
					state = supervise.StateIdle
				}
				if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
					Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
					Generation: 1, State: state, LastStartRow: tc.lastStart}); err != nil {
					t.Fatal(err)
				}
			}
			act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, tc.row)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if act != tc.want {
				t.Fatalf("the role stop = %s, want %s", act, tc.want)
			}
			var marked bool
			if err := st.Pool().QueryRow(ctx, `select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != tc.wantMk {
				t.Fatalf("the release's mark stands = %t, want %t", marked, tc.wantMk)
			}
		})
	}
}

// releaseThroughTheStore marks a release of the root issue the way the idle tick does — the three
// rows in one transaction with the mark that records them (ReleaseIdleIssue) — and answers the
// ids the mark holds: the save, the role stop that is its boundary, and the teardown.
func releaseThroughTheStore(t *testing.T, st *Store) (first, boundary, last int64) {
	t.Helper()
	ctx := context.Background()
	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: 1, Role: claim.RoleArchitect, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"architect","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	if err := st.Pool().QueryRow(ctx, `select first_row, boundary_row, last_row from issue_releases
		where project = 'legion' and issue = $1`, rootIssue).Scan(&first, &boundary, &last); err != nil {
		t.Fatalf("read the release's mark: %v", err)
	}
	return first, boundary, last
}

// R4's boundary, driven the way production drives it: the mark and the release's rows are
// written in one transaction, so every row the release queued has an id above the boundary and
// a delivery that reaches the pod after the mark has one above them all. Until the first role
// process of the pod is stopped, such a delivery — and a turn that started without one, which
// shows as a claim holding a process — cancels the whole release, because the bundle the save
// stored is older than what the pod has done since.
func TestWorkThePodIsGivenAfterTheMarkCancelsTheReleaseAtItsBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		payload string
		state   supervise.ClaimState
	}{
		{name: "a task queued for a role of the pod", kind: "supervise",
			payload: `{"op":"start","tree":"LEGION-208","role":"tester","generation":1}`},
		{name: "an architect's notice", kind: "notice", payload: `{"kind":"todo","reason":"r"}`},
		{name: "a phase's end message", kind: "phase_ended", payload: `{"kind":"todo","reason":"r"}`},
		{name: "a turn a message started", state: supervise.StateWorking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			token := claim.Token("legion-legion-legion-208-architect")
			live := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
				Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}
			if err := st.PutClaim(ctx, live); err != nil {
				t.Fatal(err)
			}
			_, boundary, _ := releaseThroughTheStore(t, st)

			// The delivery takes its id from the outbox's own sequence, as every writer of the
			// outbox does, so it lands above every row the release queued.
			if tc.kind != "" {
				if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
					values ($1, $2, $3, 0, now(), '')`, tc.kind, rootIssue, tc.payload); err != nil {
					t.Fatal(err)
				}
			}
			if tc.state != "" {
				live.State = tc.state
				if err := st.PutClaim(ctx, live); err != nil {
					t.Fatal(err)
				}
			}

			act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if act != ReleaseCancel {
				t.Fatalf("the boundary = %s, want %s: the pod was given work after the release was marked", act, ReleaseCancel)
			}
			var marked bool
			if err := st.Pool().QueryRow(ctx, `select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked {
				t.Fatal("the cancelled release left its mark behind")
			}
		})
	}
}

// A release's rows are still on the lane after it is cancelled, and the pod can be released
// again before they run. Such a row names a release the mark is not, and acting on it would
// suspend a claim of the live release, or tear its pod down and delete the bundle it saved. The
// mark holds the ids of its own rows, and a row outside them does nothing at all.
func TestARowOfACancelledReleaseNeitherActsNorCancelsTheReleaseRunningNow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-architect")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}

	firstA, boundaryA, lastA := releaseThroughTheStore(t, st)
	if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('notice', $1, '{"kind":"todo","reason":"r"}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || act != ReleaseCancel {
		t.Fatalf("the first release's boundary = %s, %v; want it cancelled", act, err)
	}
	// The notice is delivered and the pod goes quiet again, so a second release is marked. Its
	// rows are a second set, and the first release's are still queued behind them.
	if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	firstB, boundaryB, lastB := releaseThroughTheStore(t, st)
	if firstB <= lastA {
		t.Fatalf("the second release's rows are %d..%d, want them above the first's %d..%d", firstB, lastB, firstA, lastA)
	}

	if act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || act != ReleaseOver {
		t.Fatalf("the cancelled release's role stop = %s, %v; want %s", act, err, ReleaseOver)
	}
	stale := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: lastA}
	if act, err := st.IssueRelease(ctx, "legion", stale, record.OutOfWorkflow); err != nil || act != ReleaseOver {
		t.Fatalf("the cancelled release's teardown = %s, %v; want %s", act, err, ReleaseOver)
	}
	var boundary int64
	if err := st.Pool().QueryRow(ctx, `select boundary_row from issue_releases where issue = $1`, rootIssue).Scan(&boundary); err != nil {
		t.Fatalf("read the mark after the stale rows ran: %v", err)
	}
	if boundary != boundaryB {
		t.Fatalf("the mark names boundary %d, want the running release's %d", boundary, boundaryB)
	}
	// The running release's own rows still act.
	if act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryB); err != nil || act != ReleaseActs {
		t.Fatalf("the running release's boundary = %s, %v; want %s", act, err, ReleaseActs)
	}
}
