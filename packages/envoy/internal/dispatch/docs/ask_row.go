package docs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// AskColumns selects one ask row, aliased a, in the order ScanAsk reads it.
const AskColumns = `a.id::text, a.issue_key, a.artifact_id::text, a.block_id, a.block_artifact_id::text, a.author, a.question, a.options, a.multiple, a.urgency,
		a.anchor, a.state, a.answer, a.resolution, a.created_at, a.edited_at, a.kind, a.approval`

// ScanAsk decodes one AskColumns row. extra receives, in order, any columns the query
// selects after AskColumns.
func ScanAsk(row pgx.Row, extra ...any) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution, approval []byte
	var editedAt *time.Time
	targets := append([]any{
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &ask.BlockID, &ask.BlockArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
		&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt, &ask.Kind, &approval,
	}, extra...)
	if err := row.Scan(targets...); err != nil {
		return model.Ask{}, err
	}
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask options: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := json.Unmarshal(anchor, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask anchor: %w", err)
		}
		ask.Anchor = &value
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask resolution: %w", err)
		}
		ask.Resolution = &value
	}
	if len(approval) > 0 {
		var value model.AskApproval
		if err := json.Unmarshal(approval, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask approval: %w", err)
		}
		ask.Approval = &value
	}
	if editedAt != nil {
		value := editedAt.UTC().Format(time.RFC3339Nano)
		ask.EditedAt = &value
	}
	return ask, nil
}
