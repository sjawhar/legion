package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// answering is the daemon's answer loop, with `answers` set: the Legion project token it answers
// for, the hand-off consumer connect opens, and the loop attachAnswers builds once supervision runs
// (the gate is supervision's).
type answering struct {
	project  string
	handoffs *intake.Handoffs
	loop     *answerloop.Loop
}

// attachAnswers builds the answer loop over the daemon's store, its Dispatch client (posting in an
// agent's name), its Envoy listener, supervision's answer gate, the issue record, the gh tool and
// the conversation store. Without `answers` it builds nothing.
func (w *workflowRuntime) attachAnswers(cfg config.Config, p plan, s *supervision, st *store.Store) error {
	if w.answers == nil {
		return nil
	}
	client, ok := w.dispatch.(answerloop.Dispatch)
	if !ok {
		return fmt.Errorf("the answer loop: the daemon's Dispatch client (%T) cannot post in an agent's name", w.dispatch)
	}
	legion, err := os.Executable()
	if err != nil {
		return fmt.Errorf("the answer loop: resolve this daemon's executable for the gh tool's wrapper: %w", err)
	}
	host := ghtool.Host{
		GH: p.answers.gh, Git: p.tools["git"], Legion: legion, StateDir: cfg.StateDir,
		Credential: appauth.GitHubCredential(w.tokens, p.repositories.Owners), Calls: st, Log: w.log, Now: time.Now,
	}
	w.answers.loop = answerloop.New(answerloop.Config{
		Project: p.project, Answers: *cfg.Answers, Store: st, Dispatch: client,
		Envoy: notify.New(cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"]), Gate: s.answerGate,
		Issues: recordFacts{w}, Tools: ghTools{host: host, bounds: cfg.Answers.Bounds},
		Conversations: answerloop.Harnesses{OpenSQL: conversationStore(*cfg.Answers, p.answers)},
		PassOn:        controllerPassOn{pool: w.pool, records: w.records, now: time.Now, log: w.log}, Decisions: loggedDecisions{w.log},
		AppID: func(role claim.Role) string {
			if appauth.AppRoleFor(role) == appauth.Implement {
				return cfg.GitHubApps.Implement.AppID
			}
			return cfg.GitHubApps.Review.AppID
		},
		Answer: answer.Answer, Now: time.Now, Log: w.log,
	})
	return nil
}

// recordFacts reads an agent's issue from the daemon's record for the change note: the status
// Dispatch showed, the phase, its pull request as the daemon's GitHub reads last observed it, and
// the repository it works (the record's, or the project's default for an issue it does not record).
type recordFacts struct{ w *workflowRuntime }

func (r recordFacts) Facts(ctx context.Context, key string) (answerloop.IssueFacts, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	if err := pgx.BeginFunc(ctx, r.w.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = r.w.records.Issue(ctx, tx, key); err != nil {
			return err
		}
		pr, err = r.w.records.PullRequest(ctx, tx, key)
		return err
	}); err != nil {
		return answerloop.IssueFacts{}, err
	}
	repo, _, err := r.w.issueRepository(ctx, key)
	if err != nil {
		return answerloop.IssueFacts{}, err
	}
	facts := answerloop.IssueFacts{Recorded: issue != nil, Repository: repo}
	if issue != nil {
		facts.DispatchStatus, facts.Phase = issue.DispatchStatus, string(issue.Phase)
	}
	if pr != nil {
		facts.PullRequest = &answerloop.PullRequestFacts{Number: pr.Number, State: string(pr.State), Head: pr.HeadSHA}
	}
	return facts, nil
}

// ghTools opens the gh tool of one answer (ghtool.Open) under the daemon's host.
type ghTools struct {
	host   ghtool.Host
	bounds config.AnswerBounds
}

func (g ghTools) Open(ctx context.Context, agent ghtool.Agent, question string) (answerloop.Tool, error) {
	a, err := ghtool.Open(ctx, g.host, agent, question)
	if err != nil {
		return nil, err
	}
	return ghTool{answer: a, bounds: g.bounds}, nil
}

// ghTool is one answer's gh tool: its calls (ghtool.Run) and its restart lookup (ghtool.Lookup).
type ghTool struct {
	answer *ghtool.Answer
	bounds config.AnswerBounds
}

func (t ghTool) Run(ctx context.Context, seq int, args string) ghtool.Result {
	return ghtool.Run(ctx, ghtool.Call{Answer: t.answer, Seq: seq, Args: args, Bounds: t.bounds})
}

func (t ghTool) Lookup(ctx context.Context, seq int) (ghtool.Found, error) {
	return ghtool.Lookup(ctx, t.answer, seq, t.bounds)
}

func (t ghTool) Close() error { return t.answer.Close() }

// controllerPassOn passes a question to the controller: in one transaction it marks the question
// passed by its id (processed_events, store.QuestionPassedSource) and queues its agent-question
// notice, a controller notice row the outbox publishes to the controller topic as it does every
// controller notice, so the notice survives a restart, and a restart that finds the question not
// yet marked passed on and calls this again queues nothing more. records wakes the outbox once the
// transaction commits (projectRecords). `legion state` lists the question from the mark
// (questionsForController).
type controllerPassOn struct {
	pool    *pgxpool.Pool
	records record.Store
	now     func() time.Time
	log     *slog.Logger
}

func (p controllerPassOn) PassOn(ctx context.Context, q store.AnswerQuestion, reason answerloop.Reason, origin answerloop.Origin) error {
	row, err := record.NewOutboxRow(q.Issue, record.ControllerNotice{
		Kind: record.AgentQuestionNotice, Role: q.Role, Reason: string(reason),
		Question: &record.AgentQuestion{
			ID: q.ID, Issue: q.Issue, Session: q.Session, Asker: origin.Asker, Ref: origin.Ref,
			Text: record.AgentQuestionText(origin.Text, origin.Ref),
		},
	}, p.now())
	if err != nil {
		return fmt.Errorf("the agent-question notice of %s: %w", q.ID, err)
	}
	queued := false
	// The pass is its own transaction, so it owns the commit hook the row's write registers.
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, p.pool, func(tx pgx.Tx) error {
		fresh, err := p.records.MarkProcessed(scoped, tx, store.QuestionPassedSource, q.ID)
		if err != nil || !fresh {
			return err
		}
		queued = true
		return p.records.Enqueue(scoped, tx, row)
	}); err != nil {
		return fmt.Errorf("pass the question %s to the controller: %w", q.ID, err)
	}
	committed()
	if queued {
		p.log.Info("answers: question passed to the controller", "question", q.ID, "issue", q.Issue, "role", q.Role, "session", q.Session,
			"reason", reason, "asker", origin.Asker, "ref", origin.Ref)
	}
	return nil
}

// questionsForController is the project's questions passed to the controller in the
// api.QuestionsForControllerWindow before now, newest first, at most api.QuestionsForControllerLimit,
// read through tx, each with who asked and where as its recorded hand-off says. A hand-off that no
// longer reads names no asker and no ref; the question is listed all the same.
func questionsForController(ctx context.Context, tx pgx.Tx, st *store.Store, project string, now time.Time) ([]api.QuestionForController, error) {
	passed, err := st.PassedQuestionsTx(ctx, tx, project, now.Add(-api.QuestionsForControllerWindow), api.QuestionsForControllerLimit)
	if err != nil {
		return nil, err
	}
	questions := make([]api.QuestionForController, len(passed))
	for i, p := range passed {
		questions[i] = api.QuestionForController{
			ID: p.ID, Issue: p.Issue, Role: p.Role, Session: p.Session, Reason: p.PassReason, PassedAt: p.PassedAt.UTC(),
		}
		if h, err := intake.DecodeHandoff(p.Handoff); err == nil {
			if origin, err := answerloop.ReadOrigin(h); err == nil {
				questions[i].Asker, questions[i].Ref = origin.Asker, origin.Ref
			}
		}
	}
	return questions, nil
}

// loggedDecisions records a decision in the log. Routing it to the architect or the controller is
// plan 2.9's, which replaces this.
type loggedDecisions struct{ log *slog.Logger }

func (d loggedDecisions) Decide(_ context.Context, decision answerloop.Decision) error {
	d.log.Info("answers: decision recorded", "event", decision.Handoff.Envelope.EventID, "type", decision.Event.Type,
		"issue", decision.Conversation.Issue, "role", decision.Conversation.Role, "session", decision.Conversation.Session)
	return nil
}
