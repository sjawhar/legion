package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

	inTx(t, store, func(tx pgx.Tx) error {
		byToken, found, err := ReleasedClaimByToken(ctx, tx, "legion", string(releasedToken))
		if err != nil || !found {
			t.Fatalf("ReleasedClaimByToken = %+v, %t, %v; want the released claim", byToken, found, err)
		}
		if byToken.Issue != rootIssue || byToken.Role != claim.RoleImplementer || byToken.ServingGeneration != 1 {
			t.Fatalf("the claim read by token = %+v, want the implementer of %s at generation 1", byToken, rootIssue)
		}
		bySession, found, err := ReleasedClaimBySession(ctx, tx, "legion", "session-1")
		if err != nil || !found || bySession.Token != releasedToken {
			t.Fatalf("ReleasedClaimBySession = %+v, %t, %v; want the same claim", bySession, found, err)
		}
		if _, found, err := ReleasedClaimBySession(ctx, tx, "legion", "another-daemons-session"); err != nil || found {
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
			if _, found, err := ReleasedClaimByToken(ctx, tx, "legion", string(releasedToken)); err != nil || found {
				t.Fatalf("a claim suspended by %s reads released = %t, %v; want not found", source, found, err)
			}
			if _, found, err := ReleasedClaimBySession(ctx, tx, "legion", "session-1"); err != nil || found {
				t.Fatalf("a claim suspended by %s reads released by session = %t, %v; want not found", source, found, err)
			}
			return nil
		})
	}
}

// A wake mark is taken once per key, which is what makes one head that settles again as each of
// its checks finishes wake a pod once; a different key is a different wake.
func TestAWakeMarkIsTakenOncePerKey(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	inTx(t, store, func(tx pgx.Tx) error {
		first, err := MarkPodWake(ctx, tx, "legion", rootIssue, WakeChecks, "head1", at)
		if err != nil || !first {
			t.Fatalf("the first mark of head1 = %t, %v; want it taken", first, err)
		}
		again, err := MarkPodWake(ctx, tx, "legion", rootIssue, WakeChecks, "head1", at)
		if err != nil || again {
			t.Fatalf("the second mark of head1 = %t, %v; want it already taken", again, err)
		}
		other, err := MarkPodWake(ctx, tx, "legion", rootIssue, WakeChecks, "head2", at)
		if err != nil || !other {
			t.Fatalf("the mark of head2 = %t, %v; want it taken", other, err)
		}
		return nil
	})
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

	inTx(t, restarted, func(tx pgx.Tx) error {
		fired, err := FirePodBackstop(ctx, tx, due2[0].ID)
		if err != nil || !fired {
			t.Fatalf("the first fire = %t, %v; want it fired", fired, err)
		}
		again, err := FirePodBackstop(ctx, tx, due2[0].ID)
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
// so no later wake acts on it.
func TestForgettingAPodsWakesDropsItsMarksAndItsBackstops(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Role: claim.RoleImplementer, DueAt: at}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		if _, err := MarkPodWake(ctx, tx, "legion", rootIssue, WakeChecks, "head1", at); err != nil {
			t.Fatalf("mark the wake: %v", err)
		}
		return ForgetPodWakes(ctx, tx, "legion", rootIssue)
	})
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the take-over = %+v, %v; want none", open, err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		first, err := MarkPodWake(ctx, tx, "legion", rootIssue, WakeChecks, "head1", at)
		if err != nil || !first {
			t.Fatalf("the mark of head1 after the take-over = %t, %v; want the key free again", first, err)
		}
		return nil
	})
}

// R7: the daemon releases nothing unless it has read a `listener_ready` report within the window.
// A daemon that has read none, and one whose newest report is older than the window, both answer
// no; one inside the window answers yes.
func TestTheListenerIsLiveOnlyWithAReportInsideTheWindow(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	const window = 15 * time.Minute

	live, _, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || found {
		t.Fatalf("with no report at all: live = %t, found = %t, %v; want neither", live, found, err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return RecordListenerReady(ctx, tx, "legion", now.Add(-16*time.Minute))
	})
	live, at, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || !found {
		t.Fatalf("with a sixteen-minute-old report: live = %t, found = %t, %v; want found but not live", live, found, err)
	}
	if !at.Equal(now.Add(-16 * time.Minute)) {
		t.Fatalf("the report is stamped %s, want %s", at, now.Add(-16*time.Minute))
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return RecordListenerReady(ctx, tx, "legion", now.Add(-5*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("with a five-minute-old report: live = %t, %v; want live", live, err)
	}
	// A report that arrives out of order never moves the stamp backwards: the reports are the
	// listener's own clock, and one delivered late must not make a live listener look dead.
	inTx(t, store, func(tx pgx.Tx) error {
		return RecordListenerReady(ctx, tx, "legion", now.Add(-20*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("after an out-of-order older report: live = %t, %v; want it still live", live, err)
	}
}

// TakePodWake is the outbox's wake: it marks, records the resume against the pod's newest
// release and cancels the open backstops, once.
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
	id, taken, err := store.TakePodWake(ctx, "legion", rootIssue, "implementer", WakeDelivery, "row-7", at)
	if err != nil || !taken || id == 0 {
		t.Fatalf("TakePodWake = %d, %t, %v; want the wake taken", id, taken, err)
	}
	_, again, err := store.TakePodWake(ctx, "legion", rootIssue, "implementer", WakeDelivery, "row-7", at)
	if err != nil || again {
		t.Fatalf("the second take of row-7 = %t, %v; want it already taken", again, err)
	}
	records, err := store.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	if len(records.Resumes) != 1 || records.Resumes[0].Release != release || records.Resumes[0].Kind != WakeDelivery {
		t.Fatalf("resumes = %+v, want the one delivery wake of release %d", records.Resumes, release)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the wake = %+v, %v; want none: the pod is coming back", open, err)
	}
	// The resume is the one a pod-ready and a first-turn observation stamp, and it is no longer
	// open once its first turn is in.
	got, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer)
	if err != nil || !found || got != id {
		t.Fatalf("OpenPodResume = %d, %t, %v; want %d", got, found, err, id)
	}
	if err := store.MarkPodResumeFirstTurn(ctx, id, at.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the first turn: %v", err)
	}
	if _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer); err != nil || found {
		t.Fatalf("OpenPodResume after the first turn = %t, %v; want none", found, err)
	}
}
