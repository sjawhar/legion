package docs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/reearth/ygo/crdt"

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

// SetAskBlockText writes want into the ask block blockID of artifactID and returns what the
// block parses back to, which is what its ask row must hold. The caller writes the row from the
// return value, never from its own request, so normalisation cannot leave the two disagreeing.
//
// It returns *ErrAskBlockUnrepresentable, having written nothing, when the block would not carry
// the text unchanged.
func (s *Service) SetAskBlockText(
	ctx context.Context,
	artifactID, blockID string,
	want AskBlockText,
	actor model.Actor,
) (AskBlockText, error) {
	normalized := AskBlockText{
		Question: normalizeAskQuestion(want.Question),
		Options:  normalizeAskOptions(want.Options),
		Multiple: want.Multiple,
		Urgency:  want.Urgency,
	}
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		tree, err := treeOf(doc)
		if err != nil {
			return err
		}
		next, err := pmdoc.SetBlockBody(tree, blockID, askBlockChildren(normalized), pmdoc.Attrs{
			"multiple": normalized.Multiple,
			"urgency":  normalized.Urgency,
		})
		if err != nil {
			return err
		}
		if err := verifyAskBlockRoundTrip(next, blockID, normalized); err != nil {
			return err
		}
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
	if err != nil {
		var unrepresentable *ErrAskBlockUnrepresentable
		if errors.As(err, &unrepresentable) {
			return AskBlockText{}, err
		}
		return AskBlockText{}, fmt.Errorf("write ask block text: %w", err)
	}
	return normalized, nil
}

// verifyAskBlockRoundTrip reads the written block back the two ways the rest of Dispatch reads
// it - settlement's own parser, and the canonical markdown a version records and an upload
// re-parses - and reports the first that does not return want.
func verifyAskBlockRoundTrip(next *pmdoc.Node, blockID string, want AskBlockText) error {
	block, err := askBlockOf(next, blockID)
	if err != nil {
		return err
	}
	if err := compareAskBlockText(block, want); err != nil {
		return err
	}
	// The block alone, rendered and re-parsed: a version records this markdown and an upload
	// parses it back, so text that survives the tree but not the page is refused here too.
	markdown, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{block.node}})
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "question", Reason: "the text cannot be written as document markdown"}
	}
	rendered, err := pmdoc.Parse(markdown)
	if err != nil {
		return &ErrAskBlockUnrepresentable{
			Field:  "question",
			Reason: "the text would leave the document's markdown unreadable; a line may not begin with \":::\"",
		}
	}
	reparsed, err := askBlockOf(rendered, blockID)
	if err != nil {
		return &ErrAskBlockUnrepresentable{Field: "question", Reason: "the text does not survive the document's markdown"}
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
			return askBlock{}, &ErrAskBlockUnrepresentable{Field: "question", Reason: candidate.reason.Error()}
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
		return &ErrAskBlockUnrepresentable{
			Field:  "options",
			Reason: "an option label may not contain \": \", which separates a label from its description",
		}
	}
	if block.multiple != want.Multiple {
		return &ErrAskBlockUnrepresentable{Field: "multiple", Reason: "the block carries a different value"}
	}
	if block.urgency != want.Urgency {
		return &ErrAskBlockUnrepresentable{Field: "urgency", Reason: "the block carries a different value"}
	}
	return nil
}

// askBlockChildren renders an ask's text as the block body parseAskBlock reads: one paragraph per
// blank-line-separated part, a hard break for a single newline inside one, and a bullet per
// option written `Label: Description`, or a bare `Label` when it has no description.
func askBlockChildren(text AskBlockText) []*pmdoc.Node {
	children := []*pmdoc.Node{}
	for _, part := range strings.Split(text.Question, "\n\n") {
		children = append(children, &pmdoc.Node{Type: "paragraph", Children: inlineWithHardBreaks(part)})
	}
	if len(text.Options) == 0 {
		return children
	}
	items := make([]*pmdoc.Node, 0, len(text.Options))
	for _, option := range text.Options {
		line := option.Label
		if option.Description != "" {
			line += ": " + option.Description
		}
		items = append(items, &pmdoc.Node{
			Type:     "list_item",
			Children: []*pmdoc.Node{{Type: "paragraph", Children: inlineWithHardBreaks(line)}},
		})
	}
	return append(children, &pmdoc.Node{Type: "bullet_list", Children: items})
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
