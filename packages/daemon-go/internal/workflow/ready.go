package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ready tells the human the pull request is ready to merge. The packet is the merger's READY
// completion's summary — its first line `READY #<n> at <sha> (approved at <sha>) for <KEY>
// (<url>)`, then the diff summary and the gate facts (the shared merger prompt's step 4) — posted
// verbatim on the Dispatch issue and, when the project names a merge queue role, published to it.
// The daemon posts it, not the merger, so the READY is told exactly when the issue reaches
// awaiting_merge: on the completion itself, or on the approval that opens a gate that refused it.
func (e *Engine) ready(ctx context.Context, tx pgx.Tx, issue record.Issue, packet string) error {
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
// in between.
func (e *Engine) advancePendingReady(ctx context.Context, tx pgx.Tx, rootKey string, gate record.DesignGate) error {
	if !classify.DesignGateOpen(gate) {
		return nil
	}
	if lingers, err := e.treeLingers(ctx, tx, rootKey); err != nil || lingers {
		return err
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Tree != rootKey || issue.Phase != phase.Merging || issue.ReadyPendingVersion == nil || *issue.ReadyPendingVersion > gate.LatestVersion {
			continue
		}
		row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
		if err != nil {
			return err
		}
		// A READY the gate refused before migration 0016 kept the merger's packet has none: it
		// would post a message of the outbox marker alone, and a merge queue publish without a
		// packet fails the whole approval. That READY cannot tell anyone to merge, so it is void,
		// the issue stays in merging, and the tree's architect is told why.
		if row.Summary == "" {
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
