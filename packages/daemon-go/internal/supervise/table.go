package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// RequestStop ends the claim.
type RequestStop struct{ Claim claim.Token }

// RequestDeliver gives the claim a task. ID is a durable outbox delivery id when an outbox row
// drives the request; an empty ID asks the machine to mint an ordinary operator delivery id.
type RequestDeliver struct {
	Claim claim.Token
	Task  string
	ID    string
}

// RequestExit is the agent reporting its own end.
type RequestExit struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Reason     string
}

func (RuntimeObservation) isEvent() {}
func (StreamHello) isEvent()        {}
func (StreamTurnStart) isEvent()    {}
func (StreamTurnEnd) isEvent()      {}
func (StreamClosed) isEvent()       {}
func (StreamLateRefusal) isEvent()  {}
func (PromptAcked) isEvent()        {}
func (PromptRefused) isEvent()      {}
func (Timer) isEvent()              {}
func (RequestSpawn) isEvent()       {}
func (RequestRegister) isEvent()    {}
func (RequestReady) isEvent()       {}
func (RequestSuspend) isEvent()     {}
func (RequestResume) isEvent()      {}
func (RequestStop) isEvent()        {}
func (RequestDeliver) isEvent()     {}
func (RequestExit) isEvent()        {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because the four waits mean four different things.
type eventKind string

const (
	onObservation eventKind = "runtime_observation"
	onHello       eventKind = "stream_hello"
	onTurnStart   eventKind = "stream_turn_start"
	onTurnEnd     eventKind = "stream_turn_end"
	onClosed      eventKind = "stream_closed"
	onLateRefusal eventKind = "stream_late_refusal"
	onAcked       eventKind = "prompt_acked"
	onRefused     eventKind = "prompt_refused"
	onBootTimer   eventKind = timerPrefix + eventKind(TimerBoot)
	onDeadline    eventKind = timerPrefix + eventKind(TimerRegistration)
	onTurnTimer   eventKind = timerPrefix + eventKind(TimerTurn)
	onProbeTimer  eventKind = timerPrefix + eventKind(TimerProbe)
	onSpawn       eventKind = "request_spawn"
	onRegister    eventKind = "request_register"
	onReady       eventKind = "request_ready"
	onSuspend     eventKind = "request_suspend"
	onResume      eventKind = "request_resume"
	onStop        eventKind = "request_stop"
	onDeliver     eventKind = "request_deliver"
	onExit        eventKind = "request_exit"

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
		StateReady, StateWorking, StateIdle)
	t.row(onSuspend, "already suspended", nothingToDo, nil, StateSuspended)
	t.ignore(onSuspend, "the agent is not ready, so there is nothing to suspend yet", unready...)
	t.ignore(onSuspend, failedClaim, StateFailed)
	t.ignore(onSuspend, retiredClaim, StateRetired)

	t.row(onResume, "resume the same session", resume, []ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onResume, "the claim is not suspended", live...)
	t.ignore(onResume, "a queued claim is spawned, not resumed", StateQueued)
	t.ignore(onResume, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onResume, failedClaim, StateFailed)
	t.ignore(onResume, retiredClaim, StateRetired)

	t.row(onStop, "stop the process and retire the claim", stop, []ClaimState{StateRetired},
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle,
		StateSuspended, StateFailed)
	t.row(onStop, "already retired", nothingToDo, nil, StateRetired)

	t.row(onDeliver, "queue the task; it goes when the claim is next ready or idle", deliverLater, nil,
		StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateWorking)
	t.ignore(onDeliver, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.row(onDeliver, "queue the task and send it", deliverNow, []ClaimState{StateWorking}, prompted...)
	t.row(onDeliver, "queue the task and resume the claim for it", deliverResuming,
		[]ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onDeliver, failedClaim, StateFailed)
	t.ignore(onDeliver, retiredClaim, StateRetired)

	t.row(onExit, "the agent reported its exit", exit, []ClaimState{StateRetired},
		StateRegistered, StateReady, StateWorking, StateIdle)
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

// lateRefused is the agent refusing a prompt it had acknowledged. The claim's state stays what the
// stream observed — a turn that confirmed the delivery may be a foreign one that really is running.
func lateRefused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamLateRefusal)
	m.log.Warn("supervise: the agent refused an acknowledged prompt", "delivery", r.DeliveryID, "error", r.Error)
	return m.promptFailed(ctx, "refused after its acknowledgement: "+r.Error)
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
// registered: alive, it is retired and counted — the third meaning of a missing worker — and dead,
// it is counted. A stop that fails leaves everything and tries again at the next probe interval.
func registrationDeadline(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(ctx context.Context) error {
		alive := *m.claim.Locator
		if err := m.deps.Runtime.Stop(ctx, alive, m.deps.Timeouts.StopGrace); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent never registered: stop: %w", m.claim.Token, err)
		}
		m.log.Warn("supervise: the agent never registered; retired its process", "incarnation", alive.Incarnation)
		return m.relaunchAfterFailure(ctx, &alive)
	}, TimerRegistration, m.deps.Timeouts.Probe)
}

func noTurn(m *Machine, ctx context.Context, _ Event) error {
	return m.promptFailed(ctx, fmt.Sprintf("acknowledged, and no turn started within %s", m.deps.Timeouts.RPC))
}

func spawn(m *Machine, ctx context.Context, _ Event) error { return m.launch(ctx, nil) }

func register(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	m.claim.Session, m.claim.SessionFile = r.Session, r.SessionFile
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

func suspend(m *Machine, ctx context.Context, _ Event) error {
	suspending := *m.claim.Locator
	if err := m.deps.Runtime.Suspend(ctx, suspending); err != nil {
		return fmt.Errorf("suspend %s: %w", m.claim.Token, err)
	}
	m.disarmAll()
	m.forgetSend()
	m.claim.State = StateSuspended
	m.claim.Locator = nil
	m.previous = &suspending
	return m.persist(ctx)
}

func resume(m *Machine, ctx context.Context, _ Event) error { return m.launch(ctx, m.previous) }

func stop(m *Machine, ctx context.Context, _ Event) error {
	if loc := m.claim.Locator; loc != nil {
		if err := m.deps.Runtime.Stop(ctx, *loc, m.deps.Timeouts.StopGrace); err != nil {
			return fmt.Errorf("stop %s: %w", m.claim.Token, err)
		}
	}
	return m.retire(ctx)
}

func deliverLater(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestDeliver)
	return m.queue(ctx, request.Task, request.ID)
}

func deliverNow(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestDeliver)
	if err := m.queue(ctx, request.Task, request.ID); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

func deliverResuming(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestDeliver)
	if err := m.queue(ctx, request.Task, request.ID); err != nil {
		return err
	}
	return m.launch(ctx, m.previous)
}

func exit(m *Machine, ctx context.Context, ev Event) error {
	m.log.Info("supervise: the agent reported its exit", "reason", ev.(RequestExit).Reason)
	return m.retire(ctx)
}

func nothingToDo(*Machine, context.Context, Event) error { return nil }
