package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// settlementCredit is the authors a pending settlement still needs. Pending are the people its
// version credits; LastActor is the latest edit source used by ask reconciliation and events when
// no version is written. It lives beside the pending settlement so closing a document's issue, a
// room release or a restart cannot lose its attribution before that settlement commits.
type settlementCredit struct {
	Pending   []model.Actor `json:"pending"`
	LastActor *model.Actor  `json:"last_actor,omitempty"`
}

func (state *roomState) settlementCreditLocked() settlementCredit {
	credit := settlementCredit{Pending: actorSlice(state.pending)}
	if state.lastActor != nil {
		actor := *state.lastActor
		credit.LastActor = &actor
	}
	return credit
}

func (credit settlementCredit) empty() bool {
	return len(credit.Pending) == 0 && credit.LastActor == nil
}

func mergeSettlementCredit(first, second settlementCredit) settlementCredit {
	pending := make(map[string]model.Actor, len(first.Pending)+len(second.Pending))
	for _, actor := range first.Pending {
		pending[actorKey(actor)] = actor
	}
	for _, actor := range second.Pending {
		pending[actorKey(actor)] = actor
	}
	merged := settlementCredit{Pending: actorSlice(pending), LastActor: first.LastActor}
	if second.LastActor != nil {
		actor := *second.LastActor
		merged.LastActor = &actor
	}
	return merged
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
func (s *Service) persistSettlementCredit(ctx context.Context, room string, credit settlementCredit) error {
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
	if err := upsertSettlementCredit(ctx, tx, room, credit, false); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit settlement credit: %w", err)
	}
	return nil
}

// recordSettlementCredit retains a committed write's attribution when persisting it fails only by
// leaving the room state unsettled; the committed document itself must still be published. A later
// close retries the record before it releases that state.
func (s *Service) recordSettlementCredit(ctx context.Context, room string, credit settlementCredit) {
	if err := s.persistSettlementCredit(ctx, room, credit); err != nil {
		slog.Error("dispatch: record document settlement authors", "room", room, "error", err)
	}
}

// pendingSettlementCredit reads the durable settlement credit with the row that says a document
// owes a settlement. A row from before migration 0068 carries the empty default credit.
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

// upsertSettlementCredit merges credit into room's pending-settlement row. updateMarkedAt is true
// only for a newly appended document update; recording authors after its transaction committed or
// while closing an issue must not make an old row wait another resumption age.
func upsertSettlementCredit(ctx context.Context, tx pgx.Tx, room string, credit settlementCredit, updateMarkedAt bool) error {
	var encoded []byte
	err := tx.QueryRow(ctx, `
		select settlement_authors from doc_settlements_pending where artifact_id = $1
	`, room).Scan(&encoded)
	var existing settlementCredit
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read the document's pending settlement authors: %w", err)
	default:
		if err := json.Unmarshal(encoded, &existing); err != nil {
			return fmt.Errorf("decode the document's pending settlement authors: %w", err)
		}
	}
	merged := mergeSettlementCredit(existing, credit)
	encoded, err = json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode the document's pending settlement authors: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, settlement_authors) values ($1, $2)
		on conflict (artifact_id) do update set
			settlement_authors = excluded.settlement_authors,
			marked_at = case when $3 then now() else doc_settlements_pending.marked_at end
	`, room, encoded, updateMarkedAt); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
}
