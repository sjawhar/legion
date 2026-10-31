package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
	// SeveralOptionLists reports that the block the write left holds more than one bullet
	// list, so no parse reads it as a decision (readAskBlockStrictly) and Options above is the
	// options of every one of those lists joined, which is the lenient reading an ask's row
	// must not take (refuseAskContentEdit). One edit leaves such a block: the question written
	// into a decision whose question a person emptied, which keeps every list the block holds
	// (askBlockChildren). Settlement leaves the row of a block it cannot read as it is, so a
	// row given the joined options would offer them until someone repairs the block.
	SeveralOptionLists bool
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
// then parses back to, which is what its ask row must hold, except where the return value reports
// that the block does not read as a decision (AskBlockText.SeveralOptionLists, which says what a
// row may hold then). The caller writes the row from the return value, never from its own
// request, so normalisation cannot leave the two disagreeing, and a field the edit did not name
// comes back as the block's own.
//
// It returns *ErrAskBlockUnrepresentable, having written nothing, when the block would not carry
// the text unchanged.
//
// A block whose question a person's edit emptied is read leniently (askBlockToEdit), so that an
// edit carrying a question repairs it: a block holding no question is one no ask edit could
// reach, and the refusal of every other edit of it names the step that does
// (refuseEmptyQuestionEdit). Such a block is weighed on that question rather than on its content
// rule, and an edit carrying a question is written into it whatever that rule says.
func (s *Service) SetAskBlockText(
	ctx context.Context,
	artifactID, blockID string,
	edit AskBlockEdit,
	actor model.Actor,
) (AskBlockText, error) {
	var stored *AskBlockText
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		valid, err := startTree(current, doc)
		if err != nil {
			return err
		}
		tree := valid.Node()
		block, emptyQuestion, err := askBlockToEdit(tree, blockID)
		if err != nil {
			return err
		}
		// A block read leniently is weighed on its empty question alone. No document edit
		// repairs it: a delete may not leave a decision it changes unreadable
		// (validateEditedAskBlocks), and one asking nothing is unreadable, so the deletes the
		// content rule's refusal names are a step no caller can take yet, and a refusal must
		// name only the step that applies now. That step is the question, which this route
		// writes into the block whatever the content rule says, since no document edit writes
		// a paragraph inside a decision (refuseEmptyQuestionEdit holds that reason). Every
		// other block is weighed on its content rule instead: for one that reads as a decision
		// the rule is decided once, before the edit is built, and askBlockChildren then yields
		// the question's paragraphs and at most one list, which keeps it. A block that breaks
		// it is refused, and repaired where a person writes it (refuseAskContentEdit).
		if emptyQuestion {
			if err := refuseEmptyQuestionEdit(block, edit); err != nil {
				return err
			}
		} else if err := refuseAskContentEdit(tree, block); err != nil {
			return err
		}
		want := AskBlockText{
			Question: block.question,
			Options:  block.options,
			Multiple: block.multiple,
			Urgency:  block.urgency,
			// The write leaves more than one bullet list only where it repairs an emptied
			// question (askBlockChildren); AskBlockText.SeveralOptionLists says what a row
			// may hold then. Every other edit reaching here passed the content rule, which
			// leaves the block at most one list, and writes at most one itself.
			SeveralOptionLists: len(askBlockLists(block.node)) > 1,
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
			// What an options edit would delete that only the document holds is weighed before
			// the children are built, over the pairing they are built on (refuseLostOptionWords).
			if err := refuseLostOptionWords(block, edit); err != nil {
				return err
			}
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
// what a person wrote under an option and rebuilding the list would delete it unseen. An item no
// sent option is paired with is dropped here; the ask-edit route weighs what that deletes before
// it builds these children (refuseLostOptionWords), which settlement's restore of a decided
// wording cannot (writeDecidedWording).
//
// One budget pays for every part of the body this write sends: the question's text is weighed on
// budget, the write's, and parsed back on it (parsedBlockBody), and so are the option items the
// caller sent, where the merged list is parsed on a budget of its own for the reason
// editedOptionList gives.
//
// Every bullet list the block holds is kept, in document order, each the node the document
// already holds, with its block id, its items and their marks. A block holding more than one
// reaches here only for the edit that writes a question into a block whose question a person
// emptied (parseAskBlockLeniently), the one edit of such a block that applies: every other edit
// of it is refused on its empty question (refuseEmptyQuestionEdit). An options edit of such a
// block is refused, since the merge pairs each sent option with the item of one list
// (mergeAskOptions) and such a block gives it no way to say which, so rebuilding across them
// would move a person's words from one list into another.
func askBlockChildren(current askBlock, edit AskBlockEdit, budget *pmdoc.WriteBudget) ([]*pmdoc.Node, error) {
	paragraphs := []*pmdoc.Node{}
	for _, child := range current.node.Children {
		if child.Type != "bullet_list" {
			paragraphs = append(paragraphs, child)
		}
	}
	lists := askBlockLists(current.node)

	edit = withoutUnchanged(current, edit)

	if edit.Question != nil {
		rebuilt, err := questionBlocks(normalizeAskQuestion(*edit.Question), budget)
		if err != nil {
			return nil, err
		}
		paragraphs = rebuilt
	}

	if edit.Options != nil {
		if len(lists) > 1 {
			return nil, &ErrAskBlockUnrepresentable{Field: "options", Reason: "this decision holds more than one " +
				"bullet list, so an options edit cannot say which list an option is in; send the question alone, " +
				"and edit the options once the decision reads as one"}
		}
		rebuilt, err := editedOptionList(optionListOf(lists), normalizeAskOptions(*edit.Options), budget)
		if err != nil {
			return nil, err
		}
		lists = nil
		if rebuilt != nil {
			lists = []*pmdoc.Node{rebuilt}
		}
	}

	children := append([]*pmdoc.Node{}, paragraphs...)
	return append(children, lists...), nil
}

// askBlockLists is every bullet list block holds, in document order.
func askBlockLists(block *pmdoc.Node) []*pmdoc.Node {
	lists := []*pmdoc.Node{}
	for _, child := range block.Children {
		if child.Type == "bullet_list" {
			lists = append(lists, child)
		}
	}
	return lists
}

// optionListOf is the list an options edit writes, out of the lists a block holds: its one list,
// or nil where it holds none and every option sent is new. A block holding more than one is
// refused the edit (askBlockChildren).
func optionListOf(lists []*pmdoc.Node) *pmdoc.Node {
	if len(lists) != 1 {
		return nil
	}
	return lists[0]
}

// questionBlocks is the paragraphs an edit's question is written as: the text is weighed on
// budget, the write's, and read back through the document's own markdown pipeline
// (parsedBlockBody), so the question carries the attributes and block ids a parse gives rather
// than the ones a hand-built node would invent. Text the pipeline reads as anything but
// paragraphs, or as nothing at all, is no question, and is refused.
func questionBlocks(question string, budget *pmdoc.WriteBudget) ([]*pmdoc.Node, error) {
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
	return rebuilt, nil
}

// parsedBlockBody is what the document's own markdown pipeline makes of nodes, which hold text as
// a caller wrote it: rendering escapes the text so it reads back literally, and parsing supplies
// the attributes and block ids a hand-built node has no business inventing. The rendering is parsed
// on budget: the caller write's, which has weighed its text already (pmdoc.WriteBudget.
// RefusePlainText), or one of its own, which editedOptionList says when and why. refusal is why
// the text cannot be held, when the renderer or the parser refuses it (ErrDocSchema,
// pmdoc.ErrSchema); any other error - a refusal of its size, or a panic (pmdoc.ErrPanic), which
// is pmdoc's bug rather than a reason - is err.
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
//
// It writes over what a person wrote under an option the decision never held, which the merge
// drops (mergeAskOptions) and the ask-edit route refuses first (refuseLostOptionWords): no
// caller here can answer a refusal, and a block left disagreeing with the row of the ask it was
// decided on would disagree for good.
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

// writesNothingButItsText reports whether node writes no more than the text in it: it renders to
// exactly what a paragraph holding its own text and the breaks a person typed in it renders to
// (textAndBreaksOf). A paragraph holding an image, or a link whose text is empty, writes what no
// text of its own does; one holding the whitespace a person typed, under the mark the editor
// leaves on every character they type, writes that whitespace and nothing else - as the
// character reference the markdown spells a space a line would drop with, which every reader
// decodes back to the space. So a caller weighing what a write over a paragraph would delete
// reads it by what it renders to, not by its marks: the marks a reader sees are the marks the
// markdown writes (rendersTheSame).
func writesNothingButItsText(node *pmdoc.Node) bool {
	return rendersTheSame(node, &pmdoc.Node{Type: "paragraph", Children: textAndBreaksOf(node)})
}

// textAndBreaksOf is node's own inline content as its text alone: each text node's text and each
// break node as it stands, both without the marks they carry, and nothing else. The breaks are
// kept rather than rebuilt from the text (inlineWithHardBreaks), which writes every one of them
// as a hard break: a soft break, which the editor keeps from a paste and the markdown writes as
// the space a reader sees, and a line feed a text node ends with, which the markdown drops as
// the trailing whitespace it is, both read back as the line feed a hard break reads back as
// (pmdoc.TextContent), and rebuilding either would make a break a person typed look like markup
// their line does not hold. Every other inline node - an image, a footnote's reference, a piece
// of inline HTML - is left out, so it is in node and not in the text it is weighed against, and
// a write over it is weighed as the deletion it is; a link's address rides on a mark, and goes
// with the marks.
func textAndBreaksOf(node *pmdoc.Node) []*pmdoc.Node {
	inline := make([]*pmdoc.Node, 0, len(node.Children))
	for _, child := range node.Children {
		if child.Type != "text" && child.Type != "hardbreak" {
			continue
		}
		unmarked := *child
		unmarked.Marks = nil
		inline = append(inline, &unmarked)
	}
	return inline
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
// into it; it returns nil where one can, which is the one edit of such a block that applies and
// is written whatever the block's content rule says (askBlockChildren). The one thing that
// repairs such a block is a question: an edit carrying one has it written where the block's
// paragraphs are, and an edit carrying none is refused with that step, since the block's own
// sentence names none and an agent reading it has nowhere to go.
//
// This is where the reason the rest of this package names lives: no document edit writes a decision
// a question, since a delete writes no words and an insert anchored on a block of a decision
// lands beside that decision rather than in it (pmdoc.BlockBoundary reads the document-level
// block holding the anchor); and a decision left asking nothing is refused every document edit
// that changes it (validateEditedAskBlocks). So the question is written here or nowhere, and a
// refusal that names a document edit of such a decision names a step no caller can take.
//
// An ask asking nothing has no text in the paragraphs the write replaces, so replacing them
// loses no word. It can still lose what is no word - an image, a link whose text is empty - so a
// paragraph that writes more than the text in it is refused before the write, and the refusal
// quotes it and names the two places the question can be written without losing it: an upload of
// the document as a new version, which carries a block whose question it writes and is the path
// an agent has, and the document editor, where a person writes the question beside the
// paragraph. The upload is named with the owner the document belongs to, since the command does
// not address the document without one, and both spellings of that owner stand in one clause,
// since an agent holding a project document would read a step it cannot run were the issue named
// alone. The refusal says a document edit reaches this block for nothing: an agent reading it
// has been told to change a document with a document edit rather than to upload its name again
// (the dispatch_artifact tool's own description), and only the reason above lets it do otherwise
// here. The block and the paragraph are quoted as data (quotedAskChild), never written into the
// reason raw.
func refuseEmptyQuestionEdit(block askBlock, edit AskBlockEdit) error {
	for _, child := range block.node.Children {
		if child.Type == "bullet_list" || writesNothingButItsText(child) {
			continue
		}
		quoted, err := quotedAskChild(child)
		if err != nil {
			return err
		}
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: fmt.Sprintf(
			"ask block %q has an empty question, and writing a question into it would delete what one of its "+
				"paragraphs holds, which is no text: %s; no document edit writes a paragraph into a "+
				"decision, so none reaches this block - upload the document again "+
				"as a new version with the question written beside that paragraph (dispatch artifact "+
				"--issue <KEY>, or --project <KEY> for a project's document, --name <the document> "+
				"--content-file -), or write the question beside it in "+
				"the document editor; either keeps what the paragraph holds",
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
	markdown, err := renderBlock(child)
	if err != nil {
		return "", fmt.Errorf("render a block of a broken ask: %w", err)
	}
	name := "a block with no id"
	if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
		name = fmt.Sprintf("block %q", id)
	}
	return fmt.Sprintf("%s (%s) reads %q", name, child.Type, strings.TrimSpace(markdown)), nil
}

// renderBlock is the markdown of one block, rendered as the whole of a document holding it alone:
// the renderer takes a document, and a caller quoting a block in a refusal or weighing two blocks
// by what they write (rendersTheSame) has one block. A tree the renderer refuses is its caller's
// to read, as pmdoc.Render leaves it.
func renderBlock(block *pmdoc.Node) (string, error) {
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{block}})
}

// refuseAskContentEdit refuses every edit of block, an ask block breaking its content rule
// (pmdoc.AskContentError), which settlement marks `invalid` (readAskBlockStrictly). askBlockOf
// reads such an ask leniently, joining every paragraph into its question and every list into its
// options, where the browser's card reads its question from the first child and its options from
// the last, so the row must not take that reading, and rebuilding the body from it would move,
// merge or flatten what a collaborator wrote. The route rebuilds no broken decision: a document
// edit deletes, in one edit, the blocks the refusal names (planAskRepair), and the ask edit
// follows. A decision reaching here asks something: one whose question a person emptied is
// weighed on that question instead, and an edit carrying one writes it (refuseEmptyQuestionEdit).
// Where a decision nested in this one holds no paragraph with text, no document edit repairs
// that one either, for the reason refuseEmptyQuestionEdit holds, so the refusal says what it
// needs is words written, and names the editor. Until the document settles its block ids (a
// child with no id, or an id repeated in the tree, which ApplyOps would restamp first) the
// refusal names no delete and asks for a retry. Where no one edit of deletes leaves every
// decision it changes readable, the refusal names no delete and no decision to edit: it says the
// decision is repaired where a person writes it, in the document editor. It sends no caller to
// an ask edit of another decision, which is not the caller's to make: a decision that does not
// parse is refused before its content rule is read (askBlockOf), and a decided one, or one
// another person opened, is refused by the route above.
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
