package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The events. Each carries the claim it is for and the fields its fences read: an observation its
// incarnation, a hello its generation, a replayed turn start its delivery id, a prompt outcome its
// generation and delivery id, a timer its arming, an agent's own request its generation and
// session.

// RuntimeObservation is a process fact from the runtime's sweep. The claim is its locator's.
type RuntimeObservation struct{ Observation runtime.Observation }

// StreamHello is the shim connecting with this claim's boot token, resolved to its generation.
type StreamHello struct {
	Claim      claim.Token
	Generation uint64
}

// StreamTurnStart is the agent's turn starting. Oh My Pi's own agent_start carries no delivery id;
// only the shim's replay of one, answering a repeated prompt, does.
type StreamTurnStart struct {
	Claim      claim.Token
	DeliveryID string
}

// StreamTurnEnd is the agent's turn ending.
type StreamTurnEnd struct{ Claim claim.Token }

// StreamClosed is the claim's connection closing. It says nothing about the process by itself.
type StreamClosed struct{ Claim claim.Token }

// StreamLateRefusal is Oh My Pi refusing a prompt after it acknowledged it: a failed response for
// the same request, following the successful one.
type StreamLateRefusal struct {
	Claim      claim.Token
	DeliveryID string
	Error      string
}

// PromptAcked is a send's prompt acknowledged, posted by the send's own goroutine.
type PromptAcked struct {
	Claim      claim.Token
	Generation uint64
	DeliveryID string
}

// PromptRefused is a send's prompt refused, or lost to the transport, posted by the send's own
// goroutine.
type PromptRefused struct {
	Claim      claim.Token
	Generation uint64
	DeliveryID string
	Err        error
}

// TimerKind names the waits a machine times.
type TimerKind string

const (
	// TimerBoot is the boot observation interval: an unregistered process is probed.
	TimerBoot TimerKind = "boot"
	// TimerRegistration is the registration deadline: a live process that never registered is
	// retired.
	TimerRegistration TimerKind = "registration"
	// TimerTurn is the wait for the turn an acknowledged prompt should start.
	TimerTurn TimerKind = "turn"
	// TimerProbe is the re-probe of an uncertain process.
	TimerProbe TimerKind = "probe"
)

// Timer is a timer the machine armed, firing.
type Timer struct {
	Claim      claim.Token
	Kind       TimerKind
	Generation uint64
	Seq        uint64
	DeliveryID string
}

// TreeVolumeLost is another claim of the claim's tree finding the tree volume lost (its
// workspace-init found neither the clone nor its session): whatever session this claim recorded
// was on that volume, and is gone with it. The daemon sends it to the tree's other claims.
type TreeVolumeLost struct{ Claim claim.Token }

// RequestSpawn launches a queued claim.
type RequestSpawn struct{ Claim claim.Token }

// RequestRegister is the agent's registration, already authenticated by its boot token: the
// session it became, the transcript it persists, and the hash of the secret it is being issued.
// The machine persists all three before Handle returns.
type RequestRegister struct {
	Claim          claim.Token
	Generation     uint64
	Session        string
	SessionFile    string
	CapabilityHash []byte
}

// RequestReady is the agent saying it can be prompted.
type RequestReady struct {
	Claim      claim.Token
	Generation uint64
	Session    string
}

// RequestSuspend stops the claim's process and keeps its session.
type RequestSuspend struct{ Claim claim.Token }

// RequestResume relaunches a suspended claim's session.
type RequestResume struct{ Claim claim.Token }

// RequestStop ends one claim. The tree's root claim is not one it can end: a root ends only with
// its tree, so any other stop of it is refused, and suspending it is how its process is stopped.
//
// A close is its own event rather than a flag here, and there are two of them, because who decided
// the close is what says whether TreeClosable is asked. A flag has an unsafe default, and the
// caller that never thought about it gets that default; these two make a new caller name which
// close it is making, and the table holds a row for each.
type RequestStop struct{ Claim claim.Token }

// RequestTreeClose is the workflow closing a tree: every claim of a tree whose linger expired,
// the root's included. It is the workflow's own decision and is not put to TreeClosable — the
// workflow still holds the issue record of the tree it is closing, which is the very record
// TreeClosable refuses on, so asking would refuse exactly the closes it is entitled to make.
type RequestTreeClose struct{ Claim claim.Token }

// RequestOperatorClose is the operator closing a tree through the API, through its root claim; the
// tree's other claims are stopped, not closed (api.closeTree). It is put to TreeClosable: an
// operator may close a tree no workflow issue backs, and a tree one does back closes when its
// linger expires, never by hand.
type RequestOperatorClose struct{ Claim claim.Token }

// RequestRetry relaunches a failed or retired claim's session with fresh budgets: the workflow's
// decision that the role runs again — the tree's architect retrying a held phase, or a closed tree
// re-admitted after its linger stopped every claim.
type RequestRetry struct{ Claim claim.Token }

// RequestDeliver gives the claim a task. ID is a durable outbox delivery id when an outbox row
// drives the request; an empty ID asks the machine to mint an ordinary operator delivery id.
//
// Phase is the issue phase the task is for, and says when the task stops being the work to do: a
// task of a phase the issue has left is dropped rather than sent, and a suspension, which ends
// that phase, retires it. The workflow names it; an operator's own delivery names none, is never
// dropped for its issue's phase, and waits out a suspension to be sent on the resume.
type RequestDeliver struct {
	Claim claim.Token
	Task  string
	ID    string
	Phase phase.Phase
	// Generation is the issue generation the task belongs to, and is what a completion reported
	// for it is attributed to. An operator's own task names none.
	Generation uint64
}

// RequestExit is the agent reporting its own end.
type RequestExit struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Reason     string
}

func (RuntimeObservation) isEvent()   {}
func (StreamHello) isEvent()          {}
func (StreamTurnStart) isEvent()      {}
func (StreamTurnEnd) isEvent()        {}
func (StreamClosed) isEvent()         {}
func (StreamLateRefusal) isEvent()    {}
func (PromptAcked) isEvent()          {}
func (PromptRefused) isEvent()        {}
func (Timer) isEvent()                {}
func (TreeVolumeLost) isEvent()       {}
func (RequestSpawn) isEvent()         {}
func (RequestRegister) isEvent()      {}
func (RequestReady) isEvent()         {}
func (RequestSuspend) isEvent()       {}
func (RequestResume) isEvent()        {}
func (RequestStop) isEvent()          {}
func (RequestTreeClose) isEvent()     {}
func (RequestOperatorClose) isEvent() {}
func (RequestRetry) isEvent()         {}
func (RequestDeliver) isEvent()       {}
func (RequestExit) isEvent()          {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because the four waits mean four different things.
type eventKind string

const (
	onObservation   eventKind = "runtime_observation"
	onHello         eventKind = "stream_hello"
	onTurnStart     eventKind = "stream_turn_start"
	onTurnEnd       eventKind = "stream_turn_end"
	onClosed        eventKind = "stream_closed"
	onLateRefusal   eventKind = "stream_late_refusal"
	onAcked         eventKind = "prompt_acked"
	onRefused       eventKind = "prompt_refused"
	onBootTimer     eventKind = timerPrefix + eventKind(TimerBoot)
	onDeadline      eventKind = timerPrefix + eventKind(TimerRegistration)
	onTurnTimer     eventKind = timerPrefix + eventKind(TimerTurn)
	onProbeTimer    eventKind = timerPrefix + eventKind(TimerProbe)
	onSpawn         eventKind = "request_spawn"
	onRegister      eventKind = "request_register"
	onReady         eventKind = "request_ready"
	onSuspend       eventKind = "request_suspend"
	onResume        eventKind = "request_resume"
	onStop          eventKind = "request_stop"
	onTreeClose     eventKind = "request_tree_close"
	onOperatorClose eventKind = "request_operator_close"
	onRetry         eventKind = "request_retry"
	onDeliver       eventKind = "request_deliver"
	onExit          eventKind = "request_exit"

	onVolumeLost eventKind = "tree_volume_lost"

	timerPrefix eventKind = "timer:"
)

func kindOf(ev Event) eventKind {
	switch ev := ev.(type) {
	case RuntimeObservation:
		return onObservation
	case StreamHello:
		return onHello
	case StreamTurnStart:
		return onTurnStart
	case StreamTurnEnd:
		return onTurnEnd
	case StreamClosed:
		return onClosed
	case StreamLateRefusal:
		return onLateRefusal
	case PromptAcked:
		return onAcked
	case PromptRefused:
		return onRefused
	case Timer:
		return timerPrefix + eventKind(ev.Kind)
	case TreeVolumeLost:
		return onVolumeLost
	case RequestSpawn:
		return onSpawn
	case RequestRegister:
		return onRegister
	case RequestReady:
		return onReady
	case RequestSuspend:
		return onSuspend
	case RequestResume:
		return onResume
	case RequestStop:
		return onStop
	case RequestTreeClose:
		return onTreeClose
	case RequestOperatorClose:
		return onOperatorClose
	case RequestRetry:
		return onRetry
	case RequestDeliver:
		return onDeliver
	case RequestExit:
		return onExit
	}
	return ""
}

// requestName is a request's name as a refusal says it; the other events are not requests.
func requestName(ev Event) (string, bool) {
	switch ev.(type) {
	case RequestSpawn:
		return "spawn", true
	case RequestRegister:
		return "register", true
	case RequestReady:
		return "ready", true
	case RequestSuspend:
		return "suspend", true
	case RequestResume:
		return "resume", true
	case RequestStop:
		return "stop", true
	case RequestTreeClose, RequestOperatorClose:
		return "close", true
	case RequestRetry:
		return "retry", true
	case RequestDeliver:
		return "deliver", true
	case RequestExit:
		return "exit", true
	}
	return "", false
}

type key struct {
	state ClaimState
	kind  eventKind
}

// rule is one cell of the table: a row — what the event does in that state, named, with the states
// it may leave the claim in besides the one it found — or an ignore, with the reason the event
// means nothing there. Handle refuses a request that meets an ignore, and holds every row to its
// destinations.
type rule struct {
	name   string
	to     []ClaimState
	act    func(m *Machine, ctx context.Context, ev Event) error
	ignore string
}

var (
	live        = []ClaimState{StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}
	booting     = []ClaimState{StateLaunching, StateShimConnected}
	prompted    = []ClaimState{StateReady, StateIdle}
	processless = []ClaimState{StateQueued, StateLaunchUncertain, StateSuspended, StateFailed, StateRetired}
	unready     = []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered}
	gone        = []ClaimState{StateSuspended, StateFailed, StateRetired}
)

const (
	noProcess     = "no process of this claim is running"
	noSend        = "no prompt is sent in this state"
	failedClaim   = "the claim failed; what happens to it next is not the supervisor's decision"
	retiredClaim  = "the claim is retired"
	notRegistered = "the agent has not registered"
)

// table is the transition table: a row or a named ignore for every event kind in every state.
// TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState reads the sealed Event set, the
// states, and the timer kinds from this package's source and holds the table to all of them.
//
// It is filled in init because its actions reach Handle (a timer they arm calls it), and Handle
// reads the table: as a package-level initializer that is a cycle the compiler refuses.
var table map[key]rule

func init() { table = build(fillTable) }

func fillTable(t *builder) {
	// What the runtime says about the process.
	t.row(onObservation, "judge the process; alive and ready or idle, send what is waiting", observe,
		[]ClaimState{StateLaunching, StateFailed, StateWorking}, live...)
	t.ignore(onObservation, noProcess, processless...)

	// The shim.
	t.row(onHello, "the shim connected", helloed, []ClaimState{StateShimConnected}, StateLaunching)
	t.ignore(onHello, "the shim reconnected before its agent registered", StateShimConnected)
	t.ignore(onHello, "the shim reconnected, or its agent's registration overtook its hello", StateRegistered)
	t.row(onHello, "the shim reconnected: send what is pending", reconnected, []ClaimState{StateWorking}, prompted...)
	t.row(onHello, "the shim reconnected mid-turn: after a restart, ask whether the turn is still running",
		reconnectedMidTurn, []ClaimState{StateIdle, StateWorking}, StateWorking)
	t.ignore(onHello, "no launch has minted a boot token", StateQueued)
	t.ignore(onHello, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onHello, noProcess, gone...)

	t.row(onClosed, "the connection closed: probe the process", closed, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onClosed, noProcess, processless...)

	// The prompt's own outcome, from the send's goroutine.
	t.row(onAcked, "the prompt was acknowledged: wait for its turn", acked, nil, prompted...)
	t.ignore(onAcked, "a turn already confirmed the delivery", StateWorking)
	t.ignore(onAcked, noSend, unready...)
	t.ignore(onAcked, noSend, gone...)

	t.row(onRefused, "the prompt was refused (charged) or lost (not charged): re-queue it", refused,
		[]ClaimState{StateLaunching, StateFailed}, StateReady, StateIdle, StateWorking)
	t.ignore(onRefused, noSend, unready...)
	t.ignore(onRefused, noSend, gone...)

	t.row(onLateRefusal, "the agent refused an acknowledged prompt", lateRefused,
		[]ClaimState{StateLaunching, StateFailed}, StateReady, StateIdle, StateWorking)
	t.ignore(onLateRefusal, noSend, unready...)
	t.ignore(onLateRefusal, noSend, gone...)

	// Turns.
	t.row(onTurnStart, "a turn started", turnStarted, []ClaimState{StateWorking}, prompted...)
	t.ignore(onTurnStart, "a turn is already in flight", StateWorking)
	t.ignore(onTurnStart, "the agent is not ready, so no delivery is in flight", unready...)
	t.ignore(onTurnStart, noProcess, gone...)

	t.row(onTurnEnd, "the turn ended", turnEnded, []ClaimState{StateIdle, StateWorking}, StateWorking)
	t.ignore(onTurnEnd, "no turn is in flight", prompted...)
	t.ignore(onTurnEnd, "the agent is not ready, so no turn of a delivery is in flight", unready...)
	t.ignore(onTurnEnd, noProcess, gone...)

	// Timers.
	t.row(onBootTimer, "the boot interval: probe the unregistered process", bootInterval,
		[]ClaimState{StateLaunching, StateFailed}, booting...)
	t.ignore(onBootTimer, "the agent registered, so the boot watch is over", StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onBootTimer, "no boot is being watched", processless...)

	t.row(onDeadline, "the registration deadline", registrationDeadline, []ClaimState{StateLaunching, StateFailed}, booting...)
	t.ignore(onDeadline, "the agent registered, so the boot watch is over", StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onDeadline, "no boot is being watched", processless...)

	t.row(onTurnTimer, "an acknowledged prompt started no turn", noTurn, []ClaimState{StateLaunching, StateFailed}, prompted...)
	t.ignore(onTurnTimer, "the turn started", StateWorking)
	t.ignore(onTurnTimer, noSend, unready...)
	t.ignore(onTurnTimer, noSend, gone...)

	t.row(onProbeTimer, "probe the uncertain process again", reprobe, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onProbeTimer, noProcess, processless...)

	// The tree's volume, found lost by another claim of the tree.
	t.row(onVolumeLost, "the tree volume was lost: drop the session it held", sessionLost, nil,
		slices.Concat(processless, booting)...)
	t.ignore(onVolumeLost, "the agent registered, so its session is on the volume its process runs on",
		StateRegistered, StateReady, StateWorking, StateIdle)

	// Requests.
	t.row(onSpawn, "launch", spawn, []ClaimState{StateLaunching, StateFailed}, StateQueued)
	t.ignore(onSpawn, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onSpawn, "the claim is already launched", live...)
	t.ignore(onSpawn, "a suspended claim is resumed, not spawned", StateSuspended)
	t.ignore(onSpawn, failedClaim, StateFailed)
	t.ignore(onSpawn, retiredClaim, StateRetired)

	t.row(onRegister, "the agent registered", register, []ClaimState{StateRegistered}, booting...)
	t.row(onRegister, "the recorded session registered again", reregister, nil,
		StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onRegister, "no launch has minted a boot token", StateQueued)
	t.ignore(onRegister, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRegister, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onRegister, failedClaim, StateFailed)
	t.ignore(onRegister, retiredClaim, StateRetired)

	t.row(onReady, "the agent is ready", ready, []ClaimState{StateReady, StateWorking}, StateRegistered)
	t.row(onReady, "the agent said ready again", reready, []ClaimState{StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onReady, notRegistered, booting...)
	t.ignore(onReady, "no launch has minted a boot token", StateQueued)
	t.ignore(onReady, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onReady, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onReady, failedClaim, StateFailed)
	t.ignore(onReady, retiredClaim, StateRetired)

	t.row(onSuspend, "suspend: stop the process, keep the session", suspend, []ClaimState{StateSuspended},
		StateRegistered, StateReady, StateWorking, StateIdle)
	t.row(onSuspend, "already suspended", nothingToDo, nil, StateSuspended)
	t.ignore(onSuspend, notRegistered,
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected)
	t.ignore(onSuspend, failedClaim, StateFailed)
	t.ignore(onSuspend, retiredClaim, StateRetired)

	t.row(onResume, "resume the same session", resume, []ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onResume, "the claim is not suspended", live...)
	t.ignore(onResume, "a queued claim is spawned, not resumed", StateQueued)
	t.ignore(onResume, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onResume, failedClaim, StateFailed)
	t.ignore(onResume, retiredClaim, StateRetired)

	stoppable := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered,
		StateReady, StateWorking, StateIdle, StateSuspended, StateFailed}
	t.row(onStop, "release the claim and retire it; the tree's root only at the tree's close", stop, []ClaimState{StateRetired}, stoppable...)
	t.row(onStop, "already retired", nothingToDo, nil, StateRetired)

	t.row(onTreeClose, "the workflow's close of the tree: release the claim and retire it", treeClose, []ClaimState{StateRetired}, stoppable...)
	t.row(onTreeClose, "already retired", nothingToDo, nil, StateRetired)

	t.row(onOperatorClose, "the operator's close of the tree, if no workflow issue backs it", operatorClose, []ClaimState{StateRetired}, stoppable...)
	// A close of a tree already retired is the outcome the operator asked for, so it answers as
	// the close that did it: an operator retrying after a timeout is not told their close failed.
	// It is still refused for a claim that is not its tree's root, and for a tree a workflow issue
	// backs: retiring is not a way past either.
	t.row(onOperatorClose, "the tree is already closed", operatorCloseRetired, nil, StateRetired)

	t.row(onRetry, "retry: fresh budgets, and the same session relaunched", retry, []ClaimState{StateLaunching, StateFailed}, StateFailed, StateRetired)
	t.ignore(onRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRetry, "the claim has not failed", live...)
	t.ignore(onRetry, "a suspended claim is resumed, not retried", StateSuspended)

	t.row(onDeliver, "queue the task; it goes when the claim is next ready or idle", deliverLater, nil,
		StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateWorking)
	t.ignore(onDeliver, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.row(onDeliver, "queue the task and send it", deliverNow, []ClaimState{StateWorking}, prompted...)
	t.row(onDeliver, "queue the task and resume the claim for it", deliverResuming,
		[]ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onDeliver, failedClaim, StateFailed)
	t.ignore(onDeliver, retiredClaim, StateRetired)

	t.row(onExit, "the agent reported its exit: release a worker's claim, suspend the tree's root", exit,
		[]ClaimState{StateRetired, StateSuspended}, StateRegistered, StateReady, StateWorking, StateIdle)
	t.row(onExit, "the exit of a process the daemon already ended", nothingToDo, nil, gone...)
	t.ignore(onExit, notRegistered, StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected)
}

type builder struct{ rules map[key]rule }

func build(fill func(*builder)) map[key]rule {
	b := &builder{rules: map[key]rule{}}
	fill(b)
	return b.rules
}

func (b *builder) put(k key, r rule) {
	if _, taken := b.rules[k]; taken {
		panic(fmt.Sprintf("supervise: two rules for %s in %s", k.kind, k.state))
	}
	b.rules[k] = r
}

func (b *builder) row(kind eventKind, name string, act func(*Machine, context.Context, Event) error, to []ClaimState, states ...ClaimState) {
	for _, state := range states {
		b.put(key{state, kind}, rule{name: name, to: to, act: act})
	}
}

func (b *builder) ignore(kind eventKind, reason string, states ...ClaimState) {
	for _, state := range states {
		b.put(key{state, kind}, rule{ignore: reason})
	}
}

// The actions. Each runs under the machine's lock, after the event passed its fences.

// observe is the runtime's sweep. A live process in a ready or idle claim is also the retry of a
// delivery that is waiting — after a refusal, a prompt that started no turn, or a missing
// connection — when nothing else will come: the retry is bounded by the sweep's interval and by
// the prompt budget, never made on the spot.
func observe(m *Machine, ctx context.Context, ev Event) error {
	return m.judge(ctx, ev.(RuntimeObservation).Observation, m.sendPending, TimerProbe, m.deps.Timeouts.Probe)
}

func helloed(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateShimConnected
	return m.persist(ctx)
}

func reconnected(m *Machine, ctx context.Context, _ Event) error {
	if m.send != nil {
		m.helloDuringSend = true
	}
	return m.sendPending(ctx)
}

// reconnectedMidTurn is the shim's hello while the claim is working. Within one daemon's life the
// shim's backlog replays whatever the turn did meanwhile, agent_end included; a restored claim
// may be waiting on an agent_end an earlier daemon received and never recorded, so its first
// hello asks.
func reconnectedMidTurn(m *Machine, ctx context.Context, ev Event) error {
	if !m.askFirst {
		return nil
	}
	m.askFirst = false
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok || m.streaming(ctx, conn) {
		return nil
	}
	m.log.Info("supervise: the turn an earlier daemon saw start is over")
	return turnEnded(m, ctx, ev)
}

func closed(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

func reprobe(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

func acked(m *Machine, ctx context.Context, _ Event) error {
	m.helloDuringSend = false
	p := m.claim.Pending
	p.DeliveredAt = m.deps.Clock.Now()
	m.markedBy = p.ID
	m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	return m.deps.Store.PutDelivery(ctx, m.claim.Token, *p)
}

// refused is a prompt that did not reach a turn from its own send: the agent refused it
// (runtime.ErrPromptRefused), or the transport lost it. Either way a turn that confirmed the send
// meanwhile was not the delivery's, and the delivery keeps its id — the shim drops a refused
// delivery's record, and answers a lost one it did see without a second turn.
//
// A refusal is the agent's answer and is charged like a prompt that started no turn (LEGION-60:
// an agent that refused every prompt would otherwise be retried forever); it is retried by the next
// sweep, hello, turn end, or ready, never on the spot. A prompt lost to the transport is not
// charged and waits for the shim's next hello — which may already have come while the send was out.
func refused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(PromptRefused)
	resend := m.helloDuringSend
	m.helloDuringSend = false
	if p := m.claim.Pending; !p.ConfirmedAt.IsZero() {
		p.ConfirmedAt = time.Time{}
		if err := m.deps.Store.PutDelivery(ctx, m.claim.Token, *p); err != nil {
			return err
		}
	}
	if errors.Is(r.Err, runtime.ErrPromptRefused) {
		m.log.Warn("supervise: the agent refused the prompt", "delivery", r.DeliveryID, "error", r.Err)
		return m.chargePrompt(ctx, "refused: "+r.Err.Error())
	}
	m.log.Warn("supervise: the prompt was lost to the transport; the delivery waits", "delivery", r.DeliveryID, "error", r.Err)
	if resend {
		return m.sendPending(ctx)
	}
	return nil
}

// lateRefused is the agent refusing a prompt it had acknowledged. OMP answers that way when it
// finds a turn it did not start — a notice delivered to the pane, a human's steer — and by the
// same route when the prompt fails before any turn begins, a provider answering "No API key
// found for …" among them. The claim's state stays what the stream observed, since a foreign
// turn may really be running.
//
// Which of the two it is, is the turn: one the stream already saw start, one Oh My Pi names in
// its own busy answer, or one the agent says it is in when asked. The busy answer is read
// because it is the authority — Oh My Pi refuses that way exactly when a turn is running — and
// because the ask can time out, where charging a working agent is the worse mistake. A turn of its own really running
// is the collision: the agent is working, not refusing to work, and the daemon delivers those
// notices itself, so a tree publishing them while a worker boots must not spend that worker's
// prompt budget. The task is taken back and the turn's end sends it, charged nothing.
//
// No turn running is the agent refusing the work. It is charged like any acknowledged prompt
// that becomes no turn, and the retry waits for the sweep that follows, so a refusal that never
// stops walks the budget to its end — a relaunch, and then the retirement — instead of looping
// on the spot for ever against an agent that cannot start a turn at all.
func lateRefused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamLateRefusal)
	// A refusal naming the prompt whose acknowledgement marked the task, where that is no longer the
	// pending id, says that prompt never ran, so the task loses its read mark — and nothing else.
	// The wait that gave up on it already re-queued the task under a new id and charged the prompt;
	// rotating or charging again would spend the budget twice for one prompt. A turn of the task's
	// own that is running meanwhile keeps its run: that is its confirmation, not this mark (markedBy).
	if r.DeliveryID != m.claim.Pending.ID {
		m.log.Warn("supervise: the prompt that marked the task as read was refused; the mark is cleared",
			"delivery", r.DeliveryID, "error", r.Error)
		return m.markUnread(ctx)
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if m.claim.State == StateWorking || agentBusy(r.Error) || (connected && m.streaming(ctx, conn)) {
		m.log.Warn("supervise: the agent refused an acknowledged prompt; it is in a turn of its own",
			"delivery", r.DeliveryID, "error", r.Error)
		// The agent is in a turn it started itself, so it never read this task: the delivery goes
		// back to waiting unread, and nothing the worker reports from that turn belongs to it.
		return m.takeBackPending(ctx, taskUnread)
	}
	m.log.Warn("supervise: the agent refused an acknowledged prompt and is in no turn; the prompt is charged",
		"delivery", r.DeliveryID, "error", r.Error)
	// The prompt did not run: the agent answered it a failure with no turn of its own to explain
	// it — a provider that rejected before any turn began, an agent that will not take the work.
	// The task goes back unread, so nothing reported from whatever turn follows belongs to it.
	return m.promptFailed(ctx, "refused an acknowledged prompt with no turn running: "+r.Error, taskUnread)
}

// agentBusy is Oh My Pi's own refusal of a prompt for a turn already running, which it words
// exactly this way. Every other rejection — a provider that answered before any turn began among
// them — is the agent failing to take the work. Reading the message is the daemon's only handle
// on which it is: the wire carries a string, not a kind.
func agentBusy(reason string) bool {
	return strings.HasPrefix(reason, "Agent is already processing")
}

// turnStarted is a turn starting in a ready or idle claim. It confirms the pending delivery when
// it is that delivery's turn: a replay naming it, or — B1 — Oh My Pi's own agent_start while the
// delivery is in flight (sent and not yet answered, acknowledged and waiting, or possibly sent by
// an earlier daemon). Any other turn is a foreign one, and confirms nothing.
func turnStarted(m *Machine, ctx context.Context, ev Event) error {
	start := ev.(StreamTurnStart)
	m.claim.State = StateWorking
	p := m.claim.Pending
	_, awaiting := m.timers[TimerTurn]
	inFlight := m.send != nil || awaiting || m.askFirst
	if p != nil && p.ConfirmedAt.IsZero() && (start.DeliveryID == p.ID || (start.DeliveryID == "" && inFlight)) {
		return m.confirm(ctx)
	}
	return m.persist(ctx)
}

// turnEnded is the turn over: the delivery it confirmed retires, and one queued meanwhile goes. The
// agent just said where it is, so nothing is left to ask it after a restart.
func turnEnded(m *Machine, ctx context.Context, _ Event) error {
	m.askFirst = false
	m.claim.State = StateIdle
	if err := m.persist(ctx); err != nil {
		return err
	}
	if err := m.settle(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

func bootInterval(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(context.Context) error {
		m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		return nil
	}, TimerBoot, m.deps.Timeouts.Boot)
}

// registrationDeadline is a process that has had its boot intervals and whose agent never
// registered: alive, it is retired — suspended, since the claim is relaunched — and counted, the
// third meaning of a missing worker; dead, it is counted. A suspension that fails leaves everything
// and tries again at the next probe interval.
func registrationDeadline(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(ctx context.Context) error {
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent never registered: suspend: %w", m.claim.Token, err)
		}
		m.log.Warn("supervise: the agent never registered; retired its process", "incarnation", incarnation)
		return m.relaunchAfterFailure(ctx)
	}, TimerRegistration, m.deps.Timeouts.Probe)
}

func noTurn(m *Machine, ctx context.Context, _ Event) error {
	// The bound alone keeps the mark: a turn that has not started within it may still be this
	// task's, starting late, and the agent read the prompt when the turn began.
	return m.promptFailed(ctx, fmt.Sprintf("acknowledged, and no turn started within %s", m.deps.Timeouts.RPC), taskRead)
}

func spawn(m *Machine, ctx context.Context, _ Event) error { return m.launch(ctx) }

// sessionLost is another claim of the tree finding the tree volume lost: the session this claim
// recorded was on it, so the claim drops it and its next launch is a fresh session that recreates
// its workspace — never a resume that finds the session missing (beside a clone another claim may
// already have recreated) and fails until its budget runs out. A claim with no session has nothing
// to drop.
func sessionLost(m *Machine, ctx context.Context, _ Event) error {
	if m.claim.Session == "" && m.claim.SessionFile == "" {
		return nil
	}
	m.log.Warn("supervise: the tree volume the session lived on was lost; the next launch is a fresh session", "session", m.claim.Session)
	m.loseSession()
	return m.persist(ctx)
}

// register records the session the agent became; a claim whose workspace was lost has its fresh
// agent now, whose session is on the recreated volume, so the loss is over.
func register(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	m.claim.Session, m.claim.SessionFile, m.claim.WorkspaceLost = r.Session, r.SessionFile, false
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.State = StateRegistered
	m.disarm(TimerBoot)
	m.disarm(TimerRegistration)
	return m.persist(ctx)
}

func reregister(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	m.claim.SessionFile = r.SessionFile
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	return m.persist(ctx)
}

func ready(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateReady
	m.claim.Budgets.LaunchFailures = 0
	if err := m.persist(ctx); err != nil {
		return err
	}
	m.terminal(StateReady)
	return m.sendPending(ctx)
}

func reready(m *Machine, ctx context.Context, _ Event) error { return m.sendPending(ctx) }

// suspend stops the process and keeps the session. A suspension ends the claim's phase, so a task
// queued for a phase and still pending unconfirmed (acknowledged and then refused, or lost to the
// transport) is retired with it (settle): the next resume is started with its new phase's task,
// never handed the finished one's. A task of no phase — an operator's own, an architect's — is
// not the workflow's to end, and goes on that resume.
func suspend(m *Machine, ctx context.Context, _ Event) error {
	if err := m.suspendProcess(ctx); err != nil {
		return fmt.Errorf("suspend %s: %w", m.claim.Token, err)
	}
	return m.suspended(ctx)
}

// suspended moves the claim to suspended: its session kept, and the process it stopped let go for
// the resume to wait out. It only persists — revoking the stopped agent's capability, in memory
// even when the write fails. The finished phase's unconfirmed task is retired after it by settle,
// which Handle runs after every row; a retirement that fails is reported, and the claim's next
// decision retires it before anything else.
func (m *Machine) suspended(ctx context.Context) error {
	m.letGo()
	m.claim.State = StateSuspended
	return m.persist(ctx)
}

func resume(m *Machine, ctx context.Context, _ Event) error { return m.launch(ctx) }

// retry is a failed or retired claim given another run: its budgets start over, and its session,
// when it has one, is relaunched after the process the claim last ran is gone. A pending delivery
// the claim kept goes once the agent is ready.
func retry(m *Machine, ctx context.Context, _ Event) error {
	m.claim.Budgets = Budgets{}
	return m.launch(ctx)
}

// stop ends one claim: the runtime releases it, and it retires. A release that fails changes
// nothing, so the stop can be asked again. The tree's root claim ends only with its tree: a
// retired root would leave the orphan sweep's known set, which would then take whatever the
// runtime holds for the tree — under a sandbox, the tree volume. Any other stop of it is refused.
func stop(m *Machine, ctx context.Context, _ Event) error {
	if m.claim.treeRoot() {
		return &RefusedError{State: m.claim.State, Request: "stop", Err: rootStopRefusal(m.claim.State)}
	}
	return m.end(ctx)
}

// treeClose is the workflow's close of the tree, which ends the root claim too. It is the
// workflow's own decision: it stops every claim of a tree whose linger expired, and the issue
// record it still holds for that tree is exactly what TreeClosable refuses on, so it is not asked.
func treeClose(m *Machine, ctx context.Context, _ Event) error { return m.end(ctx) }

// operatorClose is the operator's own close, asked of a claim through the API. TreeClosable is
// asked here rather than by the caller, so the answer and the stop it decides sit together rather
// than a round trip apart; it does not lock the record it reads.
func operatorClose(m *Machine, ctx context.Context, _ Event) error {
	if err := m.closeRefusal(ctx); err != nil {
		return err
	}
	return m.end(ctx)
}

// operatorCloseRetired answers a close of a claim that has already retired. The outcome the
// operator asked for holds, so an operator retrying after a timeout is not told their close
// failed — but only for a close that would have been allowed: a claim that is not its tree's
// root, and a tree a workflow issue backs, are refused whatever state the claim is in. The caller
// reads a nil here as the close that did it and goes on to stop the rest of the tree, which is
// the whole tree's fate to hand to a claim that never held it.
func operatorCloseRetired(m *Machine, ctx context.Context, _ Event) error {
	return m.closeRefusal(ctx)
}

// closeRefusal is the refusal an operator's close earns, or nil. It is the two questions the
// close turns on, asked where the close is decided rather than a round trip before it: whether
// this claim is its tree's root, and whether a workflow issue backs the tree. Neither reads the
// claim's state, so a retired claim answers them the same way a live one does.
func (m *Machine) closeRefusal(ctx context.Context) error {
	if !m.claim.treeRoot() {
		return &RefusedError{State: m.claim.State, Request: "close",
			Err: fmt.Errorf("%s is not its tree's root claim; stop it instead", m.claim.Token)}
	}
	if m.deps.TreeClosable == nil {
		return nil
	}
	closable, err := m.deps.TreeClosable(ctx, m.claim)
	if err != nil {
		return fmt.Errorf("close %s: %w", m.claim.Token, err)
	}
	if !closable {
		return &RefusedError{State: m.claim.State, Request: "close",
			Err: fmt.Errorf("%s is a workflow issue's tree, which closes when its linger expires", m.claim.Tree)}
	}
	return nil
}

// end releases the claim's process and retires the claim, which is what every stop and close does
// once it is allowed.
func (m *Machine) end(ctx context.Context) error {
	if err := m.release(ctx); err != nil {
		return err
	}
	return m.retire(ctx)
}

// rootStopRefusal is ErrRootStop with what stops the root's process in state instead: a suspension,
// which takes a registered agent, and nothing where no process runs.
func rootStopRefusal(state ClaimState) error {
	switch {
	case slices.Contains(processless, state):
		return fmt.Errorf("%w; %s", ErrRootStop, noProcess)
	case slices.Contains(booting, state):
		return fmt.Errorf("%w; suspend it to stop its process once its agent has registered", ErrRootStop)
	default:
		return fmt.Errorf("%w; suspend it to stop its process", ErrRootStop)
	}
}

func deliverLater(m *Machine, ctx context.Context, ev Event) error {
	return m.queue(ctx, ev.(RequestDeliver))
}

func deliverNow(m *Machine, ctx context.Context, ev Event) error {
	if err := m.queue(ctx, ev.(RequestDeliver)); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

func deliverResuming(m *Machine, ctx context.Context, ev Event) error {
	if err := m.queue(ctx, ev.(RequestDeliver)); err != nil {
		return err
	}
	return m.launch(ctx)
}

// exit is the agent reporting its own end. A worker's or sub-architect's claim ends with it and is
// released. The tree's root claim ends only with its tree, whose close is a stop: its exit suspends
// it instead, so it stays resumable and known to the orphan sweep, which would otherwise take
// whatever the runtime holds for the tree with it. The agent has ended either way, so a runtime
// that cannot release or suspend its process is logged and the claim moves all the same: a
// finished agent's claim never stays live, its secret still authenticating.
func exit(m *Machine, ctx context.Context, ev Event) error {
	m.log.Info("supervise: the agent reported its exit", "reason", ev.(RequestExit).Reason)
	if m.claim.treeRoot() {
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx); err != nil {
			m.log.Error("supervise: could not suspend the exited root's process; suspending its claim anyway",
				"incarnation", incarnation, "error", err)
		}
		return m.suspended(ctx)
	}
	if err := m.release(ctx); err != nil {
		m.log.Error("supervise: could not release the exited agent's process; retiring its claim anyway", "error", err)
	}
	return m.retire(ctx)
}

func nothingToDo(*Machine, context.Context, Event) error { return nil }
