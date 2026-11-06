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

// compacting is the compaction a claim is in: the connection it runs over and the generation it
// was taken at, which is what the end of it is fenced on. It is memory only, as a held
// suspension is: a daemon that restarts mid-compaction leaves a claim stored working, which its
// agent's own answer to "are you streaming" then ends (askTurnOver), and the owed task goes on
// that end.
type compacting struct {
	conn       runtime.Conn
	generation uint64
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
// Nil is a session compacted. A compaction Oh My Pi refused or skipped — a session too small,
// one already compacted, a summarizer that failed — carries its words
// (runtime.RefusalReason); a wait that ran out, or a transport that failed, is an error of its
// own. The claim is given back and the owed task is sent again in every one of those cases, and
// only a compaction that ran puts the claim's run of provider-error turns back to zero
// (compactEnded).
func (m *Machine) Compact(ctx context.Context, customInstructions string) error {
	if strings.TrimSpace(customInstructions) == "" {
		return ErrCompactInstructions
	}
	token := m.View().Token
	if err := m.Handle(ctx, RequestCompact{Claim: token}); err != nil {
		return err
	}
	run, taken := m.compaction()
	if !taken {
		// The claim lost its process between the take and this read (letGo), which leaves it
		// suspended, failed or relaunching rather than working: its own path decides what
		// happens to it next, and there is no compaction to end.
		return fmt.Errorf("supervise: %s lost its process before its session could be compacted", token)
	}
	err := run.conn.Compact(ctx, customInstructions, runtime.CompactTimeout)
	// The end runs whatever the compaction did and whatever became of ctx — a caller that hung up
	// still leaves a claim to give back — so it is posted on a context the caller cannot cancel.
	ended := CompactEnded{Claim: token, Generation: run.generation, Compacted: err == nil}
	return errors.Join(err, m.Handle(context.WithoutCancel(ctx), ended))
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
func compactStart(m *Machine, ctx context.Context, _ Event) error {
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return &RefusedError{State: m.claim.State, Request: "compact",
			Reason: "the claim's shim is not connected, so nothing can be asked of its session"}
	}
	m.compacting = &compacting{conn: conn, generation: m.claim.Generation}
	m.claim.State = StateWorking
	m.log.Info("supervise: the claim's session is being compacted; it is busy until the compaction ends",
		"generation", m.claim.Generation, "session", m.claim.Session)
	if err := m.persist(ctx); err != nil {
		m.compacting = nil
		m.claim.State = StateIdle
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
func compactEnded(m *Machine, ctx context.Context, ev Event) error {
	ended := ev.(CompactEnded)
	m.compacting = nil
	m.disarm(TimerPromptRetry)
	if ended.Compacted {
		m.claim.ProviderErrorRun = 0
	}
	m.log.Info("supervise: the compaction of the claim's session is over", "compacted", ended.Compacted,
		"providerErrorRun", m.claim.ProviderErrorRun)
	if m.held != nil {
		// A suspension that arrived while the compaction ran was held for it, as one that
		// arrives in a turn is (holdSuspension); it runs now, exactly as the turn's end runs it.
		if err := m.suspendHeld(ctx); err != nil {
			if m.held == nil {
				return err
			}
			m.claim.State = StateIdle
			return errors.Join(err, m.persist(ctx))
		}
		return nil
	}
	m.claim.State = StateIdle
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.afterTurn(ctx)
}
