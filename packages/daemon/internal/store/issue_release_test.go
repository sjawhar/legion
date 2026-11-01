package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// its own: the release's boundary row, and the newest start row the pod's claims held when the
// release was marked.
func markRelease(t *testing.T, store *Store, boundary, started int64) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, boundary_row, started_row, role, marked_at)
		values ('legion', $1, 1, $2, $3, 'architect', now())`, rootIssue, boundary, started)
	if err != nil {
		t.Fatalf("mark the release of %s: %v", rootIssue, err)
	}
}

// IssueRelease authorizes the pod of an issue that is IN the workflow — the mirror of
// IssueSuspension, which authorizes one that has left it — and it refuses or waits on the same
// fences in the same order: generations unmoved, the tree's cleanup unreserved, a wait while one
// of the release's own role stops is unrun or a stored claim may still hold a process.
func TestIssueReleaseAuthorizesAnIssueInTheWorkflowAndWaitsForAllStoredRoles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   string
		state    supervise.ClaimState
		noMark   bool
		wantAct  bool
		wantWait string
	}{
		{name: "every role released", state: supervise.StateSuspended, wantAct: true},
		{name: "no mark: the release was cancelled or is over", state: supervise.StateSuspended, noMark: true},
		{name: "held turn", state: supervise.StateWorking, wantWait: "stored claim"},
		{name: "a role resumed into the pod", state: supervise.StateIdle, wantWait: "stored claim"},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, wantWait: "stored claim"},
		{
			name: "the issue left the workflow", state: supervise.StateSuspended,
			change: "update issues set status='backlog' where key='LEGION-208'",
		},
		{
			name: "the issue is done", state: supervise.StateSuspended,
			change: "update issues set phase='done', status='done' where key='LEGION-208'",
		},
		{
			name: "the tree lingers", state: supervise.StateSuspended,
			change: "update issues set linger_until = now() + interval '1 hour' where key='LEGION-208'",
		},
		{
			name: "a re-admission moved the generation", state: supervise.StateSuspended,
			change: "update issues set generation=2 where key='LEGION-208'",
		},
		{
			name: "the tree's cleanup is reserved", state: supervise.StateSuspended,
			change: "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'",
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
				markRelease(t, st, 8, 0)
			}
			c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
				Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: tc.state,
				SuspendedBy: supervise.SuspendedByReleased}
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
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || act {
					t.Fatalf("release = %t, %v; want the wait %q", act, err, tc.wantWait)
				}
				return
			}
			if err != nil || act != tc.wantAct {
				t.Fatalf("release = %t, %v; want act %t", act, err, tc.wantAct)
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
// IssueReleaseStop is the one place it is read. A task, notice, message or start queued below
// that row was queued while every role still ran, so it cancels the release and the mark goes;
// one above it waits behind the release on the lane, so nothing here reads it. A start below the
// boundary the lane already ran leaves a claim started above the mark's started_row, and cancels
// it in the same place. Every later role stop of a cancelled release finishes without acting.
func TestTheReleasesBoundaryCancelsOnWhatWasQueuedBeforeItAndNotAfter(t *testing.T) {
	const boundary = 20
	for _, tc := range []struct {
		name      string
		row       int64
		queued    int64
		kind      string
		started   int64
		lastStart int64
		noMark    bool
		wantAct   bool
		wantMk    bool
	}{
		{name: "nothing queued", row: boundary, wantAct: true, wantMk: true},
		{name: "a start queued before the boundary", row: boundary, queued: 19, kind: "supervise"},
		{name: "a notice queued before the boundary", row: boundary, queued: 19, kind: "notice"},
		{name: "a phase-ended message queued before the boundary", row: boundary, queued: 19, kind: "phase_ended"},
		{name: "a board write queued before the boundary", row: boundary, queued: 19, kind: "dispatch_status", wantAct: true, wantMk: true},
		{name: "a start queued after the boundary", row: boundary, queued: 21, kind: "supervise", wantAct: true, wantMk: true},
		{name: "a later role stop of the release", row: boundary + 1, queued: 19, kind: "supervise", wantAct: true, wantMk: true},
		{name: "a start below the boundary already ran", row: boundary, started: 5, lastStart: 19},
		{name: "the start the mark was taken at", row: boundary, started: 19, lastStart: 19, wantAct: true, wantMk: true},
		{name: "the release was already cancelled", row: boundary, noMark: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if !tc.noMark {
				markRelease(t, st, boundary, tc.started)
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
			if tc.lastStart != 0 {
				if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
					Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
					Generation: 1, State: supervise.StateIdle, LastStartRow: tc.lastStart}); err != nil {
					t.Fatal(err)
				}
			}
			act, err := st.IssueReleaseStop(ctx, "legion", rootIssue, tc.row)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if act != tc.wantAct {
				t.Fatalf("the role stop acts = %t, want %t", act, tc.wantAct)
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
