package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Handoff is one message the listener handed off: the topic it was for (the agent's own,
// notifications.agent.<session>, or its role's, notifications.role.<token>), the agent session it
// is for (the session addressed, or the role's newest), that session's role token, the Legion
// project token, and the Envoy envelope exactly as it would have been delivered to the agent.
type Handoff struct {
	OriginalTopic string          `json:"original_topic"`
	TargetSession string          `json:"target_session"`
	RoleToken     string          `json:"role_token"`
	Project       string          `json:"project"`
	Envelope      HandoffEnvelope `json:"envelope"`
}

// HandoffEnvelope is the Envoy envelope a hand-off carries (EnvelopeSchema, packages/contracts/src/
// envelope.ts): who sent it (Source, SourceSession, Sender), its identity (EventID, DedupeKey), the
// topic it was published to, its text (PayloadSummary, and Payload: the whole text of a long
// message, or the JSON a Dispatch delivery carries, which Dispatch decodes), and its reply
// correlation (InReplyTo).
type HandoffEnvelope struct {
	EventID        string         `json:"event_id"`
	Source         string         `json:"source"`
	SourceEventID  string         `json:"source_event_id"`
	SourceSession  string         `json:"source_session,omitempty"`
	Topic          string         `json:"topic"`
	DedupeKey      string         `json:"dedupe_key"`
	IssuedAt       int64          `json:"issued_at"`
	ExpiresAt      *int64         `json:"expires_at,omitempty"`
	PayloadSummary string         `json:"payload_summary"`
	Payload        string         `json:"payload,omitempty"`
	PayloadRef     string         `json:"payload_ref,omitempty"`
	TraceID        string         `json:"trace_id"`
	Sender         *HandoffSender `json:"sender,omitempty"`
	InReplyTo      string         `json:"in_reply_to,omitempty"`
	Supersedes     string         `json:"supersedes,omitempty"`
	Urgency        string         `json:"urgency,omitempty"`
	ExpectsReply   string         `json:"expects_reply,omitempty"`
}

// HandoffSender is the listener's stamp of an envelope's sending session.
type HandoffSender struct {
	SessionID string   `json:"session_id"`
	Machine   string   `json:"machine,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Title     string   `json:"title,omitempty"`
	Roles     []string `json:"roles,omitempty"`
}

// HandoffSubject is the subject the listener hands a message for session off on.
func HandoffSubject(project, session string) string {
	return handoffPrefix(project) + session
}

func handoffPrefix(project string) string { return "notifications.legion." + project + ".agents." }

// DecodeHandoff reads a hand-off and checks what the daemon relies on: a target session, a role
// token, a project, and an envelope with its event id, source, topic and dedupe key.
func DecodeHandoff(data []byte) (Handoff, error) {
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return Handoff{}, fmt.Errorf("decode the hand-off: %w", err)
	}
	switch {
	case h.OriginalTopic == "" || h.TargetSession == "" || h.RoleToken == "" || h.Project == "":
		return Handoff{}, errors.New("the hand-off lacks its original topic, target session, role token or project")
	case h.Envelope.EventID == "" || h.Envelope.Source == "" || h.Envelope.Topic == "" || h.Envelope.DedupeKey == "":
		return Handoff{}, errors.New("the hand-off's envelope lacks its event id, source, topic or dedupe key")
	}
	return h, nil
}

// DispatchActor is a Dispatch writer: a person ("user", their login), an agent ("session", its
// session id), or Dispatch itself ("system").
type DispatchActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Person says whether the actor is a person: only a person's act is a decision on an agent's ask
// (dispatch.Actor.Person of sjawhar/legion#2163).
func (a DispatchActor) Person() bool { return a.Kind == "user" }

// DispatchDelivery is a targeted send's delivery: the attempt the recipient replies through, the
// mode it was sent in, and for a mention the comment and the mention's target.
type DispatchDelivery struct {
	Attempt   int    `json:"attempt"`
	Mode      string `json:"mode"`
	CommentID string `json:"comment_id,omitempty"`
	Target    string `json:"target,omitempty"`
}

// DispatchMention is one mention a comment carries: its target and the session it resolved to.
type DispatchMention struct {
	Target    string  `json:"target"`
	SessionID *string `json:"session_id"`
}

// DispatchAnswer is a person's answer to an ask as ask.answered carries it (Dispatch's AskAnswer):
// who answered, the options they selected, and their text, nil when they wrote none.
type DispatchAnswer struct {
	User     string   `json:"user"`
	Selected []string `json:"selected"`
	Text     *string  `json:"text"`
}

// DispatchPayload is what the loop reads of a Dispatch event's payload: the message's, comment's
// or ask's id, its issue, its text, its author (an ask's), its thread (InReplyTo of a message,
// ReplyTo and AskID of a comment), a message's target, a comment's mentions, whom an ask waits on
// after a reply, and an ask's kind (`question`, or `approval` for a document's review request) and
// answer.
type DispatchPayload struct {
	ID           string            `json:"id"`
	IssueKey     *string           `json:"issue_key"`
	Body         string            `json:"body"`
	Author       *DispatchActor    `json:"author"`
	Target       *string           `json:"target"`
	InReplyTo    *string           `json:"in_reply_to"`
	ReplyTo      *string           `json:"reply_to"`
	AskID        *string           `json:"ask_id"`
	Mentions     []DispatchMention `json:"mentions"`
	AskWaitingOn string            `json:"ask_waiting_on"`
	Kind         string            `json:"kind"`
	Answer       *DispatchAnswer   `json:"answer"`
}

// DispatchEvent is a Dispatch event as an envelope's payload carries it: its id, its type, who
// acted, its issue and project, and its payload; Delivery is set when it came in a targeted send's
// frame ({event, delivery}), nil for a bare event on a follower or author route.
type DispatchEvent struct {
	ID       int64           `json:"id"`
	Type     string          `json:"type"`
	Actor    DispatchActor   `json:"actor"`
	IssueKey *string         `json:"issue_key"`
	Project  string          `json:"project"`
	Payload  DispatchPayload `json:"payload"`
	Delivery *DispatchDelivery
}

// HandsTurnBack says whether the event is a person's reply on an ask that leaves the next move
// with the ask's agent, as sjawhar/legion#2163's dispatchAskReply reads a reply: a comment on an
// ask (AskID) by a person whose ask_waiting_on is not `human`. Dispatch hands an open ask to the
// agent at every reply of a person (`agent`); a reply under an answered or resolved ask records no
// turn (empty), and is the only answer a person can still give there. A person's reply that leaves
// the ask with the human is their own hand-back of an approval request, which hands nothing back.
func (e DispatchEvent) HandsTurnBack() bool {
	return e.Type == "comment.created" && e.Payload.AskID != nil && e.Actor.Person() && e.Payload.AskWaitingOn != "human"
}

// Dispatch decodes a Dispatch envelope's payload: a targeted send's {event, delivery} frame, or the
// bare event Dispatch's outbox publishes.
func (e HandoffEnvelope) Dispatch() (DispatchEvent, error) {
	if e.Source != "dispatch" {
		return DispatchEvent{}, fmt.Errorf("an envelope from %q is not Dispatch's", e.Source)
	}
	var frame struct {
		Event    *DispatchEvent    `json:"event"`
		Delivery *DispatchDelivery `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &frame); err != nil {
		return DispatchEvent{}, fmt.Errorf("decode the Dispatch payload of %s: %w", e.EventID, err)
	}
	if frame.Event != nil && frame.Delivery != nil {
		event := *frame.Event
		event.Delivery = frame.Delivery
		return event, nil
	}
	var event DispatchEvent
	if err := json.Unmarshal([]byte(e.Payload), &event); err != nil {
		return DispatchEvent{}, fmt.Errorf("decode the Dispatch event of %s: %w", e.EventID, err)
	}
	if event.Type == "" {
		return DispatchEvent{}, fmt.Errorf("the Dispatch payload of %s names no event type", e.EventID)
	}
	return event, nil
}

// HandoffSpec configures the hand-off consumer: the Legion project token whose hand-offs it reads,
// how long a delivery may take before JetStream redelivers it, how long a refused one waits, and
// where it logs.
type HandoffSpec struct {
	Project  string
	AckWait  time.Duration
	NakDelay time.Duration
	Logger   *slog.Logger
}

// HandoffHandler takes one hand-off: nil once it is recorded, so the delivery is acknowledged; an
// error to have it delivered again after the spec's NakDelay. raw is the hand-off as published.
type HandoffHandler func(ctx context.Context, handoff Handoff, raw []byte) error

// Handoffs is the project's durable hand-off consumer.
type Handoffs struct {
	spec     HandoffSpec
	consumer jetstream.Consumer
}

// OpenHandoffs creates or updates the project's durable hand-off consumer on
// notifications.legion.<P>.agents.>, as OpenConsumers does its own: one it creates starts at the
// next hand-off, one that exists keeps its position.
func OpenHandoffs(ctx context.Context, js jetstream.JetStream, spec HandoffSpec) (*Handoffs, error) {
	spec, err := normalizedHandoffSpec(spec)
	if err != nil {
		return nil, err
	}
	stream, err := js.Stream(ctx, notificationStream)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", notificationStream, err)
	}
	consumer, err := openConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       handoffConsumerName(spec.Project),
		FilterSubject: handoffPrefix(spec.Project) + ">",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the hand-off durable consumer: %w", err)
	}
	return &Handoffs{spec: spec, consumer: consumer}, nil
}

// Run hands every hand-off to handle until ctx ends.
func (h *Handoffs) Run(ctx context.Context, handle HandoffHandler) error {
	consuming, err := h.consumer.Consume(func(message jetstream.Msg) {
		ConsumeHandoff(ctx, message, h.spec, handle)
	})
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		consuming.Stop()
		<-consuming.Closed()
		return nil
	case <-consuming.Closed():
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("durable consumer %s stopped unexpectedly", h.consumer.CachedInfo().Name)
	}
}

// HandoffDelivery is what ConsumeHandoff reads and settles of one delivery (jetstream.Msg).
type HandoffDelivery interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(delay time.Duration) error
	Term() error
}

// ConsumeHandoff settles one delivery: a hand-off that cannot be decoded, or that is for another
// project or another session than its subject names, is poison and terminated; one handle records
// is acknowledged; one handle fails on is delivered again after the spec's NakDelay.
func ConsumeHandoff(ctx context.Context, delivery HandoffDelivery, spec HandoffSpec, handle HandoffHandler) {
	spec, err := normalizedHandoffSpec(spec)
	if err != nil {
		spec.Logger.Error("hand-off consumer misconfigured", "error", err)
		return
	}
	subject := delivery.Subject()
	handoff, err := DecodeHandoff(delivery.Data())
	if err == nil {
		session, ok := strings.CutPrefix(subject, handoffPrefix(spec.Project))
		switch {
		case handoff.Project != spec.Project:
			err = fmt.Errorf("the hand-off names project %q on this daemon's subject", handoff.Project)
		case !ok || session != handoff.TargetSession:
			err = fmt.Errorf("the hand-off for session %q came on subject %s", handoff.TargetSession, subject)
		}
	}
	if err != nil {
		spec.Logger.Error("poison hand-off", "subject", subject, "error", err)
		if termErr := delivery.Term(); termErr != nil {
			spec.Logger.Error("term poison hand-off", "subject", subject, "error", termErr)
		}
		return
	}
	handled, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	if err := handle(handled, handoff, delivery.Data()); err != nil {
		spec.Logger.Warn("hand-off not recorded; it is delivered again", "subject", subject, "event_id", handoff.Envelope.EventID,
			"retry_in", spec.NakDelay, "error", err)
		if nakErr := delivery.NakWithDelay(spec.NakDelay); nakErr != nil {
			spec.Logger.Error("nak hand-off", "subject", subject, "error", nakErr)
		}
		return
	}
	if err := delivery.Ack(); err != nil {
		spec.Logger.Error("ack hand-off", "subject", subject, "error", err)
	}
}

func normalizedHandoffSpec(spec HandoffSpec) (HandoffSpec, error) {
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}
	if strings.TrimSpace(spec.Project) == "" {
		return spec, errors.New("the hand-off consumer's project is required")
	}
	if spec.AckWait <= 0 {
		spec.AckWait = defaultAckWait
	}
	if spec.NakDelay <= 0 {
		spec.NakDelay = defaultNakDelay
	}
	return spec, nil
}

func handoffConsumerName(project string) string {
	return "legion-go-" + project + "-answers"
}
