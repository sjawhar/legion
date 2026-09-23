package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// OutboxKind names one durable side effect.
type OutboxKind string

const (
	OutboxKindDispatchStatus  OutboxKind = "dispatch_status"
	OutboxKindDispatchMessage OutboxKind = "dispatch_message"
	OutboxKindNotice          OutboxKind = "notice"
	OutboxKindSupervise       OutboxKind = "supervise"
	OutboxKindGateSeed        OutboxKind = "gate_seed"
	OutboxKindLingerClose     OutboxKind = "linger_close"
	OutboxKindWorkspaceRemove OutboxKind = "workspace_remove"
)

// OutboxPayload is the sealed vocabulary of payloads a workflow may enqueue.
type OutboxPayload interface{ OutboxKind() OutboxKind }

// StatusWrite records the Dispatch status observed when this write was enqueued.
type StatusWrite struct {
	Status         string `json:"status"`
	ObservedStatus string `json:"observedStatus"`
}

func (StatusWrite) OutboxKind() OutboxKind { return OutboxKindDispatchStatus }

// MessagePost is a Dispatch message body. The runner appends its outbox marker.
type MessagePost struct {
	Body string `json:"body"`
}

func (MessagePost) OutboxKind() OutboxKind { return OutboxKindDispatchMessage }

// NoticeKind identifies one persisted notice delivery.
type NoticeKind string

// Notice is a role-facing workflow observation.
type Notice struct {
	Kind    NoticeKind  `json:"kind"`
	Role    claim.Role  `json:"role,omitempty"`
	Phase   phase.Phase `json:"phase,omitempty"`
	Summary string      `json:"summary,omitempty"`
	Version int         `json:"version,omitempty"`
	Reason  string      `json:"reason,omitempty"`
}

func (Notice) OutboxKind() OutboxKind { return OutboxKindNotice }

// SuperviseOp identifies a worker-session operation.
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
type WorkspaceRemove struct{}

func (WorkspaceRemove) OutboxKind() OutboxKind { return OutboxKindWorkspaceRemove }

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
	case MessagePost, GateSeed, LingerClose, WorkspaceRemove:
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
	default:
		return fmt.Errorf("unknown outbox payload %T", payload)
	}
	return nil
}

func validNoticeKind(kind NoticeKind) bool {
	switch kind {
	case "phase-finished", "worker-died", "held", "pr-blocked", "design-approved", "design-changes-requested", "ready-refused", "child-closed", "child-status":
		return true
	default:
		return false
	}
}

func validSuperviseOp(op SuperviseOp) bool {
	switch op {
	case "start", "suspend", "stop":
		return true
	default:
		return false
	}
}
