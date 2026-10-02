package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ApprovalAskAt returns the newest open approval ask on artifactID, or nil. The caller holds the
// document owner's row, so it observes the same version the review and approval routes use.
func ApprovalAskAt(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	row := tx.QueryRow(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = $1
		order by a.created_at desc, a.id desc
		limit 1
		for no key update
	`, artifactID)
	ask, err := ScanAsk(row)
	if err == nil {
		return &ask, nil
	}
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return nil, fmt.Errorf("read open approval ask: %w", err)
}

// MoveApprovalAsks advances every open approval ask on the document to a new settled version. The
// same rows and their threads stay open; requested_version records that the agent must hand each
// moved request back before a human sees it in Waiting on you again.
func MoveApprovalAsks(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, serverURL string) ([]model.Event, error) {
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = $1
		order by a.created_at desc, a.id desc
		for no key update
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("lock open approval asks: %w", err)
	}
	open, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Ask, error) { return ScanAsk(row) })
	if err != nil {
		return nil, fmt.Errorf("read open approval asks: %w", err)
	}
	actor := SettlementActor
	if len(version.Authors) == 1 {
		actor = version.Authors[0]
	}
	events := make([]model.Event, 0, len(open))
	for index := range open {
		ask := open[index]
		if ask.Approval.Version == version.Number {
			continue
		}
		summary, err := approvalAskSummary(ask)
		if err != nil {
			return nil, err
		}
		previous := model.AskEditPrevious{
			Question: ask.Question,
			Options:  ask.Options,
			Multiple: ask.Multiple,
			Urgency:  ask.Urgency,
		}
		ask.Question = approvalQuestion(ask.Approval.Name, version.Number, summary)
		ask.Approval.Version = version.Number
		approval, err := json.Marshal(ask.Approval)
		if err != nil {
			return nil, fmt.Errorf("encode moved approval ask: %w", err)
		}
		var editedAt time.Time
		if err := tx.QueryRow(ctx, `
			update asks
			set question = $2, approval = $3, edited_at = clock_timestamp()
			where id = $1
			returning edited_at
		`, ask.ID, ask.Question, approval).Scan(&editedAt); err != nil {
			return nil, fmt.Errorf("move approval ask: %w", err)
		}
		ask.EditedAt = askTimestamp(&editedAt)
		changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, serverURL)
		if err != nil {
			return nil, fmt.Errorf("index moved approval ask: %w", err)
		}
		event, err := broker.Append(ctx, tx, model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type: "ask.edited", Actor: actor,
			Payload: model.NewAskEditEventPayload(ask, previous, actor, changes),
		})
		if err != nil {
			return nil, fmt.Errorf("append moved approval ask: %w", err)
		}
		if err := refs.Stamp(ctx, tx, "ask", ask.ID, event.ID); err != nil {
			return nil, fmt.Errorf("stamp moved approval ask references: %w", err)
		}
		events = append(events, event)
	}
	return events, nil
}

func approvalQuestion(name string, version int, summary string) string {
	question := fmt.Sprintf("Approve %s (version %d)?", name, version)
	if summary == "" {
		return question
	}
	return question + " " + summary
}

func approvalAskSummary(ask model.Ask) (string, error) {
	prefix := approvalQuestion(ask.Approval.Name, ask.Approval.Version, "")
	if ask.Question == prefix {
		return "", nil
	}
	if summary, ok := strings.CutPrefix(ask.Question, prefix+" "); ok && summary != "" {
		return summary, nil
	}
	return "", fmt.Errorf("approval ask %s has a question not written by the approval route", ask.ID)
}
