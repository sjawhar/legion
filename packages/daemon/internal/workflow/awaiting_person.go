package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// dispatchAskOpened releases a quiet admitted root only for a question its own tree session
// opened. Approval asks are represented by the design gate instead.
func (e *Engine) dispatchAskOpened(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskOpened) (intake.Result, error) {
	if fact.Kind != "question" || fact.AuthorSession == "" {
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	ownsTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.AuthorSession)
	if err != nil || !ownsTree {
		return intake.Result{}, err
	}
	return intake.Result{}, e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask}, fmt.Sprintf("question %s is awaiting an answer", fact.Ask))
}

func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	return intake.Result{}, e.answerAwaitedAsk(ctx, tx, fact.Key, fact.Ask)
}

func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	if fact.WaitingOn != "agent" {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.answerAwaitedAsk(ctx, tx, fact.Key, fact.Ask)
}

func (e *Engine) answerAwaitedAsk(ctx context.Context, tx pgx.Tx, key, ask string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.Ask != ask || issue.Awaiting.AnsweredAt != nil {
		return err
	}
	at := e.now()
	issue.Awaiting.AnsweredAt = &at
	e.logOnCommit(ctx, "workflow: a person answered an architect question", "issue", issue.Key, "ask", ask)
	return e.store.PutIssue(ctx, tx, *issue)
}

// dispatchClaim keeps the newest Dispatch claim separate from LastDispatchSeq: claim events do
// not carry the full issue observation admission records, but their holder fences an awaited root's
// resume.
func (e *Engine) dispatchClaim(ctx context.Context, tx pgx.Tx, fact intake.DispatchClaim) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || fact.Seq <= issue.ClaimSeq {
		return intake.Result{}, err
	}
	issue.ClaimedBy, issue.ClaimSeq = fact.ClaimedBy, fact.Seq
	return intake.Result{}, e.store.PutIssue(ctx, tx, *issue)
}
