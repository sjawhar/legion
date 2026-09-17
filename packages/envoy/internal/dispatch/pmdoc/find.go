package pmdoc

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

// Range is a half-open ProseMirror position range.
type Range struct {
	From int `json:"from"`
	To   int `json:"to"`
}

var ErrTargetNotFound = errors.New("pmdoc: target not found")
var ErrTargetSpansBlocks = errors.New("pmdoc: target spans textblocks")

// ErrQuoteNotFound reports a quote miss with up to three closest rendered textblocks, in
// document order among equals. It unwraps ErrTargetNotFound so callers can preserve their
// existing miss handling.
type ErrQuoteNotFound struct {
	Nearest []string
}

func (e *ErrQuoteNotFound) Error() string {
	return fmt.Sprintf("%s; nearest blocks: %s", ErrTargetNotFound, QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return ErrTargetNotFound }

// QuoteBlocks renders nearest-block candidates as `"a" | "b" | "c"` for error text.
func QuoteBlocks(blocks []string) string {
	quoted := make([]string, len(blocks))
	for index, block := range blocks {
		quoted[index] = fmt.Sprintf("%q", block)
	}
	return strings.Join(quoted, " | ")
}

// nearestBlockCount is how many candidate blocks a quote miss names.
const nearestBlockCount = 3

// atxHeadingMarker is the `# ` … `###### ` prefix a quote may carry to mean "the heading
// whose text follows"; the level is not matched, only the heading-ness.
var atxHeadingMarker = regexp.MustCompile(`^#{1,6} +`)

// Candidate gives one matching range and enough surrounding document text to
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

// FindQuote finds quote in the document text and returns its ProseMirror
// range. occurrence is zero-based. When several exact matches exist, near
// chooses the range whose start is nearest the ProseMirror position hint.
func FindQuote(doc *Node, quote string, occurrence *int, near *int) (Range, error) {
	if doc == nil {
		return Range{}, fmt.Errorf("%w: FindQuote wants a document", ErrSchema)
	}
	if err := doc.Validate(); err != nil {
		return Range{}, err
	}
	text := buildFlattenedText(doc)
	var matches []quoteMatch
	if marker := atxHeadingMarker.FindString(quote); marker != "" && marker != quote {
		matches = headingQuoteMatches(doc, text, quote[len(marker):])
	} else {
		matches = quoteMatches(text, quote)
	}
	if len(matches) == 0 {
		return Range{}, quoteNotFound(doc, quote)
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
			Context: slice16(text.value, max(0, match.textFrom-40), min(len16(text.value), match.textTo+40)),
		})
	}
	return Range{}, &ErrTargetAmbiguous{Candidates: candidates}
}

// TargetSpansBlocks reports whether r is not wholly contained by one
// textblock. Inline code and links remain inside their containing textblock.
func TargetSpansBlocks(doc *Node, r Range) bool {
	withinTextblock := false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if isTextblock(node.Type) && pos+1 <= r.From && r.To <= end-1 {
			withinTextblock = true
			return false
		}
		return true
	})
	return !withinTextblock
}

// FindHeading finds the heading whose text equals title exactly. occurrence is
// zero-based; without it, repeated titles return ErrTargetAmbiguous.
func FindHeading(doc *Node, title string, occurrence *int) (Range, error) {
	if doc == nil {
		return Range{}, fmt.Errorf("%w: FindHeading wants a document", ErrSchema)
	}
	if err := doc.Validate(); err != nil {
		return Range{}, err
	}

	var matches []Candidate
	pos := 0
	for _, child := range doc.Children {
		end := pos + nodeSize(child)
		if child.Type == "heading" {
			var text strings.Builder
			for _, inline := range child.Children {
				if inline.Type == "text" {
					text.WriteString(inline.Text)
				}
			}
			headingText := text.String()
			if headingText == title {
				matches = append(matches, Candidate{
					Range:   Range{From: pos, To: end},
					Context: headingText,
				})
			}
		}
		pos = end
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
	return Range{}, &ErrTargetAmbiguous{Candidates: matches}
}

// Size returns the ProseMirror position immediately after the last block.
func Size(doc *Node) int {
	return nodeSize(doc)
}

type inlineMarkup uint8

const (
	inlineMarkupCode inlineMarkup = 1 << iota
	inlineMarkupStrong
	inlineMarkupEmphasis
	inlineMarkupLink
)

type flattenedText struct {
	value     string
	positions []int
	marks     []inlineMarkup
}

func buildFlattenedText(doc *Node) flattenedText {
	var out strings.Builder
	var positions []int
	var marks []inlineMarkup
	lastEnd := -1
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "text" {
			return true
		}
		if lastEnd >= 0 && lastEnd != pos {
			out.WriteByte(' ')
			positions = append(positions, lastEnd)
			marks = append(marks, 0)
		}
		markup := nodeInlineMarkup(node)
		for _, char := range node.Text {
			out.WriteRune(char)
			width := 1
			if char > 0xffff {
				width = 2
			}
			for offset := range width {
				positions = append(positions, pos+offset)
				marks = append(marks, markup)
			}
			pos += width
		}
		lastEnd = end
		return true
	})
	return flattenedText{value: out.String(), positions: positions, marks: marks}
}

func nodeInlineMarkup(node *Node) inlineMarkup {
	var markup inlineMarkup
	for _, mark := range node.Marks {
		switch mark.Type {
		case "inlineCode":
			markup |= inlineMarkupCode
		case "strong":
			markup |= inlineMarkupStrong
		case "emphasis":
			markup |= inlineMarkupEmphasis
		case "link":
			markup |= inlineMarkupLink
		}
	}
	return markup
}

type quoteMatch struct {
	Range
	textFrom int
	textTo   int
}

// quoteMatches is the matcher cascade a plain quote runs through: exact text first, then
// whitespace-normalized text, then the quote rendered as markdown against inline markup.
func quoteMatches(text flattenedText, quote string) []quoteMatch {
	matches := exactQuoteMatches(text, quote)
	if len(matches) == 0 {
		matches = normalizedQuoteMatches(text, quote)
	}
	if len(matches) == 0 {
		matches = markdownQuoteMatches(text, quote)
	}
	return matches
}

func exactQuoteMatches(text flattenedText, quote string) []quoteMatch {
	needle := utf16.Encode([]rune(quote))
	var matches []quoteMatch
	for _, offset := range findAllUnits(utf16.Encode([]rune(text.value)), needle) {
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[offset],
				To:   text.positions[offset+len(needle)-1] + 1,
			},
			textFrom: offset,
			textTo:   offset + len(needle),
		})
	}
	return matches
}

type range16 struct {
	from int
	to   int
}

func normalizedQuoteMatches(text flattenedText, quote string) []quoteMatch {
	normalizedText, textSpans := normalizedRanges(text.value)
	normalizedQuote, quoteSpans := normalizedRanges(quote)
	if normalizedQuote == "" || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	needle := []rune(normalizedQuote)
	var matches []quoteMatch
	for _, offset := range findAllRunes([]rune(normalizedText), needle) {
		from := textSpans[offset].from
		to := textSpans[offset+len(needle)-1].to
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[from],
				To:   text.positions[to-1] + 1,
			},
			textFrom: from,
			textTo:   to,
		})
	}
	return matches
}

func markdownQuoteMatches(text flattenedText, quote string) []quoteMatch {
	rendered, marked := renderedMarkdownQuote(quote)
	if !marked {
		return nil
	}
	return normalizedMarkdownQuoteMatches(text, rendered)
}

func renderedMarkdownQuote(quote string) (flattenedText, bool) {
	parsed, err := Parse(quote)
	if err != nil {
		return flattenedText{}, false
	}
	rendered := buildFlattenedText(parsed)
	if rendered.value == quote || !hasInlineMarkup(rendered.marks) {
		return flattenedText{}, false
	}
	return rendered, true
}

func hasInlineMarkup(marks []inlineMarkup) bool {
	for _, markup := range marks {
		if markup != 0 {
			return true
		}
	}
	return false
}

func normalizedMarkdownQuoteMatches(text, quote flattenedText) []quoteMatch {
	normalizedText, textSpans := normalizedRanges(text.value)
	normalizedQuote, quoteSpans := normalizedRanges(quote.value)
	if normalizedQuote == "" || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	needle := []rune(normalizedQuote)
	var matches []quoteMatch
	for _, offset := range findAllRunes([]rune(normalizedText), needle) {
		if !matchingInlineMarkup(text, quote, textSpans[offset:offset+len(needle)], quoteSpans) {
			continue
		}
		from := textSpans[offset].from
		to := textSpans[offset+len(needle)-1].to
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[from],
				To:   text.positions[to-1] + 1,
			},
			textFrom: from,
			textTo:   to,
		})
	}
	return matches
}

func matchingInlineMarkup(text, quote flattenedText, textSpans, quoteSpans []range16) bool {
	for index, quoteSpan := range quoteSpans {
		required := markupInRange(quote.marks, quoteSpan)
		if required == 0 {
			continue
		}
		if markupInRange(text.marks, textSpans[index])&required != required {
			return false
		}
	}
	return true
}

func markupInRange(marks []inlineMarkup, span range16) inlineMarkup {
	var markup inlineMarkup
	for index := span.from; index < span.to; index++ {
		markup |= marks[index]
	}
	return markup
}

func quoteNotFound(doc *Node, quote string) error {
	if marker := atxHeadingMarker.FindString(quote); marker != "" {
		quote = quote[len(marker):]
	} else if rendered, marked := renderedMarkdownQuote(quote); marked {
		quote = rendered.value
	}
	return &ErrQuoteNotFound{Nearest: nearestBlocks(doc, quote, nearestBlockCount)}
}

// nearestBlocks ranks every textblock by the length of its common prefix with quote and
// returns the top `limit` (ties keep document order), each trimmed to 80 runes.
func nearestBlocks(doc *Node, quote string, limit int) []string {
	type ranked struct {
		text   string
		prefix int
	}
	var candidates []ranked
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if isTextblock(node.Type) {
			candidate := textContent(node)
			candidates = append(candidates, ranked{text: candidate, prefix: commonPrefixLength(quote, candidate)})
		}
		return true
	})
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].prefix > candidates[right].prefix
	})
	nearest := make([]string, 0, limit)
	for _, candidate := range candidates[:min(limit, len(candidates))] {
		nearest = append(nearest, firstRunes(candidate.text, 80))
	}
	return nearest
}

// headingQuoteMatches returns the text range of every heading whose rendered text equals
// title, so a quote written with its ATX marker (`## Title`) selects headings only.
func headingQuoteMatches(doc *Node, text flattenedText, title string) []quoteMatch {
	if title == "" {
		return nil
	}
	candidates := quoteMatches(text, title)
	var matches []quoteMatch
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "heading" {
			return true
		}
		for _, match := range candidates {
			if pos+1 <= match.From && match.To <= end-1 {
				matches = append(matches, match)
			}
		}
		return true
	})
	return matches
}

func textContent(node *Node) string {
	if node.Type == "text" {
		return node.Text
	}
	if node.Type == "hardbreak" {
		return "\n"
	}
	var out strings.Builder
	for _, child := range node.Children {
		out.WriteString(textContent(child))
	}
	return out.String()
}

func commonPrefixLength(left, right string) int {
	prefix := 0
	rightOffset := 0
	for _, leftRune := range left {
		if rightOffset == len(right) {
			return prefix
		}
		rightRune, width := utf8.DecodeRuneInString(right[rightOffset:])
		if leftRune != rightRune {
			return prefix
		}
		rightOffset += width
		prefix++
	}
	return prefix
}

func firstRunes(value string, count int) string {
	for index := range value {
		if count == 0 {
			return value[:index]
		}
		count--
	}
	return value
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

// FindMark finds the first document-contiguous range covered by a mark identity.
func FindMark(doc *Node, markType, id string) (Range, string, bool) {
	var quote strings.Builder
	var marked Range
	found := false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "text" {
			return true
		}
		if nodeMarkID(node, markType) != id {
			return !found
		}
		if !found {
			found = true
			marked = Range{From: pos, To: end}
			quote.WriteString(node.Text)
			return true
		}
		if pos != marked.To {
			quote.WriteByte(' ')
		}
		marked.To = end
		quote.WriteString(node.Text)
		return true
	})
	if !found {
		return Range{}, "", false
	}
	return marked, quote.String(), true
}

type MarkRef struct {
	Type string
	ID   string
}

// ListMarks returns each distinct Proof or Dispatch mark identity in document
// order.
func ListMarks(doc *Node) []MarkRef {
	seen := make(map[MarkRef]struct{})
	var refs []MarkRef
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if node.Type != "text" {
			return true
		}
		for _, mark := range node.Marks {
			if !strings.HasPrefix(mark.Type, "proof") && mark.Type != "dispatchAsk" {
				continue
			}
			id, ok := mark.Attrs["id"].(string)
			if !ok {
				continue
			}
			ref := MarkRef{Type: mark.Type, ID: id}
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
		return true
	})
	return refs
}

// MarkAttrs returns a copy of the attributes on the first matching mark run.
func MarkAttrs(doc *Node, markType, id string) (Attrs, bool) {
	var attrs Attrs
	found := false
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if node.Type != "text" || nodeMarkID(node, markType) != id {
			return true
		}
		for _, mark := range node.Marks {
			if mark.Type != markType {
				continue
			}
			markID, ok := mark.Attrs["id"].(string)
			if !ok || markID != id {
				continue
			}
			attrs = cloneAttrs(mark.Attrs)
			found = true
			return false
		}
		return true
	})
	return attrs, found
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

// MarkRange adds mark to every selected text run. A mark may cross textblocks
// when the target schema allows it in each textblock.
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
			if !markAllowedInTextblock(mark.Type, span.textblock) {
				return fmt.Errorf("%w: mark %q is not allowed in %s", ErrSchema, mark.Type, span.textblock)
			}
			affected = append(affected, span)
		}
	}
	if len(affected) == 0 {
		return ErrTargetNotFound
	}

	attributes := crdt.Attributes{mark.Type: markAttributeValue(mark)}
	for _, span := range affected {
		from := max(r.From, span.From) - span.From
		to := min(r.To, span.To) - span.From
		span.text.Format(txn, from, to-from, attributes)
	}
	return nil
}

func markAllowedInTextblock(markType, textblock string) bool {
	if textblock != "code_block" {
		return textblock == "paragraph" || textblock == "heading"
	}
	switch markType {
	case "proofAuthored", "proofSuggestion", "proofComment", "proofFlagged", "proofApproved":
		return true
	default:
		return false
	}
}

func markAttributeValue(mark Mark) crdt.Attributes {
	value := make(crdt.Attributes, len(mark.Attrs))
	for key, attr := range mark.Attrs {
		value[key] = attr
	}
	return value
}

type yTextRange struct {
	text      *crdt.YXmlText
	textblock string
	Range
}

type yTextNode struct {
	text      *crdt.YXmlText
	textblock string
}

type textLocation struct {
	pos int
	end int
}

func yTextRanges(frag *crdt.YXmlFragment) ([]yTextRange, error) {
	doc, err := yFragmentShape(frag)
	if err != nil {
		return nil, err
	}
	var locations []textLocation
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type == "text" {
			locations = append(locations, textLocation{pos: pos, end: end})
		}
		return true
	})

	var texts []yTextNode
	if err := collectYTexts(frag, "", &texts); err != nil {
		return nil, err
	}
	ranges := make([]yTextRange, 0, len(texts))
	location := 0
	for _, current := range texts {
		length := current.text.Len()
		if length == 0 {
			continue
		}
		if location == len(locations) {
			return nil, fmt.Errorf("%w: Yjs text has no ProseMirror text node", ErrSchema)
		}
		from := locations[location].pos
		to := from
		for remaining := length; remaining > 0; location++ {
			if location == len(locations) || locations[location].pos != to {
				return nil, fmt.Errorf("%w: Yjs text does not align with ProseMirror text runs", ErrSchema)
			}
			width := locations[location].end - locations[location].pos
			if width > remaining {
				return nil, fmt.Errorf("%w: Yjs text splits a ProseMirror text run", ErrSchema)
			}
			to = locations[location].end
			remaining -= width
		}
		ranges = append(ranges, yTextRange{
			text:      current.text,
			textblock: current.textblock,
			Range:     Range{From: from, To: to},
		})
	}
	if location != len(locations) {
		return nil, fmt.Errorf("%w: ProseMirror text has no Yjs text", ErrSchema)
	}
	return ranges, nil
}

func collectYTexts(frag *crdt.YXmlFragment, textblock string, out *[]yTextNode) error {
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			*out = append(*out, yTextNode{text: current, textblock: textblock})
		case *crdt.YXmlElement:
			next := textblock
			switch current.NodeName {
			case "paragraph", "heading", "code_block":
				next = current.NodeName
			}
			if err := collectYTexts(&current.YXmlFragment, next, out); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return nil
}

func yFragmentShape(frag *crdt.YXmlFragment) (*Node, error) {
	children, err := yFragmentShapeChildren(frag)
	if err != nil {
		return nil, err
	}
	return &Node{Type: "doc", Children: children}, nil
}

func yFragmentShapeChildren(frag *crdt.YXmlFragment) ([]*Node, error) {
	var children []*Node
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			delta, err := yTextDeltaInTransaction(current)
			if err != nil {
				return nil, err
			}
			for _, operation := range delta {
				value, ok := operation.Insert.(string)
				if !ok {
					return nil, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
				}
				children = append(children, &Node{Type: "text", Text: value})
			}
		case *crdt.YXmlElement:
			grandchildren, err := yFragmentShapeChildren(&current.YXmlFragment)
			if err != nil {
				return nil, err
			}
			children = append(children, &Node{Type: current.NodeName, Children: grandchildren})
		default:
			return nil, fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return children, nil
}

// Unmark removes every markType mark whose id attribute equals id.
func Unmark(txn *crdt.Transaction, frag *crdt.YXmlFragment, markType, id string) error {
	if txn == nil || frag == nil {
		return fmt.Errorf("%w: Unmark requires transaction and fragment", ErrSchema)
	}
	if !markTypes[markType] {
		return fmt.Errorf("%w: mark %q", ErrSchema, markType)
	}

	spans, err := yTextRanges(frag)
	if err != nil {
		return err
	}
	found := false
	for _, span := range spans {
		operations, err := yTextDeltaInTransaction(span.text)
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
					span.text.Format(txn, offset, len16(value), crdt.Attributes{attributeName: nil})
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
