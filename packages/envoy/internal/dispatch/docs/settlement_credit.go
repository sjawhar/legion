package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
// from an abandoned generation - the task that credited it stopped before releasing it - is
// never lost and never stuck forever, but only once that generation's lease has expired
// (doc_settlement_generation_leases, generationLeaseTTL): the next room to load this document, or
// any live room's own lease-refresh tick on the same document, adopts every entry whose own
// generation currently holds no unexpired lease into its own generation
// (bumpSettlementCreditGeneration, adoptAbandonedSettlementCredit), after which that room's own
// release can take it out normally. An entry under a generation whose lease is still live is left
// alone - the room holding it is live and its own release will reach it; adopting it here too
// would let two rooms credit the same author on their own next versions, the very duplicate
// credit a generation tag alone does not prevent on its own.
// LastActor is the latest edit source
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
		Generation:        generation,
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
// generation (every entry a live room credits shares its one state.creditGeneration; pendingAuthor
// has no generation field of its own). The caller holds state.mu.
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
// The gate below discards an incoming credit two ways. A credit from a generation strictly
// older than the row's own current generation is always stale and always discarded, whatever its
// CreditSeq says: that generation is gone (bumpSettlementCreditGeneration already moved the row
// past it, between this write's own capture and this upsert reaching the row - queued behind the
// document's advisory lock, same as the ordinary, same-generation race below), and merging its
// credit would resurrect an author under a generation nothing will ever release again. A credit
// from the row's own current generation is discarded when its CreditSeq is both positive and at
// or before the row's released_through: that version's own release already consumed it. CreditSeq
// (settlementCredit's own aggregate field) is the room's creditSeq when credit was captured (0
// for a credit this same transaction's own version will immediately release, which never races a
// concurrent reader - see Ledger.recordSettlementCredit). Either way, merging the discarded
// credit would resurrect an author a version already credited durably, so the row is left exactly
// as it stood instead.
func upsertSettlementCredit(ctx context.Context, tx pgx.Tx, room string, credit settlementCredit, updateMarkedAt bool) error {
	encoded, err := json.Marshal(credit)
	if err != nil {
		return fmt.Errorf("encode the document's pending settlement authors: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, settlement_authors) values ($1, $2::jsonb)
		on conflict (artifact_id) do update set
			settlement_authors = case
				when $4::bigint < coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0)
					or ($3::bigint > 0
						and $3::bigint <= coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0)
						and $4::bigint = coalesce((doc_settlements_pending.settlement_authors->>'generation')::bigint, 0))
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
		with drop_keys as (
			select coalesce(array_agg(k), '{}'::text[]) as keys
			from doc_settlements_pending, jsonb_object_keys(
				case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
					then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end
			) as k
			where doc_settlements_pending.artifact_id = $1
				and ($3::bool or k = any($2::text[]))
				and coalesce((doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint, 0) > 0
				and (doc_settlements_pending.settlement_authors->'pending_seq'->>k)::bigint <= $4::bigint
				and coalesce((doc_settlements_pending.settlement_authors->'pending_generation'->>k)::bigint, -1) = $5::bigint
		)
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
							- (select keys from drop_keys)
						),
						'{pending_seq}',
						(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_seq') = 'object'
							then doc_settlements_pending.settlement_authors->'pending_seq' else '{}'::jsonb end)
						- (select keys from drop_keys)
					),
					'{pending_generation}',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending_generation') = 'object'
						then doc_settlements_pending.settlement_authors->'pending_generation' else '{}'::jsonb end)
					- (select keys from drop_keys)
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

// execQueryer is a pool that can run a statement for effect, not only read one: the surface a
// lease operation needs, which both *pgxpool.Pool (the pool reserved for loads) and *store.Pool
// (the general pool, which the lease-refresh ticker uses) implement, so the same lease functions
// run on whichever pool their caller holds.
type execQueryer interface {
	Queryer
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// bumpSettlementCreditGeneration gives room a new generation - its own per-room-load instance id
// - the moment a fresh roomState loads it (onLoadDocument), and durably adopts every pending
// entry whose own generation currently holds no unexpired lease (generationLeases) into the new
// one, with its own pending_seq reset to zero (the same sentinel recordSettlementCredit's own
// fresh upserts use): this room has not yet given any of them a sequence of its own, so none is
// eligible for release until something re-credits it or this room's own future capture reads it
// from state.pending. An entry under a generation whose lease has not expired is left entirely
// alone - neither restamped nor returned - since the room still holding that lease is live and
// its own release will reach it; adopting it here would let two rooms credit the same author on
// their own next versions (LEGION-513). Returns the generation and exactly the entries this call
// adopted, so the caller's in-memory merge (mergeSettlementCreditLocked) touches only what the
// row actually took: bumping the generation, checking leases, adopting and reporting what was
// adopted all happen in this one statement, so no credit landing between separate steps could be
// adopted durably here yet missed by that merge, or vice versa.
//
// This is what keeps an entry a stopped task's generation can never release again from staying
// stuck forever: once its lease expires, the next room to load the document, whichever process
// it runs in, adopts it into a generation that process's own releases can reach - and a live
// room's own lease-refresh tick performs the same adoption on its own document
// (refreshGenerationLeaseAndAdopt), so a document that never reloads is not stuck waiting for one
// either. Takes rooms, the pool reserved for document loads, rather than the general pool a load
// must not draw a second connection from (onLoadDocument's own comment on the wedge that risks);
// needs no advisory lock, since every statement here is a plain, atomic single-row operation
// whose correctness Postgres's own row-level locking already guarantees against a concurrent
// writer's equally brief transaction - never an indefinite wait on a lock a long-lived document
// writer could hold (LEGION-513, the deadlock round 17 fixed by removing exactly that kind of
// wait from a room load). A document with no row yet is given one, at generation 1, before the
// same call retries once against it: landing on generation 2, since the retry's own update
// always increments, not 1 - harmless, since nothing is ever adopted from a row that was just
// created, and every generation after this one still increments by exactly one from there.
func bumpSettlementCreditGeneration(ctx context.Context, rooms execQueryer, room string, now time.Time) (uint64, map[string]model.Actor, error) {
	var generation int64
	var adopted []byte
	err := rooms.QueryRow(ctx, `
		with live as (
			select l.generation from doc_settlement_generation_leases l
				where l.artifact_id = $1 and l.expires_at > $2
		),
		next_generation as (
			select coalesce((settlement_authors->>'generation')::bigint, 0) + 1 as value
			from doc_settlements_pending where artifact_id = $1
		),
		adoptable as (
			select kv.key
			from doc_settlements_pending, jsonb_each_text(
				case when jsonb_typeof(settlement_authors->'pending_generation') = 'object'
					then settlement_authors->'pending_generation' else '{}'::jsonb end
			) as kv(key, value)
			where artifact_id = $1
				and kv.value::bigint != (select value from next_generation)
				and not exists (select 1 from live where live.generation = kv.value::bigint)
		)
		update doc_settlements_pending
		set settlement_authors = jsonb_set(
			jsonb_set(
				jsonb_set(
					settlement_authors,
					'{generation}',
					to_jsonb((select value from next_generation))
				),
				'{pending_generation}',
				(select coalesce(jsonb_object_agg(
						kv.key,
						case when kv.key in (select key from adoptable) then to_jsonb((select value from next_generation)) else kv.value end
					), '{}'::jsonb)
					from jsonb_each(
						case when jsonb_typeof(settlement_authors->'pending_generation') = 'object'
							then settlement_authors->'pending_generation' else '{}'::jsonb end
					) as kv(key, value))
			),
			'{pending_seq}',
			(select coalesce(jsonb_object_agg(
					kv.key,
					case when kv.key in (select key from adoptable) then '0'::jsonb else kv.value end
				), '{}'::jsonb)
				from jsonb_each(
					case when jsonb_typeof(settlement_authors->'pending_seq') = 'object'
						then settlement_authors->'pending_seq' else '{}'::jsonb end
				) as kv(key, value))
		)
		where artifact_id = $1
		returning
			(select value from next_generation),
			(select coalesce(jsonb_object_agg(kv.key, kv.value), '{}'::jsonb)
				from jsonb_each(
					case when jsonb_typeof(settlement_authors->'pending') = 'object'
						then settlement_authors->'pending' else '{}'::jsonb end
				) as kv(key, value)
				where kv.key in (select key from adoptable))
	`, room, now).Scan(&generation, &adopted)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row yet: a document this is the first credit-related event for ever. Nothing to
		// adopt, since nothing has ever been pending; the row starts at generation 1.
		if _, insertErr := rooms.Exec(ctx, `
			insert into doc_settlements_pending (artifact_id, settlement_authors)
				values ($1, jsonb_build_object('pending', '{}'::jsonb, 'pending_seq', '{}'::jsonb, 'pending_generation', '{}'::jsonb, 'released_through', 0, 'generation', 1))
			on conflict (artifact_id) do nothing
		`, room); insertErr != nil {
			return 0, nil, fmt.Errorf("start the document's pending settlement at generation 1: %w", insertErr)
		}
		// A concurrent first load could have raced this insert and already taken generation 1;
		// either way, retrying the same update now finds a row and runs the ordinary path.
		return bumpSettlementCreditGeneration(ctx, rooms, room, now)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("adopt the document's pending settlement into a new generation: %w", err)
	}
	var pending map[string]model.Actor
	if err := json.Unmarshal(adopted, &pending); err != nil {
		return 0, nil, fmt.Errorf("decode the document's adopted settlement authors: %w", err)
	}
	return uint64(generation), pending, nil
}

// takeOrRefreshGenerationLease takes or refreshes room's lease on generation, extending its
// expiry to generationLeaseTTL from now: a live room holds its own lease, refreshed on a ticker
// while it stays live (refreshGenerationLeaseAndAdopt), so bumpSettlementCreditGeneration's own
// adoption check never mistakes this room's own, still-live generation for an abandoned one.
func takeOrRefreshGenerationLease(ctx context.Context, pool execQueryer, room string, generation uint64, now time.Time) error {
	if _, err := pool.Exec(ctx, `
		insert into doc_settlement_generation_leases (artifact_id, generation, expires_at)
			values ($1, $2, $3)
		on conflict (artifact_id, generation) do update set expires_at = excluded.expires_at
	`, room, int64(generation), now.Add(generationLeaseTTL)); err != nil {
		return fmt.Errorf("take the document's generation lease: %w", err)
	}
	return nil
}

// deleteGenerationLease deletes room's lease on generation: a clean unload (releaseUnloadedRoom)
// calls this so the next load, or another live room's own lease-refresh tick, adopts this
// generation's entries at once rather than waiting out generationLeaseTTL.
func deleteGenerationLease(ctx context.Context, pool execQueryer, room string, generation uint64) error {
	if _, err := pool.Exec(ctx, `
		delete from doc_settlement_generation_leases where artifact_id = $1 and generation = $2
	`, room, int64(generation)); err != nil {
		return fmt.Errorf("delete the document's generation lease: %w", err)
	}
	return nil
}

// deleteGenerationLeasesForRoom deletes every one of room's generation leases, whichever
// generation each belongs to: resumeOwedSettlements calls this before arming a document's
// settlement, since its own age check already established that whatever generation last credited
// this document is not coming back to refresh a lease of its own (LEGION-513).
func deleteGenerationLeasesForRoom(ctx context.Context, pool execQueryer, room string) error {
	if _, err := pool.Exec(ctx, `
		delete from doc_settlement_generation_leases where artifact_id = $1
	`, room); err != nil {
		return fmt.Errorf("delete the document's generation leases: %w", err)
	}
	return nil
}

// adoptAbandonedSettlementCredit adopts, into room's own already-leased generation, every pending
// entry on the document whose own generation currently holds no unexpired lease - the same
// adoption bumpSettlementCreditGeneration performs at load, run again on this already-live room's
// own lease-refresh tick (refreshGenerationLeaseAndAdopt) so a document that never reloads is not
// stuck waiting for one before an abandoned generation's entries are picked up. Returns exactly
// what it adopted, for the caller's in-memory merge.
func adoptAbandonedSettlementCredit(ctx context.Context, pool execQueryer, room string, generation uint64, now time.Time) (map[string]model.Actor, error) {
	var adopted []byte
	err := pool.QueryRow(ctx, `
		with live as (
			select generation from doc_settlement_generation_leases
				where artifact_id = $1 and expires_at > $3
		),
		adoptable as (
			select kv.key
			from doc_settlements_pending, jsonb_each_text(
				case when jsonb_typeof(settlement_authors->'pending_generation') = 'object'
					then settlement_authors->'pending_generation' else '{}'::jsonb end
			) as kv(key, value)
			where artifact_id = $1
				and kv.value::bigint != $2
				and not exists (select 1 from live where live.generation = kv.value::bigint)
		)
		update doc_settlements_pending
		set settlement_authors = jsonb_set(
			jsonb_set(
				settlement_authors,
				'{pending_generation}',
				(select coalesce(jsonb_object_agg(
						kv.key,
						case when kv.key in (select key from adoptable) then to_jsonb($2::bigint) else kv.value end
					), '{}'::jsonb)
					from jsonb_each(
						case when jsonb_typeof(settlement_authors->'pending_generation') = 'object'
							then settlement_authors->'pending_generation' else '{}'::jsonb end
					) as kv(key, value))
			),
			'{pending_seq}',
			(select coalesce(jsonb_object_agg(
					kv.key,
					case when kv.key in (select key from adoptable) then '0'::jsonb else kv.value end
				), '{}'::jsonb)
				from jsonb_each(
					case when jsonb_typeof(settlement_authors->'pending_seq') = 'object'
						then settlement_authors->'pending_seq' else '{}'::jsonb end
				) as kv(key, value))
		)
		where artifact_id = $1
		returning (
			select coalesce(jsonb_object_agg(kv.key, kv.value), '{}'::jsonb)
			from jsonb_each(
				case when jsonb_typeof(settlement_authors->'pending') = 'object'
					then settlement_authors->'pending' else '{}'::jsonb end
			) as kv(key, value)
			where kv.key in (select key from adoptable)
		)
	`, room, int64(generation), now).Scan(&adopted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adopt abandoned settlement credit: %w", err)
	}
	var pending map[string]model.Actor
	if err := json.Unmarshal(adopted, &pending); err != nil {
		return nil, fmt.Errorf("decode the document's adopted settlement authors: %w", err)
	}
	return pending, nil
}

// runGenerationLeaseTicker refreshes room's lease on generation every generationLeaseRefresh
// while this room stays live, and performs the same abandoned-entry adoption a load does
// (refreshGenerationLeaseAndAdopt), so a document that never reloads is not stuck waiting for one
// before it picks up another, abandoned generation's entries. Stops once stop closes (a clean
// unload, releaseUnloadedRoom, or Shutdown): the lease itself is already deleted by then, by
// whichever of those closed stop, synchronously and before it did - never left to this goroutine
// to race asynchronously against the next load's own adoption check (LEGION-513).
func (s *Service) runGenerationLeaseTicker(room string, generation uint64, stop chan struct{}) {
	ticker := time.NewTicker(generationLeaseRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := s.refreshGenerationLeaseAndAdopt(context.Background(), room, generation); err != nil {
				slog.Error("dispatch: refresh document generation lease", "room", room, "error", err)
			}
		}
	}
}

// refreshGenerationLeaseAndAdopt refreshes room's lease on generation and adopts into it every
// pending entry on the document whose own generation currently holds no unexpired lease
// (adoptAbandonedSettlementCredit), merging what it adopted into this room's own state.pending
// the same way onLoadDocument's own adoption does. Exported to tests as the hook that stands in
// for the real ticker's own tick, so a test advances generation-lease expiry with a fake clock
// instead of sleeping generationLeaseRefresh for real.
func (s *Service) refreshGenerationLeaseAndAdopt(ctx context.Context, room string, generation uint64) error {
	now := s.now()
	if err := takeOrRefreshGenerationLease(ctx, s.store.Pool, room, generation, now); err != nil {
		return err
	}
	adopted, err := adoptAbandonedSettlementCredit(ctx, s.store.Pool, room, generation, now)
	if err != nil {
		return err
	}
	if len(adopted) == 0 {
		return nil
	}
	state := s.lockExistingState(room)
	if state == nil {
		return nil
	}
	state.mergeSettlementCreditLocked(settlementCredit{Pending: adopted})
	s.unlockState(room, state)
	return nil
}
