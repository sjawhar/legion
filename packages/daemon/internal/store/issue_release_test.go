package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// its own: the ids of the release's first row, of its boundary and of its last, the newest
// start row the pod's claims held when the release was marked, and the roles whose claims the
// release's own stops have suspended so far.
func markRelease(t *testing.T, store *Store, first, boundary, last, started int64, suspended ...claim.Role) {
	t.Helper()
	roles := make([]string, 0, len(suspended))
	for _, role := range suspended {
		roles = append(roles, string(role))
	}
	_, err := store.Pool().Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('legion', $1, 1, $2, $3, $4, $5, 'architect', $6, now())`, rootIssue, first, boundary, last, started, roles)
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
		wantCause ReleaseCause
		wantWait  string
	}{
		{name: "every role released", state: supervise.StateSuspended, want: ReleaseActs},
		{name: "no mark: the release was cancelled or is over", state: supervise.StateSuspended, noMark: true, want: ReleaseOver},
		// A claim holding a process is the pod being used again, which the teardown must not
		// take: it is cancelled rather than waited on, since the bundle the save stored is
		// already older than whatever that process is doing.
		{name: "held turn", state: supervise.StateWorking, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a role resumed into the pod", state: supervise.StateIdle, wantWait: "stored claim"},
		{
			name: "the issue left the workflow", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set status='backlog' where key='LEGION-208'",
		},
		{
			name: "the issue is done", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set phase='done', status='done' where key='LEGION-208'",
		},
		{
			name: "the tree lingers", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set linger_until = now() + interval '1 hour' where key='LEGION-208'",
		},
		{
			name: "a re-admission moved the generation", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set generation=2 where key='LEGION-208'",
		},
		{
			name: "the tree's cleanup is reserved", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'",
		},
		{
			// R4's boundary never saw this one: the start ran before the role stop that would
			// have caught it, so the claim is in the pod again and the teardown would take it.
			// The cause is the one a role stop reads for the same start, so the records of a
			// deployment carry one reason and not two.
			name: "a role started again since the mark", state: supervise.StateSuspended,
			lastStart: 7, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken,
		},
		{
			name: "a start of a role of the pod is queued above the release's rows", state: supervise.StateSuspended,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken,
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
			decision, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if tc.wantWait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || decision.Act == ReleaseActs {
					t.Fatalf("release = %s, %v; want the wait %q", decision.Act, err, tc.wantWait)
				}
				return
			}
			if err != nil || decision.Act != tc.want {
				t.Fatalf("release = %s, %v; want %s", decision.Act, err, tc.want)
			}
			if decision.Cause != tc.wantCause {
				t.Fatalf("the cancel's cause = %s, want %s", decision.Cause, tc.wantCause)
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
// release at the boundary. A claim started since the mark, or one holding a process, cancels it
// at any of the release's role stops. A row no mark holds does nothing.
//
// The mark stands through every one of those answers: the cancel is carried out by
// CancelIssueRelease, which queues the resumes and deletes the mark together, and a mark
// deleted here would leave a cancel that did not finish unreadable by the row's own retry.
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
		wantCause ReleaseCause
	}{
		{name: "nothing queued", row: boundary, want: ReleaseActs},
		{name: "a start queued below the release's rows", row: boundary, queued: first - 1, kind: "supervise",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a notice queued below them", row: boundary, queued: first - 1, kind: "notice",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a phase-ended message queued below them", row: boundary, queued: first - 1, kind: "phase_ended",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a board write queued below them", row: boundary, queued: first - 1, kind: "dispatch_status", want: ReleaseActs},
		// The release's own rows are inside the range, so they are not work the pod was given.
		{name: "a role stop of the release itself", row: boundary, queued: boundary + 1, kind: "supervise", want: ReleaseActs},
		{name: "a start queued above the release's rows", row: boundary, queued: last + 1, kind: "supervise",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a notice queued above them", row: boundary, queued: last + 1, kind: "notice",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		// A later role stop reads the claims and not the queue: the first role process is
		// already stopped, so a delivery waits behind the release rather than cancelling it.
		{name: "a later role stop with a delivery queued", row: boundary + 1, queued: last + 1, kind: "notice", want: ReleaseActs},
		{name: "a start the lane already ran", row: boundary, started: 5, lastStart: 19,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a start the lane already ran, at a later role stop", row: boundary + 1, started: 5, lastStart: 19,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "the start the mark was taken at", row: boundary, started: 19, lastStart: 19, want: ReleaseActs},
		{name: "a claim in a turn", row: boundary, state: supervise.StateWorking,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "the release was already cancelled", row: boundary, noMark: true, want: ReleaseOver},
		{name: "a row of a release this mark is not", row: last + 5, want: ReleaseOver},
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
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, tc.row)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if decision.Act != tc.want || decision.Cause != tc.wantCause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", decision.Act, decision.Cause, tc.want, tc.wantCause)
			}
			var marked bool
			if err := st.Pool().QueryRow(ctx, `select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != !tc.noMark {
				t.Fatalf("the release's mark stands = %t, want %t: only the cancel itself deletes it", marked, !tc.noMark)
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

			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the boundary = %s for %s, want %s for %s: the pod was given work after the release was marked",
					decision.Act, decision.Cause, ReleaseCancel, ReleaseCauseOvertaken)
			}
			// The mark stands until the cancel is carried out, and goes with it.
			if !releaseMarked(t, st) {
				t.Fatal("the cancel was decided and the mark was already gone; a cancel that fails would be unreadable")
			}
			if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow); err != nil {
				t.Fatalf("carry the cancel out: %v", err)
			}
			if releaseMarked(t, st) {
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
	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the first release's boundary = %s, %v; want it cancelled", decision.Act, err)
	}
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow); err != nil {
		t.Fatalf("carry the first release's cancel out: %v", err)
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

	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || decision.Act != ReleaseOver {
		t.Fatalf("the cancelled release's role stop = %s, %v; want %s", decision.Act, err, ReleaseOver)
	}
	stale := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: lastA}
	if decision, err := st.IssueRelease(ctx, "legion", stale, record.OutOfWorkflow); err != nil || decision.Act != ReleaseOver {
		t.Fatalf("the cancelled release's teardown = %s, %v; want %s", decision.Act, err, ReleaseOver)
	}
	var boundary int64
	if err := st.Pool().QueryRow(ctx, `select boundary_row from issue_releases where issue = $1`, rootIssue).Scan(&boundary); err != nil {
		t.Fatalf("read the mark after the stale rows ran: %v", err)
	}
	if boundary != boundaryB {
		t.Fatalf("the mark names boundary %d, want the running release's %d", boundary, boundaryB)
	}
	// The running release's own rows still act.
	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryB); err != nil || decision.Act != ReleaseActs {
		t.Fatalf("the running release's boundary = %s, %v; want %s", decision.Act, err, ReleaseActs)
	}
}

// releaseMarked is whether a release of the root issue is marked in flight.
func releaseMarked(t *testing.T, st *Store) bool {
	t.Helper()
	var marked bool
	if err := st.Pool().QueryRow(context.Background(),
		`select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
		t.Fatalf("read whether %s is being released: %v", rootIssue, err)
	}
	return marked
}

// resumeRow is the row a cancel queues to bring one claim back, as the daemon's own builder
// writes it (outbox.releaseResume): the `start` of that role at the generation the cancel read.
func resumeRow(role claim.Role, generation uint64) (IdleReleaseRow, error) {
	return IdleReleaseRow{Kind: "supervise", Payload: []byte(fmt.Sprintf(
		`{"op":"start","tree":%q,"role":%q,"generation":%d}`, rootIssue, role, generation))}, nil
}

// releasedClaim is one claim of the root issue the daemon's release left suspended as
// `released`, which is the state a cancel resumes from.
func releasedClaim(t *testing.T, st *Store, role claim.Role) {
	t.Helper()
	if err := st.PutClaim(context.Background(), supervise.Claim{
		Token: claim.Token("legion-legion-legion-208-" + string(role)), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: role, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatalf("seed the released %s: %v", role, err)
	}
}

// startRows are the roles of the `start` rows queued for the root issue, in id order: what a
// cancel left behind to bring the pod's roles back.
func startRows(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select payload->>'role' from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
	}
	return roles
}

// A cancel decided at a role stop is not carried out by that decision: the mark stands until
// the resumes of the claims the release suspended are queued, and the two happen in one
// transaction. A mark deleted at the decision left the row's retry reading a release that is
// over — nothing to resume, nothing to undo — with the roles the release had already suspended
// `released` in a pod that still stands, which is what a resume that fails, or a daemon that
// stops between the two, produces.
func TestACancelKeepsTheMarkUntilItsResumesAreQueued(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	// A notice the pod must answer, queued above the release's own rows: the cancel at the
	// boundary that comes of it is the one every other cancel is shaped like.
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
		values ($1,'notice',$2,'{"kind":"todo","reason":"r"}',0,now(),'')`, last+1, rootIssue); err != nil {
		t.Fatal(err)
	}

	decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the boundary = %s, %v; want it cancelled", decision.Act, err)
	}
	if !releaseMarked(t, st) {
		t.Fatal("the decision deleted the mark; the cancel it decided had not been carried out")
	}
	// The row is retried — its resume failed, or the daemon stopped — and reads the same cancel.
	retried, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || retried.Act != ReleaseCancel {
		t.Fatalf("the retried stop = %s, %v; want the cancel read again", retried.Act, err)
	}

	refused := errors.New("the resume could not be encoded")
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, func(claim.Role, uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow); !errors.Is(err, refused) {
		t.Fatalf("a cancel whose resume could not be built = %v, want %v", err, refused)
	}
	if !releaseMarked(t, st) {
		t.Fatal("a cancel that queued no resume still deleted the mark")
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the refused cancel = %v, want none", queued)
	}

	resumed, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow)
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the one claim the release suspended", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued its resumes and left the mark standing")
	}
}

// The bundle a cancelled release saved goes in the cancel's own transaction, with the resumes and
// the mark. The pod that work came off is the pod the resumes bring back, with the work in its own
// workspace, so the row is a second copy of a live disk's work: left behind it would hold the
// issue's pod (workflow.IdleHoldBundleUnrestored) and wait for whatever pod the issue made next —
// an eviction's, an update round's, a park and unpark's — to restore it over everything those
// roles pushed in the meantime.
//
// One transaction is what makes that true of a cancel that fails too: a resume the store cannot
// queue leaves the mark standing and the bundle with it, so the row's retry runs the whole cancel
// again rather than going on with the bundle already gone or already kept.
func TestACancelDeletesTheBundleItsReleaseSavedInTheSameTransaction(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	var release int64
	if err := st.Pool().QueryRow(ctx, `select id from issue_releases where issue = $1`, rootIssue).Scan(&release); err != nil {
		t.Fatalf("read the id of the release of %s: %v", rootIssue, err)
	}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the cancelled release"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: time.Now().UTC(), PodUID: "pod-being-released"}
	if err := st.PutWorkspaceBundle(ctx, rootIssue, release, saved); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}

	refused := errors.New("the resume could not be encoded")
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, func(claim.Role, uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow); !errors.Is(err, refused) {
		t.Fatalf("a cancel whose resume could not be built = %v, want %v", err, refused)
	}
	if _, held, err := st.WorkspaceBundleSize(ctx, rootIssue); err != nil || !held {
		t.Fatalf("after a cancel that failed the store holds the bundle = %t (%v), want it kept with the mark", held, err)
	}

	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow); err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if _, held, err := st.WorkspaceBundleSize(ctx, rootIssue); err != nil || held {
		t.Fatalf("after the cancel the store holds the bundle = %t (%v), want it deleted with the mark", held, err)
	}
}

// A claim this release suspended that something resumed is the pod being used again, and no
// start row says so: a wake resumes a released claim by rule (supervise's RequestRuleResume),
// which moves no last_start_row and leaves the claim ready. Without reading the release's own
// suspended set the teardown would wait on that claim for good — its stored claim may hold a
// process, so the fence waits, and the mark it waits under keeps every idle tick off the pod.
func TestAClaimTheReleaseSuspendedThatSomethingResumedOvertakesIt(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
		want  ReleaseAct
		cause ReleaseCause
	}{
		{name: "still released", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, want: ReleaseActs},
		{name: "resumed by rule, ready again", state: supervise.StateReady, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		{name: "resumed by rule, idle again", state: supervise.StateIdle, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		// A close, a linger or an operator that took the claim over re-stamps the source: that
		// suspension is theirs to end, and the release is over either way.
		{name: "taken over by another path", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
				Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
				Generation: 1, State: tc.state, SuspendedBy: tc.by}); err != nil {
				t.Fatal(err)
			}

			// A later role stop of the same release, where the resumed claim is all there is to read.
			stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary+1)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if stop.Act != tc.want || stop.Cause != tc.cause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", stop.Act, stop.Cause, tc.want, tc.cause)
			}

			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
			teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if tc.want == ReleaseActs {
				if err != nil || teardown.Act != ReleaseActs {
					t.Fatalf("the teardown = %s, %v; want it to act", teardown.Act, err)
				}
				return
			}
			if err != nil || teardown.Act != ReleaseCancel || teardown.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the teardown = %s for %s, %v; want %s for %s", teardown.Act, teardown.Cause, err,
					ReleaseCancel, ReleaseCauseOvertaken)
			}
		})
	}
}

// A cancel resumes the claims its own release suspended and no others. A release that finishes
// leaves its claims `released` until a wake brings them back, so an issue being released again
// can hold claims an earlier release left: resuming those puts a role back into a pod that has
// no work for it, with no wake, no task and no record of why it is there.
func TestACancelResumesNoClaimAnEarlierReleaseSuspended(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	// The implementer was released by an earlier release and never woken; this release was
	// marked over the architect alone, which is the one role its stop suspended.
	releasedClaim(t, st, claim.RoleImplementer)
	releasedClaim(t, st, claim.RoleArchitect)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)

	resumed, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow)
	if err != nil {
		t.Fatalf("cancel the release: %v", err)
	}
	if len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the architect alone: the implementer belongs to the release before this one", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's alone", queued)
	}
}

// The mark's suspended set is written by the stops themselves, once each has suspended its
// claim, and a row of a release that is over writes nothing: it is the record of what this
// release did, which the cancel and the release's own record are read from.
func TestAReleasesMarkRecordsTheClaimsItsStopsSuspended(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0)

	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleArchitect, boundary); err != nil {
		t.Fatalf("record the architect's suspension: %v", err)
	}
	// The row ran twice: one claim, one entry.
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleArchitect, boundary); err != nil {
		t.Fatalf("record the architect's suspension again: %v", err)
	}
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary+1); err != nil {
		t.Fatalf("record the implementer's suspension: %v", err)
	}
	// A row of a release this mark is not records nothing.
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleTester, last+5); err != nil {
		t.Fatalf("record a stale row's suspension: %v", err)
	}

	decision, err := st.IssueReleaseSave(ctx, "legion", rootIssue, first)
	if err != nil {
		t.Fatalf("read the release's mark through its save: %v", err)
	}
	if len(decision.Suspended) != 2 || decision.Suspended[0] != claim.RoleArchitect || decision.Suspended[1] != claim.RoleImplementer {
		t.Fatalf("the mark's suspended roles = %v, want the architect and the implementer in the order their stops ran", decision.Suspended)
	}
	if decision.Release <= 0 {
		t.Fatalf("the decision's release id = %d, want the id the mark carries", decision.Release)
	}
}

// A wake of the pod taken after the release was marked cancels the release, whichever claim it
// was for. The wake is the fact read (one pod_resumes row above the mark's own), because what a
// wake does to a claim says too little: the outbox's wake of a delivery it holds resumes its
// role by rule (supervise's RequestRuleResume), which queues no start row and moves no
// last_start_row, and a wake for a claim an earlier release left `released` touches no claim of
// this release's suspended set. Here the architect is that claim: R1 released it, this release
// stopped the implementer alone, and a notice for the architect wakes the pod. Without the wake
// read, every claim of the pod reads as standing still, the teardown takes the pod, and the
// implementer stays suspended in a pod that is coming back for its architect.
//
// The wake taken before the mark is the mark's own high-water, so a release is never cancelled
// by a wake that was over before it began.
func TestAWakeTakenAfterTheMarkCancelsTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		woken bool
		want  ReleaseAct
		cause ReleaseCause
	}{
		{name: "no wake since the mark", want: ReleaseActs},
		{name: "a wake for a claim an earlier release left released", woken: true,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			releasedClaim(t, st, claim.RoleArchitect)
			releasedClaim(t, st, claim.RoleImplementer)
			at := time.Now().UTC()
			// The pod was woken once before this release: that wake is spent.
			if taken, err := st.TakePodWake(ctx, "legion", rootIssue, claim.RoleArchitect,
				pods.WakeDelivery, "row-1", at.Add(-time.Hour)); err != nil || !taken {
				t.Fatalf("take the wake before the mark = %t, %v; want it taken", taken, err)
			}

			first, boundary, last := releaseThroughTheStore(t, st)
			if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary); err != nil {
				t.Fatalf("record the implementer's suspension: %v", err)
			}
			if tc.woken {
				if taken, err := st.TakePodWake(ctx, "legion", rootIssue, claim.RoleArchitect,
					pods.WakeDelivery, "row-7", at); err != nil || !taken {
					t.Fatalf("take the wake after the mark = %t, %v; want it taken", taken, err)
				}
			}

			stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if stop.Act != tc.want || stop.Cause != tc.cause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", stop.Act, stop.Cause, tc.want, tc.cause)
			}
			// The lane deleted the save and the role stop as it ran them, so the teardown is the
			// release's one row left.
			if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3`,
				rootIssue, first, boundary); err != nil {
				t.Fatalf("finish the release's run rows: %v", err)
			}
			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
			teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if err != nil {
				t.Fatalf("IssueRelease: %v", err)
			}
			if teardown.Act != tc.want || teardown.Cause != tc.cause {
				t.Fatalf("the teardown = %s for %s, want %s for %s", teardown.Act, teardown.Cause, tc.want, tc.cause)
			}
		})
	}
}

// A release cancelled because its issue is no longer the workflow's queues no resume at all, and
// clears its mark all the same: those claims are the close's to end, and the close is already
// running. A resume queued for them lands above the close's own rows and supersedes them — the
// close's suspension then finishes without acting, its Sandbox is never deleted, and the roles
// of an issue that has left run on in it with their launcher tokens and `gh` credentials.
//
// The sequence is the one production runs: the release's stops have suspended the pod's claims,
// the root is parked while the save runs so the linger's rows are queued above the release's,
// and the release's teardown is the next row of the lane.
func TestACancelOfAnIssueOutOfTheWorkflowQueuesNoResumeSoItsCloseActs(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	first, boundary, last := releaseThroughTheStore(t, st)
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary); err != nil {
		t.Fatalf("record the implementer's suspension: %v", err)
	}

	// The root is parked: its tree lingers, its phase is done, and its close queues its own rows
	// above the release's (workflow's beginLinger).
	if _, err := st.Pool().Exec(ctx, `update issues set phase = 'done', status = 'backlog',
		linger_until = now() + interval '1 hour' where key = $1`, rootIssue); err != nil {
		t.Fatalf("park the root: %v", err)
	}
	var closeRow int64
	if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('issue_suspend', $1, '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), '', '')
		returning id`, rootIssue).Scan(&closeRow); err != nil {
		t.Fatalf("queue the close's own row: %v", err)
	}

	release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
	teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
	if err != nil || teardown.Act != ReleaseCancel || teardown.Cause != ReleaseCauseUnauthorized {
		t.Fatalf("the teardown = %s for %s, %v; want %s for %s", teardown.Act, teardown.Cause, err,
			ReleaseCancel, ReleaseCauseUnauthorized)
	}
	resumed, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow)
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed = %v, want none: the claims of an issue that has left are its close's", resumed)
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the cancel = %v, want none", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued no resume and left its mark behind; every later decision would read a release that is over")
	}

	// The lane deleted the release's own rows as it ran them, so the only rows left below the
	// close's are its own.
	if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3`,
		rootIssue, first, last); err != nil {
		t.Fatalf("finish the release's rows: %v", err)
	}
	act, err := st.IssueSuspension(ctx, "legion",
		IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: closeRow},
		record.OutOfWorkflow)
	if err != nil || !act {
		t.Fatalf("the close = %t, %v; want it to act: its Sandbox deletion is what takes the pod's role Secrets with it", act, err)
	}
}
