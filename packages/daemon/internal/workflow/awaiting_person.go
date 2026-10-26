package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Unresumable is the one rule for whether a root that returns its admission slot to wait on a
// person may run again in its own architect session: it returns why it may not, or "" when it may.
// Its hand-over label gone, or its leaving the workflow, ends that, and so does a Dispatch claim
// held by a person or by a session outside its tree, which owns the issue instead; the daemon's own
// session and a session that claims within the tree are legitimate holders. The claim is the one
// the daemon's mirror of Dispatch's issues shows (record.Store.DispatchClaim). ReconcileAwaiting
// keeps a root it answers no for at admitted, holding its slot, and admission removes an awaiting
// root, and skips an answered one, that it answers no for.
func Unresumable(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (string, error) {
	switch {
	case !issue.HandedOver:
		return "the legion label was removed", nil
	case issue.Lingers() || !InWorkflow(issue):
		return "it left the workflow", nil
	}
	claimedBy, err := store.DispatchClaim(ctx, tx, issue.Key)
	if err != nil || claimedBy == "" {
		return "", err
	}
	kind, id, found := strings.Cut(claimedBy, ":")
	if !found || kind == "" || id == "" {
		return fmt.Sprintf("its Dispatch claim is held by %q", claimedBy), nil
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
	return fmt.Sprintf("its Dispatch claim is held by %s", claimedBy), nil
}

// armed says whether the design gate is armed: with it off, no approval is asked for.
func (e *Engine) armed() bool { return e.cfg.DesignGate != config.DesignGateOff }

// declare records a hand-back of root's architect to the person at mark (its sequence and
// Dispatch's time of it, where the wait it starts begins), fresh for its ask: one at or below the
// root's last handout belongs to work the daemon already handed out again, and declares nothing.
func (e *Engine) declare(ctx context.Context, tx pgx.Tx, root record.Issue, mark record.EventMark, what string) error {
	if mark.Seq <= root.HandoutSeq {
		e.logOnCommit(ctx, "workflow: a hand-back at or below the root's last handout declares nothing", "issue", root.Key,
			"what", what, "seq", mark.Seq, "handout_seq", root.HandoutSeq)
		return nil
	}
	return e.store.Declare(ctx, tx, root.Key, mark)
}

// personAnswered answers root's wait with a person's act on ask at mark. The architect is the
// tree's only agent while it waits, so a person's act on any ask of the root counts — an act on
// another question, which the architect may have asked in place of the one the wait names, among
// them. The act answers only a wait it follows: one Dispatch recorded at or before the wait's
// start (record.EventMark.After against Awaiting.Since) is an answer the person gave before the
// architect handed the work back to them, and the root waits on their next act.
func (e *Engine) personAnswered(ctx context.Context, tx pgx.Tx, root record.Issue, ask string, mark record.EventMark) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
		return nil
	}
	answer, by := "workflow: a person acted on an ask of a root waiting on them", record.AnsweredByReply
	if root.Awaiting.Ask == ask {
		answer, by = "workflow: an architect ask was answered", record.AnsweredByAsk
	}
	if !placedAfter(root, mark) {
		e.logOnCommit(ctx, "workflow: a person's act recorded before the wait began answers no wait", "issue", root.Key,
			"ask", ask, "act_seq", mark.Seq, "since_seq", root.Awaiting.Since.Seq)
		return nil
	}
	e.logOnCommit(ctx, answer, "issue", root.Key, "ask", ask)
	return e.answerWait(ctx, tx, root, by, ask)
}

// itemLeft applies an act that is nobody's answer and takes an item of root's wait away from the
// person: a close by anyone who is not a person, Dispatch's settlement restoring an earlier
// answer, or a session's reply handing a question back to the agent's turn. before and after are
// that ask's mirror row around the act. Nobody will answer what is no longer with the person, so
// the wait moves to what still is (withHuman), keeping where it began, and is answered — by what
// took the item away, which the resume task names — only once nothing is. An act on an ask the
// root could not wait on, and one that leaves the ask with the person, change no wait.
func (e *Engine) itemLeft(ctx context.Context, tx pgx.Tx, root record.Issue, before, after record.RootAsk, by record.Answer, detail string) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
		return nil
	}
	if !before.ByTree || !before.Question() || !before.WithHuman() || after.WithHuman() {
		return nil
	}
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	if questions := withHuman(asks[root.Key]); len(questions) > 0 {
		if root.Awaiting.Ask != before.Ask {
			return nil
		}
		e.logOnCommit(ctx, "workflow: the question a root waits on left the person, and its wait moves to what is still with them",
			"issue", root.Key, "ask", before.Ask, "next_ask", questions[0].Ask, "left_by", by)
		root.Awaiting.Ask = questions[0].Ask
		return e.store.PutIssue(ctx, tx, root)
	}
	e.logOnCommit(ctx, "workflow: an item of a root's wait left the person, and nothing else is with them", "issue", root.Key,
		"ask", before.Ask, "left_by", by, "detail", detail)
	return e.answerWait(ctx, tx, root, by, detail)
}

// answerWait records that by answered root's wait now, with detail, which puts the root at the
// front of the waiting line (record.Waiting) and names the act in the task its architect resumes
// with (resumeTask). The detail is the act's own: the ask a person acted on, the version and
// reason of the review that answered, or who took the question away, so a later act of the same
// kind does not rename it.
func (e *Engine) answerWait(ctx context.Context, tx pgx.Tx, root record.Issue, by record.Answer, detail string) error {
	at := e.now()
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &at, by, detail
	return e.store.PutIssue(ctx, tx, root)
}
