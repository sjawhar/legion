package docs

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An accepted suggestion writes blocks, so what it stores is read back before it is written, and an
// accept whose document would not read back as the live tree is refused, naming replace_with, with
// nothing written and the suggestion left open. applySuggestion runs these in order: settleAccepted,
// then outside an ask refuseUnreadableAccept and refuseReshapedAccept over each document-level block
// the accept changed, the write's block-id check, refuseBrokenAsks, and last refuseMisreadAccept
// over the whole document.

// settleAccepted makes what an accept wrote hold what its markdown reads back as where that is
// unambiguous (pmdoc.AgreeWithReadBack): the empty halves a block replacement leaves of the
// textblock it lands in go where the renderer does not write them, and the lists and list items in
// the blocks it changed take the spread their markdown reads back with, except one it left
// unchanged whose spread already read back otherwise. An empty replacement keeps the paragraph it
// empties, which the renderer does not write beside other blocks. Everything else, block, mark and
// node, stays as it was.
func settleAccepted(before, after *pmdoc.Node, match pmdoc.Range, with string) (*pmdoc.Node, error) {
	first, _, lastAfter, err := changedBlocks(before, after, match)
	if err != nil {
		return nil, err
	}
	return pmdoc.AgreeWithReadBack(before, after, first, lastAfter, with != ""), nil
}

// acceptedCode is the code text an accepted suggestion writes in the code block at, and the range
// it writes it over, match or the lines around it, as that code reads back, so the accept stores
// what reads back and changes nothing but the lines it writes: in a list item's code, the lines it
// leaves blank (blankListItemLines); then, markdown dropping the line breaks that end code, where
// only line breaks follow the match, the text's own ending breaks, a blank line it ends with
// included. A match can run past the code into later blocks of tree, to the textblock it ends in.
// One that takes all of that textblock's text leaves nothing after the text, as at the code's end,
// so the same rules apply. One that ends inside that text is written as sent: the rest of it,
// which the splice joins to the code, follows the text, and neither rule can judge it, so a line
// of spaces and tabs that text leaves in a list item's code reads back empty and the accept is
// refused.
func acceptedCode(tree *pmdoc.Node, with string, at pmdoc.TextblockAt, match pmdoc.Range) (string, pmdoc.Range) {
	past := match.To > at.Content.To
	if past {
		if end, _ := pmdoc.ContainingTextblock(tree, match.To); match.To != end.Content.To {
			return with, match
		}
	}
	var text strings.Builder
	for _, child := range at.Node.Children {
		text.WriteString(child.Text)
	}
	code := utf16.Encode([]rune(text.String()))
	from, to := match.From-at.Content.From, min(match.To, at.Content.To)-at.Content.From
	if insideListItem(at) {
		with, from, to = blankListItemLines(code, with, from, to)
	}
	if !slices.ContainsFunc(code[to:], func(unit uint16) bool { return unit != '\n' }) {
		with = strings.TrimRight(with, "\n")
	}
	written := pmdoc.Range{From: at.Content.From + from, To: at.Content.From + to}
	if past {
		written.To = match.To
	}
	return with, written
}

// blankListItemLines writes empty each line that with, written over code from to to, leaves
// holding only spaces and tabs, the spaces and tabs the line keeps around the match included, and
// widens from and to over those. A list item's code reads such a line, CommonMark's blank line,
// back empty; any other character, a no-break space or a form feed among them, is kept, as both
// readers keep it.
func blankListItemLines(code []uint16, with string, from, to int) (string, int, int) {
	lineStart, lineEnd := from, to
	for lineStart > 0 && code[lineStart-1] != '\n' {
		lineStart--
	}
	for lineEnd < len(code) && code[lineEnd] != '\n' {
		lineEnd++
	}
	lines := strings.Split(with, "\n")
	last := len(lines) - 1
	for index := range lines {
		line := lines[index]
		if index == 0 {
			line = string(utf16.Decode(code[lineStart:from])) + line
		}
		if index == last {
			line += string(utf16.Decode(code[to:lineEnd]))
		}
		if strings.Trim(line, " \t") != "" {
			continue
		}
		lines[index] = ""
		if index == 0 {
			from = lineStart
		}
		if index == last {
			to = lineEnd
		}
	}
	return strings.Join(lines, "\n"), from, to
}

func insideListItem(at pmdoc.TextblockAt) bool {
	for _, ancestor := range at.Ancestors {
		if ancestor.Type == "list_item" {
			return true
		}
	}
	return false
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

// refuseMisreadAccept refuses an accept whose document reads back otherwise where it did not
// before (pmdoc.NewMisread), its attributes and text as well as its blocks, such as a task item
// emptied to `- [ ]`, which reads back as a plain item. A block that already read back otherwise
// the same way before the accept, the one it changed included, is not the accept's. The checks
// before it read one block at a time; this one reads the blocks beside each other. The refusal
// names what reads back and advises rejecting.
func refuseMisreadAccept(before, after *pmdoc.Node, with string) error {
	misread := pmdoc.NewMisread(before, after)
	if misread == "" {
		return nil
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

// refuseAcceptedCodeThatReshapes refuses an accept whose code changes how a block around the code
// reads back (pmdoc.BlockShapeError), in the words of the other accept refusals: the block, why,
// and what the person accepting can do, since they cannot move the code as the edit route's
// refusal (refuseCodeThatReshapesItsBlock) advises. The block named is the typed block holding
// the code, or else the document-level block that reads back otherwise, such as the table a
// suggestion running out of the code runs into.
func refuseAcceptedCodeThatReshapes(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string) error {
	var broken *pmdoc.Node
	reshaped, err := replacementBroke(before, after, match, func(block *pmdoc.Node) error {
		err := pmdoc.BlockShapeError(block)
		if err != nil {
			broken = block
		}
		return err
	})
	if err != nil || reshaped == nil {
		return err
	}
	for _, ancestor := range at.Ancestors {
		if pmdoc.IsTypedBlock(ancestor.Type) {
			holder := holderName(ancestor)
			return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
				"replace_with %q changes how the %s holding this code block reads back (%v); reject the suggestion, or reply asking for code the %s can hold",
				with, holder, reshaped, holder,
			)}
		}
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
		"replace_with %q changes how the %s reads back (%v); reject the suggestion, or reply asking for a change that stays inside the code block",
		with, holderName(broken), reshaped,
	)}
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
