package docs

import (
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// askRepairStep is the step a refusal names for plan, the one document edit that repairs the
// decision, and the ask edit after it. It says what that edit keeps of a decision nested in the
// blocks it keeps - the same blocks - and that one the content rule leaves nothing of is deleted
// whole (askRepairKeepsNothingOfItsOwn), which the plan names in place of its children.
//
// Where the edit deletes a decision, it carries what the client refuses: a document edit may not
// take a decision block out of the document while its ask is open
// (refuseRemovingOpenDecisionBlocks), and that ask is answered, resolved, or the decision
// repaired by hand, before the delete. A decision that never held a question never had a row
// (addedAskBlocks, readableAskBlockIDs), so the common case passes that guard untouched.
func askRepairStep(plan askRepairPlan) string {
	step := ", so delete the blocks to drop, all in one document edit (dispatch doc-edit, deleting a block " +
		"by its id; one edit holding one delete per block), which keeps the question's paragraphs and the " +
		"first list after them, and keeps the same of every decision nested in the blocks it keeps, deleting " +
		"whole a nested decision the rule leaves nothing of" +
		"; the blocks to delete are " + strings.Join(plan.drop, ", ")
	if plan.decision {
		step += "; a decision among them whose ask is still open cannot be deleted by a document edit, so " +
			"answer or resolve that ask, or repair the decision in the document editor, before this delete"
	}
	return step + "; then edit the ask"
}

// askRepairPlan is the one document edit that repairs a decision breaking its content rule.
type askRepairPlan struct {
	// drop is the id of each block to delete, Go-quoted, in document order. It is empty when no
	// one edit of deletes leaves the decision valid.
	drop []string
	// decision reports that a block the edit deletes is a decision, or holds one, which the
	// client refuses while that decision's ask is open (refuseRemovingOpenDecisionBlocks).
	decision bool
	// blocked is why the deletes the plan would have named leave the edited decision, or one
	// nested in it, unreadable, nil when they do not or when a decision holding it is the one
	// they would leave unreadable (holder).
	blocked error
	// holder reports whether the deletes the plan would have named leave a decision holding the
	// edited one unreadable; no ask edit of this decision repairs that.
	holder bool
}

// planAskRepair is the one document edit that repairs ask, the decision of tree being edited,
// which breaks its content rule. It deletes the blocks ask's own rule leaves no room for
// (askBlocksToDrop) and, in every decision nested in the blocks it keeps that breaks the rule,
// that decision's own, as deep as they nest: the rule reads the decisions nested in one
// (pmdoc.AskContentError), so repairing them is part of repairing this one, and a nested
// decision's block is deleted by its id like any other. A nested decision the rule leaves
// nothing of is deleted whole (askRepairKeepsNothingOfItsOwn).
//
// No document edit writes a decision a question, for the reason refuseEmptyQuestionEdit holds. So
// a decision holding no paragraph with text keeps nothing its rule leaves room for, the re-read
// below finds what the deletes leave unreadable, and the refusal says the decision is repaired
// where a person writes it.
//
// A document edit is refused where it leaves a decision it wrote or changed unreadable
// (validateEditedAskBlocks), and a delete inside a decision changes every decision holding it, so
// the plan is held to what one such edit allows: with the deletes applied, ask, every decision
// nested in it that loses a block and every decision holding ask must read strictly
// (readAskBlockStrictly, which the deletes alone cannot give a decision with no question left, or
// one whose options the parse cannot read). Where one would not, the plan names no delete and
// carries why: a decision holding ask, which this repair may not leave unreadable (holder), or
// ask itself, or one nested in it (blocked). Either way the caller is sent to no other decision's
// ask edit, which can be one no caller may make.
func planAskRepair(tree, ask *pmdoc.Node) askRepairPlan {
	drop, changed := askRepairDrops(ask)
	if len(drop) == 0 {
		return askRepairPlan{}
	}
	dropped := make(map[*pmdoc.Node]struct{}, len(drop))
	for _, node := range drop {
		dropped[node] = struct{}{}
	}
	repaired := asksByBlockID(withoutNodes(tree, dropped))
	for _, id := range askHolderIDs(tree, ask) {
		if node, found := repaired[id]; found {
			if reason := readAskBlockStrictly(node); reason != nil {
				return askRepairPlan{holder: true}
			}
		}
	}
	for _, id := range changed {
		if node, found := repaired[id]; found {
			if reason := readAskBlockStrictly(node); reason != nil {
				return askRepairPlan{blocked: reason}
			}
		}
	}
	return askRepairPlan{drop: droppedBlockIDs(ask, dropped), decision: dropsADecision(dropped)}
}

// askRepairDrops is every block to delete inside ask, an ask breaking its content rule, and the
// block id of each decision losing one - ask first, then the decisions nested in it, as deep as
// they nest. A decision whose own blocks keep the rule loses none of them: only the decisions
// nested in it that break it are repaired. One whose own blocks the rule leaves nothing of is
// deleted whole (askRepairKeepsNothingOfItsOwn), and so is named in place of its children, which
// leaves no decision for the re-read to find unreadable where deleting it is what repairs the
// decision holding it.
func askRepairDrops(ask *pmdoc.Node) (drop []*pmdoc.Node, changed []string) {
	if pmdoc.AskContentError(ask) == nil {
		return nil, nil
	}
	kept := ask.Children
	if askBreaksItsOwnRule(ask) {
		var own []*pmdoc.Node
		own, kept = askBlocksToDrop(ask.Children)
		drop = append(drop, own...)
	}
	var nested []string
	for _, inner := range nestedAsks(kept) {
		if askRepairKeepsNothingOfItsOwn(inner) {
			drop = append(drop, inner)
			continue
		}
		innerDrop, innerChanged := askRepairDrops(inner)
		drop = append(drop, innerDrop...)
		nested = append(nested, innerChanged...)
	}
	if len(drop) == 0 {
		return nil, nil
	}
	id, _ := ask.Attrs[pmdoc.BlockIDAttr].(string)
	return drop, append([]string{id}, nested...)
}

// askRepairKeepsNothingOfItsOwn reports whether deleting the blocks ask's content rule leaves no
// room for would keep none of them: its own blocks break the rule and it holds no paragraph with
// text, so the rule leaves room for nothing it holds. Such a decision is deleted whole by the one
// edit that repairs the decision holding it, since the deletes alone would leave a block no parse
// reads as a decision, and no document edit writes another person's decision the question it
// would need.
func askRepairKeepsNothingOfItsOwn(ask *pmdoc.Node) bool {
	if !askBreaksItsOwnRule(ask) {
		return false
	}
	_, kept := askBlocksToDrop(ask.Children)
	return len(kept) == 0
}

// dropsADecision reports whether a node of dropped is a decision or holds one, so that the
// document edit the plan names takes a decision block out of the document. The client refuses
// such an edit while that decision's ask is open (refuseRemovingOpenDecisionBlocks), so the
// refusal says what is answered, resolved or repaired first.
func dropsADecision(dropped map[*pmdoc.Node]struct{}) bool {
	for node := range dropped {
		if len(nestedAsks([]*pmdoc.Node{node})) > 0 {
			return true
		}
	}
	return false
}

// askBreaksItsOwnRule reports whether ask's own blocks break the content rule, where
// pmdoc.AskContentError reads the decisions nested in it too: it holds ask to the rule over its
// blocks' types alone, which is all the rule reads of them.
func askBreaksItsOwnRule(ask *pmdoc.Node) bool {
	types := make([]*pmdoc.Node, len(ask.Children))
	for index, child := range ask.Children {
		if child != nil {
			types[index] = &pmdoc.Node{Type: child.Type}
		}
	}
	return pmdoc.AskContentError(withChildren(ask, types)) != nil
}

// nestedAsks is each decision nested in blocks that no nearer decision holds, in document order.
func nestedAsks(blocks []*pmdoc.Node) []*pmdoc.Node {
	var found []*pmdoc.Node
	var collect func(node *pmdoc.Node)
	collect = func(node *pmdoc.Node) {
		switch {
		case node == nil:
		case node.Type == "ask":
			found = append(found, node)
		default:
			for _, child := range node.Children {
				collect(child)
			}
		}
	}
	for _, block := range blocks {
		collect(block)
	}
	return found
}

// askBlocksToDrop splits the children of a broken ask into the ones to delete and the ones to
// keep, each in document order. It keeps the run of paragraphs starting at the first paragraph
// holding text, up to the next other block, and the bullet list right after them; it drops every
// other child, the paragraphs holding no text before that run among them, since a question read
// from those alone is empty. The caller checks what the kept children leave reads strictly: a
// decision nested in them can still break the rule, and a kept list can hold an option the parse
// cannot read.
//
// A decision no paragraph of which holds text keeps nothing: it asks nothing, and no document
// edit can give it a question (refuseEmptyQuestionEdit holds that reason). Where it is the
// decision being edited the caller's re-read finds what that leaves unreadable; where it is
// nested in one, deleting it whole is what repairs the decision holding it
// (askRepairKeepsNothingOfItsOwn).
func askBlocksToDrop(children []*pmdoc.Node) (drop, kept []*pmdoc.Node) {
	first := slices.IndexFunc(children, func(child *pmdoc.Node) bool {
		return child.Type == "paragraph" && strings.TrimSpace(pmdoc.TextContent(child)) != ""
	})
	if first < 0 {
		return children, nil
	}
	end := first
	for end < len(children) && children[end].Type == "paragraph" {
		end++
	}
	if end < len(children) && children[end].Type == "bullet_list" {
		end++
	}
	drop = make([]*pmdoc.Node, 0, len(children)-(end-first))
	drop = append(drop, children[:first]...)
	return append(drop, children[end:]...), children[first:end]
}

// withChildren is a copy of node holding children in place of its own.
func withChildren(node *pmdoc.Node, children []*pmdoc.Node) *pmdoc.Node {
	copied := *node
	copied.Children = children
	return &copied
}

// withoutNodes is a copy of node holding neither the nodes of dropped nor anything nested in
// them, or nil when node itself is one. A node holding none of them is shared, not copied.
func withoutNodes(node *pmdoc.Node, dropped map[*pmdoc.Node]struct{}) *pmdoc.Node {
	if _, drop := dropped[node]; drop {
		return nil
	}
	if len(node.Children) == 0 {
		return node
	}
	children := make([]*pmdoc.Node, 0, len(node.Children))
	changed := false
	for _, child := range node.Children {
		kept := child
		if child != nil {
			kept = withoutNodes(child, dropped)
		}
		changed = changed || kept != child
		if kept != nil {
			children = append(children, kept)
		}
	}
	if !changed {
		return node
	}
	return withChildren(node, children)
}

// asksByBlockID is each ask block of tree by its block id. Block ids are unique in a settled
// document (pmdoc.BlockIDRepairCount), which the caller has checked.
func asksByBlockID(tree *pmdoc.Node) map[string]*pmdoc.Node {
	found := map[string]*pmdoc.Node{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			found[id] = node
		}
		return true
	})
	return found
}

// askHolderIDs is the block id of each decision holding ask in tree, outermost first.
func askHolderIDs(tree, ask *pmdoc.Node) []string {
	var holders []string
	var find func(node *pmdoc.Node, holding []string) bool
	find = func(node *pmdoc.Node, holding []string) bool {
		if node == ask {
			holders = holding
			return true
		}
		if node.Type == "ask" {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				holding = append(slices.Clone(holding), id)
			}
		}
		for _, child := range node.Children {
			if child != nil && find(child, holding) {
				return true
			}
		}
		return false
	}
	find(tree, nil)
	return holders
}

// droppedBlockIDs is the id of each node of dropped, Go-quoted, in the order ask holds them. A
// node nested in another the edit deletes is not named: deleting the block holding it takes it.
func droppedBlockIDs(ask *pmdoc.Node, dropped map[*pmdoc.Node]struct{}) []string {
	ids := make([]string, 0, len(dropped))
	var collect func(node *pmdoc.Node)
	collect = func(node *pmdoc.Node) {
		if _, drop := dropped[node]; drop {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			ids = append(ids, strconv.Quote(id))
			return
		}
		for _, child := range node.Children {
			if child != nil {
				collect(child)
			}
		}
	}
	collect(ask)
	return ids
}
