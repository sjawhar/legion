package docs

import (
	"context"
	"errors"
	"fmt"
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
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		tree, err := treeOf(doc)
		if err != nil {
			return err
		}
		current, err := askBlockOf(tree, blockID)
		if err != nil {
			return err
		}
		want := AskBlockText{
			Question: current.question,
			Options:  current.options,
			Multiple: current.multiple,
			Urgency:  current.urgency,
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

		wroteBody := askBlockBodyChanges(current, edit)
		var next *pmdoc.Node
		if wroteBody {
			children, err := askBlockChildren(current, edit)
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
		return AskBlockText{}, fmt.Errorf("write ask block text: %w", err)
	}
	// stored is set by the closure, which applyLive runs before it can return nil or
	// ErrNoChanges; a nil here would mean it never ran, which must not read as an empty ask.
	if stored == nil {
		return AskBlockText{}, fmt.Errorf("ask block %q was never read", blockID)
	}
	return *stored, nil
}

// askBlockBodyChanges reports whether edit still names a part of the body to rewrite, after the
// parts it names but does not change have been dropped.
func askBlockBodyChanges(current askBlock, edit AskBlockEdit) bool {
	if edit.Question != nil && normalizeAskQuestion(*edit.Question) != current.question {
		return true
	}
	return edit.Options != nil && !reflect.DeepEqual(normalizeAskOptions(*edit.Options), current.options)
}

// askBlockChildren is the block's children after edit: the parts it names rebuilt, the parts it
// does not left exactly as they are. A rebuilt part is the children the markdown pipeline
// produces, not hand-built nodes, so a bullet list carries the list attributes every other
// document write gives it.
func askBlockChildren(current askBlock, edit AskBlockEdit) ([]*pmdoc.Node, error) {
	paragraphs := []*pmdoc.Node{}
	var list *pmdoc.Node
	for _, child := range current.node.Children {
		if child.Type == "bullet_list" {
			list = child
			continue
		}
		paragraphs = append(paragraphs, child)
	}

	// A field the caller named but did not change is not rewritten. An idempotent retry sends
	// the whole ask back, and rebuilding nodes that already say the same thing would mint fresh
	// inner block ids and orphan the anchors inside them for no change at all.
	if edit.Question != nil && normalizeAskQuestion(*edit.Question) == current.question {
		edit.Question = nil
	}
	if edit.Options != nil && reflect.DeepEqual(normalizeAskOptions(*edit.Options), current.options) {
		edit.Options = nil
	}

	if edit.Question != nil {
		rebuilt, err := parsedBlockBody(questionParagraphs(normalizeAskQuestion(*edit.Question)))
		if err != nil {
			return nil, &ErrAskBlockUnrepresentable{Field: "question", Reason: err.Error()}
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
			rebuilt, err := parsedBlockBody([]*pmdoc.Node{optionList(options)})
			if err != nil {
				return nil, &ErrAskBlockUnrepresentable{Field: "options", Reason: err.Error()}
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

// parsedBlockBody is what the document's own markdown pipeline makes of nodes: rendering escapes
// the text so it reads back literally, and parsing supplies the attributes and block ids a
// hand-built node has no business inventing.
func parsedBlockBody(nodes []*pmdoc.Node) ([]*pmdoc.Node, error) {
	markdown, err := renderTree(&pmdoc.Node{Type: "doc", Children: nodes})
	if err != nil {
		return nil, fmt.Errorf("the text cannot be written as document markdown: %w", err)
	}
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		return nil, errors.New("the text would leave the document's markdown unreadable; a line may not begin with \":::\"")
	}
	return parsed.Children, nil
}

// verifyAskBlockRoundTrip reads the written block back the ways the rest of Dispatch reads it -
// settlement's own parser, and, when this edit rewrote text, the canonical markdown a version
// records and an upload re-parses - and reports the first that does not return want.
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
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: "the text cannot be written as document markdown"}
	}
	rendered, err := pmdoc.Parse(markdown)
	if err != nil {
		return &ErrAskBlockUnrepresentable{
			Field:  "block",
			Reason: "the text would leave the document's markdown unreadable; a line may not begin with \":::\"",
		}
	}
	reparsed, err := askBlockOf(rendered, blockID)
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "block", Reason: "the text does not survive the document's markdown"}
	}
	return compareAskBlockText(reparsed, want)
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
