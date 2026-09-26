package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
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

// ErrInvalidAskBlock refuses a write that would leave an ask it writes or changes unreadable: an
// upload (a spec at issue creation, a new document or version) or an edit whose ask breaks the
// content rule paragraph+ bullet_list?, or an edit whose ask settlement cannot read.
type ErrInvalidAskBlock struct {
	Reason error
}

func (e *ErrInvalidAskBlock) Error() string { return e.Reason.Error() }

func (e *ErrInvalidAskBlock) Unwrap() error { return e.Reason }

// SettlementActor writes what a document's settlement decides on its own: the retraction of an
// ask whose block left the document. Crediting the room's last editor instead would put a
// deletion nobody made in their name.
var SettlementActor = model.Actor{Kind: "system", ID: "document-settlement"}

// SettlementRetractionReason opens the reason of every retraction settlement writes, followed by
// the version the block left in.
const SettlementRetractionReason = "removed from the document in version"

// settlementRetracted reports whether ask's retraction is one settlement wrote when its block
// left the document, which returning the block undoes. A retraction a person or a session wrote
// is their decision and stands, however the document moves. Retractions written before
// settlement began naming itself carry only its reason, so that prefix still counts - which is
// why resolveAsk refuses a caller reason that begins with it.
func settlementRetracted(ask model.Ask) bool {
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "retracted" {
		return false
	}
	return ask.Resolution.Actor.SameAs(SettlementActor) ||
		strings.HasPrefix(ask.Resolution.Reason, SettlementRetractionReason)
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
	blocks, invalidBlocks, err := collectAskBlocksForSettlement(tree)
	if err != nil {
		return settlementReconciliation{}, err
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return settlementReconciliation{}, err
	}

	reconciled := settlementReconciliation{}
	for _, invalid := range invalidBlocks {
		delete(rows, invalid.id)
		if setAskInvalidAttribute(invalid.node, invalid.reason.Error()) {
			reconciled.changed = true
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"block.invalid",
				actor,
				model.BlockInvalidEventPayload{
					BlockID:     invalid.id,
					Version:     version,
					Reason:      invalid.reason.Error(),
					DisturbedBy: actor,
				},
			))
		}
	}
	for _, block := range blocks {
		ask, exists := rows[block.id]
		if !exists {
			ask, err = createAskBlock(ctx, tx, artifactID, owner, block, actor)
			if err != nil {
				return settlementReconciliation{}, err
			}
			changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, s.serverURL)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index new ask block: %w", err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner, artifactID, "ask.opened", actor, model.NewAskEventPayload(ask, changes),
			))
			if setAskServerAttributes(block.node, ask) {
				reconciled.changed = true
			}
			continue
		}
		delete(rows, block.id)

		if settlementRetracted(ask) {
			restored, err := restoreRetractedAsk(ctx, tx, ask)
			if err != nil {
				return settlementReconciliation{}, err
			}
			ask = restored
			eventType := "ask.opened"
			if ask.State == "answered" {
				eventType = "ask.answered"
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner, artifactID, eventType, actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
			))
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
			ask.EditedAt = askTimestamp(&editedAt)
			changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, s.serverURL)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index reconciled ask: %w", err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"ask.edited",
				actor,
				model.NewAskEditEventPayload(ask, previous, actor, changes),
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
		// Only an open ask is retracted: an answered or resolved one is already closed, its
		// answer or resolution is the record, and the asks table forbids a resolved row that
		// still carries an answer.
		if ask.State != "open" {
			continue
		}
		resolution := model.AskResolution{
			Kind:   "retracted",
			Reason: fmt.Sprintf("%s %d", SettlementRetractionReason, version),
			Actor:  SettlementActor,
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
		reconciled.events = append(reconciled.events, documentAskEvent(
			owner, artifactID, "ask.resolved", SettlementActor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		))
	}
	return reconciled, nil
}

type invalidAskBlock struct {
	node   *pmdoc.Node
	id     string
	reason error
}

// validateEditedAskBlocks refuses an edit that leaves an ask unreadable - breaking its content
// rule, or holding what settlement cannot read - when the edit wrote or changed it.
func validateEditedAskBlocks(before, after *pmdoc.Node) error {
	return refuseChangedAsks(before, after, func(ask *pmdoc.Node) error {
		if err := pmdoc.AskContentError(ask); err != nil {
			return err
		}
		_, err := parseAskBlock(ask)
		return err
	}, nodeToken)
}

// refuseChangedAsks is the first reason check gives against an ask in after that before does not
// hold as it is - an ask after wrote or changed - or nil. Two asks are the same when fingerprint
// gives both the same value. An ask a browser edit already left unreadable,
// which the write carries through unchanged, is not the write's to refuse: refusing it would refuse
// every write to the document until someone repairs that ask in the browser, and settlement flags
// it `invalid` meanwhile.
func refuseChangedAsks(before, after *pmdoc.Node, check func(*pmdoc.Node) error, fingerprint func(*pmdoc.Node) (string, error)) error {
	var held map[string]string
	var refusal, walkErr error
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		if refusal != nil || walkErr != nil {
			return false
		}
		if node.Type != "ask" {
			return true
		}
		reason := check(node)
		if reason == nil {
			return true
		}
		if held == nil {
			if held, walkErr = askFingerprints(before, fingerprint); walkErr != nil {
				return false
			}
		}
		value, err := fingerprint(node)
		if err != nil {
			walkErr = err
			return false
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); held[id] == value {
			return true
		}
		refusal = reason
		return false
	})
	if walkErr != nil {
		return walkErr
	}
	return refusal
}

// askFingerprints is each ask in tree by its block id, as fingerprint gives it.
func askFingerprints(tree *pmdoc.Node, fingerprint func(*pmdoc.Node) (string, error)) (map[string]string, error) {
	held := make(map[string]string)
	var err error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if err != nil {
			return false
		}
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		held[id], err = fingerprint(node)
		return true
	})
	return held, err
}

// askMarkdown is what an uploaded version can say of an ask: its rendering alone, taken as an
// upload is (asUploaded) - without anchor marks, and with its server-owned attributes (`state`,
// the answer, `invalid`) at their defaults, since an upload's are discarded for the ask row's. The
// attributes a reader's browser derives are never rendered. A new version is markdown, so this is
// how a version says it carries an ask unchanged.
func askMarkdown(ask *pmdoc.Node) (string, error) {
	return pmdoc.Render(asUploaded(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{ask}}))
}

func collectAskBlocksForSettlement(tree *pmdoc.Node) ([]askBlock, []invalidAskBlock, error) {
	blocks := []askBlock{}
	invalidBlocks := []invalidAskBlock{}
	seen := map[string]struct{}{}
	var collectErr error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, duplicate := seen[id]; duplicate {
			collectErr = fmt.Errorf("duplicate ask block id %q", id)
			return false
		}
		seen[id] = struct{}{}
		block, err := parseAskBlock(node)
		if err != nil {
			invalidBlocks = append(invalidBlocks, invalidAskBlock{node: node, id: id, reason: err})
			return true
		}
		blocks = append(blocks, block)
		return true
	})
	if collectErr != nil {
		return nil, nil, collectErr
	}
	return blocks, invalidBlocks, nil
}

func parseAskBlock(node *pmdoc.Node) (askBlock, error) {
	blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	urgency, _ := node.Attrs["urgency"].(string)
	multiple, _ := node.Attrs["multiple"].(bool)
	if blockID == "" || urgency == "" {
		return askBlock{}, fmt.Errorf("ask block is missing required attributes")
	}
	questionParts := []string{}
	// An ask block without a bullet list is a free-text decision; its options are an empty
	// list on the wire (never JSON null), the same shape every other ask carries.
	options := []model.AskOption{}
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

// loadAskBlocks reads and locks every ask anchored to a block of this document. `for no key
// update` by the rule at lockDocumentRoom: settlement holds these rows across the room lock, and
// nothing deletes an ask or writes a key column - reconciliation here and the ask routes update
// question, options, multiple, urgency, edited_at, state, answer and resolution, and `asks.id` is
// the key.
func loadAskBlocks(ctx context.Context, tx pgx.Tx, artifactID string) (map[string]model.Ask, error) {
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`
		from asks a where a.block_artifact_id = $1 and a.block_id is not null for no key update
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("load ask blocks: %w", err)
	}
	defer rows.Close()
	asks := map[string]model.Ask{}
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ask block: %w", err)
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
	if err := asks.FollowAuthor(ctx, tx, ask.ID, author); err != nil {
		return model.Ask{}, err
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
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid"} {
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

func setAskInvalidAttribute(node *pmdoc.Node, reason string) bool {
	if current, present := node.Attrs["invalid"]; present && current == reason {
		return false
	}
	node.Attrs["invalid"] = reason
	return true
}
func documentAskEvent(owner artifactOwner, artifactID, eventType string, actor model.Actor, payload any) model.Event {
	event := model.Event{IssueKey: owner.IssueKey, Type: eventType, Actor: actor, Payload: payload}
	if owner.IssueKey == nil {
		event.ArtifactID = &artifactID
	}
	return event
}
