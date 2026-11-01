package supervise

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	Claim        claim.Token
	Generation   uint64
	AgentSecrets *AgentSecretsIdentity
}

// StreamTurnStart is the agent's turn starting. Oh My Pi's own agent_start carries no delivery id;
// only the shim's replay of one, answering a repeated prompt, does.
type StreamTurnStart struct {
	Claim      claim.Token
	DeliveryID string
}

// StreamTurnEnd is the agent's turn ending.
type StreamTurnEnd struct{ Claim claim.Token }

// IssueTurnEnded is a turn ending on another claim of this claim's issue, posted by the daemon to
// every other claim of the issue once that turn's own machine has recorded its end. The roles of
// an issue work one jj workspace, so a phase task this claim is holding rather than adopt that
// working copy under an open turn (sendPending) may go now. It carries no fact of its own — the
// send asks again which claims are in a turn — so one posted for a turn another role began
// meanwhile simply holds again.
type IssueTurnEnded struct{ Claim claim.Token }

// StreamPromptResult is Oh My Pi saying how one prompt's work ended (shimwire.PromptResult): the
// one frame that names the prompt a turn belonged to, where agent_start and agent_end name none.
// It arrives on the heels of the agent_end of the turn the prompt ran, so a turn that ends with
// the prompt's result still to come is a turn the prompt did not run: a background result's own,
// which is what wakes a settled session with background work on.
//
// AgentInvoked is false for a prompt that ran no turn at all — one a slash command or an
// extension answered locally — which is the definitive form of what the wait for the turn
// (TimerTurn) otherwise only times out on.
type StreamPromptResult struct {
	Claim        claim.Token
	DeliveryID   string
	AgentInvoked bool
	Error        string
}

// StreamClosed is the claim's connection closing. It says nothing about the process by itself.
type StreamClosed struct{ Claim claim.Token }

// StreamLateRefusal is Oh My Pi refusing a prompt after it acknowledged it: a failed response for
// the same request, following the successful one.
type StreamLateRefusal struct {
	Claim      claim.Token
	DeliveryID string
	Error      string
	// Replayed is a refusal the connection did not send the prompt for: the shim's backlog
	// replaying what Oh My Pi answered while no daemon was connected. It answers an earlier prompt,
	// possibly of the very delivery this connection is re-sending, so it charges nothing, and it
	// clears the mark that prompt set. It takes the task back only when a turn not its own confirmed
	// it and the task was not sent through the connection it arrived on or a newer one
	// (takesBackConfirmed).
	Replayed bool
	// ConnSequence is the registration sequence of the connection the refusal arrived on
	// (runtime.Conn.Sequence).
	ConnSequence uint64
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
	// TimerRegistration is the registration deadline, which runs until the agent is ready: a live
	// process whose agent never registered, or registered and never said it was ready, is retired.
	TimerRegistration TimerKind = "registration"
	// TimerTurn is the wait for the turn an acknowledged prompt should start.
	TimerTurn TimerKind = "turn"
	// TimerProbe is the re-probe of an uncertain process.
	TimerProbe TimerKind = "probe"
	// TimerSuspend is the held suspension's timer (holdSuspension): the stop timeout of its wait for
	// the agent's turn, and the retry of a stop that failed.
	TimerSuspend TimerKind = "suspend"
	// TimerRelaunch is the retry of a relaunch left waiting with no process (relaunchLater): one whose
	// lifecycle check failed with no cleanup reserved, or a launch whose write failed. The claim waits
	// launching with no process until it fires.
	TimerRelaunch TimerKind = "relaunch"
)

// Timer is a timer the machine armed, firing.
type Timer struct {
	Claim      claim.Token
	Kind       TimerKind
	Generation uint64
	Seq        uint64
	DeliveryID string
}

// StalePlugin is a re-adopted claim found still speaking a plugin contract this daemon does not,
// posted once at boot for every restored claim in that state (refreshStalePlugins in
// internal/daemon), before any of them is relaunched, and again at every orphan sweep for a claim
// still live whose contract still differs: the sweep is the one periodic pass that already reads
// every claim, so a relaunch the tree lifecycle or the store refuses — which leaves the claim
// live, its contract unchanged, and nothing else re-tried — rides that same pass rather than a
// timer of its own; without the sweep's re-post the claim would stay stale for the rest of the
// daemon's life. Trigger says which pass posted it — "boot" or "sweep" — for the log line the
// relaunch writes; a claim still launching is left to its own registration, which decides the
// comparison the moment it arrives.
type StalePlugin struct {
	Claim   claim.Token
	Trigger string
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
	// PluginContract is the contract the registering process's plugin declared, 0 when it
	// declared none.
	PluginContract int
}

// RequestReady is the agent saying it can be prompted.
type RequestReady struct {
	Claim      claim.Token
	Generation uint64
	Session    string
}

// RequestSuspend stops the claim's process and keeps its session. Reason is why, for the line the
// suspension logs; By is who asks, which the suspended claim records (Claim.SuspendedBy) and every
// suspension must name. Deadline, when set, is when a suspension held for a turn stops the process
// mid-turn if the turn has not ended by then (holdSuspension); a daemon's suspension without one
// waits for the turn however long it takes. The daemon sets one for a carried child's role still
// running in its old tree, at the old root's linger expiry, where that tree's close would have
// ended it (the daemon's outbox, stopElsewhere). The operator's suspension sets none: it is bounded
// at the stop timeout from its arrival.
type RequestSuspend struct {
	Claim    claim.Token
	Reason   string
	By       SuspendSource
	Deadline time.Time
}

// RequestResume is the operator's resume: it relaunches a suspended claim's session, whoever
// suspended it.
type RequestResume struct{ Claim claim.Token }

// RequestRuleResume is the daemon's own resume, by rule: it relaunches a suspended claim's session
// unless an operator suspended it (Claim.RuleMayResume): that suspension holds until the operator
// resumes the claim. The daemon asks it of
// a claim that owns its issue's current phase (the outbox's start of that phase, the boot's
// included) and of its own controller. It resumes nothing that is not suspended — a failed claim's
// launch budget is spent, and only the workflow's retry relaunches it — so it is asked again as
// often as the rule meets the claim without ever relaunching one that is running. Reason is the
// rule, for the line it logs.
type RequestRuleResume struct {
	Claim  claim.Token
	Reason string
}

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

// RequestIssueClose is the workflow closing a child issue as done (workflow's leave): the claim
// ends as a tree close ends it, keeping its whole session - the id, which the volume reaping reads
// as its evidence, and the resume path. It is not a lost workspace: nothing is recovered, and the
// issue's next run, should a person set it todo again, resumes that session on a pod and a disk of
// its own (a session the store lost starts fresh and is reported). Like the tree's close it is the
// workflow's own decision and is not put to TreeClosable.
type RequestIssueClose struct{ Claim claim.Token }

// RequestOperatorClose is the operator closing a tree through the API, through its root claim; the
// tree's other claims are stopped, not closed (api.closeTree). It is put to TreeClosable: an
// operator may close a tree no workflow issue backs, and a tree one does back closes when its
// linger expires, never by hand.
type RequestOperatorClose struct{ Claim claim.Token }

// RequestRetry relaunches a failed or retired claim's session with fresh budgets: the workflow's
// decision that the role runs again — the tree's architect retrying a held phase, or a closed tree
// re-admitted after its linger stopped every claim.
//
// FreshSession asks for a new session in place of the recorded one. It is a field rather than an
// event of its own, as the closes are, because both answers are the same decision made by the same
// caller — the claim runs again — and neither skips a check the other makes: it says only which
// session the retry starts. A retry that asks for none still starts one when the provider refused
// the recorded session out of its budget (freshSession).
type RequestRetry struct {
	Claim        claim.Token
	FreshSession bool
}

// RequestOperatorRetry is the operator relaunching a failed architect through the API: the retry
// RequestRetry makes, fresh budgets and the same session, from failed alone, for the root and
// sub-architects nothing else relaunches (operatorRetry). A retired claim belongs to its tree's
// lifecycle, which the workflow decides, so the operator's retry refuses it, as it refuses every
// claim that has not failed. It is its own event, as the closes are, so the table answers each
// caller's retry its own way. FreshSession is `legion claims retry --fresh-session`, as
// RequestRetry takes it.
type RequestOperatorRetry struct {
	Claim        claim.Token
	FreshSession bool
}

// RequestForgetSession drops the named recorded session of a claim that runs nothing — queued,
// suspended, failed, or retired — so that whatever launches it next spawns a fresh agent instead
// of resuming those turns: the operator's own `legion claims forget-session`, and `legion claims
// retry --fresh-session` of a phase worker that failed or that an operator stopped, whose session
// is the operator's to leave behind (api's forgetSession and retryClaim). Session is the session
// the operator means, and a claim recording another is refused, so a decision that changed the
// claim since it was read cannot clear a session nobody asked about.
//
// It launches nothing, which is what makes it its own event rather than another retry: a phase
// worker is relaunched by the workflow's start of its phase, so the two halves of that retry have
// different owners — which session the claim starts on is the operator's to say, and when it
// starts is the workflow's.
type RequestForgetSession struct {
	Claim   claim.Token
	Session string
}

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
	// Assignment is the task the issue's current phase hands the role, which the workflow minted
	// for the start that queued this delivery (record.SuperviseRequest.Assignment). A completion is
	// held to it as well as to the run, since every phase entry of one run carries that run. A task
	// of no assignment — an operator's own, a resume, a production check's look — names none, and
	// leaves the assignment the claim is serving alone.
	Assignment string
}

// RequestExit is the agent reporting its own end.
type RequestExit struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Reason     string
}

// RequestTurnOutcome is the outcome of one turn, reported by the producer that ended it: the
// pi-legion agent_end hook for an Oh My Pi session, the Claude bridge for a Claude Code one, each
// over the claim's own HTTP route. Sequence is that producer's own count of the turns it has
// ended at Generation, from 1, and the pair is the key the claim keeps the report under
// (recordTurnOutcome).
//
// Neither is fenced. A report naming a generation the claim has left is not stale: it is a turn of
// that generation, which belongs in its own place in the order the run is derived in, and the
// secret the route authenticates is the current registration's either way.
type RequestTurnOutcome struct {
	Claim      claim.Token
	Generation uint64
	Sequence   uint64
	Refused    bool
	Message    string
}

func (RuntimeObservation) isEvent()   {}
func (StreamHello) isEvent()          {}
func (StreamTurnStart) isEvent()      {}
func (StreamTurnEnd) isEvent()        {}
func (IssueTurnEnded) isEvent()       {}
func (StreamPromptResult) isEvent()   {}
func (StreamClosed) isEvent()         {}
func (StreamLateRefusal) isEvent()    {}
func (PromptAcked) isEvent()          {}
func (PromptRefused) isEvent()        {}
func (Timer) isEvent()                {}
func (StalePlugin) isEvent()          {}
func (RequestSpawn) isEvent()         {}
func (RequestRegister) isEvent()      {}
func (RequestReady) isEvent()         {}
func (RequestSuspend) isEvent()       {}
func (RequestResume) isEvent()        {}
func (RequestRuleResume) isEvent()    {}
func (RequestStop) isEvent()          {}
func (RequestTreeClose) isEvent()     {}
func (RequestIssueClose) isEvent()    {}
func (RequestOperatorClose) isEvent() {}
func (RequestRetry) isEvent()         {}
func (RequestOperatorRetry) isEvent() {}
func (RequestForgetSession) isEvent() {}
func (RequestDeliver) isEvent()       {}
func (RequestExit) isEvent()          {}
func (RequestTurnOutcome) isEvent()   {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because the six waits mean six different things.
type eventKind string

const (
	onObservation   eventKind = "runtime_observation"
	onHello         eventKind = "stream_hello"
	onTurnStart     eventKind = "stream_turn_start"
	onTurnEnd       eventKind = "stream_turn_end"
	onIssueTurnEnd  eventKind = "issue_turn_ended"
	onPromptResult  eventKind = "stream_prompt_result"
	onClosed        eventKind = "stream_closed"
	onLateRefusal   eventKind = "stream_late_refusal"
	onAcked         eventKind = "prompt_acked"
	onRefused       eventKind = "prompt_refused"
	onBootTimer     eventKind = timerPrefix + eventKind(TimerBoot)
	onDeadline      eventKind = timerPrefix + eventKind(TimerRegistration)
	onTurnTimer     eventKind = timerPrefix + eventKind(TimerTurn)
	onProbeTimer    eventKind = timerPrefix + eventKind(TimerProbe)
	onSuspendTimer  eventKind = timerPrefix + eventKind(TimerSuspend)
	onRelaunchTimer eventKind = timerPrefix + eventKind(TimerRelaunch)
	onSpawn         eventKind = "request_spawn"
	onRegister      eventKind = "request_register"
	onReady         eventKind = "request_ready"
	onSuspend       eventKind = "request_suspend"
	onResume        eventKind = "request_resume"
	onRuleResume    eventKind = "request_rule_resume"
	onStop          eventKind = "request_stop"
	onTreeClose     eventKind = "request_tree_close"
	onIssueClose    eventKind = "request_issue_close"
	onOperatorClose eventKind = "request_operator_close"
	onRetry         eventKind = "request_retry"
	onOperatorRetry eventKind = "request_operator_retry"
	onForgetSession eventKind = "request_forget_session"
	onDeliver       eventKind = "request_deliver"
	onExit          eventKind = "request_exit"
	onTurnOutcome   eventKind = "request_turn_outcome"

	onStalePlugin eventKind = "stale_plugin"

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
	case IssueTurnEnded:
		return onIssueTurnEnd
	case StreamPromptResult:
		return onPromptResult
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
	case StalePlugin:
		return onStalePlugin
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
	case RequestRuleResume:
		return onRuleResume
	case RequestStop:
		return onStop
	case RequestTreeClose:
		return onTreeClose
	case RequestIssueClose:
		return onIssueClose
	case RequestOperatorClose:
		return onOperatorClose
	case RequestRetry:
		return onRetry
	case RequestOperatorRetry:
		return onOperatorRetry
	case RequestForgetSession:
		return onForgetSession
	case RequestDeliver:
		return onDeliver
	case RequestExit:
		return onExit
	case RequestTurnOutcome:
		return onTurnOutcome
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
	case RequestResume, RequestRuleResume:
		return "resume", true
	case RequestStop:
		return "stop", true
	case RequestTreeClose, RequestOperatorClose, RequestIssueClose:
		return "close", true
	case RequestRetry, RequestOperatorRetry:
		return "retry", true
	case RequestForgetSession:
		return "forget-session", true
	case RequestDeliver:
		return "deliver", true
	case RequestExit:
		return "exit", true
	case RequestTurnOutcome:
		return "turn outcome", true
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
	// runsNothing is where a claim has no process of its own, running or coming up: queued, for which
	// nothing was launched, or gone. A launch whose outcome is uncertain may still run, so it is not
	// here. It is the one statement of the set: Claim.RunsNothing reads it in memory, and the store's
	// queries take it as a parameter (RunsNothingStates).
	runsNothing = append([]ClaimState{StateQueued}, gone...)
)

// LiveStates are the states of a claim whose process is up or coming up: launched, and not
// suspended, failed or retired. It is a copy, so no caller changes the table's own set.
func LiveStates() []ClaimState {
	return slices.Clone(live)
}

// RunsNothingStates are the states of a claim with no process of its own (runsNothing). It is a
// copy, so no caller changes the table's own set.
func RunsNothingStates() []ClaimState {
	return slices.Clone(runsNothing)
}

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
	t.row(onObservation, "judge the process; alive and ready or idle, send what is waiting; working after an unanswered ask, ask again whether the turn is over", observe,
		[]ClaimState{StateLaunching, StateFailed, StateWorking, StateIdle}, live...)
	t.ignore(onObservation, noProcess, processless...)

	// The shim.
	t.row(onHello, "the shim connected", helloed, []ClaimState{StateShimConnected}, StateLaunching)
	t.row(onHello, "the shim reconnected before ready: hand it the enrollment if it lacks one", enrolledOnly, nil, StateShimConnected, StateRegistered)
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
	t.row(onAcked, "a turn already confirmed the delivery: the prompt is still outstanding until its result", ackedInTurn, nil, StateWorking)
	t.ignore(onAcked, noSend, unready...)
	t.ignore(onAcked, noSend, gone...)

	t.row(onRefused, "the prompt was refused (charged, unless the agent is in a turn of its own) or lost (not charged): re-queue it", refused,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle, StateWorking)
	t.ignore(onRefused, noSend, unready...)
	t.ignore(onRefused, noSend, gone...)

	t.row(onLateRefusal, "the agent refused an acknowledged prompt", lateRefused,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle, StateWorking)
	t.row(onLateRefusal, "a prompt was refused while no process can be prompted: the mark it set goes",
		refusedUnprompted, nil, slices.Concat(unready, gone)...)

	// Turns.
	t.row(onTurnStart, "a turn started", turnStarted, []ClaimState{StateWorking}, prompted...)
	t.ignore(onTurnStart, "a turn is already in flight", StateWorking)
	t.ignore(onTurnStart, "the agent is not ready, so no delivery is in flight", unready...)
	t.ignore(onTurnStart, noProcess, gone...)

	t.row(onTurnEnd, "the turn ended", turnEnded, []ClaimState{StateIdle, StateWorking}, StateWorking)
	t.ignore(onTurnEnd, "no turn is in flight", prompted...)
	t.ignore(onTurnEnd, "the agent is not ready, so no turn of a delivery is in flight", unready...)
	t.ignore(onTurnEnd, noProcess, gone...)

	t.row(onIssueTurnEnd, "a turn ended elsewhere on the issue: send the task that was waiting for the working copy they share",
		issueTurnEnded, []ClaimState{StateWorking}, prompted...)
	t.ignore(onIssueTurnEnd, "the claim is in a turn of its own; what it holds goes when that turn ends", StateWorking)
	t.ignore(onIssueTurnEnd, noSend, unready...)
	t.ignore(onIssueTurnEnd, noSend, gone...)

	t.row(onPromptResult, "the prompt's work ended: that names the turn it ran, and ends its delivery", promptResulted,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle, StateWorking)
	t.ignore(onPromptResult, noSend, unready...)
	t.ignore(onPromptResult, noSend, gone...)

	// Timers.
	t.row(onBootTimer, "the boot interval: probe the unregistered process", bootInterval,
		[]ClaimState{StateLaunching, StateFailed}, booting...)
	t.ignore(onBootTimer, "the agent registered, so its process is no longer probed each interval", StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onBootTimer, "no boot is being watched", processless...)

	t.row(onDeadline, "the registration deadline: the agent never registered, or never said it was ready", registrationDeadline,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onDeadline, "the agent is ready, so the boot watch is over", StateReady, StateWorking, StateIdle)
	t.ignore(onDeadline, "no boot is being watched", processless...)

	t.row(onTurnTimer, "an acknowledged prompt started no turn", noTurn, []ClaimState{StateLaunching, StateFailed, StateSuspended}, prompted...)
	t.ignore(onTurnTimer, "the turn started", StateWorking)
	t.ignore(onTurnTimer, noSend, unready...)
	t.ignore(onTurnTimer, noSend, gone...)

	t.row(onProbeTimer, "probe the uncertain process again", reprobe, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onProbeTimer, noProcess, processless...)

	t.row(onSuspendTimer, "the held suspension's deadline stops the turn, or its stop is tried again", suspendAtTimeout,
		[]ClaimState{StateSuspended}, StateWorking, StateIdle, StateReady, StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onSuspendTimer, "no suspension is held", StateQueued, StateLaunchUncertain, StateSuspended, StateRetired, StateFailed)

	t.row(onRelaunchTimer, "the waiting relaunch is tried again, or a suspension held meanwhile ends it", relaunchAgain,
		[]ClaimState{StateFailed, StateSuspended}, StateLaunching)
	t.ignore(onRelaunchTimer, "no relaunch is waiting", StateQueued, StateLaunchUncertain, StateShimConnected, StateRegistered,
		StateReady, StateWorking, StateIdle, StateSuspended, StateFailed, StateRetired)

	// A re-adopted claim found still speaking another plugin contract, at boot or at a later sweep.
	t.row(onStalePlugin, "the process speaks another plugin contract: relaunch it onto the current one", refreshStalePlugin,
		[]ClaimState{StateLaunching, StateFailed}, StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onStalePlugin, "a claim still launching is left to its registration, which decides it the moment it arrives",
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateSuspended, StateFailed, StateRetired)

	// Requests.
	t.row(onSpawn, "launch", spawn, []ClaimState{StateLaunching, StateFailed}, StateQueued)
	t.ignore(onSpawn, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onSpawn, "the claim is already launched", live...)
	t.ignore(onSpawn, "a suspended claim is resumed, not spawned", StateSuspended)
	t.ignore(onSpawn, failedClaim, StateFailed)
	t.ignore(onSpawn, retiredClaim, StateRetired)

	t.row(onRegister, "the agent registered", register, []ClaimState{StateRegistered, StateLaunching, StateFailed}, booting...)
	t.row(onRegister, "the recorded session registered again", reregister, []ClaimState{StateLaunching, StateFailed},
		StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onRegister, "no launch has minted a boot token", StateQueued)
	t.ignore(onRegister, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRegister, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onRegister, failedClaim, StateFailed)
	t.ignore(onRegister, retiredClaim, StateRetired)

	t.row(onReady, "the agent is ready", ready, []ClaimState{StateReady, StateWorking, StateSuspended}, StateRegistered)
	t.row(onReady, "the agent said ready again", reready, []ClaimState{StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onReady, notRegistered, booting...)
	t.ignore(onReady, "no launch has minted a boot token", StateQueued)
	t.ignore(onReady, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onReady, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onReady, failedClaim, StateFailed)
	t.ignore(onReady, retiredClaim, StateRetired)

	t.row(onSuspend, "suspend: stop the process, keep the session", suspend, []ClaimState{StateSuspended},
		StateRegistered, StateReady, StateIdle)
	t.row(onSuspend, "hold the suspension for the turn's end: stopping the agent now would cut its turn off", holdSuspension,
		nil, StateWorking)
	t.row(onSuspend, "a suspension of a launch stops the start, and is otherwise held until the agent is ready", holdSuspendUntilReady,
		[]ClaimState{StateSuspended}, StateLaunching, StateShimConnected)
	t.row(onSuspend, "already suspended: an operator's suspend takes the suspension over", suspendedAgain, nil, StateSuspended)
	t.ignore(onSuspend, notRegistered,
		StateQueued, StateLaunchUncertain)
	t.ignore(onSuspend, failedClaim, StateFailed)
	t.ignore(onSuspend, retiredClaim, StateRetired)

	t.row(onResume, "resume the same session", resume, []ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onResume, "the claim is not suspended", live...)
	t.ignore(onResume, "a queued claim is spawned, not resumed", StateQueued)
	t.ignore(onResume, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onResume, failedClaim, StateFailed)
	t.ignore(onResume, retiredClaim, StateRetired)

	t.row(onRuleResume, "resume the same session by rule, unless the operator suspended it", resumeByRule,
		[]ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.row(onRuleResume, "the claim runs: nothing to resume", nothingToDo, nil, live...)
	t.row(onRuleResume, "a queued claim is spawned by its start, not resumed", nothingToDo, nil, StateQueued)
	t.row(onRuleResume, "the previous launch's pane is still uncertain: the boot's reconciliation decides it", nothingToDo, nil, StateLaunchUncertain)
	t.row(onRuleResume, "a failed claim spent its launch budget: only the workflow's retry relaunches it", nothingToDo, nil, StateFailed)
	t.row(onRuleResume, "the claim is retired", nothingToDo, nil, StateRetired)

	stoppable := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered,
		StateReady, StateWorking, StateIdle, StateSuspended, StateFailed}
	t.row(onStop, "release the claim and retire it; the tree's root only at the tree's close", stop, []ClaimState{StateRetired}, stoppable...)
	t.row(onStop, "already retired", nothingToDo, nil, StateRetired)

	t.row(onTreeClose, "the workflow's close of the tree: release the claim and retire it", treeClose, []ClaimState{StateRetired}, stoppable...)
	t.row(onTreeClose, "already retired", nothingToDo, nil, StateRetired)

	t.row(onIssueClose, "the workflow's close of the issue as done: release the claim and retire it, its session kept for a reopen", issueClose,
		[]ClaimState{StateRetired}, stoppable...)
	t.row(onIssueClose, "already retired, with its session kept for a reopen", nothingToDo, nil, StateRetired)

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

	t.row(onOperatorRetry, "the operator's retry of an architect: fresh budgets, and the same session relaunched", operatorRetry,
		[]ClaimState{StateLaunching, StateFailed}, StateFailed)
	t.ignore(onOperatorRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onOperatorRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onOperatorRetry, "the claim has not failed", live...)
	t.ignore(onOperatorRetry, "a suspended claim is resumed, not retried", StateSuspended)
	t.ignore(onOperatorRetry, "a retired claim runs again only when the workflow starts it", StateRetired)

	t.row(onForgetSession, "drop the session so the next launch is fresh", forgetSession, nil,
		StateQueued, StateSuspended, StateFailed, StateRetired)
	t.ignore(onForgetSession, "the claim has a live process or launch", StateLaunchUncertain, StateLaunching,
		StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle)

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

	// The outcome of a turn, from the producer that ended it. Every live state takes one: the
	// report is the producer's HTTP call, which races the turn's end on the worker stream and the
	// relaunch of a process that is coming up, and a report is about the provider rather than
	// about where the claim stands.
	t.row(onTurnOutcome, "record the turn's outcome; the claim fails once the provider's refusals reach the launch failure limit",
		turnOutcome, []ClaimState{StateFailed}, live...)
	t.ignore(onTurnOutcome, "no launch has minted a boot token", StateQueued)
	t.ignore(onTurnOutcome, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onTurnOutcome, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onTurnOutcome, failedClaim, StateFailed)
	t.ignore(onTurnOutcome, retiredClaim, StateRetired)
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
// the prompt budget, never made on the spot. In a working claim it asks again whether the turn an
// earlier daemon saw start is over, when the last ask went unanswered (askTurnOver).
func observe(m *Machine, ctx context.Context, ev Event) error {
	return m.judge(ctx, ev.(RuntimeObservation).Observation, func(ctx context.Context) error {
		if err := m.ensureEnrolled(ctx); err != nil {
			return err
		}
		if m.claim.State == StateWorking {
			return m.askTurnOver(ctx, ev)
		}
		return m.sendPending(ctx)
	}, TimerProbe, m.deps.Timeouts.Probe)
}

// helloed is the shim's first connection: StateLaunching to StateShimConnected, and the only path
// where the process is known to hold a running agent process (OMP's own, not whatever the pod ran
// before it under init), so the registration deadline is re-armed from here at its base
// Boot×RegistrationIntervals alone (armRegistration derives the bound from m.claim.State, which
// this sets to StateShimConnected first): a Kubernetes pod can only dial its hello once both init
// containers have finished. An Oh My Pi that boots and never registers is retired on that base
// deadline alone, counted from the hello rather than from the launch.
func helloed(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateShimConnected
	m.armRegistration()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.ensureEnrolled(ctx)
}

// enrolledOnly is a hello in a state the reconnect changes nothing about — the shim connected or
// the agent registered, not yet ready — except that the reconnected shim may need the enrollment
// it never received, or the resumed claim its first one.
func enrolledOnly(m *Machine, ctx context.Context, _ Event) error { return m.ensureEnrolled(ctx) }

func reconnected(m *Machine, ctx context.Context, _ Event) error {
	if m.send != nil {
		m.helloDuringSend = true
	}
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// reconnectedMidTurn is the shim's hello while the claim is working. Within one daemon's life the
// shim's backlog replays whatever the turn did meanwhile, agent_end included; a restored claim
// may be waiting on an agent_end an earlier daemon received and never recorded, so its first
// hello asks.
func reconnectedMidTurn(m *Machine, ctx context.Context, ev Event) error {
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	return m.askTurnOver(ctx, ev)
}

// unansweredAsksWarn is the count of unanswered asks in a row from which askTurnOver logs each one
// at Warn: a restored working claim whose agent never answers stays working with no limit while
// its process lives, and an operator should see that.
const unansweredAsksWarn = 3

// askTurnOver asks a restored working claim's agent whether the turn an earlier daemon saw start
// is still running, and ends it when the agent says it is not. An ask the agent does not answer
// says nothing about the turn: the claim stays working, the question stays armed, and the next
// sweep asks again. Taken as "not streaming", it would end a turn still running and set the claim
// idle, and the sweep would then send the task into the busy agent every interval.
func (m *Machine) askTurnOver(ctx context.Context, ev Event) error {
	if !m.askFirst {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// No shim yet (a sweep before its hello): the hello asks.
		return nil
	}
	streaming, answered := m.streaming(ctx, conn)
	if !answered {
		m.unansweredAsks++
		level := slog.LevelInfo
		if m.unansweredAsks >= unansweredAsksWarn {
			level = slog.LevelWarn
		}
		m.log.Log(ctx, level, "supervise: the agent did not say whether the turn an earlier daemon saw start is over; the claim stays working and the next sweep asks again",
			"claim", m.claim.Token, "unanswered", m.unansweredAsks)
		return nil
	}
	m.askFirst, m.unansweredAsks = false, 0
	if streaming {
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

// acked is the prompt taken: the task is marked read, the wait for its turn is armed, and the
// prompt is outstanding until Oh My Pi reports how its work ended (awaitingResult), which is
// what says which of the turns that follow was this prompt's.
// It is also where the abandoned session's sentence becomes told, since this is the first word
// the daemon has that a prompt carrying it reached the agent at all (AbandonedSession.Told): a
// prompt the agent refused, or one the transport lost, goes back in the queue and must carry the
// sentence again. The claim then goes in the delivery's own write, so a restart between the two
// cannot read a claim that has told the sentence beside a task no prompt of which was ever
// acknowledged, or the other way about.
func acked(m *Machine, ctx context.Context, _ Event) error {
	m.helloDuringSend = false
	m.awaitingResult = m.claim.Pending.ID
	p := m.claim.Pending
	m.markRead(p)
	m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	if m.claim.Abandoned.Tells() {
		m.claim.Abandoned.Told = true
		return m.saved(m.deps.Store.PutClaimAndDelivery(ctx, m.stored(), *p))
	}
	return m.putPending(ctx)
}

// ackedInTurn is the acknowledgement of a prompt whose turn the stream reported first: Oh My Pi's
// agent_start can land before the answer to the prompt that caused it, and that start has already
// confirmed the delivery (turnStarted). The mark and the wait for a turn are that turn's to need,
// and it has started — but the prompt itself is still outstanding, and which of the turns that
// follow is its own is still a question only its result answers, so it is recorded here too.
// Nothing else moves, and nothing is written.
func ackedInTurn(m *Machine, _ context.Context, _ Event) error {
	m.helloDuringSend = false
	m.awaitingResult = m.claim.Pending.ID
	return nil
}

// refused is a prompt that did not reach a turn from its own send: the agent refused it
// (runtime.ErrPromptRefused), or the transport lost it. Either way a turn that confirmed the send
// meanwhile was not the delivery's, and the delivery keeps its id — the shim drops a refused
// delivery's record, and answers a lost one it did see without a second turn.
//
// A refusal is the agent's answer and is charged like a prompt that started no turn (LEGION-60:
// an agent that refused every prompt would otherwise be retried forever); it is retried by the next
// sweep, hello, turn end, or ready, never on the spot. Oh My Pi's busy answer (agentBusy) is the
// exception, as it is for a late refusal (lateRefused): the agent is in a turn of its own — an
// Envoy notice's, which a tree publishes freely while its worker boots — so it is working, not
// refusing to work, and charging it would spend the budget of the very turn doing the work, three
// such collisions retiring the agent mid-turn. The task waits unread, charged nothing, and that
// turn's end sends it (the next sweep does, when the turn's start was not seen). A prompt lost to
// the transport is not charged and waits for the shim's next hello — which may already have come
// while the send was out.
func refused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(PromptRefused)
	resend := m.helloDuringSend
	m.helloDuringSend = false
	if p := m.claim.Pending; !p.ConfirmedAt.IsZero() {
		p.ConfirmedAt = time.Time{}
		if err := m.putPending(ctx); err != nil {
			return err
		}
	}
	if errors.Is(r.Err, runtime.ErrPromptRefused) {
		if agentBusy(runtime.RefusalReason(r.Err)) {
			m.log.Warn("supervise: the agent refused the prompt; it is in a turn of its own, whose end sends the task again",
				"delivery", r.DeliveryID, "error", r.Err)
			return nil
		}
		m.log.Warn("supervise: the agent refused the prompt", "delivery", r.DeliveryID, "error", r.Err)
		return m.chargePrompt(ctx, "refused: "+r.Err.Error())
	}
	m.log.Warn("supervise: the prompt was lost to the transport; the delivery waits", "delivery", r.DeliveryID, "error", r.Err)
	if resend {
		return m.sendPending(ctx)
	}
	return nil
}

// refusedElsewhere is the part of a late refusal every state shares, and it reports whether that
// was all of it. Two kinds say only that a prompt this task was sent under never ran:
//
//   - A refusal naming a prompt other than the pending one. It is the prompt whose acknowledgement
//     set the read mark (the fence lets no other through). The wait that gave up on it already
//     re-queued the task under a new id and charged the prompt, so the mark goes and nothing else;
//     rotating or charging again would spend the budget twice for one prompt.
//   - A refusal the shim replayed from its backlog, answering a prompt an earlier connection sent.
//     The pending id may be the very delivery this connection is re-sending, whose own answer is
//     still to come, so it clears only the mark it set.
//
// A replayed refusal that takes back a task a turn not its own confirmed is the exception
// (takesBackConfirmed): the task goes back unread, charged nothing, so that turn's end sends it
// again rather than retiring it as served.
func refusedElsewhere(m *Machine, ctx context.Context, r StreamLateRefusal) (bool, error) {
	p := m.claim.Pending
	switch {
	case m.takesBackConfirmed(r):
		m.log.Warn("supervise: a replayed refusal names the prompt a foreign turn confirmed; the task goes back unread",
			"delivery", r.DeliveryID, "error", r.Error)
		return true, m.takeBackPending(ctx, taskUnread)
	case r.Replayed || r.DeliveryID != p.ID:
		m.log.Warn("supervise: a refused prompt had marked the task as read; the mark is cleared",
			"delivery", r.DeliveryID, "replayed", r.Replayed, "error", r.Error)
		return true, m.markUnread(ctx)
	}
	return false, nil
}

// takesBackConfirmed is whether a replayed refusal of the pending prompt says the turn that
// confirmed the task was not the task's. It is when no send is in flight and the task was not sent
// through the connection the refusal arrived on or a newer one: then the confirmation came from a
// turn the agent was already in, and the refused prompt was an older connection's. A send in
// flight, or one through that connection or a later one, may be the prompt whose turn confirmed
// the task, so the task stays confirmed and the refusal clears only the mark. Connections are
// compared by the order they were registered in, not by the order the machine handles their
// events in: a sweep can send through a connection before its hello is handled, the shim can redial
// before the machine reaches the old connection's backlog, and an acknowledgement is handled by
// its send's own goroutine.
func (m *Machine) takesBackConfirmed(r StreamLateRefusal) bool {
	p := m.claim.Pending
	return r.Replayed && p != nil && r.DeliveryID == p.ID && !p.ConfirmedAt.IsZero() &&
		m.send == nil && m.sentThrough < r.ConnSequence
}

// refusedUnprompted is a refusal landing while the claim cannot be prompted: relaunching after the
// process that refused died, or stopped. Nothing is charged or re-sent here, since the process that
// refused is not the one that will be prompted next. It still says the prompt never ran, so the
// mark it set goes; a task still carrying that prompt's id also goes back under a new one, so the
// next send is a new prompt that no refusal of the old one can name.
func refusedUnprompted(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamLateRefusal)
	if done, err := refusedElsewhere(m, ctx, r); done {
		return err
	}
	m.log.Warn("supervise: a prompt was refused while no process can be prompted; the task goes back unread",
		"delivery", r.DeliveryID, "state", string(m.claim.State), "error", r.Error)
	return m.takeBackPending(ctx, taskUnread)
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
	if done, err := refusedElsewhere(m, ctx, r); done {
		return err
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	streaming := func() bool { busy, _ := m.streaming(ctx, conn); return busy }
	if m.claim.State == StateWorking || agentBusy(r.Error) || (connected && streaming()) {
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
// an earlier daemon). Any other turn is a foreign one, and confirms nothing. Either way the claim
// records when this turn began (Claim.LastTurnStart), which is persisted with it: the time is a
// reader's only handle on how long the agent has been in the turn it is in.
func turnStarted(m *Machine, ctx context.Context, ev Event) error {
	start := ev.(StreamTurnStart)
	m.claim.State = StateWorking
	m.claim.LastTurnStart = m.deps.Clock.Now()
	p := m.claim.Pending
	_, awaiting := m.timers[TimerTurn]
	inFlight := m.send != nil || awaiting || m.askFirst
	if p != nil && p.ConfirmedAt.IsZero() && (start.DeliveryID == p.ID || (start.DeliveryID == "" && inFlight)) {
		return m.confirm(ctx)
	}
	return m.persist(ctx)
}

// turnEnded is the turn over: the delivery whose turn it was retires (settle), and one queued
// meanwhile goes — unless a suspension is held for this turn (holdSuspension), which runs
// instead; a stop the runtime refuses leaves the claim idle, still holding it. The agent just
// said where it is, so nothing is left to ask it after a restart. The claim records when the turn
// ended (Claim.LastTurnEnd) before any of that: every path out of here persists the claim, and
// the time is what tells an idle worker that finished a moment ago from one idle since yesterday.
//
// A turn that ends is also the end of the work the deaths were charged against (chargeDeath), so
// the count goes with it: an agent killed twice in its turns and through the third is making
// progress, and only deaths that keep a turn from ending should ever fail its claim.
//
// Whose turn it was is a question background work made real, and settle is where it is answered:
// a turn that ends while the task's prompt is still outstanding retires nothing, because Oh My
// Pi's agent_end names no prompt and a role's own background result starts turns the daemon
// never prompted.
func turnEnded(m *Machine, ctx context.Context, _ Event) error {
	m.claim.LastTurnEnd = m.deps.Clock.Now()
	m.claim.Budgets.Deaths = 0
	m.askFirst = false
	if m.held != nil {
		if err := m.suspendHeld(ctx); err != nil {
			if m.held == nil {
				// The stop landed and let the process go; only its write failed, and the claim is
				// suspended in memory (suspended).
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
	if err := m.settle(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// issueTurnEnded is a turn ending on another claim of this claim's issue. It carries nothing the
// claim records: the send it may unblock asks for itself which claims of the issue are in a turn
// (sendPending), so a wake that arrives while another role has started one holds the task again,
// and a wake that never arrives — a turn whose process died instead of ending it — costs nothing,
// because the sweep's next observation asks the same question.
func issueTurnEnded(m *Machine, ctx context.Context, _ Event) error { return m.sendPending(ctx) }

// promptResulted is Oh My Pi's answer to the one question the turn frames cannot settle: which
// turn was this prompt's. The prompt's work is over, so the delivery's life is over with it —
// whether the claim is idle because the turn already ended (the usual order, the result landing
// milliseconds after that turn's agent_end) or still working because the result came first. The
// retire itself is settle's, which runs around this decision as it does around every other.
//
// A prompt that ran no turn at all (AgentInvoked false) is the definitive form of what the wait
// for the turn (TimerTurn) otherwise only times out on: it is charged the same, and the task goes
// back to waiting with its read mark, since a prompt the agent took and answered without a turn
// may still have been read.
//
// A result for a delivery the claim no longer holds — retired, or taken back under a new id — is
// fenced out before this runs (fence).
func promptResulted(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamPromptResult)
	m.awaitingResult = ""
	if !r.AgentInvoked {
		why := "acknowledged, and Oh My Pi ran no turn for it"
		if r.Error != "" {
			why += ": " + r.Error
		}
		return m.promptFailed(ctx, why, taskRead)
	}
	if p := m.claim.Pending; p.ConfirmedAt.IsZero() {
		// No turn was ever matched to this prompt — each one the claim saw was taken for another's
		// — and yet Oh My Pi ran one for it: the agent did read the task.
		m.log.Info("supervise: the task's prompt ran a turn the daemon never matched to it; confirming it from the result",
			"delivery", r.DeliveryID)
		if err := m.confirm(ctx); err != nil {
			return err
		}
	}
	return m.sendPending(ctx)
}

// resultPending is whether the claim is still waiting on the result of a prompt it has had
// acknowledged: the window in which a turn that ends may be any turn at all. It is open only
// while the agent can still answer — a claim whose process a suspension, a death or a retirement
// has taken is told nothing more, and the turn's end is all the answer there will ever be.
func (m *Machine) resultPending() bool {
	p := m.claim.Pending
	if p == nil || m.awaitingResult == "" || p.ID != m.awaitingResult {
		return false
	}
	switch m.claim.State {
	case StateReady, StateIdle, StateWorking:
		return true
	}
	return false
}

func bootInterval(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(context.Context) error {
		m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		return nil
	}, TimerBoot, m.deps.Timeouts.Boot)
}

// registrationDeadline is a process that has had its boot intervals and whose agent never became
// ready — it never registered, or registered and never said it was ready. Without a held
// suspension, it is retired, counted and relaunched. A relaunch the tree lifecycle refuses is
// checked before the retirement, so the process is kept and the deadline tried again at the probe
// interval, as one whose stop fails is. An operator suspension retained through a rule-resuming
// launch instead completes here: no ready will arrive to run it, and relaunching would drop the
// operator's hold.
func registrationDeadline(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil && m.held.By == SuspendedByOperator {
		return m.suspendHeld(ctx)
	}
	never := "never registered"
	if m.claim.State == StateRegistered {
		never = "registered and never said it was ready"
	}
	return m.probe(ctx, func(ctx context.Context) error {
		if err := m.checkLaunch(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent %s: %w", m.claim.Token, never, err)
		}
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent %s: suspend: %w", m.claim.Token, never, err)
		}
		m.log.Warn("supervise: the agent "+never+"; retired its process", "incarnation", incarnation)
		return m.relaunchAfterFailure(ctx)
	}, TimerRegistration, m.deps.Timeouts.Probe)
}

func noTurn(m *Machine, ctx context.Context, _ Event) error {
	// The bound alone keeps the mark: a turn that has not started within it may still be this
	// task's, starting late, and the agent read the prompt when the turn began.
	return m.promptFailed(ctx, fmt.Sprintf("acknowledged, and no turn started within %s", m.deps.Timeouts.RPC), taskRead)
}

func spawn(m *Machine, ctx context.Context, _ Event) error { return m.revive(ctx) }

// register records the session the agent became. The boot intervals end
// with the registration, but not the deadline: it is armed again at its base bound, and only the
// agent's ready ends it. A registration whose answer never reached its agent leaves a claim no
// ready will ever come for, and from here that looks exactly like an agent slow to boot. A
// registration whose plugin still differs from the daemon's is relaunched at once and refused
// (staleRegistration): the agent that just registered is not the one that relaunch starts.
func register(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	m.claim.Session, m.claim.SessionFile = r.Session, r.SessionFile
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.PluginContract = r.PluginContract
	if r.PluginContract == m.deps.PluginContract {
		m.claim.PluginRelaunchFor = 0
	}
	m.claim.State = StateRegistered
	m.disarm(TimerBoot)
	m.armRegistration()
	if err := m.persist(ctx); err != nil {
		return err
	}
	// A stale registration's process is let go by its relaunch, so it is never enrolled: the
	// refusal returns before the enrollment a current registration gets.
	if err := m.staleRegistration(ctx, r.PluginContract); err != nil {
		return err
	}
	return m.ensureEnrolled(ctx)
}

// reregister is the recorded session registering again: it is issued a new secret, and the
// deadline stays where the first registration set it, so an agent that keeps registering and never
// says it is ready is still relaunched. A registration whose plugin still differs from the
// daemon's is relaunched at once and refused the same way register's is (staleRegistration).
func reregister(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	m.claim.SessionFile = r.SessionFile
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.PluginContract = r.PluginContract
	if r.PluginContract == m.deps.PluginContract {
		m.claim.PluginRelaunchFor = 0
	}
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.staleRegistration(ctx, r.PluginContract)
}

// ready is the agent saying it can be prompted. The time is kept on the claim (Claim.LastReady),
// since it is what dates the two causes beside it — a death and a failed start are both older
// than the agent that is running now (machine.go's Claim).
func ready(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateReady
	m.claim.LastReady = m.deps.Clock.Now()
	m.claim.Budgets.LaunchFailures = 0
	m.disarm(TimerRegistration)
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

// finishReady persists the current claim before publishing ready or delivering work. reready may
// reach it while the claim is idle or working, so it must not rewrite the state to ready.
func finishReady(m *Machine, ctx context.Context) error {
	if err := m.persist(ctx); err != nil {
		return err
	}
	m.terminal(StateReady)
	return m.sendPending(ctx)
}

// reready persists the in-memory ready state before answering an agent retry: a prior ready's
// claim write may have failed after the machine made the transition. A retained launch-time
// suspension still wins: the retry completes that suspension instead of publishing ready and
// releasing notices to a claim the operator or workflow is stopping.
func reready(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

func resume(m *Machine, ctx context.Context, _ Event) error { return m.revive(ctx) }

// resumeByRule is the daemon's own resume of a suspended claim (RequestRuleResume): the same
// session relaunched, as the operator's resume relaunches it, unless an operator suspended the
// claim: that suspension holds until
// the operator resumes the claim. A launch that fails is charged to the claim's launch budget like
// any other, so a claim the rule resumes that keeps failing fails once the budget is spent, and a
// failed claim is never resumed by rule.
func resumeByRule(m *Machine, ctx context.Context, ev Event) error {
	rule := ev.(RequestRuleResume).Reason
	if !m.claim.RuleMayResume() {
		m.log.Info("supervise: not resumed by rule: its suspension holds until the operator resumes it", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
		return nil
	}
	m.log.Info("supervise: resuming by rule", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
	return m.revive(ctx)
}

// retry is a failed or retired claim given another run: its budgets start over, and its session,
// when it has one, is relaunched after the process the claim last ran is gone. A pending delivery
// the claim kept goes once the agent is ready.
//
// A retry that asks for a new session, and one of a claim whose provider refused the recorded
// session out of its budget, forgets that session instead, so the launch spawns a fresh agent
// (freshSession): relaunching a session every turn of which was refused would be refused again,
// and the claim would fail its way through another budget having done nothing.
func retry(m *Machine, ctx context.Context, ev Event) error {
	m.claim.Budgets = Budgets{}
	if fresh, why := freshSession(ev, m.claim, m.deps.Limits.LaunchFailures); fresh {
		m.log.Info("supervise: retrying in a new session", "why", why, "session", m.claim.Session,
			"refusedRun", m.claim.RefusedRun())
		// The failure the claim carries is the fuller answer where it has one — it names how many
		// turns the provider refused and what it said about the newest of them — and the retry's
		// own reason answers for a claim that failed nothing, an operator's retry of a worker it
		// stopped.
		m.forgetSession(cmp.Or(m.claim.Failure, why))
	}
	return m.revive(ctx)
}

// operatorRetry is the operator's retry of a failed claim: retry, for an architect, whether its
// tree's root or a sub-architect, which nothing else relaunches. Any other claim has a retry of its
// own, so it is refused, naming that retry: a phase worker's goes through its issue's phase — the
// architect's retry_or_escalate while the issue is held, the operator's own `legion claims retry`
// while the issue stands in that worker's phase (api's retryClaim), and the next start of its role
// otherwise — and the daemon's controller's is its keeper. A claim's role never changes, so the
// refusal cannot race the decision it guards.
func operatorRetry(m *Machine, ctx context.Context, ev Event) error {
	switch m.claim.Role {
	case claim.RoleArchitect:
		return retry(m, ctx, ev)
	case claim.RoleController:
		return &RefusedError{State: m.claim.State, Request: "retry",
			Err: fmt.Errorf("%w; the daemon's controller keeper retries the controller", ErrRetryArchitectOnly)}
	default:
		return &RefusedError{State: m.claim.State, Request: "retry",
			Err: fmt.Errorf("%w; a phase worker is retried through its issue's phase: its architect's retry_or_escalate while the issue is held, the operator's retry while the issue stands in that worker's phase, and the next start of its role otherwise", ErrRetryArchitectOnly)}
	}
}

// stop ends one claim: the runtime releases it, and it retires. A release that fails changes
// nothing, so the stop can be asked again. The tree's root claim ends only with its tree: a
// retired root would leave the orphan sweep's known set, which would then take whatever the
// runtime holds for the root issue — under a sandbox, its Sandbox, and with it the pod and the
// disk that pod owns. Any other
// stop of it is refused, naming the operator's close when it is that close which ends the tree.
func stop(m *Machine, ctx context.Context, _ Event) error {
	if !m.claim.treeRoot() {
		return m.end(ctx)
	}
	closable, err := m.treeClosable(ctx)
	if err != nil {
		return fmt.Errorf("stop %s: %w", m.claim.Token, err)
	}
	return &RefusedError{State: m.claim.State, Request: "stop", Err: rootStopRefusal(m.claim.State, closable)}
}

// treeClose is the workflow's close of the tree, which ends the root claim too. It is the
// workflow's own decision: it stops every claim of a tree whose linger expired, and the issue
// record it still holds for that tree is exactly what TreeClosable refuses on, so it is not asked.
func treeClose(m *Machine, ctx context.Context, _ Event) error { return m.end(ctx) }

// issueClose is the workflow's close of a child issue as done: it releases the claim through the
// runtime, which ends at most this role's process and forgets the claim. It deletes no issue
// Sandbox — under Agent Sandbox a release deletes only the controller's, whose Sandbox is that
// claim's and nothing else (Runtime.Release, releaseControllerSandbox) — so an issue's pod and the
// disk it owns go with the separately fenced IssueSuspend, or with the tree cleanup once every
// claim of the tree has retired. That is why a merged close leaves the implementer's pod standing:
// its five siblings take this close and no IssueSuspend is queued (workflow/linger.go). A release
// that fails changes nothing, so the close can be asked again.
//
// The claim keeps the session it recorded, both halves of it. A done issue is reopened — a person
// moves it back, a production check fails — and its roles are started again on the same issue,
// where a session forgotten here would start each as a fresh agent that has read none of the work
// it is continuing (dispatch://INFRA-714 A10). The session id is what the Sandbox volume reaping
// reads as its evidence, and the resume path is the only handle either runtime resumes by, so
// dropping the path would be dropping the resume whatever the id said.
//
// A store that has since lost the session is not this decision's to guess at. Keeping the path
// costs one resume when the session is gone: the runtime answers runtime.ErrSessionFileMissing,
// the same try forgets the session and spawns a fresh agent, the claim is charged nothing, and
// the loss is reported to the issue, its architect and the controller, with the fresh agent told
// in its own prompt (Machine.start, startFresh, Deps.SessionLost; dispatch://INFRA-714 R8).
// Clearing the path here instead would reach that same fresh agent silently, which is the one
// outcome R8 forbids. Nothing of the workspace is recovered either way: a later start of the
// issue runs on a pod and a disk of its own, resuming the agent's session on it.
func issueClose(m *Machine, ctx context.Context, _ Event) error {
	if err := m.release(ctx); err != nil {
		return err
	}
	return m.retire(ctx)
}

// forgetSession drops the named session record from a processless claim: the operator's
// `legion claims forget-session`, and the half of `legion claims retry --fresh-session` that is
// the claim's rather than the workflow's (api's retryClaim). It launches nothing — whatever starts
// the claim next, for a phase worker the workflow's own start of its phase, spawns a fresh agent
// rather than resuming the turns the operator asked to leave behind. The comparison runs under
// the machine lock, so a decision that changed the claim since an operator saw it cannot clear a
// later session.
func forgetSession(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestForgetSession)
	if request.Session != m.claim.Session {
		return &RefusedError{Request: "forget-session", Err: fmt.Errorf(
			"%s records session %s; pass --session %s to start fresh", m.claim.Token, m.claim.Session, m.claim.Session,
		)}
	}
	m.log.Info("supervise: forgetting the recorded session at the operator's ask", "session", m.claim.Session,
		"sessionFile", m.claim.SessionFile, "state", string(m.claim.State))
	m.forgetSession("an operator cleared it with `legion claims forget-session`, so this claim starts fresh")
	return m.persist(ctx)
}

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
	closable, err := m.treeClosable(ctx)
	if err != nil {
		return fmt.Errorf("close %s: %w", m.claim.Token, err)
	}
	if !closable {
		return &RefusedError{State: m.claim.State, Request: "close",
			Err: fmt.Errorf("%s is a workflow issue's tree, which closes when its linger expires", m.claim.Tree)}
	}
	return nil
}

// treeClosable is Deps.TreeClosable's answer for this claim's tree; with none, every tree closes.
// Each caller names the request it answers when the read fails.
func (m *Machine) treeClosable(ctx context.Context) (bool, error) {
	if m.deps.TreeClosable == nil {
		return true, nil
	}
	return m.deps.TreeClosable(ctx, m.claim)
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
// which takes a registered agent, and nothing where no process runs. A tree no workflow issue backs
// (closable) ends by the operator's close, which the refusal names; a workflow issue's tree ends
// when its linger expires.
func rootStopRefusal(state ClaimState, closable bool) error {
	var err error
	switch {
	case slices.Contains(processless, state):
		err = fmt.Errorf("%w; %s", ErrRootStop, noProcess)
	case slices.Contains(booting, state):
		err = fmt.Errorf("%w; suspend it to stop its process once its agent has registered", ErrRootStop)
	default:
		err = fmt.Errorf("%w; suspend it to stop its process", ErrRootStop)
	}
	if closable {
		return fmt.Errorf("%w; no workflow issue backs its tree, so legion claims close ends it", err)
	}
	return err
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

// deliverResuming queues a task on a suspended claim and resumes it for that task. The workflow's
// task (an outbox delivery, which carries a durable id) resumes a claim unless an operator
// suspended it: an operator's suspension answers ErrHeldByOperator, queuing
// nothing, and its start waits for the operator's resume. The operator's own delivery resumes the
// claim whoever suspended it.
func deliverResuming(m *Machine, ctx context.Context, ev Event) error {
	d := ev.(RequestDeliver)
	if d.ID != "" && !m.claim.RuleMayResume() {
		return fmt.Errorf("deliver %s to %s: %w", d.ID, m.claim.Token, m.current().RuleHoldError())
	}
	if err := m.queue(ctx, d); err != nil {
		return err
	}
	return m.revive(ctx)
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
		if err := m.endProcess(ctx); err != nil {
			m.log.Error("supervise: could not suspend the exited root's process; suspending its claim anyway",
				"incarnation", incarnation, "error", err)
		}
		return m.suspended(ctx, SuspendedByExit)
	}
	if err := m.release(ctx); err != nil {
		m.log.Error("supervise: could not release the exited agent's process; retiring its claim anyway", "error", err)
	}
	return m.retire(ctx)
}

func nothingToDo(*Machine, context.Context, Event) error { return nil }
