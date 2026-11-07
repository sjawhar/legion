// Package heavybodies holds the heavy markdown bodies a document write is measured and gated on,
// in one place, so the reader's tests (internal/dispatch/pmdoc) and the API's (internal/dispatch/api)
// write the very same bytes. They lived in pmdoc's own _test.go files, which no other package can
// import, until the browser rows that wrote them through the API were moved into Go (INFRA-1404).
//
// Eight of these bodies are spelled a second time in TypeScript, in packages/dispatch/e2e/heavy-bodies.ts,
// which the browser suite draws them from; the other three are Go's alone, drawn by no browser row.
// What reads those three is Go: internal/dispatch/api's TestHeavyDocumentWriteVerdicts writes each
// of them through the API, pmdoc's own tests parse them, and digests_test.go beside this file only
// hashes them. A checked-in digest table holds the two spellings to each other:
// testdata/shared-bodies.json names every body both sides spell - by the ID each Shape and
// NestedDirective below carries - with that body's length in bytes and the SHA-256 of those bytes,
// and every body only one side has, with why it is alone. Both sides
// read the table and neither may drift from it: digests_test.go here, and
// packages/dispatch/e2e/__tests__/heavy-bodies-digests.test.ts there, each fail where a body of
// their own no longer digests to its row, and where their side has gained, lost or renamed one. So
// a shape changed on one side alone fails that side's test, instead of leaving the other comparing
// different bytes against the same numbers (sjawhar/legion#2519). The table carries its own
// contract in its schema field, JSON having nowhere to put a comment.
//
// This package imports pmdoc nowhere, on purpose: pmdoc's tests are in-package, so an import of
// pmdoc here would close a cycle. What it needs of pmdoc's bounds it carries itself, as MaxNesting,
// and pmdoc's own test holds the two to each other.
package heavybodies

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// BlockCap is the longest one block of a body may be, in characters: the bound the comment,
// message and broadcast routes refuse a longer block against (contracts.DispatchBlockUnits).
// Every shape here is one or two blocks standing at it, so what is measured is the worst one
// reader can be sent in a block.
const BlockCap = contracts.DispatchBlockUnits

// MaxNesting is how many blocks a markdown document may open inside one another, the bound the
// refusals below quote. It is pmdoc's own maxNesting (internal/dispatch/pmdoc/nesting.go), copied
// because this package may not import pmdoc; pmdoc's
// TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedInTime holds the two to each other.
const MaxNesting = 100

// Shape is one heavy body, at whatever length a caller asks of it.
type Shape struct {
	// ID is the key testdata/shared-bodies.json pins this shape's bytes to, and the key
	// e2e/heavy-bodies.ts spells the same body under in its own record. The two sides are matched
	// by it rather than by Name, which only this one has.
	ID string
	// Name says what the shape is and names its subtest.
	Name string
	// Body is the shape carrying size characters of whatever it repeats, after its own fixed
	// opening (a quote's first line, a nesting of items, a typed fence) where it has one.
	Body func(size int) string
	// Grown is how many repeated characters the shape carries at the body cap. It is also the
	// size a growth measurement scales, so a shape whose opening is a fixed cost - the items the
	// nested shape indents before its blank lines - is measured over the part of it that grows,
	// not over a body two thirds of which is the same prefix at both sizes.
	Grown int
	// Refusal is what a write of the shape is refused with, under pmdoc.ErrSchema, or "" where
	// the body is stored.
	Refusal string
}

// Grow is the shape at the body cap: the bytes a write of it sends.
func (s Shape) Grow() string { return s.Body(s.Grown) }

// nestedItems is depth list items, each indented one level deeper than the item before it, so the
// item on line i opens a list and an item inside the i-1 lists and items above it.
func nestedItems(depth int) string {
	var body strings.Builder
	for level := range depth {
		body.WriteString(strings.Repeat("  ", level))
		body.WriteString("- a\n")
	}
	return body.String()
}

// nestedRefusal is how a write nesting its blocks past MaxNesting is refused, the block past the
// bound standing on line.
func nestedRefusal(line int) string {
	return fmt.Sprintf("line %d opens a block inside %d blocks", line, MaxNesting)
}

// Shapes are the bodies a reader's browser tab froze on (sjawhar/legion#2519), each at the body
// cap: a plain list of 8,000 items, a quote and a list item each followed by lines that carry no
// marker of their own, a run of brackets, items nested by indentation and then blank lines, 49
// markers on one line and then blank lines, a chain of list markers on one line, and a typed
// fence whose label opens a run of brackets.
//
// A list opener opens a list and its first item, two of the blocks MaxNesting counts, so 49 items
// nested by indentation stand inside 98 blocks and are stored, while the 51st line of 100 such
// items opens a block inside 100 and is refused - as the chain of markers on one line is, at its
// 51st marker. The browser draws exactly those lines as the text they are instead.
//
// Six of the nine are bytes for bytes what e2e/heavy-bodies.ts spells for the browser suite, and
// three are Go's alone, drawn by no browser row: which is which, and what each of the six digests
// to, is testdata/shared-bodies.json, held over the shapes here by digests_test.go and over the
// TypeScript by e2e/__tests__/heavy-bodies-digests.test.ts.
func Shapes() []Shape {
	nest49, nest100 := nestedItems(49), nestedItems(100)
	// markers49 is the same 98 blocks written on one line.
	markers49 := strings.Repeat("- ", 49) + "x\n"
	// The typed fence spends eleven characters on its own opening, and its label opens a run of
	// 15,000 brackets that the rest of the line closes all but eleven of.
	const typedFence = ":::callout["
	const bracketOpeners = 15_000
	return []Shape{
		{
			ID:    "plain-list",
			Name:  "a plain list of 8,000 items",
			Body:  func(size int) string { return strings.Repeat("- x\n", size/4) },
			Grown: BlockCap,
		},
		{
			ID:    "lazy-quote",
			Name:  "a quote then lines carrying no marker",
			Body:  func(size int) string { return "> x\n" + strings.Repeat("x\n", size/2) },
			Grown: BlockCap - len("> x\n"),
		},
		{
			ID:    "lazy-list-item",
			Name:  "a list item then lines carrying no marker",
			Body:  func(size int) string { return "- x\n" + strings.Repeat("x\n", size/2) },
			Grown: BlockCap - len("- x\n"),
		},
		{
			ID:   "bracket-run",
			Name: "a run of 15,000 brackets and as many closers",
			Body: func(size int) string {
				return strings.Repeat("[", size/2) + strings.Repeat("]", size-size/2)
			},
			Grown: 30_000,
		},
		{
			ID:    "nested-49-items-then-blank-lines",
			Name:  "49 items nested by indentation then blank lines",
			Body:  func(size int) string { return nest49 + strings.Repeat("\n", size) },
			Grown: BlockCap - len(nest49),
		},
		{
			// 49 markers on ONE line, not one item per line as the shape above writes them. The
			// two nest the same 98 blocks and are stored alike, but they are not the same bytes
			// and they cost the parser different things - one line holding 50 open blocks over
			// 31,900 blank lines, against 49 lines of two characters each - so both are here
			// rather than one standing in for the other (INFRA-1404). e2e/heavy-bodies.ts spelled
			// this body too, as nested49ThenBlankLines, for the document writes the deleted
			// heavy-document-writes.e2e.ts made through the API; no browser row draws it now, so
			// testdata/shared-bodies.json lists this shape and the one above alike, as bodies only
			// Go has. The 49-marker row heavy-bodies.e2e.ts still draws is a third body again: 49
			// markers padded to one character under the parse worker's threshold, not to the cap.
			ID:    "markers-49-then-blank-lines",
			Name:  "49 list markers on one line then blank lines",
			Body:  func(size int) string { return markers49 + strings.Repeat("\n", size) },
			Grown: BlockCap - len(markers49),
		},
		{
			ID:      "nested-100-items-then-blank-lines",
			Name:    "100 items nested by indentation then blank lines",
			Body:    func(size int) string { return nest100 + strings.Repeat("\n", size) },
			Grown:   BlockCap - len(nest100),
			Refusal: nestedRefusal(51),
		},
		{
			ID:      "marker-chain",
			Name:    "a chain of 15,999 list markers on one line",
			Body:    func(size int) string { return strings.Repeat("- ", size/2) + "x" },
			Grown:   BlockCap - len("x"),
			Refusal: nestedRefusal(1),
		},
		{
			ID:   "callout-bracket-run",
			Name: "a typed fence whose label opens a run of brackets",
			Body: func(size int) string {
				return typedFence + strings.Repeat("[", bracketOpeners) + strings.Repeat("]", size-bracketOpeners)
			},
			Grown: 30_000 - len(typedFence),
			// The label swallows the line, so what stands there is a fence naming no typed block.
			Refusal: "typed block directives use :::name{...}",
		},
	}
}

// NestedDirective is one of the bodies that froze a reader's tab in the browser engine
// (INFRA-581): a bare `:::a`, which Go reads as a malformed directive whatever it is nested in, so
// a document write of either is refused.
type NestedDirective struct {
	// ID is the key testdata/shared-bodies.json pins this body's bytes to, and the key
	// e2e/heavy-bodies.ts spells the same body under in its own record. The two sides are matched
	// by it rather than by Name, which only this one has.
	ID string
	// Name says what the body is and names its subtest.
	Name string
	// Markdown is the body itself, whole: unlike a Shape it does not grow.
	Markdown string
}

// NestedDirectives are the two deeply nested bare-directive bodies a document write refuses. The
// browser suite spells both (e2e/heavy-bodies.ts, drawn by e2e/nested-directive.e2e.ts), so both
// stand in testdata/shared-bodies.json.
func NestedDirectives() []NestedDirective {
	return []NestedDirective{
		{
			ID:       "directive-openers-then-long-line",
			Name:     "2,000 unclosed openers then a long line",
			Markdown: strings.Repeat(":::a\n", 2000) + strings.Repeat("x", 21000),
		},
		{
			ID:       "directive-openers-then-closers",
			Name:     "1,000 openers then 1,000 closing lines",
			Markdown: strings.Repeat(":::a\n", 1000) + strings.Repeat(":::\n", 1000),
		},
	}
}
