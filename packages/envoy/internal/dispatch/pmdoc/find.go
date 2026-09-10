package pmdoc

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Range is a half-open ProseMirror position range.
type Range struct {
	From int `json:"from"`
	To   int `json:"to"`
}

var ErrTargetNotFound = errors.New("pmdoc: target not found")

// Candidate gives one matching range and enough surrounding markdown to
// disambiguate it.
type Candidate struct {
	Range
	Context string `json:"context"`
}

// ErrTargetAmbiguous reports a quote that needs an occurrence or position hint.
type ErrTargetAmbiguous struct {
	Candidates []Candidate
}

func (e *ErrTargetAmbiguous) Error() string { return "pmdoc: target is ambiguous" }

// FindQuote finds quote in the rendered markdown and returns its ProseMirror
// range. occurrence is zero-based. When several exact matches exist, near
// chooses the range whose start is nearest the ProseMirror position hint.
func FindQuote(doc *Node, quote string, occurrence *int, near *int) (Range, error) {
	markdown, positions, err := Render(doc)
	if err != nil {
		return Range{}, err
	}

	matches := exactQuoteMatches(markdown, quote, positions)
	if len(matches) == 0 {
		matches = normalizedQuoteMatches(markdown, quote, positions)
	}
	if len(matches) == 0 {
		return Range{}, ErrTargetNotFound
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			return Range{}, ErrTargetNotFound
		}
		return matches[*occurrence].Range, nil
	}
	if len(matches) == 1 {
		return matches[0].Range, nil
	}
	if near != nil {
		best := matches[0]
		bestDistance := distance(best.From, *near)
		for _, match := range matches[1:] {
			candidateDistance := distance(match.From, *near)
			if candidateDistance < bestDistance || (candidateDistance == bestDistance && match.From < best.From) {
				best = match
				bestDistance = candidateDistance
			}
		}
		return best.Range, nil
	}

	candidates := make([]Candidate, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, Candidate{
			Range:   match.Range,
			Context: text.Slice16(markdown, max(0, match.mdFrom-40), min(len16(markdown), match.mdTo+40)),
		})
	}
	return Range{}, &ErrTargetAmbiguous{Candidates: candidates}
}

type quoteMatch struct {
	Range
	mdFrom int
	mdTo   int
}

func exactQuoteMatches(markdown, quote string, positions *PositionMap) []quoteMatch {
	needle := utf16.Encode([]rune(quote))
	var matches []quoteMatch
	for _, offset := range findAllUnits(utf16.Encode([]rune(markdown)), needle) {
		matches = append(matches, quoteMatch{
			Range:  Range{From: positions.ToPM(offset), To: positions.ToPM(offset + len(needle))},
			mdFrom: offset,
			mdTo:   offset + len(needle),
		})
	}
	return matches
}

type range16 struct {
	from int
	to   int
}

func normalizedQuoteMatches(markdown, quote string, positions *PositionMap) []quoteMatch {
	normalizedMarkdown, markdownSpans := normalizedRanges(markdown)
	normalizedQuote, quoteSpans := normalizedRanges(quote)
	if normalizedQuote == "" || len(markdownSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	needle := []rune(normalizedQuote)
	var matches []quoteMatch
	for _, offset := range findAllRunes([]rune(normalizedMarkdown), needle) {
		from := markdownSpans[offset].from
		to := markdownSpans[offset+len(needle)-1].to
		matches = append(matches, quoteMatch{
			Range:  Range{From: positions.ToPM(from), To: positions.ToPM(to)},
			mdFrom: from,
			mdTo:   to,
		})
	}
	return matches
}

func findAllUnits(haystack, needle []uint16) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	matches := make([]int, 0)
	for start := 0; start <= len(haystack)-len(needle); start++ {
		if matched := true; matched {
			for offset, unit := range needle {
				if haystack[start+offset] != unit {
					matched = false
					break
				}
			}
			if matched {
				matches = append(matches, start)
			}
		}
	}
	return matches
}

func findAllRunes(haystack, needle []rune) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	matches := make([]int, 0)
	for start := 0; start <= len(haystack)-len(needle); start++ {
		matched := true
		for offset, value := range needle {
			if haystack[start+offset] != value {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	return matches
}

func normalizedRanges(value string) (string, []range16) {
	out := make([]rune, 0, len(value))
	spans := make([]range16, 0, len(value))
	unitOffset := 0
	inWhitespace := false
	whitespace := range16{}
	for _, value := range []rune(value) {
		width := len16(string(value))
		if unicode.IsSpace(value) {
			if !inWhitespace {
				whitespace = range16{from: unitOffset, to: unitOffset + width}
				inWhitespace = true
			} else {
				whitespace.to = unitOffset + width
			}
			unitOffset += width
			continue
		}
		if inWhitespace && len(out) > 0 {
			out = append(out, ' ')
			spans = append(spans, whitespace)
		}
		inWhitespace = false
		out = append(out, value)
		spans = append(spans, range16{from: unitOffset, to: unitOffset + width})
		unitOffset += width
	}
	return string(out), spans
}

func distance(left, right int) int {
	if left < right {
		return right - left
	}
	return left - right
}

// FindMark finds the first contiguous text range covered by a mark identity.
func FindMark(doc *Node, markType, id string) (Range, string, bool) {
	var matches []markedText
	position := 0
	walkTextNodes(doc, &position, func(node *Node, from int) {
		if nodeMarkID(node, markType) != id {
			return
		}
		matches = append(matches, markedText{Range: Range{From: from, To: from + len16(node.Text)}, text: node.Text})
	})
	if len(matches) == 0 {
		return Range{}, "", false
	}

	first := matches[0]
	var quote strings.Builder
	quote.WriteString(first.text)
	out := first.Range
	for _, match := range matches[1:] {
		if match.From != out.To {
			break
		}
		out.To = match.To
		quote.WriteString(match.text)
	}
	return out, quote.String(), true
}

type markedText struct {
	Range
	text string
}

func nodeMarkID(node *Node, markType string) string {
	for _, mark := range node.Marks {
		if mark.Type != markType {
			continue
		}
		if id, ok := mark.Attrs["id"].(string); ok {
			return id
		}
	}
	return ""
}

func walkTextNodes(node *Node, position *int, visit func(*Node, int)) {
	if node == nil {
		return
	}
	if node.Type == "doc" {
		for _, child := range node.Children {
			walkTextNodes(child, position, visit)
		}
		return
	}
	if node.Type == "text" {
		visit(node, *position)
		*position += len16(node.Text)
		return
	}
	if isLeafNodeType(node.Type) {
		*position++
		return
	}
	*position++
	for _, child := range node.Children {
		walkTextNodes(child, position, visit)
	}
	*position++
}

func isLeafNodeType(nodeType string) bool {
	switch nodeType {
	case "hr", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

// MarkRange adds mark to the text covered by r. Marks cannot span distinct
// inline containers such as paragraphs or table cells.
func MarkRange(txn *crdt.Transaction, frag *crdt.YXmlFragment, r Range, mark Mark) error {
	if txn == nil || frag == nil || r.From >= r.To {
		return fmt.Errorf("%w: invalid mark range", ErrSchema)
	}
	if !markTypes[mark.Type] {
		return fmt.Errorf("%w: mark %q", ErrSchema, mark.Type)
	}

	spans, err := yTextRanges(frag)
	if err != nil {
		return err
	}
	var affected []yTextRange
	for _, span := range spans {
		if span.From < r.To && r.From < span.To {
			affected = append(affected, span)
		}
	}
	if len(affected) == 0 {
		return ErrTargetNotFound
	}
	block := affected[0].block
	if block == nil {
		return fmt.Errorf("%w: mark range is outside an inline container", ErrSchema)
	}
	for _, span := range affected[1:] {
		if span.block != block {
			return fmt.Errorf("%w: mark range spans blocks", ErrSchema)
		}
	}

	attributes := crdt.Attributes{mark.Type: markAttributeValue(mark)}
	for _, span := range affected {
		from := max(r.From, span.From) - span.From
		to := min(r.To, span.To) - span.From
		span.text.Format(txn, from, to-from, attributes)
	}
	return nil
}

func markAttributeValue(mark Mark) crdt.Attributes {
	value := make(crdt.Attributes, len(mark.Attrs))
	for key, attr := range mark.Attrs {
		value[key] = attr
	}
	return value
}

type yTextRange struct {
	text  *crdt.YXmlText
	block *crdt.YXmlElement
	Range
}

func yTextRanges(frag *crdt.YXmlFragment) ([]yTextRange, error) {
	position := 0
	var ranges []yTextRange
	if err := walkYFragment(frag, &position, nil, &ranges); err != nil {
		return nil, err
	}
	return ranges, nil
}

func walkYFragment(frag *crdt.YXmlFragment, position *int, block *crdt.YXmlElement, ranges *[]yTextRange) error {
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			length := current.Len()
			*ranges = append(*ranges, yTextRange{text: current, block: block, Range: Range{From: *position, To: *position + length}})
			*position += length
		case *crdt.YXmlElement:
			if isLeafNodeType(current.NodeName) {
				*position++
				continue
			}
			*position++
			nextBlock := block
			if isInlineContainerType(current.NodeName) {
				nextBlock = current
			}
			if err := walkYFragment(&current.YXmlFragment, position, nextBlock, ranges); err != nil {
				return err
			}
			*position++
		default:
			return fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return nil
}

func isInlineContainerType(nodeType string) bool {
	switch nodeType {
	case "paragraph", "heading":
		return true
	default:
		return false
	}
}

// Unmark removes every markType mark whose id attribute equals id.
func Unmark(txn *crdt.Transaction, frag *crdt.YXmlFragment, markType, id string) error {
	if txn == nil || frag == nil {
		return fmt.Errorf("%w: Unmark requires transaction and fragment", ErrSchema)
	}
	if !markTypes[markType] {
		return fmt.Errorf("%w: mark %q", ErrSchema, markType)
	}

	var texts []*crdt.YXmlText
	if err := walkYTexts(frag, &texts); err != nil {
		return err
	}
	found := false
	for _, ytext := range texts {
		operations, err := yTextDeltaInTransaction(ytext)
		if err != nil {
			return err
		}
		offset := 0
		for _, operation := range operations {
			value, ok := operation.Insert.(string)
			if !ok {
				return fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
			}
			for attributeName, attributeValue := range operation.Attributes {
				if yattrToMarkName(attributeName) == markType && markAttributeID(attributeValue) == id {
					ytext.Format(txn, offset, len16(value), crdt.Attributes{attributeName: nil})
					found = true
				}
			}
			offset += len16(value)
		}
	}
	if !found {
		return ErrTargetNotFound
	}
	return nil
}

func markAttributeID(value any) string {
	attrs, err := attrsFromY(value)
	if err != nil {
		return ""
	}
	id, _ := attrs["id"].(string)
	return id
}

func walkYTexts(frag *crdt.YXmlFragment, texts *[]*crdt.YXmlText) error {
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			*texts = append(*texts, current)
		case *crdt.YXmlElement:
			if err := walkYTexts(&current.YXmlFragment, texts); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return nil
}

// Splice returns a copy of doc with r replaced by with. An inline replacement
// is used only when r and with are both contained in one inline block.
func Splice(doc *Node, r Range, with *Node) (*Node, error) {
	if doc == nil || with == nil || doc.Type != "doc" || with.Type != "doc" || r.From >= r.To {
		return nil, fmt.Errorf("%w: invalid splice", ErrSchema)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	if err := with.Validate(); err != nil {
		return nil, err
	}

	fromIndex, fromStart, ok := blockContaining(doc, r.From)
	if !ok {
		return nil, ErrTargetNotFound
	}
	toIndex, _, ok := blockContaining(doc, r.To)
	if !ok {
		return nil, ErrTargetNotFound
	}
	out := cloneNode(doc)
	if fromIndex == toIndex && isInlineContainerType(out.Children[fromIndex].Type) && inlineDocument(with) {
		if err := spliceInline(out.Children[fromIndex], r, fromStart, with.Children[0].Children); err != nil {
			return nil, err
		}
		return out, nil
	}

	replacement := make([]*Node, 0, len(with.Children))
	for _, node := range with.Children {
		replacement = append(replacement, cloneNode(node))
	}
	out.Children = append(out.Children[:fromIndex:fromIndex], append(replacement, out.Children[toIndex+1:]...)...)
	return out, nil
}

func blockContaining(doc *Node, position int) (index, start int, ok bool) {
	for index, block := range doc.Children {
		size := nodeSize(block)
		if position >= start+1 && position <= start+size-1 {
			return index, start, true
		}
		start += size
	}
	return 0, 0, false
}

func inlineDocument(doc *Node) bool {
	if len(doc.Children) != 1 || !isInlineContainerType(doc.Children[0].Type) {
		return false
	}
	for _, child := range doc.Children[0].Children {
		if child.Type != "text" && !isLeafNodeType(child.Type) {
			return false
		}
	}
	return true
}

func spliceInline(block *Node, r Range, blockStart int, replacement []*Node) error {
	position := blockStart + 1
	children := make([]*Node, 0, len(block.Children)+len(replacement))
	inserted := false
	for _, child := range block.Children {
		size := nodeSize(child)
		end := position + size
		if r.To <= position || r.From >= end {
			appendInline(&children, []*Node{cloneNode(child)})
			position = end
			continue
		}
		if child.Type == "text" {
			if r.From > position {
				appendText(&children, text.Slice16(child.Text, 0, r.From-position), child.Marks)
			}
			if !inserted {
				for _, node := range replacement {
					appendInline(&children, []*Node{cloneNode(node)})
				}
				inserted = true
			}
			if r.To < end {
				appendText(&children, text.Slice16(child.Text, r.To-position, size), child.Marks)
			}
		} else {
			if r.From > position || r.To < end {
				return fmt.Errorf("%w: splice partially selects inline leaf", ErrSchema)
			}
			if !inserted {
				for _, node := range replacement {
					appendInline(&children, []*Node{cloneNode(node)})
				}
				inserted = true
			}
		}
		position = end
	}
	if !inserted {
		return ErrTargetNotFound
	}
	block.Children = children
	return nil
}

func nodeSize(node *Node) int {
	if node == nil {
		return 0
	}
	if node.Type == "text" {
		return len16(node.Text)
	}
	if isLeafNodeType(node.Type) {
		return 1
	}
	size := 2
	for _, child := range node.Children {
		size += nodeSize(child)
	}
	return size
}

func cloneNode(node *Node) *Node {
	if node == nil {
		return nil
	}
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: node.Text, Marks: cloneMarks(node.Marks)}
	for _, child := range node.Children {
		out.Children = append(out.Children, cloneNode(child))
	}
	return out
}

func cloneAttrs(attrs Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}
	out := make(Attrs, len(attrs))
	for key, value := range attrs {
		out[key] = value
	}
	return out
}

func cloneMarks(marks []Mark) []Mark {
	if len(marks) == 0 {
		return nil
	}
	out := make([]Mark, len(marks))
	for index, mark := range marks {
		out[index] = Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)}
	}
	return out
}

// StripAnchorMarks returns a copy of doc with Proof and Dispatch anchors
// removed, coalescing adjacent equal-mark text runs.
func StripAnchorMarks(node *Node) *Node {
	if node == nil {
		return nil
	}
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: node.Text}
	for _, mark := range node.Marks {
		if !strings.HasPrefix(mark.Type, "proof") && mark.Type != "dispatchAsk" {
			out.Marks = append(out.Marks, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
		}
	}
	for _, child := range node.Children {
		clean := StripAnchorMarks(child)
		if len(out.Children) > 0 {
			previous := out.Children[len(out.Children)-1]
			if previous.Type == "text" && clean.Type == "text" && marksEqual(previous.Marks, clean.Marks) {
				previous.Text += clean.Text
				continue
			}
		}
		out.Children = append(out.Children, clean)
	}
	return out
}
