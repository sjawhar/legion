package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
// retraction of an ask whose block left the document, and a settlement derived from ambiguous
// browser content with no known latest editor. A service write's new ask and an approval move name
// the actor that introduced them instead.
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

// reconcileAskBlocks indexes tree's ask blocks against their rows for a settlement whose authors and
// ask-block sources are credit. A new block is authored by the update that introduced it
// (askBlockSources.author); one whose update observer has not run is left for the settlement that
// observer arms.
func (s *Service) reconcileAskBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	credit settlementCredit,
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
	for _, block := range blocks {
		if _, recorded := credit.askSources.authors[block.id]; recorded {
			reconciled.indexedAskBlocks = append(reconciled.indexedAskBlocks, block.id)
		}
		ask, exists := rows[block.id]
		if !exists {
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
			reconciled.repair(block.node, block.id, askServerAttributes(ask))
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
		if reconciled.repair(block.node, block.id, askServerAttributes(ask)) {
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"block.repaired",
				actor,
				model.BlockRepairedEventPayload{BlockID: block.id, DisturbedBy: actor},
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

// askBlockIDs collects the id of every ask block tree holds. It never returns nil: a document
// with none still returns an empty map, which askBlockSources.author reads as "no block is
// unindexed" rather than as an unknown baseline (askBlockSources.observed).
func askBlockIDs(tree *pmdoc.Node) map[string]struct{} {
	var ids map[string]struct{}
	walkAskBlocks(tree, func(id string) {
		if id == "" {
			return
		}
		if ids == nil {
			ids = make(map[string]struct{})
		}
		ids[id] = struct{}{}
	})
	if ids == nil {
		return map[string]struct{}{}
	}
	return ids
}

// askBlockOrder lists the id of every ask block tree holds in document order, "" for a block
// with none.
func askBlockOrder(tree *pmdoc.Node) []string {
	var ids []string
	walkAskBlocks(tree, func(id string) { ids = append(ids, id) })
	return ids
}

// walkAskBlocks calls visit with the id of each ask block tree holds, in document order, wherever
// it stands: in a blockquote, a list item or another typed block. It does not descend into text,
// which holds no block.
func walkAskBlocks(tree *pmdoc.Node, visit func(id string)) {
	if tree == nil {
		return
	}
	if tree.Type == "ask" {
		id, _ := tree.Attrs[pmdoc.BlockIDAttr].(string)
		visit(id)
	}
	for _, child := range tree.Children {
		if child.Type != "text" {
			walkAskBlocks(child, visit)
		}
	}
}

// stampedAsk is an ask block a block-id stamp gave a new id: previous, the id it had ("" for
// none), minted, the one it has, and copied, whether another block still holds previous - a repeat
// the stamp re-minted, a copy of the block that keeps the id.
type stampedAsk struct {
	previous, minted string
	copied           bool
}

// stampedAskBlocks pairs the ask block ids of one tree before and after a block-id stamp, which
// changes ids and nothing else, and returns each ask whose id the stamp changed.
func stampedAskBlocks(before, after []string) []stampedAsk {
	var held map[string]struct{}
	var stamped []stampedAsk
	for index, previous := range before {
		if after[index] == previous {
			continue
		}
		if held == nil {
			held = make(map[string]struct{}, len(after))
			for _, id := range after {
				held[id] = struct{}{}
			}
		}
		_, copied := held[previous]
		stamped = append(stamped, stampedAsk{previous: previous, minted: after[index], copied: copied})
	}
	return stamped
}

// recordStampedAskBlocks records ask blocks whose ids a settlement's own stamp minted. The room's
// update observer skips that stamp, so no update it renders introduces those ids: each is marked
// seen, and keeps the author recorded for the id it replaced, unless it is a copy of the block
// that keeps that id. The id it replaced is forgotten from the room's held blocks too, so a later
// typed block that reuses that literal id - author-controlled text, unlike a minted one - is seen
// as new rather than mistaken for the one this stamp just retired. The caller holds state.mu.
func (state *roomState) recordStampedAskBlocks(stamped []stampedAsk) {
	for _, ask := range stamped {
		if state.askBlocks != nil {
			state.askBlocks[ask.minted] = struct{}{}
			delete(state.askBlocks, ask.previous)
		}
		if author, recorded := state.askAuthors[ask.previous]; recorded && !ask.copied {
			state.askAuthors[ask.minted] = author
		} else {
			delete(state.askAuthors, ask.minted)
		}
	}
}

// registerAskAuthors credits actor as the author of every id in ids a committed transaction's
// write or a service mutation introduces, computed from its own before/after trees at the write
// site rather than guessed from whichever update's observer later renders a catch-up that happens
// to include it: the room's update observer consumes each entry here the first time it sees the
// id (observeAskBlocks), and never prunes one it has not consumed, so attribution survives however
// many other updates land first (LEGION-503). It never overwrites an id a more specific
// registration already gave an author - a stamp's own carry-forward
// (carryForwardRenamedAskAuthors), which runs first within the same write. The caller holds
// state.mu.
func (state *roomState) registerAskAuthors(ids map[string]struct{}, actor model.Actor) {
	for id := range ids {
		if _, registered := state.pendingAskAuthors[id]; registered {
			continue
		}
		if state.pendingAskAuthors == nil {
			state.pendingAskAuthors = make(map[string]model.Actor, len(ids))
		}
		state.pendingAskAuthors[id] = actor
	}
}

// carryForwardRenamedAskAuthors registers the author recorded for each renamed ask block's
// previous id as the pending author of its minted one, for a stamp whose own update the room's
// observer renders normally - ApplyOps's own id repair - unlike a settlement's or the backfill's
// suppressed stamp (recordStampedAskBlocks), which marks the minted id seen itself because no
// observer ever will. Marking it seen here instead would race a concurrent, unrelated update's own
// render, which replaces state.askBlocks wholesale: the normal observer that renders this stamp's
// own update marks it seen when it runs, and pendingAskAuthors survives until then because
// registerAskAuthors's caller never prunes it (LEGION-503). A rename with no author to carry
// forward registers nothing, leaving the block to the observer's own fallback when it runs. The
// caller holds state.mu.
func (state *roomState) carryForwardRenamedAskAuthors(stamped []stampedAsk) {
	for _, ask := range stamped {
		author, recorded := state.askAuthors[ask.previous]
		if !recorded || ask.copied {
			continue
		}
		if state.pendingAskAuthors == nil {
			state.pendingAskAuthors = make(map[string]model.Actor, len(stamped))
		}
		state.pendingAskAuthors[ask.minted] = author
	}
}

// observeAskBlocks records ids, the ask blocks the room holds after an update its observer
// rendered, and attributes each one not already recorded: first to whichever write registered it
// (registerAskAuthors, carryForwardRenamedAskAuthors - consumed here the first time its id is
// seen, and never pruned, since no render order can tell which write introduced a block another
// update's catch-up happens to include first), otherwise to author, the room's one connected
// browser or SettlementActor when several are, nil for none - an id no write registered can only
// have come from a browser, whichever update's own observer ends up being the one to see it
// (LEGION-503). It forgets the author of a block the room no longer holds. A room whose earlier
// ask blocks are unknown - its load could not read the document - takes ids as its baseline and
// records no author at all, since it cannot tell which blocks are new; its caller passes author as
// nil for the same reason on a fresh load. The caller holds state.mu.
func (state *roomState) observeAskBlocks(ids map[string]struct{}, author *model.Actor) {
	for id := range state.askAuthors {
		if _, held := ids[id]; !held {
			delete(state.askAuthors, id)
		}
	}
	if state.askBlocks != nil {
		for id := range ids {
			if _, seen := state.askBlocks[id]; seen {
				continue
			}
			if registered, pending := state.pendingAskAuthors[id]; pending {
				delete(state.pendingAskAuthors, id)
				if state.askAuthors == nil {
					state.askAuthors = make(map[string]model.Actor)
				}
				state.askAuthors[id] = registered
				continue
			}
			if author == nil {
				continue
			}
			if state.askAuthors == nil {
				state.askAuthors = make(map[string]model.Actor)
			}
			state.askAuthors[id] = *author
		}
	}
	state.askBlocks = ids
}

// askBlockSources is where the ask blocks a settlement indexes came from, copied from the room's
// state once the settlement holds the tree it reconciles: observed, the ask blocks the room's
// update observer has seen (nil when it cannot tell), and authors, the author of the update that
// introduced each block not yet indexed.
type askBlockSources struct {
	observed map[string]struct{}
	authors  map[string]model.Actor
}

// askSources copies the room's ask-block sources for a settlement. The caller holds state.mu.
func (state *roomState) askSources() askBlockSources {
	return askBlockSources{observed: maps.Clone(state.askBlocks), authors: maps.Clone(state.askAuthors)}
}

// author names who wrote the ask block carrying id, which no ask row indexes yet: the author of
// the update that introduced it, as the update observer recorded it, or, for a block a settlement's
// own id-repair renamed, the author carried forward from the id it replaced
// (recordStampedAskBlocks). ok is false for a block the observer has not seen - its update is in
// the tree, but its observer has not run - which that observer records, arming the settlement that
// indexes it. A block the observer saw without an author - one the room held when it loaded, one
// an update with no source introduced, or one a stamp renamed while its own update's observer was
// still behind, so no author was there to carry forward - is fallback's, named after the
// settlement's own actor rather than its original editor.
func (sources askBlockSources) author(id string, fallback model.Actor) (model.Actor, bool) {
	if actor, recorded := sources.authors[id]; recorded {
		return actor, true
	}
	if sources.observed != nil {
		if _, seen := sources.observed[id]; !seen {
			return model.Actor{}, false
		}
	}
	return fallback, true
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

// askServerAttributes is setAskServerAttributes for ask as it stands now, for a repair to make
// again on another tree (settlementReconciliation.repair).
func askServerAttributes(ask model.Ask) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool { return setAskServerAttributes(node, ask) }
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
