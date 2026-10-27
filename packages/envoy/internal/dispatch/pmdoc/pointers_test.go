package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

// A pointer a write introduces to a block of a type its attribute does not allow is refused,
// naming what is wrong; one to an allowed or an absent block is accepted, and one the tree before
// the write already held is never refused, whatever its target has become.
func TestCheckBlockPointersRefusesATargetOfADisallowedType(t *testing.T) {
	value := blockPointerTestSchema(nil)
	// A draft holds the same attribute name as text, which a retype into a claim makes a pointer.
	value.Types = append(value.Types, BlockTypeSchema{Name: "draft", Content: BlockContentParagraphs, Render: "host", Attributes: map[string]BlockAttributeSchema{
		"serves": {Kind: "string"},
	}})
	anyBlock := blockPointerTestSchema(map[string]BlockAttributeSchema{"serves": {Kind: "block[]"}})
	UseBlockSchemaForTest(t, value)
	const (
		requirement = "dispatch://CORE-1/spec#b-r1"
		paragraph   = "dispatch://CORE-1/spec#b-p1"
		callout     = "dispatch://CORE-1/spec#b-k1"
		absent      = "dispatch://CORE-1/spec#b-gone"
	)
	types := map[string][]string{requirement: {"requirement"}, paragraph: {"paragraph"}, callout: {"callout"}}
	resolve := func(ref string) ([]string, error) { return types[ref], nil }
	parse := func(markdown string) *Node {
		t.Helper()
		tree, err := Parse(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return tree
	}
	claim := func(ref string) *Node {
		return parse(":::claim{#c1 serves=\"" + ref + "\"}\nIt holds.\n:::\n")
	}
	for _, test := range []struct {
		name       string
		tree       *Node
		prior      *Node
		refusedFor string // the target type the refusal names; empty when accepted
	}{
		{name: "a requirement", tree: claim(requirement)},
		{name: "a paragraph", tree: claim(paragraph), refusedFor: "paragraph"},
		{name: "a callout", tree: claim(callout), refusedFor: "callout"},
		{name: "a block no document holds", tree: claim(absent)},
		{name: "a callout the tree before already pointed at", tree: claim(callout), prior: claim(callout)},
		{name: "a callout replacing a requirement", tree: claim(callout), prior: claim(requirement), refusedFor: "callout"},
		{name: "a callout after a pointer of another block", tree: parse(":::claim{#c2 serves=\"" + callout + "\"}\nIt holds.\n:::\n"), prior: claim(callout), refusedFor: "callout"},
		{name: "a callout pointed at by a block retyped from text", tree: claim(callout), prior: parse(":::draft{#c1 serves=\"" + callout + "\"}\nIt holds.\n:::\n"), refusedFor: "callout"},
		{name: "a callout second after a requirement", tree: claim(requirement + " " + callout), prior: claim(requirement), refusedFor: "callout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckBlockPointers(test.tree, test.prior, resolve)
			if test.refusedFor == "" {
				if err != nil {
					t.Fatalf("CheckBlockPointers = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, ErrBlockPointer) || errors.Is(err, ErrSchema) {
				t.Fatalf("CheckBlockPointers = %v, want ErrBlockPointer and no schema refusal", err)
			}
			ref := map[string]string{"paragraph": paragraph, "callout": callout}[test.refusedFor]
			for _, want := range []string{`"serves"`, ref, `"` + test.refusedFor + `"`, "requirement"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("CheckBlockPointers = %v, want it to name %s", err, want)
				}
			}
		})
	}

	// Two blocks share a node id only across a store's rows; any of their types outside the
	// targets refuses.
	shared := func(string) ([]string, error) { return []string{"requirement", "callout"}, nil }
	if err := CheckBlockPointers(claim(requirement), nil, shared); !errors.Is(err, ErrBlockPointer) {
		t.Fatalf("a target holding a requirement and a callout = %v, want refused", err)
	}
	// A pointer with no targets points at any block, so nothing is looked up for it.
	UseBlockSchemaForTest(t, anyBlock)
	if err := CheckBlockPointers(claim(callout), nil, func(string) ([]string, error) {
		return nil, errors.New("looked up a pointer with no targets")
	}); err != nil {
		t.Fatalf("a pointer with no targets = %v, want accepted", err)
	}
}
