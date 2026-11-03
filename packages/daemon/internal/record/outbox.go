package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// OutboxKind names one durable side effect.
type OutboxKind string

const (
	OutboxKindDispatchStatus   OutboxKind = "dispatch_status"
	OutboxKindDispatchMessage  OutboxKind = "dispatch_message"
	OutboxKindNotice           OutboxKind = "notice"
	OutboxKindControllerNotice OutboxKind = "controller_notice"
	OutboxKindSupervise        OutboxKind = "supervise"
	OutboxKindGateSeed         OutboxKind = "gate_seed"
	OutboxKindLingerClose      OutboxKind = "linger_close"
	OutboxKindIssueSuspend     OutboxKind = "issue_suspend"
	OutboxKindWorkspaceRemove  OutboxKind = "workspace_remove"
	OutboxKindIssueBranch      OutboxKind = "issue_branch"
	OutboxKindCheckCompare     OutboxKind = "check_compare"
	OutboxKindCheckLook        OutboxKind = "check_look"
	OutboxKindCheckBound       OutboxKind = "check_bound"
	OutboxKindRoleMessage      OutboxKind = "role_message"
	OutboxKindPhaseEnded       OutboxKind = "phase_ended"
)

// OutboxLane is which executor of an issue runs a kind. An issue's rows run on three lanes side by
// side. The lifecycle lane runs the rows that start, stop and tidy the issue's roles and their
// resources, one at a time in the order they were queued: each assumes the state its predecessor
// leaves (a start after the branch it waits for, a workspace removal after the suspend that
// stopped the roles). It also carries the production check's rows
// (check_compare, check_look, check_bound), which start and end the implementer's looks in order
// with the starts and suspends they queue. The board lane runs the issue's Dispatch writes and
// notice publishes beside them, so a status write or a notice never waits behind a start pulling
// an image, and a slow publish never holds a stop back. The role-message lane runs the one kind the
// daemon addresses to a role of the issue that is not its architect (phase_ended), on a lane of its
// own because the board lane's architect notices are ordered among themselves and one of them waits
// for as long as its architect holds no role (the daemon's errNoticeWaits): a role told that its
// phase ended must hear it while that is news, not after whatever its tree's architect is owed.
type OutboxLane string

const (
	LaneLifecycle   OutboxLane = "lifecycle"
	LaneBoard       OutboxLane = "board"
	LaneRoleMessage OutboxLane = "role_message"
)

// laneKinds is each lane's kinds in the order LaneKinds lists them. Every OutboxKind is in exactly
// one lane (TestEveryOutboxKindIsInExactlyOneLane).
var laneKinds = map[OutboxLane][]OutboxKind{
	LaneLifecycle:   {OutboxKindSupervise, OutboxKindIssueSuspend, OutboxKindLingerClose, OutboxKindWorkspaceRemove, OutboxKindGateSeed, OutboxKindIssueBranch, OutboxKindCheckCompare, OutboxKindCheckLook, OutboxKindCheckBound},
	LaneBoard:       {OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindRoleMessage},
	LaneRoleMessage: {OutboxKindPhaseEnded},
}

// LaneOf is the lane that runs kind.
func LaneOf(kind OutboxKind) OutboxLane {
	for lane, kinds := range laneKinds {
		if slices.Contains(kinds, kind) {
			return lane
		}
	}
	return ""
}

// LaneKinds is the kinds lane runs, in a fixed order; nil for a lane that is not one.
func LaneKinds(lane OutboxLane) []OutboxKind {
	return slices.Clone(laneKinds[lane])
}

// LaneKey names one lane of one issue.
type LaneKey struct {
	Issue string
	Lane  OutboxLane
}

// HeldRow is a due lifecycle row its lane cannot run yet: WaitsOn is the id of the row of its issue
// under the live lease it waits behind (Store.HeldRows).
type HeldRow struct {
	OutboxRow
	WaitsOn int64
}

// OutboxPayload is the sealed vocabulary of payloads a workflow may enqueue.
type OutboxPayload interface{ OutboxKind() OutboxKind }

// StatusRowKind is what a status row is for, and the one thing that decides the rule the outbox
// judges its retry by (the daemon's stillWanted). It is stored in the payload rather than worked
// out from the row's other fields: while the kind was derived from a set of marks, a kind nobody
// had given a mark read as the ordinary one and was judged by the ordinary rule in silence, so a
// row queued at a status out of the workflow — a re-assert of the daemon's own status over a
// session's close, a person's handover, the daemon's own close — was dropped for standing at the
// very status it exists to undo. Every kind is named once, in StatusRowKinds, which the daemon's
// rule switch and the tests of both packages read, so a kind added to that list and nowhere else
// fails its row and reddens the package rather than borrowing another kind's rule.
type StatusRowKind string

const (
	// OrdinaryStatusRow is the workflow moving an issue along inside the workflow: the phase
	// statuses. The done a merge or an architect's close writes is a CloseStatusRow.
	OrdinaryStatusRow StatusRowKind = "ordinary"
	// PersonHandoverStatusRow is a person's handover: a person's legion label on an unrecorded
	// root in triage, or a person's approval of a parked labelled root's primary document. It is
	// queued at the triage, backlog or icebox that act found, to bring the issue into the
	// workflow. The pending-row rule (Store.PersonHandoverPending) reads the kind to tell such a
	// row from the workflow's other status rows, which suppress nothing: while one is pending for
	// an issue, no second one is queued for it. The outbox's status executor also drops such a row
	// at delivery when the re-read issue no longer carries the label: a person who took it off
	// withdrew the handover. A HandOverStatusRow, also a todo, is not one.
	PersonHandoverStatusRow StatusRowKind = "person-handover"
	// ReassertStatusRow is the daemon setting its own status back over one a session wrote
	// (workflow's sessionStatusWrite): the session's close or park stands in Dispatch and the
	// record kept the daemon's status, so this row is queued observing a status the issue is not
	// meant to be at — done, backlog or icebox — exactly as a person's handover is. Asking the
	// ordinary rule of it, whether the issue is still in the workflow, would drop the row for
	// being at the status it exists to undo.
	ReassertStatusRow StatusRowKind = "reassert"
	// HandOverStatusRow hands a child of a closed tree to Legion: the write also puts
	// dispatch.LegionLabel on the issue, in the same Dispatch write, and the runner makes it while
	// the label is missing even where Dispatch already shows Status.
	HandOverStatusRow StatusRowKind = "hand-over"
	// CloseStatusRow is the daemon's own close of an issue: the done a merge or an architect's
	// close_root writes, queued in the transaction that takes the record out of the workflow. The
	// ordinary rule cannot judge it, for the reason the re-assert's cannot: one refusal — an
	// agent's comment, a handoff record, or the done reason this row itself just posted — sends
	// it to its retry, which would find the record already out of the workflow and drop the write
	// that puts Dispatch there, leaving the issue open in Dispatch for good while the tree
	// lingers and frees its slot. Its own rule asks the question that matters instead: the close
	// still stands in the record at the generation the row names.
	CloseStatusRow StatusRowKind = "close"
)

// StatusRowKinds is every kind of status row, listed once. The daemon's rule switch, the
// migration that gave the stored rows their kind and the tests of both packages read this list,
// so a kind is added in one place and every reader of it is held to the same five.
var StatusRowKinds = []StatusRowKind{OrdinaryStatusRow, PersonHandoverStatusRow, ReassertStatusRow, HandOverStatusRow, CloseStatusRow}

// StatusWrite is a Dispatch status the daemon writes. ObservedStatus is the status Dispatch shows
// until the write runs: the runner writes Status only while Dispatch still shows ObservedStatus, so a
// move someone else made since the write was queued stands. Kind is what the write is for, set by
// the constructor its queueing site goes through — OrdinaryStatusWrite, PersonHandoverStatusWrite,
// ReassertStatusWrite, HandOverStatusWrite or CloseStatusWrite — which are the only places one is
// built, so no row reaches the outbox without a kind (validateOutboxPayload refuses one that does).
type StatusWrite struct {
	Kind           StatusRowKind `json:"kind"`
	Status         string        `json:"status"`
	ObservedStatus string        `json:"observedStatus"`
	// Reason is a done write's reason: the runner posts it as a message on the issue before it
	// writes the status, since Dispatch refuses a message on a closed issue. Empty for a write
	// with no reason to record.
	Reason string `json:"reason,omitempty"`
	// HandOverTree and HandOverLinger fence a hand-over (HandOverStatusRow): the runner writes it
	// only while the issue is still that tree's and the tree's root still lingers at that
	// generation (Issue.LingersAt), so a root re-admitted since keeps the issue as its child.
	HandOverTree   string `json:"handOverTree,omitempty"`
	HandOverLinger uint64 `json:"handOverLinger,omitempty"`
	// CloseGeneration fences the daemon's own close (CloseStatusRow) on the run it closed: every
	// attempt of the row writes done only while the record still holds that generation, so a
	// person's reopen, which re-admits the issue in a generation of its own, drops a close that
	// has not landed rather than closing the issue the person just took back. Zero in a row no generation is
	// known for — one migration 20261102003600 gave its kind, queued by a release that fenced
	// such a write on nothing — which writes as that release wrote it.
	CloseGeneration uint64 `json:"closeGeneration,omitempty"`
}

func (StatusWrite) OutboxKind() OutboxKind { return OutboxKindDispatchStatus }

// OrdinaryStatusWrite is the workflow's own move of an issue inside the workflow, to status from
// the observed status Dispatch shows until the write runs. reason is a done write's, posted as a
// message before the status lands (StatusWrite.Reason); every other write has none.
func OrdinaryStatusWrite(status, observed, reason string) StatusWrite {
	return StatusWrite{Kind: OrdinaryStatusRow, Status: status, ObservedStatus: observed, Reason: reason}
}

// PersonHandoverStatusWrite is the write a person's handover queues, observed at the status that
// person's act found (PersonHandoverStatusRow).
func PersonHandoverStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: PersonHandoverStatusRow, Status: status, ObservedStatus: observed}
}

// ReassertStatusWrite is the daemon's own status set back over the one a session wrote, which the
// row observes (ReassertStatusRow).
func ReassertStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: ReassertStatusRow, Status: status, ObservedStatus: observed}
}

// HandOverStatusWrite hands a child of tree to Legion at status, fenced on that tree and on the
// root generation whose linger queued the write (HandOverStatusRow).
func HandOverStatusWrite(status, observed, tree string, linger uint64) StatusWrite {
	return StatusWrite{Kind: HandOverStatusRow, Status: status, ObservedStatus: observed,
		HandOverTree: tree, HandOverLinger: linger}
}

// CloseStatusWrite is the daemon's own close of an issue, to status (done) from the observed
// status Dispatch shows, with reason posted as a message before the status lands, fenced on the
// generation of the run the close ended (CloseStatusRow).
func CloseStatusWrite(status, observed, reason string, generation uint64) StatusWrite {
	return StatusWrite{Kind: CloseStatusRow, Status: status, ObservedStatus: observed, Reason: reason,
		CloseGeneration: generation}
}

// MessagePost is a Dispatch message body. The runner posts it as Posted: the body, then its outbox
// marker.
type MessagePost struct {
	Body string `json:"body"`
}

func (MessagePost) OutboxKind() OutboxKind { return OutboxKindDispatchMessage }

// The outbox marker closes every message the runner posts. It names the row, so a retried row
// finds the post it already made instead of posting twice.
const (
	messageSeparator   = "\n\n"
	messageMarkerOpen  = "<!-- legion-outbox:"
	messageMarkerClose = " -->"
)

// MessagePostLimit is the longest body the runner can post, in UTF-16 code units: Dispatch's cap
// less the separator and the marker at its longest, whose row id (a bigserial) has at most the 19
// digits of math.MaxInt64. The marker is ASCII, so its length in bytes is its length in units.
const MessagePostLimit = dispatch.MessageBodyLimit -
	len(messageSeparator+messageMarkerOpen+messageMarkerClose) - len("9223372036854775807")

// MessageMarker is the marker of outbox row id.
func MessageMarker(id int64) string {
	return messageMarkerOpen + strconv.FormatInt(id, 10) + messageMarkerClose
}

// outboxKeyPrefix begins the dedupe key the runner publishes a row under.
const outboxKeyPrefix = "legion-outbox:"

// OutboxKey is the dedupe key the runner publishes outbox row id under, so the listener and the
// plugin recognise a retried publish of the row as the same message.
func OutboxKey(id int64) string {
	return outboxKeyPrefix + strconv.FormatInt(id, 10)
}

// ParseOutboxKey is the row id OutboxKey names, and whether key is a key OutboxKey writes.
func ParseOutboxKey(key string) (int64, bool) {
	digits, found := strings.CutPrefix(key, outboxKeyPrefix)
	id, err := strconv.ParseInt(digits, 10, 64)
	return id, found && err == nil && id > 0 && OutboxKey(id) == key
}

// Posted is the message the runner posts for row id: the body, then the row's marker.
func (m MessagePost) Posted(id int64) string {
	return m.Body + messageSeparator + MessageMarker(id)
}

// NoticeKind identifies one persisted notice delivery.
type NoticeKind string

// Notice is a role-facing workflow observation. A phase-finished notice carries the finishing
// worker's own summary and, beside it, the verdict it gave (the tester's pass or fail). A
// review-stuck notice's summary names the fact that wrote it, and its topic is the reviewer's role
// topic, which the architect asks for the decision.
type Notice struct {
	Kind    NoticeKind  `json:"kind"`
	Role    claim.Role  `json:"role,omitempty"`
	Phase   phase.Phase `json:"phase,omitempty"`
	Summary string      `json:"summary,omitempty"`
	Verdict string      `json:"verdict,omitempty"`
	Version int         `json:"version,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	Topic   string      `json:"topic,omitempty"`
	// Resends counts the times the notice was queued again after the listener could not forward
	// it to its architect's session (the daemon's rehold), which stops at a cap.
	Resends int `json:"resends,omitempty"`
	// ResendOf is the outbox row a re-held copy copies. The copy is published under that row's
	// dedupe key (OutboxKey), so the listener's report of a failed forward names the original row,
	// whichever copy failed (exceptionNotice), and the listener's role lane drops a copy that
	// reaches the holder the original reached within its delivery caches
	// (packages/envoy/cmd/listener/delivery.go). The stream and the hosts treat the key as no dedupe
	// key (dedupeKeyNamesItsEvent, packages/contracts/src/envelope.ts). The row is the daemon's own
	// bookkeeping: the executor publishes the notice without it.
	ResendOf int64 `json:"resend_of,omitempty"`
	// CatchUp is a catch-up notice's account of the tree, and absent from every other kind.
	CatchUp *CatchUp `json:"catch_up,omitempty"`
	// OpenCapabilities names every open row of the deployment's capability report
	// (capabilities.Deployment.Open) when the daemon reports them: a deployment row with no
	// decision, or a live row a live session's check failed (LEGION-663). Only a tick controller
	// notice carries it (admit.Admission.ReportCapabilities), and a tick with no open row carries
	// none. The controller reads each row's detail in `legion state`, and on a deployment row alone
	// the legion.yaml line that records a decision; a gap is the operator's to close or decide, a
	// live row's the session's environment to fix, and neither is the controller's.
	OpenCapabilities []string `json:"openCapabilities,omitempty"`
	// Blocked is every issue Legion is waiting to start because it waits on a Dispatch issue
	// nobody has finished (INFRA-225), with what each still waits on. Only a tick controller
	// notice carries it, as OpenCapabilities does: the tick is the turn that posts the day's
	// report, so the controller names each there and takes nothing for it. A tick with nothing
	// blocked carries none.
	Blocked []BlockedIssue `json:"blocked,omitempty"`
	// Actor is who made the spec decision a design-approved, design-changes-requested or
	// carried-gate-opened notice reports, or the decision a parked-decision notice relays: the
	// Dispatch actor as `<kind>:<id>`, a person `user:<login>`. Empty where no person decided:
	// gates.design off opened the gate.
	Actor string `json:"actor,omitempty"`
	// Ref is that decision's Dispatch reference: `dispatch://<KEY>/spec` for a spec decision,
	// `dispatch://<KEY>/ask/<id>` for an answer or a resolution, `dispatch://<KEY>/comment/<id>`
	// for a reply.
	Ref string `json:"ref,omitempty"`
	// Decision is what a parked-decision notice relays, and Root the root of the tree it was made
	// on. Every other kind carries neither.
	Decision DecisionKind `json:"decision,omitempty"`
	Root     string       `json:"root,omitempty"`
	// Update is an update-stalled notice's account of the round that is not finishing, and is
	// absent from every other kind. Only an UpdateStalledNotice carries one.
	Update *UpdateStalled `json:"update,omitempty"`
}

// UpdateStalled is what an update-stalled controller notice carries: the update round the daemon
// opened over an issue's pod, and why it has not replaced that pod. Round is the round's identity
// (supervise.Claim.UpdateRound), Pod the pod it was opened over, and Reasons the drift the round
// was opened for, as the task states it. Waiting is every claim the round is blocked on, so the
// controller can retry one claim or ask the operator to act on a failed or suspended one rather
// than read the whole claims list to find out which worker the round waits on; what each entry
// says, and what the controller does with it, is UpdateWaiting's. The notice's issue is the
// outbox row's own.
//
// Reasons is one string, never a list: a single reason carries ", " inside it already — a moved
// address reads `<where> <held>, now <handed>`, a pod-shape drift `image <was>, now <now>`, and a
// plugin round's reason names both contracts — so a list built by splitting what the claim
// records (supervise.Claim.UpdateReasons, one string) would cut each reason into pieces that read
// as reasons of their own. It is the drift exactly as the round's task states it to the worker.
type UpdateStalled struct {
	Round   string          `json:"round"`
	Pod     string          `json:"pod,omitempty"`
	Reasons string          `json:"reasons"`
	Waiting []UpdateWaiting `json:"waiting"`
}

// UpdateWaiting is one claim an update round is blocked on: its token, the role it holds, the
// state it is in (supervise.ClaimState — an operator-suspended claim waits for the operator; a
// pod round lists no `failed` claim, since it does not wait for one), and Answered for a claim
// that has already answered this round.
//
// The contract the claim's plugin records is not here. A plugin with no ready_for_update op
// answers by ending the turn its update task started (supervise's askForUpdate and turnEnded),
// so such a claim is one the round is waiting on like any other, and the controller has nothing
// of its own to do about it.
//
// Answered is what tells the controller which claims are its to prompt. Its playbook publishes
// the update task to a claim that has not answered, and that publish starts a turn, which takes
// the claim's answer back (supervise's takeBackSignal): told to do that for a claim that has
// answered, it would undo the answers of exactly the round the daemon is stuck carrying
// (skill://legion-controller/references/escalation.md).
type UpdateWaiting struct {
	Claim    string     `json:"claim"`
	Role     claim.Role `json:"role,omitempty"`
	State    string     `json:"state"`
	Answered bool       `json:"answered,omitempty"`
}

// MarshalJSON keeps `waiting` an array on the wire, empty rather than `null`: the plugin's strict
// reader refuses a null.
func (u UpdateStalled) MarshalJSON() ([]byte, error) {
	type wire UpdateStalled
	out := wire(u)
	if out.Waiting == nil {
		out.Waiting = []UpdateWaiting{}
	}
	return json.Marshal(out)
}

// BlockedIssue is one issue Legion is waiting to start and the issues it waits on that are not
// done, in the order Dispatch lists them (blockers.Unfinished): a root admission skips, or a
// child the workflow holds at `admitted`. It lives here, below both readers, so the tick notice
// above and the state's `blocked` (api.BlockedIssue, an alias of this type) carry one shape
// rather than two that could drift.
type BlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy"`
}

// MarshalJSON keeps `blockedBy` an array on the wire, empty rather than `null`: the plugin's
// strict reader refuses a null.
func (b BlockedIssue) MarshalJSON() ([]byte, error) {
	type wire BlockedIssue
	out := wire(b)
	if out.BlockedBy == nil {
		out.BlockedBy = []string{}
	}
	return json.Marshal(out)
}

// CatchUp is what a tree's root architect is told of its tree when its claim is ready at a launch
// (a catch-up notice), which is the architect's first instruction: admission launches it with no
// task, or only with the decisions kept for it. It is the tree as the daemon records it then: the
// root's generation, the launch of the architect's claim it was written for, whether a hand-over
// created the root at this generation with its child's phase and pull request (HandedOver) and
// whether its old root's gate came with them (Carried), the design gate policy and the root's
// gate, and every issue of the tree. A handed-over root's workers already run their phases, so its
// architect decomposes nothing anew; one that carried no gate registers its own spec. Decided names
// what a person decided on the tree while nobody in it could hear it and the start did not name (a
// decision made while the architect's claim had ended, before this relaunch), in the words of
// workflow.Engine.ParkedDecisionTask; empty when there is none. Kept is what Decided named, which
// a published catch-up forgets (Store.ForgetKeptDecisions): a catch-up a newer one supersedes
// hands it over to a fresh catch-up instead (workflow.HandOverKept), and one that finishes
// undelivered any other way leaves it all for the architect's next ready to name again.
type CatchUp struct {
	Generation uint64         `json:"generation"`
	Launch     uint64         `json:"launch"`
	HandedOver bool           `json:"handed_over,omitempty"`
	Carried    bool           `json:"carried,omitempty"`
	Gate       CatchUpGate    `json:"gate"`
	Issues     []CatchUpIssue `json:"issues"`
	Decided    string         `json:"decided,omitempty"`
	Kept       *KeptDecisions `json:"kept,omitempty"`
}

// CatchUpGate is the design gate as a catch-up states it: the project's policy (gates.design,
// root-issues or off) and, once the architect has registered one, the root's registered document,
// its latest version, and whether that version is approved.
type CatchUpGate struct {
	Policy   string `json:"policy"`
	Artifact string `json:"artifact,omitempty"`
	Version  int    `json:"version,omitempty"`
	Open     bool   `json:"open"`
}

// CatchUpIssue is one issue of the tree as a catch-up lists it.
type CatchUpIssue struct {
	Key    string      `json:"key"`
	Parent string      `json:"parent,omitempty"`
	Title  string      `json:"title"`
	Phase  phase.Phase `json:"phase"`
	Status string      `json:"status"`
}

func (Notice) OutboxKind() OutboxKind { return OutboxKindNotice }

// Published is the notice the runner publishes for outbox row id, and the dedupe key it goes under:
// a re-held copy goes under the key of the row it copies, and without ResendOf.
func (n Notice) Published(id int64) (Notice, string) {
	if n.ResendOf == 0 {
		return n, OutboxKey(id)
	}
	copied := n.ResendOf
	n.ResendOf = 0
	return n, OutboxKey(copied)
}

// ControllerNotice is a Notice for the project's controller topic (notify.ControllerTopic) alone,
// in an outbox row of its own, so its publish retries apart from the architect's notice row. It is
// published as the Notice it is.
type ControllerNotice Notice

func (ControllerNotice) OutboxKind() OutboxKind { return OutboxKindControllerNotice }

// The controller notices no architect is told of. A triage notice's root is unrecorded, so no
// architect owns it; a slot-free notice names the root whose slot admission released while no
// waiting root took it. A todo notice names an issue not handed to Legion that changed in `todo`
// while a slot stood free. An awaiting-person notice names a root that released its slot while its
// architect waits on a person (workflow's enterAwaiting). The controller takes no issue for any of
// the three (skill://legion-controller, "Free slots"). A tick is the daemon's periodic wake, whose
// issue is the project key. An unslotted notice names a root admission found in the workflow with
// neither a slot nor a place in the waiting line, which it took out of the workflow; its reason
// names the status the root stood at. An update-stalled notice names an issue whose pod the
// daemon opened an update round over and cannot replace: a worker has not answered within
// `pod_update_notice_hours`, a turn that will not end holds the replacement, or the replacement
// itself keeps failing (the daemon's reconcilePodUpdates). When admission sends each is its
// rule (admit.Admission.wakeController and its callers, admit's releaseUnslotted).
//
// A parked-decision notice names a person's decision on a tree Legion is not running, which
// nobody inside the tree will hear: a spec decision the notice executor could not deliver to the
// tree's architect (it stopped with its finished tree, or its claim ended), or a person's answer,
// resolution or reply (one that leaves the turn with the agent, or one under a closed ask) on an ask
// of an issue whose tree lingers or has closed. Its issue is where the decision was made, Root the
// tree's root, Decision what the person did, Ref where in Dispatch, Actor who; a spec decision
// carries its Version, a changes request its Reason. The controller decides whether the tree runs
// again (skill://legion-controller/references/parked-decisions.md).
const (
	TriageNotice         NoticeKind = "triage"
	SlotFreeNotice       NoticeKind = "slot-free"
	TodoNotice           NoticeKind = "todo"
	TickNotice           NoticeKind = "tick"
	UnslottedNotice      NoticeKind = "unslotted"
	AwaitingPersonNotice NoticeKind = "awaiting-person"
	ParkedDecisionNotice NoticeKind = "parked-decision"
	UpdateStalledNotice  NoticeKind = "update-stalled"
)

// SuperviseOp identifies a worker-session operation: "start" starts, resumes, or retries the role's
// claim; "suspend" stops its process and keeps its session; "tree_close" is the tree's close —
// the request that ends the claim, the tree's root claim included; "issue_close" is a child issue's
// close as done, which ends the claim as a tree close does and drops its session, since the issue
// is finished and its next run, should a person set it todo again, starts fresh. There
// is no plain stop: only a close ends a claim from the workflow, so the op states which.
type SuperviseOp string

// SuperviseRequest describes the session operation the outbox runner performs.
type SuperviseRequest struct {
	Op   SuperviseOp `json:"op"`
	Tree string      `json:"tree"`
	Role claim.Role  `json:"role"`
	Task string      `json:"task,omitempty"`
	// Kept is what an architect start's Task named of the decisions its root keeps
	// (workflow.Engine.ParkedDecisionTask). The start settles them once it has run (the outbox's
	// settleKept): a task the root architect's claim queued forgets them
	// (Store.ForgetKeptDecisions), and a start that reached no claim hands them over to a fresh
	// catch-up (workflow.HandOverKept). Every other request carries none.
	Kept *KeptDecisions `json:"kept,omitempty"`
	// Generation is the issue generation the request serves. A request of an earlier generation
	// finishes without acting: it must never suspend, stop, or start the next generation's worker.
	Generation uint64 `json:"generation"`
	// Phase is the phase a phase worker's start serves; a start for a phase the issue has left
	// finishes without acting. It is empty for the architect, which serves every phase, and for a
	// suspend or a tree's close, which must still act after the issue moves on.
	Phase phase.Phase `json:"phase,omitempty"`
	// Assignment is the task the issue's current phase hands the role, as the workflow minted it
	// for this start (workflow.Engine.start): the same token stands on the role's phases row,
	// written in the transition's transaction. It travels to the claim's delivery and, once a
	// confirmed delivery is retired, onto the claim, so a completion can be held to the assignment
	// the turn reporting it was given (api/handoff.go, HANDOFF_STALE_ASSIGNMENT) rather than to the
	// run alone, whose value every phase entry of one run shares. Empty for a start that hands no
	// new task — a resume, a production check's look — and for every other operation.
	Assignment string `json:"assignment,omitempty"`
	// ResumeTask marks Task as the phase's resume task (workflow.ResumePhaseTask), the one a
	// re-admitted tree's promotion gives a mid-phase child: a claim that already holds a task for
	// the same generation and phase is given it once it is ready, so the start delivers none.
	ResumeTask bool `json:"resumeTask,omitempty"`
	// RuleResume marks the boot's resume-by-rule start. If its resumed claim spends its launch
	// budget, the runner finishes this row rather than retrying it as RequestRetry with fresh
	// budgets: the failed claim stays failed for the boot report to name.
	RuleResume bool `json:"ruleResume,omitempty"`
	// FreshSession marks a start that must not resume the session its claim recorded: the
	// operator's `legion claims retry --fresh-session` (api's retryClaim). The runner passes it
	// to the claim's retry, which forgets the session inside that one decision (supervise's
	// RequestRetry), so a start that never ran leaves the session where it was.
	FreshSession bool `json:"freshSession,omitempty"`
	// Linger is the root generation whose linger a tree close expires. A member keeps its
	// generation across re-admission, so the close acts only while its tree lingers at that root
	// generation: never in the tree's next run, nor in a later linger of it.
	Linger uint64 `json:"linger,omitempty"`
	// Reason is why a suspend stops the claim, for the line the suspended claim logs.
	Reason string `json:"reason,omitempty"`
	// CheckLook is the look of the issue's production check a start starts, 0 for a phase start.
	// A check start runs after the close, where the issue has no phase and its tree may linger, so
	// the outbox fences it on the open check and its look (the row acts only while the check is
	// open at that look with the look open) instead of on the generation and the lingering tree.
	CheckLook int `json:"checkLook,omitempty"`
	// CheckOpenedAt is the check instance the look belongs to (ProductionCheck.OpenedAt, its ID):
	// a later merge of the same issue replaces the check and counts its looks from one again, so a
	// start row of the replaced check's look 1 would otherwise act on the new check's. The outbox
	// finishes a start without acting when the issue's check is not the one it names; zero names
	// no instance (a row queued before the field) and is not fenced.
	CheckOpenedAt time.Time `json:"checkOpenedAt,omitzero"`
}

func (SuperviseRequest) OutboxKind() OutboxKind { return OutboxKindSupervise }

// GateSeed records the design document version that starts a gate.
type GateSeed struct {
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
	// Generation is the issue generation that registered the gate; the seed's fact is that
	// generation's.
	Generation uint64 `json:"generation"`
}

func (GateSeed) OutboxKind() OutboxKind { return OutboxKindGateSeed }

// LingerClose records the issue generation whose linger should close.
type LingerClose struct {
	Generation uint64 `json:"generation"`
}

func (LingerClose) OutboxKind() OutboxKind { return OutboxKindLingerClose }

// IssueSuspend stops an issue's pod after its close has stopped every role, and deletes the
// Sandbox the roles ran in (runtime.IssueSuspender). Both workflow generations and this row's ID
// fence a later re-admission or start. The issue keeps nothing: its disk is its pod's own and
// goes with the pod, so a later todo re-enters the issue fresh on a Sandbox its next launch
// makes.
type IssueSuspend struct {
	Tree           string `json:"tree"`
	Generation     uint64 `json:"generation"`
	TreeGeneration uint64 `json:"treeGeneration"`
}

func (IssueSuspend) OutboxKind() OutboxKind { return OutboxKindIssueSuspend }

// WorkspaceRemove removes the workspace named by the row's issue.
type WorkspaceRemove struct {
	// Linger is the root generation whose linger the removal expires. Like a tree close, it acts
	// only while the tree lingers at that root generation: once re-admission moves the root on, the
	// workspace there is the tree's next run's, whatever generation the issue itself keeps.
	Linger uint64 `json:"linger"`
	// Tree is the tree whose linger queued the removal. An issue adopted into another tree since
	// (workflow.Engine.Adopt) works there now, so the removal finishes without acting. A row queued
	// by an earlier release has none, and is fenced by Linger against the issue's current tree.
	Tree string `json:"tree,omitempty"`
}

func (WorkspaceRemove) OutboxKind() OutboxKind { return OutboxKindWorkspaceRemove }

// RoleMessage is one message the daemon sends a role holder, published to that role's topic: Role
// is the claim token, which the
// runner addresses as a role topic, and Body is the whole of what the holder is told. It is how
// the daemon reaches a worker with something to read rather than a task to do - a settled head's
// check results, which move nothing (workflow's tellChecks, dispatch://INFRA-714/spec R1). A role
// with no live holder is told nothing: the row finishes, since the message is of a moment that
// has passed by the time anyone claims the role, and every recipient can read the pull request's
// checks on GitHub for itself.
type RoleMessage struct {
	Role string `json:"role"`
	Body string `json:"body"`
}

func (RoleMessage) OutboxKind() OutboxKind { return OutboxKindRoleMessage }

// IssueBranch creates the row's issue's branch, legion/<KEY>, on GitHub at the repository's default
// branch, where GitHub does not have it yet, before any role of the issue starts: GitHub can refuse
// the push that creates a branch of a large repository, where it takes a push that moves one. A
// start of the issue waits while the row is unfinished (outboxInOrder).
type IssueBranch struct {
	// Generation is the issue generation the branch is created for. A row of an earlier generation,
	// or of a tree that lingers, finishes without acting.
	Generation uint64 `json:"generation"`
}

func (IssueBranch) OutboxKind() OutboxKind { return OutboxKindIssueBranch }

// CheckCompare wakes a production check from a build or from a deploy run, whichever of Build and
// Deploy it carries (exactly one). Build is the commit a daemon boot reported itself built from,
// or Dispatch announced every server process serves (server.started): the executor runs a GitHub
// compare of the check's merge commit with it, as the implement App, and starts the look Look when
// the build contains the merge. Deploy is a completed
// run attempt of a deploy workflow the project names (`projects.<KEY>.deploys`): the executor reads
// the run and its jobs from GitHub, and wakes the check once for each job whose named step succeeded
// after the pull request merged. Cause names what asked for it (`daemon boot`, `dispatch start`,
// `dispatch build` or `deploy run`), for the look's wake. OpenedAt is the check instance the
// compare was queued for (ProductionCheck.OpenedAt): the executor compares nothing once a later
// merge replaced it; zero names no instance and is not fenced. A row queued before Deploy existed
// carries a build and no deploy, and decodes as it did.
type CheckCompare struct {
	Look     int        `json:"look"`
	Cause    string     `json:"cause"`
	Build    string     `json:"build,omitempty"`
	Deploy   *DeployRun `json:"deploy,omitempty"`
	OpenedAt time.Time  `json:"openedAt,omitzero"`
}

// DeployRun is the deploy workflow run a CheckCompare reads: attempt Attempt of GitHub Actions run
// Run, of the configured Workflow (its `.github/workflows/` path) and Step (the step whose success
// is a deploy), on Branch, the issue's recorded default branch. The executor holds the run GitHub
// returns to each of them, since anyone holding an Envoy bearer can publish the envelope the row
// was queued from.
type DeployRun struct {
	Run      int64  `json:"run"`
	Attempt  int    `json:"attempt"`
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Branch   string `json:"branch"`
}

func (CheckCompare) OutboxKind() OutboxKind { return OutboxKindCheckCompare }

// CheckLook is a look's bound: due at the look's wake plus the look bound, it ends look Look of
// the issue's production check as a pending report would, when the look is still open then.
// OpenedAt is the check instance the look is of (ProductionCheck.OpenedAt): a later merge of the
// same issue replaces the check and counts its looks from one again, and a bound row of the
// replaced check's look must not end the new check's; zero names no instance and is not fenced.
type CheckLook struct {
	Look     int       `json:"look"`
	OpenedAt time.Time `json:"openedAt,omitzero"`
}

func (CheckLook) OutboxKind() OutboxKind { return OutboxKindCheckLook }

// CheckBound is the whole check's bound, due at its bound_at: it starts the check's last look, or
// defers while a look is open.
type CheckBound struct{}

func (CheckBound) OutboxKind() OutboxKind { return OutboxKindCheckBound }

// PhaseEnded tells the role whose phase a transition the daemon raised on its own ended that its
// phase is over, on that role's own Envoy topic. It is the one message the daemon addresses to a
// role that is not an architect of its own accord: when the daemon moves an issue out of a phase
// on something it read of the pull request — a head that conflicts with its base, the review
// GitHub delivered, the merge — the next role starts at once, and without that message nothing
// tells the tester, the reviewer, the merger or the implementer in retro that the phase it was
// working is no longer its own, since the daemon's next task for a role waits for that role's turn
// to end. Role is the recipient, From the phase that ended, To the phase the issue is in now,
// Reason what the architect's notice carries, and TaskFollows that the recipient is also the role
// the new phase starts — the implementer sent back from retro — whose task for that phase follows
// this message. It stops nothing: the recipient keeps its turn, its jobs, its process and its
// session.
type PhaseEnded struct {
	Role        claim.Role  `json:"role"`
	From        phase.Phase `json:"from"`
	To          phase.Phase `json:"to"`
	Reason      string      `json:"reason"`
	TaskFollows bool        `json:"taskFollows,omitempty"`
}

func (PhaseEnded) OutboxKind() OutboxKind { return OutboxKindPhaseEnded }

// Message is what the runner publishes for issue: one sentence that the recipient's phase ended
// and which phase the issue is in now, the implementer's own sentence where its task follows, the
// two that keep the role off the working copy the new phase owns without stopping it (the worker
// prompt says the same, internal/prompts/go/worker-common.md), and the reason last, behind the
// `Reason:` label a started worker's task carries it under (workflow's task), so the GitHub names
// in it are marked off from the daemon's own sentences in both places.
func (p PhaseEnded) Message(issue string) string {
	follows := ""
	if p.TaskFollows {
		follows = fmt.Sprintf(" Your %s task follows.", p.To)
	}
	return fmt.Sprintf("%s: the daemon ended your %s phase, and the issue is in %s now.%s"+
		" Nothing of yours is stopped: your turn, your jobs, your process and your session run on."+
		" The workspace is the %s phase's now, so stop writing it at once and leave whatever you have uncommitted."+
		" Reason: %s.",
		issue, p.From, p.To, follows, p.To, p.Reason)
}

// NewOutboxRow encodes one validated payload for durable delivery at nextAt.
func NewOutboxRow(issue string, payload OutboxPayload, nextAt time.Time) (OutboxRow, error) {
	if err := validateOutboxPayload(payload); err != nil {
		return OutboxRow{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return OutboxRow{}, fmt.Errorf("encode outbox payload: %w", err)
	}
	return OutboxRow{Kind: payload.OutboxKind(), Issue: issue, Payload: encoded, NextAt: nextAt}, nil
}

// DecodeOutboxPayload strictly decodes a row's typed payload.
func DecodeOutboxPayload(row OutboxRow) (OutboxPayload, error) {
	payload, err := decodeOutboxJSON(row)
	if err != nil {
		return nil, err
	}
	if err := validateOutboxPayload(payload); err != nil {
		return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	if payload.OutboxKind() != row.Kind {
		return nil, fmt.Errorf("decode outbox row %d: kind %q does not match payload kind %q", row.ID, row.Kind, payload.OutboxKind())
	}
	return payload, nil
}

func decodeOutboxJSON(row OutboxRow) (OutboxPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(row.Payload))
	decoder.DisallowUnknownFields()
	var payload OutboxPayload
	switch row.Kind {
	case OutboxKindDispatchStatus:
		value := StatusWrite{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindDispatchMessage:
		value := MessagePost{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindNotice:
		value := Notice{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindControllerNotice:
		value := ControllerNotice{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindSupervise:
		value := SuperviseRequest{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindGateSeed:
		value := GateSeed{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindLingerClose:
		value := LingerClose{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueSuspend:
		value := IssueSuspend{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindWorkspaceRemove:
		value := WorkspaceRemove{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindRoleMessage:
		value := RoleMessage{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueBranch:
		value := IssueBranch{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckCompare:
		value := CheckCompare{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckLook:
		value := CheckLook{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckBound:
		value := CheckBound{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindPhaseEnded:
		value := PhaseEnded{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	default:
		return nil, fmt.Errorf("decode outbox row %d: unknown kind %q", row.ID, row.Kind)
	}
	if row.Kind == OutboxKindDispatchMessage {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row.Payload, &fields); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		if _, ok := fields["body"]; !ok {
			return nil, fmt.Errorf("decode outbox row %d: payload has no body", row.ID)
		}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode outbox row %d: payload has trailing JSON", row.ID)
		}
		return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	return payload, nil
}

func validateOutboxPayload(payload OutboxPayload) error {
	switch value := payload.(type) {
	case StatusWrite:
		// A row whose kind is not one of StatusRowKinds can never be queued, and one stored by an
		// older release is given its kind by migration 20261102003600 before this release reads
		// it: so a kind the outbox has no rule for is a kind somebody added to StatusRowKinds
		// and nowhere else, which fails here rather than reaching the outbox to be judged by
		// another kind's rule.
		if !slices.Contains(StatusRowKinds, value.Kind) {
			return fmt.Errorf("outbox status write has unknown kind %q", value.Kind)
		}
		if value.Status == "" {
			return fmt.Errorf("outbox status write has empty status")
		}
		if value.Reason != "" && value.Status != "done" {
			return fmt.Errorf("a status write to %s carries a reason; only a done write does", value.Status)
		}
		if length := dispatch.MessageBodyLength(value.Reason); length > MessagePostLimit {
			return fmt.Errorf("a status write's reason is %d characters, over the %d a message holds", length, MessagePostLimit)
		}
	case MessagePost, GateSeed, LingerClose, IssueBranch, CheckBound:
	case IssueSuspend:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 {
			return fmt.Errorf("issue suspension requires its tree and both workflow generations")
		}
	case WorkspaceRemove:
		if value.Linger == 0 {
			return fmt.Errorf("workspace removal requires the root generation of its linger")
		}
	case Notice:
		if !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown notice kind %q", value.Kind)
		}
		if (value.Kind == "catch-up") != (value.CatchUp != nil) {
			return fmt.Errorf("a %s notice carries a catch-up only when it is one", value.Kind)
		}
		if _, spec := SpecDecision(value.Kind); !spec && (value.Actor != "" || value.Ref != "") {
			return fmt.Errorf("a %s notice carries an actor or a ref; only a spec decision's does", value.Kind)
		}
		if value.Decision != "" || value.Root != "" {
			return fmt.Errorf("a %s notice carries a decision or a root; only a parked-decision controller notice does", value.Kind)
		}
		if catchUp := value.CatchUp; catchUp != nil && catchUp.Kept != nil {
			if err := catchUp.Kept.validate(); err != nil {
				return err
			}
		}
		if value.Update != nil {
			return fmt.Errorf("a %s notice carries an update round; only the controller's update-stalled notice does", value.Kind)
		}
	case ControllerNotice:
		if !controllerOnlyNoticeKind(value.Kind) && !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown controller notice kind %q", value.Kind)
		}
		if value.Kind == ParkedDecisionNotice {
			if !validDecisionKind(value.Decision) || value.Root == "" || value.Ref == "" || !PersonActor(value.Actor) {
				return fmt.Errorf("a parked-decision notice needs a known decision, its tree's root, the decision's Dispatch reference and the person who made it")
			}
		} else if value.Decision != "" || value.Actor != "" || value.Ref != "" || value.Root != "" {
			return fmt.Errorf("a %s controller notice carries a decision, an actor, a ref or a root; only a parked-decision does", value.Kind)
		}
		if (value.Kind == UpdateStalledNotice) != (value.Update != nil) {
			return fmt.Errorf("a %s notice carries an update round only when it is one", value.Kind)
		}
		if update := value.Update; update != nil && (update.Round == "" || len(update.Waiting) == 0) {
			return fmt.Errorf("an update-stalled notice requires its round and the claims it waits on")
		}
	case SuperviseRequest:
		if !validSuperviseOp(value.Op) {
			return fmt.Errorf("unknown supervise operation %q", value.Op)
		}
		if value.Op == "start" && value.Tree == "" {
			return fmt.Errorf("supervise start requires tree")
		}
		if value.Op == "start" && value.Role == "" {
			return fmt.Errorf("supervise start requires role")
		}
		if (value.Op == "tree_close") != (value.Linger != 0) {
			return fmt.Errorf("a tree close, and only a tree close, names the root generation of its linger")
		}
		if value.ResumeTask && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise resume task requires a start with a task and a phase")
		}
		if value.CheckLook != 0 && (value.CheckLook < 0 || value.Op != "start" || value.Role != claim.RoleImplementer || value.Phase != "") {
			return fmt.Errorf("a supervise check look names a start of the implementer with no phase, and only a start carries one")
		}
		if value.Kept != nil {
			if value.Op != "start" || value.Task == "" {
				return fmt.Errorf("only a supervise start with a task names the decisions it kept")
			}
			if err := value.Kept.validate(); err != nil {
				return err
			}
		}
	case RoleMessage:
		if value.Role == "" {
			return fmt.Errorf("role message requires role")
		}
		if value.Body == "" {
			return fmt.Errorf("role message requires body")
		}
	case CheckCompare:
		if value.Look <= 0 {
			return fmt.Errorf("check compare requires the look it starts")
		}
		if (value.Build == "" && value.Deploy == nil) || value.Cause == "" {
			return fmt.Errorf("check compare requires the build commit and what asked for it, or a deploy run in place of the build commit")
		}
		if value.Build != "" && value.Deploy != nil {
			return fmt.Errorf("check compare carries both a build commit and a deploy run; it compares exactly one")
		}
		if deploy := value.Deploy; deploy != nil && (deploy.Run <= 0 || deploy.Attempt <= 0 || deploy.Workflow == "" || deploy.Step == "" || deploy.Branch == "") {
			return fmt.Errorf("check compare's deploy run requires a positive run and attempt and its workflow, step and branch")
		}
	case CheckLook:
		if value.Look <= 0 {
			return fmt.Errorf("check look bound requires the look it bounds")
		}
	case PhaseEnded:
		if !claim.IsRole(value.Role) || value.Role == claim.RoleArchitect {
			return fmt.Errorf("a phase-ended message goes to the role whose phase ended, never to an architect, which its checks-red notice tells; %q is neither", value.Role)
		}
		if value.From == "" || value.To == "" || value.From == value.To {
			return fmt.Errorf("a phase-ended message names the phase that ended and the different phase the issue is in now; it names %q and %q", value.From, value.To)
		}
		if value.Reason == "" {
			return fmt.Errorf("a phase-ended message carries the reason the architect's notice carries")
		}
		if value.TaskFollows && value.Role != claim.RoleImplementer {
			return fmt.Errorf("only the implementer is told its task follows, since the phase the red opens is its own; the %s is not", value.Role)
		}
	default:
		return fmt.Errorf("unknown outbox payload %T", payload)
	}
	return nil
}

// validNoticeKind is every notice kind the daemon publishes, which is also every kind a row may
// decode as (DecodeOutboxPayload). `ready-refused` is not one of them: READY went with the
// daemon's merge, so nothing writes the kind, and migration 20261029120800 deletes every row an older
// daemon left carrying it, in the same migration that drops the four merge kinds from this list.
func validNoticeKind(kind NoticeKind) bool {
	switch kind {
	case "phase-finished", "worker-died", "held", "pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "design-approved", "design-changes-requested", "carried-gate-opened", "child-closed", "child-status", "child-release-stalled", "child-not-recorded", "catch-up", "checks-red", "review-stuck", "status-reasserted", "session-lost", "session-store-fault":
		return true
	default:
		return false
	}
}

// controllerOnlyNoticeKind says whether kind is a notice for the controller alone, never an
// architect's.
func controllerOnlyNoticeKind(kind NoticeKind) bool {
	switch kind {
	case TriageNotice, SlotFreeNotice, TodoNotice, TickNotice, UnslottedNotice, AwaitingPersonNotice, ParkedDecisionNotice, UpdateStalledNotice:
		return true
	default:
		return false
	}
}

func validSuperviseOp(op SuperviseOp) bool {
	switch op {
	case "start", "suspend", "tree_close", "issue_close":
		return true
	default:
		return false
	}
}
