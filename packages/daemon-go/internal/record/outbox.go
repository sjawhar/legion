package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

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
	OutboxKindSupervise         OutboxKind = "supervise"
	OutboxKindGateSeed          OutboxKind = "gate_seed"
	OutboxKindLingerClose       OutboxKind = "linger_close"
	OutboxKindWorkspaceRemove   OutboxKind = "workspace_remove"
	OutboxKindMergeQueuePublish OutboxKind = "merge_queue_publish"
)

// OutboxPayload is the sealed vocabulary of payloads a workflow may enqueue.
type OutboxPayload interface{ OutboxKind() OutboxKind }

// StatusWrite records the Dispatch status observed when this write was enqueued.
type StatusWrite struct {
	Status         string `json:"status"`
	ObservedStatus string `json:"observedStatus"`
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

// Posted is the message the runner posts for row id: the body, then the row's marker.
func (m MessagePost) Posted(id int64) string {
	return m.Body + messageSeparator + MessageMarker(id)
}

// NoticeKind identifies one persisted notice delivery.
type NoticeKind string

// Notice is a role-facing workflow observation. A phase-finished notice carries the finishing
// worker's own summary and, beside it, the verdict it gave (the tester's pass or fail).
type Notice struct {
	Kind    NoticeKind  `json:"kind"`
	Role    claim.Role  `json:"role,omitempty"`
	Phase   phase.Phase `json:"phase,omitempty"`
	Summary string      `json:"summary,omitempty"`
	Verdict string      `json:"verdict,omitempty"`
	Version int         `json:"version,omitempty"`
	Reason  string      `json:"reason,omitempty"`
}

func (Notice) OutboxKind() OutboxKind { return OutboxKindNotice }

// SuperviseOp identifies a worker-session operation: "start" starts, resumes, or retries the role's
// claim; "suspend" stops its process and keeps its session; "tree_close" is the tree's close —
// the one request that ends the claim, the tree's root claim included. There is no plain stop:
// only the tree's close ends a claim from the workflow, so the op states it.
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
	// Leaves is the phase a transition's suspend ends. Such a suspend finishes without acting once
	// the issue is back in a phase its role works (workflow.SuspendApplies). It is empty for the
	// suspends a linger or a child's leave queues, which stop every claim whatever phase its issue
	// holds, and for every other operation.
	Leaves phase.Phase `json:"leaves,omitempty"`
	// ResumeTask marks Task as the phase's resume task (workflow.ResumePhaseTask), the one a
	// re-admitted tree's promotion gives a mid-phase child: a claim that already holds a task for
	// the same generation and phase is given it once it is ready, so the start delivers none.
	ResumeTask bool `json:"resumeTask,omitempty"`
	// Linger is the root generation whose linger a tree close expires. A member keeps its
	// generation across re-admission, so the close acts only while its tree lingers at that root
	// generation: never in the tree's next run, nor in a later linger of it.
	Linger uint64 `json:"linger,omitempty"`
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

// WorkspaceRemove removes the workspace named by the row's issue.
type WorkspaceRemove struct {
	// Linger is the root generation whose linger the removal expires. Like a tree close, it acts
	// only while the tree lingers at that root generation: once re-admission moves the root on, the
	// workspace there is the tree's next run's, whatever generation the issue itself keeps.
	Linger uint64 `json:"linger"`
}

func (WorkspaceRemove) OutboxKind() OutboxKind { return OutboxKindWorkspaceRemove }

// MergeQueuePublish is the merger's READY packet published to the project's merge queue role,
// `projects.<KEY>.merge_queue_role`: its bare name, which the runner addresses as a role topic.
type MergeQueuePublish struct {
	Role   string `json:"role"`
	Packet string `json:"packet"`
}

func (MergeQueuePublish) OutboxKind() OutboxKind { return OutboxKindMergeQueuePublish }

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
	case MessagePost, GateSeed, LingerClose:
	case WorkspaceRemove:
		if value.Linger == 0 {
			return fmt.Errorf("workspace removal requires the root generation of its linger")
		}
	case Notice:
		if !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown notice kind %q", value.Kind)
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
	case MergeQueuePublish:
		if value.Role == "" {
			return fmt.Errorf("merge queue publish requires role")
		}
		if value.Packet == "" {
			return fmt.Errorf("merge queue publish requires packet")
		}
	default:
		return fmt.Errorf("unknown outbox payload %T", payload)
	}
	return nil
}

func validNoticeKind(kind NoticeKind) bool {
	switch kind {
	case "phase-finished", "worker-died", "held", "pr-blocked", "pr-merged", "pr-closed-unmerged", "design-approved", "design-changes-requested", "ready-refused", "child-closed", "child-status":
		return true
	default:
		return false
	}
}

func validSuperviseOp(op SuperviseOp) bool {
	switch op {
	case "start", "suspend", "tree_close":
		return true
	default:
		return false
	}
}
