package pmdoc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// The bodies a reader's browser tab froze on (sjawhar/legion#2519) are read here through the
// parser a caller's write takes, at the longest one block of a body may be: a plain list of
// 8,000 items, a quote and a list item each followed by lines that carry no marker of their own,
// a run of brackets, items nested by indentation and then blank lines, 49 markers on one line
// and then blank lines, a chain of list markers on one line, and a typed fence whose label opens
// a run of brackets. Each is stored or refused here as the browser's own suites expect it to be,
// so the two read the same body the same way.
//
// The shapes themselves are heavybodies.Shapes (pmdoc/heavybodies), not this file: the API's own
// test of the verdict each write gets reads the same bodies from there, and a _test.go file of
// this package cannot be imported (INFRA-1404).
//
// Each shape is asserted four ways: the verdict a write of it gets; the CPU time the parse spends
// on it, under heavyBodyCPUBudget (heavy_body_budget.go); and, for every stored shape, growth in
// proportion to its length, measured at an eighth of what it grows to and then at the whole of it
// (growsLinearly, linear_test.go) - of the parse alone, and of the parse and the encode of the
// document a write makes of the body together.
//
// The Playwright spec that wrote these same shapes through the API held each write to a second
// and to nothing about its growth: a ratio between two wall times a shared CI runner took, a
// doubling of the body apart, cannot separate a regression from the runner's own noise, which is
// what made that bound fail at random (INFRA-985). The growth claim is made here instead, in this
// process's own CPU time and at an eightfold step, and the verdict each write gets is gated in
// internal/dispatch/api (TestHeavyDocumentWriteVerdicts).
//
// These pass as they are written: every shape here already parses in under a second of CPU time.
// They are the bound the browser's own parse is held to on the same bodies, and the guard over the
// parse's own costs - the block ids a document's blocks are minted from, and the tab-indentation
// check each open block runs on each line - which a regression would make superlinear again.

// Every shape at the block bound is stored or refused as it is, and the parse of it spends less
// CPU time than heavyBodyCPUBudget allows.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedInTime(t *testing.T) {
	// The shapes spell their refusals with heavybodies.MaxNesting, which is this package's own
	// maxNesting copied: heavybodies may not import pmdoc, since this test is in-package, so the
	// two are held to each other here rather than by the compiler.
	if heavybodies.MaxNesting != maxNesting {
		t.Fatalf("heavybodies.MaxNesting = %d, want pmdoc's maxNesting %d", heavybodies.MaxNesting, maxNesting)
	}
	for _, shape := range heavybodies.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			body := shape.Grow()
			if len(body) > heavybodies.BlockCap {
				t.Fatalf("%s is %d characters, past the %d one block may hold", shape.Name, len(body), heavybodies.BlockCap)
			}
			var err error
			took := timedWithDeadline(t, "parsing "+shape.Name, perCallDeadline(t), func() (time.Duration, error) {
				return cpuTimeOf(again(func() { _, err = ParseForWrite(body, nil) }))
			})
			if shape.Refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v, want it stored", shape.Name, err)
				}
			} else if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), shape.Refusal) {
				t.Fatalf("%s: %v, want ErrSchema saying %q", shape.Name, err, shape.Refusal)
			}
			t.Logf("%s: %d characters in %s of CPU time", shape.Name, len(body), took.Round(time.Millisecond))
			if took > heavyBodyCPUBudget {
				t.Fatalf("%s took %s of CPU time, past the %s a body of %d characters may spend",
					shape.Name, took.Round(time.Millisecond), heavyBodyCPUBudget, len(body))
			}
		})
	}
}

// Every shape a write stores parses in time proportional to its length, measured at an eighth of
// what the shape grows and then at the whole of it: a shape whose cost grew with the square of
// its length would grow growthStep² times where a linear one may grow linearSlack*growthStep
// (growthStep, linear_test.go).
func TestHeavyBodiesAtTheBlockCapParseInTimeProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			growsLinearly(t, "parsing "+shape.Name, sizes, linearSlack, func(size int) (time.Duration, error) {
				body := shape.Body(size)
				var err error
				took, measureErr := cpuTimeOf(again(func() { _, err = ParseForWrite(body, nil) }))
				if err != nil {
					return 0, fmt.Errorf("parsing %d characters of %s: %w", size, shape.Name, err)
				}
				return took, measureErr
			})
		})
	}
}

// Every shape a write stores is parsed and encoded into a document in time proportional to its
// length, at the sizes the parse alone is measured at above: this is the whole of what a document
// write does with a body before it reaches Postgres - the parse of the markdown a caller sent and
// the write of that tree into a fresh document, encoded as the document's first durable update
// (parseReplacing and encodeDocumentTree, internal/dispatch/docs/tree.go). The parse is the part
// that has grown quadratically before (linear_test.go holds the shapes that did), and the encode
// is measured beside it because it is the rest of the time an API write spends on a body, which
// the end-to-end bound this test replaced used to cover (INFRA-985).
//
// Each run gets a document of its own, since a write changes the document it writes into, and the
// body it writes is built before the window opens (cpuTimeOf).
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInTimeProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			growsLinearly(t, "writing "+shape.Name, sizes, linearSlack, func(size int) (time.Duration, error) {
				body := shape.Body(size)
				var err error
				var update []byte
				took, measureErr := cpuTimeOf(func() func() {
					// "prosemirror" is the fragment the document service writes a document's
					// blocks into (fragmentName, internal/dispatch/docs/tree.go).
					doc := crdt.New()
					fragment := doc.GetXmlFragment("prosemirror")
					return func() {
						tree, parseErr := ParseForWrite(body, nil)
						if parseErr != nil {
							err = parseErr
							return
						}
						if err = doc.TransactE(func(txn *crdt.Transaction) error {
							return Update(txn, fragment, tree)
						}); err != nil {
							return
						}
						update = crdt.EncodeStateAsUpdateV1(doc, nil)
					}
				})
				if err != nil {
					return 0, fmt.Errorf("writing %d characters of %s: %w", size, shape.Name, err)
				}
				if len(update) == 0 {
					return 0, fmt.Errorf("writing %d characters of %s encoded no update", size, shape.Name)
				}
				return took, measureErr
			})
		})
	}
}
