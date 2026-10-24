package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Unresumable is the one rule for whether a root that returns its admission slot to wait on a
// person may run again in its own architect session: it returns why it may not, or "" when it may.
// Its hand-over label gone, or its leaving the workflow, ends that, and so does a Dispatch claim
// held by a person or by a session outside its tree, which owns the issue instead; the daemon's own
// session and a session that claims within the tree are legitimate holders. awaitPerson refuses to
// release a root it answers no for, and admission removes an awaiting root, and skips an answered
// one, that it answers no for.
func Unresumable(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (string, error) {
	switch {
	case !issue.HandedOver:
		return "the legion label was removed", nil
	case issue.Lingers() || !InWorkflow(issue):
		return "it left the workflow", nil
	case issue.ClaimedBy == "":
		return "", nil
	}
	kind, id, found := strings.Cut(issue.ClaimedBy, ":")
	if !found || kind == "" || id == "" {
		return fmt.Sprintf("its Dispatch claim is held by %q", issue.ClaimedBy), nil
	}
	if kind == "session" && id == dispatch.DaemonSession(issue.Key) {
		return "", nil
	}
	if kind == "session" {
		ownsTree, err := store.SessionClaimsTree(ctx, tx, issue.Tree, id)
		if err != nil || ownsTree {
			return "", err
		}
	}
	return fmt.Sprintf("its Dispatch claim is held by %s", issue.ClaimedBy), nil
}

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
// resume. An issue not yet recorded keeps no claim from here: its hand-over event, the one that
// records it, carries the claim Dispatch shows then (intake.DispatchIssue.ClaimedBy).
func (e *Engine) dispatchClaim(ctx context.Context, tx pgx.Tx, fact intake.DispatchClaim) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || !issue.ObserveClaim(fact.ClaimedBy, fact.Seq) {
		return intake.Result{}, err
	}
	return intake.Result{}, e.store.PutIssue(ctx, tx, *issue)
}
