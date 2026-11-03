package pmdoc

// frozenFindAllUnits, frozenFindAllRunes, frozenImageAlt, frozenUpdateYText and frozenContext16
// are findAllUnits, findAllRunes, imageAlt, updateYText and context16 as the package held them
// before the work that makes each one cheaper: copied verbatim but for their names, and kept in
// the tree beside the live functions, as find_marks_bench_test.go keeps frozenFindMark beside
// FindMarks. They are what the fuzz targets below hold each live function to, so that a faster
// implementation is held to the result the base gave, and what the benchmarks below measure
// beside it, so that benchstat of the two commits compares like with like.
//
// Only those five functions are frozen. The helpers they call - altReadsBack, textRangeMarks,
// simpleDiff, yTextDeltaInTransaction, formatAttributes, len16 - are the package's own, and each
// frozen copy calls them directly. frozenImageAlt calls altReadsBack rather than the
// altReadsBackFn seam, so a test that swaps the seam measures the live function alone.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

func frozenFindAllUnits(haystack, needle []uint16) []int {
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

func frozenFindAllRunes(haystack, needle []rune) []int {
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

func frozenImageAlt(alt string, context inlineContext) string {
	written := escapeTablePipes(strings.ReplaceAll(alt, "]", "\\]"), context.tableCell)
	writtenDepth := altReadsBack(written, alt, context)
	if writtenDepth == 2 {
		return written
	}
	var out strings.Builder
	for index := 0; index < len(alt); index++ {
		switch {
		case alt[index] == '\n':
			out.WriteString("&#10;")
		case isASCIIPunctuation(alt[index]) && (alt[index] != '|' || !context.tableCell):
			out.WriteString(escaped(rune(alt[index])))
		default:
			// A pipe in a table cell is escaped with the others below.
			out.WriteByte(alt[index])
		}
	}
	fallback := escapeTablePipes(out.String(), context.tableCell)
	if altReadsBack(fallback, alt, context) > writtenDepth {
		return fallback
	}
	return written
}

func frozenUpdateYText(txn *crdt.Transaction, text *crdt.YXmlText, want []*Node) error {
	operations, err := yTextDeltaInTransaction(text)
	if err != nil {
		return err
	}
	var current string
	for _, operation := range operations {
		value, ok := operation.Insert.(string)
		if !ok {
			return fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
		}
		current += value
	}

	var desired string
	for _, node := range want {
		if node.Type != "text" {
			return fmt.Errorf("%w: Yjs text wants %q", ErrSchema, node.Type)
		}
		desired += node.Text
	}
	index, remove, insert := simpleDiff(current, desired)
	if remove > 0 {
		text.Delete(txn, index, remove)
	}
	if insert != "" {
		text.Insert(txn, index, insert, nil)
	}

	actual, err := yTextDeltaInTransaction(text)
	if err != nil {
		return err
	}
	offset := 0
	for _, node := range want {
		length := len16(node.Text)
		attributes, matches, err := textRangeMarks(actual, offset, length, node.Marks)
		if err != nil {
			return err
		}
		if !matches {
			text.Format(txn, offset, length, formatAttributes(attributes, node.Marks))
		}
		offset += length
	}
	return nil
}

func frozenContext16(value string, from, to int) string {
	units := utf16.Encode([]rune(value))
	if splitsPair(units, from) {
		from--
	}
	if splitsPair(units, to) {
		to++
	}
	return string(utf16.Decode(units[from:to]))
}

// almostMatchingCorpus is a mebibyte of haystack and the kibibyte of needle that almost matches it
// everywhere: the haystack is runs of 1,023 `a` each ended by one `b`, and the needle is 1,024
// `a`, so at nearly every start position the needle agrees with the haystack for hundreds of
// positions before it differs - the worst case for a scan that compares the whole needle at every
// start - and it matches nowhere, since no run of `a` is long enough.
func almostMatchingCorpus() (haystack, needle string) {
	const needleLength = 1 << 10
	block := strings.Repeat("a", needleLength-1) + "b"
	return strings.Repeat(block, (1<<20)/len(block)), strings.Repeat("a", needleLength)
}

// BenchmarkFindAllUnits measures the UTF-16 scan on the corpus a quote almost matches everywhere,
// beside the base implementation (frozenFindAllUnits).
func BenchmarkFindAllUnits(b *testing.B) {
	haystack, needle := almostMatchingCorpus()
	units, pattern := utf16.Encode([]rune(haystack)), utf16.Encode([]rune(needle))
	b.Run("live", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if matches := findAllUnits(units, pattern); len(matches) != 0 {
				b.Fatalf("found %d matches, want none", len(matches))
			}
		}
	})
	b.Run("frozen", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if matches := frozenFindAllUnits(units, pattern); len(matches) != 0 {
				b.Fatalf("found %d matches, want none", len(matches))
			}
		}
	})
}

// BenchmarkFindAllRunes measures the rune scan on the same corpus BenchmarkFindAllUnits reads,
// beside the base implementation (frozenFindAllRunes).
func BenchmarkFindAllRunes(b *testing.B) {
	haystack, needle := almostMatchingCorpus()
	text, pattern := []rune(haystack), []rune(needle)
	b.Run("live", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if matches := findAllRunes(text, pattern); len(matches) != 0 {
				b.Fatalf("found %d matches, want none", len(matches))
			}
		}
	})
	b.Run("frozen", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if matches := frozenFindAllRunes(text, pattern); len(matches) != 0 {
				b.Fatalf("found %d matches, want none", len(matches))
			}
		}
	})
}

// imageAltBenchmarkAlts are the two shapes an alt text comes in: one holding no ASCII punctuation,
// no line feed and no carriage return, which is written as it stands, and one holding ASCII
// punctuation, which the writer must try escaped.
var imageAltBenchmarkAlts = []struct{ name, alt string }{
	{"no punctuation", "a plain alt text with no punctuation at all in it"},
	{"punctuation", "an alt [with] brackets, *stars*, `ticks` and a <tag> in it"},
}

// BenchmarkImageAlt measures writing an image's alt text in a paragraph, beside the base
// implementation (frozenImageAlt).
func BenchmarkImageAlt(b *testing.B) {
	context := inlineContext{startsLine: true}
	for _, test := range imageAltBenchmarkAlts {
		b.Run(test.name, func(b *testing.B) {
			b.Run("live", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if written := imageAlt(test.alt, context); written == "" {
						b.Fatal("the alt was written as nothing")
					}
				}
			})
			b.Run("frozen", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if written := frozenImageAlt(test.alt, context); written == "" {
						b.Fatal("the alt was written as nothing")
					}
				}
			})
		})
	}
}

// markedRuns is count one-character text runs, each carrying a comment mark whose id is its own:
// a Y text of them holds one delta operation per run, and a write of the same runs under another
// prefix needs a format on every one of them.
func markedRuns(prefix string, count int) []*Node {
	runs := make([]*Node, count)
	for index := range runs {
		runs[index] = &Node{Type: "text", Text: "x", Marks: []Mark{
			{Type: "proofComment", Attrs: Attrs{"id": fmt.Sprintf("%s%d", prefix, index), "by": "user:alice"}},
		}}
	}
	return runs
}

// yTextOfRuns is a Yjs document holding runs as one paragraph's text, built as update_test.go
// builds one: Update writes the tree into a fresh document, whose paragraph's Y text is the one a
// benchmark or a fuzz target then updates.
func yTextOfRuns(runs []*Node) (*crdt.Doc, *crdt.YXmlText, error) {
	doc := crdt.New(crdt.WithClientID(1))
	frag := doc.GetXmlFragment("prosemirror")
	var err error
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: runs}}})
	})
	if err != nil {
		return nil, nil, err
	}
	children := frag.Children()
	if len(children) != 1 {
		return nil, nil, fmt.Errorf("the document holds %d blocks, want one paragraph", len(children))
	}
	paragraph, ok := children[0].(*crdt.YXmlElement)
	if !ok {
		return nil, nil, fmt.Errorf("the document's block is %T, want a paragraph", children[0])
	}
	blockChildren := paragraph.Children()
	if len(blockChildren) != 1 {
		return nil, nil, fmt.Errorf("the paragraph holds %d children, want one text", len(blockChildren))
	}
	text, ok := blockChildren[0].(*crdt.YXmlText)
	if !ok {
		return nil, nil, fmt.Errorf("the paragraph's child is %T, want a text", blockChildren[0])
	}
	return doc, text, nil
}

// BenchmarkUpdateYText measures writing marks onto a Y text at 10,000 and at 20,000 runs where
// every run needs a format, beside the base implementation (frozenUpdateYText). The write
// alternates between two sets of comment ids, so that every iteration finds the marks of every run
// differing from the ones the Y text holds and formats all of them; the text itself never changes,
// so what is measured is the mark comparison and the formats, not a diff.
func BenchmarkUpdateYText(b *testing.B) {
	for _, runs := range []int{10_000, 20_000} {
		for _, variant := range []struct {
			name   string
			update func(*crdt.Transaction, *crdt.YXmlText, []*Node) error
		}{
			{name: "live", update: updateYText},
			{name: "frozen", update: frozenUpdateYText},
		} {
			b.Run(fmt.Sprintf("%d/%s", runs, variant.name), func(b *testing.B) {
				doc, text, err := yTextOfRuns(markedRuns("seed", runs))
				if err != nil {
					b.Fatalf("build a Y text of %d runs: %v", runs, err)
				}
				wants := [2][]*Node{markedRuns("a", runs), markedRuns("b", runs)}
				turn := 0
				b.ReportAllocs()
				for b.Loop() {
					var err error
					doc.Transact(func(txn *crdt.Transaction) { err = variant.update(txn, text, wants[turn%2]) })
					if err != nil {
						b.Fatalf("update %d runs: %v", runs, err)
					}
					turn++
				}
			})
		}
	}
}

// context16Corpus is a mebibyte of prose with accented and astral characters in it, the text a
// quote's context is cut from.
func context16Corpus() string {
	const sentence = "The reader writes a comment between blocks: naïve café 𝔸lpha 😀 quote. "
	return strings.Repeat(sentence, (1<<20)/len(sentence)+1)
}

// BenchmarkContext16 measures cutting one quote's context - the 80-unit window find.go asks for -
// out of the middle of a mebibyte of text, beside the base implementation (frozenContext16).
func BenchmarkContext16(b *testing.B) {
	value := context16Corpus()
	const window = 80
	middle := len16(value) / 2
	from, to := middle-window/2, middle+window/2
	b.Run("live", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if cut := context16(value, from, to); len(cut) < window {
				b.Fatalf("the cut is %d bytes, want at least the %d units asked for", len(cut), window)
			}
		}
	})
	b.Run("frozen", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if cut := frozenContext16(value, from, to); len(cut) < window {
				b.Fatalf("the cut is %d bytes, want at least the %d units asked for", len(cut), window)
			}
		}
	})
}

// findAllSeeds are haystack and needle pairs the scans must answer alike: the quotes and texts
// find_test.go's own tables hold, a needle longer than its haystack, an empty one, a text that is
// all one character, and astral characters, which are one rune and two UTF-16 units.
var findAllSeeds = []struct{ haystack, needle string }{
	{"Use `config` with care.", "Use `config`"},
	{"A **bold** statement.", "**bold**"},
	{"A __quiet__ statement.", "__quiet__"},
	{"Read [the guide](https://example.com).", "Read [the guide](https://example.com)"},
	{"See <https://example.com>.", "See <https://example.com>"},
	{"snakecase", "snake_case"},
	{"https://x/ab", "https://x/a_b"},
	{"- one\n- two\n", "one"},
	{"| one |\n| :--- |\n| two |\n", "one"},
	{"aaaaaaaa", "aaa"},
	{"aaab aaab aaab", "aaab"},
	{"short", "a needle longer than its haystack"},
	{"", ""},
	{"a", ""},
	{"", "a"},
	{"naïve café 𝔸lpha 😀 naïve", "naïve"},
	{"😀😀😀", "😀😀"},
	{"a\xffb\xffc", "\xffb"},
}

// findAllUnits answers what the base implementation answered, whatever text and quote it is given.
func FuzzFindAllUnitsMatchesFrozen(f *testing.F) {
	for _, seed := range findAllSeeds {
		f.Add(seed.haystack, seed.needle)
	}
	f.Fuzz(func(t *testing.T, haystack, needle string) {
		if len(haystack) > 16<<10 || len(needle) > 16<<10 {
			return
		}
		text, pattern := utf16.Encode([]rune(haystack)), utf16.Encode([]rune(needle))
		got, want := findAllUnits(text, pattern), frozenFindAllUnits(text, pattern)
		if !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("findAllUnits(%q, %q) = %v, frozen %v", haystack, needle, got, want)
		}
	})
}

// findAllRunes answers what the base implementation answered, whatever text and quote it is given.
func FuzzFindAllRunesMatchesFrozen(f *testing.F) {
	for _, seed := range findAllSeeds {
		f.Add(seed.haystack, seed.needle)
	}
	f.Fuzz(func(t *testing.T, haystack, needle string) {
		if len(haystack) > 16<<10 || len(needle) > 16<<10 {
			return
		}
		text, pattern := []rune(haystack), []rune(needle)
		got, want := findAllRunes(text, pattern), frozenFindAllRunes(text, pattern)
		if !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("findAllRunes(%q, %q) = %v, frozen %v", haystack, needle, got, want)
		}
	})
}

// imageAltSeeds are alt texts the writer must write alike: the ones escape_test.go's
// TestRenderKeepsImageAltText holds, a cell's pipe from render_table_test.go, alts that hold no
// punctuation at all, and line endings, which end a heading and a table row.
var imageAltSeeds = []string{
	"a[b", "[a]", "a\\", "a\\[b", "*a*", "`a`", "a&amp;b", "<b>", "a[b](u)",
	"a\n- b", "a\nb", "a\r\nb", "one|two", "one\\|two",
	"", "a plain alt", "plain words and spaces", "naïve café 𝔸lpha 😀",
	"![nested](u)", "a ~b~~ c", "a <span data-proof=\"comment\">b</span> c", "#", "a # b",
}

// imageAlt writes what the base implementation wrote, whatever alt text and wherever it is
// written: in a paragraph, in a heading or in a table cell, the three blocks it branches on.
func FuzzImageAltMatchesFrozen(f *testing.F) {
	for _, alt := range imageAltSeeds {
		f.Add(alt)
	}
	f.Fuzz(func(t *testing.T, alt string) {
		// Each call parses the alt in a document of its own, six times over, and a label of
		// brackets costs the parser far more than its length suggests: a long one spends the
		// fuzzer's budget on one input, and the writer's choices show on a short alt.
		if len(alt) > 256 {
			return
		}
		for _, context := range []inlineContext{{startsLine: true}, {heading: true}, {tableCell: true}} {
			if got, want := imageAlt(alt, context), frozenImageAlt(alt, context); got != want {
				t.Fatalf("imageAlt(%q, %+v) = %q, frozen %q", alt, context, got, want)
			}
		}
	})
}

// fuzzMarkSets are the marks a fuzzed text run carries: none, one, two, and marks whose attributes
// a run is told apart by.
var fuzzMarkSets = [][]Mark{
	nil,
	{{Type: "strong"}},
	{{Type: "emphasis"}},
	{{Type: "strong"}, {Type: "emphasis"}},
	{{Type: "link", Attrs: Attrs{"href": "https://example.test/a", "title": nil}}},
	{{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice"}}},
}

// fuzzTextRuns is value split into text runs of one to three characters, each carrying one of
// fuzzMarkSets by its place and salt, or false where value is no text a Yjs document holds: ygo
// refuses a text that is not UTF-8, a text holds at least one run, and a long one only slows the
// fuzzer down.
func fuzzTextRuns(value string, salt int) ([]*Node, bool) {
	if value == "" || len(value) > 1<<10 || !utf8.ValidString(value) {
		return nil, false
	}
	runes := []rune(value)
	runs := make([]*Node, 0, len(runes))
	for index := 0; index < len(runes); {
		width := min(1+(index+salt)%3, len(runes)-index)
		runs = append(runs, &Node{
			Type:  "text",
			Text:  string(runes[index : index+width]),
			Marks: fuzzMarkSets[(index+salt)%len(fuzzMarkSets)],
		})
		index += width
	}
	return runs, true
}

// updateYTextSeeds are the writes a Y text takes: the same text again, a text that only grows or
// only shrinks, one changed in the middle, an empty one, and astral characters, which Yjs counts
// as two units each.
var updateYTextSeeds = []struct {
	before, after string
	salt          uint8
}{
	{before: "abcdef", after: "abcdef", salt: 0},
	{before: "abcdef", after: "abcdefgh", salt: 1},
	{before: "abcdefgh", after: "abcd", salt: 2},
	{before: "abcdef", after: "abXYef", salt: 3},
	{before: "abcdef", after: "", salt: 4},
	{before: "", after: "abcdef", salt: 5},
	{before: "naïve café 😀", after: "naïve café 𝔸lpha", salt: 6},
	{before: "one two three four", after: "one two three four", salt: 7},
}

// updateYText leaves the Y text as the base implementation left it, whatever runs it is given and
// whatever the text held: the two are run over separately built but equal Y texts, and the texts'
// deltas must agree.
func FuzzUpdateYTextMatchesFrozen(f *testing.F) {
	for _, seed := range updateYTextSeeds {
		f.Add(seed.before, seed.after, seed.salt)
	}
	f.Fuzz(func(t *testing.T, before, after string, salt uint8) {
		held, ok := fuzzTextRuns(before, int(salt))
		if !ok {
			return
		}
		want, ok := fuzzTextRuns(after, int(salt)/2)
		if !ok {
			return
		}
		liveDoc, liveText, err := yTextOfRuns(held)
		if err != nil {
			return
		}
		frozenDoc, frozenText, err := yTextOfRuns(held)
		if err != nil {
			return
		}
		var liveErr, frozenErr error
		liveDoc.Transact(func(txn *crdt.Transaction) { liveErr = updateYText(txn, liveText, want) })
		frozenDoc.Transact(func(txn *crdt.Transaction) { frozenErr = frozenUpdateYText(txn, frozenText, want) })
		if fmt.Sprint(liveErr) != fmt.Sprint(frozenErr) {
			t.Fatalf("updateYText(%q -> %q, salt %d) = %v, frozen %v", before, after, salt, liveErr, frozenErr)
		}
		var live, frozen []crdt.Delta
		var liveDeltaErr, frozenDeltaErr error
		liveDoc.Transact(func(txn *crdt.Transaction) { live, liveDeltaErr = yTextDeltaInTransaction(liveText) })
		frozenDoc.Transact(func(txn *crdt.Transaction) { frozen, frozenDeltaErr = yTextDeltaInTransaction(frozenText) })
		if fmt.Sprint(liveDeltaErr) != fmt.Sprint(frozenDeltaErr) {
			t.Fatalf("reading the texts back after %q -> %q (salt %d) = %v, frozen %v", before, after, salt, liveDeltaErr, frozenDeltaErr)
		}
		if !reflect.DeepEqual(live, frozen) {
			t.Fatalf("updateYText(%q -> %q, salt %d) left %v, frozen %v", before, after, salt, live, frozen)
		}
	})
}

// context16 cuts what the base implementation cut, whatever text and window it is given.
func FuzzContext16MatchesFrozen(f *testing.F) {
	// The pairs and the hard strings position_test.go's own tables hold, each cut whole and in
	// halves, a cut inside a surrogate pair among them.
	for _, value := range append([]string{"a" + pairA + "b"}, len16Cases...) {
		units := len16(value)
		f.Add(value, 0, units)
		f.Add(value, units/2, units)
		f.Add(value, 1, units/2)
	}
	f.Fuzz(func(t *testing.T, value string, from, to int) {
		if len(value) > 16<<10 {
			return
		}
		// context16's callers cut a window inside the text: 0 <= from <= to <= its UTF-16 length.
		units := len16(value)
		from, to = int(uint(from)%uint(units+1)), int(uint(to)%uint(units+1))
		if from > to {
			from, to = to, from
		}
		if got, want := context16(value, from, to), frozenContext16(value, from, to); got != want {
			t.Fatalf("context16(%q, %d, %d) = %q, frozen %q", value, from, to, got, want)
		}
	})
}

// altReadsBackCallsOnAPlainAlt is how many times imageAlt reads an alt back while writing one that
// holds no ASCII punctuation, no line feed and no carriage return. It is the base's count; the
// change's commit lowers it to zero, since the writer leaves such an alt as it stands whatever a
// read back would say.
const altReadsBackCallsOnAPlainAlt = 1

// TestImageAltReadsAPlainAltBack holds imageAlt to how many reads back it costs to write an alt
// that needs no escape at all, through the altReadsBackFn seam.
func TestImageAltReadsAPlainAltBack(t *testing.T) {
	const alt = "a plain alt text with no punctuation at all in it"
	reads := 0
	readsBack := altReadsBackFn
	t.Cleanup(func() { altReadsBackFn = readsBack })
	altReadsBackFn = func(written, alt string, context inlineContext) int {
		reads++
		return readsBack(written, alt, context)
	}
	if written := imageAlt(alt, inlineContext{startsLine: true}); written != alt {
		t.Fatalf("imageAlt(%q) = %q, want the alt as it stands", alt, written)
	}
	if reads != altReadsBackCallsOnAPlainAlt {
		t.Fatalf("imageAlt read the alt back %d times, want %d", reads, altReadsBackCallsOnAPlainAlt)
	}
}
