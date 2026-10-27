package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
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
