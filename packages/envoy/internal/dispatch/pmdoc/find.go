package pmdoc

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// ErrTargetNotFound is the text an agent reads when a quote, heading or block anchor does not
// resolve, so it names no internal package.
var ErrTargetNotFound = errors.New("target not found")
var ErrTargetSpansBlocks = errors.New("target spans textblocks")

// ErrMarkNotAllowed is the refusal of a mark on text that stands where the schema allows no mark of
// its type: an ask's in a code block, or any anchor's in front matter. The refusal is a
// *MarkNotAllowedError, which is ErrSchema too, so a caller that only knows ErrSchema still sees
// it, and one that wrote the text itself can tell it apart from a fault of the document or the
// server.
var ErrMarkNotAllowed = errors.New("mark not allowed on this text")

// MarkNotAllowedError is MarkRange's refusal of Mark on text in Textblock, the textblock holding the
// text: "code_block", or "" for text no paragraph, heading or code block holds.
type MarkNotAllowedError struct {
	Mark      string
	Textblock string
	// At is where the text run holding the refused text begins in the document - its block's whole
	// text, not where the refused range starts. A code block or the front matter holds its text as
	// one run, so two refusals with the same Textblock and At are one place, and two with different
	// At are two places of one kind.
	At int
}

func (e *MarkNotAllowedError) Error() string {
	return fmt.Sprintf("%s: mark %q is not allowed in %s", ErrSchema, e.Mark, e.Place())
}

// Is reports err as ErrMarkNotAllowed and as ErrSchema.
func (e *MarkNotAllowedError) Is(target error) bool {
	return target == ErrMarkNotAllowed || target == ErrSchema
}

// Place names, for a person, where the refused text stands: "a code block", or "the front matter"
// for text no textblock holds, since in a document inside the schema only front matter holds text
// outside a paragraph, heading or code block.
func (e *MarkNotAllowedError) Place() string {
	singular, _ := e.place()
	return singular
}

// PlacePlural names several places of the kind the refused text stands in, for a count to stand
// before: "code blocks". It is empty where a document inside the schema holds one place of the
// kind at most - the front matter - so a caller counting the places a quote landed in names that
// one as Place names it however often the quote landed there.
func (e *MarkNotAllowedError) PlacePlural() string {
	_, plural := e.place()
	return plural
}

// place is where the refused text stands, as a person says it of one such place and of several,
// held as one mapping so the singular and the plural never name different things.
func (e *MarkNotAllowedError) place() (singular, plural string) {
	switch e.Textblock {
	case "":
		return "the front matter", ""
	case "code_block":
		return "a code block", "code blocks"
	}
	return e.Textblock, e.Textblock + "s"
}

// ErrQuoteNotFound reports a quote miss with up to three closest rendered textblocks, in
// document order among equals, and how many places the quote did match — nonzero only when an
// occurrence was out of range. It unwraps ErrTargetNotFound so callers can preserve their
// existing miss handling.
type ErrQuoteNotFound struct {
	Nearest []string
	Matches int
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

// ErrHeadingNotFound reports a `heading:` anchor miss with the anchor text and the document's
// closest headings. Heading anchors match the whole heading text exactly: a prefix of a longer
// heading is a miss, because prefix matching would make every anchor ambiguous.
type ErrHeadingNotFound struct {
	Title   string
	Nearest []string
}

func (e *ErrHeadingNotFound) Error() string {
	if len(e.Nearest) == 0 {
		return fmt.Sprintf("heading anchor %q not found; the document has no headings", e.Title)
	}
	return fmt.Sprintf(
		"heading anchor %q not found; a heading anchor matches the whole heading text exactly; nearest headings: %s",
		e.Title, QuoteBlocks(e.Nearest),
	)
}

func (e *ErrHeadingNotFound) Unwrap() error { return ErrTargetNotFound }

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

// ErrTargetAmbiguous reports a quote that needs an occurrence or position hint. Target is the
// text that matched in several places, so the reader knows which of its quotes to narrow.
type ErrTargetAmbiguous struct {
	Target     string
	Candidates []Candidate
}

func (e *ErrTargetAmbiguous) Error() string {
	return fmt.Sprintf(
		"%q matches %d places; pass a zero-based occurrence, or quote more of the surrounding text",
		e.Target, len(e.Candidates),
	)
}

// FindQuote finds quote in the document text and returns its ProseMirror
// range. occurrence is zero-based. When several exact matches exist, near
// chooses the range whose start is nearest the ProseMirror position hint.
func FindQuote(doc *Node, quote string, occurrence *int, near *int) (Range, error) {
	return NewQuoteFinder(doc).Find(quote, occurrence, near)
}

// QuoteFinder answers FindQuote for many quotes over one document. It validates the document and
// flattens its text once, at its first Find, and every later Find reuses that text and the forms
// derived from it, so a write that resolves many quotes against one tree walks it once rather than
// once a quote. The document must not change while the finder is in use.
type QuoteFinder struct {
	doc      *Node
	prepared bool
	err      error
	text     flattenedText
}

// NewQuoteFinder returns a finder over doc. It reads nothing of doc until its first Find.
func NewQuoteFinder(doc *Node) *QuoteFinder {
	return &QuoteFinder{doc: doc}
}

// Find is FindQuote(doc, quote, occurrence, near): the same range or the same error.
func (f *QuoteFinder) Find(quote string, occurrence, near *int) (Range, error) {
	matches, err := f.matches(quote)
	if err != nil {
		return Range{}, err
	}
	doc, text := f.doc, f.text
	if len(matches) == 0 {
		return Range{}, quoteNotFound(doc, quote)
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			miss := quoteNotFound(doc, quote)
			var missing *ErrQuoteNotFound
			if errors.As(miss, &missing) {
				missing.Matches = len(matches)
			}
			return Range{}, miss
		}
		return matches[*occurrence].Range, nil
	}
	if len(matches) == 1 {
		return matches[0].Range, nil
	}
	if near != nil {
		// MinFunc returns the first least match, which nearerFirst's earlier-of-two-as-near tie
		// makes the first NearestFirst lists.
		return slices.MinFunc(matches, nearerFirst(*near)).Range, nil
	}

	candidates := make([]Candidate, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, Candidate{
			Range:   match.Range,
			Context: context16(text.value, max(0, match.textFrom-40), min(len16(text.value), match.textTo+40)),
		})
	}
	return Range{}, &ErrTargetAmbiguous{Target: quote, Candidates: candidates}
}

// NearestFirst is every range quote matches, as Find matches it, ordered as Find with near chooses
// among them: the nearest to near first, and of two as near, the earlier. Find with near returns
// its first. A quote that matches nothing is ErrTargetNotFound itself, without ranking the
// document's blocks for the nearest ones the way Find's miss does, which costs another walk of the
// document: a caller that drops the miss pays nothing to describe it. A caller whose first choice
// cannot carry what it writes there takes the next.
func (f *QuoteFinder) NearestFirst(quote string, near int) ([]Range, error) {
	matches, err := f.matches(quote)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, ErrTargetNotFound
	}
	slices.SortFunc(matches, nearerFirst(near))
	ranges := make([]Range, len(matches))
	for index, match := range matches {
		ranges[index] = match.Range
	}
	return ranges, nil
}

// nearerFirst orders two matches of one quote as Find with near chooses between them: the nearer to
// near first, and of two as near, the earlier. NearestFirst sorts by it and Find takes its least,
// so the range Find chooses and the first NearestFirst lists are one rule, held in one place.
func nearerFirst(near int) func(left, right quoteMatch) int {
	return func(left, right quoteMatch) int {
		if byDistance := distance(left.From, near) - distance(right.From, near); byDistance != 0 {
			return byDistance
		}
		return left.From - right.From
	}
}

// matches is every match of quote in the finder's document, preparing the document at the first
// call: a heading quote's headings, otherwise the plain matcher cascade's.
func (f *QuoteFinder) matches(quote string) ([]quoteMatch, error) {
	if f.doc == nil {
		return nil, fmt.Errorf("%w: FindQuote wants a document", ErrSchema)
	}
	if !f.prepared {
		f.prepared = true
		if f.err = f.doc.Validate(); f.err == nil {
			f.text = buildFlattenedText(f.doc)
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if marker := atxHeadingMarker.FindString(quote); marker != "" && marker != quote {
		return headingQuoteMatches(f.doc, f.text, quote[len(marker):]), nil
	}
	return quoteMatches(f.text, quote), nil
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

// FindHeading finds the heading whose text, as a read serves it (TextContent), equals title
// exactly. occurrence is zero-based; without it, repeated titles return ErrTargetAmbiguous.
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
			headingText := TextContent(child)
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
		return Range{}, headingNotFound(doc, title)
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			return Range{}, headingNotFound(doc, title)
		}
		return matches[*occurrence].Range, nil
	}
	if len(matches) == 1 {
		return matches[0].Range, nil
	}
	return Range{}, &ErrTargetAmbiguous{Target: title, Candidates: matches}
}

// headingNotFound ranks the document's headings by their common prefix with title, the way a
// quote miss ranks blocks: the anchor that missed is usually a prefix of the one meant.
func headingNotFound(doc *Node, title string) error {
	type ranked struct {
		text   string
		prefix int
	}
	var candidates []ranked
	for _, child := range doc.Children {
		if child.Type != "heading" {
			continue
		}
		heading := TextContent(child)
		candidates = append(candidates, ranked{text: heading, prefix: commonPrefixLength(title, heading)})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].prefix > candidates[right].prefix
	})
	nearest := make([]string, 0, nearestBlockCount)
	for _, candidate := range candidates[:min(nearestBlockCount, len(candidates))] {
		nearest = append(nearest, firstRunes(candidate.text, 120))
	}
	return &ErrHeadingNotFound{Title: title, Nearest: nearest}
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

// flattenedText is a document's inline text in one string, with each UTF-16 unit's
// ProseMirror position and inline markup. forms caches the whole-text derived views the
// quote matchers read, so one FindQuote computes each at most once however far it falls
// through the exact -> normalized -> markdown chain, and a QuoteFinder at most once for all
// the quotes it answers.
type flattenedText struct {
	value     string
	positions []int
	marks     []inlineMarkup
	forms     *textForms
}

type textForms struct {
	units          []uint16
	unitsDone      bool
	normalized     []rune
	spans          []range16
	normalizedDone bool
}

// units16 is value encoded as UTF-16 units, computed on first use.
func (t flattenedText) units16() []uint16 {
	if !t.forms.unitsDone {
		t.forms.units = utf16.Encode([]rune(t.value))
		t.forms.unitsDone = true
	}
	return t.forms.units
}

// normalizedForm is normalizedRanges(value), computed on first use. The matchers compare runes,
// so the normalized text is kept as runes: a finder answering many quotes converts it once.
func (t flattenedText) normalizedForm() ([]rune, []range16) {
	if !t.forms.normalizedDone {
		t.forms.normalized, t.forms.spans = normalizedRanges(t.value)
		t.forms.normalizedDone = true
	}
	return t.forms.normalized, t.forms.spans
}

// buildFlattenedText reads doc's inline text into one string with the index the matchers read it
// through. The index is allocated once, at the document's own size, which bounds it: every unit of
// text takes one entry and so does every gap between two runs, and each of those gaps stands where
// the document holds a position no text does. Growing it instead copied the whole index at every
// doubling, which on a document whose paragraph is a mebibyte long was most of what a cold
// one-word edit allocated (sjawhar/legion#2519).
func buildFlattenedText(doc *Node) flattenedText {
	var out strings.Builder
	var positions []int
	var marks []inlineMarkup
	lastEnd := -1
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if positions == nil {
			// The walk visits the document first, so end is its size.
			out.Grow(end)
			positions = make([]int, 0, end)
			marks = make([]inlineMarkup, 0, end)
		}
		if node.Type != "text" {
			return true
		}
		if lastEnd >= 0 && lastEnd != pos {
			out.WriteByte(' ')
			positions = append(positions, lastEnd)
			marks = append(marks, 0)
		}
		markup := nodeInlineMarkup(node)
		// A quote is matched against the text every read serves, so a browser's U+0000 is U+FFFD
		// here too; both are one UTF-16 unit, so no position moves.
		for _, char := range nulAsReplacement(node.Text) {
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
	return flattenedText{value: out.String(), positions: positions, marks: marks, forms: &textForms{}}
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
	for _, offset := range findAllUnits(text.units16(), needle) {
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
	normalizedText, textSpans := text.normalizedForm()
	needle, quoteSpans := normalizedRanges(quote)
	if len(needle) == 0 || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	var matches []quoteMatch
	for _, offset := range findAllRunes(normalizedText, needle) {
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

// renderedMarkdownQuote is quote read as markdown, when its text and inline markup differ from
// quote's own. A quote matches by its text, which a short table row's padded cells do not hold, so
// its tables are read only while they need no padding (quoteBudget), and a quote each
// operation of an edit batch resolves costs no padded cell. A quote that makes more elements than
// a write may is refused before goldmark builds the rest of it, and reads as no markdown.
func renderedMarkdownQuote(quote string) (flattenedText, bool) {
	parsed, err := parseStamped(quote, quoteBudget())
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
	normalizedText, textSpans := text.normalizedForm()
	needle, quoteSpans := quote.normalizedForm()
	if len(needle) == 0 || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	var matches []quoteMatch
	for _, offset := range findAllRunes(normalizedText, needle) {
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
			candidate := TextContent(node)
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

// TextContent is node's text, hard breaks as line feeds, as a read serves it: each U+0000 a browser
// edit left as U+FFFD (nulAsReplacement). An ask row's question and options, a heading anchor and
// a miss's nearest blocks and headings are read through it.
func TextContent(node *Node) string {
	if node.Type == "text" {
		return nulAsReplacement(node.Text)
	}
	if node.Type == "hardbreak" {
		return "\n"
	}
	var out strings.Builder
	for _, child := range node.Children {
		out.WriteString(TextContent(child))
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

// normalizedRanges is value with each whitespace run read as one space and leading and trailing
// whitespace dropped, as runes, with the UTF-16 span of value each rune stands for.
func normalizedRanges(value string) ([]rune, []range16) {
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
	return out, spans
}

func distance(left, right int) int {
	if left < right {
		return right - left
	}
	return left - right
}

// MarkSpans is the text a mark identity covers as ranges, in document order: its text runs, each
// joined to the next when no text without the mark lies between them, so a run at a textblock's
// end and one at the next textblock's start are one span across the boundary between them, as the
// browser editor takes them, while text without the mark ends a span.
func MarkSpans(doc *Node, markType, id string) []Range {
	var spans []Range
	between := false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "text" {
			return true
		}
		if !nodeHasMarkID(node, markType, id) {
			between = true
			return true
		}
		if last := len(spans) - 1; last >= 0 && !between {
			spans[last].To = end
		} else {
			spans = append(spans, Range{From: pos, To: end})
		}
		between = false
		return true
	})
	return spans
}

// FindMark finds the first document-contiguous range covered by a mark identity, the first of
// its MarkSpans, and its text, the runs joined with a space where a block boundary lies between,
// each U+0000 written as U+FFFD (nulAsReplacement): the text is what an anchor row stores as its
// quote. It is FindMarks asked for the one identity, so one walk holds the rule for both.
func FindMark(doc *Node, markType, id string) (Range, string, bool) {
	key := MarkRef{Type: markType, ID: id}
	f, ok := FindMarks(doc, []MarkRef{key})[key]
	return f.Range, f.Quote, ok
}

// MarkFound is what FindMark and MarkAttrs read for one mark identity. Attrs is the attributes
// map of the identity's first mark in the tree itself, not a copy: callers read it and must not
// modify it.
type MarkFound struct {
	Range Range
	Quote string
	Attrs Attrs
}

// FindMarks is FindMark and MarkAttrs for many mark identities in one walk: each key it finds maps
// to the range and quote FindMark returns and the attributes MarkAttrs reads, uncopied, and a key it
// does not find is absent. A write refreshes every open anchor's range and quote, and a walk per
// anchor would make that refresh quadratic in a document carrying many of them; here a run costs
// only its own marks and the identities open at it.
func FindMarks(doc *Node, keys []MarkRef) map[MarkRef]MarkFound {
	if len(keys) == 0 {
		return map[MarkRef]MarkFound{}
	}
	wanted := make(map[MarkRef]*markSearch, len(keys))
	for _, key := range keys {
		if _, ok := wanted[key]; !ok {
			wanted[key] = &markSearch{last: -1}
		}
	}
	unfinished := len(wanted)
	var open []*markSearch
	run := 0
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		// Non-text nodes neither carry a mark nor end a span (MarkSpans).
		if node.Type != "text" {
			return true
		}
		run++
		for _, mark := range node.Marks {
			id, ok := mark.Attrs["id"].(string)
			if !ok {
				continue
			}
			search := wanted[MarkRef{Type: mark.Type, ID: id}]
			// A run carrying one identity twice counts once, and MarkAttrs reads the first of them.
			if search == nil || search.run == run || search.state == markClosed {
				continue
			}
			search.run = run
			if search.state == markUnseen {
				search.state = markOpen
				search.found = MarkFound{Range: Range{From: pos, To: pos}, Attrs: mark.Attrs}
				open = append(open, search)
			}
			search.add(node.Text, pos, end)
		}
		// A text run without an open identity ends its first span, and only the first counts.
		kept := open[:0]
		for _, search := range open {
			if search.run == run {
				kept = append(kept, search)
				continue
			}
			search.close()
			unfinished--
		}
		clear(open[len(kept):])
		open = kept
		return unfinished > 0
	})
	for _, search := range open {
		search.close()
	}
	found := make(map[MarkRef]MarkFound, len(wanted))
	for key, search := range wanted {
		if search.state != markUnseen {
			found[key] = search.found
		}
	}
	return found
}

type markSearchState uint8

const (
	markUnseen markSearchState = iota
	markOpen
	markClosed
)

// markSearch is one identity's progress through FindMarks' walk: unseen, then open over its first
// span, then closed for good.
type markSearch struct {
	state markSearchState
	found MarkFound
	// run is the text run that last carried the identity, so a run's sweep tells the identities it
	// carries from the open ones it ends.
	run   int
	quote strings.Builder
	// last is the end of the last run written to quote, or -1 before the first: a run starting
	// anywhere else is parted from it by a space.
	last int
	// pending holds the positions of empty runs in the span that are not yet written. The quote
	// takes a run only when its position lies before the span's end, so an empty run belongs to the
	// quote, writing a separator where it lies apart from the run before it, only once a later run
	// carries the span past it.
	pending []int
}

// add extends the open span over the run at pos..end and writes to the quote what the quote takes
// for it and for the empty runs that the span now passes.
func (s *markSearch) add(text string, pos, end int) {
	s.found.Range.To = end
	flushed := 0
	for flushed < len(s.pending) && s.pending[flushed] < end {
		s.write("", s.pending[flushed], s.pending[flushed])
		flushed++
	}
	s.pending = slices.Delete(s.pending, 0, flushed)
	if pos == end {
		s.pending = append(s.pending, pos)
		return
	}
	s.write(text, pos, end)
}

func (s *markSearch) write(text string, pos, end int) {
	if s.last >= 0 && pos != s.last {
		s.quote.WriteByte(' ')
	}
	s.quote.WriteString(text)
	s.last = end
}

// close ends the span: empty runs still pending lie at or past its end, which the quote never
// reaches.
func (s *markSearch) close() {
	s.state = markClosed
	s.found.Quote = nulAsReplacement(s.quote.String())
	s.quote = strings.Builder{}
	s.pending = nil
}

// anchorMarkTypes are the marks an ask or comment row anchors to: its quote is the text they
// cover (FindMark). They are exactly the marks whose schema declares an empty excludes (the fork's
// proofComment and proofSuggestion, packages/proof-editor's dispatchAsk), so two of one type may
// cover one character and each is stored under its own Y attribute key (markAttributeKey). The
// set moves with those schemas.
var anchorMarkTypes = map[string]bool{"proofComment": true, "proofSuggestion": true, "dispatchAsk": true}

// AnchorMarksCovering returns each anchor mark that every text run inside r carries, which is
// every anchor the range lies wholly inside. Text written over r belongs inside those anchors: a
// replacement spliced in without them ends each one's run where it lands, and FindMark then reads
// only the text before it. A range that runs past an anchor's edge has a run without it, so that
// anchor keeps only the text the write leaves alone.
func AnchorMarksCovering(doc *Node, r Range) []Mark {
	var covering []Mark
	first := true
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if pos >= r.To {
			return false
		}
		if node.Type != "text" || end <= r.From {
			return true
		}
		if first {
			first = false
			for _, mark := range node.Marks {
				if anchorMarkTypes[mark.Type] {
					covering = append(covering, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
				}
			}
			return len(covering) > 0
		}
		kept := covering[:0]
		for _, mark := range covering {
			if carriesMark(node, mark) {
				kept = append(kept, mark)
			}
		}
		covering = kept
		return len(covering) > 0
	})
	return covering
}

func carriesMark(node *Node, want Mark) bool {
	for _, mark := range node.Marks {
		if mark.Type == want.Type && attrsEqual(mark.Attrs, want.Attrs) {
			return true
		}
	}
	return false
}

// AddMarks adds marks to every text node under node, in place.
func AddMarks(node *Node, marks []Mark) {
	if len(marks) == 0 {
		return
	}
	if node.Type == "text" {
		for _, mark := range marks {
			node.Marks = append(node.Marks, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
		}
		sortMarks(node.Marks)
		return
	}
	for _, child := range node.Children {
		AddMarks(child, marks)
	}
}

type MarkRef struct {
	Type string
	ID   string
}

// Marked is a mark identity in the tree with the `by` attribute its first run carries, empty
// when it has none.
type Marked struct {
	MarkRef
	By string
}

// ListMarks returns each distinct Proof or Dispatch mark identity in document
// order, with the author its first run names. doc must be a tree Validate accepts: the walk
// descends into every node's children and computes no positions.
func ListMarks(doc *Node) []Marked {
	seen := make(map[MarkRef]struct{})
	var marked []Marked
	Walk(doc, func(node *Node) bool {
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
			by, _ := mark.Attrs["by"].(string)
			marked = append(marked, Marked{MarkRef: ref, By: by})
		}
		return true
	})
	return marked
}

// MarkAttrs returns a copy of the attributes on the first matching mark run: FindMarks' Attrs,
// copied so the caller may change them without changing the tree.
func MarkAttrs(doc *Node, markType, id string) (Attrs, bool) {
	key := MarkRef{Type: markType, ID: id}
	f, ok := FindMarks(doc, []MarkRef{key})[key]
	return cloneAttrs(f.Attrs), ok
}

// nodeHasMarkID reports whether node carries a markType mark whose id is id. A record mark's type
// does not exclude itself (anchorMarkTypes), so a run may carry two of one type, and the mark asked
// for need not be the first of them.
func nodeHasMarkID(node *Node, markType, id string) bool {
	for _, mark := range node.Marks {
		if markHasID(mark, markType, id) {
			return true
		}
	}
	return false
}

func markHasID(mark Mark, markType, id string) bool {
	if mark.Type != markType {
		return false
	}
	markID, ok := mark.Attrs["id"].(string)
	return ok && markID == id
}

// MarkRange adds mark to every selected text run. A mark may cross textblocks
// when the target schema allows it in each textblock.
func MarkRange(txn *crdt.Transaction, frag *crdt.YXmlFragment, r Range, mark Mark) error {
	return NewMarkWriter(frag).Mark(txn, r, mark)
}

// MarkWriter is MarkRange for many marks written to one fragment. It reads the fragment's text
// ranges once, at the first mark it writes, and reuses them for every later one: writing a mark
// formats Yjs texts, which changes neither a text's length nor the elements holding it, so the
// ranges stay those of the fragment. Nothing but its own marks may change the fragment while the
// writer is in use.
type MarkWriter struct {
	frag     *crdt.YXmlFragment
	prepared bool
	spans    []yTextRange
	err      error
}

// NewMarkWriter returns a writer over frag. It reads nothing of frag until its first mark.
func NewMarkWriter(frag *crdt.YXmlFragment) *MarkWriter {
	return &MarkWriter{frag: frag}
}

// Mark is MarkRange(txn, frag, r, mark): the same checks in the same order, the same Format calls
// and the same error. A fragment whose text ranges are refused refuses every mark.
func (w *MarkWriter) Mark(txn *crdt.Transaction, r Range, mark Mark) error {
	if txn == nil || w.frag == nil || r.From >= r.To {
		return fmt.Errorf("%w: invalid mark range", ErrSchema)
	}
	if !markTypes[mark.Type] {
		return fmt.Errorf("%w: mark %q", ErrSchema, mark.Type)
	}
	if err := attrNestingError("mark", mark.Type, mark.Attrs, maxMarkAttrNesting); err != nil {
		return err
	}

	if !w.prepared {
		w.prepared = true
		w.spans, w.err = yTextRanges(w.frag)
	}
	if w.err != nil {
		return w.err
	}
	// The ranges are in document order and disjoint, so the texts r overlaps are one run of them,
	// starting at the first that ends past r.From.
	spans := w.spans
	first := sort.Search(len(spans), func(index int) bool { return spans[index].To > r.From })
	last := first
	for ; last < len(spans) && spans[last].From < r.To; last++ {
		if !markAllowedInTextblock(mark.Type, spans[last].textblock) {
			return &MarkNotAllowedError{Mark: mark.Type, Textblock: spans[last].textblock, At: spans[last].From}
		}
	}
	if first == last {
		return ErrTargetNotFound
	}

	attributes := crdt.Attributes{markAttributeKey(mark): markAttributeValue(mark)}
	for _, span := range spans[first:last] {
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

// yTextRanges is the ProseMirror range of each non-empty Yjs text in frag, with the textblock
// holding it, in document order. MarkRange and Unmark call it on every anchored write, so it reads
// the fragment once and finds each text run's position as it goes.
func yTextRanges(frag *crdt.YXmlFragment) ([]yTextRange, error) {
	var texts []yTextNode
	var locations []textLocation
	if _, err := yFragmentLocations(frag, "", 1, 0, &texts, &locations); err != nil {
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

// yFragmentLocations appends each Yjs text among frag's children, which stand depth levels below
// the document, to texts with the textblock holding it. When locations is not nil, it also appends
// the range of each text node walk would visit among them in the Node shape the fragment reads as,
// with each text's delta operations as text nodes and each element as a node of its name and no
// text, where the children start at ProseMirror position pos, and returns their size as nodeSize
// counts it. It sizes and enters elements by walk's own rules (isOpaqueNodeType, opaqueNodeSize,
// boundaryTokens): an element named text is a text run of no width, and no Yjs text inside it or
// a leaf-named element is located. It refuses a node past MaxTreeDepth as Read does: an element,
// or a text holding anything, which Read makes a text node of. Its refusals, met in document order
// during the one traversal, precede any refusal from aligning texts with locations.
func yFragmentLocations(frag *crdt.YXmlFragment, textblock string, depth, pos int, texts *[]yTextNode, locations *[]textLocation) (int, error) {
	start := pos
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			if current.Len() > 0 {
				if err := treeDepthError(depth); err != nil {
					return 0, err
				}
			}
			*texts = append(*texts, yTextNode{text: current, textblock: textblock})
			delta, err := yTextDeltaInTransaction(current)
			if err != nil {
				return 0, err
			}
			for _, operation := range delta {
				value, ok := operation.Insert.(string)
				if !ok {
					return 0, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
				}
				// Outside located children no position is read, so their widths go uncounted.
				if locations != nil {
					width := len16(value)
					*locations = append(*locations, textLocation{pos: pos, end: pos + width})
					pos += width
				}
			}
		case *crdt.YXmlElement:
			if err := treeDepthError(depth); err != nil {
				return 0, err
			}
			name := current.NodeName
			innerBlock := textblock
			switch name {
			case "paragraph", "heading", "code_block":
				innerBlock = name
			}
			opaque := isOpaqueNodeType(name)
			// walk visits an element named text as a text node holding no text, so it locates one of
			// no width, and enters it no more than a leaf.
			if name == "text" && locations != nil {
				*locations = append(*locations, textLocation{pos: pos, end: pos})
			}
			innerLocations := locations
			if opaque {
				innerLocations = nil
			}
			size, err := yFragmentLocations(&current.YXmlFragment, innerBlock, depth+1, pos+boundaryTokens(name)/2, texts, innerLocations)
			if err != nil {
				return 0, err
			}
			if opaque {
				pos += opaqueNodeSize(name, "")
			} else {
				pos += size + boundaryTokens(name)
			}
		default:
			return 0, fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return pos - start, nil
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

// StripAnchorMarks returns a copy of doc with every mark the rendering does not write
// (renderedMarkTypes) removed - Proof and Dispatch anchors - coalescing adjacent equal-mark text
// runs.
func StripAnchorMarks(node *Node) *Node {
	if node == nil {
		return nil
	}
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: node.Text}
	for _, mark := range node.Marks {
		if renderedMarkTypes[mark.Type] {
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
