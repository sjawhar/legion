// Package answerloop is the daemon's question pipeline: it takes every message the Envoy listener
// hands off for one of the daemon's agents that is not running (intake.Handoff), decides before any
// model call whether it is a question the agent answers (Classify), and answers each question from
// the agent's own conversation (internal/answer), adds the exchange to that conversation in its
// harness's format, and posts the reply in the agent's name where it was asked.
//
// A question is recorded once (answer_questions), by an identity stable across retries and
// restarts, and moves received -> generating -> appended -> posted, or passed_on when the
// controller must hear of it (PassOn), forwarded when the agent runs its conversation, or dropped.
// Recovery follows the state: a question not yet appended is answered again, the model handed the
// gh calls the earlier run made (a comment or review call of unknown outcome looked up on GitHub
// first, ghtool.Lookup); one appended is posted without answering again, since its answer is in
// the conversation already.
//
// One writer: the questions to one conversation run one after another, at most
// answers.bounds.concurrent conversations at once. A conversation its claim runs, or is starting,
// is never written: the question goes to the running agent on its role topic, held until the role
// is held (Gate). The day's spend is kept per project and UTC day; over answers.bounds.daily_usd a
// question gets the daemon's reply in the agent's name and is passed on.
package answerloop

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Store is the loop's record (*store.Store): the conversations the daemon's history holds, the
// questions, their gh calls, and the day's spend.
type Store interface {
	AgentConversation(ctx context.Context, session string) (store.AgentConversation, bool, error)
	RoleConversations(ctx context.Context, token claim.Token) ([]store.AgentConversation, error)
	ReceiveQuestion(ctx context.Context, q store.AnswerQuestion) (store.AnswerQuestion, bool, error)
	OpenQuestions(ctx context.Context, project string) ([]store.AnswerQuestion, error)
	UpdateQuestion(ctx context.Context, q store.AnswerQuestion, from ...store.QuestionState) error
	AnswerToolCalls(ctx context.Context, question string) ([]store.AnswerToolCall, error)
	FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error
	AnswerSpend(ctx context.Context, project string, at time.Time) (float64, error)
	AddAnswerSpend(ctx context.Context, project string, at time.Time, usd float64) error
}

// Dispatch is what the loop reads of Dispatch and writes there in the agent's name
// (*dispatch.HTTPClient).
type Dispatch interface {
	Ask(ctx context.Context, askID string) (dispatch.Ask, error)
	ReplyToMessage(ctx context.Context, messageID string, attempt int, session, body string) error
	ReplyToComment(ctx context.Context, commentID, target string, attempt int, session, body string) error
	ReplyOnAsk(ctx context.Context, issue, askID, session, body string) error
}

// Envoy sends the loop's messages through the listener (*notify.HTTPPublisher): an answer to an
// agent's message in the answering agent's name, and a question forwarded to the running agent on
// its role topic, which a publisher marks never to be handed off.
type Envoy interface {
	Send(ctx context.Context, m notify.Message) error
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
}

// Gate keeps the loop and a claim's start off one conversation at once (the daemon's answerGate).
// Running is whether session's conversation is its claim's current one and the claim runs it, is
// starting or has been started. Append runs write while the claim cannot start, unless Running:
// then it runs nothing and answers false.
type Gate interface {
	Running(token claim.Token, session string) bool
	Append(token claim.Token, session string, write func() error) (bool, error)
}

// IssueFacts is what has changed on an agent's issue since it stopped, for the change note: the
// status Dispatch shows, the daemon's phase, the repository the issue works, and its pull request.
// Recorded is false for an issue the daemon holds no record of.
type IssueFacts struct {
	Recorded       bool
	DispatchStatus string
	Phase          string
	Repository     ghrepo.Repository
	PullRequest    *PullRequestFacts
}

// PullRequestFacts is the daemon's latest observation of a pull request: its number, its state
// (open, merged, closed) and its head commit.
type PullRequestFacts struct {
	Number int
	State  string
	Head   string
}

// Issues reads an issue's facts from the daemon's record.
type Issues interface {
	Facts(ctx context.Context, issue string) (IssueFacts, error)
}

// Tool is one answer's gh tool (*ghtool.Answer, with ghtool.Lookup and its Close).
type Tool interface {
	answer.GH
	Lookup(ctx context.Context, seq int) (ghtool.Found, error)
	Close() error
}

// Tools opens the gh tool an answer runs with, as agent, for question.
type Tools interface {
	Open(ctx context.Context, agent ghtool.Agent, question string) (Tool, error)
}

// PassOn tells the controller of a question the agent's reply does not settle: one not answered, a
// request for work, one past a bound or a fault, or over the daily limit. reason is the Reason,
// asker who asked, ref where (a dispatch:// reference, or the Envoy envelope). It is called once
// the reply is posted, and again only when a restart finds the question not yet marked passed on,
// so an implementation passes a question once by its id.
type PassOn interface {
	PassOn(ctx context.Context, question store.AnswerQuestion, reason Reason, asker, ref string) error
}

// Decision is a person's decision on an ask the stopped agent opened: the ask's answer, or a
// person's reply that hands the turn back to the agent.
type Decision struct {
	Handoff      intake.Handoff
	Event        intake.DispatchEvent
	Conversation store.AgentConversation
}

// Decisions takes the decisions the hand-offs carry (plan 2.9 routes them).
type Decisions interface {
	Decide(ctx context.Context, d Decision) error
}

// Reason is why a question is passed on: the kind of reply the agent gave, a bound or fault the
// answer met (answer.Fault), or what the daemon itself found.
type Reason string

const (
	ReasonNotAnswered    Reason = "not_answered"
	ReasonNeedsWork      Reason = "needs_work"
	ReasonOverLimit      Reason = "over_limit"
	ReasonNoConversation Reason = "no_conversation"
	ReasonUnknownFormat  Reason = Reason(answer.UnknownFormat)
	ReasonNoProvider     Reason = Reason(answer.NoProvider)
	ReasonNoPrice        Reason = Reason(answer.NoPrice)
	// ReasonPostRefused is a reply Dispatch or the listener refused for good.
	ReasonPostRefused Reason = "post_refused"
)

// Config is the loop's configuration and collaborators.
type Config struct {
	// Project is the Legion project token (claim.ProjectToken) the loop answers for.
	Project string
	// Answers is `answers`: providers, prices and bounds.
	Answers       config.Answers
	Store         Store
	Dispatch      Dispatch
	Envoy         Envoy
	Gate          Gate
	Issues        Issues
	Tools         Tools
	Conversations Conversations
	PassOn        PassOn
	Decisions     Decisions
	// AppID is the configured id of a role's GitHub App (config.GitHubApps), which the gh tool's
	// log line names.
	AppID func(claim.Role) string
	// Answer generates an answer: answer.Answer.
	Answer func(ctx context.Context, req answer.Request) (answer.Reply, error)
	Now    func() time.Time
	Log    *slog.Logger
	// Sweep is how often the loop looks again at the questions it holds: a held forward, and a
	// question an error left (30 s when zero).
	Sweep time.Duration
}

// Loop is the daemon's question pipeline.
type Loop struct {
	cfg  Config
	wake chan struct{}

	mu      sync.Mutex
	running map[string]bool
	// waiting is whether the last schedule left a conversation for want of a free worker.
	waiting bool
	workers sync.WaitGroup
}

// New is the loop over cfg.
func New(cfg Config) *Loop {
	if cfg.Sweep <= 0 {
		cfg.Sweep = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Loop{cfg: cfg, wake: make(chan struct{}, 1), running: map[string]bool{}}
}

// Run works the questions until ctx ends, then waits for the answers in flight to end.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Sweep)
	defer ticker.Stop()
	defer l.workers.Wait()
	for {
		l.schedule(ctx)
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-ticker.C:
		}
	}
}

func (l *Loop) poke() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// schedule starts a worker for each conversation with open questions that has none, oldest
// question first, while fewer than the bound run. A conversation the bound leaves waiting is
// scheduled as soon as a worker ends.
func (l *Loop) schedule(ctx context.Context) {
	open, err := l.cfg.Store.OpenQuestions(ctx, l.cfg.Project)
	if err != nil {
		if ctx.Err() == nil {
			l.cfg.Log.Error("answers: could not read the open questions; trying again at the next sweep", "error", err)
		}
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waiting = false
	for _, q := range open {
		if l.running[q.Session] {
			continue
		}
		if len(l.running) >= max(l.cfg.Answers.Bounds.Concurrent, 1) {
			l.waiting = true
			return
		}
		l.running[q.Session] = true
		l.workers.Go(func() {
			defer func() {
				l.mu.Lock()
				delete(l.running, q.Session)
				waiting := l.waiting
				l.mu.Unlock()
				if waiting {
					l.poke()
				}
			}()
			l.work(ctx, q.Session)
		})
	}
}

// work runs session's open questions one after another, in the order they were received, until
// none is left to work now: a held forward is tried once a pass, and an error ends the pass, which
// the next sweep tries again.
func (l *Loop) work(ctx context.Context, session string) {
	tried := map[string]store.QuestionState{}
	for ctx.Err() == nil {
		open, err := l.cfg.Store.OpenQuestions(ctx, l.cfg.Project)
		if err != nil {
			l.cfg.Log.Error("answers: could not read the open questions; trying again at the next sweep", "session", session, "error", err)
			return
		}
		next := slices.IndexFunc(open, func(q store.AnswerQuestion) bool {
			state, seen := tried[q.ID]
			return q.Session == session && (!seen || state != q.State)
		})
		if next < 0 {
			return
		}
		q := open[next]
		tried[q.ID] = q.State
		if err := l.process(ctx, q); err != nil {
			if ctx.Err() == nil {
				l.cfg.Log.Error("answers: question not finished; trying again at the next sweep", "question", q.ID, "issue", q.Issue,
					"role", q.Role, "session", q.Session, "state", q.State, "error", err)
			}
			return
		}
	}
}

// Receive is the hand-off consumer's handler (intake.HandoffHandler): it finds the conversation the
// hand-off is for in the daemon's history, classifies it, records a question, and wakes the loop. A
// hand-off for a session the history does not hold, and one that is no question, is logged and
// dropped; nil acknowledges it. An error leaves it to be delivered again.
func (l *Loop) Receive(ctx context.Context, h intake.Handoff, raw []byte) error {
	conv, ok, err := l.conversationFor(ctx, h)
	if err != nil {
		return err
	}
	if !ok {
		l.cfg.Log.Warn("answers: hand-off dropped: the daemon's history holds no such conversation", "event", h.Envelope.EventID,
			"targetSession", h.TargetSession, "roleToken", h.RoleToken, "topic", h.OriginalTopic)
		return nil
	}
	verdict, err := Classify(ctx, h, conv, l.cfg.Dispatch)
	if err != nil {
		return err
	}
	if verdict.Decision != nil {
		if err := l.cfg.Decisions.Decide(ctx, *verdict.Decision); err != nil {
			return fmt.Errorf("record the decision of %s: %w", h.Envelope.EventID, err)
		}
	}
	if verdict.Question == nil {
		l.cfg.Log.Info("answers: hand-off is no question; not answered", "event", h.Envelope.EventID, "issue", conv.Issue,
			"role", conv.Role, "session", conv.Session, "because", verdict.Because)
		return nil
	}
	origin := verdict.Question
	now := l.cfg.Now().UTC()
	q, inserted, err := l.cfg.Store.ReceiveQuestion(ctx, store.AnswerQuestion{
		ID: QuestionID(l.cfg.Project, origin.Identity, conv.Session), Project: l.cfg.Project, Identity: origin.Identity,
		Session: conv.Session, Token: conv.Token, Issue: conv.Issue, Role: conv.Role, Kind: string(origin.Kind),
		MayAskNothing: origin.MayAskNothing, Handoff: raw, ReceivedAt: now,
	})
	if err != nil {
		return err
	}
	if !inserted {
		l.cfg.Log.Info("answers: question handed off again; not answered again", "question", q.ID, "issue", q.Issue, "role", q.Role,
			"session", q.Session, "state", q.State, "deliveries", q.Deliveries)
		return nil
	}
	l.logState(q, "")
	l.poke()
	return nil
}

// conversationFor is the conversation a hand-off is answered from: for a message to a role, the
// role's newest conversation; for one to a session, that session's, an older session of a role a
// newer one replaced included. A conversation of another project is none of this daemon's.
func (l *Loop) conversationFor(ctx context.Context, h intake.Handoff) (store.AgentConversation, bool, error) {
	var conv store.AgentConversation
	if strings.HasPrefix(h.OriginalTopic, notify.RoleTopicPrefix) {
		role, err := l.cfg.Store.RoleConversations(ctx, claim.Token(strings.TrimPrefix(h.OriginalTopic, notify.RoleTopicPrefix)))
		if err != nil {
			return store.AgentConversation{}, false, err
		}
		if len(role) == 0 {
			return store.AgentConversation{}, false, nil
		}
		conv = role[0]
	} else {
		found, ok, err := l.cfg.Store.AgentConversation(ctx, h.TargetSession)
		if err != nil || !ok {
			return store.AgentConversation{}, false, err
		}
		conv = found
	}
	return conv, conv.Project == l.cfg.Project, nil
}

// logState is a question's one line per transition.
func (l *Loop) logState(q store.AnswerQuestion, reason Reason) {
	attrs := []any{"question", q.ID, "issue", q.Issue, "role", q.Role, "session", q.Session, "state", q.State, "kind", q.Kind}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	l.cfg.Log.Info("answers: question", attrs...)
}

// move writes q in its new state, from one of from, and logs the transition.
func (l *Loop) move(ctx context.Context, q *store.AnswerQuestion, to store.QuestionState, from ...store.QuestionState) error {
	next := *q
	next.State, next.UpdatedAt = to, l.cfg.Now().UTC()
	if err := l.cfg.Store.UpdateQuestion(ctx, next, from...); err != nil {
		return err
	}
	*q = next
	l.logState(next, Reason(next.PassReason))
	return nil
}
