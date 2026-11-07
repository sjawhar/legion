package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releasedToken is the implementer claim of the released pod these tests read.
const releasedToken = claim.Token("legion-legion-legion-208-implementer")

// seedReleasedPod is one issue pod as a release leaves it: the running tree, and the
// implementer's claim suspended with the source `released`, its session kept.
func seedReleasedPod(t *testing.T, store *Store, session string) {
	t.Helper()
	openRunningTree(t, store)
	c := supervise.Claim{Token: releasedToken, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, Session: session,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased, PluginContract: 47}
	if err := store.PutClaim(context.Background(), c); err != nil {
		t.Fatalf("seed the released claim: %v", err)
	}
}

// inTx runs read in a transaction of the store's pool, as a fact's handler does.
func inTx(t *testing.T, store *Store, read func(pgx.Tx) error) {
	t.Helper()
	if err := store.Tx(context.Background(), read); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

// podRecords is project legion's pod tables as a caller outside the store reads and writes them.
func podRecords(t *testing.T, store *Store) *PodRecords {
	t.Helper()
	records, err := store.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	return records
}

// reopenStore is a daemon restart: a fresh pool and a fresh store over the same database, with
// nothing of the first one carried in memory. Only what was written survives it.
func reopenStore(t *testing.T, store *Store) *Store {
	t.Helper()
	reopened, err := Open(context.Background(), store.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	t.Cleanup(reopened.Close)
	return reopened
}

// A wake reads a released claim by its role token and by the session it kept, and reads neither
// once another path has taken the claim over: a suspension of any other source is somebody
// else's to resume.
func TestAReleasedClaimIsReadByItsTokenAndItsSessionUntilAnotherPathTakesItOver(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	seedReleasedPod(t, store, "session-1")

	records := podRecords(t, store)
	inTx(t, store, func(tx pgx.Tx) error {
		byToken, found, err := records.ReleasedClaimByToken(ctx, tx, string(releasedToken))
		if err != nil || !found {
			t.Fatalf("ReleasedClaimByToken = %+v, %t, %v; want the released claim", byToken, found, err)
		}
		if byToken.Issue != rootIssue || byToken.Role != claim.RoleImplementer || byToken.ServingGeneration != 1 {
			t.Fatalf("the claim read by token = %+v, want the implementer of %s at generation 1", byToken, rootIssue)
		}
		bySession, found, err := records.ReleasedClaimBySession(ctx, tx, "session-1")
		if err != nil || !found || bySession.Token != releasedToken {
			t.Fatalf("ReleasedClaimBySession = %+v, %t, %v; want the same claim", bySession, found, err)
		}
		if _, found, err := records.ReleasedClaimBySession(ctx, tx, "another-daemons-session"); err != nil || found {
			t.Fatalf("a session of no claim of this project = %t, %v; want not found", found, err)
		}
		return nil
	})

	for _, source := range []supervise.SuspendSource{supervise.SuspendedByOperator, supervise.SuspendedByDaemon} {
		if _, err := store.Pool().Exec(ctx, `update claims set suspended_by = $2 where token = $1`,
			string(releasedToken), string(source)); err != nil {
			t.Fatalf("take the claim over: %v", err)
		}
		inTx(t, store, func(tx pgx.Tx) error {
			if _, found, err := records.ReleasedClaimByToken(ctx, tx, string(releasedToken)); err != nil || found {
				t.Fatalf("a claim suspended by %s reads released = %t, %v; want not found", source, found, err)
			}
			if _, found, err := records.ReleasedClaimBySession(ctx, tx, "session-1"); err != nil || found {
				t.Fatalf("a claim suspended by %s reads released by session = %t, %v; want not found", source, found, err)
			}
			return nil
		})
	}
}

// One open resume per role is the whole of the dedupe: the second wake of a role whose pod is
// already coming back writes nothing and answers false, and the wake of another role of the same
// pod is taken, since a wake resumes one claim. A role whose resume has its first turn stamped
// is free to be woken again, which is a pod that came back, ran and was released afresh.
func TestARoleTakesOneWakeWhileItsPodIsComingBack(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	records := podRecords(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	inTx(t, store, func(tx pgx.Tx) error {
		first, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, at)
		if err != nil || !first {
			t.Fatalf("the implementer's first wake = %t, %v; want it taken", first, err)
		}
		again, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeRoleMessage, at)
		if err != nil || again {
			t.Fatalf("a second wake of the implementer = %t, %v; want it refused: its pod is coming back", again, err)
		}
		other, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleArchitect, pods.WakeRoleMessage, at)
		if err != nil || !other {
			t.Fatalf("the architect's wake = %t, %v; want it taken: a wake resumes one claim", other, err)
		}
		return nil
	})
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set first_turn_at = $1 where issue = $2 and role = 'implementer'`,
		at, rootIssue); err != nil {
		t.Fatalf("stamp the implementer's first turn: %v", err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		next, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, at)
		if err != nil || !next {
			t.Fatalf("a wake after the first turn = %t, %v; want it taken: that resume is closed", next, err)
		}
		return nil
	})
}

// Two wakes of one role in transactions of their own — the workflow's, under the apply-fact
// lock, and the outbox's delivery wake, which runs in one of its own — take once. The index
// decides it inside each transaction, where a read could be overtaken between its answer and the
// insert, so the record holds one resume and the caller writes one start row.
func TestTwoWakesOfOneRoleInSeparateTransactionsTakeOnce(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	taken := 0
	for _, kind := range []string{pods.WakeChecks, pods.WakeDelivery} {
		got, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer, kind, at)
		if err != nil {
			t.Fatalf("take the %s wake: %v", kind, err)
		}
		if got {
			taken++
		}
	}
	if taken != 1 {
		t.Fatalf("wakes taken = %d, want 1: one open resume per role", taken)
	}
	var resumes int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes where issue = $1 and role = 'implementer'`,
		rootIssue).Scan(&resumes); err != nil {
		t.Fatalf("count the resumes: %v", err)
	}
	if resumes != 1 {
		t.Fatalf("resumes recorded = %d, want 1", resumes)
	}
}

// R5: the backstop is a stored row with a due time, so it survives the daemon that armed it. A
// store opened again over the same database reads the same row due, and the conditional fire is
// what makes a backstop two passes read wake its pod once.
func TestABackstopSurvivesTheDaemonAndFiresOnce(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	armed := time.Now().UTC().Truncate(time.Millisecond)
	due := armed.Add(time.Hour)
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Role: claim.RoleImplementer, DueAt: due}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	// A release retried after a failure arms the one backstop, not two.
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Role: claim.RoleImplementer, DueAt: due.Add(time.Minute)}); err != nil {
		t.Fatalf("arm the backstop again: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", due.Add(-time.Minute)); err != nil || len(open) != 0 {
		t.Fatalf("backstops due before their time = %+v, %v; want none", open, err)
	}

	// The daemon stops and starts again: a new store over the same database, as a restart makes.
	restarted := reopenStore(t, store)
	due2, err := restarted.DuePodBackstops(ctx, "legion", due.Add(time.Minute))
	if err != nil {
		t.Fatalf("read the due backstops after the restart: %v", err)
	}
	if len(due2) != 1 || due2[0].Issue != rootIssue || due2[0].Role != claim.RoleImplementer {
		t.Fatalf("due backstops after the restart = %+v, want the one armed before it", due2)
	}
	if !due2[0].DueAt.UTC().Equal(due) {
		t.Fatalf("the backstop is due at %s, want the time the first arming set, %s", due2[0].DueAt.UTC(), due)
	}

	records := podRecords(t, restarted)
	inTx(t, restarted, func(tx pgx.Tx) error {
		fired, err := records.FirePodBackstop(ctx, tx, due2[0].ID)
		if err != nil || !fired {
			t.Fatalf("the first fire = %t, %v; want it fired", fired, err)
		}
		again, err := records.FirePodBackstop(ctx, tx, due2[0].ID)
		if err != nil || again {
			t.Fatalf("the second fire = %t, %v; want it already fired", again, err)
		}
		return nil
	})
	if open, err := restarted.DuePodBackstops(ctx, "legion", due.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops due after the fire = %+v, %v; want none", open, err)
	}
}

// A path that takes a released claim over forgets the pod's wake marks and its open backstops,
// so no later backstop acts on it.
func TestTakingAReleasedPodOverCancelsItsBackstops(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Role: claim.RoleImplementer, DueAt: at}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	if err := store.CancelPodBackstopsOf(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("cancel the pod's backstops: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the take-over = %+v, %v; want none", open, err)
	}
}

// R7: the daemon releases nothing unless it has read a `listener_ready` report within the window.
// A daemon that has read none, and one whose newest report is older than the window, both answer
// no; one inside the window answers yes.
func TestTheListenerIsLiveOnlyWithAReportInsideTheWindow(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	const window = 15 * time.Minute
	records := podRecords(t, store)

	live, _, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || found {
		t.Fatalf("with no report at all: live = %t, found = %t, %v; want neither", live, found, err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-16*time.Minute))
	})
	live, at, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || !found {
		t.Fatalf("with a sixteen-minute-old report: live = %t, found = %t, %v; want found but not live", live, found, err)
	}
	if !at.Equal(now.Add(-16 * time.Minute)) {
		t.Fatalf("the report is stamped %s, want %s", at, now.Add(-16*time.Minute))
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-5*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("with a five-minute-old report: live = %t, %v; want live", live, err)
	}
	// A report that arrives out of order never moves the stamp backwards: the reports are the
	// listener's own clock, and one delivered late must not make a live listener look dead.
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-20*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("after an out-of-order older report: live = %t, %v; want it still live", live, err)
	}
}

// TakePodWake is the outbox's wake: it records the resume against the pod's newest release and
// cancels the open backstops, once, and the resume is what makes it once.
func TestTakingAWakeRecordsItAgainstTheNewestReleaseOnce(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	release, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{"implementer"}, At: at.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("record the release: %v", err)
	}
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Role: claim.RoleImplementer, DueAt: at}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	taken, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer, pods.WakeDelivery, at)
	if err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the wake taken", taken, err)
	}
	again, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer, pods.WakeDelivery, at)
	if err != nil || again {
		t.Fatalf("a second delivery wake = %t, %v; want it refused: the role's resume is still open", again, err)
	}
	records, err := store.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	if len(records.Resumes) != 1 || records.Resumes[0].Release != release || records.Resumes[0].Kind != pods.WakeDelivery {
		t.Fatalf("resumes = %+v, want the one delivery wake of release %d", records.Resumes, release)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the wake = %+v, %v; want none: the pod is coming back", open, err)
	}
	// The resume is the one a pod-ready and a first-turn observation stamp, and it is no longer
	// open once its first turn is in.
	id, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer)
	if err != nil || !found || id != records.Resumes[0].ID {
		t.Fatalf("OpenPodResume = %d, %t, %v; want the resume the wake recorded, %d", id, found, err, records.Resumes[0].ID)
	}
	if err := store.MarkPodResumeFirstTurn(ctx, id, at.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the first turn: %v", err)
	}
	if _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer); err != nil || found {
		t.Fatalf("OpenPodResume after the first turn = %t, %v; want none", found, err)
	}
}

// A release closes the resumes of the pod it stops, and closing is its own column: the role can
// be woken again afterwards, the records report no first turn for the closed row, and the stranded
// and stuck reports pass it over, since a wake of a pod that is gone is nothing either warns
// about. Writing first_turn_at to close a row, as this branch first did, reports a turn the daemon
// never saw: the release records serve that field and the plugin measures each wake's wait by it.
func TestClosingAResumeWritesNoFirstTurnAndFreesTheRole(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{"implementer"}, At: at.Add(-time.Hour)}); err != nil {
		t.Fatalf("record the release: %v", err)
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer, pods.WakeDelivery, at); err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the wake taken", taken, err)
	}

	inTx(t, store, func(tx pgx.Tx) error {
		return CloseOpenResumes(ctx, tx, "legion", rootIssue)
	})

	if _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer); err != nil || found {
		t.Fatalf("OpenPodResume after the close = %t, %v; want none", found, err)
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer, pods.WakeBackstop, at.Add(time.Hour)); err != nil || !taken {
		t.Fatalf("the wake after the close = %t, %v; want it taken: the closed resume holds nothing back", taken, err)
	}
	records, err := store.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	if len(records.Resumes) != 2 || !records.Resumes[0].FirstTurnAt.IsZero() {
		t.Fatalf("resumes = %+v, want the closed one reported with no first turn", records.Resumes)
	}
	stuck, err := store.StuckPodResumes(ctx, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the stuck resumes: %v", err)
	}
	stranded, err := store.UnfinishedPodResumes(ctx, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the unfinished resumes: %v", err)
	}
	for _, read := range []struct {
		what    string
		resumes []PodResume
	}{{"stuck", stuck}, {"stranded", stranded}} {
		for _, r := range read.resumes {
			if r.ID == records.Resumes[0].ID {
				t.Errorf("the %s report lists the closed resume %d, want it passed over", read.what, r.ID)
			}
		}
	}
}

// R5: a wake resumes one role, so it cancels that role's backstop and no other's. A released
// tester waiting on a deploy run has nothing but its own backstop to bring it back, and a pod
// woken for its architect may be held again — by an unanswered question to a person, by
// background work — before it ever reaches the tester's work.
func TestAWakeCancelsTheWokenRolesBackstopAlone(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
			Role: role, DueAt: at.Add(time.Hour)}); err != nil {
			t.Fatalf("arm the %s backstop: %v", role, err)
		}
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, claim.RoleImplementer,
		pods.WakeRoleMessage, at); err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the implementer's wake taken", taken, err)
	}

	open, err := store.DuePodBackstops(ctx, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the open backstops: %v", err)
	}
	if len(open) != 1 || open[0].Role != claim.RoleTester {
		t.Fatalf("open backstops after the implementer's wake = %+v, want the tester's alone", open)
	}

	// A path that takes the whole released pod over leaves none of them: nothing of that pod is
	// a wake's any more.
	if err := store.CancelPodBackstopsOf(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("cancel the pod's backstops: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(2*time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("open backstops after the take-over = %+v, %v; want none", open, err)
	}
}
