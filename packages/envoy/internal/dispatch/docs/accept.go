package docs

import (
	"errors"
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
	_, broke, err := replacementBroke(before, after, match, check)
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

// padCutTables pads each table in the document-level blocks a splice of r changed, from before to
// after (changedBlocks), to its width (pmdoc.PadTables), as the browser editor's table plugin pads a
// table after any change.
func padCutTables(before, after *pmdoc.Node, r pmdoc.Range) (*pmdoc.Node, error) {
	first, _, last, err := changedBlocks(before, after, r)
	if err != nil {
		return nil, err
	}
	return pmdoc.PadTables(after, first, last), nil
}

// padsLikeTheBrowser reports whether an accept may pad the tables its splice cut: whether the
// browser editor's accept writes the replacement where Splice does (proof-sdk marks.ts accept and
// applyMarkdownReplace). A range running from one table into the next, or from one body row into
// another, is never padded: the browser can join the two tables or rows, which Splice does not. An
// inline replacement (text that stays in the textblock, or code's literal text) replaces the
// matched range there as here; an empty one follows this accept's own rule, deleting the matched
// text, where the browser's accept of an empty suggestion only clears its mark. Block content the
// browser takes across two textblocks, which it replaces whole (pmdoc.MultiblockRange). Splice
// replaces what the browser does only when the match is exactly those textblocks' content and the
// first, at, is a document-level block, not one inside a callout, a quote, a list item or a table
// cell. Other block content over a table, such as a list over one cell's whole text, is not padded
// either. An accept that is not padded is judged as the splice left it, which refuses one that cut
// a table.
func padsLikeTheBrowser(tree *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, inline bool) bool {
	if joinsTwo(tree, match, "table") || joinsTwo(tree, match, "table_row") {
		return false
	}
	if inline {
		return true
	}
	blocks, ok := pmdoc.MultiblockRange(tree, match)
	return ok && blocks == match && len(at.Ancestors) == 1
}

// acceptSpliceRefusal words the refusal of an accept whose replacement Splice cannot fit, naming
// what the person accepting can do: an inline replacement running into an ask or callout from the
// text before it would join the two and leave the ask or callout empty; a replacement no level of
// the document can hold where the suggestion sits; and any other join the document cannot hold.
func acceptSpliceRefusal(err error) error {
	switch {
	case errors.Is(err, pmdoc.ErrJoinEmptiesTypedBlock):
		return &ErrInvalidOp{Field: "anchor", Reason: "the suggestion runs into an ask or callout from the text before it, " +
			"and replacing it would join the two and leave the ask or callout empty; suggest a change inside one of them"}
	case errors.Is(err, pmdoc.ErrReplacementDoesNotFit):
		return &ErrInvalidOp{Field: "replace_with", Reason: "no part of the document can hold it where the suggestion sits " +
			"(a table cell's whole text, for one, can only be replaced by inline text)"}
	case errors.Is(err, pmdoc.ErrSchema):
		return &ErrInvalidOp{Field: "anchor", Reason: fmt.Sprintf("the suggestion runs across blocks that replacing it "+
			"would join, which the document cannot hold together (%v); reject the suggestion, or suggest a change inside one block", err)}
	}
	return err
}

// refuseAcceptedCodeThatReshapes refuses an accept whose code changes how a block around the code
// reads back (pmdoc.BlockShapeError), in the words of the other accept refusals: the block, why,
// and what the person accepting can do, since they cannot move the code as the edit route's
// refusal (refuseCodeThatReshapesItsBlock) advises. The block named is the typed block holding
// the code when the document-level block that reads back otherwise is the one holding the code,
// and otherwise that block, such as the list of a task item that a suggestion running out of the
// code empties ahead of the item's nested list.
func refuseAcceptedCodeThatReshapes(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string) error {
	broken, reshaped, err := replacementBroke(before, after, match, pmdoc.BlockShapeError)
	if err != nil || reshaped == nil {
		return err
	}
	// The document-level block holding the code is the first the accept changed (changedBlocks),
	// at the same index before and after it.
	holding, _, _, err := changedBlocks(before, after, match)
	if err != nil {
		return err
	}
	if broken == holding {
		for _, ancestor := range at.Ancestors {
			if pmdoc.IsTypedBlock(ancestor.Type) {
				holder := holderName(ancestor)
				return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
					"replace_with %q changes how the %s holding this code block reads back (%v); reject the suggestion, or reply asking for code the %s can hold",
					with, holder, reshaped, holder,
				)}
			}
		}
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
		"replace_with %q changes how the %s reads back (%v); reject the suggestion, or reply asking for a change that stays inside the code block",
		with, holderName(after.Children[broken]), reshaped,
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

// replacementBroke is the index in after of the first document-level block the write changed
// (changedBlocks) that check fails, and what check says of it, when it said nothing of the blocks
// the match lay in before: a block that already failed the check, or another block that does, is
// no reason to refuse this write.
func replacementBroke(before, after *pmdoc.Node, match pmdoc.Range, check func(*pmdoc.Node) error) (block int, broke, err error) {
	first, last, lastAfter, err := changedBlocks(before, after, match)
	if err != nil {
		return -1, nil, err
	}
	for index := first; index <= last; index++ {
		if check(before.Children[index]) != nil {
			return -1, nil, nil
		}
	}
	for index := first; index <= lastAfter; index++ {
		if broke = check(after.Children[index]); broke != nil {
			return index, broke, nil
		}
	}
	return -1, nil, nil
}
