package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decidedWordingRestore is the statement that gives a decided ask back the wording its decision
// was given on, where a task of an earlier release wrote a document's rewording into the row
// (LEGION-651): an answered or resolved ask with an ask.edited after its latest ask.answered or
// ask.resolved, and no ask.opened between, gets that edit's previous wording - the first such
// edit's - wherever its row asks something else. An answer changed after the edit is a decision on
// the edited wording, so its ask is left as it is, as is a field an older event did not record.
// The statement is idempotent: a row it restored asks its first edit's previous wording, so it
// writes it no more. scoped limits it to the asks $1 names, through their primary key, for a
// settlement; unscoped it reads every decided ask, at boot. Either way it reads events through
// events_ask_payload_id (migration 0030) and returns each ask it restored: its id, its document
// (block_artifact_id, null for an ask no block holds) and the restored wording.
func decidedWordingRestore(scoped bool) string {
	decided := `a.state in ('answered', 'resolved')`
	if scoped {
		decided += ` and a.id = any($1::uuid[])`
	}
	return `
		with decided as (
			select a.id,
				(select max(d.id) from events d
				 where d.type in ('ask.answered', 'ask.resolved') and d.payload->>'id' = a.id::text) as decision
			from asks a
			where ` + decided + `
		),
		first_edit as (
			select distinct on (d.id) d.id, e.payload->'previous' as wording
			from decided d
			join events e on e.type = 'ask.edited' and e.payload->>'id' = d.id::text and e.id > d.decision
			where not exists (
				select 1 from events o
				where o.type = 'ask.opened' and o.payload->>'id' = d.id::text and o.id > d.decision and o.id < e.id)
			order by d.id, e.id
		)
		update asks a
		set question = f.wording->>'question',
			options = coalesce(f.wording->'options', a.options),
			multiple = coalesce((f.wording->>'multiple')::boolean, a.multiple),
			urgency = coalesce(f.wording->>'urgency', a.urgency)
		from first_edit f
		where a.id = f.id
			and f.wording ? 'question'
			and (a.question, a.options, a.multiple, a.urgency) is distinct from
				(f.wording->>'question', coalesce(f.wording->'options', a.options),
				 coalesce((f.wording->>'multiple')::boolean, a.multiple), coalesce(f.wording->>'urgency', a.urgency))
		returning a.id::text, a.block_artifact_id::text, a.question, a.options, a.multiple, a.urgency`
}

// restoredWording is one ask decidedWordingRestore gave back its decided wording.
type restoredWording struct {
	ask      string
	document *string
	wording  model.AskEditPrevious
}

// RestoreDecidedWordings gives every decided ask a task of an earlier release reworded its
// decided wording back (decidedWordingRestore), in one transaction, logging each ask it restored
// once that commits, and returns how many it restored. cmd/dispatch runs it once at boot; a task of
// the earlier release still running during a rolling deploy can reword one again, which the next
// settlement of its document on this release restores (reconcileAskBlocks). Each block is written
// back to the decided wording by its document's next settlement.
func (s *Service) RestoreDecidedWordings(ctx context.Context) (int, error) {
	ctx = store.WithTransactionTracking(ctx)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin restoring decided asks' wordings: %w", err)
	}
	defer tx.Rollback(ctx)
	restored, err := s.restoreWordings(ctx, tx, decidedWordingRestore(false))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit restoring decided asks' wordings: %w", err)
	}
	logRestoredWordings(restored)
	return len(restored), nil
}

// restoreDecidedWordings is RestoreDecidedWordings for the decided asks of rows, the asks of the
// document artifactID that settlement loaded and locked (loadAskBlocks), writing each restored
// wording into rows, so the rest of the settlement weighs the block against the wording the ask was
// decided on and writes that wording back into it.
func (s *Service) restoreDecidedWordings(ctx context.Context, tx pgx.Tx, rows map[string]model.Ask) error {
	var decided []string
	blocks := map[string]string{}
	for blockID, ask := range rows {
		if ask.State != "open" {
			decided = append(decided, ask.ID)
			blocks[ask.ID] = blockID
		}
	}
	if len(decided) == 0 {
		return nil
	}
	restored, err := s.restoreWordings(ctx, tx, decidedWordingRestore(true), decided)
	if err != nil {
		return err
	}
	for _, wording := range restored {
		blockID := blocks[wording.ask]
		ask := rows[blockID]
		ask.Question = wording.wording.Question
		ask.Options = wording.wording.Options
		ask.Multiple = wording.wording.Multiple
		ask.Urgency = wording.wording.Urgency
		rows[blockID] = ask
	}
	logRestoredWordings(restored)
	return nil
}

// restoreWordings runs query, a decidedWordingRestore, and indexes each restored question's
// references again, as every write of an ask's question does (refs.ReplaceCounted); no event
// carries the change, as none does for refs.RebuildAll's.
func (s *Service) restoreWordings(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]restoredWording, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("restore decided asks' wordings: %w", err)
	}
	restored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (restoredWording, error) {
		var wording restoredWording
		var options []byte
		if err := row.Scan(&wording.ask, &wording.document, &wording.wording.Question, &options,
			&wording.wording.Multiple, &wording.wording.Urgency); err != nil {
			return restoredWording{}, err
		}
		if err := json.Unmarshal(options, &wording.wording.Options); err != nil {
			return restoredWording{}, fmt.Errorf("decode the options of ask %s: %w", wording.ask, err)
		}
		return wording, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the restored decided asks: %w", err)
	}
	for _, wording := range restored {
		if _, err := refs.ReplaceCounted(ctx, tx, "ask", wording.ask, wording.wording.Question, s.serverURL); err != nil {
			return nil, fmt.Errorf("index the restored question of ask %s: %w", wording.ask, err)
		}
	}
	return restored, nil
}

// logRestoredWordings logs each restored ask with its document and the question it asks again,
// and nothing of the wording it no longer asks.
func logRestoredWordings(restored []restoredWording) {
	for _, wording := range restored {
		document := ""
		if wording.document != nil {
			document = *wording.document
		}
		slog.Info("dispatch: restored a decided ask's wording",
			"ask", wording.ask, "artifact", document, "question", wording.wording.Question)
	}
}
