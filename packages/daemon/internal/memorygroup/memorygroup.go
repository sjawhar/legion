// Package memorygroup draws the memory group of a tree: the one word a project's `memory` block
// (config.Memory) gives a tree when the tree's lifecycle row is first created, kept from then on by
// the tree's root record (record.Issue.MemoryGroup). The draw is made here and nowhere else, so
// admission's own path and the operator's spawn draw the same way under the same mode.
package memorygroup

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Group is what a tree was drawn, in the words the record stores: GroupMemory is a tree whose every
// role runs with the project's memory overlays and instructions, GroupNone a tree that runs without
// them. There is no third word, and no empty one: a tree with no draw at all carries no group
// (record.Issue.MemoryDrawnAt nil), which is not this type's business.
type Group string

const (
	GroupMemory Group = "memory"
	GroupNone   Group = "none"
)

// draw is the group word gives a tree. `off` draws none; `on` draws memory for every tree;
// `random` flips coin once and draws memory on true. The word is a mode as a draw records it
// (config.Memory.Word), which is never the empty string a project naming no block leaves: the
// empty mode is turned into `off` in that one place, and nowhere else. An unknown word is refused
// naming it: the loader takes only the three (config.readMemory), so reaching that branch is a
// programming error, never an operator's file.
func draw(word config.MemoryMode, coin func() (bool, error)) (Group, error) {
	switch word {
	case config.MemoryOff:
		return GroupNone, nil
	case config.MemoryOn:
		return GroupMemory, nil
	case config.MemoryRandom:
		memory, err := coin()
		if err != nil {
			return "", fmt.Errorf("flip the memory coin: %w", err)
		}
		if memory {
			return GroupMemory, nil
		}
		return GroupNone, nil
	default:
		return "", fmt.Errorf("draw a memory group: unknown memory mode %q", word)
	}
}

// Records is the one write a draw makes: the group, the mode it was drawn under and the instant,
// onto the tree's root issue row, written once and never again (record.Store.DrawMemory). The
// one method is named here rather than taken as record.Store, so this package reads no record of
// the tree beyond the draw's own write, and a caller's store satisfies it as it stands.
type Records interface {
	DrawMemory(ctx context.Context, tx pgx.Tx, root, group, mode string, at time.Time) (bool, error)
}

// DrawAndRecord draws tree's memory group under memory's mode and records it on the tree's root,
// in tx: the one transaction that created the tree's lifecycle row, so a crash between the two
// leaves neither. It is both paths' whole draw — admission's promoteHolds and the operator's
// spawn — so a tree is drawn the same way whichever opened it, and the mode recorded is the word
// a draw records (config.Memory.Word), never the empty string a project naming no block leaves.
// Writing no row is no error and no business of this helper's: a root that carries a draw
// already, one that left a tree the daemon had opened, and a tree with no root record at all
// each keep what they have (record.Store.DrawMemory).
func DrawAndRecord(ctx context.Context, tx pgx.Tx, records Records, tree string, memory config.Memory,
	coin func() (bool, error), at time.Time) error {
	group, err := draw(memory.Word(), coin)
	if err != nil {
		return fmt.Errorf("draw the memory group of %s: %w", tree, err)
	}
	_, err = records.DrawMemory(ctx, tx, tree, string(group), string(memory.Word()), at)
	return err
}

// CryptoCoin is the fair coin the daemon draws `random` with: one byte of crypto/rand.Reader, whose
// low bit is as likely to be set as clear. The draw decides what a tree runs under for its whole
// life and is compared across trees, so it is taken from the operating system's own source rather
// than from a seeded math/rand sequence a reader of the code could replay.
func CryptoCoin() (bool, error) {
	var b [1]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return false, fmt.Errorf("read a random byte for the memory coin: %w", err)
	}
	return b[0]&1 == 1, nil
}
