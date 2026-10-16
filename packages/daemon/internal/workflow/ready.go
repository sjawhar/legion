package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ready tells the human the pull request is ready to merge (tellReadyAudience) and queues the
// merge of READY's head (queueReadyMerge). The packet is the merger's READY completion's summary
// — its first line `READY #<n> at <sha> (approved at <sha>) for <KEY> (<url>)`, then the diff
// summary and the gate facts (the shared merger prompt's step 3) — told as record.ReadyPacket gives
// it: verbatim, or for a READY a person merges with its by-hand note at its head and its `Merge:`
// line replaced, so nobody arms it. The daemon does both, not the merger, so they happen exactly
// when the issue reaches awaiting_merge: on the completion itself, or on the approval that opens a
// gate that refused it.
func (e *Engine) ready(ctx context.Context, tx pgx.Tx, issue record.Issue, merger record.PhaseRow, pr *record.PullRequest) error {
	if err := e.tellReadyAudience(ctx, tx, issue, record.ReadyPacket(merger.Summary, merger.MergeNote)); err != nil {
		return err
	}
	return e.queueReadyMerge(ctx, tx, issue.Key, merger, pr)
}

// queueReadyMerge queues the merge of merger's READY head on issue's open pull request
// (record.ReadyMerge). A READY a person merges (its by-hand note) queues none. The row decides by
// GitHub and the record when it runs, so a READY the issue has left by then submits nothing.
func (e *Engine) queueReadyMerge(ctx context.Context, tx pgx.Tx, issue string, merger record.PhaseRow, pr *record.PullRequest) error {
	if merger.MergeNote != "" || pr == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	return e.enqueue(ctx, tx, issue, record.ReadyMerge{Number: pr.Number, Head: merger.HandoffCommit})
}

// holdReadyToItsHead holds a merger's READY to the pull request's head before anything of it is
// recorded, and records that head. A push can land after the route read the head READY names and
// before this completion commits: accepted, that READY would leave the issue awaiting a merge of a
// head it does not name, and nothing withdraws a READY the issue never reached awaiting_merge to
// hold. The route held the commit to GitHub's head, and passes the record as it read it, head and
// clock (intake.HandoffComplete.RecordedHead, RecordedHeadAt), and GitHub's clock in its read
// (HeadAt): the record follows GitHub only by webhook, so one still exactly that snapshot is
// lagging, not moved, one at the commit has caught up, and one with a clock still before GitHub's
// read applied only pushes that read had passed, so it is behind, not moved (a push between the
// snapshot's head and the commit, its webhook applied while the route checked READY). Any other
// record is a push no earlier than GitHub's read, a record back at the snapshot's head at a later
// clock among them (a force push and its undo), or a legacy record with no clock, and is refused,
// so the merger's READY at the new head is applied. Refused on a record merely lagging or behind,
// the right READY's retry at the same commit would be answered HANDOFF_ALREADY_RECORDED, and the
// issue would stay in merging. GitHub's clock is to the second, so a push in the same second as the
// READY's head, its webhook applied in that window, is refused that way too: counting a tie as
// behind would accept a push made after GitHub's read, which is worse.
//
// A READY it lets through, accepted or held by the design gate, advances the record to its head at
// GitHub's clock in the route's read (intake.HandoffComplete.HeadAt). GitHub showed that head then,
// so a synchronize older than that read, for a head between the lagging one and the READY's or for
// the lagging head itself, delivered late or again, changes nothing (classify.LateLifecycle), rather
// than voiding a held READY or withdrawing an accepted one.
func (e *Engine) holdReadyToItsHead(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, fact intake.HandoffComplete) (intake.Result, error) {
	if pr == nil || pr.State != record.PullRequestOpen {
		return intake.Result{}, nil
	}
	snapshot := pr.HeadSHA == fact.RecordedHead && pr.HeadUpdatedAt.Equal(fact.RecordedHeadAt)
	behind := !pr.HeadUpdatedAt.IsZero() && pr.HeadUpdatedAt.Before(fact.HeadAt)
	if pr.HeadSHA != fact.Commit && !snapshot && !behind {
		return refused("READY_HEAD_MOVED", fmt.Sprintf("pull request #%d of %s now heads at %s, not READY's %s: build a new packet at that head and complete again; this completion changed nothing",
			pr.Number, issue.Key, pr.HeadSHA, fact.Commit)), nil
	}
	// A record already at the commit, at a clock no older than GitHub's read, is what the advance
	// below would write: returning early only saves the write.
	if pr.HeadSHA == fact.Commit && !fact.HeadAt.After(pr.HeadUpdatedAt) {
		return intake.Result{}, nil
	}
	if pr.HeadSHA != fact.Commit {
		*pr = classify.AdvancePullRequestHead(*pr, fact.Commit)
	}
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.HeadAt)
	return intake.Result{}, e.store.PutPullRequest(ctx, tx, *pr)
}

// withdrawReady tells the READY's audience (tellReadyAudience) that the READY no longer stands:
// the head's own CI turned red, it started conflicting with its base, or a push moved it past the
// head READY named (reason, which names the cause; classify.RedWithdrawsReady,
// classify.ConflictWithdrawsReady, withdrawMovedReady), so the issue left awaiting_merge for
// implementing, and a new READY follows once the work comes back through testing and review. It
// queues the disarm of the merge armed for the READY (disarm) before it tells anyone, so no
// withdrawal leaves that merge armed. The architect hears it through its checks-red notice.
func (e *Engine) withdrawReady(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, reason string) error {
	if err := e.disarm(ctx, tx, issue.Key, pr); err != nil {
		return err
	}
	return e.tellReadyAudience(ctx, tx, issue, fmt.Sprintf("READY withdrawn for %s, pull request #%d: %s. Do not merge it: a disarm of its auto-merge and merge-queue entry is queued, the issue is back in implementing, and a new READY will follow.", issue.Key, pr.Number, reason))
}

// headMoved answers a move of pr's head: a push, or a reopen (reopened), which can carry a head
// pushed while the pull request was closed. Arming is legitimate only for an awaiting READY's head,
// so in awaiting_merge the move withdraws a READY whose head it left, and a reopen at that head
// submits its merge again (withdrawMovedReady); in any other phase it disarms whatever an earlier
// READY left armed, a child re-entered from awaiting_merge, say. In merging it also voids a READY
// the design gate holds for another head (voidMovedHeldReady), so the gate's approval never
// advances it.
func (e *Engine) headMoved(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, reopened bool) error {
	switch issue.Phase {
	case phase.AwaitingMerge:
		return e.withdrawMovedReady(ctx, tx, issue, pr, reopened)
	case phase.Merging:
		if err := e.voidMovedHeldReady(ctx, tx, issue, pr); err != nil {
			return err
		}
	}
	return e.disarm(ctx, tx, issue.Key, pr)
}

// voidMovedHeldReady voids the READY the design gate holds in merging (its packet kept on the
// merger's row, which advancePendingReady advances when a human approves the gate) once a push or
// a reopen moves the head past its commit: advanced, it would leave the issue awaiting a merge of
// a head no READY named. The merger's READY is cleared and the architect told READY_HEAD_MOVED,
// to have the merger build a new READY at the new head. The record follows GitHub only by
// webhook, so the move is judged here, when one arrives, and not at the approval, when a record
// that lags would void a READY GitHub showed at its commit. A push landing on the held READY's
// commit voids nothing, and a tree that lingers is held where it stands: its approval advances
// nothing, and re-admission clears the READY with the rest of the run.
func (e *Engine) voidMovedHeldReady(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	merger, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil || merger.Summary == "" || merger.HandoffCommit == pr.HeadSHA {
		return err
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	if err := e.clearHandoff(ctx, tx, issue.Key, claim.RoleMerger); err != nil {
		return err
	}
	version := 0
	if issue.ReadyPendingVersion != nil {
		version = *issue.ReadyPendingVersion
		issue.ReadyPendingVersion = nil
		if err := e.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "ready-refused", Role: claim.RoleArchitect, Version: version,
		Reason: fmt.Sprintf("READY_HEAD_MOVED: %s's READY at %s, which the design gate held, is void: a push moved pull request #%d's head to %s, which that READY does not name, so the gate's approval advances nothing; have the merger build a new READY at the new head once the change it carries is ready",
			issue.Key, merger.HandoffCommit, pr.Number, pr.HeadSHA)})
}

// withdrawMovedReady withdraws the READY of an awaiting_merge pull request a push or a reopen moved
// past the head READY named (the merger's completion commit), as a red or a conflicting head withdraws it
// (TriggerChecksRed, withdrawReady): GitHub checks the expected head only when auto-merge is
// enabled, so the arm would otherwise merge a head no review approved. The issue goes back to
// implementing with its implementer started and the architect told, and the new head comes back
// through testing, review and a new READY. A tree that lingers takes no transition, as merged
// leaves it, so its pull request is only disarmed. A push landing on the READY head changes
// nothing. A reopen landing there queues READY's merge again (queueReadyMerge): a row that saw the
// close in the record has ended, and one still waiting absorbs the new one, since the outbox holds
// one ready_merge row per issue and head.
func (e *Engine) withdrawMovedReady(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, reopened bool) error {
	merger, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil {
		return err
	}
	if merger.HandoffCommit != "" && merger.HandoffCommit == pr.HeadSHA {
		if reopened {
			return e.queueReadyMerge(ctx, tx, issue.Key, merger, pr)
		}
		return nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return err
	}
	if lingers {
		return e.disarm(ctx, tx, issue.Key, pr)
	}
	reason := fmt.Sprintf("its head moved from %s, the head READY named, to %s, which no review approved", merger.HandoffCommit, pr.HeadSHA)
	return e.transition(ctx, tx, issue, TriggerChecksRed, "", record.PhaseRow{}, pr, reason)
}

// disarm queues the disarm of pr's merge (record.AutoMergeDisable): arming is legitimate only for
// the READY head of an awaiting_merge pull request, so every head move but one landing on that
// head, every READY withdrawal and every re-entry disarms. The executor decides by the record when
// it runs, so it keeps the arm of a READY the issue has reached since, and it changes nothing on a
// pull request never armed.
func (e *Engine) disarm(ctx context.Context, tx pgx.Tx, issue string, pr *record.PullRequest) error {
	return e.enqueue(ctx, tx, issue, record.AutoMergeDisable{Number: pr.Number})
}

// disarmOpen disarms issue's pull request when it is open (disarm). It is how an issue that leaves
// the workflow, or is re-entered, leaves no merge armed for a READY of the run it left.
func (e *Engine) disarmOpen(ctx context.Context, tx pgx.Tx, issue string) error {
	pr, err := e.store.PullRequest(ctx, tx, issue)
	if err != nil || pr == nil || pr.State != record.PullRequestOpen {
		return err
	}
	return e.disarm(ctx, tx, issue, pr)
}

// tellReadyAudience tells packet to everyone a READY goes to: posted on the Dispatch issue, and
// published to the project's merge queue role when it names one. A READY and its withdrawal both
// go through it, so a withdrawal reaches whoever the READY reached.
func (e *Engine) tellReadyAudience(ctx context.Context, tx pgx.Tx, issue record.Issue, packet string) error {
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: packet}); err != nil {
		return err
	}
	if e.cfg.MergeQueueRole == "" {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.MergeQueuePublish{Role: e.cfg.MergeQueueRole, Packet: packet})
}

// advancePendingReady advances every merger in the tree whose READY was refused while the gate was
// closed, now that a human approved the gate's current version. That version may be later than
// the one the refusal named: the READY stands until the gate reopens, whatever the human revised
// in between. A READY refused while the tree had no design gate at all names no version — the
// issues table takes only a positive `ready_pending_version` — so it is found the same way a
// known-version refusal is found once there is a packet to release: the merger's phase row still
// carries the READY completion's summary. A non-empty summary while in merging can only be a
// refused READY's: handoff()'s READY_REQUIRED guard (engine.go:334) refuses a non-READY merger
// completion before it ever writes the summary (engine.go:347), and clearHandoff empties it again
// on every fresh entry into merging (engine.go:757), so neither leaves one behind for this to
// mistake — even though the issue itself records no pending version for it. A held READY whose
// head a push or a reopen moved was voided then (voidMovedHeldReady), so a packet this advances
// names the head GitHub showed when the route accepted it.
func (e *Engine) advancePendingReady(ctx context.Context, tx pgx.Tx, rootKey string, gate record.DesignGate) error {
	if !classify.DesignGateOpen(gate) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, rootKey); err != nil || lingers {
		return err
	}
	issues, err := e.store.TreeIssues(ctx, tx, rootKey)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Phase != phase.Merging {
			continue
		}
		if issue.ReadyPendingVersion != nil && *issue.ReadyPendingVersion > gate.LatestVersion {
			continue
		}
		row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
		if err != nil {
			return err
		}
		// An empty summary means this merging stint's merger reported no completion yet (there is
		// no refusal to release) or reported one before migration 0016 kept the packet (that READY
		// is void: posting it would leave a message of the outbox marker alone, and a merge queue
		// publish without a packet would fail the whole approval). A pending version distinguishes
		// the two: only the void case names one, since it is set only when a gate exists.
		if row.Summary == "" {
			if issue.ReadyPendingVersion == nil {
				continue
			}
			issue.ReadyPendingVersion = nil
			if err := e.store.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "ready-refused", Role: claim.RoleArchitect, Version: gate.LatestVersion,
				Reason: fmt.Sprintf("READY_PACKET_MISSING: design version %d is approved, but %s's READY was refused before the daemon kept READY packets, so it has no packet to post; that READY is void and %s stays in merging, and whether it is started over (park_child then rerun_child, for a child) or ended is your decision",
					gate.LatestVersion, issue.Key, issue.Key)}); err != nil {
				return err
			}
			continue
		}
		pr, err := e.store.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if err := e.transition(ctx, tx, issue, TriggerReady, "", row, pr, "approved design gate"); err != nil {
			return err
		}
	}
	return nil
}
