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
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wait"
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

// SuspendSource is who suspended a claim. The daemon resumes a suspended claim by rule on the
// triggers main had — its phase's start, a workflow delivery, the controller keeper
// (RequestRuleResume) — unless an operator suspended it: that suspension holds until the operator
// resumes the claim. The boot's resume by rule, which main did not have, acts only on a source
// ByRule names, so it never overrides a drain recorded before the source existed.
type SuspendSource string

const (
	// SuspendedByUnknown is the source migration 0038 assigns every claim that was already
	// suspended before the source existed: most such rows are the daemon's own (a lingering tree, a
	// parked child, a re-entry's interrupted worker), and main's triggers resume them as before.
	SuspendedByUnknown SuspendSource = "unknown"
	// SuspendedByOperator: the operator's suspend route.
	SuspendedByOperator SuspendSource = "operator"
	// SuspendedByDaemon: the workflow's own suspend — the issue left the workflow, or its tree
	// lingers.
	SuspendedByDaemon SuspendSource = "daemon"
	// SuspendedByExit: the tree root's agent reported its own exit, which suspends a root.
	SuspendedByExit SuspendSource = "exit"
)

// ByRule is whether the source is one the daemon itself recorded, the daemon's own suspend or a
// root's exit: the only sources the boot's resume by rule acts on, and the only suspended phase
// workers the stranded report leaves out.
func (s SuspendSource) ByRule() bool { return s == SuspendedByDaemon || s == SuspendedByExit }

// Claim is one role claim as the supervisor holds and persists it.
type Claim struct {
	Token      claim.Token
	Project    string
	Tree       string
	Issue      string
	Role       claim.Role
	Generation uint64
	// TreeEpoch is the durable lifecycle epoch this claim bound to before its first persistence.
	// A cleanup reservation closes that epoch before a later claim/launch can write or run.
	TreeEpoch uint64
	// Session and SessionFile are the agent's reported session id and absolute transcript path.
	// Every relaunch resumes that recorded session and refuses another one.
	Session     string
	SessionFile string
	// Abandoned is the session this claim stopped resuming when it last forgot one
	// (forgetSession), and why. The session itself stays in its store — nothing of it is deleted
	// — so this is the one record of where it went, for the operator who may want to read it and
	// for the agent that replaces it, which is told of it in the first task it is given
	// (abandonedTask). It is persisted, since that first task can be a launch and a daemon
	// restart away, and it is replaced rather than cleared by the next abandonment: the session
	// before this one is gone whatever happens next. A claim that has abandoned none records a
	// session of "".
	Abandoned AbandonedSession
	// ServingGeneration is the issue generation of the last confirmed delivery this claim retired:
	// the run it goes on working until another delivery of a run replaces it. It is the second of
	// the three sources a completion is attributed from (api/handoff.go): the pending delivery
	// whose turn is running, then this, then — for a claim serving no run — a pending task the
	// agent may have read. It is what answers for a worker told to wait, whose turn's end retired
	// its delivery and whose next turn an Envoy notice starts with no delivery behind it. Zero is
	// a claim that has retired no confirmed task of a run.
	ServingGeneration uint64
	// LastStartRow is the outbox row id of the newest start run against this claim, and 0 for one
	// no start has run for. A stop from an older row is one the newer start superseded: it was
	// written to end a run that is over, and acting on it would stop the run that start began.
	LastStartRow int64
	Locator      *runtime.Locator
	// Enrollment is the secrets broker's record of the claim's running process, when the runtime
	// enrolls pods: the enrollment id, and the incarnation it was made for. It is
	// revoked and cleared wherever the process is let go (letGo), so a record for another
	// incarnation is never acted on.
	Enrollment *Enrollment
	State      ClaimState
	Budgets    Budgets
	// Failure is the reason a terminally failed claim could not continue. It is cleared before a
	// retry and persisted so a daemon restart can report the same failure to the workflow.
	Failure string
	Pending *Delivery
	// LastTurnStart and LastTurnEnd are when the agent last began a turn and when it last ended
	// one, each zero until that event happens. They are persisted, so a restart keeps them: they
	// are what a reader has to tell a worker still making progress from one wedged in a turn or
	// idle since yesterday, and a claim rebuilt without them would look as if it had never worked.
	LastTurnStart time.Time
	LastTurnEnd   time.Time
	// TurnOutcomes are the newest turns the claim's agent ended that its backend's producer
	// reported, in (generation, sequence) order and bounded by the launch failure limit
	// (recordTurnOutcome). They are the window RefusedRun derives the run of refused turns from,
	// and they are persisted, so a daemon restart reads the run the reports before it left rather
	// than starting the count again. They go with the session whose turns they are (freshRun).
	TurnOutcomes []TurnOutcome
	// LastDeathCause is why the claim's process was last found gone — the runtime's own account of
	// the death (runtime.Observation.Detail: an OOM kill, a node that went away, a pod the cluster
	// evicted) — and LastStartFailure why its last start did not produce a running agent — a
	// launcher that refused the start, an init container that failed, a pod that never became
	// ready (launchAfter's error). They are two answers to two questions, kept apart because a
	// claim that cannot start and a claim that keeps dying are different faults, and each is kept
	// until another of its own kind replaces it. LastReady is when the agent last said it could be
	// prompted, which is what says how old either cause is. All three are persisted: the operator
	// reading a claim after a restart is the one who has to act on them, and the restarted worker
	// is told the death's cause (interruptedTask).
	LastDeathCause   string
	LastStartFailure string
	LastReady        time.Time
	// BootTokenHash is the hash of the current launch's boot token, which the shim's hello and the
	// agent's registration are resolved by. CapabilityHash is the hash of the secret the agent's
	// registration was issued.
	BootTokenHash   []byte
	CapabilityHash  []byte
	UncertainStreak int
	// SuspendedBy is who suspended the claim while it is suspended, and "" otherwise.
	SuspendedBy SuspendSource
	// SuspensionHeld is whether a suspension is held for the agent's turn (holdSuspension), and
	// SuspensionHeldBy who asked for it. Both are memory only: Machine.Claim reports them, and the
	// store never writes them.
	SuspensionHeld   bool
	SuspensionHeldBy SuspendSource
	// SessionLost is whether the launch this claim is being built a spec for is a fresh agent
	// because the session the claim recorded was found nowhere the runtime could resume it
	// (startFresh). It is an argument of that one build and of nothing else: the machine sets it
	// on the copy of the claim it hands the spec builder (spawnSpec), never on the claim it keeps
	// or persists, so the new session is told its predecessor's transcript is gone and nothing
	// afterwards reads it.
	SessionLost bool
	// PluginContract is the contract the current registration declared: 0 means no contract has
	// been recorded, which every claim has at the first boot on this release. It is set whenever a
	// registration records Session/SessionFile/CapabilityHash (register, reregister).
	PluginContract int
	// PluginRelaunchFor is the daemon contract the last stale-plugin relaunch was made for, 0 when
	// none. It is set at every stale relaunch (refreshPlugin) and cleared when a registration
	// records the daemon's own contract, so a registration that still differs while it equals the
	// daemon's contract is the one that is charged rather than relaunched again uncharged.
	PluginRelaunchFor int
}

// RuleMayResume says whether the rule may resume this suspended claim: every suspension but the
// operator's, which holds until an operator resumes the claim.
func (c Claim) RuleMayResume() bool {
	return c.State == StateSuspended && c.SuspendedBy != SuspendedByOperator
}

// RuleHolds is whether an operator's suspension blocks a daemon rule: the claim is suspended by the
// operator, or the operator's suspension is held for its agent's turn.
func (c Claim) RuleHolds() bool {
	if c.SuspensionHeld {
		return c.SuspensionHeldBy == SuspendedByOperator
	}
	return c.State == StateSuspended && c.SuspendedBy == SuspendedByOperator
}

// RuleHoldError says why a start or outbox delivery waits: the operator's suspension, which only
// the operator's resume ends.
func (c Claim) RuleHoldError() error {
	return fmt.Errorf("the claim is suspended by %s; it waits for the operator's resume: %w", SuspendedByOperator, ErrHeldByOperator)
}

// ClaimView is a claim as its machine last published it (Machine.View), with what only the decision
// in flight knows.
type ClaimView struct {
	Claim
	// Stopping is whether the decision in flight is stopping the claim's process for good — a
	// suspension, a failure, a release, a root's exit (endProcess, release) — while the claim still
	// shows the state it is leaving. A process retired and relaunched in one decision
	// (chargePrompt, the registration deadline) is not stopping: its claim keeps its role.
	Stopping bool
}

// treeRoot is whether the claim is its tree's root claim (claim.IsTreeArchitect), which ends only
// when its tree closes.
func (c Claim) treeRoot() bool { return claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) }

// Event is everything that reaches a machine: RuntimeObservation, StreamHello, StreamTurnStart,
// StreamTurnEnd, IssueTurnEnded, StreamClosed, StreamLateRefusal, PromptAcked, PromptRefused,
// Timer, StalePlugin, and the requests. The transition table has a row or a named ignore for
// every one in every state.
type Event interface{ isEvent() }

// Store is the persistence a machine writes through. A claim and its pending delivery are
// written separately: the delivery changes on every send, the claim far less often.
type Store interface {
	// AdmitClaim binds a new or reactivated claim to an open durable tree lifecycle and writes it
	// in that same short transaction. A cleanup reservation refuses before runnable work exists.
	AdmitClaim(ctx context.Context, c Claim) (Claim, error)
	// CheckLaunch rechecks the bound lifecycle before a claim persists StateLaunching or calls the
	// runtime; it returns the named cleanup wait without charging a launch failure.
	CheckLaunch(ctx context.Context, c Claim) error
	PutClaim(ctx context.Context, c Claim) error
	PutDelivery(ctx context.Context, token claim.Token, d Delivery) error
	// PutClaimAndDelivery writes both in one transaction: a confirmation records the claim and
	// the delivery its turn started, and half of that is a task nothing sends again.
	PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error
	// RetireDelivery ends the claim's delivery and writes the claim it is given in the same
	// transaction: the run a claim serves is set by the retiring of the task that ran.
	RetireDelivery(ctx context.Context, c Claim, deliveryID string) error
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
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

// Cancel stops a scheduled callback.
type Cancel interface{ Stop() bool }

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) AfterFunc(d time.Duration, f func()) Cancel { return time.AfterFunc(d, f) }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error { return backoff.Sleep(ctx, d) }

// Timeouts are every wait a machine times.
type Timeouts struct {
	// Boot is the boot observation interval (worker_boot_timeout_seconds): each interval an
	// unregistered process is probed, and a live one is left alone.
	Boot time.Duration
	// RegistrationIntervals is how many boot intervals a live process gets to register, and again
	// from its registration to say it is ready, before it is retired
	// (worker_boot_registration_deadline_intervals).
	RegistrationIntervals int
	// RPC bounds a prompt's acknowledgement and, after it, the wait for the turn it should start
	// (worker_rpc_timeout_seconds).
	RPC time.Duration
	// Probe is how soon an uncertain process is probed again (probe_interval_seconds).
	Probe time.Duration
	// Stop is how long a suspension that arrives while the agent is in a turn waits for that turn
	// to end before it stops the process anyway (worker_stop_timeout_seconds).
	Stop time.Duration
	// LaunchBackoff is the first wait before a launch is tried again after a charged launch
	// failure, doubled for each failure charged since the claim was last ready, and jittered
	// (backoff.Wait): a launch the runtime refused, and a process relaunched after its charge
	// (relaunchAfterFailure) — one that died, or was retired at its registration deadline. The
	// daemon sets it to its launchBackoffFirst.
	LaunchBackoff time.Duration
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
	// Secrets is the secrets broker's enrollment routes for the runtime's pods. nil
	// deploys with no broker: no identity is kept, and nothing is enrolled or revoked.
	Secrets Enroller
	// Identity is the git identity a role's commits carry on issue: its App's bot, minted for the
	// owner of the repository the issue works. A phase task hands it to the agent's working copy
	// before the task (adopts). nil, for a daemon with no workflow or no GitHub Apps, adopts
	// nothing.
	Identity func(ctx context.Context, role claim.Role, issue string) (runtime.GitIdentity, error)
	// IssueInTurn names a claim of issue other than except whose agent is in a turn now, and
	// reports whether there is one. The roles of an issue work one jj workspace, so the adoption a
	// phase task makes of its working copy is not a private act: one run while another role is in
	// a turn moves that role's worker onto a commit of this role's (dispatch://INFRA-714), and a
	// task that would adopt waits instead (sendPending). The daemon answers it from the state its
	// machines publish — a claim is StateWorking from its turn's start to its turn's end — which
	// is read without waiting on any decision in flight (Machine.View). nil holds no delivery
	// back.
	IssueInTurn func(issue string, except claim.Token) (claim.Token, bool)
	// PhaseHolds says whether issue is still in phase p on run generation: the workflow enqueues a
	// task for the phase and run its issue was in, and the outbox refuses to start a role for a
	// phase the issue has left (daemon/outbox.go), so a delivery queued before the phase or the run
	// moved on is that same staleness one step later and is dropped rather than sent. It is asked
	// only about a delivery that names a phase; a task an operator delivered by hand names none and
	// is never put to it, which the machine decides rather than this answer. generation is 0 for a
	// task of no run. nil holds every delivery.
	PhaseHolds func(ctx context.Context, issue string, p phase.Phase, generation uint64) (bool, error)
	// TreeClosable answers whether the operator may close this claim's tree: false refuses the
	// close, and an error is the read itself failing, which the caller sees as a failure rather
	// than a refusal. It is asked for the operator's own close — the workflow's linger close is its
	// own decision — and here rather than by the caller, so the answer and the stop it decides are
	// one decision rather than two round trips apart; and by a refused stop of the tree's root,
	// whose refusal then names that close. It is not a lock on what it reads: an issue recorded for
	// the tree commits in its own transaction and can still land between this answer and the
	// retire. nil closes every tree.
	TreeClosable func(ctx context.Context, c Claim) (bool, error)
	// PluginContract is the daemon's own plugin contract number (api.DaemonAPIVersion; supervise
	// cannot import api), compared against each claim's recorded PluginContract to decide whether a
	// registration or a restored claim is stale.
	PluginContract int
	// PluginRelaunchable is whether this daemon can relaunch a stale process onto a plugin at all:
	// true under tmux always (the pane resumes on the host's own plugin, which the boot gate holds
	// to this daemon's contract), and under Kubernetes iff a plugin pair to install is known. False
	// means a stale claim is recorded and reported, never relaunched or refused — relaunching
	// nothing it cannot build, and refusing nothing a relaunch is not already under way to fix.
	PluginRelaunchable bool
	// TaskConfirmed is told of each claim whose pending delivery a turn has just confirmed: the
	// agent took the work the daemon gave it. The workflow decides what a root still owes its
	// architect from that (admission's ReconcileAwaiting), so without it a root whose wait was
	// held by an untaken task would wait for the next tick. Nil tells no one. It is called inside
	// the machine's decision, so it returns at once.
	TaskConfirmed func(c Claim)
	// SessionLost is told of each claim whose recorded session no store held any more, once the
	// fresh agent that replaces it is running (startFresh): the issue, its architect and the
	// controller each have to know that a worker is continuing work it cannot read, and nothing
	// else would say so. detail is why, in the runtime's words. Nil tells no one.
	SessionLost func(c Claim, detail string)
	// SessionStoreFault is told when a start cannot reach the session store at all, at the first
	// such start and hourly while it lasts (startWaitsForStore): the claim is not failing and
	// nothing of it is lost, but no worker of it runs until a person fixes the store. Nil tells no
	// one. Both are called inside the machine's decision, as TaskConfirmed is, so both return at
	// once.
	SessionStoreFault func(c Claim, detail string)
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
		{"PluginContract", d.PluginContract},
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
		{"Timeouts.Stop", d.Timeouts.Stop},
		{"Timeouts.LaunchBackoff", d.Timeouts.LaunchBackoff},
	} {
		if wait.value <= 0 {
			return fmt.Errorf("supervise: %s must be positive, got %s", wait.name, wait.value)
		}
	}
	return nil
}

// RefusedError is a request the claim does not allow — the answer an API route gives its caller
// instead of pretending the request happened. A refusal the transition table makes is the claim's
// state refusing the request, and says which state and why (Reason). One an action makes is about
// something else — a delivery already pending, the claim being its tree's root — and carries the
// sentinel a caller can act on (Err: ErrDeliveryPending, ErrRootStop), which is what it says.
type RefusedError struct {
	State   ClaimState
	Request string
	Reason  string
	Err     error
}

func (e *RefusedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s refused: %v", e.Request, e.Err)
	}
	return fmt.Sprintf("%s refused: the claim is %s (%s)", e.Request, e.State, e.Reason)
}

func (e *RefusedError) Unwrap() error { return e.Err }

// ErrDeliveryPending is a delivery refused because the claim already holds one: a wait, not a
// fault — the claim takes the next task once its pending delivery's turn is over, so the caller
// asks again later.
var ErrDeliveryPending = wait.New("a delivery is already pending")

// ErrRootStop is a stop of the tree's root claim that is not its tree's close, refused in every
// state: the root ends only with its tree. The refusal adds what stops the root's process instead,
// where one runs.
var ErrRootStop = errors.New("the tree's root claim ends only when its tree closes")

// ErrRetryArchitectOnly is the operator's retry of a failed claim that is not an architect's: a
// phase worker and the daemon's controller each have their own retry (operatorRetry). The refusal
// adds who retries the claim.
var ErrRetryArchitectOnly = errors.New("the operator's retry relaunches only an architect")

// ErrHeldByOperator is the daemon's start or task meeting an operator's suspension, which no rule
// overrides (Claim.RuleHolds): a wait, ended only by the operator's resume. RuleHoldError wraps it
// for the outbox log.
var ErrHeldByOperator = wait.New("the suspension holds until the operator resumes the claim")

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
	// unsaved is a pending delivery that memory changed and the store did not take: settle writes
	// it again, with the claim, before the next decision (saved).
	unsaved bool
	// sentThrough is the registration sequence of the newest connection a prompt was sent through
	// (takesBackConfirmed). A send always goes through the connection registered last, so it only
	// rises, and a relaunch leaves it: the new process's connections are registered later still.
	sentThrough uint64
	// askFirst is a restored claim whose agent an earlier daemon was talking to: a pending
	// delivery it may already have sent, or a turn it saw start and may not have seen end. The
	// machine asks the agent (get_state) before it acts on either.
	askFirst bool
	// unansweredAsks counts the asks in a row the agent left unanswered while askFirst stayed armed
	// on a working claim (askTurnOver); from unansweredAsksWarn on, each one is logged at Warn.
	unansweredAsks int
	// held is the suspension held for the agent's turn (holdSuspension), nil for none.
	held *RequestSuspend
	// interrupt is the turn this claim was interrupted in for a start that takes over its issue's
	// phase (Quiesce), nil for none; quiesced is the newest such start that has found the claim out
	// of a turn. Both are memory only (Quiesce).
	interrupt *interrupt
	quiesced  int64
	// previous is the incarnation the claim last ran and no longer records — stopped by a
	// suspension, retired, failed on, or found dead — which every launch of the same session hands
	// the runtime to wait out until one starts. letGo is the one way a process gets here. It is
	// memory only: after a restart the boot orphan sweep has reaped every process no claim records.
	previous *runtime.Locator
	// relaunch is the relaunch relaunchLater left waiting, tried again when TimerRelaunch fires; it
	// lives as long as that timer, which letGo disarms with it. It is memory only, as the timer is: a
	// restart launches that claim again as an unfinished launch.
	relaunch func(context.Context) error
	// cancelStart cancels the start in flight, and is nil between starts; startStopped is whether
	// a suspension cancelled one, read once by that suspension's own row (stoppedAStart). startMu
	// is their own lock, held only to set, clear or call them, so a suspension can reach them
	// while the decision that started the launch holds mu for minutes (stopStart, startable).
	// storeFaultTold is when the issue and the controller were last told the session store could
	// not answer, so a start that retries every minute reports once an hour (startWaitsForStore).
	startMu        sync.Mutex
	cancelStart    context.CancelFunc
	startStopped   bool
	storeFaultTold time.Time
	// identity is the latest hello2 identity for the claim's current incarnation,
	// kept until its enrollment lands; enrollmentSent is whether that enrollment has been handed
	// to the shim over the claim's current connection.
	identity       *heldIdentity
	enrollmentSent bool
	// stale is every stale event already logged, so a repeated one is dropped in silence.
	stale map[string]bool
	// goroutines counts sends whose outcome has not been handled yet; idle wakes Wait.
	goroutines int
	idle       *sync.Cond
	// view is the claim View reports: published under mu as every locked section that can change
	// the claim ends (unlock) and at every write of the claim (persist), and read without mu, so a
	// reader never waits on a decision in flight. stopping is whether that decision is stopping
	// the claim's process for good (ClaimView.Stopping).
	view     atomic.Pointer[ClaimView]
	stopping bool
	// deciding is the event the decision in flight handles, nil between decisions (Deciding). It
	// has a lock of its own, held only to set, clear or read it, so a reader never waits on mu.
	decidingMu sync.Mutex
	deciding   Event
}

type sending struct {
	id         string
	generation uint64
}

// armed is one scheduled timer: its arming's sequence and delivery, how to cancel it, and when it
// fires (due), so a later arming can tell whether it would fire sooner.
type armed struct {
	seq        uint64
	deliveryID string
	cancel     Cancel
	due        time.Time
}

// NewMachine builds the machine for one claim: a new one the spawn route just stored, or one the
// daemon read back at boot. A restored claim that was booting, or registered and not yet ready, is
// watched again from now, and a restored delivery that may already have been sent is sent again
// only after asking the agent. ctx bounds the machine's own work — its timers and its sends.
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
	case StateRegistered:
		m.armRegistration()
	case StateReady, StateIdle:
		m.askFirst = c.Pending != nil && c.Pending.ConfirmedAt.IsZero()
	case StateWorking:
		m.askFirst = true
	}
	m.publish()
	return m, nil
}

// Handle is one event: fenced, then looked up in the transition table, then acted on. A request
// the claim's state does not allow is refused with a RefusedError; a fenced request is refused
// with the claim refusal it violates (claim.StaleGeneration, claim.SameAgentRefusal). Any other
// event that is stale is logged once and dropped. A suspension of a claim whose agent is in a turn
// answers ErrSuspendHeld (holdSuspension).
//
// Handle returns when the decision is made, which includes the runtime call it makes: a stop
// waits out its grace, a resume waits out the previous incarnation. A caller feeding many claims
// from one source hands each machine its events on its own goroutine, so one claim's stop does
// not hold another's hello. Meanwhile Deciding names ev, whoever the caller.
//
// A suspension is the one event that acts before it takes the lock: it cancels the start in
// flight, if one is running (stopStart), and then waits for the decision that start belongs to as
// every event does. Without that, suspending a worker whose pod is coming up waited out the whole
// launch — minutes under Kubernetes, and 120 seconds was not enough for an operator's
// `legion claims suspend` to be answered at all (dispatch://INFRA-714 R10). The cancelled start
// has charged nothing, and the row this suspension then runs finds a claim launching with no
// process to stop (holdSuspendUntilReady).
func (m *Machine) Handle(ctx context.Context, ev Event) error {
	if _, suspending := ev.(RequestSuspend); suspending {
		m.stopStart("a suspension of the claim is waiting for its start")
	}
	m.mu.Lock()
	defer m.unlock()
	m.decide(ev)
	if token := claimOf(ev); token != m.claim.Token {
		return fmt.Errorf("supervise: %T for claim %s reached the machine of %s", ev, token, m.claim.Token)
	}
	// Settle first, so the fence judges the task the row will find: a settle after the fence could
	// retire the very task it let the event through for.
	if err := m.settle(ctx); err != nil {
		return err
	}
	if pass, err := m.fence(ctx, ev); !pass {
		return err
	}
	if hello, ok := ev.(StreamHello); ok && hello.AgentSecrets != nil && m.claim.Locator != nil && m.deps.Secrets != nil {
		identity := *hello.AgentSecrets
		if e := m.claim.Enrollment; e != nil && e.Incarnation == m.claim.Locator.Incarnation {
			identity.PodToken = "" // already enrolled for this incarnation; the token is not needed again
		}
		m.identity = &heldIdentity{incarnation: m.claim.Locator.Incarnation, AgentSecretsIdentity: identity}
		m.enrollmentSent = false
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
	// A row ends only in a state it names, except that a held suspension may end in suspended from
	// whichever row the end comes through (holdSuspension); a claim that holds none may not.
	from, held := m.claim.State, m.held != nil
	err := errors.Join(r.act(m, ctx, ev), m.settle(ctx))
	if to := m.claim.State; to != from && !slices.Contains(r.to, to) && (!held || to != StateSuspended) {
		err = errors.Join(err, fmt.Errorf("supervise: row %q moved %s from %s to %s, outside %v",
			r.name, m.claim.Token, from, to, r.to))
	}
	return err
}

// ReleaseUncertainLaunch is the only path out of the persisted uncertain-launch state. The daemon
// calls it only after reconciliation proves the unrecorded predecessor absent or reaped; concurrent
// old-pane hellos move the state first, so the caller does not open a second pane. The release is
// written, queued at the generation the claim had, so the store and memory agree whatever the
// relaunch then meets: a refused admission leaves both queued. A release the store does not take
// is undone, the claim left launch_uncertain in both. A stop between the release and the launch's
// own write leaves the claim stored queued at that generation, which ReleasedLaunch tells apart.
func (m *Machine) ReleaseUncertainLaunch(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.State != StateLaunchUncertain || m.claim.Locator != nil {
		return false, nil
	}
	m.claim.State = StateQueued
	if err := m.persist(ctx); err != nil {
		m.claim.State = StateLaunchUncertain
		return false, err
	}
	return true, nil
}

// ReleasedLaunch is whether a stored claim is an uncertain launch ReleaseUncertainLaunch released
// and no launch then recorded: queued at a generation above zero, with no process. A claim is
// created queued at generation zero, and only a release makes one queued again after a launch, so
// the daemon's next boot counts such a claim as a launch to finish.
func (c Claim) ReleasedLaunch() bool {
	return c.State == StateQueued && c.Generation > 0 && c.Locator == nil
}

// StartedBy records the outbox row of the start being run against this claim, so a stop written
// before it can be told apart from one written after. A stop is retried until the runtime takes
// it, and a retry that lands after this start would suspend the run this start began. That
// includes a held suspension (holdSuspension), which the start drops — unless it is the operator's:
// a claim an operator's suspension holds records no start and answers ErrHeldByOperator, so the
// start waits for the operator to resume the claim.
func (m *Machine) StartedBy(ctx context.Context, row int64) error {
	m.mu.Lock()
	defer m.unlock()
	if m.current().RuleHolds() {
		return fmt.Errorf("start row %d of %s: %w", row, m.claim.Token, m.current().RuleHoldError())
	}
	if row <= m.claim.LastStartRow {
		return nil
	}
	m.claim.LastStartRow = row
	if m.held != nil {
		m.log.Info("supervise: a start drops the held suspension", "row", row, "reason", m.held.Reason)
		m.dropHeld()
	}
	return m.persist(ctx)
}

// ServingRun is the issue generation of the run whose task this claim's worker took, and 0 for a
// claim that has taken none. It is the daemon's whole answer to which run a completion belongs to:
// a completion names no run, the pane cannot say it (LEGION_GENERATION is the claim's launch
// counter, and a live worker is handed the next run's task without being relaunched), and the
// three sources are read in this order.
//
//  1. The task whose turn is running. A completion is a tool call inside a turn, and one process
//     runs one turn at a time.
//  2. Otherwise the run this claim is left serving: the turn's end retires its delivery, and the
//     worker goes on working that run — a tester waiting on CI, a merger waiting on a person —
//     until another delivery of a run replaces it. A notice's turn has no delivery behind it.
//  3. Otherwise, for a claim that has served no run at all, the task it holds, unless a prompt of
//     a prompt of that task has already come to nothing (Delivery.Prompted). Oh My Pi's own
//     agent_start names no prompt, so a turn that starts after the daemon's wait for it is
//     indistinguishable from a turn of the agent's own and confirms nothing; and a turn an Envoy
//     message starts can run the whole phase before the phase's task is sent at all, which is what
//     INFRA-264's tester did — it registered on a fresh claim, a message to its role topic started
//     its first turn six seconds later, and its completion was refused as belonging to no run. A
//     task the daemon has not sent, or one the agent acknowledged, is the only run such a claim
//     can be working, and cannot be mistaken for an older run's work, because there is no older
//     run this claim served. A task the agent refused is not one it read, and the claim answers
//     for no run until one is. The completion retires the task it is credited to, through the
//     phase it leaves behind (sendPending), so that task is never sent after the work it names is
//     done.
//
// An operator's task belongs to no run and answers for none of them: it carries generation 0.
func (c Claim) ServingRun() uint64 {
	p := c.Pending
	if p == nil || p.Generation == 0 {
		return c.ServingGeneration
	}
	if !p.ConfirmedAt.IsZero() || (c.ServingGeneration == 0 && (!p.DeliveredAt.IsZero() || !p.Prompted)) {
		return p.Generation
	}
	return c.ServingGeneration
}

// Claim is a copy of the claim as the machine holds it now. It waits for the decision in flight,
// so what it reports is what that decision left: a check a decision must not race reads it here.
func (m *Machine) Claim() Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current()
}

// View is a copy of the claim as the machine last published it: as its latest decision left it, or
// as the decision in flight last tried to write it to the store. It never waits, where Claim waits
// out a decision that holds the machine through its runtime call — a Sandbox relaunch waits minutes
// for its pods — so a reader of every claim, like the operator's listing, reads View.
func (m *Machine) View() ClaimView {
	v := *m.view.Load()
	v.Claim = copyClaim(v.Claim)
	return v
}

// Deciding is the event the decision in flight handles (Handle), or nil when the machine decides
// nothing. It never waits on the decision.
func (m *Machine) Deciding() Event {
	m.decidingMu.Lock()
	defer m.decidingMu.Unlock()
	return m.deciding
}

// decide publishes ev as the event the decision now beginning handles (Deciding), and unlock
// clears it. The caller holds mu.
func (m *Machine) decide(ev Event) {
	m.decidingMu.Lock()
	defer m.decidingMu.Unlock()
	m.deciding = ev
}

// current is a copy of the claim as the machine holds it now, with what only memory holds. The
// caller holds mu.
func (m *Machine) current() Claim {
	c := copyClaim(m.claim)
	if m.held != nil {
		c.SuspensionHeld, c.SuspensionHeldBy = true, m.held.By
	}
	return c
}

// publish makes the claim as the machine holds it now the one View reports. The caller holds mu.
func (m *Machine) publish() {
	m.view.Store(&ClaimView{Claim: m.current(), Stopping: m.stopping})
}

// stoppingProcess marks the claim's process as being stopped for good, for View, until the runtime
// call that stops it returns; the end of the decision publishes what it left. The caller holds mu.
func (m *Machine) stoppingProcess() func() {
	m.stopping = true
	m.publish()
	return func() { m.stopping = false }
}

// unlock ends a locked section that may have changed the claim: it publishes the claim, says the
// machine decides nothing, then releases mu.
func (m *Machine) unlock() {
	m.publish()
	m.decide(nil)
	m.mu.Unlock()
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
//
// A hello and a registration are the two events that move a launch on (helloed, register), and a
// live claim past launching records its process. So neither reaches the table while the claim is
// a launch that records none (launchRecordsNoProcess): a hello is dropped, and a registration is
// refused. The process that sent it is not the claim's: its launch was cut short by the daemon's
// stop, which the next boot launches again once its orphan reconciliation has reaped every process
// no claim records, or it is the process a relaunch waiting at the probe interval let go
// (relaunchLater), which the relaunch waits out.
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
		if m.launchRecordsNoProcess() {
			m.dropStale("StreamHello", "process", fmt.Sprint(ev.Generation), "none")
			return false, nil
		}
	case StreamTurnStart:
		if ev.DeliveryID != "" && (pending == nil || pending.ID != ev.DeliveryID) {
			m.dropStale("StreamTurnStart", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case StreamLateRefusal:
		// A refusal naming the prompt that set the mark always reaches the table. One naming the
		// pending id does when its connection sent that prompt, or when it takes back a task a turn
		// not its own confirmed (takesBackConfirmed): a replayed refusal answers an earlier send, and
		// the pending id may be the one its connection re-sent.
		named := pending != nil && pending.MarkedBy == ev.DeliveryID
		pendingNamed := pending != nil && pending.ID == ev.DeliveryID && !ev.Replayed
		if pending == nil || !(named || pendingNamed || m.takesBackConfirmed(ev)) {
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
		if pass, err := m.fenceRequest("register", ev.Generation, ev.Session); !pass {
			return false, err
		}
		if m.launchRecordsNoProcess() {
			m.log.Warn("supervise: refused a registration: the launch at its generation records no process",
				"generation", ev.Generation, "session", ev.Session)
			return false, &RefusedError{State: m.claim.State, Request: "register", Reason: "the launch records no process"}
		}
		return true, nil
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

// launchRecordsNoProcess is whether the claim is launching with no process recorded: a launch
// written launching before it starts (launchAfter) and cut short, or a relaunch waiting at the
// probe interval (relaunchLater). Every other live state records its process.
func (m *Machine) launchRecordsNoProcess() bool {
	return m.claim.State == StateLaunching && m.claim.Locator == nil
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
// agent resumed from its session file when the claim has one — after the process the claim last
// ran is gone — and a fresh spawn when it has none. A process the claim still records is let go
// first, so it is the one waited out. The boot token's hash is persisted before the process
// starts, so the shim's first hello resolves. A launch the runtime or the spec refuses is a launch
// failure and is tried again after the backoff (backOff), waiting out the same process, until the
// budget runs out; only a start that succeeds forgets it. A launch that fails stops the process it
// let go (stopPrevious), whichever step failed. A launch the tree lifecycle refuses (checkLaunch)
// starts nothing and is charged nothing (relaunchRefused), and so is one whose context ended under
// it — the daemon's stop. A runtime waits out a cluster that answers a launch's requests overloaded
// inside the launch's own deadline, so what reaches here failed the launch.
func (m *Machine) launch(ctx context.Context) error {
	return m.launchAfter(ctx, false)
}

// launchAfter is launch, whose first try waits the backoff when charged: the launch follows a
// launch failure charged just before it (relaunchAfterFailure). Each try is written launching, at
// its generation and with every charge so far, before its backoff waits. A try whose write fails
// has started nothing and is tried again at the probe interval (relaunchLater), the process it let
// go stopped: by then the claim records no process, so nothing else would launch it before a
// restart. One the daemon's stop cut short is left to the next boot, as a start it cut short is.
//
// Every try runs on a context of the machine's own (startable), which a suspension cancels: a
// start under Kubernetes waits minutes on a pod, and an operator suspending the claim meanwhile
// waited all of that before its request was even looked at. A cancelled start is charged nothing,
// as a start the daemon's stop cut short is, and the cause of every start that did fail is kept on
// the claim (Claim.LastStartFailure) for whoever asks why it is not running.
func (m *Machine) launchAfter(ctx context.Context, charged bool) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, func(ctx context.Context) error { return m.launchAfter(ctx, charged) }, err)
	}
	m.letGo()
	starting, started := m.startable(ctx)
	defer started()
	for {
		m.claim.Generation++
		token := rand.Text()
		m.claim.BootTokenHash = HashBootToken(token)

		m.claim.State = StateLaunching
		m.claim.Failure = ""
		if err := m.persist(starting); err != nil {
			err = errors.Join(err, m.stopPrevious(starting))
			if starting.Err() != nil {
				return err
			}
			m.log.Warn("supervise: the launch could not be written; trying it again at the probe interval",
				"generation", m.claim.Generation, "error", err)
			m.relaunchLater(m.deps.Timeouts.Probe, func(ctx context.Context) error { return m.launchAfter(ctx, charged) })
			return err
		}
		if charged {
			if err := m.backOff(starting); err != nil {
				return err
			}
		}
		loc, err := m.start(starting, token)
		if err == nil {
			m.previous = nil
			m.claim.Locator = &loc
			m.armBoot()
			m.log.Info("supervise: launched", "generation", m.claim.Generation, "incarnation", loc.Incarnation,
				"resumed", m.claim.SessionFile != "")
			return m.persist(starting)
		}
		if starting.Err() != nil {
			// The runtime refused nothing: the launch ended with its context — the daemon's stop,
			// or a suspension that cancelled the start. The claim stays launching with no process
			// recorded, which that suspension's own row then suspends (holdSuspendUntilReady), and
			// which the next boot launches again once its orphan reconciliation has reaped every
			// process no claim records, the one let go included.
			m.log.Info("supervise: launch cut short; its context ended", "generation", m.claim.Generation, "error", err)
			return err
		}
		m.claim.LastStartFailure = err.Error()
		if errors.Is(err, runtime.ErrSessionStore) {
			return m.startWaitsForStore(starting, err, charged)
		}
		err = errors.Join(err, m.stopPrevious(starting))
		m.claim.Budgets.LaunchFailures++
		m.log.Warn("supervise: launch failed", "generation", m.claim.Generation, "error", err,
			"launchFailures", m.claim.Budgets.LaunchFailures, "limit", m.deps.Limits.LaunchFailures)
		if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
			return errors.Join(err, m.fail(ctx, fmt.Sprintf("launch failures ran out: %v", err)))
		}
		charged = true
	}
}

// startable is the context the tries of one launch run on: the caller's, which a suspension can
// cancel through the machine (stopStart). The returned function clears it and cancels what it
// left, so nothing outlives the launch; a new launch also forgets that an older one was stopped,
// which only the suspension that stopped it reads (startStopped).
func (m *Machine) startable(ctx context.Context) (context.Context, func()) {
	starting, cancel := context.WithCancel(ctx)
	m.startMu.Lock()
	m.cancelStart, m.startStopped = cancel, false
	m.startMu.Unlock()
	return starting, func() {
		m.startMu.Lock()
		m.cancelStart = nil
		m.startMu.Unlock()
		cancel()
	}
}

// stopStart ends the start in flight, if one is running, and reports whether it cancelled one. It
// takes startMu alone, never mu, because the decision that is launching holds mu throughout: that
// is the whole point — a suspension that waited for mu would wait out the very launch it is
// cancelling (Handle). That it stopped one is remembered for the suspension's own row, which runs
// once the cancelled launch has let the lock go (stoppedAStart).
func (m *Machine) stopStart(why string) bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.cancelStart == nil {
		return false
	}
	m.log.Info("supervise: cancelling the start in flight", "why", why)
	m.cancelStart()
	m.startStopped = true
	return true
}

// stoppedAStart reports, once, whether the event now being handled stopped a start of this claim:
// a claim left launching with nothing running because its own suspension cut the launch short,
// which that suspension completes rather than holds (holdSuspendUntilReady).
func (m *Machine) stoppedAStart() bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	stopped := m.startStopped
	m.startStopped = false
	return stopped
}

// sessionStoreRetry is how long a start whose session store could not answer waits before trying
// again, and sessionStoreReport how often the issue and the controller are told while that lasts:
// a store outage is a deployment fault a person fixes, so the claim keeps its session and waits
// for it rather than spending a launch budget on it, and the people who can fix it hear of it at
// once and then once an hour rather than every minute (dispatch://INFRA-714 R8).
const (
	sessionStoreRetry  = time.Minute
	sessionStoreReport = time.Hour
)

// startWaitsForStore is a start the session store could not answer: the table is missing, or the
// store did not reply. That says nothing about the claim's session, so it is neither a lost
// session nor a launch failure — charging it would fail a claim whose agent is fine for a fault
// outside it — and the same session is started again every minute (relaunchLater) until the store
// answers. The issue and the controller are told at once and then hourly, since nothing else says
// why the worker is not running.
func (m *Machine) startWaitsForStore(ctx context.Context, cause error, charged bool) error {
	err := errors.Join(cause, m.stopPrevious(ctx))
	m.log.Error("supervise: the session store could not answer this start; trying it again every minute, charging nothing",
		"generation", m.claim.Generation, "retry", sessionStoreRetry, "error", err)
	m.tellStoreFault(cause)
	m.relaunchLater(sessionStoreRetry, func(ctx context.Context) error { return m.launchAfter(ctx, charged) })
	return errors.Join(err, m.persist(ctx))
}

// tellStoreFault reports the session store fault to the issue and the controller, at the first
// start that met it and at most once an hour after that (Deps.SessionStoreFault). The hour is
// counted from the last report rather than from the first fault, so an outage that ends and
// returns is reported again.
func (m *Machine) tellStoreFault(cause error) {
	if m.deps.SessionStoreFault == nil {
		return
	}
	now := m.deps.Clock.Now()
	if !m.storeFaultTold.IsZero() && now.Sub(m.storeFaultTold) < sessionStoreReport {
		return
	}
	m.storeFaultTold = now
	m.deps.SessionStoreFault(m.current(), cause.Error())
}

// backOff waits before a try that follows a charged launch failure: backoff.Wait from
// Timeouts.LaunchBackoff for the failures charged since the claim was last ready, so the tries a
// budget allows are spread over seconds rather than spent in the moment one fault lasts — a launch
// the runtime refuses would otherwise be tried again at once, and a process that dies as it starts
// be relaunched as soon as its death is seen. The wait holds the machine's decision, as a launch's
// runtime call does. The try it waits for is already written launching with its charge, so a wait
// the daemon's stop ends forgives nothing: the next boot launches that claim again, as it does a
// launch its context cut short.
func (m *Machine) backOff(ctx context.Context) error {
	wait := backoff.Wait(m.deps.Timeouts.LaunchBackoff, m.claim.Budgets.LaunchFailures)
	m.log.Info("supervise: waiting before the next launch", "after", wait, "launchFailures", m.claim.Budgets.LaunchFailures)
	if err := m.deps.Clock.Sleep(ctx, wait); err != nil {
		m.log.Info("supervise: the wait before the next launch was cut short; its context ended", "error", err)
		return err
	}
	return nil
}

// start launches the claim's process for this try: a Resume of its recorded session, or a Spawn
// when it records none.
//
// A Resume the runtime answers with runtime.ErrSessionFileMissing found the recorded session
// nowhere it could be resumed: no store holds it any more. Both runtimes answer that way for that
// one fact — the tmux runtime when no folder it searched holds the session, the Kubernetes
// launcher when the session table does not hold the row (internal/shimwire's
// LauncherStartResult) — and neither has started anything when it does. Resuming would fail the
// same way on every try and wedge the claim until its launch budget ran out, so the claim forgets
// the session and the same try spawns a fresh agent, which registers a session of its own. It is
// charged nothing: a store that lost a session is not the claim failing. The loss is reported
// (Deps.SessionLost) to the issue, its architect and the controller, and the fresh agent is told
// in its own prompt (Claim.SessionLost), since nothing in a new session says that an older one
// existed.
//
// A store that could not answer at all is not that fact and never gets here: it wraps
// runtime.ErrSessionStore and the caller waits for the store (startWaitsForStore). Any other
// refusal of a Resume is a launch failure the caller charges, and the claim keeps its session: a
// fresh agent on a claim that had one is what the same-agent refusal exists to catch.
func (m *Machine) start(ctx context.Context, token string) (runtime.Locator, error) {
	spec, err := m.spawnSpec(ctx, token, false)
	if err != nil {
		return runtime.Locator{}, err
	}
	if m.claim.SessionFile == "" {
		return m.deps.Runtime.Spawn(ctx, spec)
	}
	spec.ResumeSessionFile = m.claim.SessionFile
	loc, err := m.deps.Runtime.Resume(ctx, m.previous, spec)
	if !errors.Is(err, runtime.ErrSessionFileMissing) {
		return loc, err
	}
	return m.startFresh(ctx, token, err)
}

// spawnSpec is the launch's spec: what the daemon builds for this claim, with what only the
// machine knows — who the claim is, the generation and the boot token this try mints. A resume
// adds its session file; a spawn carries none.
//
// sessionLost is the one thing about this launch that is not the claim's own state: true says the
// agent it starts is fresh because the recorded session was found nowhere (startFresh), which the
// spec builder turns into a sentence in the agent's own prompt. It is set on the copy handed to
// the builder and never on the claim the machine keeps, which is the claim it persists.
func (m *Machine) spawnSpec(ctx context.Context, token string, sessionLost bool) (runtime.SpawnSpec, error) {
	building := m.claim
	building.SessionLost = sessionLost
	spec, err := m.deps.Specs.SpawnSpec(ctx, building)
	if err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("build the launch of %s: %w", m.claim.Token, err)
	}
	spec.Claim, spec.Project, spec.Tree, spec.Issue, spec.Role = m.claim.Token, m.claim.Project, m.claim.Tree, m.claim.Issue, m.claim.Role
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = m.claim.Generation, token, ""
	return spec, nil
}

// startFresh spawns a new agent for a claim whose recorded session no store holds any more
// (start). The spec is built again for a launch that lost its session, so the agent that starts
// is told its predecessor is gone rather than left to infer it from an empty transcript, and the
// report to the issue, the architect and the controller goes only once the fresh agent is
// actually running: a spawn that failed is a launch failure the caller charges, and says nothing
// yet about the session.
func (m *Machine) startFresh(ctx context.Context, token string, cause error) (runtime.Locator, error) {
	m.log.Warn("supervise: the recorded session is found nowhere the runtime can resume it, so the claim starts a fresh agent, charged nothing",
		"session", m.claim.Session, "sessionFile", m.claim.SessionFile, "error", cause)
	lost := m.claim.Session
	m.forgetSession(fmt.Sprintf("no session store the runtime could resume it from holds it any more: %v", cause))
	spec, err := m.spawnSpec(ctx, token, true)
	if err != nil {
		return runtime.Locator{}, err
	}
	loc, err := m.deps.Runtime.Spawn(ctx, spec)
	if err != nil {
		return loc, err
	}
	if m.deps.SessionLost != nil {
		m.deps.SessionLost(m.current(), fmt.Sprintf("the session %s this claim recorded is in no session store the runtime could resume it from, so it runs a fresh agent that has read none of its own earlier work: %v", lost, cause))
	}
	return loc, nil
}

// stopPrevious stops the process letGo moved into previous when a launch fails: the claim no longer
// records it, so nothing else ends it. Machine owns it whichever step failed (persisting the launch,
// building its spec, Spawn, Resume); a runtime stops only a process it started itself
// (runtime.Runtime's Spawn and Resume). Suspend stops that one process alone, and does nothing for
// one already gone or replaced; previous is kept, for the next launch to wait out.
func (m *Machine) stopPrevious(ctx context.Context) error {
	if m.previous == nil {
		return nil
	}
	if err := m.deps.Runtime.Suspend(ctx, *m.previous); err != nil {
		return fmt.Errorf("stop the process %s let go before its relaunch: %w", m.previous.Incarnation, err)
	}
	return nil
}

// died is the claim's process found gone — or found to be some other process — while it was
// live: one launch failure, and the same session relaunched after it, or failed when the budget
// is spent. The runtime's account of the death is kept on the claim (Claim.LastDeathCause) before
// anything else, so whoever asks afterwards — the operator's listing, the notices a failed claim
// sends, the relaunched agent's own first prompt — is told why this claim's process ended rather
// than left to read the daemon's log. A task whose turn the process was running goes back to
// waiting (interrupted), for the relaunch to send, and a death with work outstanding is counted
// as one (chargeDeath), failing the claim at the limit. A process that dies while a suspension is
// held leaves the claim suspended instead (endHeld). A process that ended with a pod the daemon
// itself replaced is not a death at all (displaced).
func (m *Machine) died(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: process died", "incarnation", m.claim.Locator.Incarnation, "observed", string(observation.Kind),
		"detail", observation.Detail)
	m.claim.LastDeathCause = deathCause(observation)
	if err := m.interrupted(ctx, "the process died in the task's turn"); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx)
	}
	if m.chargeDeath() {
		return m.fail(ctx, "deaths with work outstanding ran out")
	}
	return m.relaunchAfterFailure(ctx)
}

// deathCause is what the claim records of a death: the runtime's own account of it, behind the
// verdict it reached, so a cause reads as a sentence wherever it is repeated. A runtime that gave
// no detail leaves the verdict alone.
func deathCause(observation runtime.Observation) string {
	if observation.Detail == "" {
		return string(observation.Kind)
	}
	return fmt.Sprintf("%s: %s", observation.Kind, observation.Detail)
}

// AbandonedSession is a session a claim stopped resuming: the id and transcript path it recorded
// for it, why it was left behind, and when. Why is the sentence whoever abandoned it wrote — the
// failure of a claim the provider refused out of its budget, the reason an operator's retry gave,
// the runtime's account of a session no store holds — and it is what the agent that replaces the
// session is told, so it reads the provider's own words for the refusal, the context size in them
// included, rather than a sentence the daemon invented.
type AbandonedSession struct {
	Session     string    `json:"session"`
	SessionFile string    `json:"sessionFile,omitempty"`
	Why         string    `json:"why"`
	At          time.Time `json:"at"`
	// Told is set once a prompt carrying the sentence has gone out (startSend), so the agent that
	// replaced the session is told of it in the first task it is given and in no later one. It
	// rides the claim's next write, as the delivery's own Prompted mark does: a send the store
	// never heard of is one the fresh agent may be told of twice, which is the harmless way round.
	Told bool `json:"told,omitempty"`
}

// Tells is whether a prompt to this claim's agent must still name the session the claim
// abandoned: one was abandoned and it has not been told yet.
func (a AbandonedSession) Tells() bool { return a.Session != "" && !a.Told }

// forgetSession drops the session the claim recorded, leaving the claim as one no agent has
// registered for: the next launch is a fresh spawn that provisions its workspace anew. The turns
// reported for that session go with it (freshRun): the next agent is one the provider has refused
// nothing of. The session is recorded as abandoned, with why it was, since nothing else keeps
// where it went and the fresh agent is told of it in its first task.
func (m *Machine) forgetSession(why string) {
	if m.claim.Session != "" {
		m.claim.Abandoned = AbandonedSession{
			Session: m.claim.Session, SessionFile: m.claim.SessionFile, Why: why, At: m.deps.Clock.Now(),
		}
	}
	m.claim.Session, m.claim.SessionFile = "", ""
	m.freshRun()
}

// fail puts the claim where nothing relaunches it: its timers stop, its locator goes, and the
// pending delivery stays for whoever decides what happens next. A process the claim still records
// — found dead, or never stopped — is suspended first, so nothing of it keeps holding what it ran
// on until the tree closes; a suspension that fails is logged, and the claim fails all the same.
func (m *Machine) fail(ctx context.Context, why string) error {
	if loc := m.claim.Locator; loc != nil {
		if err := m.endProcess(ctx); err != nil {
			m.log.Error("supervise: could not suspend the failed claim's process", "incarnation", loc.Incarnation, "error", err)
		}
	}
	m.letGo()
	m.claim.State, m.claim.Failure = StateFailed, why
	m.log.Error("supervise: claim failed", "why", why, "launchFailures", m.claim.Budgets.LaunchFailures,
		"deaths", m.claim.Budgets.Deaths, "promptFailures", m.claim.Budgets.PromptFailures,
		"promptRetires", m.claim.Budgets.PromptRetires)
	if err := m.persist(ctx); err != nil {
		return err
	}
	m.terminal(StateFailed)
	return nil
}

// release asks the runtime to let go of everything it holds for the claim — the process, when one
// runs. It is the one place the machine hands the runtime a claim to end, so the claim is taken
// once, from the claim the machine holds, for the operator's stop, the tree's close, and an exit.
func (m *Machine) release(ctx context.Context) error {
	defer m.stoppingProcess()()
	if err := m.deps.Runtime.Release(ctx, runtime.Known{Claim: m.claim.Token, Locator: m.claim.Locator}); err != nil {
		return fmt.Errorf("release %s: %w", m.claim.Token, err)
	}
	return nil
}

// suspendProcess asks the runtime to stop the claim's process and keep its session — release's
// counterpart for a claim that goes on — and lets the stopped process go, for the next launch of
// the same session to wait out. A suspension that fails leaves the claim holding its process. A
// claim whose process is retired and relaunched in one decision (chargePrompt, the registration
// deadline) calls it directly; one that does not run again afterwards calls endProcess.
func (m *Machine) suspendProcess(ctx context.Context) error {
	if err := m.deps.Runtime.Suspend(ctx, *m.claim.Locator); err != nil {
		return err
	}
	m.letGo()
	return nil
}

// endProcess is suspendProcess for a claim that does not run again afterwards — suspended, failed,
// or a root that exited — so View says the process is being stopped (ClaimView.Stopping) while the
// runtime stops it, and nothing is routed to the session it stops.
func (m *Machine) endProcess(ctx context.Context) error {
	defer m.stoppingProcess()()
	return m.suspendProcess(ctx)
}

// retire ends the claim: nothing of it runs any more and nothing relaunches it.
func (m *Machine) retire(ctx context.Context) error {
	m.letGo()
	m.claim.State = StateRetired
	return m.persist(ctx)
}

// letGo ends what the machine had with the claim's process: no timer watches it, no relaunch waits
// on one (relaunchLater), no send talks to it, no suspension is held for its turn and no turn of it
// is interrupted any more (the interrupt's start finds it over, quiesced), and the process the
// claim records, when it records one, moves into previous — the claim no longer runs it (it was
// stopped, found dead, or left behind), and the next launch of the same session waits it out.
func (m *Machine) letGo() {
	m.disarmAll()
	m.relaunch = nil
	m.forgetSend()
	m.held = nil
	m.interruptOver()
	if enrollment := ClearEnrollment(&m.claim); enrollment != nil {
		m.revoke(*enrollment)
	}
	m.identity, m.enrollmentSent = nil, false
	if m.claim.Locator != nil {
		m.previous, m.claim.Locator = m.claim.Locator, nil
	}
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
		if observation.Displaced {
			return m.displaced(ctx, observation)
		}
		return m.died(ctx, observation)
	case runtime.StaleAddress:
		return m.repoint(ctx, observation)
	case runtime.Uncertain:
		m.claim.UncertainStreak++
		m.log.Warn("supervise: process uncertain", "streak", m.claim.UncertainStreak, "detail", observation.Detail)
		m.arm(again, after, "")
		return m.persist(ctx)
	}
	return fmt.Errorf("supervise: an observation of kind %q", observation.Kind)
}

// probe asks the runtime about the claim's process now and judges the answer. A claim can reach
// here with no Locator at all: onClosed (table.go) fires on a connection closing, which says
// nothing about whether its claim is still the one that opened it. There is nothing to probe and
// nothing to arm in that case — the claim's own relaunch path, not this probe, decides what
// happens to it next.
func (m *Machine) probe(ctx context.Context, alive func(context.Context) error, again TimerKind, after time.Duration) error {
	if m.claim.Locator == nil {
		m.log.Info("supervise: probed with no process to probe; nothing to do")
		return nil
	}
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
	m.armRegistration()
}

// armRegistration arms the registration deadline at its base Boot×RegistrationIntervals bound,
// plus the runtime's own ProvisionBound while the claim is still StateLaunching: a pod that has
// not said hello yet may still be provisioning. A claim already StateShimConnected — reached by
// a fresh hello (helloed) or restored there (NewMachine) — gets the base bound alone, the same
// one a tmux launch (ProvisionBound always zero) gets throughout, and so does a claim registered
// (register, or restored there), whose agent has that long to say it is ready.
func (m *Machine) armRegistration() {
	grace := time.Duration(0)
	if m.claim.State == StateLaunching {
		grace = m.deps.Runtime.ProvisionBound()
	}
	m.arm(TimerRegistration, runtime.RegistrationDeadline(m.deps.Timeouts.Boot, m.deps.Timeouts.RegistrationIntervals, grace), "")
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
	m.timers[kind] = armed{seq: m.seq, deliveryID: deliveryID, cancel: cancel, due: m.deps.Clock.Now().Add(after)}
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

// persist writes the claim alone. Every write first normalizes state that is valid only while the
// claim records a running process: a capability belongs to a registered agent, a suspension source
// belongs to a suspended claim, and an enrollment belongs to the process it names. A confirmation
// and a retire write the claim together with the delivery they change, each in one transaction.
func (m *Machine) persist(ctx context.Context) error {
	c := m.stored()
	// Published here as well as when the decision ends: a launch writes the claim before it waits
	// in the runtime, and View then shows the launch rather than what it replaced.
	m.publish()
	return m.deps.Store.PutClaim(ctx, c)
}

// NormalizeClaim clears state a claim can hold only while it records a compatible running process.
// It returns an enrollment that lost that process so callers can revoke it at the broker.
func NormalizeClaim(c *Claim) *Enrollment {
	if !HoldsCapability(c.State) {
		c.CapabilityHash = nil
	}
	if c.State != StateSuspended {
		c.SuspendedBy = ""
	}
	if c.Locator == nil {
		return ClearEnrollment(c)
	}
	return nil
}

// ClearEnrollment removes c's enrollment and returns the broker record that must be revoked.
func ClearEnrollment(c *Claim) *Enrollment {
	if c.Enrollment == nil {
		return nil
	}
	enrollment := *c.Enrollment
	c.Enrollment = nil
	return &enrollment
}

// stored normalizes the machine's live claim, revoking any enrollment it detached, then returns the
// form a claim write hands the store. A pending delivery has a table of its own.
func (m *Machine) stored() Claim {
	if enrollment := NormalizeClaim(&m.claim); enrollment != nil {
		m.revoke(*enrollment)
	}
	return StoredClaim(m.claim)
}

// StoredClaim is c in the form a claim write hands the store: a pending delivery has a table of
// its own. Call NormalizeClaim before this when c will also construct a machine.
func StoredClaim(c Claim) Claim {
	c.Pending = nil
	return c
}

// HoldsCapability says whether a claim in state has a registered agent with a running process.
func HoldsCapability(state ClaimState) bool {
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
	case IssueTurnEnded:
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
	case StalePlugin:
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
	case RequestRuleResume:
		return ev.Claim
	case RequestStop:
		return ev.Claim
	case RequestTreeClose:
		return ev.Claim
	case RequestIssueClose:
		return ev.Claim
	case RequestOperatorClose:
		return ev.Claim
	case RequestRetry:
		return ev.Claim
	case RequestOperatorRetry:
		return ev.Claim
	case RequestForgetSession:
		return ev.Claim
	case RequestDeliver:
		return ev.Claim
	case RequestExit:
		return ev.Claim
	case RequestTurnOutcome:
		return ev.Claim
	}
	return ""
}

func copyClaim(c Claim) Claim {
	if c.Locator != nil {
		loc := *c.Locator
		c.Locator = &loc
	}
	if c.Enrollment != nil {
		e := *c.Enrollment
		c.Enrollment = &e
	}
	if c.Pending != nil {
		d := *c.Pending
		c.Pending = &d
	}
	c.BootTokenHash = bytes.Clone(c.BootTokenHash)
	c.CapabilityHash = bytes.Clone(c.CapabilityHash)
	c.TurnOutcomes = slices.Clone(c.TurnOutcomes)
	return c
}
