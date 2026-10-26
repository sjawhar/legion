package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
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
		PassOn:        loggedPassOn{w.log}, Decisions: loggedDecisions{w.log},
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

// loggedPassOn logs a question passed on. The controller's agent-question notice and `legion
// state`'s listing are the next change's (plan 2.8, "Passing on"), which replaces this.
type loggedPassOn struct{ log *slog.Logger }

func (p loggedPassOn) PassOn(_ context.Context, q store.AnswerQuestion, reason answerloop.Reason, asker, ref string) error {
	p.log.Warn("answers: question passed on", "question", q.ID, "issue", q.Issue, "role", q.Role, "session", q.Session,
		"reason", reason, "asker", asker, "ref", ref)
	return nil
}

// loggedDecisions records a decision in the log. Routing it to the architect or the controller is
// plan 2.9's, which replaces this.
type loggedDecisions struct{ log *slog.Logger }

func (d loggedDecisions) Decide(_ context.Context, decision answerloop.Decision) error {
	d.log.Info("answers: decision recorded", "event", decision.Handoff.Envelope.EventID, "type", decision.Event.Type,
		"issue", decision.Conversation.Issue, "role", decision.Conversation.Role, "session", decision.Conversation.Session)
	return nil
}
