package supervise

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim whose session a provider keeps refusing is compacted rather than started again: the
// session, its workspace and everything it knows stay, and what goes is the length of the
// transcript the provider is being asked to read (dispatch://INFRA-956 R3). `legion claims
// compact` and the controller's own compact both run it (api's compact), and this file is the
// machine's half: taking the claim, asking Oh My Pi, and giving the claim back with its owed
// task sent again.

// ErrCompactInstructions is a compaction asked for with nothing to write the summary to. Oh My
// Pi runs the image-based snapcompact method for a compaction that carries no instructions and
// no internal guidance (`runsManually`, packages/coding-agent/src/session/session-maintenance.ts
// at the pinned tag), and a worker resumed on images needs a vision model to read its own past:
// the instructions are what make the summary text, so a compaction with none is refused before
// anything is sent.
var ErrCompactInstructions = errors.New("a compaction carries custom instructions, so Oh My Pi writes its summary as text rather than snapcompact's images")

// ErrCompactNotTaken is a compaction nothing was asked for: the claim could not be written as
// working, it lost its process between the take and the ask, or its shim is not connected, so
// the daemon had nothing to ask through (compactStart). The claim is where the request found it
// and no task was sent, where every other outcome gives the claim back and sends the task it
// owes (compactEnded).
var ErrCompactNotTaken = errors.New("the claim was not taken for a compaction, so nothing was asked of its session")

// ErrCompactNotEnded is a compaction whose end the store would not write: Oh My Pi answered and
// the write that gives the claim back failed, so the claim is left working and the task it owes
// is unsent. It is the store's failure and not a compaction that did not run, whose answer
// promises the claim is back and its task has gone again (api's compact). What a daemon that
// stops here leaves behind is the shape a restart already knows: a claim stored working, whose
// agent's own answer to "are you streaming" ends it (askTurnOver).
var ErrCompactNotEnded = errors.New("the compaction ended and the claim could not be given back, so the task it owes was not sent")

// ErrCompactProcessGone is a compaction whose end found it was no longer the claim's own
// (ownsCompaction): the process it ran on was let go, a relaunch has moved the claim past it, or
// a launch waits to replace that process. So compactEnded never ran: the claim was not given
// back and the task it owes was not sent.
//
// What happens to the claim next is its own path's, and that path is not always a relaunch. A
// claim whose death the daemon relaunches is sent the owed task when its agent is ready again
// (sendPending), but a claim its death failed and one a held suspension stopped each run again
// only when an operator retries or resumes them, and the task waits for that.
var ErrCompactProcessGone = errors.New("the claim lost its process while its session was being compacted, so it was not given back and the task it owes was not sent")

// compacting is the compaction a claim is in: the connection it runs over and the generation it
// was taken at. Its presence and that generation are two of the three things the end of a
// compaction is fenced on (ownsCompaction). It is memory only, as a held suspension is: a
// daemon that restarts mid-compaction leaves a claim stored working, which its agent's own
// answer to "are you streaming" then ends (askTurnOver), and the owed task goes on
// that end.
type compacting struct {
	conn       runtime.Conn
	generation uint64
	// back is the state the claim is given back in: the state the take found it in
	// (compactStart), so a claim that was ready is ready again rather than idle, which would
	// say a turn of it had ended. A turn that starts or ends under the compaction makes it
	// idle, since the claim has had a turn by then (turnStartedWhileBusy, turnEnded).
	back ClaimState
	// inTurn is whether a turn Oh My Pi started under the compaction is still running: an
	// Envoy follow-up it queued, which starts as the compaction ends. The claim is left
	// working for that turn and its end gives the claim back (compactEnded, turnEnded).
	inTurn bool
}

// Compact compacts the claim's Oh My Pi session and sends the task it owes again. It is no event
// of its own, as ReleaseUncertainLaunch and PodUpdateTold are none: the compaction is a model
// call over the whole transcript, up to runtime.CompactTimeout, and a decision that waited it out
// would hold the machine's lock for ten minutes — every other request on the claim, a
// fresh_session's read of its state among them, would wait rather than be answered. So it is
// three steps: the claim is taken under the lock (RequestCompact), Oh My Pi is asked with no lock
// held, and the claim is given back under the lock (CompactEnded).
//
// While it runs the claim is working, which is the daemon's one record of a busy agent: the
// ladder's re-send waits, a newer task waits, an update round's question waits, a second
// compaction and a fresh session are refused, and the idle release does not take the pod —
// each because the site that decides it already asks whether the claim is working, and none of
// them has learned a second word for busy (compactStart).
//
// Nil is a session compacted and the claim given back. A compaction Oh My Pi refused or skipped
// — a session too small, one already compacted, a summarizer that failed — carries its words
// (runtime.RefusalReason); a wait that ran out, or a transport that failed, is an error of its
// own. The claim is given back and the owed task is sent again in every one of those cases, and
// only a compaction that ran puts the claim's run of provider-error turns back to zero
// (compactEnded). A take that did not happen at all is ErrCompactNotTaken or a *RefusedError,
// and leaves the claim where it was with its task unsent.
//
// A compaction that ran and did not end is neither, and the end's own answer is what tells the
// two apart: ErrCompactNotEnded is the end the store would not write, and ErrCompactProcessGone
// the end the machine judged no longer the claim's own. That judgement is made under the
// machine's lock, where the claim cannot move under it (ownsCompaction), and it is the whole of
// what is read here: nothing of the claim is read from this side of the lock, where a relaunch
// the end itself raises would be taken for the process the compaction lost (afterTurn's
// refreshPlugin). Each leaves the owed task unsent, and each says so in the daemon's own words:
// what Oh My Pi answered such a compaction goes to the log rather than to the caller, which no
// model's words reach through (api's reasonBound).
func (m *Machine) Compact(ctx context.Context, customInstructions string) error {
	if strings.TrimSpace(customInstructions) == "" {
		return ErrCompactInstructions
	}
	token := m.View().Token
	var refused *RefusedError
	switch err := m.Handle(ctx, RequestCompact{Claim: token}); {
	case errors.As(err, &refused):
		// The claim is in no state to be compacted, and the refusal names that state: its
		// caller reads which refusal it is from there (api's refuseState).
		return err
	case err != nil:
		return errors.Join(ErrCompactNotTaken, err)
	}
	run, taken := m.compaction()
	if !taken {
		// The claim lost its process between the take and this read (letGo), which leaves it
		// suspended, failed or relaunching rather than working: its own path decides what
		// happens to it next, and there is no compaction to end.
		return fmt.Errorf("%w: %s lost its process before its session could be compacted", ErrCompactNotTaken, token)
	}
	err := run.conn.Compact(ctx, customInstructions, runtime.CompactTimeout)
	// The end runs whatever the compaction did and whatever became of ctx — a caller that hung up
	// still leaves a claim to give back — so it is posted on a context the caller cannot cancel.
	ended := CompactEnded{Claim: token, Generation: run.generation, Compacted: err == nil}
	endErr := m.Handle(context.WithoutCancel(ctx), ended)
	if endErr == nil {
		// compactEnded ran: the claim is back and the task it owes has gone again, so what this
		// answers is the compaction itself.
		return err
	}
	if err != nil {
		m.log.Warn("supervise: the compaction whose end did not run was itself refused or unanswered",
			"generation", run.generation, "error", err)
	}
	if errors.Is(endErr, ErrCompactProcessGone) {
		// The machine answered, under its lock, that the compaction is no longer the claim's own
		// (ownsCompaction): compactEnded did not run and sent nothing.
		return ErrCompactProcessGone
	}
	return fmt.Errorf("%w: %w", ErrCompactNotEnded, endErr)
}

// compaction is the compaction in flight, and false when none is.
func (m *Machine) compaction() (compacting, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compacting == nil {
		return compacting{}, false
	}
	return *m.compacting, true
}

// ownsCompaction is whether the compaction taken at generation is still the claim's own, which
// is the one thing the end of a compaction turns on and the only place it can be read: the
// machine's lock is what makes the three facts one answer, and Machine.Compact holds no lock
// while Oh My Pi writes the summary. It is asked where the end is fenced (Machine.fence), so
// every caller of it reads the claim as the end will find it.
//
// A compaction is the claim's own when the claim records one, since letGo drops the compaction
// with the process it ran on; when it is the one this end names, since launchNow's bump leaves
// an earlier generation's behind; and when no launch waits to replace the process it ran on
// (launchWaits), which is the whole of what a death whose relaunch the wake pacing parked leaves
// on the claim — it is still working, still over that process and still at that generation
// (parkWake), and nothing else of the claim waits there.
func (m *Machine) ownsCompaction(generation uint64) bool {
	return m.compacting != nil && m.compacting.generation == generation && !m.launchWaits()
}

// compactStart takes the claim for a compaction: it is working until the compaction ends
// (compactEnded), and that is the whole of how the rest of the daemon knows not to disturb it.
// Working is the state every one of those readers already asks about — sendPending hands a claim
// that is not ready or idle nothing, so the ladder's re-send and any newer task wait;
// askForUpdate sends the task the claim holds rather than its own question; the idle rule holds
// the pod (workflow's idleStateReleasable); a fresh session is refused in a turn (api's
// freshSession); and this row is an ignore in working, so a second compaction is refused — and
// the claim is in no turn, so nothing is lost by saying the agent is busy in the one word the
// daemon already has for it.
//
// It is written, because the idle rule and the daemon's sweeps read the claim from the store
// rather than from the machine. A claim left working by a daemon that stopped mid-compaction is
// the shape a restart already knows: it asks the agent whether the turn is still running and
// ends it when the agent says it is not (askTurnOver), and the owed task goes on that end.
//
// A prompt the agent has not answered yet is taken back before anything is asked. Oh My Pi runs
// one thing at a time, so the compaction aborts the turn that prompt started, and the result of
// an aborted turn carries no error of its own (shimwire.PromptResult): left as it was, the task
// would be confirmed by that result and retired as worked at the end of the compaction, unsent
// (promptResulted, settle). Taken back it is unconfirmed under a new id, which the result of
// the aborted prompt no longer names (Machine.fence), and the end of the compaction sends it.
// A task re-sent after work that may have run is this operation's lesser fault, as it is at
// every other send that may repeat one (sendPending).
func compactStart(m *Machine, ctx context.Context, _ Event) error {
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// No refusal of the claim's state: the state takes a compaction, and what is missing is
		// the connection to ask through. Machine.Compact answers it as a take that did not
		// happen, so its caller reports the one failure it is rather than reading the claim's
		// state for a cause that state does not hold (api's compact).
		return errors.New("the claim's shim is not connected, so nothing can be asked of its session")
	}
	if m.claim.Pending != nil && (m.send != nil || m.resultPending()) {
		m.log.Info("supervise: the compaction aborts the turn this task's prompt is running; the task goes back to waiting",
			"delivery", m.claim.Pending.ID)
		if err := m.takeBackPending(ctx, taskRead); err != nil {
			return err
		}
	}
	was := m.claim.State
	m.compacting = &compacting{conn: conn, generation: m.claim.Generation, back: was}
	m.claim.State = StateWorking
	m.log.Info("supervise: the claim's session is being compacted; it is busy until the compaction ends",
		"generation", m.claim.Generation, "session", m.claim.Session)
	if err := m.persist(ctx); err != nil {
		m.compacting = nil
		m.claim.State = was
		return err
	}
	return nil
}

// compactEnded gives the claim back once Oh My Pi has answered, or once the wait for it ran out.
// It is turnEnded's shape, for the same reason: the claim was working, nothing of it is working
// now, and what was held for that is owed now — the suspension an operator asked for while the
// compaction ran, the update round's ask, and the task the claim owes, which goes again whether
// the compaction ran or was refused (dispatch://INFRA-956 R3). A claim that owes none is sent
// nothing.
//
// A compaction that ran puts the claim's run of provider-error turns back to zero, so the
// project's controller can be told a second time if the compacted session's turns go the same
// way; without it the controller would be told once and never again until a turn ended with no
// error at all. It is this action's own reset and this action's own write: forgetSession's reset
// belongs to a claim that is losing its session, and a compaction keeps it. A compaction that
// was refused or skipped resets nothing — the session is as it was, so the run of refused turns
// it carried still stands.
//
// The ladder's wait goes whatever the compaction did (TimerPromptRetry), so the owed task is
// sent from here rather than at the end of a wait that was counted against the transcript this
// operation just shortened: the whole point of compacting a claim the provider keeps refusing
// is to put the task in front of it again, and the next step of that ladder can be ten minutes
// away (promptRetryDelay). What the ladder counted stays on the delivery
// (Delivery.ProviderErrorRetries), so a turn that fails again waits the step it had reached.
//
// A turn Oh My Pi started under the compaction and has not ended — a follow-up an Envoy
// delivery queued while the session was being compacted — leaves the claim working for that
// turn, and that turn's end gives the claim back and sends the owed task (turnEnded). Idling
// the claim here would send the owed task into a running turn and leave the daemon reading the
// agent as free while it works.
func compactEnded(m *Machine, ctx context.Context, ev Event) error {
	ended := ev.(CompactEnded)
	// The fence let this end through, so the claim holds the compaction it names (ownsCompaction).
	run := *m.compacting
	m.compacting = nil
	m.disarm(TimerPromptRetry)
	if ended.Compacted {
		m.claim.ProviderErrorRun = 0
	}
	m.log.Info("supervise: the compaction of the claim's session is over", "compacted", ended.Compacted,
		"providerErrorRun", m.claim.ProviderErrorRun, "inTurn", run.inTurn)
	if run.inTurn {
		// The claim stays working and keeps whatever is held for a turn's end, the suspension
		// among it: this is not that end.
		return m.persist(ctx)
	}
	if m.held != nil {
		// A suspension that arrived while the compaction ran was held for it, as one that
		// arrives in a turn is (holdSuspension); it runs now, exactly as the turn's end runs it.
		if err := m.suspendHeld(ctx); err != nil {
			if m.held == nil {
				return err
			}
			m.claim.State = run.back
			return errors.Join(err, m.persist(ctx))
		}
		return nil
	}
	m.claim.State = run.back
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.afterTurn(ctx)
}
