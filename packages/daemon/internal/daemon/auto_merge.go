package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// autoMergeTimeout bounds one auto_merge_disable or ready_merge row's GitHub calls, its token's
// mint with them: the row runs on its issue's lifecycle lane, so one unanswered call would hold
// every later lifecycle row of the issue behind it.
const autoMergeTimeout = 30 * time.Second

// mergeWaitWarn is how long a ready_merge row waits before its issue's architect is told once that
// GitHub has not completed the merge Legion submitted (readyMergeWaiting). It runs from
// record.OutboxRow.CreatedAt, the moment the workflow queued the row at READY's acceptance:
// nothing in the daemon records when GitHub was first asked, and created_at survives every retry,
// the payload write that remembers the arm and a reopen's upsert of the same issue and head.
// Measured on sjawhar/legion on 2026-10-23, main requires the checks lint, typecheck, test and
// pr-title; over the last eight main heads those took test 12-16 minutes, lint 3-5, typecheck and
// pr-title under a minute, and across the last 97 completed pull_request runs the median was 2.5
// minutes and the 90th percentile 17, with one 82-minute outlier. An hour is above every required
// check's observed maximum and above the 90th percentile of every run, so a row still waiting then
// waits on something the repository's own checks will not end.
const mergeWaitWarn = time.Hour

// autoMergeDisable disarms pull request payload.Number as the implement App, which submits READY's
// merge (ghautomerge.Disarm), deciding by the record when it runs, never by when the row was queued:
// while the issue awaits its merge, the merger's READY commit is the head whose arm is kept, and
// any other head is disarmed. A row queued by a withdrawal the issue has since come back from so
// leaves the READY that followed armed. The record is read after GitHub, so a READY recorded while
// the row runs keeps the arm its ready_merge row made.
func (r *outbox) autoMergeDisable(ctx context.Context, row record.OutboxRow, payload record.AutoMergeDisable) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, repo, err := r.graphQL(ctx, row.Issue)
	if err != nil {
		return err
	}
	keep := func(ctx context.Context) (string, error) {
		head, _, err := r.awaitedREADY(ctx, row.Issue)
		return head, err
	}
	disarmed, err := ghautomerge.Disarm(ctx, github, repo, payload.Number, keep)
	if err != nil {
		return err
	}
	r.log.Info("outbox disarmed a pull request", "row", row.ID, "issue", row.Issue, "repo", repo.String(), "number", payload.Number,
		"missing", disarmed.Missing, "kept", disarmed.Kept, "autoMergeDisabled", disarmed.AutoMerge, "dequeued", disarmed.Queue)
	return nil
}

// readyMerge submits the merge of an accepted READY's head as the implement App
// (ghautomerge.Submit), deciding by GitHub and then the record when it runs: only while the issue
// still awaits that READY head on an open pull request does it ask GitHub to enqueue, merge or arm
// it, and GitHub's merge state and rules decide which. A repository that allows no auto-merge, or a
// base that requires a merge queue, leaves the row waiting until GitHub reports the pull request
// mergeable, so a merge-queue base never has auto-merge armed; an arm or a queue entry leaves it
// waiting until GitHub reports the merge (ghautomerge.Armed), and a READY the record has left ends
// the row. The first arm or entry the row sees is recorded on the row (rememberArm), so a later run
// that finds it gone reads the removal from GitHub instead of submitting again: the merge queue
// failing the merge sends the issue back to implementing (intake.MergeFailed, applied through the
// workflow), and anyone else's removal ends the row with the architect told (ghautomerge.Removed,
// merge-removed). A refusal a retry would meet again (ghautomerge.Refused) and a disagreement with
// the record that has stood on GitHub longer than ghautomerge.LagBound (ghautomerge.Lagging.Stalled)
// end it too, the architect told (permanentRefusal, finishRefused); every other failure retries. A
// wait GitHub has left standing longer than mergeWaitWarn tells the architect once that the merge
// is still waiting (merge-waiting, rememberWait), and the row waits on.
func (r *outbox) readyMerge(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, repo, err := r.graphQL(ctx, row.Issue)
	if err != nil {
		return err
	}
	// The record ends the row only when it awaits no READY head or has applied a close: a close
	// GitHub shows first leaves the row waiting for the record (ghautomerge.Lagging).
	keep := func(ctx context.Context) (string, error) {
		head, open, err := r.awaitedREADY(ctx, row.Issue)
		if !open {
			head = ""
		}
		return head, err
	}
	outcome, err := ghautomerge.Submit(ctx, github, repo, payload.Number, payload.Head, payload.Armed, keep)
	var armed *ghautomerge.Armed
	var failed *ghautomerge.MergeFailed
	switch {
	case errors.As(err, &armed) && !payload.Armed:
		if remembered := r.rememberWait(ctx, row, payload, true, err); remembered != nil {
			return remembered
		}
		return err
	case errors.As(err, &failed):
		fact := intake.MergeFailed{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Reason: failed.Error()}
		applied, applyErr := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-failed:%s:%s:%s", row.Issue, payload.Head, failed.Event), fact, r.handlers...)
		if applyErr != nil {
			return fmt.Errorf("send %s back to implementing on its failed merge: %w", row.Issue, applyErr)
		}
		// The workflow moves only an issue that still awaits that READY (workflow's mergeFailed), and
		// a fact already applied moves nothing.
		message := "outbox READY merge failed in the merge queue; the workflow sends the issue back to implementing if it still awaits that READY"
		if applied.Duplicate {
			message = "outbox READY merge failed in the merge queue; that failure was already applied, so nothing changed"
		}
		r.log.Info(message, "row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "reason", failed.Error())
		return nil
	case err != nil:
		if remembered := r.rememberWait(ctx, row, payload, false, err); remembered != nil {
			return remembered
		}
		return err
	}
	r.log.Info("outbox submitted a READY merge", "row", row.ID, "issue", row.Issue, "repo", repo.String(), "number", payload.Number, "head", payload.Head, "outcome", outcome)
	return nil
}

// rememberWait records on row what its wait learned that its next run must know, and tells the
// issue's architect of a merge GitHub is taking long over, in one transaction under row's lease, so
// a run writes the payload at most once. arm is GitHub having shown READY's head armed or queued
// (record.ReadyMerge.Armed): from then on the arm disappearing is a removal the row reads from
// GitHub rather than a cue to submit again. A row queued longer ago than mergeWaitWarn that has not
// told its architect yet carries readyMergeWaiting's notice, enqueued in the same transaction as
// the payload that records the telling (record.ReadyMerge.Warned), so the row is never recorded as
// warned without its notice, nor the architect told twice; a cause that ends the row
// (permanentRefusal) warns nobody, since finishRefused tells the architect of that one itself. A
// reopen's queue that took the row since starts it over unarmed and unwarned, and its own payload
// stands: the replacement writes nothing then, and the notice rolls back with it.
func (r *outbox) rememberWait(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge, arm bool, cause error) error {
	now := r.now().UTC()
	waited := now.Sub(row.CreatedAt)
	warn := !payload.Warned && waited > mergeWaitWarn && !permanentRefusal(row, cause, now)
	if !arm && !warn {
		return nil
	}
	payload.Armed = payload.Armed || arm
	payload.Warned = payload.Warned || warn
	var notice record.OutboxRow
	if warn {
		var err error
		if notice, err = record.NewOutboxRow(row.Issue, readyMergeWaiting(payload, waited, cause), now); err != nil {
			return err
		}
	}
	// The notice is written in the payload's own transaction, which owns the commit hook that write
	// registers (projectRecords, intake.OnCommit), so its lane is woken as the payload commits.
	scoped, committed := intake.WithCommitHooks(ctx)
	var replaced bool
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		var err error
		if replaced, err = r.records.ReplaceOutboxPayload(scoped, tx, row.ID, row.LeaseToken, payload); err != nil || !replaced || !warn {
			return err
		}
		return r.records.Enqueue(scoped, tx, notice)
	}); err != nil {
		return fmt.Errorf("record what outbox row %d learned while it waits: %w", row.ID, err)
	}
	committed()
	if warn && replaced {
		r.log.Warn("outbox READY merge is still waiting on GitHub; its architect is told once", "row", row.ID, "issue", row.Issue,
			"number", payload.Number, "head", payload.Head, "waited", waited.Round(time.Minute), "error", cause)
	}
	return nil
}

// readyMergeEnded is the architect's notice of a READY merge row ended without a merge by cause:
// GitHub's refusal, which a retry would meet again (merge-refused), GitHub showing the pull
// request closed or at another head than the record awaits since a change older than
// ghautomerge.LagBound, a webhook the daemon never received (merge-stalled), or a person, or GitHub
// on its own, taking it out of the merge queue or turning its auto-merge off at READY's head, not
// because the merge failed (merge-removed).
func readyMergeEnded(cause error) record.Notice {
	var lagging *ghautomerge.Lagging
	var removed *ghautomerge.Removed
	switch {
	case errors.As(cause, &lagging):
		return record.Notice{Kind: "merge-stalled", Role: claim.RoleArchitect,
			Reason: fmt.Sprintf("GitHub has shown pull request #%d %s at %s since %s, longer than %s, while the record awaits its merge at READY's head %s: the daemon never received that change's webhook, so it stopped submitting READY's merge",
				lagging.Number, lagging.State, lagging.GitHubHead, lagging.Since.UTC().Format(time.RFC3339), ghautomerge.LagBound, lagging.Head)}
	case errors.As(cause, &removed):
		return record.Notice{Kind: "merge-removed", Role: claim.RoleArchitect, Reason: removed.Error()}
	}
	return record.Notice{Kind: "merge-refused", Role: claim.RoleArchitect, Reason: cause.Error()}
}

// readyMergeWaiting is the architect's notice of a merge the daemon submitted that GitHub has not
// completed within mergeWaitWarn of the row's queueing (merge-waiting): the pull request, READY's
// head, how long the row has waited and what GitHub last answered, cause. Nothing has gone wrong
// with the row — the merge has not been withdrawn, the issue stays in awaiting_merge and the
// daemon goes on submitting it — so the architect is told once (rememberWait,
// record.ReadyMerge.Warned) and what is left to find is on GitHub: a required check that never
// ran or failed, a review only a person can give, a merge queue that has not landed it.
func readyMergeWaiting(payload record.ReadyMerge, waited time.Duration, cause error) record.Notice {
	return record.Notice{Kind: "merge-waiting", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("Legion submitted the merge of pull request #%d at READY's head %s %s ago and GitHub has not completed it; GitHub last answered: %s. The merge has not been withdrawn, the issue stays in awaiting_merge and the daemon goes on submitting it: read the pull request on GitHub to find what GitHub is waiting for",
			payload.Number, payload.Head, waited.Round(time.Minute), cause)}
}

// graphQL is a GitHub GraphQL client carrying the implement App's token, the App that writes the
// issue branch and submits or disarms its pull request's merge, minted for the owner of the
// repository issue's record names, and that repository.
func (r *outbox) graphQL(ctx context.Context, issue string) (githubrest.Client, ghrepo.Repository, error) {
	recorded, err := r.issue(ctx, issue)
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, err
	}
	repo, err := recorded.Repository()
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, err
	}
	lease, err := r.tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, fmt.Errorf("mint the implement App token for %s: %w", repo.Owner(), err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.GraphQLAPI(r.githubAPI)}, repo, nil
}

// awaitedREADY is the READY head issue awaits the merge of, and whether the record holds its pull
// request open. The head is the merger's READY commit while the issue awaits its merge, and none in
// any other phase, where no arm is legitimate, nor for a READY a person merges (its by-hand note,
// record.PhaseRow.MergeNote), which covers no arm, nor in a tree that lingers after its root left the
// workflow, whose members beginLinger disarms. A disarm keeps the arm of that head; a READY merge
// also needs the pull request open, since a close the record has applied ends the row and the
// close already told the architect.
func (r *outbox) awaitedREADY(ctx context.Context, issue string) (string, bool, error) {
	var head string
	var open bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		current, err := r.records.Issue(ctx, tx, issue)
		if err != nil || current == nil || current.Phase != phase.AwaitingMerge {
			return err
		}
		if lingers, err := record.TreeLingers(ctx, r.records, tx, current.Tree); err != nil || lingers {
			return err
		}
		pr, err := r.records.PullRequest(ctx, tx, issue)
		if err != nil {
			return err
		}
		open = pr != nil && pr.State == record.PullRequestOpen
		phases, err := r.records.Phases(ctx, tx, issue)
		if err != nil {
			return err
		}
		for _, row := range phases {
			if row.Role == claim.RoleMerger && row.MergeNote == "" {
				head = row.HandoffCommit
			}
		}
		return nil
	}); err != nil {
		return "", false, fmt.Errorf("read the READY head %s awaits: %w", issue, err)
	}
	return head, open, nil
}
