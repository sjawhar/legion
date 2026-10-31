package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decidedWordingEdits finds the wording a decided ask's decision was given on, where a task of an
// earlier release wrote a document's rewording into the row (LEGION-651): an answered or resolved
// ask with an ask.edited after its latest ask.answered or ask.resolved, and no ask.opened between,
// gets that edit's previous wording - the first such edit's - wherever its row asks something else
// (decidedWordingDiffers). An answer changed after the edit is a decision on the edited wording, so
// its ask is left as it is, as is a field an older event did not record. $1 names the asks it may
// restore, through their primary key; null, it reads every decided ask, at boot. Either way it
// reads events through events_ask_payload_id (migration 0030).
const decidedWordingEdits = `
	with decided as (
		select a.id,
			(select max(d.id) from events d
			 where d.type in ('ask.answered', 'ask.resolved') and d.payload->>'id' = a.id::text) as decision
		from asks a
		where a.state in ('answered', 'resolved') and ($1::uuid[] is null or a.id = any($1::uuid[]))
	),
	first_edit as (
		select distinct on (d.id) d.id, e.payload->'previous' as wording
		from decided d
		join events e on e.type = 'ask.edited' and e.payload->>'id' = d.id::text and e.id > d.decision
		where not exists (
			select 1 from events o
			where o.type = 'ask.opened' and o.payload->>'id' = d.id::text and o.id > d.decision and o.id < e.id)
		order by d.id, e.id
	)`

// decidedWordingColumns is the wording an ask a gets back from its first edit's previous wording
// f: the question the edit recorded, and options, multiple and urgency, each the row's own where
// the edit recorded none.
const decidedWordingColumns = `
	f.wording->>'question', coalesce(f.wording->'options', a.options),
	coalesce((f.wording->>'multiple')::boolean, a.multiple), coalesce(f.wording->>'urgency', a.urgency)`

// decidedWordingDiffers holds for an ask a whose row asks something other than its first edit's
// previous wording f.
const decidedWordingDiffers = `
	a.id = f.id
	and f.wording ? 'question'
	and (a.question, a.options, a.multiple, a.urgency) is distinct from (` + decidedWordingColumns + `)`

// decidedWordingPlan reads each ask decidedWordingRestore would give back its decided wording,
// writing nothing: its id, its document and block (both null for an ask no block holds) and the
// wording.
const decidedWordingPlan = decidedWordingEdits + `
	select a.id::text, a.block_artifact_id::text, a.block_id, ` + decidedWordingColumns + `
	from asks a join first_edit f on a.id = f.id
	where ` + decidedWordingDiffers + `
	order by a.id`

// decidedWordingRestore is the statement that gives each ask $1 names back its decided wording
// (decidedWordingEdits), returning the ids it restored. It is idempotent: a row it restored asks
// its first edit's previous wording, so it writes it no more.
const decidedWordingRestore = decidedWordingEdits + `
	update asks a
	set (question, options, multiple, urgency) = (` + decidedWordingColumns + `)
	from first_edit f
	where ` + decidedWordingDiffers + `
	returning a.id::text`

// errDecidedWordingsMoved refuses a restoration whose update restored other asks than its plan
// read: a decided ask's row or events changed in between, which only a writer holding no owner
// lock of the asks can do - a task of an earlier release during a rolling deploy, at boot. The
// restoration rolls back; the boot logs it and goes on, and each document's next settlement
// restores its own asks (restoreDocumentDecidedWordings).
var errDecidedWordingsMoved = errors.New("the decided asks to restore changed between their plan and their restoration")

// restoredWording is one ask decidedWordingRestore gave back its decided wording.
type restoredWording struct {
	ask      string
	document *string
	block    *string
	wording  model.AskEditPrevious
}

// decidedWordingRollbackTimeout bounds the rollback of a boot restoration that did not commit,
// which runs on a context of its own: the boot's may have run out.
const decidedWordingRollbackTimeout = 5 * time.Second

// RestoreDecidedWordings gives every decided ask a task of an earlier release reworded its
// decided wording back (decidedWordingRestore) and marks owed, in the same transaction, the
// settlement of each restored block ask's own document and of every document showing a copy of
// one of them (owedSettlementsOfRestored), which it arms once that commits. The own document's
// block still asks the rewording beside the decision, and its settlement writes the decided
// wording back into it (reconcileAskBlocks); a copy settled while its source's row held the
// rewording matched that wording and shows the decision on it, and its next settlement opens the
// block's own ask, as an answer or a resolution of a copy's source settles it. It logs each ask it
// restored once the transaction commits and returns how many asks it restored and how many copies
// it marked owed. cmd/dispatch runs it once at boot; a task of the earlier release still running
// during a rolling deploy can reword one again, which the next settlement of its document on this
// release restores (restoreDocumentDecidedWordings), writing the decided wording back in that same
// settlement. It reads what it restores, plans the questions' references, takes the copy locks and
// reads the owed documents before it writes anything, holding no ask's row; then it restores them,
// writes their references and marks the documents owed back to back, and commits. Its update
// restores exactly the asks it read or the restoration is refused (errDecidedWordingsMoved). A
// settlement takes its own document's owner lock before its copy lock and an answer or a
// resolution its ask's owner lock before SettleCopiesOf, so none of them waits on a lock this
// holds once it holds the copy lock.
func (s *Service) RestoreDecidedWordings(ctx context.Context) (restored, copies int, err error) {
	ctx = store.WithTransactionTracking(ctx)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin restoring decided asks' wordings: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decidedWordingRollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	queued, ledger := queueWrites(ctx, tx)
	wordings, err := s.restoreWordings(queued, tx, nil)
	if err != nil {
		return 0, 0, err
	}
	owed, copies, err := owedSettlementsOfRestored(queued, tx, wordings)
	if err != nil {
		return 0, 0, err
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit restoring decided asks' wordings: %w", err)
	}
	logRestoredWordings(wordings)
	for _, document := range owed {
		s.scheduleSettle(document)
	}
	return len(wordings), copies, nil
}

// owedSettlementsOfRestored marks owed, in one upsert (markSettlementPending), the settlement of
// each document holding the block of an ask of restored and of every document showing a copy of
// one (lockedCopiesOf, which takes the copy locks first), reading each such document's owner, and
// returns them with how many are copies. An ask no block holds has neither.
func owedSettlementsOfRestored(ctx context.Context, tx pgx.Tx, restored []restoredWording) ([]string, int, error) {
	var documents []documentAsks
	index := map[string]int{}
	for _, wording := range restored {
		if wording.document == nil || wording.block == nil {
			continue
		}
		at, found := index[*wording.document]
		if !found {
			owner, err := documentOwner(ctx, tx, *wording.document)
			if err != nil {
				return nil, 0, err
			}
			at = len(documents)
			index[*wording.document] = at
			documents = append(documents, documentAsks{artifactID: *wording.document, owner: owner})
		}
		documents[at].asks = append(documents[at].asks, model.Ask{ID: wording.ask, BlockID: wording.block, BlockArtifactID: wording.document})
	}
	copies, err := lockedCopiesOf(ctx, tx, documents...)
	if err != nil {
		return nil, 0, err
	}
	owed := slices.Clone(copies)
	for _, document := range documents {
		owed = append(owed, document.artifactID)
	}
	if len(owed) == 0 {
		return nil, 0, nil
	}
	slices.Sort(owed)
	owed = slices.Compact(owed)
	if err := markSettlementPending(ctx, tx, owed, nil, false, true, nil, false); err != nil {
		return nil, 0, err
	}
	return owed, len(copies), nil
}

// restoreDocumentDecidedWordings is RestoreDecidedWordings for the decided asks of rows, the asks
// of the document artifactID that settlement loaded and locked (loadAskBlocks), writing each
// restored wording into rows, so the rest of the settlement weighs the block against the wording
// the ask was decided on and writes that wording back into it. It returns the asks it restored, as
// rows now holds them, whose copies the settlement marks owed with the copies of the asks it
// retracts (reconcileAskBlocks).
func (s *Service) restoreDocumentDecidedWordings(ctx context.Context, tx pgx.Tx, rows map[string]model.Ask) ([]model.Ask, error) {
	var decided []string
	blocks := map[string]string{}
	for blockID, ask := range rows {
		if ask.State != "open" {
			decided = append(decided, ask.ID)
			blocks[ask.ID] = blockID
		}
	}
	// decidedWordingPlan reads a null $1 as every decided ask, so an empty list never reaches it.
	if len(decided) == 0 {
		return nil, nil
	}
	restored, err := s.restoreWordings(ctx, tx, decided)
	if err != nil {
		return nil, err
	}
	asks := make([]model.Ask, 0, len(restored))
	for _, wording := range restored {
		blockID := blocks[wording.ask]
		ask := rows[blockID]
		ask.Question = wording.wording.Question
		ask.Options = wording.wording.Options
		ask.Multiple = wording.wording.Multiple
		ask.Urgency = wording.wording.Urgency
		rows[blockID] = ask
		if ask.BlockArtifactID != nil {
			recordAskBlock(ctx, tx, *ask.BlockArtifactID, ask)
		}
		asks = append(asks, ask)
	}
	logRestoredWordings(restored)
	return asks, nil
}

// restoreWordings gives the decided asks ids names, or every decided ask when ids is nil, their
// decided wording back, and indexes each restored question's references again, as every write of
// an ask's question does (refs.ReplaceCounted); no event carries the change, as none does for
// refs.RebuildAll's. Which asks it restores and their references are read and planned where it is
// called (decidedWordingPlan, planReplaceCounted); the restoration and the references go through
// write. The restoration names the planned asks and refuses to have restored any other set
// (errDecidedWordingsMoved).
func (s *Service) restoreWordings(ctx context.Context, tx pgx.Tx, ids []string) ([]restoredWording, error) {
	rows, err := tx.Query(ctx, decidedWordingPlan, ids)
	if err != nil {
		return nil, fmt.Errorf("read the decided asks' wordings to restore: %w", err)
	}
	restored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (restoredWording, error) {
		var wording restoredWording
		var options []byte
		if err := row.Scan(&wording.ask, &wording.document, &wording.block, &wording.wording.Question, &options,
			&wording.wording.Multiple, &wording.wording.Urgency); err != nil {
			return restoredWording{}, err
		}
		if err := json.Unmarshal(options, &wording.wording.Options); err != nil {
			return restoredWording{}, fmt.Errorf("decode the options of ask %s: %w", wording.ask, err)
		}
		return wording, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the decided asks' wordings to restore: %w", err)
	}
	if len(restored) == 0 {
		return nil, nil
	}
	planned := make([]string, len(restored))
	steps := make([]WriteStep, len(restored))
	for index, wording := range restored {
		planned[index] = wording.ask
		_, step, err := planAskReferences(ctx, tx, wording.ask, wording.wording.Question, s.serverURL)
		if err != nil {
			return nil, fmt.Errorf("index the restored question of ask %s: %w", wording.ask, err)
		}
		steps[index] = step
	}
	slices.Sort(planned)
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("restore decided asks' wordings", decidedWordingRestore, planned).Query(func(rows pgx.Rows) error {
			written, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return fmt.Errorf("restore decided asks' wordings: %w", err)
			}
			slices.Sort(written)
			if !slices.Equal(written, planned) {
				return fmt.Errorf("%w: planned %v, restored %v", errDecidedWordingsMoved, planned, written)
			}
			return nil
		})
		for _, step := range steps {
			if err := step(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
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
