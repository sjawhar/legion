package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// IssueAskRepairInterval is how often RunIssueAskRepair repairs the asks of closed and reopened
// issues after the boot's own pass. Old and new processes share one database during a roll, so a
// process older than the withdrawal can close an issue after this one's boot pass, leaving its
// asks open, or reopen one without restoring its withdrawn asks: each interval converges them.
const IssueAskRepairInterval = 10 * time.Minute

// issueAskRepairTimeout bounds one interval pass. Each issue commits on its own, so a pass cut
// short keeps what it repaired, and the next one goes on from what is left.
const issueAskRepairTimeout = 5 * time.Minute

// issueAskRepairLease names the lease one interval pass runs under. Two passes running at once
// are correct - they serialise on each issue's row, and the second finds nothing left - but they
// would read the same documents for the same result, which is reason enough for one process to
// run it each interval.
const issueAskRepairLease = "issue-ask-repair"

// IssueAskRepair counts what one RepairIssueAsks pass changed: the asks it withdrew on closed
// issues, the asks it restored on reopened ones, and the readable ask blocks with no ask row its
// sweeps of closed issues' documents marked resolved.
type IssueAskRepair struct {
	Withdrawn int
	Restored  int
	Swept     int
}

// Changed reports whether the pass changed anything.
func (r IssueAskRepair) Changed() bool {
	return r.Withdrawn > 0 || r.Restored > 0 || r.Swept > 0
}

// LogArgs are the counts as slog key-value pairs.
func (r IssueAskRepair) LogArgs() []any {
	return []any{"withdrawn", r.Withdrawn, "restored", r.Restored, "swept", r.Swept}
}

func (r *IssueAskRepair) add(other IssueAskRepair) {
	r.Withdrawn += other.Withdrawn
	r.Restored += other.Restored
	r.Swept += other.Swept
}

// RepairIssueAsks brings every issue's asks to what its last close or reopen leaves them, for the
// issues a process older than the withdrawal closed or reopened:
//   - a closed issue holding open asks gets them withdrawn exactly as a close withdraws them
//     (unindexed blocks marked resolved too), attributed to the actor of its latest issue.closed
//     event, every field as the event stores it;
//   - an open issue holding withdrawn asks gets them restored exactly as a reopen restores them,
//     attributed to the actor of the event that reopened it last: the first issue.updated after its
//     latest issue.closed whose status is not done;
//   - a closed issue one of whose documents owes a settlement marked after markedSince (any, when
//     it is nil) - a write from another process's room, which SetIssueClosed never reached, leaves
//     one, and no settlement runs while the issue is closed - gets the close's final sweep: its
//     withdrawn asks' blocks and copies are written again from their rows, and every readable ask
//     block with no ask row and no source is marked resolved, each one logged, attributed to its
//     latest closer.
//
// An issue with no event naming the closer or the reopener is logged and left as it is: no actor
// is invented. Every record is dated when the pass writes it. Each issue is repaired in a
// transaction of its own that holds the issue's row and reads its state and asks again under it,
// so two passes running at once serialise on the row and the second finds nothing left; a pass
// that finds nothing writes nothing and appends no event. One issue's failure is logged and the
// pass goes on to the next; the returned error names how many failed. It reads deps.Store,
// deps.Docs and deps.Events.
func RepairIssueAsks(ctx context.Context, deps Deps, markedSince *time.Time) (IssueAskRepair, error) {
	s := &server{deps: deps}
	repair, _, err := s.repairIssueAsks(ctx, markedSince)
	return repair, err
}

// RunIssueAskRepair runs RepairIssueAsks every IssueAskRepairInterval until ctx ends, each pass
// under issueAskRepairLease for one interval, so one process repairs per interval however many
// serve the database; leases is required, as it is of every leased loop. A pass that fails or runs
// out of time is logged, and the next interval's pass repairs what it left. The boot runs its own
// pass before this starts (cmd/dispatch).
//
// The sweeps are bounded the same way: a closed issue's document keeps owing the settlement its
// close's own write left for as long as the issue stays closed, so a pass that swept every such
// document would read every closed issue's documents each interval, for good. After its first
// pass, which sweeps them all, each pass sweeps only the documents marked owing since the last
// pass that finished began, less one interval, so a write whose transaction began before that
// pass and committed after it is still swept; a pass that fails moves nothing, so the next one
// covers what it left.
func RunIssueAskRepair(ctx context.Context, deps Deps, leases duty.Leases) {
	s := &server{deps: deps}
	var markedSince *time.Time
	ticker := time.NewTicker(IssueAskRepairInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			duty.Exclusive(ctx, leases, issueAskRepairLease, IssueAskRepairInterval, func(ctx context.Context) {
				if began, finished := s.repairIssueAsksOnTick(ctx, markedSince); finished {
					since := began.Add(-IssueAskRepairInterval)
					markedSince = &since
				}
			})
		}
	}
}

// repairIssueAsksOnTick is one interval pass of RunIssueAskRepair, bounded by
// issueAskRepairTimeout, logging what it changed and why it failed. It answers when the pass
// began, by the database's clock, and whether it finished.
func (s *server) repairIssueAsksOnTick(ctx context.Context, markedSince *time.Time) (time.Time, bool) {
	passCtx, cancel := context.WithTimeout(ctx, issueAskRepairTimeout)
	defer cancel()
	repair, began, err := s.repairIssueAsks(passCtx, markedSince)
	if repair.Changed() {
		slog.Info("dispatch: repaired the asks of closed and reopened issues", repair.LogArgs()...)
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("dispatch: repair the asks of closed and reopened issues", append(repair.LogArgs(), "error", err)...)
	}
	return began, err == nil
}

// issueAskRepairCandidates lists the issues a repair may have work on, read without locks: each
// is decided again under its row. The withdrawals and restorations come first, so a pass cut
// short by its time spends it on them rather than on the sweeps. A closed issue is swept when one
// of its documents owes a settlement marked after $1, or at all when $1 is null. It also answers
// the database's clock as the list was read.
const issueAskRepairCandidates = `
	select statement_timestamp(), array(
		select key from (
			select i.key, 0 as pass from issues i
			where i.closed_at is not null
				and exists (select 1 from asks a where a.issue_key = i.key and a.state = 'open')
			union all
			select i.key, 0 from issues i
			where i.closed_at is null
				and exists (
					select 1 from asks a
					where a.issue_key = i.key and a.state = 'resolved' and a.resolution->>'kind' = '` + withdrawnResolution + `'
				)
			union all
			select i.key, 1 from issues i
			where i.closed_at is not null
				and exists (
					select 1 from artifacts d join doc_settlements_pending p on p.artifact_id = d.id
					where d.issue_key = i.key and d.kind = 'doc' and ($1::timestamptz is null or p.marked_at > $1)
				)
		) candidates
		group by key
		order by min(pass), key
	)
`

// repairIssueAsks is RepairIssueAsks, sweeping only the closed issues whose documents were marked
// owing a settlement after markedSince, or every one when it is nil; it answers when it began.
func (s *server) repairIssueAsks(ctx context.Context, markedSince *time.Time) (IssueAskRepair, time.Time, error) {
	var total IssueAskRepair
	var began time.Time
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return total, began, errors.New("repair issue asks: database unavailable")
	}
	ctx = store.WithTransactionTracking(ctx)
	var issues []string
	if err := s.deps.Store.Pool.QueryRow(ctx, issueAskRepairCandidates, markedSince).Scan(&began, &issues); err != nil {
		return total, began, fmt.Errorf("list the issues whose asks need repair: %w", err)
	}
	var failures []error
	for _, issueKey := range issues {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		repair, err := s.repairIssueAsksOf(ctx, issueKey)
		if err != nil {
			slog.Error("dispatch: repair the asks of an issue", "issue", issueKey, "error", err)
			failures = append(failures, fmt.Errorf("%s: %w", issueKey, err))
			continue
		}
		total.add(repair)
	}
	if len(failures) > 0 {
		return total, began, fmt.Errorf("repair the asks of %d of %d issues: %w", len(failures), len(issues), errors.Join(failures...))
	}
	return total, began, nil
}

// repairIssueAsksOf repairs issueKey's asks (RepairIssueAsks) in a transaction of its own, which
// it commits through the document ledger before publishing the events it appended.
func (s *server) repairIssueAsksOf(ctx context.Context, issueKey string) (IssueAskRepair, error) {
	var repair IssueAskRepair
	tx, err := s.begin(ctx)
	if err != nil {
		return repair, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	var closed, owesSettlement bool
	var open, withdrawn int
	if err := tx.QueryRow(ctx, `select closed_at is not null from issues where key = $1 for no key update`, issueKey).Scan(&closed); err != nil {
		return repair, fmt.Errorf("lock the issue: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		select
			count(*) filter (where a.state = 'open'),
			count(*) filter (where a.state = 'resolved' and a.resolution->>'kind' = '`+withdrawnResolution+`'),
			exists (
				select 1 from artifacts d join doc_settlements_pending p on p.artifact_id = d.id
				where d.issue_key = $1 and d.kind = 'doc'
			)
		from asks a where a.issue_key = $1
	`, issueKey).Scan(&open, &withdrawn, &owesSettlement); err != nil {
		return repair, fmt.Errorf("read the issue's asks: %w", err)
	}
	var events []model.Event
	switch {
	case closed && (open > 0 || owesSettlement):
		closer, found, err := latestIssueEventActor(ctx, tx, `
			select actor from events where issue_key = $1 and type = 'issue.closed' order by seq desc limit 1
		`, issueKey)
		if err != nil {
			return repair, fmt.Errorf("read the issue's closer: %w", err)
		}
		if !found {
			slog.Warn("dispatch: leave the asks of a closed issue with no issue.closed event as they are",
				"issue", issueKey, "open_asks", open, "owes_settlement", owesSettlement)
			return repair, nil
		}
		if owesSettlement {
			// The close's final sweep (sweepClosedIssueAsks), from the withdrawn asks' rows as they
			// stand under the issue's row. Every document write comes before the first event.
			current, err := s.readIssueAsks(documentCtx, tx, `
				select `+askRowColumns+`
				`+askRowFrom+`
				where a.issue_key = $1 and a.state = 'resolved' and a.resolution->>'kind' = '`+withdrawnResolution+`'
				order by a.id
			`, issueKey)
			if err != nil {
				return repair, err
			}
			unindexed, err := s.deps.Docs.WriteIssueAskStates(documentCtx, issueKey, current, closer, true)
			if err != nil {
				return repair, err
			}
			logUnindexedAskBlocks(issueKey, unindexed)
			repair.Swept = len(unindexed)
		}
		if open > 0 {
			var asks []model.Ask
			asks, events, err = s.withdrawIssueAsksTx(documentCtx, tx, issueKey, closer, true)
			if err != nil {
				return repair, err
			}
			repair.Withdrawn = len(asks)
		}
	case !closed && withdrawn > 0:
		reopener, found, err := latestIssueEventActor(ctx, tx, `
			select e.actor from events e
			where e.issue_key = $1 and e.type = 'issue.updated' and e.payload->>'status' is distinct from 'done'
				and e.seq > (select max(c.seq) from events c where c.issue_key = $1 and c.type = 'issue.closed')
			order by e.seq limit 1
		`, issueKey)
		if err != nil {
			return repair, fmt.Errorf("read the issue's reopener: %w", err)
		}
		if !found {
			slog.Warn("dispatch: leave the withdrawn asks of an open issue with no event reopening it as they are",
				"issue", issueKey, "withdrawn_asks", withdrawn)
			return repair, nil
		}
		var asks []model.Ask
		asks, events, err = s.restoreIssueAsksTx(documentCtx, tx, issueKey, reopener)
		if err != nil {
			return repair, err
		}
		repair.Restored = len(asks)
	default:
		return repair, nil
	}
	if err := ledger.Commit(ctx); err != nil {
		return IssueAskRepair{}, err
	}
	s.publish(events...)
	if repair.Changed() {
		slog.Info("dispatch: repaired the asks of an issue", append([]any{"issue", issueKey}, repair.LogArgs()...)...)
	}
	return repair, nil
}

// latestIssueEventActor reads the actor of the event query selects for issueKey, exactly as the
// event stores it, and whether there is one.
func latestIssueEventActor(ctx context.Context, tx pgx.Tx, query, issueKey string) (model.Actor, bool, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, query, issueKey).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, err
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		return model.Actor{}, false, fmt.Errorf("decode the event's actor: %w", err)
	}
	return actor, true, nil
}
