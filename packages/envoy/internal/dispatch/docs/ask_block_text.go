package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
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
	// The ask row is read before the live document, so its options are the ones the caller's
	// row holds: an options edit of a two-list ask must not delete a list the row never held.
	rowOptions, err := s.askRowOptions(ctx, artifactID, blockID)
	if err != nil {
		return AskBlockText{}, err
	}
	var stored *AskBlockText
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
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
		if err := refuseAskContentEdit(block, rowOptions, edit); err != nil {
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
// Options sent to a block that breaks its content rule always count as a change: the joined
// reading of a two-list ask can equal them, yet rebuilding them as one list is the repair the
// refusal tells the caller to send (refuseAskContentEdit).
func withoutUnchanged(current askBlock, edit AskBlockEdit) AskBlockEdit {
	if edit.Question != nil && normalizeAskQuestion(*edit.Question) == current.question {
		edit.Question = nil
	}
	if edit.Options != nil && pmdoc.AskContentError(current.node) == nil &&
		reflect.DeepEqual(normalizeAskOptions(*edit.Options), current.options) {
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

// askRowOptions is the options of the ask row anchored to blockID of artifactID, trimmed as
// normalizeAskOptions trims them; none when no row is anchored there.
func (s *Service) askRowOptions(ctx context.Context, artifactID, blockID string) ([]model.AskOption, error) {
	var encoded []byte
	err := s.queryFrom(ctx).QueryRow(ctx, `
		select a.options from asks a
		where a.block_artifact_id = $1 and block_id_key(a.block_id) = block_id_key($2) and a.block_id = $2
	`, artifactID, blockID).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ask row options: %w", err)
	}
	var options []model.AskOption
	if err := json.Unmarshal(encoded, &options); err != nil {
		return nil, fmt.Errorf("decode ask row options: %w", err)
	}
	return normalizeAskOptions(options), nil
}

// refuseAskContentEdit refuses edit of block, an ask block breaking its content rule
// (pmdoc.AskContentError), which settlement marks `invalid` (readAskBlockStrictly). askBlockOf
// reads such an ask leniently, joining every paragraph into its question and every list into its
// options, where the browser's card reads its question from the first child and its options from
// the last, so the row must not take that reading.
//
// An ask holding a block after its first list is refused whatever the edit sends: rebuilding the
// body would move that block's text into the question or delete it, and the caller, reading the
// row, cannot know it is there. A document edit moves or deletes it. So is an ask whose lists
// hold two items with one label: its options cannot be rebuilt as one list of unique labels
// without losing one, so a document edit removes one and the ask edit sends it renamed (a
// document edit renaming it leaves the decision broken, which doc-edit refuses).
//
// An ask holding only paragraphs and then two or more lists, where an item the block holds and
// the row does not (below) holds more than its first paragraph, is refused whatever the edit
// sends: rebuilding keeps each item's plain text only. The refusal quotes in full, by block id,
// the lists a document edit deletes to leave the ask valid (listsToDelete), so the caller can
// delete them (doc-edit refuses an edit leaving the decision broken, so deleting only the extra
// content would be refused), edit the ask sending every option, and restore the rest.
//
// An ask holding only paragraphs and then two or more lists is refused unless the edit sends
// options, which rebuilds them as one list and so repairs the ask. Settlement keeps such an ask's
// row (rowOptions) as it was before the break, so a caller building options from the row would
// delete or rewrite what a collaborator wrote in the block: the edit is refused unless its
// options hold, exactly (label and description), every item the block holds and the row does
// not. The refusal quotes each such item whole, so sending what it quotes loses nothing.
func refuseAskContentEdit(block askBlock, rowOptions []model.AskOption, edit AskBlockEdit) error {
	broken := pmdoc.AskContentError(block.node)
	if broken == nil {
		return nil
	}
	if after, found := textAfterOptions(block.node); found {
		return &ErrAskBlockUnrepresentable{
			Field: "block",
			Reason: broken.Error() + "; the ask-edit route will not move " + after +
				" into the question nor delete it: move it out of the decision (a document edit places it before or after the decision block) or delete it with a document edit (dispatch doc-edit), then edit the ask",
		}
	}
	items := normalizeAskOptions(block.options)
	for i, item := range items {
		if !slices.ContainsFunc(items[i+1:], func(other model.AskOption) bool { return other.Label == item.Label }) {
			continue
		}
		shared := slices.DeleteFunc(slices.Clone(items), func(other model.AskOption) bool { return other.Label != item.Label })
		return &ErrAskBlockUnrepresentable{
			Field: "block",
			Reason: broken.Error() + "; the options cannot be rebuilt as one list, since " + quotedItems(shared) +
				" share a label: remove one with a document edit (dispatch doc-edit), then edit the ask sending every option, the removed one renamed",
		}
	}
	protected := slices.DeleteFunc(slices.Clone(items), func(item model.AskOption) bool { return slices.Contains(rowOptions, item) })
	if lists := listsToDelete(block.node, items, protected); len(lists) > 0 {
		quoted, ops, err := quotedLists(lists)
		if err != nil {
			return err
		}
		return &ErrAskBlockUnrepresentable{
			Field: "block",
			Reason: broken.Error() + "; an option the decision holds but its ask does not holds more than its text, which rebuilding the options as text would delete: delete " +
				quoted + ", with a document edit (dispatch doc-edit --ops-json " + ops + "), then edit the ask sending every option; the quoted words below an option can come back in its description",
		}
	}
	if edit.Options == nil {
		reason := broken.Error() + "; send options to rebuild the options as one list"
		if len(protected) > 0 {
			reason += ", keeping " + quotedItems(protected) + " exactly as quoted, which the decision holds but its ask does not"
		}
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: reason}
	}
	sent := normalizeAskOptions(*edit.Options)
	missing := slices.DeleteFunc(protected, func(item model.AskOption) bool { return slices.Contains(sent, item) })
	if len(missing) == 0 {
		return nil
	}
	return &ErrAskBlockUnrepresentable{
		Field: "block",
		Reason: broken.Error() + "; the edit leaves out or changes " + quotedItems(missing) +
			", which the decision holds but its ask does not: send each exactly as quoted with the options to rebuild one list",
	}
}

// quotedItems quotes options whole, as `"Label: Description"`, or `"Label"` without a
// description, so a caller can send each back exactly.
func quotedItems(options []model.AskOption) string {
	parts := make([]string, 0, len(options))
	for _, option := range options {
		text := option.Label
		if option.Description != "" {
			text += ": " + option.Description
		}
		parts = append(parts, fmt.Sprintf("%q", text))
	}
	return strings.Join(parts, ", ")
}

// listsToDelete is the lists of ask a document edit deletes to leave it valid where an item of
// them that reads as one of protected holds more than its first paragraph - nested lists,
// further paragraphs: every list holding such an item, and every other list but the first, so
// at most one is left. None when no such item is held. items are ask's options in list order, as
// parseAskBlock reads them.
func listsToDelete(ask *pmdoc.Node, items, protected []model.AskOption) []*pmdoc.Node {
	lists := []*pmdoc.Node{}
	holders := []*pmdoc.Node{}
	index := 0
	for _, child := range ask.Children {
		if child.Type != "bullet_list" {
			continue
		}
		lists = append(lists, child)
		holds := false
		for _, item := range child.Children {
			option := items[index]
			index++
			holds = holds || (len(item.Children) > 1 && slices.Contains(protected, option))
		}
		if holds {
			holders = append(holders, child)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	kept := false
	return slices.DeleteFunc(lists, func(list *pmdoc.Node) bool {
		if kept || slices.Contains(holders, list) {
			return false
		}
		kept = true
		return true
	})
}

// quotedLists names lists by block id, each with its whole markdown quoted, nested content and
// all, so a caller deleting them can restore every word, and the doc-edit ops deleting them.
func quotedLists(lists []*pmdoc.Node) (quoted, ops string, err error) {
	parts := make([]string, 0, len(lists))
	deletes := make([]string, 0, len(lists))
	for _, list := range lists {
		markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{list}})
		if err != nil {
			return "", "", fmt.Errorf("render a list of a broken ask: %w", err)
		}
		id, _ := list.Attrs[pmdoc.BlockIDAttr].(string)
		parts = append(parts, fmt.Sprintf("the list block %q, which reads %q", id, strings.TrimSpace(markdown)))
		encoded, err := json.Marshal(model.EditOp{Op: "delete", Block: id})
		if err != nil {
			return "", "", fmt.Errorf("encode a delete of a list of a broken ask: %w", err)
		}
		deletes = append(deletes, string(encoded))
	}
	return strings.Join(parts, " and "), "'[" + strings.Join(deletes, ",") + "]'", nil
}

// quoteCut quotes parts joined by spaces, cut short where they run on, or names empty when they
// hold no text.
func quoteCut(parts []string, empty string) string {
	if len(parts) == 0 {
		return empty
	}
	text := []rune(strings.Join(parts, " "))
	if len(text) > quotedRunes {
		return fmt.Sprintf("%q", strings.TrimRight(string(text[:quotedRunes]), " ")+"…")
	}
	return fmt.Sprintf("%q", string(text))
}

// quotedRunes is how much of a block's text a refusal quotes before cutting it short.
const quotedRunes = 80

// textAfterOptions names the blocks ask holds after its first bullet list that are not lists -
// their text quoted, cut short where it runs on - and reports whether it holds any.
func textAfterOptions(ask *pmdoc.Node) (string, bool) {
	first := slices.IndexFunc(ask.Children, func(child *pmdoc.Node) bool { return child.Type == "bullet_list" })
	if first < 0 {
		return "", false
	}
	parts := []string{}
	found := false
	for _, child := range ask.Children[first+1:] {
		if child.Type == "bullet_list" {
			continue
		}
		found = true
		if text := strings.Join(strings.Fields(pmdoc.TextContent(child)), " "); text != "" {
			parts = append(parts, text)
		}
	}
	if !found {
		return "", false
	}
	return quoteCut(parts, "the empty paragraph after the options"), true
}
