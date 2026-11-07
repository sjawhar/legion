package daemon

import (
	"context"
	"errors"
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
		Credential: appauth.GitHubCredential(w.tokens, p.repositories.Owners, cfg.RepositoryRenames), Calls: st, Log: w.log, Now: time.Now,
	}
	w.answers.loop = answerloop.New(answerloop.Config{
		Project: p.project, Answers: *cfg.Answers, Store: st, Dispatch: client,
		Envoy: notify.New(cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"]), Gate: s.answerGate,
		Issues: recordFacts{w}, Tools: ghTools{host: host, bounds: cfg.Answers.Bounds},
		Conversations: answerloop.Harnesses{OpenSQL: conversationStore(*cfg.Answers, p.answers)},
		PassOn:        controllerPassOn{pool: w.pool, records: w.records, now: time.Now, log: w.log},
		Decisions:     routedDecisions{outbox: w.outbox, now: time.Now, log: w.log},
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

// decisionPassedSource is the processed_events source the daemon marks a decision passed on under,
// by the decision's id (answerloop.Decision.ID), in the transaction that queues its agent-decision
// notice: the mark is what passes a decision once, across redeliveries and restarts.
const decisionPassedSource = string(record.AgentDecisionNotice)

// routedDecisions passes on a person's decision on an ask a stopped agent opened (plan 2.9): in one
// transaction it marks the decision passed by its id and queues one agent-decision notice. The
// notice is an architect notice row when the architect that owns the agent's issue holds its role
// (decisionRoute), which the outbox routes to it as it routes every notice; otherwise a
// controller notice row, so the controller hears the decision nobody in the tree will. A redelivery,
// and a restart's, finds the mark and queues nothing more. outbox's records wake the outbox once the
// transaction commits (projectRecords).
//
// A decision the parked-decision relay sends itself is the one this queues nothing for
// (relayOwns): sjawhar/legion#2163's relay tells the controller in its own parked-decision notice
// and keeps the decision for the root's next start, and this loop merges second, so it records the
// decision passed — a redelivery and a restart queue nothing later either — and logs the one line
// that says whose it is. What the same message asks the agent is still answered: only the decision
// is the relay's.
type routedDecisions struct {
	outbox *outbox
	now    func() time.Time
	log    *slog.Logger
}

func (d routedDecisions) Decide(ctx context.Context, decision answerloop.Decision) error {
	conv := decision.Conversation
	notice := record.Notice{Kind: record.AgentDecisionNotice, Role: conv.Role, AgentDecision: &record.AgentDecision{
		ID: decision.ID, Issue: conv.Issue, Session: conv.Session, Asker: decision.Asker, Ref: decision.Ref,
		Text: record.AgentDecisionText(decision.Text, decision.Ref),
	}}
	// to is whom the decision was queued for, empty when an earlier hand-off passed it on or when
	// #2163's relay owns it (relayed).
	to, relayed := "", false
	// The pass is its own transaction, so it owns the commit hook the row's write registers.
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, d.outbox.pool, func(tx pgx.Tx) error {
		to, relayed = "", false
		fresh, err := d.outbox.records.MarkProcessed(scoped, tx, decisionPassedSource, decision.ID)
		if err != nil || !fresh {
			return err
		}
		architect, parked, err := d.decisionRoute(scoped, tx, conv.Issue)
		if err != nil {
			return err
		}
		if parked && relayOwns(decision) {
			relayed = true
			return nil
		}
		var payload record.OutboxPayload = record.ControllerNotice(notice)
		to = "the controller"
		if architect != "" {
			payload, to = notice, string(architect)
		}
		row, err := record.NewOutboxRow(conv.Issue, payload, d.now())
		if err != nil {
			return fmt.Errorf("the agent-decision notice of %s: %w", decision.ID, err)
		}
		return d.outbox.records.Enqueue(scoped, tx, row)
	}); err != nil {
		return fmt.Errorf("pass the decision %s on: %w", decision.ID, err)
	}
	committed()
	switch {
	case relayed:
		d.log.Info("answers: decision left to the parked-decision relay of its parked tree", "decision", decision.ID,
			"issue", conv.Issue, "role", conv.Role, "session", conv.Session, "asker", decision.Asker, "ref", decision.Ref)
	case to != "":
		d.log.Info("answers: decision passed on", "decision", decision.ID, "to", to, "issue", conv.Issue, "role", conv.Role,
			"session", conv.Session, "asker", decision.Asker, "ref", decision.Ref)
	}
	return nil
}

// relayOwns says whether the parked-decision relay sends decision itself once its tree is parked
// (decisionRoute): every reply on an ask's thread, which the relay sends whatever the ask's kind
// (workflow's dispatchAskReply), and a person's answer only of an ask kind it answers for
// (record.RelaysAskAnswer, workflow's dispatchAskClosed). An answer the relay leaves alone — a
// legacy `action` ask's, or one whose event names no kind — is this loop's to pass on, as it is
// on a tree that runs, so no decision reaches nobody.
func relayOwns(decision answerloop.Decision) bool {
	return !decision.Answered || record.RelaysAskAnswer(decision.AskKind)
}

// decisionRoute is where a decision on the agent's issue key goes, read through tx as the outbox
// reads a notice's route (readNoticeRoute, owningArchitect): the architect that owns the issue
// when it holds its role (holdsItsRole) and its tree neither lingers nor has closed, and whether
// the issue stands in a parked tree, whose decisions are #2163's relay's as far as it relays them
// (record.ParkedTree, relayOwns). No architect and no parked tree is the controller's: the tree's
// architect is suspended, being stopped, failed, retired or not supervised, the root lingers on a
// status someone moved back into the workflow, which the relay sends nothing for
// (record.RelayNotParked), or the issue is not recorded in a tree the daemon routes notices of.
func (d routedDecisions) decisionRoute(ctx context.Context, tx pgx.Tx, key string) (claim.Token, bool, error) {
	issue, route, err := d.outbox.readNoticeRoute(ctx, tx, key, map[string]noticeRoute{})
	switch {
	case errors.Is(err, errNoticeUnroutable):
		return "", false, nil
	case err != nil:
		return "", false, err
	case record.ParkedTree(route.root):
		return "", true, nil
	case route.root.Lingers():
		return "", false, nil
	}
	architect, err := owningArchitect(route.project, route.issues, issue, record.AgentDecisionNotice, d.outbox.holdsItsRole)
	if err != nil || !d.outbox.holdsItsRole(architect) {
		return "", false, nil
	}
	return architect, false, nil
}
