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
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// OutboxKind names one durable side effect.
type OutboxKind string

const (
	OutboxKindDispatchStatus    OutboxKind = "dispatch_status"
	OutboxKindDispatchMessage   OutboxKind = "dispatch_message"
	OutboxKindNotice            OutboxKind = "notice"
	OutboxKindControllerNotice  OutboxKind = "controller_notice"
	OutboxKindSupervise         OutboxKind = "supervise"
	OutboxKindGateSeed          OutboxKind = "gate_seed"
	OutboxKindLingerClose       OutboxKind = "linger_close"
	OutboxKindIssueSuspend      OutboxKind = "issue_suspend"
	OutboxKindWorkspaceRemove   OutboxKind = "workspace_remove"
	OutboxKindMergeQueuePublish OutboxKind = "merge_queue_publish"
	OutboxKindIssueBranch       OutboxKind = "issue_branch"
	OutboxKindAutoMergeDisable  OutboxKind = "auto_merge_disable"
	OutboxKindReadyMerge        OutboxKind = "ready_merge"
	OutboxKindCheckCompare      OutboxKind = "check_compare"
	OutboxKindCheckLook         OutboxKind = "check_look"
	OutboxKindCheckBound        OutboxKind = "check_bound"
)

// OutboxLane is which executor of an issue runs a kind. An issue's rows run on two lanes side by
// side. The lifecycle lane runs the rows that start, stop and tidy the issue's roles and their
// resources, one at a time in the order they were queued: each assumes the state its predecessor
// leaves (a start after the branch it waits for, a workspace removal after the suspend that
// stopped the roles). It also carries the issue's pull-request mutations, the disarm
// (auto_merge_disable) and the READY merge's submission (ready_merge), which must never run beside
// each other: a disarm and a submit of one pull request at once would race on GitHub, and the lane
// (with outboxLaneFree across daemons) is what keeps them apart; and the production check's rows
// (check_compare, check_look, check_bound), which start and end the implementer's looks in order
// with the starts and suspends they queue. The board lane runs the issue's Dispatch writes and
// notice publishes beside them, so a status write or a notice never waits behind a start pulling
// an image, and a slow publish never holds a stop back.
type OutboxLane string

const (
	LaneLifecycle OutboxLane = "lifecycle"
	LaneBoard     OutboxLane = "board"
)

// laneKinds is each lane's kinds in the order LaneKinds lists them. Every OutboxKind is in exactly
// one lane (TestEveryOutboxKindIsInExactlyOneLane).
var laneKinds = map[OutboxLane][]OutboxKind{
	LaneLifecycle: {OutboxKindSupervise, OutboxKindIssueSuspend, OutboxKindLingerClose, OutboxKindWorkspaceRemove, OutboxKindGateSeed, OutboxKindIssueBranch, OutboxKindAutoMergeDisable, OutboxKindReadyMerge, OutboxKindCheckCompare, OutboxKindCheckLook, OutboxKindCheckBound},
	LaneBoard:     {OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindMergeQueuePublish},
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

// StatusWrite is a Dispatch status the daemon writes. ObservedStatus is the status Dispatch shows
// until the write runs: the runner writes Status only while Dispatch still shows ObservedStatus, so a
// move someone else made since the write was queued stands.
type StatusWrite struct {
	Status         string `json:"status"`
	ObservedStatus string `json:"observedStatus"`
	// Reason is a done write's reason: the runner posts it as a message on the issue before it
	// writes the status, since Dispatch refuses a message on a closed issue. Empty for a write
	// with no reason to record.
	Reason string `json:"reason,omitempty"`
	// HandOver also puts dispatch.LegionLabel on the issue, in the same Dispatch write: the write
	// hands the issue to Legion, and the runner writes it while the label is missing even where
	// Dispatch already shows Status. HandOverTree and HandOverLinger fence it: the runner writes it
	// only while the issue is still that tree's and the tree's root still lingers at that
	// generation (Issue.LingersAt), so a root re-admitted since keeps the issue as its child.
	HandOver       bool   `json:"handOver,omitempty"`
	HandOverTree   string `json:"handOverTree,omitempty"`
	HandOverLinger uint64 `json:"handOverLinger,omitempty"`
}

func (StatusWrite) OutboxKind() OutboxKind { return OutboxKindDispatchStatus }

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
	// dedupe key (OutboxKey), so a session that already has the notice recognises the copy. The
	// row is the daemon's own bookkeeping: the executor publishes the notice without it.
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
	// Question is an agent-question notice's account of the question the answer loop passed on,
	// and absent from every other kind; the notice's Role is the agent's and its Reason why the
	// question was passed on.
	Question *AgentQuestion `json:"question,omitempty"`
	// AgentDecision is an agent-decision notice's account of the person's decision the answer loop
	// passed on, and absent from every other kind; the notice's Role is the agent's.
	AgentDecision *AgentDecision `json:"agent_decision,omitempty"`
}

// AgentQuestion is a question to a stopped agent that the daemon's answer loop passed to the
// controller (an agent-question notice): the question's id (answer_questions), the agent's issue
// and session, who asked (`user:<login>`, or `session:<id>` with the asking session's title when
// the listener stamped one), where it was asked (a dispatch:// reference, or `envoy:<event id>`),
// and its text, cut at AgentQuestionTextLimit characters with a note saying so.
type AgentQuestion struct {
	ID      string `json:"id"`
	Issue   string `json:"issue"`
	Session string `json:"session"`
	Asker   string `json:"asker"`
	Ref     string `json:"ref"`
	Text    string `json:"text"`
}

// AgentDecision is a person's decision on an ask a stopped agent opened, which the daemon's answer
// loop passed on (an agent-decision notice): the decision's id (`dispatch:<event id>` of the ask's
// answer, or `dispatch:<comment id>` of a reply that hands the ask back to the agent), the agent's
// issue and session, the person who decided (`user:<login>`), where (`dispatch://<KEY>/ask/<id>`
// or `dispatch://<KEY>/comment/<id>`), and what they decided (the options they selected and their
// text), cut as AgentDecisionText cuts it.
type AgentDecision struct {
	ID      string `json:"id"`
	Issue   string `json:"issue"`
	Session string `json:"session"`
	Asker   string `json:"asker"`
	Ref     string `json:"ref"`
	Text    string `json:"text"`
}

// AgentQuestionTextLimit is the most characters of a question's or a decision's text an
// agent-question or agent-decision notice carries; the rest is left out, and a note says how long
// the text was and where to read it.
const AgentQuestionTextLimit = 2000

// AgentQuestionText is text as an agent-question notice carries it: whole when it is at most
// AgentQuestionTextLimit characters, otherwise its first AgentQuestionTextLimit characters and a
// note naming its length and ref, where the whole question is.
func AgentQuestionText(text, ref string) string { return cutNoticeText(text, ref, "question") }

// AgentDecisionText is text as an agent-decision notice carries it, cut as AgentQuestionText cuts a
// question's.
func AgentDecisionText(text, ref string) string { return cutNoticeText(text, ref, "decision") }

func cutNoticeText(text, ref, what string) string {
	runes := 0
	for i := range text {
		if runes == AgentQuestionTextLimit {
			return text[:i] + fmt.Sprintf("\n\n[cut: the %s is %d characters; the first %d are shown, the whole %s is at %s]",
				what, utf8.RuneCountInString(text), AgentQuestionTextLimit, what, ref)
		}
		runes++
	}
	return text
}

// CatchUp is what a tree's root architect is told of its tree when its claim is ready at a launch
// (a catch-up notice), which is the architect's first instruction: admission launches it with no
// task. It is the tree as the daemon records it then: the root's generation, the launch of the
// architect's claim it was written for, whether a hand-over created the root at this generation
// with its child's phase and pull request (HandedOver) and whether its old root's gate came with
// them (Carried), the design gate policy and the root's gate, and every issue of the tree. A
// handed-over root's workers already run their phases, so its architect decomposes nothing anew;
// one that carried no gate registers its own spec.
type CatchUp struct {
	Generation uint64         `json:"generation"`
	Launch     uint64         `json:"launch"`
	HandedOver bool           `json:"handed_over,omitempty"`
	Carried    bool           `json:"carried,omitempty"`
	Gate       CatchUpGate    `json:"gate"`
	Issues     []CatchUpIssue `json:"issues"`
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
// while a slot stood free; the controller takes no issue for either (skill://legion-controller,
// "Free slots"). A tick is the daemon's periodic wake, whose issue is the project key. An
// unslotted notice names a root admission found in the workflow with neither a slot nor a place in
// the waiting line, which it took out of the workflow; its reason names the status the root stood
// at. An agent-question notice names a question to a stopped agent that the daemon's answer loop
// passed on, with its account (Notice.Question), the agent's role and why (Reason), once per
// question (the daemon's PassOn). When admission sends each of the others is its rule
// (admit.Admission.wakeController and its callers, admit's releaseUnslotted).
const (
	TriageNotice        NoticeKind = "triage"
	SlotFreeNotice      NoticeKind = "slot-free"
	TodoNotice          NoticeKind = "todo"
	TickNotice          NoticeKind = "tick"
	UnslottedNotice     NoticeKind = "unslotted"
	AgentQuestionNotice NoticeKind = "agent-question"
)

// AgentDecisionNotice names a person's decision on an ask a stopped agent opened — their answer to
// it, or their reply that hands it back to the agent — which the daemon's answer loop passed on,
// with its account (Notice.AgentDecision) and the agent's role, once per decision (the daemon's
// Decide). It is an architect's notice, sent to the architect that owns the agent's issue while that
// architect holds its role, and the controller's otherwise, in a controller notice row of the same
// kind.
const AgentDecisionNotice NoticeKind = "agent-decision"

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
	// Generation is the issue generation the request serves. A request of an earlier generation
	// finishes without acting: it must never suspend, stop, or start the next generation's worker.
	Generation uint64 `json:"generation"`
	// Phase is the phase a phase worker's start serves; a start for a phase the issue has left
	// finishes without acting. It is empty for the architect, which serves every phase, and for a
	// suspend or a tree's close, which must still act after the issue moves on.
	Phase phase.Phase `json:"phase,omitempty"`
	// ResumeTask marks Task as the phase's resume task (workflow.ResumePhaseTask), the one a
	// re-admitted tree's promotion gives a mid-phase child: a claim that already holds a task for
	// the same generation and phase is given it once it is ready, so the start delivers none.
	ResumeTask bool `json:"resumeTask,omitempty"`
	// RuleResume marks the boot's resume-by-rule start. If its resumed claim spends its launch
	// budget, the runner finishes this row rather than retrying it as RequestRetry with fresh
	// budgets: the failed claim stays failed for the boot report to name.
	RuleResume bool `json:"ruleResume,omitempty"`
	// Quiesce is the role whose phase a start takes over without that role's completion: CI settled
	// red while it tested, reviewed a round it had not completed, or worked toward its READY as the
	// merger (workflow's TriggerChecksRed). The start acts only once that role's claim is out of its
	// turn (supervise.Machine.Quiesce), so the two never write the shared workspace together. It is
	// empty for every other start, which a completion or a person's move hands the phase, for a move
	// between two phases of one role (retro back to implementing on a red, where the implementer
	// keeps its turn and is handed the new task once it ends), and for every other operation.
	Quiesce claim.Role `json:"quiesce,omitempty"`
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
}

func (WorkspaceRemove) OutboxKind() OutboxKind { return OutboxKindWorkspaceRemove }

// MergeQueuePublish is the merger's READY packet, or the daemon's withdrawal of it, published to
// the project's merge queue role, `projects.<KEY>.merge_queue_role`: its bare name, which the
// runner addresses as a role topic.
type MergeQueuePublish struct {
	Role   string `json:"role"`
	Packet string `json:"packet"`
}

func (MergeQueuePublish) OutboxKind() OutboxKind { return OutboxKindMergeQueuePublish }

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

// AutoMergeDisable turns off whatever would merge the pull request without a person: GitHub's
// auto-merge, then its place in a merge queue. Arming is legitimate only for the READY head of an
// issue awaiting its merge, so the workflow queues it on every head move but one landing on that
// head (a push, or a reopen), on every READY withdrawal, on a child's re-entry and on an issue's
// leaving the workflow. Its executor decides by the record when it runs, keeping the arm only while
// the issue awaits its merge at the merger's READY commit.
type AutoMergeDisable struct {
	Number int `json:"number"`
}

func (AutoMergeDisable) OutboxKind() OutboxKind { return OutboxKindAutoMergeDisable }

// ReadyMerge submits the merge of pull request Number at Head, the head an accepted READY named.
// The workflow queues it when an issue reaches awaiting_merge on a READY nobody marked by hand. Its
// executor decides by GitHub and then the record when it runs, so a READY withdrawn, moved past or
// left since submits nothing, and it waits while GitHub reports the pull request not yet mergeable
// in a repository that allows no auto-merge or on a base that requires a merge queue. Armed is the
// row having seen Head armed for auto-merge or in the merge queue on GitHub, which its executor
// records on the row (Store.ReplaceOutboxPayload): from then on the arm or entry disappearing
// without a merge is a removal the row reads from GitHub rather than a cue to submit again
// (ghautomerge.Submit). Warned is the row having told the issue's architect once that GitHub has
// not completed the merge it submitted (merge-waiting, the daemon's mergeWaitWarn), which keeps it
// from telling again however much longer it waits, and Queued is the moment the workflow queued
// this submission, which that wait is measured from. A queue of the same issue and head starts the
// row over unarmed, unwarned and at a fresh Queued (readyMergeConflict): the upsert replaces the
// payload, so a reopen resets the clock and the once-flag together and the two can never disagree,
// as they would were the wait measured from the row's created_at, which the upsert leaves at the
// original queueing. A row a daemon before Queued enqueued carries the zero time, and its executor
// measures that one from created_at, which is what it was measured from then.
type ReadyMerge struct {
	Number int       `json:"number"`
	Head   string    `json:"head"`
	Armed  bool      `json:"armed,omitempty"`
	Warned bool      `json:"warned,omitempty"`
	Queued time.Time `json:"queued,omitzero"`
}

func (ReadyMerge) OutboxKind() OutboxKind { return OutboxKindReadyMerge }

// CheckCompare wakes a production check from a build or from a deploy run, whichever of Build and
// Deploy it carries (exactly one). Build is the commit a daemon boot or a Dispatch start reported
// itself built from: the executor runs a GitHub compare of the check's merge commit with it, as the
// implement App, and starts the look Look when the build contains the merge. Deploy is a completed
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
	case OutboxKindMergeQueuePublish:
		value := MergeQueuePublish{}
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
	case OutboxKindAutoMergeDisable:
		value := AutoMergeDisable{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindReadyMerge:
		value := ReadyMerge{}
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
		if value.Question != nil {
			return fmt.Errorf("a %s notice carries a question; only a controller's agent-question notice does", value.Kind)
		}
		if err := validAgentDecision(value.Kind, value.AgentDecision); err != nil {
			return err
		}
	case ControllerNotice:
		if !controllerOnlyNoticeKind(value.Kind) && !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown controller notice kind %q", value.Kind)
		}
		if (value.Kind == AgentQuestionNotice) != (value.Question != nil) {
			return fmt.Errorf("a %s controller notice carries a question only when it is an agent-question notice", value.Kind)
		}
		if value.Question != nil && value.Question.ID == "" {
			return fmt.Errorf("an agent-question notice names its question's id")
		}
		if err := validAgentDecision(value.Kind, value.AgentDecision); err != nil {
			return err
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
		if value.Quiesce != "" && (value.Op != "start" || value.Quiesce == value.Role) {
			return fmt.Errorf("a supervise quiesce names another role than its start's, and only a start carries one")
		}
		if value.CheckLook != 0 && (value.CheckLook < 0 || value.Op != "start" || value.Role != claim.RoleImplementer || value.Phase != "") {
			return fmt.Errorf("a supervise check look names a start of the implementer with no phase, and only a start carries one")
		}
	case MergeQueuePublish:
		if value.Role == "" {
			return fmt.Errorf("merge queue publish requires role")
		}
		if value.Packet == "" {
			return fmt.Errorf("merge queue publish requires packet")
		}
	case AutoMergeDisable:
		if value.Number <= 0 {
			return fmt.Errorf("auto-merge disable requires its pull request number")
		}
	case ReadyMerge:
		if value.Number <= 0 || value.Head == "" {
			return fmt.Errorf("READY merge requires its pull request number and head")
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
	default:
		return fmt.Errorf("unknown outbox payload %T", payload)
	}
	return nil
}

func validNoticeKind(kind NoticeKind) bool {
	switch kind {
	case "phase-finished", "worker-died", "held", "pr-blocked", "pr-merged", "pr-closed-unmerged", "merge-refused", "merge-stalled", "merge-removed", "merge-waiting", "design-approved", "design-changes-requested", "carried-gate-opened", "ready-refused", "child-closed", "child-status", "child-release-stalled", "child-not-recorded", "catch-up", "checks-red", "review-stuck", "status-reasserted", AgentDecisionNotice:
		return true
	default:
		return false
	}
}

// validAgentDecision checks that a notice of kind carries a decision's account when, and only
// when, it is an agent-decision notice, and that the account names its decision's id.
func validAgentDecision(kind NoticeKind, decision *AgentDecision) error {
	if (kind == AgentDecisionNotice) != (decision != nil) {
		return fmt.Errorf("a %s notice carries a decision only when it is an agent-decision notice", kind)
	}
	if decision != nil && decision.ID == "" {
		return fmt.Errorf("an agent-decision notice names its decision's id")
	}
	return nil
}

// controllerOnlyNoticeKind says whether kind is a notice for the controller alone, never an
// architect's.
func controllerOnlyNoticeKind(kind NoticeKind) bool {
	switch kind {
	case TriageNotice, SlotFreeNotice, TodoNotice, TickNotice, UnslottedNotice, AgentQuestionNotice:
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
