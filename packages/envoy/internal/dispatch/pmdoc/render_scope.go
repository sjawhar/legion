package pmdoc

// typedScope is a typed block the renderer is writing the blocks of: the prefix its lines are written
// at, how many colons its fence has, and how many quotes stand around it.
type typedScope struct {
	prefix string
	colons int
	quotes int
}

// scope is where the blocks being written stand: the innermost container around them, nil at the
// document's level, and the prefix its lines are written behind - the prefix of the lines around it
// and its own columns, a list item's width, a quote's `> `, a footnote definition's indentation,
// or none, a typed block's - with how many list items and quotes stand around them, each a quote
// marker on their lines' prefix, the footnote definition around them (footnoteScope) and the
// innermost typed block (typedScope). A lone `:::` closes a three-colon typed block as a line less
// than four columns past the prefix its lines are written at (typedFenceReach), which a paragraph
// written there can produce.
type scope struct {
	node     *Node
	prefix   string
	items    int
	quotes   int
	footnote *footnoteScope
	typed    *typedScope
	// listReadsLoose reports, in a list item's frame, whether goldmark's looseness reads the item's
	// list's spread as it is (loosenessReadsSpread).
	listReadsLoose bool
}

// footnoteScope is a footnote definition the blocks being written stand in: how many quotes stand
// around it, and where its indentation starts in the prefix of every line written inside it.
type footnoteScope struct {
	quotes   int
	indentAt int
}

// scope is the frame the blocks being written stand in, the document's where no container does.
func (r *renderer) scope() scope {
	if len(r.scopes) == 0 {
		return scope{}
	}
	return r.scopes[len(r.scopes)-1]
}

// inFootnote reports whether the blocks being written stand in a footnote definition.
func (r *renderer) inFootnote() bool { return r.scope().footnote != nil }

// enter and leave bracket the writing of a container's blocks: enter pushes their frame, a copy of
// the container's own with the container counted.
func (r *renderer) enter(container *Node, prefix string) {
	frame := r.scope()
	frame.node, frame.prefix = container, prefix
	switch container.Type {
	case "blockquote":
		frame.quotes++
	case "list_item":
		frame.items++
	case "footnote_definition":
		frame.footnote = &footnoteScope{quotes: frame.quotes, indentAt: len(prefix) - len(definitionIndent)}
	}
	r.scopes = append(r.scopes, frame)
}

// enterTyped enters a typed block written with a fence of colons.
func (r *renderer) enterTyped(container *Node, prefix string, colons int) {
	r.enter(container, prefix)
	frame := &r.scopes[len(r.scopes)-1]
	frame.typed = &typedScope{prefix: prefix, colons: colons, quotes: frame.quotes}
}

// enterItem enters a list item of a list whose spread goldmark's looseness reads as it is, or not
// (listReadsLoose).
func (r *renderer) enterItem(item *Node, indent string, listReadsLoose bool) {
	r.enter(item, indent)
	r.scopes[len(r.scopes)-1].listReadsLoose = listReadsLoose
}

func (r *renderer) leave() { r.scopes = r.scopes[:len(r.scopes)-1] }

// definitionOpensOnALaterLine reports whether the footnote definition being written opens with
// nothing past its `]:` and its first block on the next line, which the browser editor's parser
// reads as a blank line of the list item holding the definition, spreading it. It is written so
// where the definition is the only block a spread item writes and nothing else spreads the item:
// no blank line between two of the definition's own blocks (spreadByItsOwnLines), and no quote
// around the item, where the blank lines after it are the definition's (blanksAfterDefinitionItem).
func (r *renderer) definitionOpensOnALaterLine() bool {
	frame := r.scope()
	if frame.node == nil || frame.quotes > 0 || frame.footnote != nil {
		return false
	}
	item := frame.node
	return item.Type == "list_item" && item.Attrs["spread"] == true && !spreadByItsOwnLines(item, false, false)
}

// inItemBelowQuotes reports whether the blocks being written stand in a list item with no quote
// between: a line holding only their prefix is a blank line in the item, which the browser editor
// reads as spacing it, where with a quote between it is the quote's.
func (r *renderer) inItemBelowQuotes() bool {
	for index := len(r.scopes) - 1; index >= 0; index-- {
		switch r.scopes[index].node.Type {
		case "blockquote":
			return false
		case "list_item":
			return true
		}
	}
	return false
}

// emptyCodeWrittenBare reports whether an empty code block is written as its fences alone, with no
// line between them, where no quote between takes a line holding only the prefix as its own: in a
// footnote definition, where both parsers keep that line as the code's text, and in a typed block in
// a list item, where it is a blank line the browser editor reads as spacing the nearest item. That
// line is written there, as main wrote it, where the nearest item is spread and goldmark's
// looseness, which this parser leaves that item's list to, reads the list's spread as it is
// (loosenessReadsSpread), unless the document holds a shape this parser cannot decide the spacing
// of (browserOnlyShape), where it is refused: render writes such a document again with the fences
// alone (bareEmptyCode).
func (r *renderer) emptyCodeWrittenBare() bool {
	typed := false
	for index := len(r.scopes) - 1; index >= 0; index-- {
		switch r.scopes[index].node.Type {
		case "blockquote":
			return false
		case "footnote_definition":
			return true
		case "list_item":
			if !typed {
				continue
			}
			if r.bareEmptyCode || r.scopes[index].node.Attrs["spread"] != true || !r.scopes[index].listReadsLoose {
				return true
			}
			r.wroteBlankEmptyCode = true
			return false
		default:
			typed = true
		}
	}
	return false
}
