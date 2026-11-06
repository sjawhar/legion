package itemblocks

import (
	"fmt"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/contracts"
)

// CapExceeded is one block of a body over contracts.DispatchBlockUnits. Field is the field the
// body arrived in, Position the block's place in the body counted from 1, Type the block's Proof
// node type, Units its length in UTF-16 code units and Limit the bound it passed. The refusal's
// wording lives in Error() and nowhere else; a caller adds only the HTTP status and the code.
type CapExceeded struct {
	Field    string
	Position int
	Type     string
	Units    int
	Limit    int
}

func (e *CapExceeded) Error() string {
	return fmt.Sprintf("%s block %d (%s) is %d characters over the %d-character block limit (%d/%d)",
		e.Field, e.Position, e.Type, e.Units-e.Limit, e.Limit, e.Units, e.Limit)
}

// CountExceeded is a body holding more blocks than contracts.DispatchItemBlocks. Field is the
// field the body arrived in, Blocks how many blocks it holds and Limit the bound it passed. The
// refusal's wording lives in Error() and nowhere else, as CapExceeded's does.
type CountExceeded struct {
	Field  string
	Blocks int
	Limit  int
}

func (e *CountExceeded) Error() string {
	return fmt.Sprintf("%s is %d blocks over the %d-block limit (%d/%d)",
		e.Field, e.Blocks-e.Limit, e.Limit, e.Blocks, e.Limit)
}

// Check answers the first bound body passes, or nil when it passes none: a *CountExceeded when the
// body holds more than contracts.DispatchItemBlocks blocks, else a *CapExceeded naming the first
// block over contracts.DispatchBlockUnits. The count is checked first, so a body built to break
// the indexer is refused for how many blocks it holds rather than for one long block.
//
// The blocks are Split's, the body as its author wrote it, never the canonical rendering: a table
// the renderer pads and a list it escapes are measured as written, which is what is stored.
func Check(field, body string) error {
	blocks := Split(body)
	if len(blocks) > contracts.DispatchItemBlocks {
		return &CountExceeded{Field: field, Blocks: len(blocks), Limit: contracts.DispatchItemBlocks}
	}
	for index, block := range blocks {
		if units := Units16(block.Text); units > contracts.DispatchBlockUnits {
			return &CapExceeded{
				Field:    field,
				Position: index + 1,
				Type:     block.Type,
				Units:    units,
				Limit:    contracts.DispatchBlockUnits,
			}
		}
	}
	return nil
}

// Units16 is text's length in UTF-16 code units, how the server counts a text: a rune outside the
// Basic Multilingual Plane counts as the two units its surrogate pair takes. A byte that is not
// valid UTF-8 reads as U+FFFD, one unit, as the JSON encoder writes it.
func Units16(text string) int {
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	return units
}
