package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// withdrawnResolution is the resolution kind an issue's close records on each ask it withdraws.
// Only the server writes it (the resolve route refuses it), so the reopen recognises the close's
// withdrawals by this kind alone.
const withdrawnResolution = "withdrawn"

// withdrawnReason is the reason recorded on an ask the close of issueKey withdrew.
func withdrawnReason(issueKey string) string {
	return fmt.Sprintf("Withdrawn by the close of %s.", issueKey)
}

// settleIssueBeforeClose runs, before a close of issueKey opens its transaction, the settlement
// each document of the issue owes, so every ask block written into them is an ask, credited to its
// author, for the close to withdraw with a record: no settlement runs once the issue is closed. It
// holds no transaction or lock, so it cannot meet the close's. A settlement that stops short is
// run again a few times within a short budget (docs.SettleIssueDocuments): one that met updates
// the room had not stored yet, one a write superseded, and one refused on a room another process's
// append left behind its durable head, whose room is reloaded at the head first, each settle on a
// later attempt. A settlement still short after those is logged and the close proceeds; its blocks
// are not asks yet, and the close's sweep marks them resolved (sweepClosedIssueAsks). An issue
// already closed, or none, settles nothing: its close is refused.
func (s *server) settleIssueBeforeClose(ctx context.Context, issueKey string) {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	var open bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `select closed_at is null from issues where key = $1`, issueKey).Scan(&open); err != nil || !open {
		return
	}
	if err := s.deps.Docs.SettleIssueDocuments(ctx, issueKey); err != nil {
		slog.Warn("dispatch: close an issue whose documents did not all settle first", "issue", issueKey, "error", err)
	}
}

// withdrawIssueAsksTx withdraws every open ask of issueKey, as the issue's close does: each is
// recorded resolved with kind withdrawn, naming actor, the closer, and gets one ask.resolved event
// whose actor is the closer and whose owner is the issue. The blocks showing the asks in the
// issue's documents, and their copies, are written resolved first, while the rows still read open
// (docs.WriteIssueAskStates classifies copies by the rows as they stand), so every document write
// comes before the first event this appends. With markUnindexed, every readable ask block no
// settlement has indexed yet is written resolved too, and each one is logged.
//
// ctx is joined to tx (Docs.Join); the caller holds the issue's row and has locked every event
// owner it appends to (LockOwners) before calling, and commits through the ledger and publishes
// the returned events. It answers the asks as withdrawn, oldest id first.
func (s *server) withdrawIssueAsksTx(ctx context.Context, tx pgx.Tx, issueKey string, actor model.Actor, markUnindexed bool) ([]model.Ask, []model.Event, error) {
	open, err := s.readIssueAsks(ctx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and a.state = 'open'
		order by a.id
		for no key update of a
	`, issueKey)
	if err != nil {
		return nil, nil, err
	}
	// The blocks take the state the rows are about to; WriteIssueAskStates writes nothing from a
	// resolution.
	shown := make([]model.Ask, len(open))
	for index, ask := range open {
		ask.State = "resolved"
		shown[index] = ask
	}
	unindexed, err := s.deps.Docs.WriteIssueAskStates(ctx, issueKey, shown, actor, markUnindexed)
	if err != nil {
		return nil, nil, err
	}
	logUnindexedAskBlocks(issueKey, unindexed)
	reason := withdrawnReason(issueKey)
	withdrawn := make([]model.Ask, 0, len(open))
	events := make([]model.Event, 0, len(open))
	for _, ask := range open {
		ask, err := docs.WriteAskResolution(ctx, tx, ask, withdrawnResolution, reason, actor)
		if err != nil {
			return nil, nil, err
		}
		event, err := s.appendEvent(ctx, tx, issueOwner(issueKey).event(
			"ask.resolved", actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		))
		if err != nil {
			return nil, nil, err
		}
		withdrawn = append(withdrawn, ask)
		events = append(events, event)
	}
	return withdrawn, events, nil
}

// restoreIssueAsksTx opens again every ask of issueKey a close withdrew, as the issue's reopen
// does: each row gets back state open with no answer and no resolution and keeps everything else
// (an approval request its id, approval and hand-back, so its turn is the one it had), then the
// blocks showing them and their copies are written open, now that the rows read open, and each
// gets one ask.opened event whose actor is actor, the reopener, as settlement's own restoration
// of an ask appends.
//
// ctx is joined to tx (Docs.Join); the caller holds the issue's row and has locked every event
// owner it appends to before calling, and commits through the ledger and publishes the returned
// events. It answers the restored asks, oldest id first.
func (s *server) restoreIssueAsksTx(ctx context.Context, tx pgx.Tx, issueKey string, actor model.Actor) ([]model.Ask, []model.Event, error) {
	withdrawn, err := s.readIssueAsks(ctx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and a.state = 'resolved' and a.resolution->>'kind' = '`+withdrawnResolution+`'
		order by a.id
		for no key update of a
	`, issueKey)
	if err != nil {
		return nil, nil, err
	}
	restored := make([]model.Ask, 0, len(withdrawn))
	for _, ask := range withdrawn {
		if _, err := tx.Exec(ctx, `update asks set state = 'open', answer = null, resolution = null where id = $1`, ask.ID); err != nil {
			return nil, nil, fmt.Errorf("restore withdrawn ask: %w", err)
		}
		ask.State = "open"
		ask.Answer = nil
		ask.Resolution = nil
		restored = append(restored, ask)
	}
	if _, err := s.deps.Docs.WriteIssueAskStates(ctx, issueKey, restored, actor, false); err != nil {
		return nil, nil, err
	}
	events := make([]model.Event, 0, len(restored))
	for _, ask := range restored {
		event, err := s.appendEvent(ctx, tx, issueOwner(issueKey).event(
			"ask.opened", actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		))
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	return restored, events, nil
}

// sweepClosedIssueAsks is the close's last pass over its issue's documents, run once the close
// has committed and SetIssueClosed has closed the issue's rooms, when no browser of this process
// can write to them any more. In a transaction of its own, holding the issue's row, it writes the
// blocks of withdrawn - the asks the close withdrew - and their copies again, from their rows as
// they now stand, and writes resolved every readable ask block no settlement has indexed: one a
// browser wrote into the room after the close forked it, or one whose settlement failed before the
// close. Each such block is logged. An issue reopened in the meantime is left alone. The sweep
// never fails the close: an error is logged.
//
// ctx must not be cancelled by the client (context.WithoutCancel), and no transaction is open.
func (s *server) sweepClosedIssueAsks(ctx context.Context, issueKey string, withdrawn []model.Ask, actor model.Actor) {
	if err := s.sweepClosedIssueAsksTx(ctx, issueKey, withdrawn, actor); err != nil {
		slog.Error("dispatch: sweep the ask blocks of a closed issue", "issue", issueKey, "error", err)
	}
}

func (s *server) sweepClosedIssueAsksTx(ctx context.Context, issueKey string, withdrawn []model.Ask, actor model.Actor) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	var closed bool
	if err := tx.QueryRow(ctx, `select closed_at is not null from issues where key = $1 for no key update`, issueKey).Scan(&closed); err != nil {
		return fmt.Errorf("lock the closed issue: %w", err)
	}
	if !closed {
		return nil
	}
	ids := make([]string, len(withdrawn))
	for index, ask := range withdrawn {
		ids[index] = ask.ID
	}
	// The rows as they stand under the issue's row: a reopen, an answer and a second close
	// between the close's commit and this lock leave them other than the close wrote them.
	current, err := s.readIssueAsks(documentCtx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and a.id = any($2::uuid[])
		order by a.id
	`, issueKey, ids)
	if err != nil {
		return err
	}
	unindexed, err := s.deps.Docs.WriteIssueAskStates(documentCtx, issueKey, current, actor, true)
	if err != nil {
		return err
	}
	logUnindexedAskBlocks(issueKey, unindexed)
	return ledger.Commit(ctx)
}

// readIssueAsks reads the asks query selects (askRowColumns from askRowFrom), taking whatever row
// locks it names, with their opening event ids, as the ask.* event payloads carry them.
func (s *server) readIssueAsks(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]model.Ask, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the issue's asks: %w", err)
	}
	asks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Ask, error) {
		return scanAskRow(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read the issue's asks: %w", err)
	}
	pointers := make([]*model.Ask, len(asks))
	for index := range asks {
		pointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, tx, pointers); err != nil {
		return nil, err
	}
	return asks, nil
}

// logUnindexedAskBlocks logs each ask block with no ask row that a write to issueKey's
// documents marked resolved: it is not an ask yet, so it has no record to show, and settlement
// indexes it as a new open ask once the issue reopens.
func logUnindexedAskBlocks(issueKey string, blocks []docs.BlockRef) {
	for _, block := range blocks {
		slog.Warn("dispatch: mark resolved an ask block no settlement indexed before its issue closed",
			"issue", issueKey, "artifact", block.ArtifactID, "block", block.BlockID)
	}
}
