package docs

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An accepted suggestion writes blocks, so what it stores is read back before it is written, and an
// accept whose document would not read back as the live tree is refused, naming replace_with, with
// nothing written and the suggestion left open. applySuggestion runs these in order: settleAccepted,
// then outside an ask refuseUnreadableAccept and refuseReshapedAccept over each document-level block
// the accept changed, the write's block-id check, refuseBrokenAsks, and last refuseMisreadAccept
// over the whole document.

// settleAccepted makes the blocks an accept changed hold what their markdown reads back as where
// that is unambiguous (pmdoc.AgreeWithReadBack): the empty halves a block replacement leaves of the
// textblock it lands in go where the renderer does not write them, and a list's spread is the one
// its markdown reads back with. An empty replacement keeps the paragraph it empties, which the
// renderer does not write beside other blocks.
func settleAccepted(before, after *pmdoc.Node, match pmdoc.Range, with string) (*pmdoc.Node, error) {
	first, _, lastAfter, err := changedBlocks(before, after, match)
	if err != nil {
		return nil, err
	}
	settled, _ := pmdoc.AgreeWithReadBack(after, first, lastAfter, with != "")
	return settled, nil
}

// insideAsk reports whether an accepted suggestion lands in an ask. refuseBrokenAsks is the ask
// round-trip and semantic rule, so it names an empty question or another invalid ask before a
// document-level parser failure can.
func insideAsk(at pmdoc.TextblockAt) bool {
	for _, ancestor := range at.Ancestors {
		if ancestor.Type == "ask" {
			return true
		}
	}
	return false
}

// refuseUnreadableAccept refuses a suggestion whose changed document-level block cannot be read
// from its rendered markdown.
func refuseUnreadableAccept(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node) error {
	return refuseAcceptBy(before, after, match, at, with, replacement, pmdoc.BlockReadError)
}

// refuseReshapedAccept refuses a suggestion whose changed document-level block reads back as
// blocks of another shape.
func refuseReshapedAccept(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node) error {
	return refuseAcceptBy(before, after, match, at, with, replacement, pmdoc.BlockShapeError)
}

func refuseAcceptBy(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node, check func(*pmdoc.Node) error) error {
	broke, err := replacementBroke(before, after, match, check)
	if err != nil || broke == nil {
		return err
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: acceptRefusal(before, after, match, at, with, replacement, broke)}
}

// refuseMisreadAccept refuses an accept whose document reads back otherwise where the accept
// reached (pmdoc.NewMisread): a block it changed, or one beside them that read back as written
// before, its attributes and text as well as its blocks, such as a task item emptied to `- [ ]`,
// which reads back as a plain item. What already read back otherwise before the accept, a block or
// an attribute, is not the accept's. The checks before it read one block at a time; this one reads
// the blocks beside each other. The refusal names what reads back and advises rejecting.
func refuseMisreadAccept(before, after *pmdoc.Node, match pmdoc.Range, with string) error {
	first, last, lastAfter, err := changedBlocks(before, after, match)
	if err != nil {
		return err
	}
	misread, err := pmdoc.NewMisread(before, after, first, last, lastAfter)
	if err != nil || misread == "" {
		return err
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
		"replace_with %q leaves blocks the document reads back otherwise (%s); reject the suggestion", with, misread,
	)}
}

// acceptRefusal says what an accepted suggestion's text does where it lands, why the document
// cannot carry it (broke), and what the person accepting can do: reject the suggestion, or ask for
// text the block can hold. It names no edit operation, since accepting takes none. Blocks that
// rewrite a typed block around the match in place land where that block stood, so that is the
// block named. The text empties the paragraph it lands in only when it renders no content, and then
// the delete it names removes only the paragraph where the rest of its block stands without it;
// blocks written at a list item's start leave the item's own line empty ahead of them.
func acceptRefusal(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node, broke error) string {
	holder := holderName(at.Ancestors[0])
	landed, found := pmdoc.ContainingTextblock(after, match.From)
	emptied := found && emptyTextblock(landed.Node)
	if !isInlineDocument(replacement) {
		parent := at.Ancestors[0]
		for index := 1; index < len(at.Ancestors) && pmdoc.IsTypedBlock(parent.Type) && pmdoc.RewritesBlock(replacement.Children, parent); index++ {
			parent = at.Ancestors[index]
		}
		holder = holderName(parent)
		where, ask := "in this "+holder, "text the "+holder+" can hold"
		if holder == "list item" && emptied {
			where, ask = "at the start of this list item", "the block to follow text on the item's line"
		}
		return fmt.Sprintf(
			"replace_with %q writes %s %s, which the document cannot read back there (%v); reject the suggestion, or reply asking for %s",
			with, pmdoc.BlockNames(replacement.Children), where, broke, ask,
		)
	}
	if emptyTextblock(replacement.Children[0]) && (!found || emptied) {
		advice := "reject the suggestion, or delete the " + holder + " in the document"
		if len(at.Ancestors[0].Children) > 1 {
			advice = "reject the suggestion, since the rest of the " + holder + " cannot be written without this paragraph"
			if _, err := pmdoc.DeleteBlock(before, blockID(at.Node)); err == nil {
				advice = "reject the suggestion, or delete the paragraph in the document, which leaves the rest of the " + holder
			}
		}
		return fmt.Sprintf(
			"replace_with %q empties the paragraph this %s holds, and the %s cannot be written with it empty; %s",
			with, holder, holder, advice,
		)
	}
	return fmt.Sprintf(
		"replace_with %q leaves text the document reads back as another block where it lands (%v); reject the suggestion, or reply asking for the text inside a line",
		with, broke,
	)
}

// holderName names the block holding a match as a reader names it.
func holderName(parent *pmdoc.Node) string {
	if parent.Type == "doc" {
		return "document"
	}
	return strings.ReplaceAll(parent.Type, "_", " ")
}

// emptyTextblock reports whether a textblock holds no text but whitespace.
func emptyTextblock(textblock *pmdoc.Node) bool {
	for _, child := range textblock.Children {
		if child.Type != "text" || strings.TrimSpace(child.Text) != "" {
			return false
		}
	}
	return true
}

// changedBlocks is the document-level blocks a write changed: before's from the one holding the
// match's start to the one holding its end, and after's from the same first one to as many more as
// the document gained, since the blocks after them keep their order. A replace stays inside its
// textblock, so its block holds the same index before and after; an accepted suggestion can write
// blocks, and its match can span them.
func changedBlocks(before, after *pmdoc.Node, match pmdoc.Range) (first, last, lastAfter int, err error) {
	if first, err = pmdoc.BlockIndex(before, pmdoc.Range{From: match.From, To: match.From}); err != nil {
		return 0, 0, 0, err
	}
	if last, err = pmdoc.BlockIndex(before, pmdoc.Range{From: match.To, To: match.To}); err != nil {
		return 0, 0, 0, err
	}
	return first, last, max(first, last+len(after.Children)-len(before.Children)), nil
}

// replacementBroke is what check says of a document-level block the write changed
// (changedBlocks), when it said nothing of the blocks the match lay in before: a block that already
// failed the check, or another block that does, is no reason to refuse this write.
func replacementBroke(before, after *pmdoc.Node, match pmdoc.Range, check func(*pmdoc.Node) error) (broke, err error) {
	first, last, lastAfter, err := changedBlocks(before, after, match)
	if err != nil {
		return nil, err
	}
	for index := first; index <= last; index++ {
		if check(before.Children[index]) != nil {
			return nil, nil
		}
	}
	for index := first; index <= lastAfter; index++ {
		if broke = check(after.Children[index]); broke != nil {
			return broke, nil
		}
	}
	return nil, nil
}
