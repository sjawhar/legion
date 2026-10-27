package daemon

import (
	"cmp"
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// maxReclaimsInFlight bounds how many deaf panes the daemon has relaunching at once. A listener
	// reset can leave a fleet deaf — the measured incident was 212 distinct architect roles with no
	// holder at once — and each relaunch is an `omp --resume` that re-provisions a workspace and runs
	// a capability probe before its claims/ready (@legion/pi-shared legion-claim-core.ts), so
	// relaunching hundreds at once on a saturated host (the incident's ~600 panes at load ~305/360)
	// is a thundering herd. Worse, the boot role claim runs before claims/ready, so a listener that is
	// briefly unreachable makes the boot throw, the process exit, and the daemon charge a death
	// (legion-claim-core.ts:108-113); hundreds of concurrent boots against one blipping listener would
	// turn a transient problem into a fleet of budget-dead claims an operator must retry by hand. A
	// small cap keeps that exposure bounded.
	maxReclaimsInFlight = 4
	// reclaimMinInterval staggers relaunch starts across the fleet: the daemon starts at most one new
	// reclaim this often, so a fleet-wide deaf state heals a few per minute and queues the rest — their
	// held messages retry on the outbox backoff and reach a slot once the stagger and the cap allow it.
	// At this interval ~212 claims start over ~50 minutes, steadily, rather than at once.
	reclaimMinInterval = 15 * time.Second
	// maxReclaimAttempts bounds how many times the daemon relaunches one claim to re-claim its role
	// before it gives up and logs once. A role another live session holds makes a soft boot claim (the
	// claude backend's) throw on boot, so relaunching forever would burn the death budget invisibly;
	// after the bound the role stays unheld until the pane claims it itself or an operator retries, and
	// the give-up names the claim so the stuck role is diagnosable.
	maxReclaimAttempts = 3
)

// reclaimTry is one claim's reclaim history in the limiter: how many times it has been relaunched,
// the earliest its next relaunch may start (its own backoff), and whether the daemon has given up on
// it and logged so.
type reclaimTry struct {
	count  int
	nextAt time.Time
	gaveUp bool
}

// reclaimBackoff spaces one claim's bounded relaunches, longer each time, so a claim that keeps
// coming back deaf is retried a few times over minutes rather than hammered.
func reclaimBackoff(count int) time.Duration {
	switch count {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// reclaimBooting is whether a claim in state is a relaunch the limiter is still waiting on: a reclaim
// moves the claim ready/idle -> launching, and it boots back through shim_connected and registered to
// ready/idle. While it boots it holds its in-flight slot; once it is prompted again (or has failed,
// retired, or suspended) the slot frees for the next deaf pane.
func reclaimBooting(state supervise.ClaimState) bool {
	switch state {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered:
		return true
	default:
		return false
	}
}

// reclaimUnheldRunning heals a pane that went deaf: a daemon publish to its role topic, or a delivery
// exception for it, found the role had no Envoy holder, yet the topic's claim is a running pane this
// daemon supervises. A listener reset drops a role's holder, and the pane's heartbeat re-asserts only
// a role its in-process envoy instance still believes it holds, which the reset can erase
// (docs/solutions/envoy/heartbeat-role-reassertion-and-regain-hooks.md); so nothing on the session
// side re-claims until the pane's next claim boot, which a running pane never reaches on its own. The
// relaunch re-runs that claim boot, which re-issues the role claim before its own claims/ready, so the
// next copy of the held message lands.
//
// It is a dispatch on the claim's state as this daemon supervises it, the recognizer its two callers
// share — the notice executor's publish (outbox.go, notice) and the delivery-exception consumer
// (notice_exceptions.go, rehold):
//
//   - ready or idle — a live pane that can be prompted and is not in a turn — is relaunched through
//     the limiter (admitReclaim), which bounds the fleet-wide blast radius.
//   - working is left for its turn to end: a relaunch would lose the turn's work. The caller holds
//     the message (errNoticeWaits) or re-queues it, so a later attempt catches the pane idle.
//   - a suspended claim is left alone here; the idle-suspended-worker branch (→ RequestRuleResume)
//     that another lane adds goes in this switch's empty slot.
//   - every other state — booting, failed, retired, or not supervised — holds no running pane with a
//     role to re-claim.
//
// Both callers only reach it with "no live holder" already established for the pane they name, so it
// never relaunches into a healthy holder: the publish path is in its ErrNoHolder branch (the listener
// reported no holder at all), and the exception path gates on the exception naming the claim's own
// current session (notice_exceptions.go), never a different session that has taken the role over. The
// relaunch is uncharged — the pane did nothing wrong — and any error is logged and swallowed: the
// message stays held and its retry asks again.
func (r *outbox) reclaimUnheldRunning(ctx context.Context, token claim.Token, trigger string) {
	machine, found := r.supervisor.Machine(token)
	if !found {
		return
	}
	switch machine.View().State {
	case supervise.StateReady, supervise.StateIdle:
		if !r.admitReclaim(token) {
			return
		}
		if err := machine.Handle(ctx, supervise.RoleUnheld{Claim: token, Trigger: trigger}); err != nil {
			r.releaseReclaim(token)
			r.log.Warn("outbox could not relaunch a running-but-unheld pane to re-claim its role",
				"claim", token, "trigger", trigger, "error", err)
		}
	// case supervise.StateSuspended: an idle-suspended worker is resumed by rule (RequestRuleResume)
	// here — another lane adds that branch.
	default:
		// A working pane waits for its turn to end; a booting pane claims the role on its own; a
		// failed, retired, or unsupervised claim holds no role to re-claim.
	}
}

// admitReclaim decides whether to relaunch token now and, when it does, reserves an in-flight slot,
// counts the attempt, and holds the fleet-wide stagger. It returns false — relaunch nothing — when
// this claim's reclaim attempts are spent (logged once, so a role another live session holds is
// visible instead of relaunched forever), when its own backoff or the fleet-wide stagger has not
// elapsed, or when the daemon-wide in-flight cap is reached. A false is never a drop: the held message
// retries on the outbox backoff and reaches a slot once one frees.
func (r *outbox) admitReclaim(token claim.Token) bool {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	r.pruneReclaimInFlightLocked()
	try := r.reclaimTries[token]
	if try.gaveUp {
		return false
	}
	if try.count >= cmp.Or(r.reclaimTryCap, maxReclaimAttempts) {
		try.gaveUp = true
		r.setReclaimTryLocked(token, try)
		r.log.Warn("outbox gave up relaunching a pane to re-claim its role; the role stays unheld until the pane claims it itself or an operator retries the claim",
			"claim", token, "attempts", try.count)
		return false
	}
	now := r.now()
	if now.Before(try.nextAt) || now.Before(r.reclaimNextAt) {
		return false
	}
	if len(r.reclaimInFlight) >= cmp.Or(r.reclaimCap, maxReclaimsInFlight) {
		return false
	}
	if r.reclaimInFlight == nil {
		r.reclaimInFlight = map[claim.Token]struct{}{}
	}
	r.reclaimInFlight[token] = struct{}{}
	try.count++
	try.nextAt = now.Add(reclaimBackoff(try.count))
	r.setReclaimTryLocked(token, try)
	r.reclaimNextAt = now.Add(cmp.Or(r.reclaimInterval, reclaimMinInterval))
	return true
}

// releaseReclaim undoes an admitted reclaim whose relaunch the machine refused (a reserved tree
// cleanup, a store error): the slot frees, the attempt is not counted — the relaunch never started —
// and the claim's backoff is cleared so its held message's next retry asks again at once.
func (r *outbox) releaseReclaim(token claim.Token) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	delete(r.reclaimInFlight, token)
	try := r.reclaimTries[token]
	if try.count > 0 {
		try.count--
	}
	try.nextAt = time.Time{}
	r.setReclaimTryLocked(token, try)
}

func (r *outbox) setReclaimTryLocked(token claim.Token, try reclaimTry) {
	if r.reclaimTries == nil {
		r.reclaimTries = map[claim.Token]reclaimTry{}
	}
	r.reclaimTries[token] = try
}

// pruneReclaimInFlightLocked drops every in-flight claim whose relaunch has finished — it is prompted
// again, or has failed, retired, or suspended — so its slot frees. View never waits on a decision in
// flight, so a claim mid-relaunch is read without blocking (supervisedClaim).
func (r *outbox) pruneReclaimInFlightLocked() {
	for token := range r.reclaimInFlight {
		if !reclaimBooting(r.claimState(token)) {
			delete(r.reclaimInFlight, token)
		}
	}
}
