package docs

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// editedOptionList is the block's bullet list after an options edit, or nil where the edit sends
// no option and the block keeps no list. Each sent option is paired with the item already
// holding it (mergeAskOptions), the merged items are rendered and parsed as any other document
// write is (parsedBlockBody), so the list and its items carry the attributes that parse gives,
// spread included, and the identity the markdown cannot carry is put back afterwards
// (restoreOptionItems). list is the block's own list, nil where it holds none and every option
// is new.
//
// The items the caller sent are charged to budget, the write's, and the merged list is parsed on
// one of its own, so the list is parsed twice: a parse is the only way to count what an item
// makes, and the two parses weigh different content. What the caller sends is their text - a new
// item, and the line a rewrite replaces an item's own with (callerSentItems) - so one write may
// make no more of it than any other write may (pmdoc.MaxDocumentElements), counting the question
// it sends beside them (askBlockChildren). The merged list carries what the document already
// holds under an option as well, which is not the caller's text: charging the write's budget for
// it would refuse an edit of one option's line for the weight of another option's sub-list, which
// the caller never sent and cannot make smaller.
func editedOptionList(list *pmdoc.Node, options []model.AskOption, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	merged := mergeAskOptions(list, options)
	if len(merged) == 0 {
		return nil, nil
	}
	texts := make([]string, 0, 2*len(options))
	for _, option := range options {
		texts = append(texts, option.Label, option.Description)
	}
	if err := budget.RefusePlainText(texts...); err != nil {
		return nil, err
	}
	if sent := callerSentItems(merged); len(sent.Children) > 0 {
		if _, refusal, err := parsedBlockBody([]*pmdoc.Node{sent}, budget); err != nil {
			return nil, err
		} else if refusal != "" {
			return nil, &ErrAskBlockUnrepresentable{Field: "options", Reason: refusal}
		}
	}
	written := &pmdoc.Node{Type: "bullet_list", Children: make([]*pmdoc.Node, 0, len(merged))}
	if list != nil {
		written.Attrs = list.Attrs
	}
	for _, option := range merged {
		written.Children = append(written.Children, option.written)
	}
	rebuilt, refusal, err := parsedBlockBody([]*pmdoc.Node{written}, pmdoc.NewWriteBudget())
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, &ErrAskBlockUnrepresentable{Field: "options", Reason: refusal}
	}
	if len(rebuilt) != 1 || rebuilt[0].Type != "bullet_list" {
		return nil, &ErrAskBlockUnrepresentable{
			Field:  "options",
			Reason: "the options do not read back as one bullet list",
		}
	}
	return restoreOptionItems(rebuilt[0], list, merged)
}

// callerSentItems is the list of what an options edit's caller sent of their own, out of the
// merged items (mergeAskOptions): an item no option held, whole, and for a rewritten line the
// caller's own line alone (mergedOption.line), without the nodes under it the document already
// held. Its parse is what charges the write's budget for the caller's items (editedOptionList),
// so it carries no content the document already holds; an item the merge keeps whole carries
// nothing the caller sent and is left out.
func callerSentItems(merged []mergedOption) *pmdoc.Node {
	sent := &pmdoc.Node{Type: "bullet_list", Children: make([]*pmdoc.Node, 0, len(merged))}
	for _, option := range merged {
		switch {
		case option.held == nil:
			sent.Children = append(sent.Children, option.written)
		case !option.whole:
			sent.Children = append(sent.Children, withChildren(option.written, []*pmdoc.Node{option.line}))
		}
	}
	return sent
}

// mergedOption is one item of the list an options edit writes: the node the merged markdown
// writes for it, and the item the sent option was paired with, which carries the identity and
// the nodes that rendering cannot (restoreOptionItems). held is nil for an option no item held.
type mergedOption struct {
	written *pmdoc.Node
	held    *pmdoc.Node
	// whole reports that the held item already says what the edit sends, so the result is that
	// item itself and not what the parse made of its rendering.
	whole bool
	// line is the line the caller sent for a rewritten item, the paragraph the merge wrote above
	// what that item already held, and nil for every other entry: the charge for the caller's own
	// text is of the node they sent (callerSentItems), never of whichever node the merge happened
	// to write first.
	line *pmdoc.Node
}

// mergeAskOptions is the item each sent option is written as, in the order sent, over the
// pairing the ask-edit route weighed before it (pairedAskOptions): a pair that says the same
// thing keeps its item whole, a pair whose description changed has the item's line rewritten
// above everything else the item holds, and an option no item holds is a new item. An item no
// option is paired with is dropped, which is what a removal, a rename and an edit sending no
// option at all come to. What that drop and that rewrite would delete is refused before the
// write (refuseLostOptionWords), except for settlement's restore of a decided ask's recorded
// wording, which writeDecidedWording says why no refusal reaches.
func mergeAskOptions(list *pmdoc.Node, options []model.AskOption) []mergedOption {
	var items []*pmdoc.Node
	if list != nil {
		items = list.Children
	}
	held := heldAskOptions(items)
	merged := make([]mergedOption, 0, len(options))
	for sent, match := range pairedAskOptions(held, options) {
		option := options[sent]
		if match < 0 {
			merged = append(merged, mergedOption{written: optionItem(option)})
			continue
		}
		item := items[match]
		if held[match].Description == option.Description {
			merged = append(merged, mergedOption{written: item, held: item, whole: true})
			continue
		}
		line := optionParagraph(option)
		merged = append(merged, mergedOption{
			written: withChildren(item, append([]*pmdoc.Node{line}, itemContent(item)...)),
			held:    item,
			line:    line,
		})
	}
	return merged
}

// pairedAskOptions is the item each sent option is written in, by its index in the list, -1 for
// an option no item holds. It pairs in two passes over the options, in the order sent: each
// option first takes the first item no earlier option took that says exactly what it says, and
// then an option still unpaired takes the first item no option took holding its label. Two items
// can carry one label, and the words a person wrote belong to the item they wrote them under:
// pairing on the label alone would give the first of them to the first option sent, so an edit
// changing the other's description would move a person's sub-list under the line it rewrote and
// drop the item that says what it left alone.
//
// held is what each item of the list says, read as parseAskBlock reads it (heldAskOptions). It
// is read once, by the merge, and the ask-edit route's refusals weigh the merge's result
// (refuseLostOptionWords), so the words the route weighs are the words the merge writes over.
func pairedAskOptions(held, options []model.AskOption) []int {
	pairs := make([]int, len(options))
	for index := range pairs {
		pairs[index] = -1
	}
	taken := make([]bool, len(held))
	for _, whole := range []bool{true, false} {
		for sent, option := range options {
			if pairs[sent] >= 0 {
				continue
			}
			for index, item := range held {
				if taken[index] || item.Label != option.Label || whole && item != option {
					continue
				}
				pairs[sent], taken[index] = index, true
				break
			}
		}
	}
	return pairs
}

// heldAskOptions is the option each item of a list holds, as parseAskBlock reads it
// (optionOfItem).
func heldAskOptions(items []*pmdoc.Node) []model.AskOption {
	held := make([]model.AskOption, len(items))
	for index, item := range items {
		held[index] = optionOfItem(item)
	}
	return held
}

// refuseLostOptionWords refuses an options edit of current that would delete words only the
// document holds, writing nothing; it returns nil where the edit deletes none. The ask row shows
// nothing of what a person wrote under an option or of the markup in its line, so the caller
// sending the edit cannot see what it would take: an item no sent option is paired with is
// dropped by the merge, and a pair whose description changed has its line replaced by the plain
// line the edit sends (mergeAskOptions).
//
// It weighs the merge's own result rather than pairing a second time (mergeAskOptions): an entry
// holding an item the merge does not keep whole is a rewrite of that item's line, and an item no
// entry holds is a drop, so what the refusal weighs and what the write does cannot drift. A
// rewrite is weighed before a drop, and the whole edit is refused on the first of either. It is
// weighed outside the merge because settlement's restore of a decided wording goes through the
// merge, which writeDecidedWording says why no refusal reaches.
func refuseLostOptionWords(current askBlock, edit AskBlockEdit) error {
	edit = withoutUnchanged(current, edit)
	if edit.Options == nil {
		return nil
	}
	// A block holding no list holds no words to lose, and one holding more than one is refused
	// the edit where its children are built (askBlockChildren).
	list := optionListOf(askBlockLists(current.node))
	if list == nil {
		return nil
	}
	merged := mergeAskOptions(list, normalizeAskOptions(*edit.Options))
	paired := make(map[*pmdoc.Node]struct{}, len(merged))
	for _, option := range merged {
		if option.held == nil {
			continue
		}
		paired[option.held] = struct{}{}
		// An item the merge keeps whole is written back as it is, and one holding nothing has
		// no line to lose: the rewrite writes one and deletes nothing.
		if option.whole || len(option.held.Children) == 0 {
			continue
		}
		if line := option.held.Children[0]; !writesNothingButItsText(line) {
			return refuseRewrittenOptionLine(line, optionOfItem(option.held))
		}
	}
	for _, item := range list.Children {
		if _, held := paired[item]; held || holdsOnlyItsOptionLine(item) {
			continue
		}
		return refuseDroppedOptionItem(list, item, optionOfItem(item))
	}
	return nil
}

// restoreOptionItems gives parsed, the bullet list the merged markdown read back as, the identity
// its markdown does not carry: list's own block id, since it is the same list and the anchors
// inside it must survive; for an option the edit left alone, the very node the document holds;
// and for one it rewrote, that item's own attributes and the nodes under its line, with the
// parsed paragraph of the new line above them. A new item keeps what the parse gave it.
func restoreOptionItems(parsed, list *pmdoc.Node, merged []mergedOption) (*pmdoc.Node, error) {
	if len(parsed.Children) != len(merged) {
		return nil, &ErrAskBlockUnrepresentable{
			Field:  "options",
			Reason: fmt.Sprintf("the options read back as %d items, not the %d sent", len(parsed.Children), len(merged)),
		}
	}
	for index, option := range merged {
		switch {
		case option.held == nil:
		case option.whole:
			parsed.Children[index] = option.held
		default:
			line := parsed.Children[index].Children
			if len(line) == 0 || line[0].Type != "paragraph" {
				return nil, &ErrAskBlockUnrepresentable{
					Field:  "options",
					Reason: fmt.Sprintf("option %d does not read back as a plain line", index+1),
				}
			}
			parsed.Children[index] = withChildren(option.held, append([]*pmdoc.Node{line[0]}, itemContent(option.held)...))
		}
	}
	if list != nil {
		if id, _ := list.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			parsed.Attrs[pmdoc.BlockIDAttr] = id
		}
	}
	return parsed, nil
}

// refuseDroppedOptionItem refuses the whole options edit, writing nothing, because it drops item,
// which holds more than its own plain line (holdsOnlyItsOptionLine); option is what the item
// says, read as parseAskBlock reads it, and names it in the refusal. An ask row shows
// nothing of what a person wrote under an option, so the caller sending the edit cannot see what
// dropping it deletes: the refusal quotes the item whole, its nested content and all, as the
// markdown of a document holding one bullet list holding it alone, so every word of it can be
// restored, and the item is quoted as data (strconv.Quote), never written into the reason raw.
func refuseDroppedOptionItem(list, item *pmdoc.Node, option model.AskOption) error {
	markdown, err := renderBlock(&pmdoc.Node{Type: "bullet_list", Attrs: list.Attrs, Children: []*pmdoc.Node{item}})
	if err != nil {
		return fmt.Errorf("render the list item of a dropped option: %w", err)
	}
	return &ErrAskBlockUnrepresentable{
		Field: "options",
		Reason: fmt.Sprintf("the edit drops the option %s, whose list item holds more than its plain line, "+
			"and the ask-edit route does not delete what a person wrote under an option; its item reads %s; "+
			"send that option back with this edit, or remove what the item holds with a document edit first "+
			"(dispatch doc-edit, deleting a block by its id)",
			strconv.Quote(option.Label), strconv.Quote(strings.TrimSpace(markdown))),
	}
}

// optionOfItem is the option parseAskBlock reads in a list item: the text of its first block, cut
// at the first ": " into a label and a description, both trimmed, and no description where the
// text holds no ": ".
func optionOfItem(item *pmdoc.Node) model.AskOption {
	if len(item.Children) == 0 {
		return model.AskOption{}
	}
	label, description, found := strings.Cut(strings.TrimSpace(pmdoc.TextContent(item.Children[0])), ": ")
	if !found {
		description = ""
	}
	return model.AskOption{Label: strings.TrimSpace(label), Description: strings.TrimSpace(description)}
}

// itemContent is what a list item holds under its line: every block after the first, which a
// rewrite of that line keeps and a drop would delete.
func itemContent(item *pmdoc.Node) []*pmdoc.Node {
	if len(item.Children) == 0 {
		return nil
	}
	return item.Children[1:]
}

// holdsOnlyItsOptionLine reports whether item holds nothing a rebuilt list would not write
// again. It weighs what the whole item writes, not what its line writes: `checked` is the one
// list-item attribute the markdown writes as content - the `- [x] ` or `- [ ] ` of a task - so
// an item is weighed against one holding the same line's text and breaks (textAndBreaksOf) under
// its own list-item attributes but for that one. A ticked option is then no edit's to drop, and
// the issue's task count does not change under the person who ticked it, while the attributes a
// rebuilt list carries anyway (`label`, `listType`, `spread`) are on both sides and blame
// nothing.
//
// An item holding a sub-list, a second paragraph, a link's address, bold, code or an image
// writes more than its line, and dropping it would delete what a person wrote; the whitespace
// and the line breaks a person typed in that line are text, and are written again, so an item
// whose line reads "Hold: Wait " is the edit's to drop as one reading "Hold: Wait" is.
func holdsOnlyItsOptionLine(item *pmdoc.Node) bool {
	if len(item.Children) != 1 || item.Children[0].Type != "paragraph" {
		return false
	}
	attrs := maps.Clone(item.Attrs)
	delete(attrs, "checked")
	return rendersTheSame(itemInAListOfItsOwn(item), itemInAListOfItsOwn(&pmdoc.Node{
		Type:     "list_item",
		Attrs:    attrs,
		Children: []*pmdoc.Node{{Type: "paragraph", Children: textAndBreaksOf(item.Children[0])}},
	}))
}

// itemInAListOfItsOwn is item in the shape a document holds it, a bullet list holding it alone,
// which is the shape the renderer reads: a list item is no block of a document on its own. Both
// sides of a weighing are wrapped the same way (holdsOnlyItsOptionLine), so what the list
// carries of its own makes no difference to them.
func itemInAListOfItsOwn(item *pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: "bullet_list", Children: []*pmdoc.Node{item}}
}

// rendersTheSame reports whether two nodes write the same markdown, which is all a document
// keeps of them: an anchor mark the rendering never writes - the editor's proofAuthored on every
// character a person types, a comment's, a suggestion's - is in neither, and a link's address,
// bold, code or an image is in one alone. A node the renderer refuses is held to be the same as
// nothing, which no node it accepts renders to, so a caller weighing what a write would delete
// keeps what it cannot read.
func rendersTheSame(left, right *pmdoc.Node) bool {
	leftMarkdown, err := renderBlock(left)
	if err != nil {
		return false
	}
	rightMarkdown, err := renderBlock(right)
	if err != nil {
		return false
	}
	return leftMarkdown == rightMarkdown
}

// refuseRewrittenOptionLine refuses the whole options edit, writing nothing, because it rewrites
// the line of option, and line, the block the item holds that line in, writes more than the line
// itself: a link's address, bold, code or an image in it. The rewrite replaces that block with
// the plain line the edit sends (mergeAskOptions), which would delete the markup unseen, the
// deletion the drop refusal exists to prevent (refuseDroppedOptionItem). The line is quoted
// whole, as data (strconv.Quote), never written into the reason raw.
func refuseRewrittenOptionLine(line *pmdoc.Node, option model.AskOption) error {
	markdown, err := renderBlock(line)
	if err != nil {
		return fmt.Errorf("render the line of a rewritten option: %w", err)
	}
	return &ErrAskBlockUnrepresentable{
		Field: "options",
		Reason: fmt.Sprintf("the edit rewrites the line of the option %s, which holds more than its plain "+
			"text, and the ask-edit route does not delete the markup a person wrote in a line; its line reads "+
			"%s; send that option back unchanged with this edit, or rewrite the line with a document edit "+
			"(dispatch doc-edit, replacing quoted text with replace)",
			strconv.Quote(option.Label), strconv.Quote(strings.TrimSpace(markdown))),
	}
}

// optionLine is the plain line parseAskBlock reads an option back from: `Label: Description`, or
// a bare `Label` where it has no description.
func optionLine(option model.AskOption) string {
	if option.Description == "" {
		return option.Label
	}
	return option.Label + ": " + option.Description
}

// optionParagraph is the paragraph an option's list item opens with, holding its line alone.
func optionParagraph(option model.AskOption) *pmdoc.Node {
	return &pmdoc.Node{Type: "paragraph", Children: inlineWithHardBreaks(optionLine(option))}
}

// optionItem is the list item an option no list item holds is written as.
func optionItem(option model.AskOption) *pmdoc.Node {
	return &pmdoc.Node{Type: "list_item", Children: []*pmdoc.Node{optionParagraph(option)}}
}
