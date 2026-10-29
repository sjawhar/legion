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
//
// A block whose question a person's edit emptied is read leniently (askBlockToEdit), so that an
// edit carrying a question repairs it: a block holding no question is one no ask edit could
// reach, and the refusal of every other edit of it names the step that does (refuseEmptyQuestionEdit).
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
		block, emptyQuestion, err := askBlockToEdit(tree, blockID)
		if err != nil {
			return err
		}
		// The content rule is decided once, before the edit is built: askBlockChildren then
		// yields the question's paragraphs and at most one list, which keeps the rule. It is
		// weighed before the question is, since a question written into a block that breaks the
		// rule is a rebuild of that block, which this route makes for no edit: such a block is
		// refused with its rule, and repaired where a person writes it (refuseAskContentEdit).
		if err := refuseAskContentEdit(tree, block); err != nil {
			return err
		}
		if emptyQuestion {
			if err := refuseEmptyQuestionEdit(block, edit); err != nil {
				return err
			}
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
// document write gives it. An options edit writes only the lines it sends: each option is paired
// with the list item already holding it (editedOptionList), since an ask row shows nothing of
// what a person wrote under an option and rebuilding the list would delete it unseen. The text
// the edit writes is caller text, so it is weighed on budget, the write's, before it is rendered
// and as it is parsed back (parsedBlockBody).
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
		rebuilt, err := editedOptionList(list, normalizeAskOptions(*edit.Options), budget)
		if err != nil {
			return nil, err
		}
		list = rebuilt
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
// on budget: the caller write's, which has weighed its text already (pmdoc.WriteBudget.
// RefusePlainText), or, where the nodes carry content the document already holds and the caller
// never wrote, one of its own (editedOptionList). refusal is why the text cannot be held, when
// the renderer or the parser refuses it (ErrDocSchema, pmdoc.ErrSchema); any other error - a
// refusal of its size, or a panic (pmdoc.ErrPanic), which is pmdoc's bug rather than a reason -
// is err.
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
	block, unreadable, err := collectedAskBlock(tree, blockID)
	switch {
	case err != nil:
		return askBlock{}, err
	case unreadable != nil:
		return askBlock{}, &ErrAskBlockUnrepresentable{Field: "block", Reason: unreadable.reason.Error()}
	}
	return block, nil
}

// askBlockToEdit is the ask block blockID of tree as the ask-edit route reads it before it writes:
// askBlockOf's reading, except that a block whose one parse failure is an empty question is read
// leniently (parseAskBlockLeniently) and comes back with emptyQuestion set. An ask edit carrying
// a question writes one into such a block, which is the only edit of it that can be applied, so
// reading it is what gives it a way back; every other refusal the collector gives stands, since
// no ask edit repairs a block the parse cannot read for another reason.
func askBlockToEdit(tree *pmdoc.Node, blockID string) (block askBlock, emptyQuestion bool, err error) {
	block, unreadable, err := collectedAskBlock(tree, blockID)
	if err != nil || unreadable == nil {
		return block, false, err
	}
	lenient, lenientErr := parseAskBlockLeniently(unreadable.node)
	if lenientErr != nil || lenient.question != "" {
		return askBlock{}, false, &ErrAskBlockUnrepresentable{Field: "block", Reason: unreadable.reason.Error()}
	}
	return lenient, true, nil
}

// collectedAskBlock is the ask block blockID of tree as settlement's own collector reads it,
// either parsed or, in unreadable, with the node and the reason the parse refused it. err is the
// collector's own refusal of the tree, or that the tree holds no such block.
func collectedAskBlock(tree *pmdoc.Node, blockID string) (askBlock, *invalidAskBlock, error) {
	blocks, invalid, err := collectAskBlocksForSettlement(tree)
	if err != nil {
		return askBlock{}, nil, err
	}
	for index, candidate := range invalid {
		if candidate.id == blockID {
			return askBlock{}, &invalid[index], nil
		}
	}
	for _, candidate := range blocks {
		if candidate.id == blockID {
			return candidate, nil, nil
		}
	}
	return askBlock{}, nil, fmt.Errorf("%w: ask block %q", pmdoc.ErrTargetNotFound, blockID)
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

// editedOptionList is the block's bullet list after an options edit, or nil where the edit sends
// no option and the block keeps no list. Each sent option is paired with the item already
// holding it (mergeAskOptions), the merged items are rendered and parsed as any other document
// write is (parsedBlockBody), so the list and its items carry the attributes that parse gives,
// spread included, and the identity the markdown cannot carry is put back afterwards
// (restoreOptionItems). list is the block's own list, nil where it holds none and every option
// is new.
func editedOptionList(list *pmdoc.Node, options []model.AskOption, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	merged, err := mergeAskOptions(list, options)
	if err != nil {
		return nil, err
	}
	if len(merged) == 0 {
		return nil, nil
	}
	texts := make([]string, 0, 2*len(options))
	for _, option := range options {
		texts = append(texts, option.Label, option.Description)
	}
	if err := budget.RefusePlainText(texts...); err != nil {
		return nil, err
	}
	written := &pmdoc.Node{Type: "bullet_list", Children: make([]*pmdoc.Node, 0, len(merged))}
	if list != nil {
		written.Attrs = list.Attrs
	}
	for _, option := range merged {
		written.Children = append(written.Children, option.written)
	}
	// The merged list carries what the document already holds under an option as well as the
	// lines the caller sent, and that content is not the caller's text. Parsing it on the write's
	// own budget refuses an edit of one option's line for the size of another option's sub-list:
	// a preserved sub-list weighing 62,042 elements, under the 65,536 one write may make, was
	// refused on a budget a 5,000-element question had charged first, and read with no refusal on
	// a budget of its own. So the caller's label and description text is weighed on their budget
	// above (RefusePlainText) and the merged rendering is parsed on a budget of its own.
	rebuilt, refusal, err := parsedBlockBody([]*pmdoc.Node{written}, pmdoc.NewWriteBudget())
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
	return restoreOptionItems(rebuilt[0], list, merged)
}

// mergedOption is one item of the list an options edit writes: the node the merged markdown
// writes for it, and the item the sent option was paired with, which carries the identity and
// the nodes that rendering cannot (restoreOptionItems). held is nil for an option no item held.
type mergedOption struct {
	written *pmdoc.Node
	held    *pmdoc.Node
	// whole reports that the held item already says what the edit sends, so the result is that
	// item itself and not what the parse made of its rendering.
	whole bool
}

// mergeAskOptions pairs each sent option, in the order sent, with the first item of list holding
// its label that no earlier option took, reading an item as parseAskBlock reads it
// (optionOfItem). A pair that says the same thing keeps its item whole, a pair whose description
// changed has the item's line rewritten above everything else the item holds, and an option no
// item holds is a new item. An item no option took is dropped only where it holds nothing but
// its plain line; otherwise the whole edit is refused (refuseDroppedOptionItem), which is what a
// removal, a rename and an edit sending no option at all come to.
func mergeAskOptions(list *pmdoc.Node, options []model.AskOption) ([]mergedOption, error) {
	var items []*pmdoc.Node
	if list != nil {
		items = list.Children
	}
	held := make([]model.AskOption, len(items))
	for index, item := range items {
		held[index] = optionOfItem(item)
	}
	taken := make([]bool, len(items))
	merged := make([]mergedOption, 0, len(options))
	for _, option := range options {
		match := -1
		for index := range items {
			if !taken[index] && held[index].Label == option.Label {
				match = index
				break
			}
		}
		if match < 0 {
			merged = append(merged, mergedOption{written: optionItem(option)})
			continue
		}
		taken[match] = true
		item := items[match]
		if held[match].Description == option.Description {
			merged = append(merged, mergedOption{written: item, held: item, whole: true})
			continue
		}
		merged = append(merged, mergedOption{
			written: withChildren(item, append([]*pmdoc.Node{optionParagraph(option)}, itemContent(item)...)),
			held:    item,
		})
	}
	for index, item := range items {
		if taken[index] || holdsOnlyItsOptionLine(item, held[index]) {
			continue
		}
		return nil, refuseDroppedOptionItem(list, item, held[index])
	}
	return merged, nil
}

// restoreOptionItems gives parsed, the bullet list the merged markdown read back as, the identity
// its markdown does not carry: list's own block id, since it is the same list and the anchors
// inside it must survive; for an option the edit left alone, the very node the document holds;
// and for one it rewrote, that item's own attributes and the nodes under its line, with the
// parsed paragraph of the new line above them. A new item keeps what the parse gave it.
func restoreOptionItems(parsed, list *pmdoc.Node, merged []mergedOption) (*pmdoc.Node, error) {
	if len(parsed.Children) != len(merged) {
		return nil, &ErrAskBlockUnrepresentable{
			Field:  "options",
			Reason: fmt.Sprintf("the options read back as %d items, not the %d sent", len(parsed.Children), len(merged)),
		}
	}
	for index, option := range merged {
		switch {
		case option.held == nil:
		case option.whole:
			parsed.Children[index] = option.held
		default:
			line := parsed.Children[index].Children
			if len(line) == 0 || line[0].Type != "paragraph" {
				return nil, &ErrAskBlockUnrepresentable{
					Field:  "options",
					Reason: fmt.Sprintf("option %d does not read back as a plain line", index+1),
				}
			}
			parsed.Children[index] = withChildren(option.held, append([]*pmdoc.Node{line[0]}, itemContent(option.held)...))
		}
	}
	if list != nil {
		if id, _ := list.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			parsed.Attrs[pmdoc.BlockIDAttr] = id
		}
	}
	return parsed, nil
}

// refuseDroppedOptionItem refuses the whole options edit, writing nothing, because it drops item,
// the list item of option, which holds more than the line the option writes. An ask row shows
// nothing of what a person wrote under an option, so the caller sending the edit cannot see what
// dropping it deletes: the refusal quotes the item whole, its nested content and all, as the
// markdown of a document holding one bullet list holding it alone, so every word of it can be
// restored, and the item is quoted as data (strconv.Quote), never written into the reason raw.
func refuseDroppedOptionItem(list, item *pmdoc.Node, option model.AskOption) error {
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		{Type: "bullet_list", Attrs: list.Attrs, Children: []*pmdoc.Node{item}},
	}})
	if err != nil {
		return fmt.Errorf("render the list item of a dropped option: %w", err)
	}
	return &ErrAskBlockUnrepresentable{
		Field: "options",
		Reason: fmt.Sprintf("the edit drops the option %s, whose list item holds more than its plain line, "+
			"and the ask-edit route does not delete what a person wrote under an option; its item reads %s; "+
			"send that option back with this edit, or remove what the item holds with a document edit first "+
			"(dispatch doc-edit, deleting a block by its id)",
			strconv.Quote(option.Label), strconv.Quote(strings.TrimSpace(markdown))),
	}
}

// optionOfItem is the option parseAskBlock reads in a list item: the text of its first block, cut
// at the first ": " into a label and a description, both trimmed, and no description where the
// text holds no ": ".
func optionOfItem(item *pmdoc.Node) model.AskOption {
	if len(item.Children) == 0 {
		return model.AskOption{}
	}
	label, description, found := strings.Cut(strings.TrimSpace(pmdoc.TextContent(item.Children[0])), ": ")
	if !found {
		description = ""
	}
	return model.AskOption{Label: strings.TrimSpace(label), Description: strings.TrimSpace(description)}
}

// itemContent is what a list item holds under its line: every block after the first, which a
// rewrite of that line keeps and a drop would delete.
func itemContent(item *pmdoc.Node) []*pmdoc.Node {
	if len(item.Children) == 0 {
		return nil
	}
	return item.Children[1:]
}

// holdsOnlyItsOptionLine reports whether item holds nothing a rebuilt list would not write again:
// one paragraph, of text and hard breaks carrying no mark, reading the line option writes
// (optionLine). An item holding a sub-list, a second paragraph, a link's address, bold or code
// holds more, and dropping it would delete what a person wrote.
func holdsOnlyItsOptionLine(item *pmdoc.Node, option model.AskOption) bool {
	if len(item.Children) != 1 || item.Children[0].Type != "paragraph" || !plainInline(item.Children[0]) {
		return false
	}
	return strings.TrimSpace(pmdoc.TextContent(item.Children[0])) == optionLine(option)
}

// plainInline reports whether every inline node under node is a text or hardbreak node carrying
// no mark, which is all inlineWithHardBreaks writes.
func plainInline(node *pmdoc.Node) bool {
	for _, child := range node.Children {
		if child.Type != "text" && child.Type != "hardbreak" || len(child.Marks) > 0 || !plainInline(child) {
			return false
		}
	}
	return true
}

// optionLine is the plain line parseAskBlock reads an option back from: `Label: Description`, or
// a bare `Label` where it has no description.
func optionLine(option model.AskOption) string {
	if option.Description == "" {
		return option.Label
	}
	return option.Label + ": " + option.Description
}

// optionParagraph is the paragraph an option's list item opens with, holding its line alone.
func optionParagraph(option model.AskOption) *pmdoc.Node {
	return &pmdoc.Node{Type: "paragraph", Children: inlineWithHardBreaks(optionLine(option))}
}

// optionItem is the list item an option no list item holds is written as.
func optionItem(option model.AskOption) *pmdoc.Node {
	return &pmdoc.Node{Type: "list_item", Children: []*pmdoc.Node{optionParagraph(option)}}
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

// refuseEmptyQuestionEdit refuses edit, an ask edit of block, an ask block whose question a
// person's edit emptied (parseAskBlockLeniently), where no question of the edit's can be written
// into it; it returns nil where one can. The block keeps its content rule here, which
// refuseAskContentEdit weighed first, so the one thing that repairs it is a question: an edit
// carrying one has it written where the block's paragraphs are, and an edit carrying none is
// refused with that step, since the block's own sentence names none and an agent reading it has
// nowhere to go.
//
// An ask asking nothing has no text in the paragraphs the write replaces, so replacing them loses
// no word. It can still lose what is no word - an image, a link whose text is empty - so a
// paragraph holding anything but text and hard breaks carrying no mark is refused before the
// write, and the refusal quotes it and names the one place it can be kept. That place is the
// document editor: no document edit writes a question beside it, since an insert anchored on a
// block of a decision lands beside the decision rather than in it (pmdoc.BlockBoundary), and a
// decision left asking nothing is refused every document edit that changes it
// (validateEditedAskBlocks). The block and the paragraph are quoted as data (quotedAskChild),
// never written into the reason raw.
func refuseEmptyQuestionEdit(block askBlock, edit AskBlockEdit) error {
	for _, child := range block.node.Children {
		if child.Type == "bullet_list" || plainInline(child) {
			continue
		}
		quoted, err := quotedAskChild(child)
		if err != nil {
			return err
		}
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: fmt.Sprintf(
			"ask block %q has an empty question, and writing a question into it would delete what one of its "+
				"paragraphs holds, which is no text: %s; write the question beside it in the document editor, "+
				"which keeps it",
			block.id, quoted)}
	}
	if edit.Question != nil && normalizeAskQuestion(*edit.Question) != "" {
		return nil
	}
	return &ErrAskBlockUnrepresentable{Field: "block", Reason: fmt.Sprintf(
		"ask block %q has an empty question; send this ask edit again with a question, which writes it into "+
			"the block", block.id)}
}

// quotedAskChild is how a refusal quotes one block of a decision: its id, its type and its
// markdown in full, nested content and all, the id and the markdown Go-quoted, so a caller
// deleting or replacing the block can restore every word of it and no block id is written into a
// reason raw. A block the document has yet to stamp an id into is named by neither.
func quotedAskChild(child *pmdoc.Node) (string, error) {
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{child}})
	if err != nil {
		return "", fmt.Errorf("render a block of a broken ask: %w", err)
	}
	name := "a block with no id"
	if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
		name = fmt.Sprintf("block %q", id)
	}
	return fmt.Sprintf("%s (%s) reads %q", name, child.Type, strings.TrimSpace(markdown)), nil
}

// refuseAskContentEdit refuses every edit of block, an ask block breaking its content rule
// (pmdoc.AskContentError), which settlement marks `invalid` (readAskBlockStrictly). askBlockOf
// reads such an ask leniently, joining every paragraph into its question and every list into its
// options, where the browser's card reads its question from the first child and its options from
// the last, so the row must not take that reading, and rebuilding the body from it would move,
// merge or flatten what a collaborator wrote. The route rebuilds no broken decision: a document
// edit deletes, in one edit, the blocks the refusal names (planAskRepair), and the ask edit
// follows. Where that decision holds no paragraph with text no document edit repairs it: a
// delete writes no words, and an insert anchored on one of its blocks lands beside the decision
// rather than in it (pmdoc.BlockBoundary reads the document-level block holding the anchor), so
// the refusal says what it needs is words written, and names the editor. Until the document
// settles its block ids (a child with no id, or an id repeated in the tree, which ApplyOps would
// restamp first) the refusal names no delete and asks for a retry.
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
		quoted, err := quotedAskChild(child)
		if err != nil {
			return err
		}
		if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id == "" {
			settled = false
		}
		parts = append(parts, quoted)
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
		reason += askRepairStep(plan)
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

// askRepairStep is the step a refusal names for plan, the one document edit that repairs the
// decision, and the ask edit after it.
//
// Where the edit deletes a decision, it carries what the client refuses: a document edit may not
// take a decision block out of the document while its ask is open
// (refuseRemovingOpenDecisionBlocks), and that ask is answered, resolved, or the decision
// repaired by hand, before the delete. A decision that never held a question never had a row
// (addedAskBlocks, readableAskBlockIDs), so the common case passes that guard untouched.
func askRepairStep(plan askRepairPlan) string {
	step := ", so delete the blocks to drop, all in one document edit (dispatch doc-edit, deleting a block " +
		"by its id; one edit holding one delete per block), which keeps the question's paragraphs and the " +
		"first list after them, here and in every decision nested in the blocks it keeps" +
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
// No document edit writes a decision a question: a delete cannot, and an insert anchored on a
// block of a decision lands beside that decision, not in it (pmdoc.BlockBoundary reads the
// document-level block holding its anchor). So a decision holding no paragraph with text keeps
// nothing its rule leaves room for, the re-read below finds what the deletes leave unreadable,
// and the refusal says the decision is repaired where a person writes it.
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
		holds := false
		pmdoc.Walk(node, func(found *pmdoc.Node) bool {
			holds = holds || found.Type == "ask"
			return !holds
		})
		if holds {
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
// edit can give it a question, since a delete writes no words and an insert anchored on a block
// of a decision lands beside that decision (pmdoc.BlockBoundary). Where it is the decision being
// edited the caller's re-read finds what that leaves unreadable; where it is nested in one,
// deleting it whole is what repairs the decision holding it (askRepairKeepsNothingOfItsOwn).
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
