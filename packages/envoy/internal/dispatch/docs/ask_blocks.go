package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

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

// settlementReconciliation is what one settlement's pass over the ask blocks found. Everything
// that names a version number - a repair, an invalid block, the retraction of an ask whose block
// left - is left for nameVersion, since settlement knows the number only once it has rendered
// the reconciled tree: a settlement whose markdown repeats the latest version writes no version
// and leaves the document at the one it found.
type settlementReconciliation struct {
	repairs   []askRepair
	events    []model.Event
	retracted []model.Ask
	// indexedAskBlocks are the blocks with a recorded author that this settlement indexed or
	// found indexed, whose author the room forgets once it commits (consumeAskAuthors).
	indexedAskBlocks []string
	// rewrites are the ask blocks whose server attributes repairAsk repaired, which
	// withholdAnswers reads again where a repair wrote back an answer.
	rewrites []serverRewrite
}

// askRepair is one server-owned attribute repair settlement made to the ask block carrying blockID
// in the tree it read, which set makes again on that block in the live document (repairLive).
type askRepair struct {
	blockID string
	set     func(*pmdoc.Node) bool
}

// repair runs set on node, the ask block carrying blockID in the tree settlement read, and records
// it for repairLive when it changed the block.
func (r *settlementReconciliation) repair(node *pmdoc.Node, blockID string, set func(*pmdoc.Node) bool) bool {
	if !set(node) {
		return false
	}
	r.repairs = append(r.repairs, askRepair{blockID: blockID, set: set})
	return true
}

// repairAsk repairs the server attributes of node, the ask block carrying blockID in the tree
// settlement read, to agree with ask (setAskServerAttributes), and records the repair for
// repairLive and, when event is not nil, the block.repaired it emits - each where withholdAnswers
// finds it again.
func (r *settlementReconciliation) repairAsk(node *pmdoc.Node, blockID string, ask model.Ask, event *model.Event) {
	found := readAskServerState(node)
	changed, wroteAnswer := setAskServerAttributes(node, ask, false)
	if !changed {
		return
	}
	rewrite := serverRewrite{node: node, ask: ask, found: found, answer: wroteAnswer, repair: len(r.repairs), event: -1}
	r.repairs = append(r.repairs, askRepair{blockID: blockID, set: askServerAttributes(ask, false)})
	if event != nil {
		rewrite.event = len(r.events)
		r.events = append(r.events, *event)
	}
	r.rewrites = append(r.rewrites, rewrite)
}

// repairLive makes the reconciliation's repairs on live, the document as it stands when settlement
// writes them, which a peer may have edited since settlement read the tree it reconciled, and
// reports whether any changed it. A repair lands on the first block carrying its id - the holder
// EnsureBlockIDs keeps the id for - while that block is an ask: one removed or retyped since takes
// none, and the settlement its removal scheduled reconciles it.
func (r *settlementReconciliation) repairLive(live *pmdoc.Node) bool {
	holders := make(map[string]*pmdoc.Node, len(r.repairs))
	for _, repair := range r.repairs {
		holders[repair.blockID] = nil
	}
	unfound := len(holders)
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if holder, wanted := holders[id]; id != "" && wanted && holder == nil {
			holders[id] = node
			unfound--
		}
		return unfound > 0
	})
	changed := false
	for _, repair := range r.repairs {
		if holder := holders[repair.blockID]; holder != nil && holder.Type == "ask" && repair.set(holder) {
			changed = true
		}
	}
	return changed
}

// writeLive writes the reconciliation's repairs into doc as it stands (rewriteLive, repairLive) and
// reports whether any changed it.
func (r *settlementReconciliation) writeLive(doc *crdt.Doc, origin any) (bool, error) {
	_, repaired, err := rewriteLive(doc, origin, r.repairLive)
	return repaired, err
}

// nameVersion completes the reconciliation at the version the settled document is at, writing
// the retractions whose reason names it and stamping it into every event that carries one.
func (r *settlementReconciliation) nameVersion(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	version int,
) error {
	for index, event := range r.events {
		switch payload := event.Payload.(type) {
		case model.BlockRepairedEventPayload:
			payload.Version = version
			r.events[index].Payload = payload
		case model.BlockInvalidEventPayload:
			payload.Version = version
			r.events[index].Payload = payload
		}
	}
	for _, ask := range r.retracted {
		retracted, err := WriteAskResolution(ctx, tx, ask, "retracted", fmt.Sprintf("%s %d", SettlementRetractionReason, version), SettlementActor)
		if err != nil {
			return fmt.Errorf("retract deleted ask block: %w", err)
		}
		r.events = append(r.events, documentAskEvent(
			owner, artifactID, "ask.resolved", SettlementActor, model.NewAskEventPayload(retracted, model.ReferenceChanges{}),
		))
	}
	return nil
}

// ErrInvalidAskBlock refuses a write that would leave an ask it writes or changes unreadable: an
// upload (a spec at issue creation, a new document or version) or an edit whose ask breaks the
// content rule paragraph+ bullet_list?, or an edit whose ask settlement cannot read.
type ErrInvalidAskBlock struct {
	Reason error
}

func (e *ErrInvalidAskBlock) Error() string { return e.Reason.Error() }

func (e *ErrInvalidAskBlock) Unwrap() error { return e.Reason }

// SettlementActor writes what a document decides on its own rather than any one person: the
// retraction of an ask whose block left the document, and an approval request's move when the
// version credits no writer or several. Crediting the room's last editor instead would put a
// change nobody made in their name.
var SettlementActor = model.Actor{Kind: "system", ID: "document-settlement"}

// SettlementRetractionReason opens the reason of every retraction settlement writes, followed by
// the version the block left in.
const SettlementRetractionReason = "removed from the document in version"

// settlementRetracted reports whether ask's retraction is one settlement wrote when its block
// left the document, which returning the block undoes. A retraction a person or a session wrote
// is their decision and stands, however the document moves. Retractions written before
// settlement began naming itself carry only its reason, so that prefix still counts - which is
// why resolveAsk refuses a caller reason that begins with it. It applies only to block asks,
// the rows loadAskBlocks reads, which is why an approval ask retracted as SettlementActor is
// never restored.
func settlementRetracted(ask model.Ask) bool {
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "retracted" {
		return false
	}
	return ask.Resolution.Actor.SameAs(SettlementActor) ||
		strings.HasPrefix(ask.Resolution.Reason, SettlementRetractionReason)
}

// reconcileAskBlocks makes the asks rows and the ask blocks of tree, the document settlement read,
// agree. A new block is authored by the update that introduced it (askBlockSources.author); one
// whose update observer has not run is left for the settlement that observer arms. A block copied
// from another document's ask opens none and shows that ask's state instead (copiedAskSources).
// before is the document's rendering as settlement read it ("" where it did not render), which the
// answers settlement writes back into returning blocks are weighed against (withholdAnswers).
func (s *Service) reconcileAskBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	credit settlementCredit,
	before string,
) (settlementReconciliation, error) {
	blocks, invalidBlocks, err := collectAskBlocksForSettlement(tree)
	if err != nil {
		return settlementReconciliation{}, err
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return settlementReconciliation{}, err
	}

	actor := credit.actor
	var reconciled settlementReconciliation
	for _, invalid := range invalidBlocks {
		delete(rows, invalid.id)
		if reconciled.repair(invalid.node, invalid.id, askInvalidAttribute(invalid.reason.Error())) {
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"block.invalid",
				actor,
				model.BlockInvalidEventPayload{
					BlockID:     invalid.id,
					Reason:      invalid.reason.Error(),
					DisturbedBy: actor,
				},
			))
		}
	}
	sources, err := copiedAskSources(ctx, tx, artifactID, owner, blocks, rows)
	if err != nil {
		return settlementReconciliation{}, err
	}
	for _, block := range blocks {
		if _, recorded := credit.askSources.authors[block.id]; recorded {
			reconciled.indexedAskBlocks = append(reconciled.indexedAskBlocks, block.id)
		}
		ask, exists := rows[block.id]
		if !exists {
			// A copy is read-only here: its source's state is written into the block, and
			// nothing this settlement does writes the source's row.
			if source, copied := sources[block.id]; copied {
				reconciled.repairAsk(block.node, block.id, source, nil)
				continue
			}
			blockActor, known := credit.askSources.author(block.id, actor)
			if !known {
				continue
			}
			ask, err = createAskBlock(ctx, tx, artifactID, owner, block, blockActor)
			if err != nil {
				return settlementReconciliation{}, err
			}
			changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, s.serverURL)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index new ask block: %w", err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner, artifactID, "ask.opened", blockActor, model.NewAskEventPayload(ask, changes),
			))
			reconciled.repairAsk(block.node, block.id, ask, nil)
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
		repaired := documentAskEvent(
			owner,
			artifactID,
			"block.repaired",
			actor,
			model.BlockRepairedEventPayload{BlockID: block.id, DisturbedBy: actor},
		)
		reconciled.repairAsk(block.node, block.id, ask, &repaired)
	}
	reconciled.withholdAnswers(artifactID, tree, before)

	for _, ask := range rows {
		// Only an open ask is retracted: an answered or resolved one is already closed, its
		// answer or resolution is the record, and the asks table forbids a resolved row that
		// still carries an answer.
		if ask.State != "open" {
			continue
		}
		reconciled.retracted = append(reconciled.retracted, ask)
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

// addedAskBlockIDs is the id of every readable ask block in after that before does not hold: one
// an edit inserted, retyped another block into, or repaired after a browser left it unreadable,
// wherever it lands - a blockquote, a list item - as the parser reads it, so an opener quoted in
// code adds none. One the edit moved, reworded or left alone keeps its id and is not among them. A
// nil before - a write with no readable starting tree, a repair - holds nothing, so every readable
// ask in after counts.
func addedAskBlockIDs(before, after *pmdoc.Node) map[string]struct{} {
	held := map[string]struct{}{}
	pmdoc.Walk(before, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			if _, err := parseAskBlock(node); err == nil {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				held[id] = struct{}{}
			}
		}
		return true
	})
	added := map[string]struct{}{}
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			if _, err := parseAskBlock(node); err == nil {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				if _, kept := held[id]; !kept {
					added[id] = struct{}{}
				}
			}
		}
		return true
	})
	return added
}

// addedAskBlocks counts the readable ask blocks in after whose block id no readable ask block in
// before carries: one an edit inserted, retyped another block into, or repaired after a browser left
// it unreadable, wherever it lands - a blockquote, a list item - as the parser reads it, so an opener
// quoted in code adds none. One the edit moved, reworded or left alone keeps its id and counts
// nothing.
func addedAskBlocks(before, after *pmdoc.Node) int {
	return len(addedAskBlockIDs(before, after))
}

// newAskMarkdown is what an uploaded version can say of an ask, for one refuseChangedAsks call: its
// rendering alone, taken as an upload is (asUploaded) - without anchor marks, and with its
// server-owned attributes (`state`, the answer, `invalid`) at their defaults, since an upload's are
// discarded for the ask row's. The attributes a reader's browser derives are never rendered. A new
// version is markdown, so this is how a version says it carries an ask unchanged.
//
// An ask can hold a table. A document's render writes its tables' spans out under one budget for
// the whole document (pmdoc.Render), and a version uploaded from that markdown holds them as the
// cells they were written as. So the asks one call renders share one budget (pmdoc.SpanBudget),
// spent in document order, and each ask is written as the stored markdown holds it wherever the
// document's render still had the budget for its table. A budget for each ask would write every
// ask past the first budget's end with cells the document never wrote, and spend a budget for
// every ask the document holds. Past the point a budget runs out, a table is written with fewer
// span cells than its spans cover. Where only the asks' tables have spent the budget before an
// ask, the fingerprint and the stored markdown write its table alike, and the ask reads as changed,
// refusing an upload of the document's own markdown, when its table as written holds a body row
// shorter than its widest, which the parser pads when it reads the upload: a body cell spanning
// columns under a wider header, a rowspan, or rows the budget ran out partway through. The header
// is written as wide as the widest row, so it never counts, and a table whose rows each hold as
// many cells as written is kept. Where a table outside the asks spent some of it first, the
// document can have written an ask's table with fewer span cells than the fingerprint writes, and
// the ask reads as changed unless padding its stored rows gives the fingerprint's cells; one the
// document still had the budget for is written alike and judged as above. An ask over a
// span-free table with a short body row reads as changed the same way.
func newAskMarkdown() func(ask *pmdoc.Node) (string, error) {
	budget := pmdoc.NewSpanBudget()
	return func(ask *pmdoc.Node) (string, error) {
		return budget.Render(asUploaded(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{ask}}))
	}
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

// askReadability visits each ask block in document order with the reason it cannot be read, nil
// when it can: settlement's parse, then the content rule the browser editor holds an ask to (an ask
// breaking it is dropped from the shared document when an editor renders it, and settlement then
// retracts it). An ask repeating an earlier ask's id is unreadable as a duplicate, so every ask
// gets an answer where collectAskBlocksForSettlement stops at the first repeat.
func askReadability(tree *pmdoc.Node, visit func(id string, reason error) bool) {
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		var reason error
		if _, duplicate := seen[id]; duplicate {
			reason = fmt.Errorf("duplicate ask block id %q", id)
		} else if _, reason = parseAskBlock(node); reason == nil {
			reason = pmdoc.AskContentError(node)
		}
		seen[id] = struct{}{}
		return visit(id, reason)
	})
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
			questionParts = append(questionParts, strings.TrimSpace(pmdoc.TextContent(child)))
		case "bullet_list":
			for _, item := range child.Children {
				if len(item.Children) == 0 {
					return askBlock{}, fmt.Errorf("ask block %q has an empty option", blockID)
				}
				label, description, found := strings.Cut(strings.TrimSpace(pmdoc.TextContent(item.Children[0])), ": ")
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

// copiedAskSources finds the ask each block of blocks was copied from, keyed by block id, among the
// blocks no ask of this document indexes (rows). Block ids are unique per document only
// (asks_block_id_unique is on block_artifact_id and block_id), so a document copied from another
// carries its ask blocks' ids, and an id an author chose, such as `decision`, can name unrelated
// questions on two documents. A block is therefore a copy only of an ask on another document of
// the same owner - the same issue, or for a project document the same project's other documents -
// that indexes the same block id and asks the block's question, or once asked it: the copy can be
// of an earlier version, from before the ask was reworded (an ask.opened or ask.edited event names
// each question it has asked). A block reworded on the copy is a new question and opens an ask. An
// ask settlement retracted because its block left its document is no source: a block cut from one
// document and pasted into another is the only one left, and opens an ask as any new block does.
// Of several sources, the earliest asked is the original, since a copy that opened asks of its own
// before copies were recognised came after it. The copy shows its source's state as of this
// settlement; answering the source later changes the copy at the copy's next settlement. The rows
// are read, never locked or written: settlement holds this document's owner row and room lock, and
// a source's answer route takes the source's ask row before its own document's room.
func copiedAskSources(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	blocks []askBlock,
	rows map[string]model.Ask,
) (map[string]model.Ask, error) {
	var unindexed []string
	for _, block := range blocks {
		if _, indexed := rows[block.id]; !indexed {
			unindexed = append(unindexed, block.id)
		}
	}
	if len(unindexed) == 0 {
		return nil, nil
	}
	// The owner's documents come first (artifacts_issue_key for an issue), then each one's asks
	// under the copied ids through asks_block_id_unique.
	documents := `d.issue_key = $3`
	ownerKey := owner.Project
	if owner.IssueKey != nil {
		ownerKey = *owner.IssueKey
	} else {
		documents = `d.issue_key is null and d.session_id is null and d.project_key = $3`
	}
	query, err := tx.Query(ctx, `
		select `+AskColumns+`
		from artifacts d
		join asks a on a.block_artifact_id = d.id and a.block_id = any($1)
		where `+documents+` and d.id <> $2
		order by a.created_at, a.id
	`, unindexed, artifactID, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("load copied ask sources: %w", err)
	}
	defer query.Close()
	candidates := map[string][]model.Ask{}
	for query.Next() {
		ask, err := ScanAsk(query)
		if err != nil {
			return nil, fmt.Errorf("scan copied ask source: %w", err)
		}
		if settlementRetracted(ask) {
			continue
		}
		candidates[*ask.BlockID] = append(candidates[*ask.BlockID], ask)
	}
	if err := query.Err(); err != nil {
		return nil, fmt.Errorf("iterate copied ask sources: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sources := map[string]model.Ask{}
	var reworded []askBlock
	for _, block := range blocks {
		matched := false
		for _, ask := range candidates[block.id] {
			if ask.Question == block.question {
				sources[block.id] = ask
				matched = true
				break
			}
		}
		if !matched && len(candidates[block.id]) > 0 {
			reworded = append(reworded, block)
		}
	}
	if len(reworded) == 0 {
		return sources, nil
	}
	asked, err := questionsAsked(ctx, tx, reworded, candidates)
	if err != nil {
		return nil, err
	}
	for _, block := range reworded {
		for _, ask := range candidates[block.id] {
			if _, once := asked[ask.ID][block.question]; once {
				sources[block.id] = ask
				break
			}
		}
	}
	return sources, nil
}

// questionsAsked is every question each candidate of the blocks has asked, keyed by ask id, read
// from the ask.opened and ask.edited events that record its questions (events_ask_payload_id).
func questionsAsked(ctx context.Context, tx pgx.Tx, blocks []askBlock, candidates map[string][]model.Ask) (map[string]map[string]struct{}, error) {
	var ids []string
	for _, block := range blocks {
		for _, ask := range candidates[block.id] {
			ids = append(ids, ask.ID)
		}
	}
	rows, err := tx.Query(ctx, `
		select payload->>'id', coalesce(payload->>'question', ''), coalesce(payload#>>'{previous,question}', '')
		from events
		where type in ('ask.opened', 'ask.edited') and payload->>'id' = any($1)
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load questions copied asks asked: %w", err)
	}
	defer rows.Close()
	asked := map[string]map[string]struct{}{}
	for rows.Next() {
		var id, question, previous string
		if err := rows.Scan(&id, &question, &previous); err != nil {
			return nil, fmt.Errorf("scan question a copied ask asked: %w", err)
		}
		if asked[id] == nil {
			asked[id] = map[string]struct{}{}
		}
		for _, text := range []string{question, previous} {
			if text != "" {
				asked[id][text] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate questions copied asks asked: %w", err)
	}
	return asked, nil
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

// askServerAttributeNames are the attributes of an ask block the server keeps in agreement with
// its ask row (setAskServerAttributes).
var askServerAttributeNames = [...]string{"state", "answered_by", "answered_at", "selected", "answer", "invalid"}

// askServerAttributes is setAskServerAttributes for ask as it stands now, leaving its answer out
// when withholdAnswer, for a repair to make again on another tree (repairLive).
func askServerAttributes(ask model.Ask, withholdAnswer bool) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		changed, _ := setAskServerAttributes(node, ask, withholdAnswer)
		return changed
	}
}

// setAskServerAttributes makes node's server attributes agree with ask, and reports whether it
// changed any and whether it wrote an answer (pmdoc.IsAnswerAttribute): its words, or the options
// it selects, which are caller text the server keeps. With withholdAnswer it leaves the answer out
// of the block, taking off any the block holds, and writes the rest: the block still says who
// answered and when.
func setAskServerAttributes(node *pmdoc.Node, ask model.Ask, withholdAnswer bool) (changed, answered bool) {
	desired := pmdoc.Attrs{"state": ask.State}
	if ask.State == "answered" && ask.Answer != nil {
		desired["answered_by"] = ask.Answer.User
		desired["answered_at"] = ask.Answer.At.UTC().Format(time.RFC3339Nano)
		if !withholdAnswer {
			desired["selected"] = append([]string(nil), ask.Answer.Selected...)
			if ask.Answer.Text != nil {
				desired["answer"] = *ask.Answer.Text
			}
		}
	}
	for _, name := range askServerAttributeNames {
		want, present := desired[name]
		got, exists := node.Attrs[name]
		if present {
			if exists && askServerAttributeEqual(got, want) {
				continue
			}
			node.Attrs[name] = want
			changed = true
			answered = answered || pmdoc.IsAnswerAttribute(name)
			continue
		}
		if exists {
			delete(node.Attrs, name)
			changed = true
		}
	}
	return changed, answered
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

// askInvalidAttribute is setAskInvalidAttribute for reason, for a repair to make again on another
// tree (settlementReconciliation.repair).
func askInvalidAttribute(reason string) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool { return setAskInvalidAttribute(node, reason) }
}

func documentAskEvent(owner artifactOwner, artifactID, eventType string, actor model.Actor, payload any) model.Event {
	event := model.Event{IssueKey: owner.IssueKey, Type: eventType, Actor: actor, Payload: payload}
	if owner.IssueKey == nil {
		event.ArtifactID = &artifactID
	}
	return event
}
