package supervise

import (
	"bytes"
	"context"
	"crypto/rand"
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

// PodUpdateOpen opens, or keeps open, an update round over this claim: the process holding it is
// about to be replaced, and the worker is asked to push its work and answer ready_for_update
// first. Round is the round's identity and kind in one string (Claim.UpdateRound), Reasons what
// the daemon found — the drift of the pod, or the two plugin contracts — and Task the words the
// worker is given (UpdateTask).
//
// The daemon's sweep posts it to every claim of the round at every tick while the round is open,
// so the action is idempotent: the first post of a round starts the round's clock, and each later
// one only refreshes the reasons, which can grow while the round runs. A claim that has answered
// is asked nothing more, and one whose plugin predates PodUpdateContract is never asked at all.
type PodUpdateOpen struct {
	Claim   claim.Token
	Round   string
	Reasons string
	Task    string
}

// UpdateTask is the task text an update round sends. round is the round, which says what the
// worker is told about the consequence, and reasons is what the daemon found, as
// Claim.UpdateReasons holds it. The sweep (internal/daemon) and openPluginUpdate are its callers,
// and each gives it the round it opened, so the two kinds can never say the other's words.
//
// The three steps are the same for both kinds. The consequence is not. A pod's round deletes the
// pod and the disk the workspace is on, so work the worker does not push is lost. A plugin round
// keeps both: the daemon starts the process again on its own plugin, in the same pod, and only
// the turn is lost.
//
// Another agent reads it, so it follows ASD-STE100: one instruction per sentence, active voice,
// and no word that has a second reading.
func UpdateTask(round, reasons string) string {
	first, consequence := "The daemon must replace the pod that runs this issue's roles.",
		`The replacement deletes the pod and the disk. The disk holds your workspace. Work that you do not push is lost.

The daemon starts your process again on the new pod, in this same session.`
	// The kind is the round's own, read by the one reader of it (Claim.InPluginRound), so the
	// words can never disagree with what endPodUpdate does for the same round.
	if (Claim{UpdateRound: round}).InPluginRound() {
		first, consequence = "The daemon must start your process again on a different plugin.",
			`The restart keeps the pod and the disk. The disk holds your workspace. Work that you do not push stays on the disk. The restart stops the turn that you are in.

The daemon starts your process again on this same pod, in this same session.`
	}
	return fmt.Sprintf(`%s The reasons are: %s.

Do these three steps now:

1. Push every commit you want to keep to this issue's branch.
2. Call the `+"`legion`"+` tool with op: "ready_for_update".
3. End your turn.

%s The daemon holds your next task until your process runs again. Do not start new work now.`, first, reasons, consequence)
}

// PodUpdateEnded is the update round over for this claim: every update_* field goes, and the task
// the round held back is released. Which round ended is the round's own id (Claim.InPluginRound),
// read before the fields go, rather than a flag the caller must keep in step with it. A pod's
// round: the pod itself has just been replaced, and each role comes back on its own session
// through the displaced path, so nothing is relaunched here. A claim's own plugin round
// (refreshPlugin): the process is replaced now, onto this daemon's plugin contract, which is what
// the round was waiting for — and only where that process runs (HoldsCapability), since a
// suspended or launching claim has no process to replace and its next launch installs this
// daemon's plugin itself. A claim in no round at all is decided nothing by this event.
type PodUpdateEnded struct {
	Claim claim.Token
}

// PodUpdateSignalled is the worker's answer to the update task (api's claims/ready-for-update):
// it has pushed everything it wants kept, and the process holding this claim may now be replaced.
// Head is the commit it reports as the head of its issue branch at origin, "" when the branch is
// not there — a planner before its first push — and is the worker's word, which nothing verifies.
// Generation and Session are the claim it says it holds, as every agent request carries them.
type PodUpdateSignalled struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Head       string
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
// ends as a tree close ends it, and drops the session it recorded, since the issue is finished
// and the pod that session lived on goes with the close (record.IssueSuspend). It is not a lost
// workspace: nothing is recovered, and the issue's next run, should a person set it todo again,
// starts fresh on a pod and a disk of its own. Like the tree's close it is the workflow's own
// decision and is not put to TreeClosable.
type RequestIssueClose struct{ Claim claim.Token }

// RequestOperatorClose is the operator closing a tree through the API, through its root claim; the
// tree's other claims are stopped, not closed (api.closeTree). It is put to TreeClosable: an
// operator may close a tree no workflow issue backs, and a tree one does back closes when its
// linger expires, never by hand.
type RequestOperatorClose struct{ Claim claim.Token }

// RequestRetry relaunches a failed or retired claim's session with fresh budgets: the workflow's
// decision that the role runs again — the tree's architect retrying a held phase, or a closed tree
// re-admitted after its linger stopped every claim.
type RequestRetry struct{ Claim claim.Token }

// RequestOperatorRetry is the operator relaunching a failed architect through the API: the retry
// RequestRetry makes, fresh budgets and the same session, from failed alone, for the root and
// sub-architects nothing else relaunches (operatorRetry). A retired claim belongs to its tree's
// lifecycle, which the workflow decides, so the operator's retry refuses it, as it refuses every
// claim that has not failed. It is its own event, as the closes are, so the table answers each
// caller's retry its own way.
type RequestOperatorRetry struct{ Claim claim.Token }

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
func (StalePlugin) isEvent()          {}
func (PodUpdateOpen) isEvent()        {}
func (PodUpdateEnded) isEvent()       {}
func (PodUpdateSignalled) isEvent()   {}
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
func (RequestDeliver) isEvent()       {}
func (RequestExit) isEvent()          {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because the six waits mean six different things.
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
	onDeliver       eventKind = "request_deliver"
	onExit          eventKind = "request_exit"

	onStalePlugin eventKind = "stale_plugin"

	onPodUpdateOpen      eventKind = "pod_update_open"
	onPodUpdateEnded     eventKind = "pod_update_ended"
	onPodUpdateSignalled eventKind = "pod_update_signalled"

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
	case StalePlugin:
		return onStalePlugin
	case PodUpdateOpen:
		return onPodUpdateOpen
	case PodUpdateEnded:
		return onPodUpdateEnded
	case PodUpdateSignalled:
		return onPodUpdateSignalled
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
	case RequestResume, RequestRuleResume:
		return "resume", true
	case RequestStop:
		return "stop", true
	case RequestTreeClose, RequestOperatorClose, RequestIssueClose:
		return "close", true
	case RequestRetry, RequestOperatorRetry:
		return "retry", true
	case RequestDeliver:
		return "deliver", true
	case RequestExit:
		return "exit", true
	case PodUpdateSignalled:
		return "ready_for_update", true
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
	t.ignore(onAcked, "a turn already confirmed the delivery", StateWorking)
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

	// The update round over the process holding this claim: opened and kept open by the daemon's
	// sweep, and ended once the replacement it waited for has been made. inRound is every state a
	// claim can hold an open round in.
	inRound := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected,
		StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended}
	t.row(onPodUpdateOpen, "record the update round, and ask the worker to push its work and answer", openPodUpdate,
		[]ClaimState{StateLaunching, StateFailed, StateWorking}, inRound...)
	t.ignore(onPodUpdateOpen, "the claim failed; what happens to it next is not the supervisor's decision, and no process of it would read the task", StateFailed)
	t.ignore(onPodUpdateOpen, "the claim is retired; nothing of it runs on the pod being replaced", StateRetired)

	t.row(onPodUpdateEnded, "the update round is over: clear it, and relaunch the process or release the task it held", endPodUpdate,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, append(slices.Clone(inRound), StateFailed)...)
	t.ignore(onPodUpdateEnded, "the claim is retired; its update round ended with it", StateRetired)

	// The worker's answer to the update task: it has pushed its work and may be replaced.
	t.row(onPodUpdateSignalled, "the worker signalled that its work is pushed and its process may be replaced", podUpdateSignalled,
		nil, StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended)
	t.ignore(onPodUpdateSignalled, "a claim with no registered agent cannot have signalled",
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected)
	t.ignore(onPodUpdateSignalled, "the claim failed; the process that would have signalled is gone", StateFailed)
	t.ignore(onPodUpdateSignalled, "the claim is retired; its update round ended with it", StateRetired)

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
	t.row(onSuspend, "a suspension of a rule-resuming launch is held until the agent is ready", holdSuspendUntilReady,
		nil, StateLaunching, StateShimConnected)
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

	t.row(onIssueClose, "the workflow's close of the issue as done: release the claim, drop its session and retire it", issueClose,
		[]ClaimState{StateRetired}, stoppable...)
	t.row(onIssueClose, "already retired: the session it kept goes with the issue", issueCloseRetired, nil, StateRetired)

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

func acked(m *Machine, ctx context.Context, _ Event) error {
	m.helloDuringSend = false
	p := m.claim.Pending
	m.markRead(p)
	m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	return m.putPending(ctx)
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
// an earlier daemon). Any other turn is a foreign one, and confirms nothing.
func turnStarted(m *Machine, ctx context.Context, ev Event) error {
	start := ev.(StreamTurnStart)
	m.claim.State = StateWorking
	p := m.claim.Pending
	_, awaiting := m.timers[TimerTurn]
	inFlight := m.send != nil || awaiting || m.askFirst
	own := p != nil && p.ConfirmedAt.IsZero() && (start.DeliveryID == p.ID || (start.DeliveryID == "" && inFlight))
	m.takeBackSignal(own)
	if own {
		return m.confirm(ctx)
	}
	return m.persist(ctx)
}

// takeBackSignal takes back this claim's answer to its open round when the turn starting is not
// the round's own task's: an Envoy notice delivered into the session, or a person's steer, starts
// a turn the daemon sent nothing for, and that turn can commit work the worker's answer already
// said was pushed. The answer stands for the state of the branch when it was given, and a turn
// after it can move the branch, so the worker is asked again and the round waits for the second
// answer. The ask's own clock goes with it (UpdateTaskedAt), so the next post of the round asks
// at once rather than at the re-task bound hours away. Only the turn of an ask of this round
// keeps the answer (updatesRound): that is the turn the worker answers in.
//
// The claim is written by turnStarted's own confirm or persist, so the answer goes in the same
// write as the turn that took it back. ownTask is whether this start is the pending delivery's,
// as turnStarted reads it.
func (m *Machine) takeBackSignal(ownTask bool) {
	if m.claim.UpdateRound == "" || m.claim.UpdateSignalledAt == nil {
		return
	}
	if ownTask && updatesRound(m.claim.Pending, m.claim.UpdateRound) {
		return
	}
	m.log.Warn("supervise: a turn started after this claim answered its update round; the answer is taken back and the worker is asked again",
		"round", m.claim.UpdateRound, "head", m.claim.UpdateHead, "delivery", pendingID(m.claim.Pending))
	m.claim.UpdateSignalledAt, m.claim.UpdateHead, m.claim.UpdateTaskedAt = nil, "", nil
}

// turnEnded is the turn over: the delivery it confirmed retires, and one queued meanwhile goes —
// unless a suspension is held for this turn (holdSuspension), which runs instead; a stop the
// runtime refuses leaves the claim idle, still holding it. A turn interrupted for a start that takes
// over the issue's phase lets that start go on (Quiesce). The agent just said where it is, so
// nothing is left to ask it after a restart.
func turnEnded(m *Machine, ctx context.Context, _ Event) error {
	m.askFirst = false
	m.interruptOver()
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

func ready(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateReady
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
func retry(m *Machine, ctx context.Context, _ Event) error {
	m.claim.Budgets = Budgets{}
	return m.revive(ctx)
}

// operatorRetry is the operator's retry of a failed claim: retry, for an architect, whether its
// tree's root or a sub-architect, which nothing else relaunches. Any other claim has a retry of its
// own, so it is refused, naming that retry: a phase worker's is its architect's retry_or_escalate
// while its issue is held and the next start of its role otherwise, and the daemon's controller's is
// its keeper. A claim's role never changes, so the refusal cannot race the decision it guards.
func operatorRetry(m *Machine, ctx context.Context, ev Event) error {
	switch m.claim.Role {
	case claim.RoleArchitect:
		return retry(m, ctx, ev)
	case claim.RoleController:
		return &RefusedError{State: m.claim.State, Request: "retry",
			Err: fmt.Errorf("%w; the daemon's controller keeper retries the controller", ErrRetryArchitectOnly)}
	default:
		return &RefusedError{State: m.claim.State, Request: "retry",
			Err: fmt.Errorf("%w; a phase worker is retried by its architect's retry_or_escalate while its issue is held, and by the next start of its role otherwise", ErrRetryArchitectOnly)}
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
// its five siblings take this close and no IssueSuspend is queued (workflow/linger.go). The claim
// ends as a tree close ends it, and the session it recorded goes first — the session is a row in
// the session database rather than a file on any disk, so what is dropped is the record of which
// session this claim held. Nothing of the workspace is recovered, and a later start of the issue,
// re-entered by a person's todo, is a fresh spawn on a pod and a disk of its own. A release that
// fails changes nothing, so the close can be asked again.
func issueClose(m *Machine, ctx context.Context, _ Event) error {
	if err := m.release(ctx); err != nil {
		return err
	}
	m.forgetSession()
	return m.retire(ctx)
}

// issueCloseRetired is the issue's close reaching a claim already retired (a tree's close retired it
// with its session kept): the issue is finished, so the session goes with it — a later start of the
// issue, re-entered by a person's todo, is a fresh one — and a claim recording none is left as it
// is.
func issueCloseRetired(m *Machine, ctx context.Context, _ Event) error {
	if m.claim.Session == "" && m.claim.SessionFile == "" {
		return nil
	}
	m.forgetSession()
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

// openPodUpdate opens, or keeps open, this claim's update round (PodUpdateOpen). The daemon's
// sweep posts the event at every tick while the round runs, so the action is idempotent in two
// steps.
//
// The round's own record comes first. A round the claim is not in yet starts its clock here —
// UpdateOpenedAt — and clears what an earlier round left: the task it sent, the signal it got, the
// head that signal reported, and the notice it wrote. The same round again keeps that clock and
// only takes the reasons again, which can grow while the round runs (a second key of the pod drifts
// while the first is still waiting).
//
// The ask comes second, and only for a claim that can still answer. A claim that has already
// signalled is asked nothing more: it is waiting to be replaced. A running process whose
// registration declared a plugin contract below PodUpdateContract cannot answer at all (that
// constant says why), so it is never asked, and the round blocks on it until an operator drains
// it. Every other claim with a registered agent (HoldsCapability), and every suspended one, is
// given the round's task — once, under an id of its own (updateTaskID), and again only once the
// last send is PodUpdateRetask old. A suspended claim is revived for the task, as deliverResuming
// revives one, whatever contract it last recorded: the revive launches it onto this daemon's
// plugin, so the process that comes up can answer. A claim an operator's suspension holds is
// given the task and left suspended, since only the operator's resume ends that suspension, and
// the task goes then.
//
// A round never replaces another while the worker is in the turn of that other round's ask. The
// answer the worker gives carries no round of its own (PodUpdateSignalled): it is recorded
// against whatever round the claim stands in when it arrives, so a switch made mid-turn would
// file the answer to the ask the worker read — a plugin round's, which promises the pod and the
// disk survive — against the pod round that deletes both, and the sweep would replace the pod on
// it. The post returns having changed nothing, and the next post after that turn ends makes the
// switch, which clears the answer and asks the worker again under the new round's own words.
//
// A claim that already holds a phase task is answered ErrDeliveryPending, which is a wait rather
// than a fault: the update task must never displace the work in flight. The refusal is swallowed,
// UpdateTaskedAt is left as it was, and the next tick asks again — which lands as soon as that
// turn is over.
//
// The claim is written only where this post changed it: a round the claim was not in, reasons
// that have grown, or a task this post actually sent. The sweep posts to every claim of every
// open round at every tick, for the hours a round can run, and a post that decided nothing has
// nothing to write — as refreshPlugin's own re-post returns before any write.
func openPodUpdate(m *Machine, ctx context.Context, ev Event) error {
	open := ev.(PodUpdateOpen)
	now := m.deps.Clock.Now()
	changed := false
	if m.claim.UpdateRound != open.Round {
		if m.answeringRound() {
			m.log.Info("supervise: a turn is answering this claim's update round; the round that replaces it waits for that turn to end",
				"round", m.claim.UpdateRound, "next", open.Round, "delivery", pendingID(m.claim.Pending))
			return nil
		}
		m.log.Info("supervise: an update round is open on this claim; its worker is asked to push its work and answer",
			"round", open.Round, "reasons", open.Reasons)
		if err := m.dropUpdateTask(ctx); err != nil {
			return err
		}
		opened := now
		m.clearUpdate()
		m.claim.UpdateRound, m.claim.UpdateReasons, m.claim.UpdateOpenedAt = open.Round, open.Reasons, &opened
		changed = true
	} else if m.claim.UpdateReasons != open.Reasons {
		m.claim.UpdateReasons = open.Reasons
		changed = true
	}
	tasked := m.claim.UpdateTaskedAt
	if err := m.askForUpdate(ctx, open, now); err != nil {
		return err
	}
	if !changed && m.claim.UpdateTaskedAt == tasked {
		return nil
	}
	return m.persist(ctx)
}

// answeringRound is a turn of this claim running on an ask its current round made: the worker is
// reading that round's own words now, and the answer it gives in that turn is that round's
// (openPodUpdate). A turn the daemon sent nothing for is not one — takeBackSignal already takes
// the answer back for those — and neither is a turn on a phase task.
func (m *Machine) answeringRound() bool {
	p := m.claim.Pending
	return m.claim.State == StateWorking && updatesRound(p, m.claim.UpdateRound) && !p.ConfirmedAt.IsZero()
}

// updateTaskPrefix starts the delivery id of every ask an update round makes; the round and a
// fresh word of this ask's own complete it (updateTaskID).
const updateTaskPrefix = "update:"

// updateTaskID is the delivery id of one ask of round, and names no other ask of it. Each ask is
// its own delivery, because the shim answers a prompt carrying the delivery id it already holds
// from its own record itself — an acknowledgement, the replayed agent_start and, for a turn
// already over, an agent_end — and prompts Oh My Pi with nothing (internal/shimwire's Dedupe). A
// second ask under the first ask's id would therefore reach no worker at all, and the worker a
// re-task is for is exactly the one that read the first ask and ended its turn without answering.
//
// What makes it this ask's own is a random word, as an operator's own delivery id is (queue), not
// the clock: two asks are the round's re-task bound apart, but identity that reads the clock is
// identity a clock that steps back repeats.
func updateTaskID(round string) string {
	return updateTaskPrefix + round + ":" + rand.Text()
}

// updatesRound says whether d is an ask of round: every ask of a round carries the round in its
// delivery id, whichever ask of it this is.
func updatesRound(d *Delivery, round string) bool {
	return d != nil && round != "" && strings.HasPrefix(d.ID, updateTaskPrefix+round+":")
}

// updateRoundOf is the round whose ask carries delivery id, and false for an id no update round
// made. An ask's id is updateTaskPrefix, the round, and this ask's own word, so the round is
// everything between the prefix and the last colon: a round's own id carries one colon (PodRound,
// PluginRound) and the word none.
func updateRoundOf(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, updateTaskPrefix)
	if !ok {
		return "", false
	}
	cut := strings.LastIndex(rest, ":")
	if cut <= 0 {
		return "", false
	}
	return rest[:cut], true
}

// rotateTaskID is the new id a task going back to waiting takes, under which the shim's record
// holds nothing: an ask of an update round keeps that round (updateTaskID), and every other task
// takes a fresh word, as an operator's own delivery id does (queue).
//
// The round must survive the rotation. A delivery id is the only record of which round an ask is
// of, and three paths rotate one in the middle of a round — a late agent_start (noTurn), Oh My
// Pi's "Agent is already processing" refusal (lateRefused), and a death mid-turn (interrupted).
// An ask that lost its round would be read as a phase task by every reader of it: updatesRound,
// dropUpdateTask, askForUpdate's guard and takeBackSignal. The round's end would then leave that
// task pending, the worker would be prompted with the words of a round whose pod is already
// replaced, and every task of its next phase would wait behind it (ErrDeliveryPending).
func rotateTaskID(id string) string {
	if round, ok := updateRoundOf(id); ok {
		return updateTaskID(round)
	}
	return rand.Text()
}

// dropUpdateTask takes back an ask of this claim's round that no turn has confirmed, which is
// what a round leaving the claim means for the task it sent: the words name a round that is over
// — a pod this daemon has already replaced — and until some turn reads them, every task of the
// claim's next phase is refused behind this one (ErrDeliveryPending). A task a turn confirmed is
// left where it is: that turn is the worker's, and its end retires the delivery (settle).
func (m *Machine) dropUpdateTask(ctx context.Context) error {
	p := m.claim.Pending
	if !updatesRound(p, m.claim.UpdateRound) || !p.ConfirmedAt.IsZero() {
		return nil
	}
	m.log.Info("supervise: the update round is over for this claim; the task it sent and nothing read is taken back",
		"round", m.claim.UpdateRound, "delivery", p.ID)
	return m.retirePending(ctx)
}

// askForUpdate gives the claim the round's task, when this claim is one the round asks and the ask
// is due. openPodUpdate says which those are.
func (m *Machine) askForUpdate(ctx context.Context, open PodUpdateOpen, now time.Time) error {
	if m.claim.UpdateSignalledAt != nil {
		return nil
	}
	if !HoldsCapability(m.claim.State) && m.claim.State != StateSuspended {
		return nil
	}
	// A running process whose plugin predates PodUpdateContract has no op to answer with, so it
	// is never asked (CannotAnswerUpdate, the one statement of that rule). A suspended claim is
	// asked and revived all the same, whatever contract it last recorded: the revive launches it
	// onto this daemon's plugin, and the process that comes up answers the task waiting for it.
	// Reading the recorded contract of a process that is gone as "this claim can never answer"
	// would leave such a claim suspended through every round this deployment ever opens.
	if CannotAnswerUpdate(m.claim) {
		m.log.Info("supervise: the claim's plugin predates ready_for_update, so it is not asked to answer the update round",
			"round", open.Round, "pluginContract", m.claim.PluginContract, "podUpdateContract", PodUpdateContract)
		return nil
	}
	// An ask of this round that no turn has confirmed is still the task the claim is about to be
	// given — it waits for a connection, for the operator's resume, or for its prompt's turn — so
	// this post has nothing to add. The question is the task the claim holds, never the id it
	// holds it under: each ask has an id of its own (updateTaskID), and a second ask made while
	// the first is still waiting would only rotate the task's id for nobody.
	if p := m.claim.Pending; updatesRound(p, open.Round) && p.ConfirmedAt.IsZero() {
		return nil
	}
	if tasked := m.claim.UpdateTaskedAt; tasked != nil && now.Sub(*tasked) < m.deps.podUpdateRetask() {
		return nil
	}
	task := RequestDeliver{Claim: m.claim.Token, ID: updateTaskID(open.Round), Task: open.Task}
	if err := m.queue(ctx, task); err != nil {
		if errors.Is(err, ErrDeliveryPending) {
			m.log.Info("supervise: the claim holds a task of its own, so the update task waits for that turn to end",
				"round", open.Round, "delivery", pendingID(m.claim.Pending))
			return nil
		}
		return err
	}
	tasked := now
	m.claim.UpdateTaskedAt = &tasked
	if m.claim.State == StateSuspended {
		if !m.claim.RuleMayResume() {
			m.log.Info("supervise: the operator's suspension holds this claim, so the update task waits for the operator's resume",
				"round", open.Round)
			return nil
		}
		return m.revive(ctx)
	}
	return m.sendPending(ctx)
}

// endPodUpdate is the round over for this claim (PodUpdateEnded): the ask it left unread goes,
// every field the round wrote goes, and what follows is the round's own kind, read from its id
// before the fields go (Claim.InPluginRound).
//
// A claim in no round ends nothing. The sweep posts the end from what it read at the start of a
// tick, so a claim whose round ended in between — this same post at an earlier tick, a round the
// registration ended (endCurrentPluginRound), a round cleared by a retirement — is posted an end
// for a round that is already over, and a relaunch there would replace a process no round ever
// asked anything of.
//
// A pod's round ends after the pod itself was replaced. Each role's process died with that pod,
// and the displaced path brings each one back on its own session, so nothing is launched here,
// whatever state the claim is in: a claim that failed while the round ran is left failed for the
// workflow's retry to decide, and sendPending hands a claim with no agent nothing (delivery.go).
// Leaving the round on it instead would outlive the pod the round was opened over, and the sweep
// reads the round of every claim.
//
// A claim's own plugin round ends with the relaunch it was opened for: the worker has pushed its
// work and answered, so the process is replaced now, through the path the stale-plugin lane used
// to take at once (replaceProcess). PluginRelaunchFor records the contract this relaunch is made
// for, as refreshPlugin records it, so a registration that still differs afterwards is charged
// rather than relaunched again for free (staleRegistration).
//
// A plugin round ends for nothing while its worker is in a turn, or while its answer is not
// standing. That end keeps the claim's process and its pod, so what decides it is the claim as
// the machine holds it now, never the sweep's snapshot: the sweep posts the end from claims it
// read at the start of a tick, and a turn that started since — or an answer takeBackSignal took
// back since — makes that post a stop of a worker mid-turn, losing the turn and whatever it has
// not pushed. Nothing is changed there, and the next sweep posts the end again once the turn is
// over and the answer stands.
//
// Only a claim whose process is running is replaced (HoldsCapability). A plugin round whose claim
// is suspended, launching or queued has no process to replace, and its next launch installs this
// daemon's plugin anyway, so the round ends there and launches nothing: relaunching a claim an
// operator suspended would end that suspension, which only the operator's resume may do.
func endPodUpdate(m *Machine, ctx context.Context, _ Event) error {
	if m.claim.UpdateRound == "" {
		m.log.Info("supervise: this claim is in no update round; the end posted for it decides nothing")
		return nil
	}
	if m.claim.InPluginRound() && (m.claim.State == StateWorking || m.claim.UpdateSignalledAt == nil) {
		m.log.Info("supervise: this claim's plugin round is not ready to end; its process is left as it is and the next sweep posts the end again",
			"round", m.claim.UpdateRound, "state", m.claim.State, "answered", m.claim.UpdateSignalledAt != nil)
		return nil
	}
	relaunch := m.claim.InPluginRound() && HoldsCapability(m.claim.State)
	m.log.Info("supervise: the update round is over for this claim", "round", m.claim.UpdateRound, "relaunch", relaunch)
	if !relaunch {
		return m.leaveRound(ctx)
	}
	if err := m.dropUpdateTask(ctx); err != nil {
		return err
	}
	m.clearUpdate()
	m.claim.PluginRelaunchFor = m.deps.PluginContract
	return m.replaceProcess(ctx, "the process on another plugin contract is replaced now that its worker has pushed its work")
}

// leaveRound is the round going from this claim with no process replaced: the ask nothing read
// goes (dropUpdateTask), every field the round wrote goes (clearUpdate), the claim is written,
// and the task the round was holding back is sent. It is the whole of the round's end for a pod's
// round and for a plugin round the registration ended, which differ only in what they log.
// sendPending hands nothing to a claim that is not ready or idle, so a failed or suspended claim
// is sent nothing by it.
func (m *Machine) leaveRound(ctx context.Context) error {
	if err := m.dropUpdateTask(ctx); err != nil {
		return err
	}
	m.clearUpdate()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// endCurrentPluginRound ends a plugin round over a claim whose registration has just recorded
// this daemon's own contract (staleRegistration). What the round waited for has happened: the
// process it was opened over is gone, and the one that registered speaks this daemon's plugin.
// The ask nothing read goes with it, and the task the round held back is sent — a round left open
// here would hold that task until the sweep ended the round, hours later, in a relaunch of a
// process that is already current. A claim in a pod round is left exactly as it is: that round is
// its pod's, which a registration says nothing about.
func (m *Machine) endCurrentPluginRound(ctx context.Context) error {
	if !m.claim.InPluginRound() {
		return nil
	}
	m.log.Info("supervise: the registration speaks this daemon's plugin contract; its plugin round is over",
		"round", m.claim.UpdateRound, "pluginContract", m.claim.PluginContract)
	return m.leaveRound(ctx)
}

// clearUpdate drops every field an update round writes, which is what a round leaving this claim
// means: the round the sweep ends (endPodUpdate), the round a new one replaces, whose fields are
// each the earlier round's (openPodUpdate), and the round a claim retires in (retire).
func (m *Machine) clearUpdate() {
	m.claim.UpdateRound, m.claim.UpdateReasons, m.claim.UpdateHead = "", "", ""
	m.claim.UpdateOpenedAt, m.claim.UpdateTaskedAt = nil, nil
	m.claim.UpdateSignalledAt, m.claim.UpdateToldAt = nil, nil
}

// podUpdateSignalled records the worker's answer to the update task — its work is pushed, and the
// process holding this claim may now be replaced — and the head it reported, which nothing
// verifies. It changes no state: the round's own sweep is what reads the signal and replaces the
// process, once every claim of the round has answered. A signal that arrives while no round is
// open is not an error either; it is recorded, and with no round to end it does nothing.
//
// The line it logs is the one durable record of the head: the claim's UpdateHead goes with the
// round that ends (clearUpdate), and the answer is what an operator reads the replacement back
// against — `git ls-remote` of each head a worker reported says whether the work survived the pod.
func podUpdateSignalled(m *Machine, ctx context.Context, ev Event) error {
	signal := ev.(PodUpdateSignalled)
	at := m.deps.Clock.Now()
	m.claim.UpdateSignalledAt = &at
	m.claim.UpdateHead = signal.Head
	m.log.Info("supervise: the worker answered the update round; its work is pushed and its process may be replaced",
		"round", m.claim.UpdateRound, "head", signal.Head)
	return m.persist(ctx)
}

func nothingToDo(*Machine, context.Context, Event) error { return nil }
