package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// settlementCredit is the authors a pending settlement still needs. Pending are keyed by actor,
// so an update can merge them in PostgreSQL with JSONB's object concatenation. PendingSeq is each
// pending author's own credit_seq, the row's durable twin of roomState.pendingAuthor.creditSeq,
// keyed the same way as Pending: the point a release (releaseSettlementCredit) compares an
// entry's own capture against, so an author credited again under the same key after that point -
// a different, newer entry - survives a release that names or sweeps the key (LEGION-513). A key
// Pending holds but PendingSeq does not - this transaction's own upsert (recordSettlementCredit),
// whose room-side sequence is not yet assigned when the row is written - reads as zero: a
// release's own filter treats zero as exempt, never satisfying its "at or before" comparison, so
// an entry with no sequence of its own is never swept until something gives it a real one.
//
// PendingGeneration is each entry's own per-room-load instance id, the row's twin of
// roomState.creditGeneration: a release only ever takes out an entry whose generation matches
// the releasing room's own, never one from a different generation. roomSeq alone is not a total
// order across processes - a rolling deploy can hold two Dispatch tasks' own live rooms for one
// document at once, each with its own in-process roomSeq counter, so a release comparing roomSeq
// alone could discard another task's genuinely unsettled, lower-numbered entry. generation makes
// that comparison meaningless across rooms: two different rooms never share one, so a release
// never reaches across the boundary a cross-process roomSeq comparison could not see. An entry
// from an abandoned generation - the task that credited it stopped before releasing it - is never
// lost and never stuck forever: the next room to load this document adopts every row entry into
// its own generation (bumpSettlementCreditGeneration), after which that room's own release can
// take it out normally, or a further load adopts it again. LastActor is the latest edit source
// used by ask reconciliation and events when no version is written. It lives beside the pending
// settlement so closing a document's issue, a room release or a restart cannot lose its
// attribution before that settlement commits. Generation is the row's own current generation,
// read back (unlike CreditSeq, which only ever travels toward storage): a release compares its
// own room's generation against it before trusting any PendingGeneration entry at all, so a
// release whose room has since been superseded (adopted by a later load while the release was in
// flight) touches nothing. CreditSeq is the room's creditSeq when this credit was captured
// (creditContentChange); it never reaches storage (json:"-") - it rides along only as far as
// upsertSettlementCredit's own gate check, which discards a credit whose CreditSeq is at or
// before the row's released_through watermark and whose Generation matches the row's (a version's
// release already consumed everything visible as of that point, within that generation). Its zero
// value means "not subject to the watermark": recordSettlementCredit's own upserts never set it,
// since a write's own credit and its own version's release are already ordered by the same
// transaction.
type settlementCredit struct {
	Pending           map[string]model.Actor `json:"pending"`
	PendingSeq        map[string]uint64      `json:"pending_seq"`
	PendingGeneration map[string]uint64      `json:"pending_generation"`
	LastActor         *model.Actor           `json:"last_actor,omitempty"`
	ReleasedThrough   uint64                 `json:"released_through"`
	Generation        uint64                 `json:"generation"`
	CreditSeq         uint64                 `json:"-"`
}

// MarshalJSON writes Pending, PendingSeq and PendingGeneration as objects even when nil, so every
// encoded credit carries an object for the pending-settlement row's merge (upsertSettlementCredit)
// to concatenate.
func (credit settlementCredit) MarshalJSON() ([]byte, error) {
	type wire settlementCredit
	if credit.Pending == nil {
		credit.Pending = map[string]model.Actor{}
	}
	if credit.PendingSeq == nil {
		credit.PendingSeq = map[string]uint64{}
	}
	if credit.PendingGeneration == nil {
		credit.PendingGeneration = map[string]uint64{}
	}
	return json.Marshal(wire(credit))
}

// settlementCreditFor builds a credit whose every pending author shares creditSeq and generation:
// the point and the room instance this one call captured them at (creditContentChange), or zero
// creditSeq for an upsert this same transaction's own commit will immediately release
// (recordSettlementCredit, whose release already runs first in its own order and so never reads
// these entries at all, and whose zero tag keeps any later, unrelated release from reading them
// as "at or before" its own point either) tagged with the writing room's own generation regardless.
// settlementCreditLocked, whose entries keep their own individually-captured sequences from
// state.pending, builds its credit directly instead of through this helper.
func settlementCreditFor(pending map[string]model.Actor, lastActor *model.Actor, creditSeq, generation uint64) settlementCredit {
	credit := settlementCredit{
		Pending:           make(map[string]model.Actor, len(pending)),
		PendingSeq:        make(map[string]uint64, len(pending)),
		PendingGeneration: make(map[string]uint64, len(pending)),
	}
	for _, actor := range pending {
		key := settlementCreditKey(actor)
		credit.Pending[key] = actor
		credit.PendingSeq[key] = creditSeq
		credit.PendingGeneration[key] = generation
	}
	if lastActor != nil {
		actor := *lastActor
		credit.LastActor = &actor
	}
	return credit
}

func settlementCreditKey(actor model.Actor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(actorKey(actor)))
}

// settlementCreditLocked is the room's whole pending credit, each author at its own creditSeq
// (pendingAuthor.creditSeq) rather than one shared point, all under this one room's own
// generation (pendingAuthor.generation is always state.creditGeneration, since every entry a
// live room credits is this room's own). The caller holds state.mu.
func (state *roomState) settlementCreditLocked() settlementCredit {
	generation := state.creditGeneration.Load()
	credit := settlementCredit{
		Pending:           make(map[string]model.Actor, len(state.pending)),
		PendingSeq:        make(map[string]uint64, len(state.pending)),
		PendingGeneration: make(map[string]uint64, len(state.pending)),
		Generation:        generation,
	}
	for _, entry := range state.pending {
		key := settlementCreditKey(entry.actor)
		credit.Pending[key] = entry.actor
		credit.PendingSeq[key] = entry.creditSeq
		credit.PendingGeneration[key] = generation
	}
	if state.lastActor != nil {
		actor := *state.lastActor
		credit.LastActor = &actor
	}
	return credit
}

func (credit settlementCredit) empty() bool {
	return len(credit.Pending) == 0 && credit.LastActor == nil
}

// mergeSettlementCreditLocked merges a durable credit into the room, each author at the room's
// own current creditSeq and creditGeneration rather than whatever credit.PendingSeq/
// PendingGeneration recorded: those were some room's own, possibly a different process's or an
// earlier instance's, which this room's release can never compare against directly
// (releaseSettlementCredit's whole reason for existing) - this is the adoption that keeps a
// merged entry from staying stuck under a generation nothing will ever release again, since it
// is restamped under this room's own generation as surely as one this room credits itself is.
// bumpSettlementCreditGeneration performs the same adoption durably, in the same load, so the
// row and the room agree. The caller holds state.mu.
func (state *roomState) mergeSettlementCreditLocked(credit settlementCredit) {
	creditSeq := state.creditSeq.Load()
	for _, actor := range credit.Pending {
		state.creditPendingLocked(actorKey(actor), actor, creditSeq)
	}
	if credit.LastActor != nil {
		actor := *credit.LastActor
		state.lastActor = &actor
	}
	if !credit.empty() {
		state.unsettled = true
	}
}

// persistSettlementCredit records credit beside the document's pending-settlement row. It holds
// the same advisory lock that orders document updates and settlements, so an update cannot be
// appended or settled between reading the row's authors and replacing their merged value.
func (s *Service) persistSettlementCredit(ctx context.Context, room string, credit settlementCredit, creditSeq uint64) error {
	if credit.empty() {
		return nil
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin settlement-credit transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return err
	}
	credit.CreditSeq = creditSeq
	if err := upsertSettlementCredit(ctx, tx, room, credit, false); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit settlement credit: %w", err)
	}
	return nil
}

// settlementCreditPersisted releases authors from an already-closed document only when its state
// still holds the exact credit snapshot that reached the pending-settlement row. A later write
// advances creditSeq and keeps its own authors until it persists them.
func (s *Service) settlementCreditPersisted(room string, creditSeq uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.closed && state.creditSeq.Load() == creditSeq {
		state.releaseAllPendingLocked(creditSeq)
		state.lastActor = nil
		state.unsettled = false
	}
	s.unlockState(room, state)
}

// pendingSettlementCredit reads the durable settlement credit with the row that says a document
// owes a settlement. A row from before migration 0079 carries the empty default credit.
func pendingSettlementCredit(ctx context.Context, q Queryer, room string) (bool, settlementCredit, error) {
	var encoded []byte
	err := q.QueryRow(ctx, `
		select settlement_authors from doc_settlements_pending where artifact_id = $1
	`, room).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, settlementCredit{}, nil
	}
	if err != nil {
		return false, settlementCredit{}, fmt.Errorf("read the document's pending settlement: %w", err)
	}
	var credit settlementCredit
	if err := json.Unmarshal(encoded, &credit); err != nil {
		return false, settlementCredit{}, fmt.Errorf("decode the document's pending settlement authors: %w", err)
	}
	return true, credit, nil
}

// upsertSettlementCredit merges credit into room's pending-settlement row: its pending authors
// and their own PendingSeq/PendingGeneration all join the row's, a later credit for an actor
// replacing the earlier one's authors, sequence and generation together. A credit naming a last
// actor or pending authors sets the row's last actor to its own, none included: the room clears
// its last actor for an edit no one actor can be credited with (creditContentChange), and that
// credit names the edit's authors without one. An append with no credit keeps the row's. Every
// operand is parenthesized: PostgreSQL gives `->` and `||` the same precedence. A stored or
// encoded pending, pending_seq or pending_generation that is not an object - a legacy or
// defensive row a write never produced - counts as empty, since concatenating an object with
// anything else raises "cannot concatenate jsonb object". updateMarkedAt is true only for a newly
// appended document update; recording authors after its transaction committed or while closing an
// issue must not make an old row wait another resumption age.
//
// The gate below discards credit.CreditSeq wholesale when it is both positive and at or before
// the row's released_through, provided credit.Generation still matches the row's own current
// generation: a credit whose generation has since been superseded (bumpSettlementCreditGeneration
// ran a load between this write's capture and this upsert reaching the row) is never gated by a
// watermark from a generation it no longer belongs to, and is merged as any other new credit
// would be - the merge's own per-key authority, not this gate, is what keeps it correct, since an
// adopting load has already restamped the row's own entries under the new generation by the time
// this upsert can run. CreditSeq (settlementCredit's own aggregate field) is the room's creditSeq
// when credit was captured (0 for a credit this same transaction's own version will immediately
// release, which never races a concurrent reader - see Ledger.recordSettlementCredit). A credit
// captured at or before the row's released_through watermark, in the same generation, was already
// consumed by the version's own release; merging it would resurrect an author that version already
// credited durably, so the row is left exactly as it stood instead.
func upsertSettlementCredit(ctx context.Context, tx pgx.Tx, room string, credit settlementCredit, updateMarkedAt bool) error {
	encoded, err := json.Marshal(credit)
	if err != nil {
		return fmt.Errorf("encode the document's pending settlement authors: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, settlement_authors) values ($1, $2::jsonb)
		on conflict (artifact_id) do update set
			settlement_authors = case
				when $3::bigint > 0
					and $3::bigint <= coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0)
					and $4::bigint = coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0)
				then doc_settlements_pending.settlement_authors
				else jsonb_strip_nulls(jsonb_build_object(
					'pending',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
						then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end) ||
					(case when jsonb_typeof(excluded.settlement_authors->'pending') = 'object'
						then excluded.settlement_authors->'pending' else '{}'::jsonb end),
					'pending_seq',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_seq') = 'object'
						then doc_settlements_pending.settlement_authors->'pending_seq' else '{}'::jsonb end) ||
					(case when jsonb_typeof(excluded.settlement_authors->'pending_seq') = 'object'
						then excluded.settlement_authors->'pending_seq' else '{}'::jsonb end),
					'pending_generation',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_generation') = 'object'
						then doc_settlements_pending.settlement_authors->'pending_generation' else '{}'::jsonb end) ||
					(case when jsonb_typeof(excluded.settlement_authors->'pending_generation') = 'object'
						then excluded.settlement_authors->'pending_generation' else '{}'::jsonb end),
					'last_actor',
					coalesce(
						excluded.settlement_authors->'last_actor',
						case when (excluded.settlement_authors->'pending') = '{}'::jsonb
							then doc_settlements_pending.settlement_authors->'last_actor' end
					),
					'released_through',
					coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0),
					'generation',
					coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0)
				))
			end,
			marked_at = case when $5 then now() else doc_settlements_pending.marked_at end
	`, room, string(encoded), int64(credit.CreditSeq), int64(credit.Generation), updateMarkedAt); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
}

// releaseSettlementCredit takes authors out of room's pending-settlement row, in the transaction
// that wrote the version, releasing only an entry whose own pending_generation matches generation
// - the releasing room's own, which a different room's entry, however its pending_seq compares,
// can never match (LEGION-513: a rolling deploy's two Dispatch tasks each hold their own live room
// for one document, each with its own in-process roomSeq counter no release may compare across) -
// and whose pending_seq is positive and at or before creditSeq - the point the version's capture
// read state.pending at (captureAuthors or, for an upload, the write's last read,
// Ledger.WroteVersion) - never a zero entry, which marks one this same transaction's own upsert is
// about to add below (recordSettlementCredit): zero can never be "at or before" anything, so such
// an entry survives whatever release runs against the row next, this one included -
// recordSettlementCredit also runs its own release first, before its own upserts, so this is a
// second guard, not the only one, protecting a different, unrelated transaction's zero-tagged
// entry it has not yet gotten around to replacing with a real sequence.
//
// fullRelease, an upload's own full release (Ledger.WroteVersion, versionPending.fullRelease),
// takes out every eligible entry of this room's own generation regardless of key, ignoring
// authors entirely: an upload's version credits its uploader alone, but its write may have
// changed or removed any pending edit visible as of its last read of the room, so its release
// must reach every one of them (releaseAllPendingLocked's durable counterpart) - or a browser's
// already-released credit survives in the row and resurrects into the room on its next load
// (onLoadDocument, mergeSettlementCreditLocked, LEGION-513). An ordinary, non-full release takes
// out only the named authors' entries and is a no-op when none are named.
//
// Either way the row's last actor stays (ask reconciliation reads it once no pending author
// remains), and released_through rises to creditSeq: a credit of this same generation captured at
// or before that point is already accounted for by this release, so a later upsertSettlementCredit
// call carrying it - an append that was queued behind this same transaction's advisory lock when
// the version ran - discards it instead of resurrecting an author this version already credited
// durably.
func releaseSettlementCredit(ctx context.Context, tx pgx.Tx, room string, authors []model.Actor, creditSeq, generation uint64, fullRelease bool) error {
	if !fullRelease && len(authors) == 0 {
		return nil
	}
	keys := make([]string, len(authors))
	for index, actor := range authors {
		keys[index] = settlementCreditKey(actor)
	}
	// An upsert, not a plain update: a document whose row no release or append has ever touched
	// has none to update, and a plain update would silently do nothing, losing this watermark
	// entirely and leaving a later append's gate check comparing against zero.
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, settlement_authors)
			values ($1, jsonb_build_object('pending', '{}'::jsonb, 'pending_seq', '{}'::jsonb, 'pending_generation', '{}'::jsonb, 'released_through', $4::bigint, 'generation', $5::bigint))
		on conflict (artifact_id) do update set
			settlement_authors = jsonb_set(
				jsonb_set(
					jsonb_set(
						jsonb_set(
							doc_settlements_pending.settlement_authors,
							'{pending}',
							(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
								then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end)
							- (
								select coalesce(array_agg(k), '{}'::text[])
								from jsonb_object_keys(
									case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
										then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
								) as k
								where ($3::bool or k = any($2::text[]))
									and coalesce((doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint, 0) > 0
									and (doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint <= $4::bigint
									and coalesce((doc_settlements_pending.settlement_authors->'pending_generation'->>k)::bigint, -1) = $5::bigint
							)
						),
						'{pending_seq}',
						(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_seq') = 'object'
							then doc_settlements_pending.settlement_authors->'pending_seq' else '{}'::jsonb end)
						- (
							select coalesce(array_agg(k), '{}'::text[])
							from jsonb_object_keys(
								case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
									then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
							) as k
							where ($3::bool or k = any($2::text[]))
								and coalesce((doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint, 0) > 0
								and (doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint <= $4::bigint
								and coalesce((doc_settlements_pending.settlement_authors->'pending_generation'->>k)::bigint, -1) = $5::bigint
						)
					),
					'{pending_generation}',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_generation') = 'object'
						then doc_settlements_pending.settlement_authors->'pending_generation' else '{}'::jsonb end)
					- (
						select coalesce(array_agg(k), '{}'::text[])
						from jsonb_object_keys(
							case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
								then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
						) as k
						where ($3::bool or k = any($2::text[]))
							and coalesce((doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint, 0) > 0
							and (doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint <= $4::bigint
							and coalesce((doc_settlements_pending.settlement_authors->'pending_generation'->>k)::bigint, -1) = $5::bigint
					)
				),
				'{released_through}',
				to_jsonb(
					case when coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0) = $5::bigint
						then greatest(coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0), $4::bigint)
						else coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0)
					end
				)
			)
	`, room, keys, fullRelease, int64(creditSeq), int64(generation)); err != nil {
		return fmt.Errorf("release the version's authors from the document's pending settlement: %w", err)
	}
	return nil
}

// bumpSettlementCreditGeneration gives room a new generation - its own per-room-load instance id
// - the moment a fresh roomState loads it (onLoadDocument), and durably adopts every pending entry
// the row already holds into that new generation, with its own pending_seq reset to zero (the
// same sentinel recordSettlementCredit's own fresh upserts use): this room has not yet given any
// of them a sequence of its own, so none is eligible for release until something re-credits it or
// this room's own future capture reads it from state.pending (mergeSettlementCreditLocked performs
// the same adoption in memory, in the same load). This is what keeps an entry a stopped task's
// generation can never release again from staying stuck forever: the next room to load the
// document, whichever process it runs in, adopts it into a generation that process's own releases
// can reach. Takes rooms, the pool reserved for document loads, rather than the general pool a
// load must not draw a second connection from (onLoadDocument's own comment on the wedge that
// risks); needs no advisory lock, since it is one plain, atomic single-row UPDATE whose
// correctness Postgres's own row-level locking already guarantees against a concurrent writer's
// equally brief transaction - never an indefinite wait on a lock a long-lived document writer
// could hold (LEGION-513, the deadlock round 17 fixed by removing exactly that kind of wait from
// a room load). A document with no row yet starts at generation 1.
func bumpSettlementCreditGeneration(ctx context.Context, rooms *pgxpool.Pool, room string) (uint64, error) {
	var generation int64
	err := rooms.QueryRow(ctx, `
		insert into doc_settlements_pending (artifact_id, settlement_authors)
			values ($1, jsonb_build_object('pending', '{}'::jsonb, 'pending_seq', '{}'::jsonb, 'pending_generation', '{}'::jsonb, 'released_through', 0, 'generation', 1))
		on conflict (artifact_id) do update set
			settlement_authors = jsonb_set(
				jsonb_set(
					doc_settlements_pending.settlement_authors,
					'{generation}',
					to_jsonb(coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0) + 1)
				),
				'{pending_generation}',
				(select coalesce(jsonb_object_agg(
						k,
						coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0) + 1
					), '{}'::jsonb)
					from jsonb_object_keys(
						case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
							then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
					) as k)
			) || jsonb_build_object(
				'pending_seq',
				(select coalesce(jsonb_object_agg(k, 0), '{}'::jsonb)
					from jsonb_object_keys(
						case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
							then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
					) as k)
			)
		returning (settlement_authors->>'generation')::bigint
	`, room).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("adopt the document's pending settlement into a new generation: %w", err)
	}
	return uint64(generation), nil
}
