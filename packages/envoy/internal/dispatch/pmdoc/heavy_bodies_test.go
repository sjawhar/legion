package pmdoc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The bodies a reader's browser tab froze on (sjawhar/legion#2519) are read here through the
// parser a caller's write takes, at the longest body one may write: a plain list of 8,000 items, a
// quote and a list item each followed by lines that carry no marker of their own, a run of
// brackets, items nested by indentation and then blank lines, a chain of list markers on one line,
// and a typed fence whose label opens a run of brackets. Each is stored or refused here as the
// browser's own suites expect it to be, so the two read the same body the same way.
//
// Each shape is asserted three ways: the verdict a write of it gets; the CPU time the parse spends
// on it, under heavyBodyCPUBudget outside -race (heavy_body_budget.go, and no time bound under it,
// where the race detector's own per-access instrumentation costs 2-8 s on these same bodies and
// bounds nothing about this package's own work); and, for every stored shape, growth in proportion
// to its length, measured at an eighth of what it grows to and then at the whole of it
// (growsLinearly, linear_test.go), in both builds.
//
// These pass as they are written: every shape here already parses in under a second of CPU time.
// They are the bound the browser's own parse is held to on the same bodies, and the guard over the
// parse's own costs - the block ids a document's blocks are minted from, and the tab-indentation
// check each open block runs on each line - which a regression would make superlinear again.

// bodyCap is the longest body a caller may write, in characters: the cap the comment, message and
// broadcast routes refuse a longer body against (contracts.DispatchBodyMax). Every shape here
// stands at it, so what is measured is the worst one reader can be sent.
const bodyCap = contracts.DispatchBodyMax

// heavyBody is one of the shapes, as a test reads it.
type heavyBody struct {
	// name says what the shape is and names its subtest.
	name string
	// body is the shape carrying size characters of whatever it repeats, after its own fixed
	// opening (a quote's first line, a nesting of items, a typed fence) where it has one.
	body func(size int) string
	// grown is how many repeated characters the shape carries at the body cap. It is also the
	// size growsLinearly scales, so a shape whose opening is a fixed cost - the items the nested
	// shape indents before its blank lines - is measured over the part of it that grows, not over
	// a body two thirds of which is the same prefix at both sizes.
	grown int
	// refusal is what a write of the shape is refused with, under ErrSchema, or "" where the body
	// is stored.
	refusal string
	// (the count of runs one timed window holds is cputest.Measure's own, through cpuTimeOf: it
	// raises a cheap shape above the resolution this process's CPU time is reported at and divides
	// the window by the runs it held, so no shape carries a repeat count of its own.)
}

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

// nestedRefusal is how a write nesting its blocks past maxNesting is refused, the block past the
// bound standing on line.
func nestedRefusal(line int) string {
	return fmt.Sprintf("line %d opens a block inside %d blocks", line, maxNesting)
}

// heavyBodyShapes are the shapes, each at the body cap.
//
// A list opener opens a list and its first item, two of the blocks maxNesting counts, so 49 items
// nested by indentation stand inside 98 blocks and are stored, while the 51st line of 100 such
// items opens a block inside 100 and is refused - as the chain of markers on one line is, at its
// 51st marker. The browser draws exactly those lines as the text they are instead.
func heavyBodyShapes() []heavyBody {
	nest49, nest100 := nestedItems(49), nestedItems(100)
	// The typed fence spends eleven characters on its own opening, and its label opens a run of
	// 15,000 brackets that the rest of the line closes all but eleven of.
	const typedFence = ":::callout["
	const bracketOpeners = 15_000
	return []heavyBody{
		{
			name:  "a plain list of 8,000 items",
			body:  func(size int) string { return strings.Repeat("- x\n", size/4) },
			grown: bodyCap,
		},
		{
			name:  "a quote then lines carrying no marker",
			body:  func(size int) string { return "> x\n" + strings.Repeat("x\n", size/2) },
			grown: bodyCap - len("> x\n"),
		},
		{
			name:  "a list item then lines carrying no marker",
			body:  func(size int) string { return "- x\n" + strings.Repeat("x\n", size/2) },
			grown: bodyCap - len("- x\n"),
		},
		{
			name: "a run of 15,000 brackets and as many closers",
			body: func(size int) string {
				return strings.Repeat("[", size/2) + strings.Repeat("]", size-size/2)
			},
			grown: 30_000,
		},
		{
			name:  "49 items nested by indentation then blank lines",
			body:  func(size int) string { return nest49 + strings.Repeat("\n", size) },
			grown: bodyCap - len(nest49),
		},
		{
			name:    "100 items nested by indentation then blank lines",
			body:    func(size int) string { return nest100 + strings.Repeat("\n", size) },
			grown:   bodyCap - len(nest100),
			refusal: nestedRefusal(51),
		},
		{
			name:    "a chain of 15,999 list markers on one line",
			body:    func(size int) string { return strings.Repeat("- ", size/2) + "x" },
			grown:   bodyCap - len("x"),
			refusal: nestedRefusal(1),
		},
		{
			name: "a typed fence whose label opens a run of brackets",
			body: func(size int) string {
				return typedFence + strings.Repeat("[", bracketOpeners) + strings.Repeat("]", size-bracketOpeners)
			},
			grown: 30_000 - len(typedFence),
			// The label swallows the line, so what stands there is a fence naming no typed block.
			refusal: "typed block directives use :::name{...}",
		},
	}
}

// Every shape at the body cap is stored or refused as it is, and the parse of it spends less CPU
// time than heavyBodyCPUBudget allows outside -race.
func TestHeavyBodiesAtTheBodyCapAreStoredOrRefusedInTime(t *testing.T) {
	for _, shape := range heavyBodyShapes() {
		t.Run(shape.name, func(t *testing.T) {
			body := shape.body(shape.grown)
			if len(body) > bodyCap {
				t.Fatalf("%s is %d characters, past the %d a caller may write", shape.name, len(body), bodyCap)
			}
			var err error
			took := timedWithDeadline(t, "parsing "+shape.name, perCallDeadline, func() (time.Duration, error) {
				return cpuTimeOf(again(func() { _, err = ParseForWrite(body, nil) }))
			})
			if shape.refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v, want it stored", shape.name, err)
				}
			} else if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), shape.refusal) {
				t.Fatalf("%s: %v, want ErrSchema saying %q", shape.name, err, shape.refusal)
			}
			t.Logf("%s: %d characters in %s of CPU time", shape.name, len(body), took.Round(time.Millisecond))
			if heavyBodyCPUBudget > 0 && took > heavyBodyCPUBudget {
				t.Fatalf("%s took %s of CPU time, past the %s a body of %d characters may spend",
					shape.name, took.Round(time.Millisecond), heavyBodyCPUBudget, len(body))
			}
		})
	}
}

// Every shape a write stores parses in time proportional to its length, measured at an eighth of
// what the shape grows and then at the whole of it, in both builds: a shape whose cost grew with
// the square of its length would grow growthStep² times where a linear one may grow
// linearSlack*growthStep (growthStep, linear_test.go).
func TestHeavyBodiesAtTheBodyCapParseInTimeProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavyBodyShapes() {
		if shape.refusal != "" {
			continue
		}
		t.Run(shape.name, func(t *testing.T) {
			sizes := []int{shape.grown / growthStep, shape.grown}
			growsLinearly(t, "parsing "+shape.name, sizes, linearSlack, func(size int) (time.Duration, error) {
				body := shape.body(size)
				var err error
				took, measureErr := cpuTimeOf(again(func() { _, err = ParseForWrite(body, nil) }))
				if err != nil {
					return 0, fmt.Errorf("parsing %d characters of %s: %w", size, shape.name, err)
				}
				return took, measureErr
			})
		})
	}
}
