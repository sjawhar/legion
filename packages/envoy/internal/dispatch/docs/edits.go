package docs

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type textMutation struct {
	from int
	to   int
	with string
}

func resolveOperations(markdown string, ops []model.EditOp) ([]textMutation, error) {
	mutations := make([]textMutation, 0, len(ops))
	for index, op := range ops {
		mutation, err := resolveOperation(markdown, op)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
		}
		mutations = append(mutations, mutation)
		markdown = replaceRange(markdown, mutation.from, mutation.to, mutation.with)
	}
	return mutations, nil
}

func resolveOperation(markdown string, op model.EditOp) (textMutation, error) {
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return textMutation{}, invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: from, to: to, with: op.With}, nil
	case "delete":
		if op.Find == "" {
			return textMutation{}, invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: from, to: to}, nil
	case "insert":
		if op.Markdown == "" {
			return textMutation{}, invalidOp("markdown")
		}
		if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
			return textMutation{}, invalidOp("after or before")
		}
		anchor := op.After
		after := anchor != ""
		if !after {
			anchor = op.Before
		}
		position, err := insertPosition(markdown, anchor, after, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: position, to: position, with: op.Markdown}, nil
	default:
		return textMutation{}, invalidOp("op")
	}
}

func insertPosition(markdown, anchor string, after bool, occurrence *int) (int, error) {
	switch anchor {
	case "start":
		return 0, nil
	case "end":
		return text.Len16(markdown), nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return 0, invalidOp("heading")
		}
		return headingPosition(markdown, title, after, occurrence)
	}
	from, to, err := text.Resolve(markdown, anchor, occurrence)
	if err != nil {
		return 0, err
	}
	if after {
		return to, nil
	}
	return from, nil
}

func headingPosition(markdown, title string, after bool, occurrence *int) (int, error) {
	position := 0
	var positions []int
	for _, line := range strings.SplitAfter(markdown, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		heading := strings.TrimLeft(trimmed, " ")
		if strings.HasPrefix(heading, "#") {
			marker := strings.TrimLeft(heading, "#")
			if len(marker) < len(heading) && strings.HasPrefix(marker, " ") && strings.TrimSpace(marker) == title {
				if after {
					positions = append(positions, position+text.Len16(line))
				} else {
					positions = append(positions, position)
				}
			}
		}
		position += text.Len16(line)
	}
	if len(positions) == 0 {
		return 0, text.ErrTargetNotFound
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(positions) {
			return 0, text.ErrTargetNotFound
		}
		return positions[*occurrence], nil
	}
	if len(positions) > 1 {
		return 0, &text.ErrTargetAmbiguous{}
	}
	return positions[0], nil
}

func replaceRange(markdown string, from, to int, with string) string {
	return text.Slice16(markdown, 0, from) + with + text.Slice16(markdown, to, text.Len16(markdown))
}

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	Field string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("invalid document operation field %q", e.Field)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}
