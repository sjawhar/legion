// Package supervise is process supervision: one state machine per role claim, and the only place
// the runtime's process facts, the worker stream's agent facts, the machine's own timers, and the
// API's requests meet.
//
// A machine is the claim's one serialised decision owner. Every event enters through Handle,
// which holds the machine's lock for the whole decision — including the runtime call a decision
// makes (a spawn, a stop, a probe). That is what makes the fences work: an event that arrives
// while a relaunch is in the runtime waits, and is then judged against the claim the relaunch
// left, not the one it replaced. The one thing a decision never waits on is a prompt: a send runs
// on its own goroutine and posts its outcome back as PromptAcked or PromptRefused, so a slow agent
// cannot hold the claim. (The only agent round trip a decision does wait on is the get_state a
// restored claim asks once, bounded by the RPC timeout.)
//
// The package reads no configuration and knows neither the worker stream nor any runtime's
// implementation: its limits and timeouts are constructor parameters, the connection directory is
// runtime.Conns, and the runtime is runtime.Runtime.
package supervise

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ClaimState is where a claim is in its life.
type ClaimState string

const (
	// StateQueued: the claim exists and nothing has been launched for it.
	StateQueued ClaimState = "queued"
	// StateLaunchUncertain: the previous daemon started a pane but crashed before recording its
	// locator, and this daemon cannot yet reconcile that pane. It survives a restart and refuses a
	// new spawn until reconciliation proves the old pane absent or reaped.
	StateLaunchUncertain ClaimState = "launch_uncertain"
	// StateLaunching: a process was started (or is being started) and its shim has not said hello.
	StateLaunching ClaimState = "launching"
	// StateShimConnected: the shim said hello with this launch's boot token. Under always-dial the
	// shim connects before it starts Oh My Pi, so this says nothing about the agent yet.
	StateShimConnected ClaimState = "shim_connected"
	// StateRegistered: the agent registered — the session is recorded and its secret issued.
	StateRegistered ClaimState = "registered"
	// StateReady: the agent finished booting and can be prompted.
	StateReady ClaimState = "ready"
	// StateWorking: a turn is in flight.
	StateWorking ClaimState = "working"
	// StateIdle: the agent finished a turn and can be prompted.
	StateIdle ClaimState = "idle"
	// StateSuspended: the process was stopped and the session kept; the claim resumes on demand.
	StateSuspended ClaimState = "suspended"
	// StateFailed: a budget ran out. Nothing relaunches the claim on its own.
	StateFailed ClaimState = "failed"
	// StateRetired: the claim is over — stopped by the operator, or ended by its agent.
	StateRetired ClaimState = "retired"
)

// Claim is one role claim as the supervisor holds and persists it.
type Claim struct {
	Token      claim.Token
	Project    string
	Tree       string
	Issue      string
	Role       claim.Role
	Generation uint64
	// Session is the Oh My Pi session id the agent registered as; SessionFile is the transcript it
	// reported, the one value `--resume` takes. Both are written by the registration, and a claim
	// that has them resumes that session on every relaunch and refuses any other.
	Session     string
	SessionFile string
	Locator     *runtime.Locator
	State       ClaimState
	Budgets     Budgets
	Pending     *Delivery
	// BootTokenHash is the hash of the current launch's boot token, which the shim's hello and the
	// agent's registration are resolved by. CapabilityHash is the hash of the secret the agent's
	// registration was issued.
	BootTokenHash   []byte
	CapabilityHash  []byte
	UncertainStreak int
}

// Event is everything that reaches a machine. The set is sealed: RuntimeObservation,
// StreamHello, StreamTurnStart, StreamTurnEnd, StreamClosed, StreamLateRefusal, PromptAcked,
// PromptRefused, Timer, and the eight requests. The transition table has a row or a named ignore
// for every one of them in every state.
type Event interface{ isEvent() }

// Store is the persistence a machine writes through. A claim and its pending delivery are
// written separately: the delivery changes on every send, the claim far less often.
type Store interface {
	PutClaim(ctx context.Context, c Claim) error
	PutDelivery(ctx context.Context, token claim.Token, d Delivery) error
	RetireDelivery(ctx context.Context, token claim.Token, deliveryID string) error
}

// Specs builds the part of a launch the claim does not carry — the pane's environment, its
// secrets, its prompt, its workspace. The machine fills in who the launch is for: the claim's
// identity, the generation, the boot token, and the session file a resume continues.
type Specs interface {
	SpawnSpec(ctx context.Context, c Claim) (runtime.SpawnSpec, error)
}

// Clock is time as a machine sees it.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Cancel
}

// Cancel stops a scheduled callback.
type Cancel interface{ Stop() bool }

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) AfterFunc(d time.Duration, f func()) Cancel { return time.AfterFunc(d, f) }

// Timeouts are every wait a machine times.
type Timeouts struct {
	// Boot is the boot observation interval (worker_boot_timeout_seconds): each interval an
	// unregistered process is probed, and a live one is left alone.
	Boot time.Duration
	// RegistrationIntervals is how many boot intervals a live process gets to register before it
	// is retired (worker_boot_registration_deadline_intervals).
	RegistrationIntervals int
	// RPC bounds a prompt's acknowledgement and, after it, the wait for the turn it should start
	// (worker_rpc_timeout_seconds).
	RPC time.Duration
	// Probe is how soon an uncertain process is probed again (probe_interval_seconds).
	Probe time.Duration
	// StopGrace is how long a stop waits for a process to end itself before killing it
	// (worker_stop_timeout_seconds).
	StopGrace time.Duration
}

// Deps is what a machine is built from.
type Deps struct {
	Runtime  runtime.Runtime
	Conns    runtime.Conns
	Store    Store
	Specs    Specs
	Clock    Clock
	Log      *slog.Logger
	Limits   Limits
	Timeouts Timeouts
	// Identity is the git identity a role's commits carry: its App's bot. Every delivery hands it
	// to the agent's working copy before the task. nil, for a daemon with no GitHub Apps, adopts
	// nothing.
	Identity func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
}

func (d Deps) check() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"Limits.LaunchFailures", d.Limits.LaunchFailures},
		{"Limits.PromptFailures", d.Limits.PromptFailures},
		{"Limits.PromptRetires", d.Limits.PromptRetires},
		{"Timeouts.RegistrationIntervals", d.Timeouts.RegistrationIntervals},
	} {
		if bound.value < 1 {
			return fmt.Errorf("supervise: %s must be at least 1, got %d", bound.name, bound.value)
		}
	}
	for _, wait := range []struct {
		name  string
		value time.Duration
	}{
		{"Timeouts.Boot", d.Timeouts.Boot},
		{"Timeouts.RPC", d.Timeouts.RPC},
		{"Timeouts.Probe", d.Timeouts.Probe},
		{"Timeouts.StopGrace", d.Timeouts.StopGrace},
	} {
		if wait.value <= 0 {
			return fmt.Errorf("supervise: %s must be positive, got %s", wait.name, wait.value)
		}
	}
	return nil
}

// RefusedError is a request the claim's state does not allow — the answer an API route gives
// its caller instead of pretending the request happened.
type RefusedError struct {
	State   ClaimState
	Request string
	Reason  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s refused: the claim is %s (%s)", e.Request, e.State, e.Reason)
}

// Machine is one claim's decision owner.
type Machine struct {
	mu         sync.Mutex
	ctx        context.Context
	deps       Deps
	log        *slog.Logger
	claim      Claim
	onTerminal func(Claim, ClaimState)

	timers map[TimerKind]armed
	seq    uint64
	// send is the prompt in flight, from the moment its goroutine starts to the moment its
	// outcome is handled; helloDuringSend is a shim hello that arrived meanwhile — the trigger for
	// a re-send, should that send fail.
	send            *sending
	helloDuringSend bool
	// askFirst is a restored claim whose agent an earlier daemon was talking to: a pending
	// delivery it may already have sent, or a turn it saw start and may not have seen end. The
	// machine asks the agent (get_state) before it acts on either.
	askFirst bool
	// previous is the incarnation a suspension stopped, which a resume hands the runtime to wait
	// out. It is memory only: after a restart the suspension is long complete.
	previous *runtime.Locator
	// stale is every stale event already logged, so a repeated one is dropped in silence.
	stale map[string]bool
	// goroutines counts sends whose outcome has not been handled yet; idle wakes Wait.
	goroutines int
	idle       *sync.Cond
}

type sending struct {
	id         string
	generation uint64
}

type armed struct {
	seq        uint64
	deliveryID string
	cancel     Cancel
}

// NewMachine builds the machine for one claim: a new one the spawn route just stored, or one the
// daemon read back at boot. A restored claim that was booting is watched again from now, and a
// restored delivery that may already have been sent is sent again only after asking the agent.
// ctx bounds the machine's own work — its timers and its sends.
func NewMachine(ctx context.Context, deps Deps, c Claim) (*Machine, error) {
	if err := deps.check(); err != nil {
		return nil, err
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	m := &Machine{
		ctx:    ctx,
		deps:   deps,
		log:    deps.Log.With("claim", string(c.Token)),
		claim:  copyClaim(c),
		timers: map[TimerKind]armed{},
		stale:  map[string]bool{},
	}
	m.idle = sync.NewCond(&m.mu)
	switch c.State {
	case StateLaunching, StateShimConnected:
		m.armBoot()
	case StateReady, StateIdle:
		m.askFirst = c.Pending != nil && c.Pending.ConfirmedAt.IsZero()
	case StateWorking:
		m.askFirst = true
	}
	return m, nil
}

// Handle is one event: fenced, then looked up in the transition table, then acted on. A request
// the claim's state does not allow is refused with a RefusedError; a fenced request is refused
// with the claim refusal it violates (claim.StaleGeneration, claim.SameAgentRefusal). Any other
// event that is stale is logged once and dropped.
//
// Handle returns when the decision is made, which includes the runtime call it makes: a stop
// waits out its grace, a resume waits out the previous incarnation. A caller feeding many claims
// from one source hands each machine its events on its own goroutine, so one claim's stop does
// not hold another's hello.
func (m *Machine) Handle(ctx context.Context, ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if token := claimOf(ev); token != m.claim.Token {
		return fmt.Errorf("supervise: %T for claim %s reached the machine of %s", ev, token, m.claim.Token)
	}
	if pass, err := m.fence(ctx, ev); !pass {
		return err
	}
	if err := m.settle(ctx); err != nil {
		return err
	}
	k := key{m.claim.State, kindOf(ev)}
	r, ok := table[k]
	if !ok {
		return fmt.Errorf("supervise: no row for %s in %s", k.kind, k.state)
	}
	if r.act == nil {
		if name, ok := requestName(ev); ok {
			return &RefusedError{State: m.claim.State, Request: name, Reason: r.ignore}
		}
		return nil
	}
	from := m.claim.State
	err := errors.Join(r.act(m, ctx, ev), m.settle(ctx))
	if to := m.claim.State; to != from && !slices.Contains(r.to, to) {
		err = errors.Join(err, fmt.Errorf("supervise: row %q moved %s from %s to %s, outside %v",
			r.name, m.claim.Token, from, to, r.to))
	}
	return err
}

// ReleaseUncertainLaunch is the only path out of the persisted uncertain-launch state. The daemon
// calls it only after reconciliation proves the unrecorded predecessor absent or reaped; concurrent
// old-pane hellos move the state first, so the caller does not open a second pane.
func (m *Machine) ReleaseUncertainLaunch(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claim.State != StateLaunchUncertain || m.claim.Locator != nil {
		return false, nil
	}
	m.claim.State = StateQueued
	if err := m.persist(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Claim is a copy of the claim as the machine holds it now.
func (m *Machine) Claim() Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyClaim(m.claim)
}

// OnTerminal installs the daemon callback for durable ready and failed transitions.
// The callback runs after the transition's claim write has succeeded.
func (m *Machine) OnTerminal(callback func(Claim, ClaimState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTerminal = callback
}

func (m *Machine) terminal(state ClaimState) {
	if m.onTerminal != nil {
		m.onTerminal(copyClaim(m.claim), state)
	}
}

// Wait blocks until every send the machine started has had its outcome handled — what a daemon
// waits for before closing the store the outcome is written to.
func (m *Machine) Wait() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.goroutines > 0 {
		m.idle.Wait()
	}
}

// fence applies the checks an event carries the fields for: the incarnation an observation is
// about, the generation of a hello or a prompt outcome or a request, the session a request comes
// from, the delivery a turn or a refusal or a timer names, and the arming a timer came from. It
// reports whether the event goes on to the table.
func (m *Machine) fence(ctx context.Context, ev Event) (bool, error) {
	pending := m.claim.Pending
	switch ev := ev.(type) {
	case RuntimeObservation:
		held := ""
		if m.claim.Locator != nil {
			held = m.claim.Locator.Incarnation
		}
		if got := ev.Observation.Locator.Incarnation; held == "" || got != held {
			m.dropStale("RuntimeObservation", "incarnation", got, held)
			return false, nil
		}
	case StreamHello:
		if ev.Generation != m.claim.Generation {
			m.dropStale("StreamHello", "generation", fmt.Sprint(ev.Generation), fmt.Sprint(m.claim.Generation))
			return false, nil
		}
	case StreamTurnStart:
		if ev.DeliveryID != "" && (pending == nil || pending.ID != ev.DeliveryID) {
			m.dropStale("StreamTurnStart", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case StreamLateRefusal:
		if pending == nil || pending.ID != ev.DeliveryID {
			m.dropStale("StreamLateRefusal", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case PromptAcked:
		return m.fenceOutcome(ctx, "PromptAcked", ev.Generation, ev.DeliveryID)
	case PromptRefused:
		return m.fenceOutcome(ctx, "PromptRefused", ev.Generation, ev.DeliveryID)
	case Timer:
		a, ok := m.timers[ev.Kind]
		if !ok || a.seq != ev.Seq || ev.Generation != m.claim.Generation || a.deliveryID != ev.DeliveryID ||
			(ev.Kind == TimerTurn && pendingID(pending) != ev.DeliveryID) {
			m.dropStale("Timer", "timer", fmt.Sprintf("%s#%d", ev.Kind, ev.Seq), fmt.Sprintf("%s#%d", ev.Kind, a.seq))
			return false, nil
		}
		delete(m.timers, ev.Kind)
	case RequestRegister:
		return m.fenceRequest("register", ev.Generation, ev.Session)
	case RequestReady:
		return m.fenceRequest("ready", ev.Generation, ev.Session)
	case RequestExit:
		return m.fenceRequest("exit", ev.Generation, ev.Session)
	}
	return true, nil
}

// fenceOutcome is the fence on a prompt's outcome. The send it belongs to is over either way; an
// outcome for a delivery or a generation the claim has moved past records nothing — and when it
// was the machine's own send that finished late, the delivery that replaced it may go now.
func (m *Machine) fenceOutcome(ctx context.Context, event string, generation uint64, deliveryID string) (bool, error) {
	own := m.send != nil && m.send.id == deliveryID && m.send.generation == generation
	if own {
		m.send = nil
	}
	if generation == m.claim.Generation && pendingID(m.claim.Pending) == deliveryID {
		return true, nil
	}
	if own {
		m.helloDuringSend = false
		return false, m.sendPending(ctx)
	}
	if generation != m.claim.Generation {
		m.dropStale(event, "generation", fmt.Sprint(generation), fmt.Sprint(m.claim.Generation))
	} else {
		m.dropStale(event, "delivery", deliveryID, pendingID(m.claim.Pending))
	}
	return false, nil
}

// fenceRequest is the fence on an agent's own request: it must come from the claim's current
// generation, and — once the claim has recorded a session — from that session.
func (m *Machine) fenceRequest(request string, generation uint64, session string) (bool, error) {
	if generation != m.claim.Generation {
		return false, fmt.Errorf("%s for %s at generation %d, the claim is at %d: %w",
			request, m.claim.Token, generation, m.claim.Generation, claim.StaleGeneration)
	}
	if m.claim.Session != "" && session != m.claim.Session {
		return false, fmt.Errorf("%s for %s as session %q, the claim resumes %q: %w",
			request, m.claim.Token, session, m.claim.Session, claim.SameAgentRefusal)
	}
	return true, nil
}

// dropStale logs a stale event the first time it is seen. The sweep observes every thirty seconds
// and a shim replays its backlog on every reconnect, so the same stale fact arrives again and
// again; the operator needs it once.
func (m *Machine) dropStale(event, fence, got, held string) {
	seen := event + "/" + fence + "/" + got
	if m.stale[seen] {
		return
	}
	m.stale[seen] = true
	m.log.Warn("supervise: dropped a stale event", "event", event, "fence", fence, "got", got, "held", held)
}

// launch starts a process for the claim at a new generation with a new boot token: the same
// agent resumed from its session file when the claim has one — after prev's incarnation is gone,
// when there is a prev to wait out — and a fresh spawn when it has none. The boot token's hash is
// persisted before the process starts, so the shim's first hello resolves. A launch the runtime
// or the spec refuses is a launch failure and is tried again at once, until the budget runs out.
func (m *Machine) launch(ctx context.Context, prev *runtime.Locator) error {
	for {
		m.disarmAll()
		m.forgetSend()
		m.previous = nil
		m.claim.Generation++
		token := rand.Text()
		m.claim.BootTokenHash = HashBootToken(token)
		m.claim.State = StateLaunching
		m.claim.Locator = nil
		if err := m.persist(ctx); err != nil {
			return err
		}
		loc, err := m.start(ctx, token, prev)
		if err == nil {
			m.claim.Locator = &loc
			m.armBoot()
			m.log.Info("supervise: launched", "generation", m.claim.Generation, "incarnation", loc.Incarnation,
				"resumed", m.claim.SessionFile != "")
			return m.persist(ctx)
		}
		m.claim.Budgets.LaunchFailures++
		m.log.Warn("supervise: launch failed", "generation", m.claim.Generation, "error", err,
			"launchFailures", m.claim.Budgets.LaunchFailures, "limit", m.deps.Limits.LaunchFailures)
		if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
			return errors.Join(err, m.fail(ctx, "launch failures ran out"))
		}
	}
}

func (m *Machine) start(ctx context.Context, token string, prev *runtime.Locator) (runtime.Locator, error) {
	spec, err := m.deps.Specs.SpawnSpec(ctx, m.claim)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("build the launch of %s: %w", m.claim.Token, err)
	}
	spec.Claim, spec.Project, spec.Tree, spec.Issue, spec.Role = m.claim.Token, m.claim.Project, m.claim.Tree, m.claim.Issue, m.claim.Role
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = m.claim.Generation, token, ""
	if m.claim.SessionFile == "" {
		return m.deps.Runtime.Spawn(ctx, spec)
	}
	spec.ResumeSessionFile = m.claim.SessionFile
	var wait runtime.Locator
	if prev != nil {
		wait = *prev
	}
	return m.deps.Runtime.Resume(ctx, wait, spec)
}

// died is the claim's process found gone — or found to be some other process — while it was
// live: one launch failure, and the same session relaunched after it, or failed when the budget
// is spent.
func (m *Machine) died(ctx context.Context, observation runtime.Observation) error {
	dead := *m.claim.Locator
	m.log.Warn("supervise: process died", "incarnation", dead.Incarnation, "observed", string(observation.Kind),
		"detail", observation.Detail)
	return m.relaunchAfterFailure(ctx, &dead)
}

// fail puts the claim where nothing relaunches it: its timers stop, its locator goes, and the
// pending delivery stays for whoever decides what happens next.
func (m *Machine) fail(ctx context.Context, why string) error {
	m.disarmAll()
	m.forgetSend()
	m.previous = nil
	m.claim.State = StateFailed
	m.claim.Locator = nil
	m.log.Error("supervise: claim failed", "why", why, "launchFailures", m.claim.Budgets.LaunchFailures,
		"promptFailures", m.claim.Budgets.PromptFailures, "promptRetires", m.claim.Budgets.PromptRetires)
	if err := m.persist(ctx); err != nil {
		return err
	}
	m.terminal(StateFailed)
	return nil
}

// retire ends the claim: nothing of it runs any more and nothing relaunches it.
func (m *Machine) retire(ctx context.Context) error {
	m.disarmAll()
	m.forgetSend()
	m.previous = nil
	m.claim.State = StateRetired
	m.claim.Locator = nil
	return m.persist(ctx)
}

// judge acts on what a probe or the sweep says about the claim's process. alive is what a live
// process means to the caller; an uncertain one, or a runtime that could not answer, is asked
// again after the given wait on the given timer — and is never read as alive or gone.
func (m *Machine) judge(ctx context.Context, observation runtime.Observation, alive func(context.Context) error, again TimerKind, after time.Duration) error {
	switch observation.Kind {
	case runtime.Alive:
		if m.claim.UncertainStreak > 0 {
			m.log.Info("supervise: process alive again", "after", m.claim.UncertainStreak)
			m.claim.UncertainStreak = 0
			if err := m.persist(ctx); err != nil {
				return err
			}
		}
		return alive(ctx)
	case runtime.Gone, runtime.NotRecordedProcess:
		return m.died(ctx, observation)
	case runtime.Uncertain:
		m.claim.UncertainStreak++
		m.log.Warn("supervise: process uncertain", "streak", m.claim.UncertainStreak, "detail", observation.Detail)
		m.arm(again, after, "")
		return m.persist(ctx)
	}
	return fmt.Errorf("supervise: an observation of kind %q", observation.Kind)
}

// probe asks the runtime about the claim's process now and judges the answer.
func (m *Machine) probe(ctx context.Context, alive func(context.Context) error, again TimerKind, after time.Duration) error {
	observation, err := m.deps.Runtime.Probe(ctx, *m.claim.Locator)
	if err != nil {
		m.log.Warn("supervise: the runtime could not probe the process; probing again", "error", err)
		m.arm(again, after, "")
		return nil
	}
	return m.judge(ctx, observation, alive, again, after)
}

func nothing(context.Context) error { return nil }

func (m *Machine) armBoot() {
	m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
	m.arm(TimerRegistration, m.deps.Timeouts.Boot*time.Duration(m.deps.Timeouts.RegistrationIntervals), "")
}

// arm schedules one timer of a kind, replacing any of that kind already armed. Its event carries
// the arming's sequence number and generation, so a timer that fires after it was replaced, or
// after a relaunch, is fenced out even if stopping it came too late.
func (m *Machine) arm(kind TimerKind, after time.Duration, deliveryID string) {
	m.disarm(kind)
	m.seq++
	ev := Timer{Claim: m.claim.Token, Kind: kind, Generation: m.claim.Generation, Seq: m.seq, DeliveryID: deliveryID}
	cancel := m.deps.Clock.AfterFunc(after, func() {
		if err := m.Handle(m.ctx, ev); err != nil {
			m.log.Error("supervise: timer", "timer", string(kind), "error", err)
		}
	})
	m.timers[kind] = armed{seq: m.seq, deliveryID: deliveryID, cancel: cancel}
}

func (m *Machine) disarm(kind TimerKind) {
	if a, ok := m.timers[kind]; ok {
		a.cancel.Stop()
		delete(m.timers, kind)
	}
}

func (m *Machine) disarmAll() {
	for kind := range m.timers {
		m.disarm(kind)
	}
}

// forgetSend drops everything the machine knew about talking to the agent it had: a new process,
// or none, has no memory of the old one's prompts.
func (m *Machine) forgetSend() {
	m.send, m.helloDuringSend, m.askFirst = nil, false, false
}

// persist writes the claim, the one place every transition passes. A capability belongs to a
// registered agent whose process runs, so a claim in any other state — relaunching after a death,
// suspended, failed, or retired — is written without one: the old secret authenticates nothing and
// every grant it minted fails its fence, as the shipped daemon revokes a session's capability and
// its grants on death, retirement, and teardown.
func (m *Machine) persist(ctx context.Context) error {
	if !holdsCapability(m.claim.State) {
		m.claim.CapabilityHash = nil
	}
	return m.deps.Store.PutClaim(ctx, m.claim)
}

// holdsCapability says whether a claim in state has a registered agent with a running process.
func holdsCapability(state ClaimState) bool {
	switch state {
	case StateRegistered, StateReady, StateWorking, StateIdle:
		return true
	default:
		return false
	}
}

func claimOf(ev Event) claim.Token {
	switch ev := ev.(type) {
	case RuntimeObservation:
		return ev.Observation.Locator.Claim
	case StreamHello:
		return ev.Claim
	case StreamTurnStart:
		return ev.Claim
	case StreamTurnEnd:
		return ev.Claim
	case StreamClosed:
		return ev.Claim
	case StreamLateRefusal:
		return ev.Claim
	case PromptAcked:
		return ev.Claim
	case PromptRefused:
		return ev.Claim
	case Timer:
		return ev.Claim
	case RequestSpawn:
		return ev.Claim
	case RequestRegister:
		return ev.Claim
	case RequestReady:
		return ev.Claim
	case RequestSuspend:
		return ev.Claim
	case RequestResume:
		return ev.Claim
	case RequestStop:
		return ev.Claim
	case RequestRetry:
		return ev.Claim
	case RequestDeliver:
		return ev.Claim
	case RequestExit:
		return ev.Claim
	}
	return ""
}

func copyClaim(c Claim) Claim {
	if c.Locator != nil {
		loc := *c.Locator
		c.Locator = &loc
	}
	if c.Pending != nil {
		d := *c.Pending
		c.Pending = &d
	}
	c.BootTokenHash = bytes.Clone(c.BootTokenHash)
	c.CapabilityHash = bytes.Clone(c.CapabilityHash)
	return c
}
