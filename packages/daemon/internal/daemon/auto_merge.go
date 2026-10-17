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
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// autoMergeTimeout bounds one auto_merge_disable or ready_merge row's GitHub calls, its token's
// mint with them: the row runs on its issue's lifecycle lane, so one unanswered call would hold
// every later lifecycle row of the issue behind it.
const autoMergeTimeout = 30 * time.Second

// autoMergeDisable disarms pull request payload.Number as the implement App, which submits READY's
// merge (ghautomerge.Disarm), deciding by the record when it runs, never by when the row was queued:
// while the issue awaits its merge, the merger's READY commit is the head whose arm is kept, and
// any other head is disarmed. A row queued by a withdrawal the issue has since come back from so
// leaves the READY that followed armed. The record is read after GitHub, so a READY recorded while
// the row runs keeps the arm its ready_merge row made.
func (r *outbox) autoMergeDisable(ctx context.Context, row record.OutboxRow, payload record.AutoMergeDisable) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, err := r.graphQL(ctx)
	if err != nil {
		return err
	}
	keep := func(ctx context.Context) (string, error) {
		head, _, err := r.awaitedREADY(ctx, row.Issue)
		return head, err
	}
	disarmed, err := ghautomerge.Disarm(ctx, github, r.repo, payload.Number, keep)
	if err != nil {
		return err
	}
	r.log.Info("outbox disarmed a pull request", "row", row.ID, "issue", row.Issue, "number", payload.Number,
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
// end it too, the architect told (permanentRefusal, finishRefused); every other failure retries.
func (r *outbox) readyMerge(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge) error {
	ctx, cancel := context.WithTimeout(ctx, autoMergeTimeout)
	defer cancel()
	github, err := r.graphQL(ctx)
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
	outcome, err := ghautomerge.Submit(ctx, github, r.repo, payload.Number, payload.Head, payload.Armed, keep)
	var armed *ghautomerge.Armed
	var failed *ghautomerge.MergeFailed
	switch {
	case errors.As(err, &armed) && !payload.Armed:
		if remembered := r.rememberArm(ctx, row, payload); remembered != nil {
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
		return err
	}
	r.log.Info("outbox submitted a READY merge", "row", row.ID, "issue", row.Issue, "number", payload.Number, "head", payload.Head, "outcome", outcome)
	return nil
}

// rememberArm records on row that its READY's head has been seen armed or queued on GitHub
// (record.ReadyMerge.Armed), while row's lease holds it: a reopen's queue that took the row since
// starts it over unarmed, and its own payload stands.
func (r *outbox) rememberArm(ctx context.Context, row record.OutboxRow, payload record.ReadyMerge) error {
	payload.Armed = true
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := r.records.ReplaceOutboxPayload(ctx, tx, row.ID, row.LeaseToken, payload)
		return err
	}); err != nil {
		return fmt.Errorf("record that READY's head is armed on outbox row %d: %w", row.ID, err)
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

// graphQL is a GitHub GraphQL client carrying the implement App's token, the App that writes the
// issue branch and submits or disarms its pull request's merge.
func (r *outbox) graphQL(ctx context.Context) (githubrest.Client, error) {
	lease, err := r.tokens.Token(ctx, appauth.Implement, r.repo.Owner())
	if err != nil {
		return githubrest.Client{}, fmt.Errorf("mint implement App token: %w", err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.GraphQLAPI(r.githubAPI)}, nil
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
