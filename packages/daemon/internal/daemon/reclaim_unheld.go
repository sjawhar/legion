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
	// maxReclaimAttempts bounds how many times the daemon relaunches one claim within one incident
	// before it gives up and logs once. A pane that boots but still cannot take its messages — a role
	// another live session holds makes a soft boot claim (the claude backend's) throw, and a registered
	// session whose process drops its forwards keeps failing them — would otherwise be relaunched for
	// every notice; after the bound the role stays unheld until the pane is seen taking its messages
	// again (reclaimHealedAfter) or an operator retries it, and the give-up names the claim.
	maxReclaimAttempts = 3
	// reclaimHealedAfter is how long a claim must take accepted publishes with no failed forward
	// reported before the daemon counts the pane healed and forgets its reclaim history, so a later,
	// independent loss starts with fresh attempts. An accepted publish alone proves nothing: the
	// listener accepts a publish for a session that is still registered and forwards it, and only after
	// its receipt window (about two seconds, longer while forwards queue on the role lane) reports
	// whether the pane took it (notice_exceptions.go) — the delivery-exception lane exists because a
	// deaf pane's stale registration keeps accepting. A minute of accepted publishes with no failed
	// forward is evidence the pane itself takes its messages.
	reclaimHealedAfter = time.Minute
)

// reclaimTry is one claim's reclaim history in the limiter, for the incident in progress: how many
// times it has been relaunched, the earliest its next relaunch may start (its own backoff), whether
// the daemon has given up on it and logged so, and deliveredAt — the first publish to it the listener
// accepted since its last reclaim or failed forward, once its relaunch was done, zero when there has
// been none. A failed forward zeroes deliveredAt; deliveredAt standing for reclaimHealedAfter ends the
// incident (pruneReclaimLocked).
type reclaimTry struct {
	count       int
	nextAt      time.Time
	gaveUp      bool
	deliveredAt time.Time
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
//   - ready or idle with no delivery in flight is relaunched through the limiter (admitReclaim),
//     which bounds the fleet-wide blast radius. A ready or idle claim that holds a pending delivery
//     (sent, acknowledged, or awaiting its turn) is in a turn in all but name and waits, since
//     relaunching it would lose that work.
//   - working is left for its turn to end: a relaunch would lose the turn's work. The caller holds
//     the message (errNoticeWaits) or re-queues it, so a later attempt catches the pane idle.
//   - a suspended claim is left alone here; the idle-suspended-worker branch (→ RequestRuleResume)
//     that another lane adds goes in this switch's empty slot.
//   - every other state — booting, failed, retired, or not supervised — holds no running pane with a
//     role to re-claim.
//
// The daemon addresses a role topic only for an architect's notices (the notice executor) and the
// merge-queue role (which does not call this), and the exception lane filters to architect reports,
// so this heals architect role claims — which is what the deaf roles are — and not a worker role (a
// worker is never daemon-role-addressed; it is driven on its worker stream) or the controller (it has
// its own session-side recovery). A peer agent's publish to an unheld running role is the residual.
// Keeping this to architects is also what makes the no-live-holder guarantee below safe without a
// listener query: an architect's claim boot issues a HARD Envoy claim (it always lands, never 409s),
// so relaunching one onto a role a holder has since taken cannot crash-loop on boot. A phase worker's
// claude-backed boot claim is SOFT and WOULD 409 into a live holder, so extending this recognizer to
// worker roles must add a real holder check first; it is architect-only until then.
//
// The relaunch runs synchronously under the claim's machine, so the row can outlive its lease and be
// re-run by the sweep; the limiter's accounting covers the whole relaunch so that cannot
// double-relaunch — the claim holds its in-flight reservation from admission until its relaunch has
// booted, a second reclaim of a claim already in flight is refused, and once it is back to ready the
// per-claim backoff rejects a re-trigger until it elapses (admitReclaim).
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
	view := machine.View()
	switch view.State {
	case supervise.StateReady, supervise.StateIdle:
		// "ready or idle" is not "no work in flight": a task sent and acknowledged keeps the claim
		// prompted (onAcked) until the agent's turn-start frame, so the pane may already be running
		// tool calls. Relaunching then kills that work and re-sends the task without the interrupted
		// preamble (the delivery is not confirmed, so interrupted is a no-op), which can duplicate
		// side effects. A pending delivery (sent, acknowledged, or awaiting its turn) is exactly the
		// out-of-turn condition quiesce.go checks, and for a ready or idle claim it is the one the
		// claim carries, so a pane with work in flight waits for its turn's end.
		if view.Pending != nil {
			return
		}
		if !r.admitReclaim(token) {
			return
		}
		r.relaunchUnheld(ctx, machine, token, trigger, view.Generation)
	// case supervise.StateSuspended: an idle-suspended worker is resumed by rule (RequestRuleResume)
	// here — another lane adds that branch.
	default:
		// A working pane waits for its turn to end; a booting pane claims the role on its own; a
		// failed, retired, or unsupervised claim holds no role to re-claim.
	}
}

// relaunchUnheld hands an admitted reclaim to the claim's machine and settles its reservation by what
// the machine did, read from the claim's generation, which every launch moves. A relaunch that started
// keeps its slot, attempt and stagger, and its slot frees once the claim leaves the booting states
// (pruneReclaimLocked). A reclaim that launched nothing is refunded as though it was never admitted
// (releaseReclaim): the launch path refused it (a reserved tree cleanup, a store error), the table
// ignored it because the claim left ready or idle after its view was read, or the claim still held a
// suspension its runtime had refused at its turn's end, which the reclaim completes instead of
// relaunching (replaceProcess's endHeld). A panic launches nothing as far as the limiter knows and is
// refunded too, so it cannot strand a slot.
func (r *outbox) relaunchUnheld(ctx context.Context, machine *supervise.Machine, token claim.Token, trigger string, before uint64) {
	launched := false
	defer func() { r.settleReclaim(token, launched) }()
	err := machine.Handle(ctx, supervise.RoleUnheld{Claim: token, Trigger: trigger})
	after := machine.View()
	launched = after.Generation != before
	switch {
	case err != nil:
		r.log.Warn("outbox could not relaunch a running-but-unheld pane to re-claim its role",
			"claim", token, "trigger", trigger, "launched", launched, "error", err)
	case !launched:
		r.log.Info("outbox reclaim of a running-but-unheld pane launched nothing; its attempt is refunded",
			"claim", token, "trigger", trigger, "state", after.State)
	}
}

// admitReclaim decides whether to relaunch token now and, when it does, reserves an in-flight slot,
// counts the attempt, starts a new incident clock (deliveredAt), and holds the fleet-wide stagger. It
// returns false — relaunch nothing — when a relaunch of this claim is already reserved or booting,
// when this claim's attempts for the incident are spent (logged once, so a pane that cannot take its
// messages is visible instead of relaunched forever), when its own backoff or the fleet-wide stagger
// has not elapsed, or when the daemon-wide in-flight cap is reached. A false is never a drop: the held
// message retries on the outbox backoff and reaches a slot once one frees.
func (r *outbox) admitReclaim(token claim.Token) bool {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	r.pruneReclaimLocked()
	if _, inFlight := r.reclaimInFlight[token]; inFlight {
		return false
	}
	try := r.reclaimTries[token]
	if try.gaveUp {
		return false
	}
	if try.count >= cmp.Or(r.reclaimTryCap, maxReclaimAttempts) {
		try.gaveUp = true
		r.setReclaimTryLocked(token, try)
		r.log.Warn("outbox gave up relaunching a pane to re-claim its role; the role stays unheld until the pane is seen taking its messages again or an operator retries the claim",
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
		r.reclaimInFlight = map[claim.Token]bool{}
	}
	r.reclaimInFlight[token] = false
	try.count++
	try.nextAt = now.Add(reclaimBackoff(try.count))
	try.deliveredAt = time.Time{}
	r.setReclaimTryLocked(token, try)
	r.reclaimNextAt = now.Add(cmp.Or(r.reclaimInterval, reclaimMinInterval))
	return true
}

// settleReclaim records what an admitted reclaim did: one that launched hands its reservation to the
// prune, which frees it once the claim has booted; one that launched nothing is refunded.
func (r *outbox) settleReclaim(token claim.Token, launched bool) {
	if !launched {
		r.releaseReclaim(token)
		return
	}
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimInFlight[token]; ok {
		r.reclaimInFlight[token] = true
	}
}

// releaseReclaim undoes an admitted reclaim that launched nothing: the slot frees, the attempt is not
// counted, the claim's backoff is cleared so its held message's next retry asks again at once, and
// the fleet-wide stagger it reserved is rolled back so a relaunch that never started does not delay
// the next, different claim by an interval.
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
	r.reclaimNextAt = r.now()
}

func (r *outbox) setReclaimTryLocked(token claim.Token, try reclaimTry) {
	if r.reclaimTries == nil {
		r.reclaimTries = map[claim.Token]reclaimTry{}
	}
	r.reclaimTries[token] = try
}

// noteReclaimDelivered records that the listener accepted a publish to token's architect. For a claim
// in a reclaim incident whose relaunch is done (it has taken its role, and holds no in-flight
// reservation), the first such publish starts the healing clock (deliveredAt). It forgets nothing by
// itself and never frees a slot: a publish accepted while a relaunch still boots may have gone to the
// stopped session's registration, and one accepted for a pane that drops its forwards is followed by a
// failed-forward report (noteReclaimException). The history is forgotten only once deliveredAt has
// stood for reclaimHealedAfter (pruneReclaimLocked).
func (r *outbox) noteReclaimDelivered(token claim.Token) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimTries[token]; !ok {
		return
	}
	r.pruneReclaimLocked()
	try, ok := r.reclaimTries[token]
	if !ok || !try.deliveredAt.IsZero() {
		return
	}
	if _, inFlight := r.reclaimInFlight[token]; inFlight || !claimTookRole(r.claimState(token)) {
		return
	}
	try.deliveredAt = r.now()
	r.reclaimTries[token] = try
}

// noteReclaimException records that the listener reported a failed forward to token's architect: the
// publishes it accepted before did not reach a pane that takes its messages, so the healing clock
// stops and the incident goes on.
func (r *outbox) noteReclaimException(token claim.Token) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	try, ok := r.reclaimTries[token]
	if !ok {
		return
	}
	try.deliveredAt = time.Time{}
	r.reclaimTries[token] = try
}

// pruneReclaimLocked frees every in-flight reservation whose relaunch has been handed to the machine
// and has finished booting — the claim is prompted again, or has failed, retired, or suspended — and
// forgets the reclaim history of a claim this daemon no longer supervises, has failed or retired, or
// has healed (deliveredAt standing for reclaimHealedAfter with no reservation), so the map holds only
// claims whose incident is still going on. A reservation whose relaunch has not yet been handed over
// is kept whatever its claim reads: until the machine moves it to launching it still reads ready, and
// freeing it then would let another reclaim past the cap. View never waits on a decision in flight,
// so a claim mid-relaunch is read without blocking (supervisedClaim).
func (r *outbox) pruneReclaimLocked() {
	for token, handed := range r.reclaimInFlight {
		if handed && !reclaimBooting(r.claimState(token)) {
			delete(r.reclaimInFlight, token)
		}
	}
	if len(r.reclaimTries) == 0 {
		return
	}
	now := r.now()
	for token, try := range r.reclaimTries {
		if claimEnded(r.claimState(token)) {
			delete(r.reclaimTries, token)
			continue
		}
		if _, inFlight := r.reclaimInFlight[token]; inFlight {
			continue
		}
		if !try.deliveredAt.IsZero() && !now.Before(try.deliveredAt.Add(reclaimHealedAfter)) {
			delete(r.reclaimTries, token)
		}
	}
}
