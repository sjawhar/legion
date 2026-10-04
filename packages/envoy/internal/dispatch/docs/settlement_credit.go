package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// settlementCredit is the authors a pending settlement still needs. Pending are keyed by actor,
// so an update can merge them in PostgreSQL with JSONB's object concatenation. LastActor is the
// latest edit source used by ask reconciliation and events when no version is written. It lives
// beside the pending settlement so closing a document's issue, a room release or a restart cannot
// lose its attribution before that settlement commits.
type settlementCredit struct {
	Pending   map[string]model.Actor `json:"pending"`
	LastActor *model.Actor           `json:"last_actor,omitempty"`
}

// MarshalJSON writes Pending as an object even when it is nil, so every encoded credit carries
// an object for the pending-settlement row's merge (upsertSettlementCredit) to concatenate.
func (credit settlementCredit) MarshalJSON() ([]byte, error) {
	type wire settlementCredit
	if credit.Pending == nil {
		credit.Pending = map[string]model.Actor{}
	}
	return json.Marshal(wire(credit))
}

func settlementCreditFor(pending map[string]model.Actor, lastActor *model.Actor) settlementCredit {
	credit := settlementCredit{Pending: make(map[string]model.Actor, len(pending))}
	for _, actor := range pending {
		credit.Pending[settlementCreditKey(actor)] = actor
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

func (state *roomState) settlementCreditLocked() settlementCredit {
	return settlementCreditFor(state.pending, state.lastActor)
}

func (credit settlementCredit) empty() bool {
	return len(credit.Pending) == 0 && credit.LastActor == nil
}

func (state *roomState) mergeSettlementCreditLocked(credit settlementCredit) {
	for _, actor := range credit.Pending {
		state.pending[actorKey(actor)] = actor
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
func (s *Service) persistSettlementCredit(ctx context.Context, room string, credit settlementCredit, sequence uint64) error {
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
	if err := upsertSettlementCredit(ctx, tx, room, credit, false, sequence); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit settlement credit: %w", err)
	}
	return nil
}

// settlementCreditPersisted releases authors from an already-closed document only when its state
// still holds the exact credit snapshot that reached the pending-settlement row. A later write
// increments creditVersion and keeps its own authors until it persists them.
func (s *Service) settlementCreditPersisted(room string, creditVersion uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.closed && state.creditVersion == creditVersion {
		clear(state.pending)
		state.lastActor = nil
		state.unsettled = false
	}
	s.unlockState(room, state)
}

// pendingSettlementCredit reads the durable settlement credit with the row that says a document
// owes a settlement. A row from before migration 0069 carries the empty default credit.
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

// upsertSettlementCredit merges credit into room's pending-settlement row: its pending authors join
// the row's, a later credit for an actor replacing the earlier one. A credit naming a last actor or
// pending authors sets the row's last actor to its own, none included: the room clears its last
// actor for an edit no one actor can be credited with (creditContentChange), and that credit names
// the edit's authors without one. An append with no credit keeps the row's. Every operand is
// parenthesized: PostgreSQL gives `->` and `||` the same precedence. A stored or encoded pending
// that is not an object - a legacy or defensive row a write never produced - counts as empty, since
// concatenating an object with anything else raises "cannot concatenate jsonb object". updateMarkedAt
// is true only for a newly appended document update; recording authors after its transaction
// committed or while closing an issue must not make an old row wait another resumption age.
//
// sequence is the room's creditVersion when credit was captured (0 for a credit this same
// transaction's own version will immediately release, which never races a concurrent reader - see
// Ledger.recordSettlementCredit). A credit captured at or before the row's released_through
// watermark - set by a version's release (releaseSettlementCredit) that ran while this write's own
// durable append waited behind the document's advisory lock - is strictly older than what that
// release already consumed from the room's pending, so merging it would resurrect an author a
// version already credited; the row is left exactly as it stood instead.
func upsertSettlementCredit(ctx context.Context, tx pgx.Tx, room string, credit settlementCredit, updateMarkedAt bool, sequence uint64) error {
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
				then doc_settlements_pending.settlement_authors
				else jsonb_strip_nulls(jsonb_build_object(
					'pending',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
						then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end) ||
					(case when jsonb_typeof(excluded.settlement_authors->'pending') = 'object'
						then excluded.settlement_authors->'pending' else '{}'::jsonb end),
					'last_actor',
					coalesce(
						excluded.settlement_authors->'last_actor',
						case when (excluded.settlement_authors->'pending') = '{}'::jsonb
							then doc_settlements_pending.settlement_authors->'last_actor' end
					),
					'released_through',
					coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0)
				))
			end,
			marked_at = case when $4 then now() else doc_settlements_pending.marked_at end
	`, room, string(encoded), int64(sequence), updateMarkedAt); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
}

// releaseSettlementCredit takes the authors a version credited out of room's pending-settlement
// row, in the transaction that wrote the version. The row's last actor stays: ask reconciliation
// reads it once no pending author remains. It also raises the row's released_through watermark to
// sequence, the room's creditVersion when the version captured its authors from state.pending
// (captureAuthors): a credit captured at or before that point is already accounted for by this
// version, so a later upsertSettlementCredit call carrying it - an append that was queued behind
// this same transaction's advisory lock when the version ran - discards it instead of resurrecting
// an author this version already credited durably.
func releaseSettlementCredit(ctx context.Context, tx pgx.Tx, room string, authors []model.Actor, sequence uint64) error {
	if len(authors) == 0 {
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
			values ($1, jsonb_build_object('pending', '{}'::jsonb, 'released_through', $3::bigint))
		on conflict (artifact_id) do update set
			settlement_authors = jsonb_set(
				jsonb_set(
					doc_settlements_pending.settlement_authors,
					'{pending}',
					(case when jsonb_typeof(doc_settlements_pending.settlement_authors->'pending') = 'object'
						then doc_settlements_pending.settlement_authors->'pending' else '{}'::jsonb end) - $2::text[]
				),
				'{released_through}',
				to_jsonb(greatest(coalesce((doc_settlements_pending.settlement_authors->>'released_through')::bigint, 0), $3::bigint))
			)
	`, room, keys, int64(sequence)); err != nil {
		return fmt.Errorf("release the version's authors from the document's pending settlement: %w", err)
	}
	return nil
}
