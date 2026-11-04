// Package memorygroup draws the memory group of a tree: the one word a project's `memory` block
// (config.Memory) gives a tree when the tree's lifecycle row is first created, kept from then on by
// the tree's root record (record.Issue.MemoryGroup). The draw is made here and nowhere else, so
// admission's own path and the operator's spawn draw the same way under the same mode.
package memorygroup

import (
	"crypto/rand"
	"fmt"
	"io"

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

// Draw is the group mode gives a tree. `off`, and the empty mode of a project whose file names no
// `memory` block (config.Memory.Off), draw none; `on` draws memory for every tree; `random` flips
// coin once and draws memory on true. An unknown mode is refused naming it: the loader takes only
// the three words (config.readMemory), so reaching that branch is a programming error, never an
// operator's file.
func Draw(mode config.MemoryMode, coin func() (bool, error)) (Group, error) {
	switch mode {
	case "", config.MemoryOff:
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
		return "", fmt.Errorf("draw a memory group: unknown memory mode %q", mode)
	}
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
