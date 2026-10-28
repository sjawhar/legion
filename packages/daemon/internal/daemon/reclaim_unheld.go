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
	// reset can leave many roles unheld at once (one deployment saw 212 architect roles with no
	// holder after one reset), and each relaunch is an `omp --resume` that re-provisions a workspace
	// and runs a capability probe before its claims/ready (@legion/pi-shared legion-claim-core.ts),
	// so relaunching hundreds at once on a busy host is a thundering herd. Worse, the boot role claim
	// runs before claims/ready, so a listener that is briefly unreachable makes the boot throw, the
	// process exit, and the daemon charge a death (legion-claim-core.ts:108-113); hundreds of
	// concurrent boots against one blipping listener would turn a transient problem into a fleet of
	// budget-dead claims an operator must retry by hand. A small cap keeps that exposure bounded.
	maxReclaimsInFlight = 4
	// reclaimMinInterval staggers relaunch starts across the fleet: the daemon starts at most one new
	// reclaim this often, so a fleet-wide deaf state heals a few per minute and queues the rest.
	reclaimMinInterval = 15 * time.Second
	// maxReclaimAttempts bounds how many times the daemon relaunches one claim within one incident
	// before it gives up and logs once. A pane that boots but still cannot take its messages — a role
	// another live session holds makes a soft boot claim (the claude backend's) throw, and a registered
	// session whose process drops its forwards keeps failing them — would otherwise be relaunched for
	// every notice. After the bound the role stays unheld until the pane is seen taking its messages
	// again (reclaimHealedAfter); the operator's way to start it over is `legion claims suspend` then
	// `legion claims resume`, whose relaunch re-runs the claim boot.
	maxReclaimAttempts = 3
	// reclaimHealedAfter is how long a claim must take accepted publishes with no failed forward
	// reported before the daemon counts the pane healed and ends its incident, so a later, independent
	// loss starts with fresh attempts. An accepted publish alone proves nothing: the listener accepts a
	// publish for a session that is still registered and forwards it, and only after its receipt
	// window (about two seconds, longer while forwards queue on the role lane) reports whether the
	// pane took it (notice_exceptions.go) — the delivery-exception lane exists because a deaf pane's
	// stale registration keeps accepting.
	reclaimHealedAfter = time.Minute
	// maxReclaimCopies bounds how many re-held copies one claim's record maps to their own rows
	// (reclaimTry.copies), the newest kept. A report about an older copy is read as one published
	// before the relaunch, which never relaunches anything.
	maxReclaimCopies = 256
)

// reclaimCopy is one publish of a notice as the listener's report names it: the row whose dedupe key
// it went under (record.Notice.Published puts every re-held copy under its original's key) and its
// copy count (the published notice's Resends). The daemon publishes no id of its own, so a report
// cannot name a copy's own row; the record maps each copy to it at publish time (reclaimTry.copies).
type reclaimCopy struct {
	keyRow  int64
	resends int
}

// reclaimTry is one claim's reclaim record. Its rows are outbox row ids, read as points in time. The
// fence holds a notice behind the earlier notices of its tree that go to the same architect
// (earlierNoticeFor), and a re-held copy is a new row, so a later row to one architect mostly goes out
// after an earlier one. Not always: a row whose publish the listener refused, or that failed, is held
// and published again under its own id while the fence keeps the architect's later rows behind it,
// so a row stands for its first publish and every retry, and a report about it is placed at the row
// whichever publish it is about; and an earlier notice whose owner becomes this architect (the
// sub-architect that owned it stops) goes out after later rows already sent here.
//
//   - count, nextAt and gaveUp are the incident's attempts, its own backoff, and the give-up.
//   - relaunched says a relaunch of this record has started; bootRow is the first row published to
//     the claim after its latest relaunch booted (zero until one is), and a row below it is read as
//     one that went to a process the relaunch has replaced.
//   - failedRow is the highest row a failed-forward report named. A publish refused for want of a
//     holder does not raise it (noteReclaimFailed).
//   - lastPublished is the newest row published while the incident is open.
//   - copies maps each re-held copy published while the record exists to its own row.
//   - deliveredAt is the healing clock: when the listener accepted the first row at or above bootRow
//     and above failedRow, zero when it is not running. A failure at or above bootRow stops it.
//   - healedAt and healEnd: the clock stood reclaimHealedAfter and the incident ended at healedAt;
//     healEnd is the newest row published before. The record stays as a tombstone: a failure reported
//     late for a row in [bootRow, healEnd] was a copy the pane did not take inside the clock, so it
//     reopens the incident with its count; one above healEnd is a new loss, started fresh at its
//     admission; one below bootRow went to the replaced process and changes nothing.
type reclaimTry struct {
	count         int
	nextAt        time.Time
	gaveUp        bool
	relaunched    bool
	bootRow       int64
	failedRow     int64
	lastPublished int64
	copies        map[reclaimCopy]int64
	deliveredAt   time.Time
	healedAt      time.Time
	healEnd       int64
}

// healed is whether the record is a tombstone: its incident ended on healing evidence.
func (t reclaimTry) healed() bool { return !t.healedAt.IsZero() }

// ordinalOf is the row a reported copy went out as, and whether the record can tell: an original
// went out as its own key row; a re-held copy as the row the record mapped it to at its publish.
// A copy of a tombstone's that it did not map, of an original above healEnd, went out after the heal
// (every copy goes after its original), so its key row stands for it.
func (t reclaimTry) ordinalOf(c reclaimCopy) (int64, bool) {
	if c.resends == 0 {
		return c.keyRow, true
	}
	if row, ok := t.copies[c]; ok {
		return row, true
	}
	if t.healed() && c.keyRow > t.healEnd {
		return c.keyRow, true
	}
	return 0, false
}

// reclaimReservation is a claim admitted for a relaunch: handed once its relaunch was given to the
// machine, the record as admission found it (before, absent when hadRecord is false), restored if
// the relaunch launches nothing, and the fleet-wide stagger this admission set (staggerAt) over the
// one before it (staggerWas), rolled back only while it is still this admission's.
type reclaimReservation struct {
	handed     bool
	hadRecord  bool
	before     reclaimTry
	staggerAt  time.Time
	staggerWas time.Time
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
// The daemon addresses a role topic only for an architect's notices (the notice executor) and the
// merge-queue role (which does not call this), and the exception lane filters to architect reports,
// so this heals architect role claims and not a worker role (a worker is never daemon-role-addressed;
// it is driven on its worker stream) or the controller (it has its own session-side recovery). A
// peer agent's publish to an unheld running role is not seen by the daemon. Keeping this to
// architects is also what makes the no-live-holder guarantee below safe without a listener query: an
// architect's claim boot issues a HARD Envoy claim (it always lands, never 409s), so relaunching one
// onto a role a holder has since taken cannot crash-loop on boot. A phase worker's claude-backed boot
// claim is SOFT and WOULD 409 into a live holder, so extending this recognizer to worker roles must
// add a real holder check first.
//
// Both callers reach it only with "no live holder" established for the pane they name: the publish
// path is in its ErrNoHolder branch, and the exception path gates on the exception naming the claim's
// own current session and on a copy the claim's current process got (noteReclaimReport). The publish
// path runs the relaunch on its own lane; the exception path hands it off its callback
// (reclaimUnheldDetached). The machine charges a reclaim nothing itself (reclaimRole), and on the
// Kubernetes runtime the relaunch is uncharged: its Resume stops the role's earlier generation before
// it starts the new one (runtime/sandbox relaunch). On the tmux runtime it is charged one launch
// failure, by a known tmux-only defect (Resume refuses while the live pane it replaces runs).
func (r *outbox) reclaimUnheldRunning(ctx context.Context, token claim.Token, trigger string) {
	if relaunch := r.prepareReclaim(token, trigger); relaunch != nil {
		relaunch(ctx)
	}
}

// reclaimUnheldDetached is reclaimUnheldRunning for the delivery-exception lane: the decision and the
// admission run on the caller, and the relaunch on a goroutine of its own, since every architect's
// failed-forward report comes in on the one subscription callback (workflow.go) and a relaunch's
// machine decision lasts until the relaunched process is spawned — through the runtime's stop grace
// and launch backoff — during which no other report would be re-held. The goroutine runs while the
// outbox runs (Run opens it and its stop joins it, as it does its lanes); outside Run the relaunch
// runs on the caller. The reservation it took at admission covers the relaunch either way.
func (r *outbox) reclaimUnheldDetached(ctx context.Context, token claim.Token, trigger string) {
	relaunch := r.prepareReclaim(token, trigger)
	if relaunch == nil {
		return
	}
	r.reclaimGoMu.Lock()
	if !r.reclaimDetach {
		r.reclaimGoMu.Unlock()
		relaunch(ctx)
		return
	}
	r.reclaimWG.Add(1)
	r.reclaimGoMu.Unlock()
	go func() {
		defer r.reclaimWG.Done()
		relaunch(ctx)
	}()
}

// openReclaims lets reclaimUnheldDetached run relaunches on goroutines of their own; Run calls it.
func (r *outbox) openReclaims() {
	r.reclaimGoMu.Lock()
	defer r.reclaimGoMu.Unlock()
	r.reclaimDetach = true
}

// closeReclaims stops handing relaunches off and waits for every one already handed off: Run's stop
// calls it, so the daemon's stop joins them. A goroutine is added only under reclaimGoMu while
// detaching is on, so none is added once the wait begins.
func (r *outbox) closeReclaims() {
	r.reclaimGoMu.Lock()
	r.reclaimDetach = false
	r.reclaimGoMu.Unlock()
	r.reclaimWG.Wait()
}

// prepareReclaim is the recognizer both reclaim paths share: a dispatch on the claim's state as this
// daemon supervises it, which returns the relaunch to run, or nil to run none.
//
//   - ready or idle with no delivery in flight is relaunched through the limiter (admitReclaim). A
//     ready or idle claim that holds a pending delivery (sent, acknowledged, or awaiting its turn) is
//     in a turn in all but name: a task sent and acknowledged keeps the claim prompted (onAcked) until
//     the agent's turn-start frame, so the pane may be running tool calls, and a relaunch would lose
//     that work and re-send the task without the interrupted preamble. A pending delivery is the
//     out-of-turn condition quiesce.go checks, so such a pane waits for its turn's end.
//   - working is left for its turn to end. The caller holds the message (errNoticeWaits) or re-queues
//     it, so a later attempt catches the pane idle.
//   - a suspended claim is left alone: it runs no process to relaunch, and the relaunch that resumes
//     it (the operator's resume, or the daemon's by rule) re-runs the claim boot, which takes the role.
//   - every other state — booting, failed, retired, or not supervised — holds no running pane with a
//     role to re-claim.
func (r *outbox) prepareReclaim(token claim.Token, trigger string) func(context.Context) {
	machine, found := r.supervisor.Machine(token)
	if !found {
		return nil
	}
	view := machine.View()
	switch view.State {
	case supervise.StateReady, supervise.StateIdle:
		if view.Pending != nil || !r.admitReclaim(token) {
			return nil
		}
		return func(ctx context.Context) { r.relaunchUnheld(ctx, machine, token, trigger, view.Generation) }
	default:
		// A working pane waits for its turn to end; a booting pane claims the role on its own; a
		// suspended claim takes it when it is resumed; a failed, retired, or unsupervised claim holds
		// no role to re-claim.
		return nil
	}
}

// relaunchUnheld hands an admitted reclaim to the claim's machine and settles its reservation by what
// the machine did, read from the claim's generation, which every launch moves. Handle returns once the
// relaunched process is spawned; its boot follows. A relaunch that started keeps its slot, attempt and
// stagger, and its slot frees once the claim leaves the booting states (pruneReclaimLocked). A reclaim
// that launched nothing is undone as though it was never admitted (releaseReclaim): the launch path
// refused it (a reserved tree cleanup, a store error), the table ignored it because the claim left
// ready or idle after its view was read, or the claim still held a suspension its runtime had refused
// at its turn's end, which the reclaim completes instead of relaunching (replaceProcess's endHeld). A
// panic launches nothing as far as the limiter knows and is undone too, so it cannot strand a slot.
// The generation also moves when the claim relaunched for a reason of its own while Handle waited for
// the machine (a death, a stale address): that is counted as this reclaim's attempt and holds its
// slot until the claim has booted, which errs safe, since that relaunch re-runs the claim boot and
// re-claims the role as the reclaim would have.
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
// counts the attempt, stops any healing clock, and holds the fleet-wide stagger. A tombstone (an
// incident that healed) is a new incident here, started with fresh attempts. It returns false —
// relaunch nothing — when a relaunch of this claim is already reserved or booting, when the
// incident's attempts are spent (logged once, so a pane that cannot take its messages is visible
// instead of relaunched forever), when its own backoff or the fleet-wide stagger has not elapsed, or
// when the daemon-wide in-flight cap is reached. A refusal drops nothing on the publish lane: the held
// notice retries on the outbox backoff and reaches a slot once one frees. On the exception lane a
// refused admission is asked again only by the next copy's report, at most three per notice
// (noticeReholdDelays); a pane whose stale registration outlives them is asked again by its next
// notice's reports, or by the publish lane once that registration lapses and the listener refuses.
func (r *outbox) admitReclaim(token claim.Token) bool {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	r.pruneReclaimLocked()
	if _, inFlight := r.reclaimInFlight[token]; inFlight {
		return false
	}
	before, hadRecord := r.reclaimTries[token]
	try := before
	if try.healed() {
		try = reclaimTry{failedRow: try.failedRow}
	}
	if try.gaveUp {
		return false
	}
	if try.count >= cmp.Or(r.reclaimTryCap, maxReclaimAttempts) {
		try.gaveUp = true
		r.setReclaimTryLocked(token, try)
		r.log.Warn("outbox gave up relaunching a pane to re-claim its role; the role stays unheld until the pane is seen taking its messages again, or until the operator runs `legion claims suspend` then `legion claims resume` on the claim",
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
		r.reclaimInFlight = map[claim.Token]reclaimReservation{}
	}
	stagger := now.Add(cmp.Or(r.reclaimInterval, reclaimMinInterval))
	r.reclaimInFlight[token] = reclaimReservation{hadRecord: hadRecord, before: before, staggerAt: stagger, staggerWas: r.reclaimNextAt}
	try.count++
	try.nextAt = now.Add(reclaimBackoff(try.count))
	try.deliveredAt = time.Time{}
	r.setReclaimTryLocked(token, try)
	r.reclaimNextAt = stagger
	return true
}

// settleReclaim records what an admitted reclaim did. One that launched hands its reservation to the
// prune, which frees it once the claim has booted, and starts the record's relaunch boundary over:
// bootRow is set again by the first publish after this relaunch boots. One that launched nothing is
// undone.
func (r *outbox) settleReclaim(token claim.Token, launched bool) {
	if !launched {
		r.releaseReclaim(token)
		return
	}
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if res, ok := r.reclaimInFlight[token]; ok {
		res.handed = true
		r.reclaimInFlight[token] = res
	}
	if try, ok := r.reclaimTries[token]; ok {
		try.relaunched = true
		try.bootRow = 0
		r.reclaimTries[token] = try
	}
}

// releaseReclaim undoes an admitted reclaim that launched nothing: the slot frees, and the record is
// put back as admission found it — its attempt, backoff and clock, and a tombstone admission had
// started over — keeping what was learnt meanwhile (the copies published and the highest failed
// row). The fleet-wide stagger it set is rolled back only while it is still the one in force, so a
// refund that returns after another claim's admission does not erase that claim's stagger.
func (r *outbox) releaseReclaim(token claim.Token) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	res, ok := r.reclaimInFlight[token]
	if !ok {
		return
	}
	delete(r.reclaimInFlight, token)
	if r.reclaimNextAt.Equal(res.staggerAt) {
		r.reclaimNextAt = res.staggerWas
	}
	if !res.hadRecord {
		delete(r.reclaimTries, token)
		return
	}
	current := r.reclaimTries[token]
	restored := res.before
	restored.failedRow = max(restored.failedRow, current.failedRow)
	for c, row := range current.copies {
		if restored.copies == nil {
			restored.copies = map[reclaimCopy]int64{}
		}
		restored.copies[c] = row
	}
	r.reclaimTries[token] = restored
}

func (r *outbox) setReclaimTryLocked(token claim.Token, try reclaimTry) {
	if r.reclaimTries == nil {
		r.reclaimTries = map[claim.Token]reclaimTry{}
	}
	r.reclaimTries[token] = try
}

// noteReclaimPublishing records that the notice executor is about to publish row to token's
// architect, as copy, before the publish goes out, so a report about it — which can come back before
// the publish returns — finds it recorded. A re-held copy is mapped to its row (a tombstone maps only
// copies of originals published before its heal, the ones ordinalOf cannot place otherwise). The row
// is the incident's newest while it is open. The first row published after the record's latest
// relaunch booted — the claim has taken its role and holds no reservation — becomes bootRow, and
// copies mapped below it are dropped, since an unmapped copy reads the same.
func (r *outbox) noteReclaimPublishing(token claim.Token, copy reclaimCopy, row int64) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimTries[token]; !ok {
		return
	}
	r.pruneReclaimLocked()
	try, ok := r.reclaimTries[token]
	if !ok {
		return
	}
	if copy.resends > 0 && (!try.healed() || copy.keyRow <= try.healEnd) {
		if try.copies == nil {
			try.copies = map[reclaimCopy]int64{}
		}
		try.copies[copy] = row
		trimReclaimCopies(try.copies)
	}
	if !try.healed() {
		try.lastPublished = max(try.lastPublished, row)
		if _, inFlight := r.reclaimInFlight[token]; try.relaunched && try.bootRow == 0 && !inFlight && claimTookRole(r.claimState(token)) {
			try.bootRow = row
			for c, mapped := range try.copies {
				if mapped < row {
					delete(try.copies, c)
				}
			}
		}
	}
	r.reclaimTries[token] = try
}

// trimReclaimCopies drops the oldest mapped copies past maxReclaimCopies.
func trimReclaimCopies(copies map[reclaimCopy]int64) {
	for len(copies) > maxReclaimCopies {
		var oldest reclaimCopy
		first := true
		for c, row := range copies {
			if first || row < copies[oldest] {
				oldest, first = c, false
			}
		}
		delete(copies, oldest)
	}
}

// noteReclaimDelivered records that the listener accepted row, published to token's architect. The
// first such row at or above bootRow and above failedRow, while the claim has taken its role
// and holds no reservation, starts the healing clock. It never frees a slot and ends nothing by
// itself: a publish accepted while a relaunch still boots may have gone to the stopped session's
// registration, and one accepted for a pane that drops its forwards is followed by a failed-forward
// report (noteReclaimReport), which can also come in before this note (failedRow keeps the clock off
// then). The incident ends only once the clock has stood for reclaimHealedAfter (pruneReclaimLocked).
func (r *outbox) noteReclaimDelivered(token claim.Token, row int64) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimTries[token]; !ok {
		return
	}
	r.pruneReclaimLocked()
	try, ok := r.reclaimTries[token]
	if !ok || try.healed() || !try.deliveredAt.IsZero() || !try.relaunched || try.bootRow == 0 || row < try.bootRow || row <= try.failedRow {
		return
	}
	if _, inFlight := r.reclaimInFlight[token]; inFlight || !claimTookRole(r.claimState(token)) {
		return
	}
	try.deliveredAt = r.now()
	r.reclaimTries[token] = try
}

// noteReclaimFailed records that the listener refused row, published to token's architect, for want
// of a holder (the publish lane's ErrNoHolder): the failure of a row noteReclaimFailedLocked reads,
// which stops the healing clock when the row went to the current process. It does not raise
// failedRow. That guard is for a failed-forward report, which comes in on the exception lane and can
// be handled before the accepted publish it is about returns. This refusal comes back inside its own
// publish call, so no acceptance of that publish follows it. The row's next publish is its retry, and
// a row's publishes run one at a time on its lane, so the retry goes out after this refusal: accepted
// once a relaunch has re-claimed the role, it is the evidence the clock waits for. Raising failedRow
// here kept that retry from starting the clock, so an incident healed on the held row never ended,
// and separate resets added up to the give-up.
func (r *outbox) noteReclaimFailed(token claim.Token, row int64) {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimTries[token]; !ok {
		return
	}
	r.pruneReclaimLocked()
	if try, ok := r.reclaimTries[token]; ok {
		r.noteReclaimFailedLocked(token, try, row)
	}
}

// noteReclaimReport records that the listener reported a failed forward of copy to token's architect,
// and says whether the report is about a copy the claim's current process got, so that it may drive
// a relaunch. A claim with no record has no relaunch to measure against: every report counts. A copy
// the record cannot place went out before the record existed, so before its relaunch, when there was
// one.
func (r *outbox) noteReclaimReport(token claim.Token, copy reclaimCopy) bool {
	r.reclaimMu.Lock()
	defer r.reclaimMu.Unlock()
	if _, ok := r.reclaimTries[token]; !ok {
		return true
	}
	r.pruneReclaimLocked()
	try, ok := r.reclaimTries[token]
	if !ok {
		return true
	}
	ordinal, known := try.ordinalOf(copy)
	if !known {
		return !try.relaunched
	}
	try.failedRow = max(try.failedRow, ordinal)
	return r.noteReclaimFailedLocked(token, try, ordinal)
}

// noteReclaimFailedLocked applies the failure of row to token's record and says whether row went to
// the claim's current process. Before any relaunch of the record it did. On an open incident, a row at
// or above bootRow did, and stops the healing clock; one below went to the process the relaunch
// replaced and changes nothing. On a tombstone, a row above healEnd went out after the heal and is a
// new loss (admission starts it fresh); a row in [bootRow, healEnd] was not taken inside the clock, so
// the incident reopens with its count; one below bootRow changes nothing. It leaves failedRow to its
// caller: only a report raises it.
func (r *outbox) noteReclaimFailedLocked(token claim.Token, try reclaimTry, row int64) bool {
	current := true
	switch {
	case !try.relaunched:
	case try.healed():
		switch {
		case row > try.healEnd:
		case try.bootRow != 0 && row >= try.bootRow:
			try.healedAt, try.deliveredAt = time.Time{}, time.Time{}
		default:
			current = false
		}
	case try.bootRow != 0 && row >= try.bootRow:
		try.deliveredAt = time.Time{}
	default:
		current = false
	}
	r.reclaimTries[token] = try
	return current
}

// pruneReclaimLocked frees every in-flight reservation whose relaunch has been handed to the machine
// and has finished booting — the claim is prompted again, or has failed, retired, or suspended —
// forgets the record of a claim this daemon no longer supervises or that has failed or retired, and
// turns a record whose healing clock has stood for reclaimHealedAfter into a tombstone. Every note and
// admission runs it first, so a record heals by its own clock whatever else runs: healEnd is the newest
// row noted before the clock ran out, since the first publish noted after it heals the record first.
// A reservation whose relaunch has not yet been handed over is kept whatever its claim reads: until
// the machine moves it to launching it still reads ready, and freeing it then would let another
// reclaim past the cap. View never waits on a decision in flight, so a claim mid-relaunch is read
// without blocking (supervisedClaim).
func (r *outbox) pruneReclaimLocked() {
	for token, res := range r.reclaimInFlight {
		if res.handed && !reclaimBooting(r.claimState(token)) {
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
		if _, inFlight := r.reclaimInFlight[token]; inFlight || try.healed() || try.deliveredAt.IsZero() {
			continue
		}
		if !now.Before(try.deliveredAt.Add(reclaimHealedAfter)) {
			try.healedAt, try.healEnd, try.deliveredAt = now, try.lastPublished, time.Time{}
			r.reclaimTries[token] = try
		}
	}
}
