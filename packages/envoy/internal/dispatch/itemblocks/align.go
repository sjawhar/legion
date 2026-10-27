package itemblocks

import (
	"fmt"
	"strconv"
)

// Stored is a block an item holds: its id, type and identity hash (Hash of its last text), and its
// position among the item's current blocks, -1 once removed.
type Stored struct {
	ID, Type, Hash string
	Position       int
}

// Align gives each block of next an id, keeping the ids stored holds, for an edit that carries no
// ids (or carries them against a revision that is no longer the item's):
//  1. a block whose text equals a live stored block's keeps that id (so a moved block keeps it);
//  2. one equal to a removed block's last text takes that id back (a restored block);
//  3. one between two blocks that kept ids by rule 1 takes, in order, the id of an untaken live
//     stored block of its type that stood between those two (a reworded block);
//  4. every other takes b<counter>, counting up, so no number is used twice;
//  5. a live stored block nothing took is removed.
//
// Rules 1 and 2 match by Hash and take the first untaken match in stored's order. For rule 3, a
// block's bounds are the stored positions of the nearest earlier and later blocks that kept their
// ids by rule 1, or the start and the end without one. counter is the next number to mint, above
// every number stored holds. It returns the ids in next's order and the counter after the last id
// minted.
func Align(stored []Stored, next []Block, counter int) ([]string, int) {
	ids := make([]string, len(next))
	taken := make([]bool, len(stored))
	hashes := make([]string, len(next))
	kept := make([]int, len(next)) // the stored position of a block that kept its id by rule 1, else -1
	for index, block := range next {
		hashes[index] = Hash(block.Text)
		kept[index] = -1
		if match := firstUntaken(stored, taken, hashes[index], true); match >= 0 {
			taken[match] = true
			ids[index] = stored[match].ID
			kept[index] = stored[match].Position
		}
	}
	for index, block := range next {
		if ids[index] != "" {
			continue
		}
		match := firstUntaken(stored, taken, hashes[index], false)
		if match < 0 {
			lower, upper := keptBounds(kept, index)
			match = reworded(stored, taken, block.Type, lower, upper)
		}
		if match >= 0 {
			taken[match] = true
			ids[index] = stored[match].ID
			continue
		}
		ids[index], counter = mint(counter)
	}
	return ids, counter
}

// keptBounds is the stored positions of the nearest blocks before and after next[index] that kept
// their ids by rule 1: -1 for the start, and the largest int for the end.
func keptBounds(kept []int, index int) (lower, upper int) {
	lower, upper = -1, int(^uint(0)>>1)
	for before := index - 1; before >= 0; before-- {
		if kept[before] >= 0 {
			lower = kept[before]
			break
		}
	}
	for after := index + 1; after < len(kept); after++ {
		if kept[after] >= 0 {
			upper = kept[after]
			break
		}
	}
	return lower, upper
}

// reworded is the untaken live stored block of nodeType nearest the lower bound strictly between
// the bounds, or -1.
func reworded(stored []Stored, taken []bool, nodeType string, lower, upper int) int {
	match := -1
	for index, block := range stored {
		if taken[index] || block.Type != nodeType || block.Position <= lower || block.Position >= upper {
			continue
		}
		if match < 0 || block.Position < stored[match].Position {
			match = index
		}
	}
	return match
}

// firstUntaken is the first untaken stored block, live or removed as live says, whose hash is hash,
// or -1.
func firstUntaken(stored []Stored, taken []bool, hash string, live bool) int {
	for index, block := range stored {
		if !taken[index] && (block.Position >= 0) == live && block.Hash == hash {
			return index
		}
	}
	return -1
}

// mint is the id b<counter> and the counter after it.
func mint(counter int) (string, int) {
	return "b" + strconv.Itoa(counter), counter + 1
}

// Carried is a block id the dashboard editor carried through an edit, and From, the byte offset in
// the edited body where the id's span starts.
type Carried struct {
	ID   string
	From int
}

// Carry gives each block of next an id for an edit that carries ids (ReadCarried checks them):
// each block takes the id of the carried span whose From falls in the block's range, and the
// ranges cover the body from its start: the first block's runs from offset 0, and every block's up
// to the next block's start, the last block's to the end of the body, so a span starting in the
// blank lines between two blocks belongs to the earlier one. When several fall in one block, the
// first keeps its id and the others are removed (a join). A split leaves the original From in the
// part that holds the block's start, so that part keeps the id and the other holds none. A block
// with no carried id takes Align's rule 1 (an untaken live stored block of equal text, a move) or
// rule 2 (a removed block of equal last text, a restore), else b<counter>; rule 3 does not apply,
// since a reworded block carries its id. A carried id is never taken by rules 1 and 2, even one a
// join removed. carried is in body order. It returns the ids in next's order and the counter after
// the last id minted.
func Carry(stored []Stored, next []Block, carried []Carried, counter int) ([]string, int) {
	ids := make([]string, len(next))
	taken := make([]bool, len(stored))
	named := make(map[string]bool, len(carried))
	block := 0
	for _, span := range carried {
		named[span.ID] = true
		for block+1 < len(next) && next[block+1].From <= span.From {
			block++
		}
		if block < len(next) && ids[block] == "" {
			ids[block] = span.ID
		}
	}
	for index, block := range stored {
		taken[index] = named[block.ID]
	}
	for index, block := range next {
		if ids[index] != "" {
			continue
		}
		hash := Hash(block.Text)
		match := firstUntaken(stored, taken, hash, true)
		if match < 0 {
			match = firstUntaken(stored, taken, hash, false)
		}
		if match >= 0 {
			taken[match] = true
			ids[index] = stored[match].ID
			continue
		}
		ids[index], counter = mint(counter)
	}
	return ids, counter
}

// CarriedError names the field of an edit's carried blocks that is wrong, as blocks[<index>].id or
// blocks[<index>].from, for the route's 400 to name.
type CarriedError struct {
	Field  string
	Reason string
}

func (e *CarriedError) Error() string { return e.Field + ": " + e.Reason }

// ReadCarried checks the ids an edit carries and returns them with each From as a byte offset
// into body. Each entry's From arrives as the editor counts it, in UTF-16 code units into body (the
// body the edit writes). An id must name a live block of stored, the item's blocks at the revision
// the edit was made against, and no id may come twice; a From must fall between two characters of
// body, and no From may come before the one ahead of it (two equal ones fall in one block, a join).
// The first wrong field is answered as a *CarriedError.
func ReadCarried(stored []Stored, body string, carried []Carried) ([]Carried, error) {
	live := make(map[string]bool, len(stored))
	for _, block := range stored {
		if block.Position >= 0 {
			live[block.ID] = true
		}
	}
	seen := make(map[string]bool, len(carried))
	out := make([]Carried, len(carried))
	for index, span := range carried {
		switch {
		case !live[span.ID]:
			return nil, &CarriedError{Field: fmt.Sprintf("blocks[%d].id", index), Reason: fmt.Sprintf("%q is not a block of the item at base_revision", span.ID)}
		case seen[span.ID]:
			return nil, &CarriedError{Field: fmt.Sprintf("blocks[%d].id", index), Reason: fmt.Sprintf("%q is named twice", span.ID)}
		}
		seen[span.ID] = true
		from, err := ByteOffset(body, span.From)
		if err != nil {
			return nil, &CarriedError{Field: fmt.Sprintf("blocks[%d].from", index), Reason: err.Error()}
		}
		if index > 0 && from < out[index-1].From {
			return nil, &CarriedError{Field: fmt.Sprintf("blocks[%d].from", index), Reason: fmt.Sprintf("%d comes before blocks[%d].from %d", span.From, index-1, carried[index-1].From)}
		}
		out[index] = Carried{ID: span.ID, From: from}
	}
	return out, nil
}
