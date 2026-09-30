package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ApprovalAskAt is the approval ask open on the document artifactID at version, or nil. An open
// approval ask naming any other version names one the document has moved past, which no answer
// can review any more (APPROVAL_ASK_STALE), so ApprovalAskAt retracts it in actor's name, with a
// reason naming version followed by instead, what takes the ask's place. Each retraction appends
// its ask.resolved, which reaches the ask's followers as any resolve does, and ApprovalAskAt
// returns those events for the caller to publish once its transaction commits. The caller holds
// the document owner's row, which every version write takes first, so no version is written
// between the version the caller read and the retraction.
func ApprovalAskAt(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version int, actor model.Actor, instead string) (*model.Ask, []model.Event, error) {
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = $1
		order by a.created_at desc, a.id desc
		for no key update
	`, artifactID)
	if err != nil {
		return nil, nil, fmt.Errorf("lock open approval asks: %w", err)
	}
	open, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Ask, error) { return ScanAsk(row) })
	if err != nil {
		return nil, nil, fmt.Errorf("read open approval asks: %w", err)
	}
	var current *model.Ask
	var retractions []model.Event
	for index := range open {
		ask := open[index]
		if ask.Approval.Version == version {
			if current == nil {
				current = &open[index]
			}
			continue
		}
		// The reason opens with the document, never with a caller's text, so it is never one of
		// the reasons only settlement writes (SettlementRetractionReason).
		resolution := model.AskResolution{
			Kind:   "retracted",
			Reason: fmt.Sprintf("the document moved on to version %d; %s", version, instead),
			Actor:  actor,
			At:     time.Now().UTC(),
		}
		encoded, err := json.Marshal(resolution)
		if err != nil {
			return nil, nil, fmt.Errorf("encode approval ask retraction: %w", err)
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, ask.ID, encoded); err != nil {
			return nil, nil, fmt.Errorf("retract stale approval ask: %w", err)
		}
		ask.State = "resolved"
		ask.Resolution = &resolution
		// A retraction writes no question text, so it moves no references and says so.
		event, err := broker.Append(ctx, tx, model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type: "ask.resolved", Actor: actor,
			Payload: model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("append approval ask retraction: %w", err)
		}
		retractions = append(retractions, event)
	}
	return current, retractions, nil
}

// RetractStaleApprovalAsks is ApprovalAskAt for a writer that has just written version: every
// approval ask open on the document names an older version, and each is retracted in the
// writer's name. Every route that writes a version calls it in the version's transaction.
func RetractStaleApprovalAsks(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version int, actor model.Actor) ([]model.Event, error) {
	_, retractions, err := ApprovalAskAt(ctx, tx, broker, artifactID, version, actor, "request approval of that version to ask again")
	return retractions, err
}
