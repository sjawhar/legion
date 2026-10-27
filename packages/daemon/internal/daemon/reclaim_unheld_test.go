package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reclaimArchitect creates issue's architect claim in state, registered as session, with a session
// file so a relaunch Resumes that session rather than spawning a fresh one.
func reclaimArchitect(t *testing.T, sup *supervisor, issue string, state supervise.ClaimState, session string) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, claim.RoleArchitect)
	c := supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: issue, Role: claim.RoleArchitect,
		State: state, Session: session, SessionFile: "/sessions/" + session + ".jsonl",
	}
	if state == supervise.StateFailed {
		c.Budgets = supervise.Budgets{Deaths: 3, LaunchFailures: 3}
	}
	if state == supervise.StateSuspended {
		c.SuspendedBy = supervise.SuspendedByOperator
	}
	if _, _, err := sup.Create(context.Background(), c, ""); err != nil {
		t.Fatalf("create the architect claim of %s: %v", issue, err)
	}
	return token
}

func reclaimNotice() record.Notice {
	return record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
}

// (1) The 212 shape: a daemon notice publish to a running architect's role topic returns
// ErrNoHolder (the listener has no holder for the role), so the pane is relaunched on its session
// to re-claim, and the notice is held for the copy the re-claim lets land. Red on main: before the
// change the notice is held and nothing relaunches the deaf pane.
func TestAnUnheldRunningArchitectsNoticePublishRelaunchesItToReclaim(t *testing.T) {
	for _, state := range []supervise.ClaimState{supervise.StateReady, supervise.StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			token := reclaimArchitect(t, sup, "LEGION-1", state, "ses_arch")
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
				notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", reclaimNotice(), time.Now()))

			err := runner.notice(context.Background(), row, reclaimNotice())

			if !errors.Is(err, errNoticeWaits) {
				t.Fatalf("notice() = %v, want the notice held (errNoticeWaits)", err)
			}
			if got := len(rt.CallsOf("Resume")); got != 1 {
				t.Fatalf("resumes = %d, want the deaf architect relaunched once on its session", got)
			}
			machine, _ := sup.Machine(token)
			if machine.View().State != supervise.StateLaunching {
				t.Fatalf("architect state = %s, want launching after the re-claim relaunch", machine.View().State)
			}
		})
	}
}

// (3) The recognizer dispatches on the claim's state: only a ready or idle pane is relaunched; a
// working pane (its turn would be lost), an operator- or daemon-suspended claim, a failed or
// retired claim is left alone. Red on main: the ready and idle rows want one relaunch and get none.
func TestReclaimOfADeafArchitectDispatchesOnItsState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   supervise.ClaimState
		resumes int
	}{
		{"ready is relaunched", supervise.StateReady, 1},
		{"idle is relaunched", supervise.StateIdle, 1},
		{"working waits for its turn", supervise.StateWorking, 0},
		{"operator-suspended is left alone", supervise.StateSuspended, 0},
		{"failed is left alone", supervise.StateFailed, 0},
		{"retired is left alone", supervise.StateRetired, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			reclaimArchitect(t, sup, "LEGION-1", tc.state, "ses_arch")
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
				notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", reclaimNotice(), time.Now()))

			_ = runner.notice(context.Background(), row, reclaimNotice())

			if got := len(rt.CallsOf("Resume")); got != tc.resumes {
				t.Fatalf("resumes = %d, want %d for a %s architect", got, tc.resumes, tc.state)
			}
		})
	}
}

// (3, continued) A deaf architect of a lingering or closed tree is left alone: its tree has
// finished and re-admission, not a relaunch, starts it again. Green before and after — a guard.
func TestADeafArchitectInALingeringTreeIsNotReclaimed(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", reclaimNotice(), time.Now()))

	_ = runner.notice(context.Background(), row, reclaimNotice())

	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want a lingering tree's architect left alone", got)
	}
}

// (2) A delivery exception for a running architect whose own live session failed to take a re-held
// copy again (the stale-registration window the publish path's ErrNoHolder never surfaces): the
// architect is re-claimed by a relaunch AND its notice is still re-held, so the two heal the pane
// together. Red on main: nothing relaunches the pane on an exception.
func TestAnUnheldRunningArchitectIsReclaimedAndItsNoticeStillReheld(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	// A copy that has already been re-held once (resends == 1) and failed again on the architect's
	// own live session (recipient == ses_arch).
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts", Resends: 1}
	report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_arch"}

	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold: %v", err)
	}

	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want the deaf architect relaunched once", got)
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v, want the notice still re-held once", got)
	}
}

// (2, negative) An exception whose recipient is a stale predecessor session the claim already
// moved off (recipient != the claim's current session) is a healthy architect on a new session:
// it is not relaunched, its notice is re-held for the copy the new session takes. Green guard.
func TestAHealthyArchitectOnANewSessionIsNotReclaimedByAStaleExceptionsReport(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_new")
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts", Resends: 1}
	report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_stopped"}

	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold: %v", err)
	}

	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want a healthy architect on a new session left alone", got)
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v, want the notice still re-held once", got)
	}
}

// (M5) A first delivery exception (Resends == 0, not a re-held copy) does NOT relaunch, even on the
// architect's own session: the first receipt timeout waits for the copy the re-hold sends, so a
// transient blip is not turned into a process restart. The notice is still re-held. This pins the
// Resends-gate false-positive guard — flipping Resends to 0 must leave the pane untouched.
func TestAFirstExceptionDoesNotReclaimAndWaitsForTheReheldCopy(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_arch"}

	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold: %v", err)
	}

	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want a first exception to wait for the re-held copy, not relaunch", got)
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v, want the notice still re-held once", got)
	}
}

// (4) A burst of exceptions for one topic triggers one relaunch, not one per report: the first
// relaunch moves the claim out of ready/idle, and every later report finds it launching and does
// nothing. Red on main: without the state transition gate nothing bounds the relaunches (there are
// none at all before the change, so the want-one assertion fails).
func TestABurstOfUnheldExceptionsForOneTopicTriggersOneReclaim(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts", Resends: 1}

	for i := range 5 {
		report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_arch"}
		if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
			t.Fatalf("rehold %d: %v", i, err)
		}
	}

	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want one relaunch for the burst of exceptions", got)
	}
}

// driveClaimReady walks a just-relaunched claim (launching) back to ready on its resumed session, so
// the limiter's in-flight slot frees for the next deaf pane.
func driveClaimReady(t *testing.T, sup *supervisor, token claim.Token) {
	t.Helper()
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	view := machine.View()
	gen, session := view.Generation, view.Session
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: gen},
		supervise.RequestRegister{Claim: token, Generation: gen, Session: session, SessionFile: "/sessions/" + session + ".jsonl"},
		supervise.RequestReady{Claim: token, Generation: gen, Session: session},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("drive %s to ready: %v", token, err)
		}
	}
}

// (global bound) N deaf topics at once produce at most the cap relaunching, and the rest are not
// dropped: once the in-flight ones come back ready, their slots free and a retry relaunches the rest.
func TestReclaimGlobalCapBoundsConcurrentRelaunchesAndDropsNone(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimCap: 2, reclaimTryCap: 5, reclaimInterval: time.Millisecond}
	tokens := make([]claim.Token, 4)
	for i := range tokens {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		tokens[i] = reclaimArchitect(t, sup, issue, supervise.StateReady, "ses_"+issue)
	}
	// Four deaf topics at once (the clock steps one stagger interval per trigger, so the cap, not the
	// stagger, is what bounds them).
	for _, token := range tokens {
		runner.reclaimUnheldRunning(context.Background(), token, "publish")
		clock = clock.Add(time.Millisecond)
	}
	if got := len(rt.CallsOf("Resume")); got != 2 {
		t.Fatalf("resumes = %d, want at most the cap (2) relaunching at once", got)
	}
	// Free the two in flight; a retry of the queued two now reaches a slot.
	driveClaimReady(t, sup, tokens[0])
	driveClaimReady(t, sup, tokens[1])
	for _, token := range tokens[2:] {
		runner.reclaimUnheldRunning(context.Background(), token, "publish")
		clock = clock.Add(time.Millisecond)
	}
	if got := len(rt.CallsOf("Resume")); got != 4 {
		t.Fatalf("resumes = %d, want all four eventually relaunched (none dropped)", got)
	}
}

// (global bound) The fleet-wide stagger starts at most one new relaunch per interval: two deaf topics
// at the same instant relaunch one now and the other only once the interval has elapsed.
func TestReclaimStaggerStartsAtMostOnePerInterval(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimCap: 8, reclaimTryCap: 5, reclaimInterval: time.Minute}
	a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
	b := reclaimArchitect(t, sup, "LEGION-2", supervise.StateReady, "ses_b")

	runner.reclaimUnheldRunning(context.Background(), a, "publish")
	runner.reclaimUnheldRunning(context.Background(), b, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want only one within the stagger interval", got)
	}
	clock = clock.Add(time.Minute)
	runner.reclaimUnheldRunning(context.Background(), b, "publish")
	if got := len(rt.CallsOf("Resume")); got != 2 {
		t.Fatalf("resumes = %d, want the second relaunched once the interval elapsed", got)
	}
}

// (loop guard) A claim whose role stays unheld after its reclaims is relaunched at most the per-claim
// bound, and the give-up is logged once — a role another live session holds (the claude backend's
// soft boot claim throws) would otherwise relaunch forever and burn the death budget.
func TestReclaimLoopGuardBoundsPerClaimAttemptsAndLogsGiveUpOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", supervisor: sup,
		project: "legion", now: func() time.Time { return clock }, reclaimTryCap: 2, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")

	// The claim keeps coming back deaf: drive it ready after each relaunch, step past its backoff.
	for range 4 {
		before := len(rt.CallsOf("Resume"))
		runner.reclaimUnheldRunning(context.Background(), token, "publish")
		if len(rt.CallsOf("Resume")) > before {
			driveClaimReady(t, sup, token)
		}
		clock = clock.Add(10 * time.Minute)
	}

	if got := len(rt.CallsOf("Resume")); got != 2 {
		t.Fatalf("resumes = %d, want at most the per-claim bound (2)", got)
	}
	if n := strings.Count(logged.String(), "gave up relaunching"); n != 1 {
		t.Fatalf("give-up logged %d times, want once", n)
	}
}

// (MEDIUM 3) A ready claim that holds a delivery sent and acknowledged but not yet confirmed — the
// pane may be running tool calls — is NOT relaunched: "ready or idle" is not "no work in flight", and
// a relaunch there would lose the turn's work and re-send the task without the interrupted preamble.
func TestAReadyClaimWithWorkInFlightIsNotReclaimed(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		State: supervise.StateReady, Session: "ses_arch", SessionFile: "/sessions/ses_arch.jsonl",
		Pending: &supervise.Delivery{ID: "outbox:1", Task: "do the work", QueuedAt: time.Now()},
	}, ""); err != nil {
		t.Fatalf("create the architect with work in flight: %v", err)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion", now: time.Now}

	runner.reclaimUnheldRunning(context.Background(), token, "publish")

	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want a pane with work in flight left alone", got)
	}
}

// (MEDIUM 2) A relaunch runs synchronously under the claim's machine, so the outbox row can outlive
// its lease and be re-run by the sweep. The accounting covers the whole relaunch: while it is booting
// a re-run is a no-op, and once it is back to ready the per-claim backoff — and only the backoff: the
// clock steps past the fleet-wide stagger first — rejects a re-trigger until the backoff elapses.
func TestReclaimDoesNotDoubleRelaunchAcrossALeaseLapse(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")

	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want the first relaunch", got)
	}
	// A lease-lapsed re-run while the relaunch is in flight (claim booting): no-op.
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want no relaunch while the first is booting", got)
	}
	// The relaunch completes (pane back to ready, still deaf). A second past the stagger but inside
	// the 30s backoff, a re-run is rejected by the backoff alone.
	driveClaimReady(t, sup, token)
	clock = clock.Add(time.Second)
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want no double-relaunch within the per-claim backoff", got)
	}
	// Past the backoff a legitimate second attempt relaunches.
	clock = clock.Add(time.Minute)
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 2 {
		t.Fatalf("resumes = %d, want the bounded second attempt once the backoff elapsed", got)
	}
}

// The per-claim backoff lengthens with each attempt of one incident — 30s, then 2m, then 5m — so a
// claim that keeps coming back deaf is retried a few times over minutes. The stagger is a nanosecond
// here, so every rejection below is the backoff's.
func TestReclaimBackoffLengthensWithEachAttempt(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	start := time.Now()
	clock := start
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimTryCap: 5, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	at := func(offset time.Duration, want int) {
		t.Helper()
		clock = start.Add(offset)
		before := len(rt.CallsOf("Resume"))
		runner.reclaimUnheldRunning(context.Background(), token, "publish")
		got := len(rt.CallsOf("Resume"))
		if got > before {
			driveClaimReady(t, sup, token)
		}
		if got != want {
			t.Fatalf("at +%s resumes = %d, want %d", offset, got, want)
		}
	}
	at(0, 1)
	at(29*time.Second, 1) // inside the first 30s backoff
	at(31*time.Second, 2) // past it; the next backoff is 2m from here
	at(31*time.Second+119*time.Second, 2)
	at(31*time.Second+121*time.Second, 3) // the next backoff is 5m from here
	at(152*time.Second+299*time.Second, 3)
	at(152*time.Second+301*time.Second, 4)
}

// (Gate2432C / Gate2432F) A claim that demonstrably healed — a publish the listener accepted, with no
// failed forward following it for reclaimHealedAfter — forgets its reclaim history, so separate,
// fully-recovered incidents over a long life do not accumulate into a permanent give-up. With the
// attempt cap at 2 and no reset, the third of these five recovered incidents would be refused.
func TestARecoveredClaimResetsItsReclaimCount(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimTryCap: 2, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")

	for range 5 {
		runner.reclaimUnheldRunning(context.Background(), token, "publish")
		driveClaimReady(t, sup, token)
		runner.noteReclaimDelivered(token) // the relaunched pane takes a publish, and no failed forward follows
		clock = clock.Add(reclaimHealedAfter + time.Minute)
	}

	if got := len(rt.CallsOf("Resume")); got != 5 {
		t.Fatalf("resumes = %d, want each independent, recovered incident healed (the count resets once the pane is seen healed)", got)
	}
}

// (Gate2432F B1) A pane that boots but keeps failing its forwards — the listener accepts each re-held
// copy for its registered session and then reports the forward failed — is not relaunched for every
// notice: an accepted publish that a failed forward follows is not evidence the pane healed, so the
// incident's count accumulates, the cap fires, and the give-up is logged once. With the history reset
// on any accepted publish this relaunched all five times.
func TestAPaneThatKeepsFailingItsForwardsIsNotRelaunchedForever(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
		notices: &holderPublisher{}, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimTryCap: 2, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts", Resends: 1}

	for range 5 {
		// The listener reports that a re-held copy's forward failed on the architect's own session.
		report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_arch"}
		before := len(rt.CallsOf("Resume"))
		if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
			t.Fatalf("rehold: %v", err)
		}
		if len(rt.CallsOf("Resume")) > before {
			driveClaimReady(t, sup, token)
		}
		// The next copy is accepted for the still-registered session — and its forward fails again
		// at the top of the next round.
		runner.noteReclaimDelivered(token)
		clock = clock.Add(10 * time.Minute)
	}

	if got := len(rt.CallsOf("Resume")); got != 2 {
		t.Fatalf("resumes = %d, want at most the per-incident bound (2)", got)
	}
	if n := strings.Count(logged.String(), "gave up relaunching"); n != 1 {
		t.Fatalf("give-up logged %d times, want once", n)
	}
}

// (wiring) A publish the listener accepts starts the architect's healing clock, and a failed-forward
// report for it stops the clock: notice() and rehold() feed the limiter its two signals.
func TestAnAcceptedPublishStartsTheHealingClockAndAFailedForwardStopsIt(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: &holderPublisher{}, supervisor: sup, project: "legion", now: time.Now, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	driveClaimReady(t, sup, token)

	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", reclaimNotice(), time.Now()))
	if err := runner.notice(context.Background(), row, reclaimNotice()); err != nil {
		t.Fatalf("notice: %v", err)
	}
	if runner.reclaimTries[token].deliveredAt.IsZero() {
		t.Fatalf("an accepted publish to a relaunched architect did not start its healing clock")
	}
	report := laneReport{"evt-exception", "receipt_timeout", architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", reclaimNotice(), "legion-outbox:7", "ses_arch"}
	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold: %v", err)
	}
	if try, ok := runner.reclaimTries[token]; !ok || !try.deliveredAt.IsZero() {
		t.Fatalf("a failed forward did not stop the healing clock (history %+v, kept %v)", try, ok)
	}
}

// (Gate2432F B2) A publish the listener accepts while a relaunch still boots — the stopped session's
// registration can answer it — neither starts the healing clock nor frees the relaunch's slot: the
// slot frees only once the claim leaves the booting states, so the cap stays "at most N booting".
func TestAnAcceptedPublishDuringABootDoesNotFreeItsSlot(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: &holderPublisher{}, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimCap: 1, reclaimInterval: time.Nanosecond}
	a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
	b := reclaimArchitect(t, sup, "LEGION-3", supervise.StateReady, "ses_b")

	runner.reclaimUnheldRunning(context.Background(), a, "publish")
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", reclaimNotice(), clock))
	if err := runner.notice(context.Background(), row, reclaimNotice()); err != nil {
		t.Fatalf("notice to the booting architect: %v", err)
	}
	clock = clock.Add(time.Second)
	runner.reclaimUnheldRunning(context.Background(), b, "publish")

	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want the second held back while the first still boots (cap 1)", got)
	}
	if !runner.reclaimTries[a].deliveredAt.IsZero() {
		t.Fatalf("a publish accepted during the boot started the healing clock")
	}
}

// A relaunch holds its slot through every booting state — launching, shim-connected, registered —
// and frees it only once the agent says ready.
func TestABootingRelaunchHoldsItsSlotUntilReady(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimCap: 1, reclaimInterval: time.Nanosecond}
	a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
	b := reclaimArchitect(t, sup, "LEGION-3", supervise.StateReady, "ses_b")
	runner.reclaimUnheldRunning(context.Background(), a, "publish")
	machine, _ := sup.Machine(a)
	gen := machine.View().Generation
	tryB := func(stage string, want int) {
		t.Helper()
		clock = clock.Add(time.Second)
		runner.reclaimUnheldRunning(context.Background(), b, "publish")
		if got := len(rt.CallsOf("Resume")); got != want {
			t.Fatalf("%s: resumes = %d, want %d", stage, got, want)
		}
	}
	tryB("launching", 1)
	if err := machine.Handle(context.Background(), supervise.StreamHello{Claim: a, Generation: gen}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	tryB("shim connected", 1)
	if err := machine.Handle(context.Background(), supervise.RequestRegister{Claim: a, Generation: gen, Session: "ses_a", SessionFile: "/sessions/ses_a.jsonl"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	tryB("registered", 1)
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: a, Generation: gen, Session: "ses_a"}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	tryB("ready", 2)
}

// liveLocator is a process the fake runtime recognises as token's, as a running pane's claim records
// one: the runtime's Resume and stop validate the locator they are handed.
func liveLocator(t *testing.T, rt *fake.Runtime, token claim.Token) *runtime.Locator {
	t.Helper()
	loc, err := rt.Spawn(context.Background(), runtime.SpawnSpec{Claim: token})
	if err != nil {
		t.Fatalf("mint a locator for %s: %v", token, err)
	}
	return &loc
}

// refusingLaunchStore refuses every launch's lifecycle check while refuse is set, as a reserved tree
// cleanup or an unanswered store does (supervise.Store.CheckLaunch).
type refusingLaunchStore struct {
	*outboxClaimStore
	refuseMu sync.Mutex
	refuse   bool
}

func (s *refusingLaunchStore) CheckLaunch(context.Context, supervise.Claim) error {
	s.refuseMu.Lock()
	defer s.refuseMu.Unlock()
	if s.refuse {
		return errors.New("the tree's cleanup is reserved")
	}
	return nil
}

func (s *refusingLaunchStore) setRefuse(refuse bool) {
	s.refuseMu.Lock()
	defer s.refuseMu.Unlock()
	s.refuse = refuse
}

// (Gate2432E Minor 2) A relaunch the launch path refuses frees its slot, refunds its attempt, clears
// its backoff and gives back the fleet-wide stagger: once the refusal clears, the same claim relaunches
// at the same instant — which needs all four — and its count is the one real attempt.
func TestARefusedRelaunchFreesItsSlotRefundsItsAttemptAndKeepsTheStagger(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	store := &refusingLaunchStore{outboxClaimStore: &outboxClaimStore{}}
	sup.deps.Store = store
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimCap: 1, reclaimInterval: time.Minute}
	token := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		State: supervise.StateReady, Session: "ses_arch", SessionFile: "/sessions/ses_arch.jsonl",
		Locator: liveLocator(t, rt, token),
	}, ""); err != nil {
		t.Fatalf("create the architect: %v", err)
	}

	store.setRefuse(true)
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want the refused relaunch to start nothing", got)
	}
	if got := runner.reclaimTries[token].count; got != 0 {
		t.Fatalf("attempts after a refused relaunch = %d, want it refunded", got)
	}

	store.setRefuse(false)
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want the claim relaunched at once (slot, backoff and stagger given back)", got)
	}
	if got := runner.reclaimTries[token].count; got != 1 {
		t.Fatalf("attempts = %d, want the one real attempt", got)
	}
}

// (Gate2432F m1, Gate2432E Minor 4) A reclaim that launches nothing without an error is refunded too.
// An idle claim can still hold a suspension its runtime refused at the end of a turn no delivery
// started (an Envoy notice's); the reclaim completes that suspension (endHeld) rather than relaunching,
// so its attempt, slot and stagger are given back and another deaf pane relaunches at once.
func TestAReclaimThatLaunchesNothingIsRefunded(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimInterval: time.Minute}
	token := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		State: supervise.StateWorking, Session: "ses_arch", SessionFile: "/sessions/ses_arch.jsonl",
		Locator: liveLocator(t, rt, token),
	}, "")
	if err != nil {
		t.Fatalf("create the architect: %v", err)
	}
	rt.FailSuspend(errors.New("the pane would not stop"))
	if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: "a test"}); err != nil && !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("suspend: %v", err)
	}
	_ = machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: token}) // the stop is refused
	if view := machine.View(); view.State != supervise.StateIdle || view.Pending != nil {
		t.Fatalf("claim is %s with pending %+v, want idle with no delivery and the suspension still held", view.State, view.Pending)
	}
	rt.FailSuspend(nil)

	runner.reclaimUnheldRunning(context.Background(), token, "publish")

	if got := len(rt.CallsOf("Resume")); got != 0 {
		t.Fatalf("resumes = %d, want nothing launched", got)
	}
	if got := machine.View().State; got != supervise.StateSuspended {
		t.Fatalf("claim is %s, want the held suspension completed", got)
	}
	if got := runner.reclaimTries[token].count; got != 0 {
		t.Fatalf("attempts = %d, want the reclaim that launched nothing refunded", got)
	}
	other := reclaimArchitect(t, sup, "LEGION-3", supervise.StateReady, "ses_other")
	runner.reclaimUnheldRunning(context.Background(), other, "publish")
	if got := len(rt.CallsOf("Resume")); got != 1 {
		t.Fatalf("resumes = %d, want another deaf pane relaunched at once (the stagger given back)", got)
	}
}

// (Gate2432E Minor 3) A claim that has ended — failed, retired, or no longer supervised — has its
// reclaim history forgotten at the next prune, so the map holds only claims whose incident goes on.
func TestAFinishedClaimsReclaimHistoryIsForgotten(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimInterval: time.Nanosecond}
	a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
	runner.reclaimUnheldRunning(context.Background(), a, "publish")
	if _, ok := runner.reclaimTries[a]; !ok {
		t.Fatalf("expected a reclaim history after a reclaim")
	}
	machine, _ := sup.Machine(a)
	if err := machine.Handle(context.Background(), supervise.RequestTreeClose{Claim: a}); err != nil {
		t.Fatalf("close the tree: %v", err)
	}

	b := reclaimArchitect(t, sup, "LEGION-3", supervise.StateReady, "ses_b")
	clock = clock.Add(time.Second)
	runner.reclaimUnheldRunning(context.Background(), b, "publish") // its admission prunes

	if _, ok := runner.reclaimTries[a]; ok {
		t.Fatalf("a retired claim's reclaim history was kept")
	}
}

// The production bounds are the documented ones: at most four relaunches booting at once, at most one
// new relaunch per 15 seconds, and at most three attempts per incident before the give-up.
func TestReclaimDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Run("cap and stagger", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		start := time.Now()
		clock := start
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
			now: func() time.Time { return clock }}
		claims := make([]claim.Token, 5)
		for i := range claims {
			issue := fmt.Sprintf("LEGION-%d", i+1)
			claims[i] = reclaimArchitect(t, sup, issue, supervise.StateReady, "ses_"+issue)
		}
		trigger := func(offset time.Duration, i, want int) {
			t.Helper()
			clock = start.Add(offset)
			runner.reclaimUnheldRunning(context.Background(), claims[i], "publish")
			if got := len(rt.CallsOf("Resume")); got != want {
				t.Fatalf("at +%s claim %d: resumes = %d, want %d", offset, i, got, want)
			}
		}
		trigger(0, 0, 1)
		trigger(14*time.Second, 1, 1) // inside the 15s stagger
		trigger(16*time.Second, 1, 2)
		trigger(32*time.Second, 2, 3)
		trigger(48*time.Second, 3, 4)
		trigger(64*time.Second, 4, 4) // four still booting: the cap
	})
	t.Run("attempts", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		clock := time.Now()
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", supervisor: sup, project: "legion",
			now: func() time.Time { return clock }}
		token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
		for range 5 {
			before := len(rt.CallsOf("Resume"))
			runner.reclaimUnheldRunning(context.Background(), token, "publish")
			if len(rt.CallsOf("Resume")) > before {
				driveClaimReady(t, sup, token)
			}
			clock = clock.Add(10 * time.Minute)
		}
		if got := len(rt.CallsOf("Resume")); got != 3 {
			t.Fatalf("resumes = %d, want three attempts before the give-up", got)
		}
	})
}

// pruneReclaims runs the limiter's prune as admitReclaim does, for a test that reads its maps.
func pruneReclaims(r *outbox) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	r.pruneReclaimLocked()
}

// A reservation holds from admission, before its relaunch is handed to the machine — while the claim
// still reads ready — so neither a second reclaim of the same claim nor another claim past the cap
// gets in; a reservation that launched nothing gives its slot back.
func TestAReservationHoldsUntilItsRelaunchIsHandedOver(t *testing.T) {
	t.Run("the same claim", func(t *testing.T) {
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		clock := time.Now()
		runner := &outbox{log: quietLogger(), supervisor: sup, project: "legion",
			now: func() time.Time { return clock }, reclaimCap: 2, reclaimInterval: time.Nanosecond}
		a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
		if !runner.admitReclaim(a) {
			t.Fatal("the first reclaim was not admitted")
		}
		clock = clock.Add(time.Minute) // past its backoff and the stagger
		if runner.admitReclaim(a) {
			t.Fatal("a second reclaim of a claim already reserved was admitted")
		}
	})
	t.Run("another claim", func(t *testing.T) {
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		clock := time.Now()
		runner := &outbox{log: quietLogger(), supervisor: sup, project: "legion",
			now: func() time.Time { return clock }, reclaimCap: 1, reclaimInterval: time.Nanosecond}
		a := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_a")
		b := reclaimArchitect(t, sup, "LEGION-3", supervise.StateReady, "ses_b")
		if !runner.admitReclaim(a) {
			t.Fatal("the first reclaim was not admitted")
		}
		clock = clock.Add(time.Second)
		if runner.admitReclaim(b) {
			t.Fatal("another claim got past the cap while the first reservation was not yet handed over")
		}
		runner.settleReclaim(a, false) // it launched nothing
		if !runner.admitReclaim(b) {
			t.Fatal("a reservation that launched nothing did not give its slot back")
		}
	})
}

// The healing clock runs a full reclaimHealedAfter from the FIRST accepted publish since the claim's
// last reclaim or failed forward: later accepted publishes do not restart it (so a busy architect is
// not kept from healing), and the history is kept until the minute is out.
func TestTheHealingClockRunsAFullMinuteFromTheFirstAcceptedPublish(t *testing.T) {
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	start := time.Now()
	clock := start
	runner := &outbox{log: quietLogger(), supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	driveClaimReady(t, sup, token)

	runner.noteReclaimDelivered(token)
	clock = start.Add(50 * time.Second)
	runner.noteReclaimDelivered(token)
	clock = start.Add(reclaimHealedAfter - time.Second)
	pruneReclaims(runner)
	if _, ok := runner.reclaimTries[token]; !ok {
		t.Fatal("the history was forgotten before the healing clock ran out")
	}
	clock = start.Add(reclaimHealedAfter + time.Second)
	pruneReclaims(runner)
	if _, ok := runner.reclaimTries[token]; ok {
		t.Fatal("the history was kept after the healing clock ran out from the first accepted publish")
	}
}

// A publish accepted while the claim relaunches for a reason of its own — its process died and the
// machine relaunched it outside the limiter — goes to whatever registration still answers, not to a
// pane that has taken its role, so it starts no healing clock.
func TestAPublishAcceptedWhileTheClaimRelaunchesForAnotherReasonStartsNoHealingClock(t *testing.T) {
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	clock := time.Now()
	runner := &outbox{log: quietLogger(), supervisor: sup, project: "legion",
		now: func() time.Time { return clock }, reclaimInterval: time.Nanosecond}
	token := reclaimArchitect(t, sup, "LEGION-1", supervise.StateReady, "ses_arch")
	runner.reclaimUnheldRunning(context.Background(), token, "publish")
	driveClaimReady(t, sup, token)
	pruneReclaims(runner) // the reclaim's relaunch has booted, so its reservation is gone

	machine, _ := sup.Machine(token)
	gone := runtime.Observation{Locator: *machine.View().Locator, Kind: runtime.Gone}
	_ = machine.Handle(context.Background(), supervise.RuntimeObservation{Observation: gone})
	if state := machine.View().State; !reclaimBooting(state) {
		t.Fatalf("claim is %s after its death, want it relaunching (resumes %d)", state, len(rt.CallsOf("Resume")))
	}
	runner.noteReclaimDelivered(token)

	if !runner.reclaimTries[token].deliveredAt.IsZero() {
		t.Fatal("a publish accepted while the claim relaunched for another reason started its healing clock")
	}
}
