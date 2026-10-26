package answerloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Kind is where a question came from, which says where its answer is posted.
type Kind string

const (
	// KindMessage is a Dispatch message sent to the agent (on an issue, or on its conversation
	// page), answered through the delivery attempt's reply route.
	KindMessage Kind = "message"
	// KindMention is a Dispatch comment mentioning the agent, answered through the mention's
	// delivery attempt.
	KindMention Kind = "mention"
	// KindAskReply is a comment on the thread of an ask the agent opened, answered on that thread.
	KindAskReply Kind = "ask_reply"
	// KindEnvoy is another agent's Envoy message, answered by a direct message back to its sender.
	KindEnvoy Kind = "envoy"
)

// AnswerMarker opens the marker every reply the loop posts carries, `<!-- legion-answer:<id> -->`:
// a hand-off carrying it is the loop's own answer and is never answered, and the loop reads an
// ask's thread for it before posting there again.
const AnswerMarker = "<!-- legion-answer:"

// answerDedupePrefix marks the loop's own Envoy publishes (a forward's dedupe key).
const answerDedupePrefix = "legion-answer:"

// outboxDedupePrefix marks the daemon's own notices (record.OutboxKey).
const outboxDedupePrefix = "legion-outbox:"

// daemonSessionPrefix is the session the daemon writes Dispatch as (dispatch.DaemonSession).
const daemonSessionPrefix = "legion-daemon:"

// Origin is a question as its hand-off carries it: where it came from and its identity, its text,
// who asked and where (for the model, and for the controller when it is passed on), whether the
// agent may reply it asks nothing, and what posting its answer needs.
type Origin struct {
	Kind          Kind
	Identity      string
	Text          string
	Asker         string
	Where         string
	Ref           string
	MayAskNothing bool
	// MessageID and Attempt are a message's delivery; CommentID, Target and Attempt a mention's.
	MessageID string
	CommentID string
	Target    string
	Attempt   int
	// Issue and AskID are an ask reply's thread.
	Issue string
	AskID string
	// Sender and EventID are an Envoy message's: who to answer and the envelope answered.
	Sender  string
	EventID string
}

// Verdict is what a hand-off is: a question to answer, a decision to pass to Decisions (a person's
// reply on the agent's ask is both), or neither, Because says why.
type Verdict struct {
	Question *Origin
	Decision *Decision
	Because  string
}

// AskReader reads an ask's author (Dispatch.Ask).
type AskReader interface {
	Ask(ctx context.Context, askID string) (dispatch.Ask, error)
}

// QuestionID is a question's own id: 32 hex digits of a hash of the project, its identity and the
// session answering it, the same however often it is handed off.
func QuestionID(project, identity, session string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + identity + "\x00" + session))
	return hex.EncodeToString(sum[:16])
}

// Classify decides, before any model call, what a hand-off for conv is. Answered: a Dispatch
// message sent to the agent; a comment that mentions it (its mention's delivery); a comment on the
// thread of an ask it opened (a follower that did not open the ask answers nothing); another
// agent's Envoy message, which may ask nothing. A decision: a person's answer to the agent's ask,
// the ask handed back to it, and a person's reply on its ask, which is also answered. Never: an
// edit, a resolution, a delivery receipt, a claim, an issue event, a reply the agent already got
// (*.answered), the daemon's own notices and posts, the broker's (source envoy), and anything
// carrying the loop's own marker (its dedupe key, the `<!-- legion-answer:` marker, a Legion footer
// with an `answer` key). The ask's author is read from Dispatch.
func Classify(ctx context.Context, h intake.Handoff, conv store.AgentConversation, asks AskReader) (Verdict, error) {
	env := h.Envelope
	switch {
	case strings.Contains(env.DedupeKey, answerDedupePrefix) || carriesLoopMark(env.PayloadSummary) || carriesLoopMark(env.Payload):
		return Verdict{Because: "the loop's own answer"}, nil
	case strings.HasPrefix(env.DedupeKey, outboxDedupePrefix):
		return Verdict{Because: "the daemon's own notice"}, nil
	case env.Source == "envoy":
		return Verdict{Because: "the broker's own message"}, nil
	case env.Source == "dispatch":
		return classifyDispatch(ctx, h, conv, asks)
	case env.Source == "agent" && env.SourceSession != "":
		origin, err := ReadOrigin(h)
		if err != nil {
			return Verdict{}, err
		}
		return Verdict{Question: &origin}, nil
	default:
		return Verdict{Because: fmt.Sprintf("a message from %q no agent answers", env.Source)}, nil
	}
}

func classifyDispatch(ctx context.Context, h intake.Handoff, conv store.AgentConversation, asks AskReader) (Verdict, error) {
	event, err := h.Envelope.Dispatch()
	if err != nil {
		return Verdict{Because: err.Error()}, nil
	}
	switch {
	case carriesLoopMark(event.Payload.Body):
		return Verdict{Because: "the loop's own answer"}, nil
	case strings.HasPrefix(event.Actor.ID, daemonSessionPrefix):
		return Verdict{Because: "the daemon's own post"}, nil
	}
	ownAsk := func(askID string) (bool, error) {
		ask, err := asks.Ask(ctx, askID)
		if err != nil {
			return false, fmt.Errorf("read the author of ask %s: %w", askID, err)
		}
		return ask.Author.Kind == "session" && ask.Author.ID == h.TargetSession, nil
	}
	decision := &Decision{Handoff: h, Event: event, Conversation: conv}
	switch event.Type {
	case "message.created":
		if event.Delivery == nil {
			return Verdict{Because: "a message not sent to the agent, with no delivery to answer"}, nil
		}
	case "comment.created":
		if event.Delivery != nil {
			break
		}
		if event.Payload.AskID == nil {
			return Verdict{Because: "a comment that neither mentions the agent nor replies on its ask"}, nil
		}
		own, err := ownAsk(*event.Payload.AskID)
		if err != nil {
			return Verdict{}, err
		}
		if !own {
			return Verdict{Because: "a reply on an ask the agent follows but did not open"}, nil
		}
		origin, err := ReadOrigin(h)
		if err != nil {
			return Verdict{Because: err.Error()}, nil
		}
		if origin.MayAskNothing {
			return Verdict{Question: &origin, Decision: decision}, nil
		}
		return Verdict{Question: &origin}, nil
	case "ask.answered", "ask.handed_back":
		own, err := ownAsk(event.Payload.ID)
		if err != nil {
			return Verdict{}, err
		}
		if !own {
			return Verdict{Because: "a decision on an ask the agent did not open"}, nil
		}
		return Verdict{Decision: decision, Because: "a decision, which the loop answers nothing for"}, nil
	default:
		return Verdict{Because: "a " + event.Type + " event, which asks the agent nothing"}, nil
	}
	origin, err := ReadOrigin(h)
	if err != nil {
		return Verdict{Because: err.Error()}, nil
	}
	return Verdict{Question: &origin}, nil
}

// ReadOrigin reads what a question's hand-off says about it, without reading anything else: its
// kind, identity, text, asker and where to post its answer. It is how the loop reads a recorded
// question's hand-off again.
func ReadOrigin(h intake.Handoff) (Origin, error) {
	env := h.Envelope
	if env.Source != "dispatch" {
		text := env.Payload
		if text == "" {
			text = env.PayloadSummary
		}
		return Origin{
			Kind: KindEnvoy, Identity: env.DedupeKey, Text: text, Asker: "session:" + env.SourceSession,
			Where: "an Envoy message from session " + env.SourceSession, Ref: "envoy:" + env.EventID, MayAskNothing: true,
			Sender: env.SourceSession, EventID: env.EventID,
		}, nil
	}
	event, err := env.Dispatch()
	if err != nil {
		return Origin{}, err
	}
	p := event.Payload
	if p.ID == "" {
		return Origin{}, fmt.Errorf("the Dispatch %s of %s names no id", event.Type, env.EventID)
	}
	issue := ""
	if p.IssueKey != nil {
		issue = *p.IssueKey
	} else if event.IssueKey != nil {
		issue = *event.IssueKey
	}
	o := Origin{Identity: "dispatch:" + p.ID, Text: p.Body, Asker: event.Actor.Kind + ":" + event.Actor.ID, Issue: issue}
	switch {
	case event.Type == "message.created" && event.Delivery != nil:
		o.Kind, o.MessageID, o.Attempt = KindMessage, p.ID, event.Delivery.Attempt
		if issue == "" {
			o.Where, o.Ref = "a message on the agent's Dispatch conversation page", "dispatch message "+p.ID
		} else {
			o.Where, o.Ref = "a Dispatch message on "+issue, "dispatch://"+issue+"/message/"+p.ID
		}
	case event.Type == "comment.created" && event.Delivery != nil:
		o.Kind, o.CommentID, o.Target, o.Attempt = KindMention, p.ID, event.Delivery.Target, event.Delivery.Attempt
		o.Where, o.Ref = "a Dispatch comment mentioning the agent on "+issue, "dispatch://"+issue+"/comment/"+p.ID
	case event.Type == "comment.created" && p.AskID != nil:
		if issue == "" {
			return Origin{}, fmt.Errorf("the reply %s is on a document's ask, which has no issue thread to answer on", p.ID)
		}
		o.Kind, o.AskID = KindAskReply, *p.AskID
		o.Where, o.Ref = "a reply on the agent's ask "+*p.AskID+" on "+issue, "dispatch://"+issue+"/comment/"+p.ID
		// A person's reply hands the ask back to the agent: a decision, which may ask nothing.
		o.MayAskNothing = event.Actor.Kind != "session"
	default:
		return Origin{}, fmt.Errorf("a Dispatch %s is no question", event.Type)
	}
	if event.Delivery != nil && o.Attempt < 1 {
		return Origin{}, fmt.Errorf("the Dispatch delivery of %s names no attempt", p.ID)
	}
	return o, nil
}

// legionFooter is a Legion footer, `<!-- legion: {…} -->`.
var legionFooter = regexp.MustCompile(`<!--\s*legion:\s*(\{[^<>]*\})\s*-->`)

// carriesLoopMark is whether text carries the loop's own marker, raw or as JSON escapes it, or a
// Legion footer with an `answer` key (a gh comment or review the loop's tool posted).
func carriesLoopMark(text string) bool {
	if strings.Contains(text, AnswerMarker) || strings.Contains(text, `\u003c!-- legion-answer:`) {
		return true
	}
	for _, match := range legionFooter.FindAllStringSubmatch(text, -1) {
		var footer map[string]json.RawMessage
		if json.Unmarshal([]byte(match[1]), &footer) == nil {
			if _, ok := footer["answer"]; ok {
				return true
			}
		}
	}
	return false
}
