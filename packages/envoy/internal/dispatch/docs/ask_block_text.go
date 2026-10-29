package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// AskBlockText is the client-owned half of an ask block: the text a person or a session writes,
// as opposed to the server-owned answer state settlement projects back into the block.
type AskBlockText struct {
	Question string
	Options  []model.AskOption
	Multiple bool
	Urgency  string
}

// AskBlockEdit names the fields one edit changes. A nil field is not written, and the block keeps
// the nodes it already has for it - with their inline marks and their inner block ids. An ask's
// row holds the question as plain text (`nodeText`), so rebuilding the body from the row would
// flatten a question's bold, code and links and mint fresh ids for the paragraphs a human's
// comment anchors point into: an edit that names only the urgency must touch none of that.
type AskBlockEdit struct {
	Question *string
	Options  *[]model.AskOption
	Multiple *bool
	Urgency  *string
}

// ErrAskBlockUnrepresentable reports text the `:::ask` block cannot carry unchanged. The ask row
// is a projection of the block, so text the block would alter is refused rather than stored:
// storing it would leave the row and the block disagreeing, and the next settlement would
// overwrite the row from the block.
type ErrAskBlockUnrepresentable struct {
	Field  string
	Reason string
}

func (e *ErrAskBlockUnrepresentable) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

// SetAskBlockText applies edit to the ask block blockID of artifactID and returns what the block
// then parses back to, which is what its ask row must hold. The caller writes the row from the
// return value, never from its own request, so normalisation cannot leave the two disagreeing,
// and a field the edit did not name comes back as the block's own.
//
// It returns *ErrAskBlockUnrepresentable, having written nothing, when the block would not carry
// the text unchanged.
func (s *Service) SetAskBlockText(
	ctx context.Context,
	artifactID, blockID string,
	edit AskBlockEdit,
	actor model.Actor,
) (AskBlockText, error) {
	var stored *AskBlockText
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		tree, err := startTree(current, doc)
		if err != nil {
			return err
		}
		block, err := askBlockOf(tree, blockID)
		if err != nil {
			return err
		}
		// The content rule is decided once, before the edit is built: askBlockChildren then
		// yields the question's paragraphs and at most one list, which keeps the rule.
		if err := refuseAskContentEdit(tree, block); err != nil {
			return err
		}
		want := AskBlockText{
			Question: block.question,
			Options:  block.options,
			Multiple: block.multiple,
			Urgency:  block.urgency,
		}
		attributes := pmdoc.Attrs{}
		if edit.Multiple != nil {
			want.Multiple = *edit.Multiple
			attributes["multiple"] = *edit.Multiple
		}
		if edit.Urgency != nil {
			want.Urgency = *edit.Urgency
			attributes["urgency"] = *edit.Urgency
		}
		if edit.Question != nil {
			want.Question = normalizeAskQuestion(*edit.Question)
		}
		if edit.Options != nil {
			want.Options = normalizeAskOptions(*edit.Options)
		}

		wroteBody := askBlockBodyChanges(block, edit)
		var next *pmdoc.Node
		if wroteBody {
			children, err := askBlockChildren(block, edit, pmdoc.NewWriteBudget())
			if err != nil {
				return err
			}
			next, err = pmdoc.SetBlockBody(tree, blockID, children, attributes)
			if err != nil {
				return err
			}
		} else {
			// Attributes alone: every child keeps its nodes, its marks and its identity.
			next, err = pmdoc.SetBlockAttributes(tree, blockID, attributes)
			if err != nil {
				return err
			}
		}
		if err := verifyAskBlockRoundTrip(next, blockID, want, wroteBody); err != nil {
			return err
		}
		stored = &want
		if next.EqualWithBlockIDs(tree) {
			return nil
		}
		fragment := doc.GetXmlFragment(fragmentName)
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, next)
		})
		return updateErr
	})
	// An edit that re-sends the values the block already holds writes nothing, which the live
	// path reports as ErrNoChanges. It is the same ask the caller asked for, so it succeeds.
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		var unrepresentable *ErrAskBlockUnrepresentable
		if errors.As(err, &unrepresentable) {
			return AskBlockText{}, err
		}
		return AskBlockText{}, wrapUnlessTooLarge(err, "write ask block text")
	}
	// stored is set by the closure, which applyLive runs before it can return nil or
	// ErrNoChanges; a nil here would mean it never ran, which must not read as an empty ask.
	if stored == nil {
		return AskBlockText{}, fmt.Errorf("ask block %q was never read", blockID)
	}
	return *stored, nil
}

// askBlockBodyChanges reports whether edit still names a part of the body to rewrite, after the
// parts it names but does not change have been dropped (withoutUnchanged).
func askBlockBodyChanges(current askBlock, edit AskBlockEdit) bool {
	edit = withoutUnchanged(current, edit)
	return edit.Question != nil || edit.Options != nil
}

// withoutUnchanged is edit without the question or options it names but does not change. An
// idempotent retry sends the whole ask back, and rebuilding nodes that already say the same thing
// would mint fresh inner block ids and orphan the anchors inside them for no change at all.
func withoutUnchanged(current askBlock, edit AskBlockEdit) AskBlockEdit {
	if edit.Question != nil && normalizeAskQuestion(*edit.Question) == current.question {
		edit.Question = nil
	}
	if edit.Options != nil && reflect.DeepEqual(normalizeAskOptions(*edit.Options), current.options) {
		edit.Options = nil
	}
	return edit
}

// askBlockChildren is the block's children after edit: the parts it names rebuilt, the parts it
// does not left exactly as they are. A rebuilt part is the children the markdown pipeline
// produces, not hand-built nodes, so a bullet list carries the list attributes every other
// document write gives it. The text the edit writes is caller text, so it is weighed on budget,
// the write's, before it is rendered and as it is parsed back (parsedBlockBody).
func askBlockChildren(current askBlock, edit AskBlockEdit, budget *pmdoc.WriteBudget) ([]*pmdoc.Node, error) {
	paragraphs := []*pmdoc.Node{}
	var list *pmdoc.Node
	for _, child := range current.node.Children {
		if child.Type == "bullet_list" {
			list = child
			continue
		}
		paragraphs = append(paragraphs, child)
	}

	edit = withoutUnchanged(current, edit)

	if edit.Question != nil {
		question := normalizeAskQuestion(*edit.Question)
		if err := budget.RefusePlainText(question); err != nil {
			return nil, err
		}
		rebuilt, refusal, err := parsedBlockBody(questionParagraphs(question), budget)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return nil, &ErrAskBlockUnrepresentable{Field: "question", Reason: refusal}
		}
		for _, node := range rebuilt {
			if node.Type != "paragraph" {
				return nil, &ErrAskBlockUnrepresentable{
					Field:  "question",
					Reason: "the text reads as a " + node.Type + " rather than a question paragraph",
				}
			}
		}
		if len(rebuilt) == 0 {
			return nil, &ErrAskBlockUnrepresentable{Field: "question", Reason: "the question is empty"}
		}
		paragraphs = rebuilt
	}

	if edit.Options != nil {
		options := normalizeAskOptions(*edit.Options)
		list = nil
		if len(options) > 0 {
			texts := make([]string, 0, 2*len(options))
			for _, option := range options {
				texts = append(texts, option.Label, option.Description)
			}
			if err := budget.RefusePlainText(texts...); err != nil {
				return nil, err
			}
			rebuilt, refusal, err := parsedBlockBody([]*pmdoc.Node{optionList(options)}, budget)
			if err != nil {
				return nil, err
			}
			if refusal != "" {
				return nil, &ErrAskBlockUnrepresentable{Field: "options", Reason: refusal}
			}
			if len(rebuilt) != 1 || rebuilt[0].Type != "bullet_list" {
				return nil, &ErrAskBlockUnrepresentable{
					Field:  "options",
					Reason: "the options do not read back as one bullet list",
				}
			}
			list = rebuilt[0]
		}
	}

	children := append([]*pmdoc.Node{}, paragraphs...)
	if list != nil {
		children = append(children, list)
	}
	return children, nil
}

// parsedBlockBody is what the document's own markdown pipeline makes of nodes, which hold text as
// a caller wrote it: rendering escapes the text so it reads back literally, and parsing supplies
// the attributes and block ids a hand-built node has no business inventing. The rendering is parsed
// on budget, the caller write's, which has weighed the text already (pmdoc.WriteBudget.
// RefusePlainText). refusal is why the text cannot be held, when the renderer or the parser
// refuses it (ErrDocSchema, pmdoc.ErrSchema); any other error - a refusal of its size, or a panic
// (pmdoc.ErrPanic), which is pmdoc's bug rather than a reason - is err.
func parsedBlockBody(nodes []*pmdoc.Node, budget *pmdoc.WriteBudget) (parsed []*pmdoc.Node, refusal string, err error) {
	markdown, err := renderTree(&pmdoc.Node{Type: "doc", Children: nodes})
	if err != nil && !errors.Is(err, ErrDocSchema) {
		return nil, "", err
	}
	if err != nil {
		return nil, "the text cannot be written as document markdown: " + err.Error(), nil
	}
	doc, err := pmdoc.ParseFragment(markdown, false, budget)
	if err != nil && !errors.Is(err, pmdoc.ErrSchema) {
		return nil, "", err
	}
	if err != nil {
		return nil, "the text would leave the document's markdown unreadable", nil
	}
	return doc.Children, "", nil
}

// verifyAskBlockRoundTrip reads the written block back the ways the rest of Dispatch reads it -
// settlement's own parser, and, when this edit rewrote text, the canonical markdown a version
// records and an upload re-parses - and reports the first that does not return want. An error
// writing or reading it that is no refusal, a panic (pmdoc.ErrPanic) among them, is pmdoc's bug,
// and is returned as it is.
func verifyAskBlockRoundTrip(next *pmdoc.Node, blockID string, want AskBlockText, wroteBody bool) error {
	block, err := askBlockOf(next, blockID)
	if err != nil {
		return err
	}
	if err := compareAskBlockText(block, want); err != nil {
		return err
	}
	if !wroteBody {
		return nil
	}
	markdown, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{block.node}})
	if err != nil && !errors.Is(err, ErrDocSchema) {
		return err
	}
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: "the text cannot be written as document markdown"}
	}
	rendered, err := pmdoc.ParseRendering(markdown)
	if err != nil && !errors.Is(err, pmdoc.ErrSchema) {
		return err
	}
	if err != nil {
		return &ErrAskBlockUnrepresentable{
			Field:  "block",
			Reason: "the text would leave the document's markdown unreadable",
		}
	}
	reparsed, err := askBlockOf(rendered, blockID)
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: "the text does not survive the document's markdown"}
	}
	return compareAskBlockText(reparsed, want)
}

// decidedWording is writeDecidedWording for ask as it stands, for a repair to make again on
// another tree (repairLive): a block it cannot give the wording is left as it is.
func decidedWording(ask model.Ask) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		changed, err := writeDecidedWording(node, ask)
		return err == nil && changed
	}
}

// writeDecidedWording gives node, the ask block of ask, the wording ask was decided on - the
// question, options, multiple and urgency its row holds - and reports whether that changed the
// block. A part the block already asks keeps its nodes, marks and inner block ids
// (askBlockChildren); a part it rewrites is what the document's own markdown pipeline makes of the
// decided text. It changes nothing, and returns why, when the block would not read back as that
// wording.
func writeDecidedWording(node *pmdoc.Node, ask model.Ask) (bool, error) {
	current, err := parseAskBlock(node)
	if err != nil {
		return false, err
	}
	decided := model.NewAskEditPrevious(ask)
	if current.asks(decided) {
		return false, nil
	}
	children, err := askBlockChildren(current, AskBlockEdit{Question: &decided.Question, Options: &decided.Options}, pmdoc.NewWriteBudget())
	if err != nil {
		return false, err
	}
	written := &pmdoc.Node{Type: node.Type, Attrs: maps.Clone(node.Attrs), Children: children}
	written.Attrs["multiple"] = decided.Multiple
	written.Attrs["urgency"] = decided.Urgency
	reads, err := parseAskBlock(written)
	if err != nil {
		return false, err
	}
	if !reads.asks(decided) {
		return false, fmt.Errorf("the block reads back as %q, not the wording its ask was decided on", reads.question)
	}
	node.Attrs, node.Children = written.Attrs, written.Children
	return true, nil
}

// askBlockOf is the ask block blockID of tree, as settlement's own collector reads it.
func askBlockOf(tree *pmdoc.Node, blockID string) (askBlock, error) {
	blocks, invalid, err := collectAskBlocksForSettlement(tree)
	if err != nil {
		return askBlock{}, err
	}
	for _, candidate := range invalid {
		if candidate.id == blockID {
			return askBlock{}, &ErrAskBlockUnrepresentable{Field: "block", Reason: candidate.reason.Error()}
		}
	}
	for _, candidate := range blocks {
		if candidate.id == blockID {
			return candidate, nil
		}
	}
	return askBlock{}, fmt.Errorf("%w: ask block %q", pmdoc.ErrTargetNotFound, blockID)
}

func compareAskBlockText(block askBlock, want AskBlockText) error {
	if block.question != want.Question {
		return &ErrAskBlockUnrepresentable{
			Field:  "question",
			Reason: fmt.Sprintf("the block carries it as %q", block.question),
		}
	}
	if !reflect.DeepEqual(block.options, want.Options) {
		return &ErrAskBlockUnrepresentable{Field: "options", Reason: askOptionsDifference(block.options, want.Options)}
	}
	if block.multiple != want.Multiple {
		return &ErrAskBlockUnrepresentable{Field: "multiple", Reason: "the block carries a different value"}
	}
	if block.urgency != want.Urgency {
		return &ErrAskBlockUnrepresentable{Field: "urgency", Reason: "the block carries a different value"}
	}
	return nil
}

// askOptionsDifference names the first way the block's options differ from the ones asked for,
// so the refusal reports what was found rather than the cause it is most often.
func askOptionsDifference(got, want []model.AskOption) string {
	if len(got) != len(want) {
		return fmt.Sprintf("the block carries %d options, not %d", len(got), len(want))
	}
	for index := range want {
		if got[index] == want[index] {
			continue
		}
		if got[index].Label != want[index].Label && strings.Contains(want[index].Label, ": ") {
			return fmt.Sprintf("option %d's label may not contain %q, which separates a label from its description; "+
				"the block reads it as %q", index+1, ": ", got[index].Label)
		}
		return fmt.Sprintf("the block carries option %d as %q/%q, not %q/%q", index+1,
			got[index].Label, got[index].Description, want[index].Label, want[index].Description)
	}
	return "the block carries different options"
}

// questionParagraphs renders a question as the paragraphs parseAskBlock reads back: one per
// blank-line-separated part, with a hard break for a single newline inside one.
func questionParagraphs(question string) []*pmdoc.Node {
	parts := strings.Split(question, "\n\n")
	paragraphs := make([]*pmdoc.Node, 0, len(parts))
	for _, part := range parts {
		paragraphs = append(paragraphs, &pmdoc.Node{Type: "paragraph", Children: inlineWithHardBreaks(part)})
	}
	return paragraphs
}

// optionList renders options as the bullet list parseAskBlock reads back: one item per option,
// written `Label: Description`, or a bare `Label` when it has no description.
func optionList(options []model.AskOption) *pmdoc.Node {
	items := make([]*pmdoc.Node, 0, len(options))
	for _, option := range options {
		line := option.Label
		if option.Description != "" {
			line += ": " + option.Description
		}
		items = append(items, &pmdoc.Node{
			Type:     "list_item",
			Children: []*pmdoc.Node{{Type: "paragraph", Children: inlineWithHardBreaks(line)}},
		})
	}
	return &pmdoc.Node{Type: "bullet_list", Children: items}
}

// inlineWithHardBreaks carries a single newline as the hardbreak node nodeText reads back as
// "\n"; a literal newline inside a text node renders to markdown that re-parses as a space.
func inlineWithHardBreaks(text string) []*pmdoc.Node {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	inline := make([]*pmdoc.Node, 0, len(lines)*2-1)
	for index, line := range lines {
		if index > 0 {
			inline = append(inline, &pmdoc.Node{Type: "hardbreak"})
		}
		if line != "" {
			inline = append(inline, &pmdoc.Node{Type: "text", Text: line})
		}
	}
	return inline
}

// normalizeAskQuestion trims the question the way parseAskBlock trims the paragraphs it reads
// back, so a caller's surrounding whitespace is stored rather than refused.
func normalizeAskQuestion(question string) string {
	parts := strings.Split(question, "\n\n")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// normalizeAskOptions trims each label and description, as parseAskBlock does.
func normalizeAskOptions(options []model.AskOption) []model.AskOption {
	normalized := make([]model.AskOption, 0, len(options))
	for _, option := range options {
		normalized = append(normalized, model.AskOption{
			Label:       strings.TrimSpace(option.Label),
			Description: strings.TrimSpace(option.Description),
		})
	}
	return normalized
}

// refuseAskContentEdit refuses every edit of block, an ask block breaking its content rule
// (pmdoc.AskContentError), which settlement marks `invalid` (readAskBlockStrictly). askBlockOf
// reads such an ask leniently, joining every paragraph into its question and every list into its
// options, where the browser's card reads its question from the first child and its options from
// the last, so the row must not take that reading, and rebuilding the body from it would move,
// merge or flatten what a collaborator wrote. The route rebuilds no broken decision: a document
// edit deletes, in one edit, the blocks the refusal names (planAskRepair), and the ask edit
// follows. Until the document settles its block ids (a child with no id, or an id repeated in the
// tree, which ApplyOps would restamp first) the refusal names no delete and asks for a retry.
// Where no one edit of deletes leaves every decision it changes readable, the refusal names no
// delete and no decision to edit: it says the decision is repaired where a person writes it, in
// the document editor. It sends no caller to an ask
// edit of another decision, which is not the caller's to make: a decision that does not parse is
// refused before its content rule is read (askBlockOf), and a decided one, or one another person
// opened, is refused by the route above.
//
// The refusal quotes every block of the decision in full, nested content and all, so a caller
// deleting one can restore every word, and gives each block id as quoted data, never inside a
// command a caller might paste into a shell. Every block it names to delete is one of those or is
// nested in one, so every word it drops is quoted.
func refuseAskContentEdit(tree *pmdoc.Node, block askBlock) error {
	broken := pmdoc.AskContentError(block.node)
	if broken == nil {
		return nil
	}
	children := block.node.Children
	parts := make([]string, 0, len(children))
	settled := pmdoc.BlockIDRepairCount(tree) == 0
	for _, child := range children {
		markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{child}})
		if err != nil {
			return fmt.Errorf("render a block of a broken ask: %w", err)
		}
		name := "a block with no id"
		if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			name = fmt.Sprintf("block %q", id)
		} else {
			settled = false
		}
		parts = append(parts, fmt.Sprintf("%s (%s) reads %q", name, child.Type, strings.TrimSpace(markdown)))
	}
	reason := broken.Error() + "; the ask-edit route does not rebuild a decision that breaks its content rule"
	quoted := "; its blocks are: " + strings.Join(parts, "; ")
	// The plan reads the document's block ids, which an unsettled document has yet to stamp.
	var plan askRepairPlan
	if settled {
		plan = planAskRepair(tree, block.node)
	}
	switch {
	case !settled:
		reason += ", and the document has not settled its block ids yet (a block has no id, or an id repeats), " +
			"so a document edit (dispatch doc-edit, deleting a block by its id) cannot name its blocks now; " +
			"retry the edit once the document settles (a few seconds)"
	case len(plan.drop) > 0:
		reason += ", so delete the blocks to drop, all in one document edit (dispatch doc-edit, deleting a block " +
			"by its id; one edit holding one delete per block), which keeps the question's paragraphs and the " +
			"first list after them, here and in every decision nested in the blocks it keeps; the blocks to " +
			"delete are " + strings.Join(plan.drop, ", ") + "; then edit the ask"
	case plan.holder:
		reason += ", and no document edit deleting blocks of this decision leaves every decision it changes " +
			"readable: a delete inside this one changes the decision holding it, which such an edit may not " +
			"leave unreadable, and these deletes would; this decision is repaired in the document editor"
	case plan.blocked != nil:
		reason += ", and no document edit deleting this decision's blocks, or those of a decision nested in it, " +
			"leaves it valid, since with them deleted " + plan.blocked.Error() + ", which needs words written, " +
			"not blocks deleted; this decision is repaired in the document editor"
	default:
		reason += ", and no document edit deleting this decision's blocks, or those of a decision nested in it, " +
			"leaves it valid; this decision is repaired in the document editor"
	}
	return &ErrAskBlockUnrepresentable{Field: "block", Reason: reason + quoted}
}

// askRepairPlan is the one document edit that repairs a decision breaking its content rule.
type askRepairPlan struct {
	// drop is the id of each block to delete, Go-quoted, in document order. It is empty when no
	// one edit of deletes leaves the decision valid.
	drop []string
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
// decision's block is deleted by its id like any other.
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
	return askRepairPlan{drop: droppedBlockIDs(ask, dropped)}
}

// askRepairDrops is every block to delete inside ask, an ask breaking its content rule, and the
// block id of each decision losing one - ask first, then the decisions nested in it, as deep as
// they nest. A decision whose own blocks keep the rule loses none of them: only the decisions
// nested in it that break it are repaired.
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
