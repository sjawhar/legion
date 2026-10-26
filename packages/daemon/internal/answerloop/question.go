package answerloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// process takes q one step on from where it stands: a question not yet appended is answered (again,
// after a restart), an appended one posted, a held forward offered to its agent again.
func (l *Loop) process(ctx context.Context, q store.AnswerQuestion) error {
	h, err := intake.DecodeHandoff(q.Handoff)
	if err != nil {
		return l.drop(ctx, &q, fmt.Sprintf("its recorded hand-off cannot be read: %v", err))
	}
	origin, err := ReadOrigin(h)
	if err != nil {
		return l.drop(ctx, &q, fmt.Sprintf("its recorded hand-off is no question: %v", err))
	}
	switch q.State {
	case store.QuestionReceived, store.QuestionGenerating:
		return l.generate(ctx, q, origin)
	case store.QuestionAppended:
		return l.post(ctx, q, origin)
	case store.QuestionForwarded:
		return l.deliverForward(ctx, q, origin)
	}
	return nil
}

// generate answers q from its conversation, unless its claim runs the conversation (forwarded) or
// the day's spend is over the limit (the daemon's reply), adds the exchange to the conversation,
// then posts the reply.
func (l *Loop) generate(ctx context.Context, q store.AnswerQuestion, origin Origin) error {
	conv, ok, err := l.cfg.Store.AgentConversation(ctx, q.Session)
	if err != nil {
		return err
	}
	if !ok {
		return l.decideFault(ctx, q, origin, ReasonNoConversation)
	}
	if l.cfg.Gate.Running(conv.Token, conv.Session) {
		return l.forward(ctx, q, origin)
	}
	now := l.cfg.Now()
	spent, err := l.cfg.Store.AnswerSpend(ctx, l.cfg.Project, now)
	if err != nil {
		return err
	}
	if spent >= l.cfg.Answers.Bounds.DailyUSD {
		return l.decideFault(ctx, q, origin, ReasonOverLimit)
	}
	if q.State != store.QuestionGenerating {
		if err := l.move(ctx, &q, store.QuestionGenerating, store.QuestionReceived); err != nil {
			return err
		}
	}
	session, err := l.cfg.Conversations.Open(ctx, conv)
	switch {
	case errors.Is(err, conversation.ErrUnknownFormat):
		l.cfg.Log.Warn("answers: the conversation's format is not one the daemon reads", "question", q.ID, "session", q.Session, "error", err)
		return l.decideFault(ctx, q, origin, ReasonUnknownFormat)
	case errors.Is(err, conversationMissing):
		l.cfg.Log.Warn("answers: the conversation is no longer kept", "question", q.ID, "session", q.Session, "error", err)
		return l.decideFault(ctx, q, origin, ReasonNoConversation)
	case err != nil:
		return err
	}
	defer session.Close()
	facts, err := l.cfg.Issues.Facts(ctx, conv.Issue)
	if err != nil {
		return fmt.Errorf("read what changed on %s: %w", conv.Issue, err)
	}
	agent := ghtool.Agent{Issue: conv.Issue, Role: conv.Role, Session: conv.Session, Phase: facts.Phase, Repository: facts.Repository}
	if l.cfg.AppID != nil {
		agent.AppID = l.cfg.AppID(conv.Role)
	}
	tool, err := l.cfg.Tools.Open(ctx, agent, q.ID)
	if err != nil {
		return fmt.Errorf("open the gh tool: %w", err)
	}
	defer func() {
		if err := tool.Close(); err != nil {
			l.cfg.Log.Warn("answers: the gh tool's directory was not removed", "question", q.ID, "error", err)
		}
	}()
	earlier, err := l.earlierCalls(ctx, q, tool)
	if err != nil {
		return err
	}
	reply, err := l.cfg.Answer(ctx, answer.Request{
		Conversation:  session.Conversation(),
		Agent:         answer.Agent{Role: conv.Role, Issue: conv.Issue, Session: conv.Session, Phase: facts.Phase},
		ChangeNote:    changeNote(conv.Issue, facts),
		Question:      answer.Question{ID: q.ID, Text: origin.Text, Asker: origin.Asker, Where: origin.Where},
		MayAskNothing: q.MayAskNothing, Earlier: earlier,
		Providers: l.cfg.Answers.Providers, Prices: l.cfg.Answers.Prices, Bounds: l.cfg.Answers.Bounds,
		GH: tool, Log: l.cfg.Log,
	})
	if reply.CostUSD > 0 {
		if spendErr := l.cfg.Store.AddAnswerSpend(context.WithoutCancel(ctx), l.cfg.Project, now, reply.CostUSD); spendErr != nil {
			l.cfg.Log.Error("answers: the answer's cost was not added to the day's spend", "question", q.ID, "costUSD", reply.CostUSD, "error", spendErr)
		}
	}
	if err != nil {
		return err
	}
	if reply.Fault != "" {
		return l.decideFault(ctx, q, origin, Reason(reply.Fault))
	}
	ran, err := l.cfg.Gate.Append(conv.Token, conv.Session, func() error {
		return session.Append(ctx, reply.Append, l.cfg.Now())
	})
	switch {
	case !ran || errors.Is(err, conversation.ErrMoved):
		l.cfg.Log.Info("answers: the agent's conversation is running or moved; the answer is dropped and the question forwarded",
			"question", q.ID, "session", q.Session, "started", !ran, "error", err)
		return l.forward(ctx, q, origin)
	case err != nil:
		l.cfg.Log.Error("answers: the answer could not be added to the conversation", "question", q.ID, "session", q.Session, "error", err)
		return l.decideFault(ctx, q, origin, ReasonUnknownFormat)
	}
	q.ReplyKind = string(reply.Kind)
	switch reply.Kind {
	case answer.None:
		return l.drop(ctx, &q, "the message asks the agent nothing; its exchange is in the conversation")
	case answer.NotAnswered:
		q.PassReason = string(ReasonNotAnswered)
	case answer.NeedsWork:
		q.PassReason = string(ReasonNeedsWork)
	}
	q.ReplyText = reply.Text
	if err := l.move(ctx, &q, store.QuestionAppended, store.QuestionGenerating); err != nil {
		return err
	}
	return l.post(ctx, q, origin)
}

// earlierCalls are the gh calls an earlier run of q made, each with its result; a comment or review
// call the daemon stopped during is looked up on GitHub by its footer first, and what was found is
// recorded as its result, so the model is told whether it posted. A call that posts nothing, or one
// whose target cannot be read, stays of unknown outcome.
func (l *Loop) earlierCalls(ctx context.Context, q store.AnswerQuestion, tool Tool) ([]store.AnswerToolCall, error) {
	calls, err := l.cfg.Store.AnswerToolCalls(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	for i, call := range calls {
		if call.Result != nil {
			continue
		}
		found, err := tool.Lookup(ctx, call.Seq)
		var outcome string
		switch {
		case errors.Is(err, ghtool.ErrNotAWrite):
			continue
		case err != nil:
			l.cfg.Log.Warn("answers: a gh call of unknown outcome could not be looked up", "question", q.ID, "seq", call.Seq, "error", err)
			continue
		case found.Exists:
			outcome = "The daemon stopped while this call ran. GitHub shows what it posted, at " + found.URL + "; do not post it again."
		default:
			outcome = "The daemon stopped while this call ran. GitHub shows nothing it posted."
		}
		calls[i].Result = &outcome
		if err := l.cfg.Store.FinishAnswerToolCall(ctx, q.ID, call.Seq, outcome, nil, l.cfg.Now().UTC()); err != nil {
			return nil, err
		}
	}
	return calls, nil
}

// decideFault fixes the daemon's own reply in the agent's name for a question the model gave no
// answer to, naming the bound or fault, and posts it; the question is passed on.
func (l *Loop) decideFault(ctx context.Context, q store.AnswerQuestion, origin Origin, reason Reason) error {
	from := q.State
	q.ReplyKind, q.ReplyText, q.PassReason = "", l.faultReply(reason), string(reason)
	if err := l.move(ctx, &q, store.QuestionAppended, from); err != nil {
		return err
	}
	return l.post(ctx, q, origin)
}

// faultReply is the daemon's reply, as the agent, for reason.
func (l *Loop) faultReply(reason Reason) string {
	b := l.cfg.Answers.Bounds
	const passed = " I passed your question to the Legion controller."
	switch reason {
	case Reason(answer.AnswerSeconds):
		return fmt.Sprintf("I could not answer within the %d-second bound.", int(b.Answer.Seconds())) + passed
	case Reason(answer.ModelCalls):
		return fmt.Sprintf("I could not answer within the bound of %d model calls.", b.ModelCalls) + passed
	case Reason(answer.Length):
		return fmt.Sprintf("My answer was cut off at the %d-token output bound.", b.OutputTokens) + passed
	case Reason(answer.Refusal):
		return "My model refused to answer." + passed
	case Reason(answer.Malformed):
		return "My reply was not in the form the daemon checks, so it is not posted." + passed
	case Reason(answer.ProviderError):
		return "My model provider failed to answer." + passed
	case ReasonNoProvider:
		return "No model provider is configured for the model my conversation ran on." + passed
	case ReasonNoPrice:
		return "No price is configured for the model my conversation ran on, so I am not answered on it." + passed
	case ReasonUnknownFormat:
		return "My conversation holds a format the daemon cannot read or write, so I cannot answer from it." + passed
	case ReasonNoConversation:
		return "My conversation is no longer kept, so I cannot answer from it." + passed
	case ReasonOverLimit:
		return fmt.Sprintf("I am over the project's daily limit of $%s for answers.", strconv.FormatFloat(b.DailyUSD, 'f', -1, 64)) + passed
	}
	return "I could not answer (" + string(reason) + ")." + passed
}

// post posts q's reply where it was asked, in the agent's name, once: every route is safe to call
// again (a delivery's reply route keeps one reply per attempt, the ask thread is read for the
// question's marker first, an Envoy message carries the question's id as its idempotency key).
// Then the question is passed on when it must be, and marked done. A post refused for good drops
// the question and passes it on.
func (l *Loop) post(ctx context.Context, q store.AnswerQuestion, origin Origin) error {
	body := q.ReplyText + "\n\n" + AnswerMarker + q.ID + " -->"
	var err error
	switch origin.Kind {
	case KindMessage:
		err = l.cfg.Dispatch.ReplyToMessage(ctx, origin.MessageID, origin.Attempt, q.Session, body)
	case KindMention:
		err = l.cfg.Dispatch.ReplyToComment(ctx, origin.CommentID, origin.Target, origin.Attempt, q.Session, body)
	case KindAskReply:
		err = l.postOnAsk(ctx, q, origin, body)
	case KindEnvoy:
		err = l.cfg.Envoy.Send(ctx, notify.Message{Target: origin.Sender, SourceSession: q.Session, Text: body, InReplyTo: origin.EventID, IdempotencyKey: q.ID})
	default:
		err = fmt.Errorf("no route posts a %s", origin.Kind)
	}
	if refusedForGood(err) {
		l.cfg.Log.Error("answers: the reply was refused; the question is passed on", "question", q.ID, "session", q.Session, "error", err)
		q.PassReason = string(ReasonPostRefused)
		if err := l.cfg.PassOn.PassOn(ctx, q, ReasonPostRefused, origin.Asker, origin.Ref); err != nil {
			return err
		}
		return l.move(ctx, &q, store.QuestionDropped, store.QuestionAppended)
	}
	if err != nil {
		return fmt.Errorf("post the reply: %w", err)
	}
	if q.PassReason == "" {
		return l.move(ctx, &q, store.QuestionPosted, store.QuestionAppended)
	}
	if err := l.cfg.PassOn.PassOn(ctx, q, Reason(q.PassReason), origin.Asker, origin.Ref); err != nil {
		return fmt.Errorf("pass the question on: %w", err)
	}
	return l.move(ctx, &q, store.QuestionPassedOn, store.QuestionAppended)
}

// postOnAsk comments on the ask's thread, unless the thread already holds the agent's comment
// carrying the question's marker: a post the daemon stopped after is not made twice.
func (l *Loop) postOnAsk(ctx context.Context, q store.AnswerQuestion, origin Origin, body string) error {
	ask, err := l.cfg.Dispatch.Ask(ctx, origin.AskID)
	if err != nil {
		return err
	}
	marker := AnswerMarker + q.ID + " -->"
	for _, reply := range ask.Replies {
		if reply.Author.Kind == "session" && reply.Author.ID == q.Session && strings.Contains(reply.Body, marker) {
			l.cfg.Log.Info("answers: the ask's thread already holds the reply; not posted again", "question", q.ID, "comment", reply.ID)
			return nil
		}
	}
	return l.cfg.Dispatch.ReplyOnAsk(ctx, origin.Issue, origin.AskID, q.Session, body)
}

// refusedForGood is whether err is a refusal a retry would meet again: Dispatch's 4xx but a
// timeout or rate limit, or the listener's refusal.
func refusedForGood(err error) bool {
	var refused *dispatch.Error
	if errors.As(err, &refused) {
		return refused.Status >= 400 && refused.Status < 500 && refused.Status != 408 && refused.Status != 429
	}
	return errors.Is(err, notify.ErrRefused)
}

// drop ends q with nothing posted.
func (l *Loop) drop(ctx context.Context, q *store.AnswerQuestion, because string) error {
	l.cfg.Log.Info("answers: question ends with nothing posted", "question", q.ID, "because", because)
	return l.move(ctx, q, store.QuestionDropped, q.State)
}

// forwardPayload is what a forwarded question carries to the running agent: the question, who
// asked and where, how to answer it, and every gh call the loop already made for it, with its
// result, so the agent does not repeat a write.
type forwardPayload struct {
	Kind     string        `json:"kind"`
	Question string        `json:"question"`
	ID       string        `json:"question_id"`
	Text     string        `json:"text"`
	Asker    string        `json:"asker"`
	Where    string        `json:"where"`
	Ref      string        `json:"ref"`
	Calls    []forwardCall `json:"gh_calls"`
}

type forwardCall struct {
	Seq    int      `json:"seq"`
	Args   []string `json:"args"`
	Result *string  `json:"result"`
	Exit   *int     `json:"exit"`
}

// forward hands q to the agent that runs its conversation, nothing posted or appended.
func (l *Loop) forward(ctx context.Context, q store.AnswerQuestion, origin Origin) error {
	if err := l.move(ctx, &q, store.QuestionForwarded, q.State); err != nil {
		return err
	}
	return l.deliverForward(ctx, q, origin)
}

// deliverForward publishes a forwarded question to its agent's role topic. While no live session
// holds the role it is held and offered again at each sweep; once the claim no longer runs the
// conversation it goes back to the loop to answer.
func (l *Loop) deliverForward(ctx context.Context, q store.AnswerQuestion, origin Origin) error {
	if !l.cfg.Gate.Running(q.Token, q.Session) {
		l.cfg.Log.Info("answers: the agent stopped before the forwarded question reached it; the loop answers it", "question", q.ID)
		return l.move(ctx, &q, store.QuestionReceived, store.QuestionForwarded)
	}
	calls, err := l.cfg.Store.AnswerToolCalls(ctx, q.ID)
	if err != nil {
		return err
	}
	payload := forwardPayload{Kind: "agent-question", Question: origin.Text, ID: q.ID, Asker: origin.Asker, Where: origin.Where, Ref: origin.Ref}
	var text strings.Builder
	fmt.Fprintf(&text, "A question reached you while you were stopped, from %s (%s, %s):\n\n%s\n\nAnswer it there yourself.", origin.Asker, origin.Where, origin.Ref, origin.Text)
	if len(calls) > 0 {
		text.WriteString(" The daemon's answer loop already ran these gh calls for it; do not repeat a write:\n")
	}
	for _, call := range calls {
		payload.Calls = append(payload.Calls, forwardCall{Seq: call.Seq, Args: call.Args, Result: call.Result, Exit: call.Exit})
		result := "outcome unknown"
		if call.Result != nil {
			result = *call.Result
		}
		args, _ := json.Marshal(call.Args)
		fmt.Fprintf(&text, "\n%d. gh %s\n%s\n", call.Seq, args, result)
	}
	payload.Text = text.String()
	summary := fmt.Sprintf("A question for the %s of %s from %s", q.Role, q.Issue, origin.Asker)
	err = l.cfg.Envoy.Publish(ctx, notify.RoleTopicPrefix+string(q.Token), summary, payload, answerDedupePrefix+q.ID)
	if errors.Is(err, notify.ErrNoHolder) {
		l.cfg.Log.Info("answers: the forwarded question waits until the agent holds its role", "question", q.ID, "token", q.Token)
		return nil
	}
	if err != nil {
		return fmt.Errorf("forward the question to %s: %w", q.Token, err)
	}
	q.ForwardDelivered = true
	return l.move(ctx, &q, store.QuestionForwarded, store.QuestionForwarded)
}

// changeNote is what has changed on the agent's issue since it stopped, as the model is told it.
func changeNote(issue string, facts IssueFacts) string {
	if !facts.Recorded {
		return "The daemon holds no record of " + issue + ", so it cannot say what changed on it since you stopped."
	}
	var note strings.Builder
	fmt.Fprintf(&note, "Since you stopped: Dispatch shows %s as %s, and the daemon has it in phase %s.", issue, facts.DispatchStatus, facts.Phase)
	if pr := facts.PullRequest; pr != nil {
		fmt.Fprintf(&note, " Its pull request #%d is %s, its head %s.", pr.Number, pr.State, pr.Head)
	} else {
		note.WriteString(" The daemon records no pull request for it.")
	}
	return note.String()
}
