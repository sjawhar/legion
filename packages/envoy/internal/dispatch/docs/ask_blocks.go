package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type askBlock struct {
	node     *pmdoc.Node
	id       string
	question string
	options  []model.AskOption
	multiple bool
	urgency  string
}

type settlementReconciliation struct {
	changed bool
	events  []model.Event
}

func (s *Service) reconcileAskBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	actor model.Actor,
	version int,
) (settlementReconciliation, error) {
	blocks, err := collectAskBlocks(tree)
	if err != nil {
		return settlementReconciliation{}, err
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return settlementReconciliation{}, err
	}

	reconciled := settlementReconciliation{}
	for _, block := range blocks {
		ask, exists := rows[block.id]
		if !exists {
			ask, err = createAskBlock(ctx, tx, artifactID, owner, block, actor)
			if err != nil {
				return settlementReconciliation{}, err
			}
			if err := refs.Replace(ctx, tx, "ask", ask.ID, ask.Question, s.serverURL); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index new ask block: %w", err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(owner, artifactID, "ask.opened", actor, ask))
			if setAskServerAttributes(block.node, ask) {
				reconciled.changed = true
			}
			continue
		}
		delete(rows, block.id)

		if ask.State == "resolved" && ask.Resolution != nil && ask.Resolution.Kind == "retracted" {
			restored, err := restoreRetractedAsk(ctx, tx, ask)
			if err != nil {
				return settlementReconciliation{}, err
			}
			ask = restored
			eventType := "ask.opened"
			if ask.State == "answered" {
				eventType = "ask.answered"
			}
			reconciled.events = append(reconciled.events, documentAskEvent(owner, artifactID, eventType, actor, ask))
		}

		if ask.Question != block.question || !reflect.DeepEqual(ask.Options, block.options) ||
			ask.Multiple != block.multiple || ask.Urgency != block.urgency {
			previous := model.AskEditPrevious{
				Question: ask.Question,
				Options:  ask.Options,
				Multiple: ask.Multiple,
				Urgency:  ask.Urgency,
			}
			options, err := json.Marshal(block.options)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("encode reconciled ask options: %w", err)
			}
			var editedAt time.Time
			if err := tx.QueryRow(ctx, `
				update asks
				set question = $2, options = $3, multiple = $4, urgency = $5, edited_at = now()
				where id = $1
				returning edited_at
			`, ask.ID, block.question, options, block.multiple, block.urgency).Scan(&editedAt); err != nil {
				return settlementReconciliation{}, fmt.Errorf("update reconciled ask: %w", err)
			}
			ask.Question = block.question
			ask.Options = block.options
			ask.Multiple = block.multiple
			ask.Urgency = block.urgency
			edited := editedAt.UTC().Format(time.RFC3339Nano)
			ask.EditedAt = &edited
			if err := refs.Replace(ctx, tx, "ask", ask.ID, ask.Question, s.serverURL); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index reconciled ask: %w", err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"ask.edited",
				actor,
				model.AskEditEventPayload{Ask: ask, Previous: previous, EditedBy: actor},
			))
		}
		if setAskServerAttributes(block.node, ask) {
			reconciled.changed = true
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"block.repaired",
				actor,
				model.BlockRepairedEventPayload{BlockID: block.id, Version: version, DisturbedBy: actor},
			))
		}
	}

	for _, ask := range rows {
		if ask.State == "resolved" && ask.Resolution != nil && ask.Resolution.Kind == "retracted" {
			continue
		}
		resolution := model.AskResolution{
			Kind:   "retracted",
			Reason: fmt.Sprintf("removed from the document in version %d", version),
			Actor:  actor,
			At:     time.Now().UTC(),
		}
		encoded, err := json.Marshal(resolution)
		if err != nil {
			return settlementReconciliation{}, fmt.Errorf("encode ask retraction: %w", err)
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, ask.ID, encoded); err != nil {
			return settlementReconciliation{}, fmt.Errorf("retract deleted ask block: %w", err)
		}
		ask.State = "resolved"
		ask.Resolution = &resolution
		reconciled.events = append(reconciled.events, documentAskEvent(owner, artifactID, "ask.resolved", actor, ask))
	}
	return reconciled, nil
}

func collectAskBlocks(tree *pmdoc.Node) ([]askBlock, error) {
	blocks := []askBlock{}
	seen := map[string]struct{}{}
	var collectErr error
	walkTree(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		block, err := parseAskBlock(node)
		if err != nil {
			collectErr = err
			return false
		}
		if _, duplicate := seen[block.id]; duplicate {
			collectErr = fmt.Errorf("duplicate ask block id %q", block.id)
			return false
		}
		seen[block.id] = struct{}{}
		blocks = append(blocks, block)
		return true
	})
	if collectErr != nil {
		return nil, collectErr
	}
	return blocks, nil
}

func parseAskBlock(node *pmdoc.Node) (askBlock, error) {
	blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	urgency, _ := node.Attrs["urgency"].(string)
	multiple, _ := node.Attrs["multiple"].(bool)
	if blockID == "" || urgency == "" {
		return askBlock{}, fmt.Errorf("ask block is missing required attributes")
	}
	questionParts := []string{}
	var options []model.AskOption
	for _, child := range node.Children {
		switch child.Type {
		case "paragraph":
			questionParts = append(questionParts, strings.TrimSpace(nodeText(child)))
		case "bullet_list":
			for _, item := range child.Children {
				if len(item.Children) == 0 {
					return askBlock{}, fmt.Errorf("ask block %q has an empty option", blockID)
				}
				label, description, found := strings.Cut(strings.TrimSpace(nodeText(item.Children[0])), ": ")
				if !found {
					description = ""
				}
				label = strings.TrimSpace(label)
				if label == "" {
					return askBlock{}, fmt.Errorf("ask block %q has an option without a label", blockID)
				}
				options = append(options, model.AskOption{Label: label, Description: strings.TrimSpace(description)})
			}
		default:
			return askBlock{}, fmt.Errorf("ask block %q has unsupported body node %q", blockID, child.Type)
		}
	}
	question := strings.TrimSpace(strings.Join(questionParts, "\n\n"))
	if question == "" {
		return askBlock{}, fmt.Errorf("ask block %q has an empty question", blockID)
	}
	return askBlock{node: node, id: blockID, question: question, options: options, multiple: multiple, urgency: urgency}, nil
}

func nodeText(node *pmdoc.Node) string {
	if node.Type == "text" {
		return node.Text
	}
	if node.Type == "hardbreak" {
		return "\n"
	}
	var text strings.Builder
	for _, child := range node.Children {
		text.WriteString(nodeText(child))
	}
	return text.String()
}

func walkTree(node *pmdoc.Node, visit func(*pmdoc.Node) bool) bool {
	if node == nil || !visit(node) {
		return false
	}
	for _, child := range node.Children {
		if !walkTree(child, visit) {
			return false
		}
	}
	return true
}

func loadAskBlocks(ctx context.Context, tx pgx.Tx, artifactID string) (map[string]model.Ask, error) {
	rows, err := tx.Query(ctx, `
		select id::text, issue_key, artifact_id::text, block_id, block_artifact_id::text, author, question, options, multiple, urgency,
		       anchor, state, answer, resolution, created_at, edited_at, kind, approval
		from asks where block_artifact_id = $1 and block_id is not null for update
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("load ask blocks: %w", err)
	}
	defer rows.Close()
	asks := map[string]model.Ask{}
	for rows.Next() {
		ask, err := scanAskBlock(rows)
		if err != nil {
			return nil, err
		}
		if ask.BlockID == nil {
			return nil, fmt.Errorf("loaded ask block without a block id")
		}
		asks[*ask.BlockID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ask blocks: %w", err)
	}
	return asks, nil
}

func scanAskBlock(row pgx.Row) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution, approval []byte
	var editedAt *time.Time
	if err := row.Scan(
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &ask.BlockID, &ask.BlockArtifactID, &author, &ask.Question, &options, &ask.Multiple,
		&ask.Urgency, &anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt, &ask.Kind, &approval,
	); err != nil {
		return model.Ask{}, fmt.Errorf("scan ask block: %w", err)
	}
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask block author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask block options: %w", err)
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask block answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask block resolution: %w", err)
		}
		ask.Resolution = &value
	}
	if editedAt != nil {
		value := editedAt.UTC().Format(time.RFC3339Nano)
		ask.EditedAt = &value
	}
	return ask, nil
}

func createAskBlock(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	block askBlock,
	author model.Actor,
) (model.Ask, error) {
	options, err := json.Marshal(block.options)
	if err != nil {
		return model.Ask{}, fmt.Errorf("encode new ask block options: %w", err)
	}
	authorJSON, err := json.Marshal(author)
	if err != nil {
		return model.Ask{}, fmt.Errorf("encode new ask block author: %w", err)
	}
	var ownerArtifactID *string
	if owner.IssueKey == nil {
		ownerArtifactID = new(artifactID)
	}
	var ask model.Ask
	if err := tx.QueryRow(ctx, `
		insert into asks (issue_key, artifact_id, block_id, block_artifact_id, author, question, options, multiple, urgency, kind)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'question')
		returning id::text, created_at
	`, owner.IssueKey, ownerArtifactID, block.id, artifactID, authorJSON, block.question, options, block.multiple, block.urgency).Scan(&ask.ID, &ask.CreatedAt); err != nil {
		return model.Ask{}, fmt.Errorf("create ask block row: %w", err)
	}
	ask.IssueKey = owner.IssueKey
	ask.ArtifactID = ownerArtifactID
	ask.BlockID = new(block.id)
	ask.BlockArtifactID = new(artifactID)
	ask.Author = author
	ask.Question = block.question
	ask.Options = block.options
	ask.Multiple = block.multiple
	ask.Urgency = block.urgency
	ask.State = "open"
	ask.Kind = "question"
	return ask, nil
}

func restoreRetractedAsk(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
	var payload []byte
	if err := tx.QueryRow(ctx, `
		select payload from events
		where type in ('ask.opened', 'ask.edited', 'ask.answered') and payload->>'id' = $1
		order by id desc limit 1
	`, ask.ID).Scan(&payload); err != nil {
		return model.Ask{}, fmt.Errorf("load ask state before retraction: %w", err)
	}
	var prior model.Ask
	if err := json.Unmarshal(payload, &prior); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask state before retraction: %w", err)
	}
	if prior.State != "open" && prior.State != "answered" {
		return model.Ask{}, fmt.Errorf("ask %q state before retraction is %q", ask.ID, prior.State)
	}
	var answer any
	if prior.Answer != nil {
		encoded, err := json.Marshal(prior.Answer)
		if err != nil {
			return model.Ask{}, fmt.Errorf("encode restored ask answer: %w", err)
		}
		answer = encoded
	}
	if _, err := tx.Exec(ctx, `update asks set state = $2, answer = $3, resolution = null where id = $1`, ask.ID, prior.State, answer); err != nil {
		return model.Ask{}, fmt.Errorf("restore retracted ask: %w", err)
	}
	ask.State = prior.State
	ask.Answer = prior.Answer
	ask.Resolution = nil
	return ask, nil
}

func setAskServerAttributes(node *pmdoc.Node, ask model.Ask) bool {
	desired := pmdoc.Attrs{"state": ask.State}
	if ask.State == "answered" && ask.Answer != nil {
		desired["answered_by"] = ask.Answer.User
		desired["answered_at"] = ask.Answer.At.UTC().Format(time.RFC3339Nano)
		desired["selected"] = append([]string(nil), ask.Answer.Selected...)
		if ask.Answer.Text != nil {
			desired["answer"] = *ask.Answer.Text
		}
	}
	changed := false
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer"} {
		want, present := desired[name]
		got, exists := node.Attrs[name]
		if present {
			if exists && askServerAttributeEqual(got, want) {
				continue
			}
			node.Attrs[name] = want
			changed = true
			continue
		}
		if exists {
			delete(node.Attrs, name)
			changed = true
		}
	}
	return changed
}

func askServerAttributeEqual(got, want any) bool {
	gotItems, gotIsItems := askStringItems(got)
	wantItems, wantIsItems := askStringItems(want)
	if gotIsItems || wantIsItems {
		if !gotIsItems || !wantIsItems || len(gotItems) != len(wantItems) {
			return false
		}
		for index := range gotItems {
			if gotItems[index] != wantItems[index] {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

func askStringItems(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return items, true
	case []any:
		out := make([]string, len(items))
		for index, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out[index] = text
		}
		return out, true
	default:
		return nil, false
	}
}

func documentAskEvent(owner artifactOwner, artifactID, eventType string, actor model.Actor, payload any) model.Event {
	event := model.Event{IssueKey: owner.IssueKey, Type: eventType, Actor: actor, Payload: payload}
	if owner.IssueKey == nil {
		event.ArtifactID = &artifactID
	}
	return event
}
