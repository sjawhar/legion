package docs

import (
	"fmt"
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
	written := &pmdoc.Node{Type: "bullet_list", Children: make([]*pmdoc.Node, 0, len(merged))}
	if list != nil {
		written.Attrs = list.Attrs
	}
	for _, option := range merged {
		written.Children = append(written.Children, option.written)
	}
	// The merged list carries what the document already holds under an option as well as the
	// lines the caller sent, and that content is not the caller's text: charging the write's
	// budget for it would refuse an edit of one option's line for the weight of another option's
	// sub-list, which the caller never sent and cannot make smaller. So the caller's label and
	// description text is weighed on their budget above (RefusePlainText) and the merged
	// rendering is parsed on a budget of its own.
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

// mergedOption is one item of the list an options edit writes: the node the merged markdown
// writes for it, and the item the sent option was paired with, which carries the identity and
// the nodes that rendering cannot (restoreOptionItems). held is nil for an option no item held.
type mergedOption struct {
	written *pmdoc.Node
	held    *pmdoc.Node
	// whole reports that the held item already says what the edit sends, so the result is that
	// item itself and not what the parse made of its rendering.
	whole bool
}

// mergeAskOptions is the item each sent option is written as, in the order sent, over the
// pairing the ask-edit route weighed before it (pairedAskOptions): a pair that says the same
// thing keeps its item whole, a pair whose description changed has the item's line rewritten
// above everything else the item holds, and an option no item holds is a new item. An item no
// option is paired with is dropped, which is what a removal, a rename and an edit sending no
// option at all come to. What that drop and that rewrite would delete is refused before the
// write (refuseLostOptionWords), except for settlement's restore of a decided ask's recorded
// wording, the one caller no refusal can reach (writeDecidedWording).
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
		merged = append(merged, mergedOption{
			written: withChildren(item, append([]*pmdoc.Node{optionParagraph(option)}, itemContent(item)...)),
			held:    item,
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
// held is what each item of the list says, read as parseAskBlock reads it (heldAskOptions). The
// ask-edit route's refusals and the merge pair here, over this one list, so the words the route
// weighs are the words the merge writes over (refuseLostOptionWords, mergeAskOptions).
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
// It is weighed before the block's children are built, over the pairing the merge writes over
// (pairedAskOptions), so the two can never disagree, and outside the merge because settlement's
// restore of a decided wording goes through the merge with no caller to answer a refusal
// (writeDecidedWording).
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
	options := normalizeAskOptions(*edit.Options)
	items := list.Children
	held := heldAskOptions(items)
	taken := make([]bool, len(items))
	for sent, match := range pairedAskOptions(held, options) {
		if match < 0 {
			continue
		}
		taken[match] = true
		if held[match].Description == options[sent].Description {
			continue
		}
		// An item holding nothing has no line to lose: the rewrite writes one and deletes
		// nothing.
		if len(items[match].Children) == 0 {
			continue
		}
		line := items[match].Children[0]
		if rendersTheSame(line, optionParagraph(held[match])) {
			continue
		}
		return refuseRewrittenOptionLine(line, held[match])
	}
	for index, item := range items {
		if taken[index] || holdsOnlyItsOptionLine(item, held[index]) {
			continue
		}
		return refuseDroppedOptionItem(list, item, held[index])
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
// the list item of option, which holds more than the line the option writes. An ask row shows
// nothing of what a person wrote under an option, so the caller sending the edit cannot see what
// dropping it deletes: the refusal quotes the item whole, its nested content and all, as the
// markdown of a document holding one bullet list holding it alone, so every word of it can be
// restored, and the item is quoted as data (strconv.Quote), never written into the reason raw.
func refuseDroppedOptionItem(list, item *pmdoc.Node, option model.AskOption) error {
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		{Type: "bullet_list", Attrs: list.Attrs, Children: []*pmdoc.Node{item}},
	}})
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
// again: it renders to exactly what a fresh item for option renders to (optionItem). An item
// holding a sub-list, a second paragraph, a link's address, bold, code or an image renders to
// more than that, and dropping it would delete what a person wrote; a mark the markdown never
// writes, the editor's own on every character a person types among them, renders to nothing and
// is nothing to lose (rendersTheSame).
func holdsOnlyItsOptionLine(item *pmdoc.Node, option model.AskOption) bool {
	return rendersTheSame(listOfOneItem(item), listOfOneItem(optionItem(option)))
}

// listOfOneItem is a list item in the shape a document holds it, which is the shape a refusal
// quotes it in and the shape one item is compared with another in (refuseDroppedOptionItem): a
// bullet list holding it alone. The list carries no attributes of its own, so what two of these
// render differently is the items.
func listOfOneItem(item *pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: "bullet_list", Children: []*pmdoc.Node{item}}
}

// rendersTheSame reports whether two nodes write the same markdown, which is all a document
// keeps of them: an anchor mark the rendering never writes - the editor's proofAuthored on every
// character a person types, a comment's, a suggestion's - is in neither, and a link's address,
// bold, code or an image is in one alone. A node the renderer refuses is held to be the same as
// nothing, which no node it accepts renders to, so a caller weighing what a write would delete
// keeps what it cannot read.
func rendersTheSame(left, right *pmdoc.Node) bool {
	leftMarkdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{left}})
	if err != nil {
		return false
	}
	rightMarkdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{right}})
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
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{line}})
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
