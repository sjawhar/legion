package docs

import (
	"cmp"
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
// writing nothing: its id, its document and block (both null for an ask no block holds), the
// wording, and the answer the decision was given on, which the restore leaves as it is and the
// restored ask's parts are indexed with (restoreWordings). The answer is read as its canonical
// jsonb text, which the restore's guard compares the row against.
const decidedWordingPlan = decidedWordingEdits + `
	select a.id::text, a.block_artifact_id::text, a.block_id, ` + decidedWordingColumns + `, a.answer::text
	from asks a join first_edit f on a.id = f.id
	where ` + decidedWordingDiffers + `
	order by a.id`

// decidedWordingRestore is the statement that gives each ask $1 names back its decided wording
// (decidedWordingEdits), returning the ids it restored. It is idempotent: a row it restored asks
// its first edit's previous wording, so it writes it no more.
//
// $2 is the answer the plan read for each of $1, in the same order, and a row whose answer is no
// longer that one is left alone. A second pod on the same build can answer a decided ask while
// this update waits on its row, and Postgres then re-checks the row it waited for against this
// statement's conditions alone: without the answer among them the restoration would go on, and
// the parts queued behind it would write the answer the plan read, which is no longer the ask's,
// and clear owed_at over it. A row the guard leaves alone is not returned, so the restoration is
// refused (errDecidedWordingsMoved) and rolls back.
const decidedWordingRestore = decidedWordingEdits + `
	update asks a
	set (question, options, multiple, urgency) = (` + decidedWordingColumns + `)
	from first_edit f, unnest($1::uuid[], $2::text[]) as p (id, answer)
	where ` + decidedWordingDiffers + `
		and a.id = p.id and a.answer::text is not distinct from p.answer
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
	// answer is the decision the restored wording was given on, which the restore leaves as it
	// is: the restored ask's parts are read with it beside the wording (restoreWordings).
	// answerText is that same answer as the row stores it, which the restore's update guards on
	// so a pod that changed it meanwhile refuses the restoration (decidedWordingRestore).
	answer     *model.AskAnswer
	answerText *string
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
// settlement. It reads what it restores, plans the questions' references and the asks' parts,
// takes the copy locks and reads the owed documents before it writes anything, holding no ask's
// row; then it restores them, writes their references and their parts and marks the documents
// owed back to back, and commits. Its update restores exactly the asks it read or the restoration
// is refused (errDecidedWordingsMoved). A settlement takes its own document's owner lock before
// its copy lock and an answer or a resolution its ask's owner lock before SettleCopiesOf, so none
// of them waits on a lock this holds once it holds the copy lock.
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
// decided wording back, and indexes each restored ask again, as every write of an ask's question
// and options does: its own references (planAskReferences, refs.ReplaceCounted's read half) and
// its parts (planAskPartIndex), the parts read from the plan's row - the restored wording with
// the answer the decision left on it. The parts are planned against what this transaction's own
// queued writes of the same ask leave, so a settlement that restores a wording and then settles
// the block reads the rows this plan queued. They are indexed at the time this restoration
// writes them, the change they record, and not at the time of the edit it undoes, which the
// restore leaves on the row (edited_at). No event carries the change, as none does for
// refs.RebuildAll's, so nothing counts what the restoration moved. Which asks it restores, their
// references and their parts are read and planned where it is called (decidedWordingPlan); the
// restoration, the references and the parts go through write, queued in that order, so an ask's
// parts are written after the update whose wording they hold. The restoration names the planned
// asks, with the answer each was read with, and refuses to have restored any other set
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
			&wording.wording.Multiple, &wording.wording.Urgency, &wording.answerText); err != nil {
			return restoredWording{}, err
		}
		if err := json.Unmarshal(options, &wording.wording.Options); err != nil {
			return restoredWording{}, fmt.Errorf("decode the options of ask %s: %w", wording.ask, err)
		}
		if wording.answerText != nil {
			var decided model.AskAnswer
			if err := json.Unmarshal([]byte(*wording.answerText), &decided); err != nil {
				return restoredWording{}, fmt.Errorf("decode the answer of ask %s: %w", wording.ask, err)
			}
			wording.answer = &decided
		}
		return wording, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the decided asks' wordings to restore: %w", err)
	}
	if len(restored) == 0 {
		return nil, nil
	}
	// The parts are written in this transaction, so they are indexed at its time.
	at, err := transactionNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	// The ids the restoration names and the answers it guards on go in one order, and the ids it
	// reports back are compared sorted, so the plan's order is sorted here rather than the ids
	// alone.
	slices.SortFunc(restored, func(first, second restoredWording) int {
		return cmp.Compare(first.ask, second.ask)
	})
	planned := make([]string, 0, len(restored))
	answers := make([]*string, 0, len(restored))
	steps := make([]WriteStep, 0, 2*len(restored))
	for _, wording := range restored {
		planned = append(planned, wording.ask)
		answers = append(answers, wording.answerText)
		_, step, err := planAskReferences(ctx, tx, wording.ask, wording.wording.Question, s.serverURL)
		if err != nil {
			return nil, fmt.Errorf("index the restored question of ask %s: %w", wording.ask, err)
		}
		steps = append(steps, step)
		// The restored ask's parts are its question, its restored options and the answer the
		// decision was given on, which the restore leaves on the row. Their write is queued
		// after the restore's, so the trigger that marks the ask's parts owed on that update has
		// already run when this index clears it.
		ask := model.Ask{
			ID:       wording.ask,
			Question: wording.wording.Question,
			Options:  wording.wording.Options,
			Answer:   wording.answer,
		}
		indexing, err := planAskPartIndex(ctx, tx, ask, s.serverURL, at)
		if err != nil {
			return nil, fmt.Errorf("index the parts of the restored ask %s: %w", wording.ask, err)
		}
		steps = append(steps, func(b *store.WriteBatch) error {
			indexing.Queue(b)
			return nil
		})
	}
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("restore decided asks' wordings", decidedWordingRestore, planned, answers).Query(func(rows pgx.Rows) error {
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
