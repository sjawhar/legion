// Package pmdoctest swaps the block schema for Dispatch tests in more than one package. It is an
// ordinary package rather than a _test.go file because api and docs cannot import one another's
// test helpers, and keeping it out of pmdoc keeps package testing out of the server binary.
package pmdoctest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// UseBlockSchema makes schema the block schema for the rest of t, as if the server had been built
// with it (pmdoc.SetBlockSchemaForTest). It fails t when schema is not a valid schema. The schema
// in use before is restored by a t.Cleanup, so a test calls this before it builds any server or
// service: cleanups run last in first out, so the restore runs after the server's own cleanup has
// stopped its room and settlement goroutines, which read the schema (a defer would restore it
// first and race them). A test that calls it does not call t.Parallel.
func UseBlockSchema(t testing.TB, schema pmdoc.BlockSchema) {
	t.Helper()
	restore, err := pmdoc.SetBlockSchemaForTest(schema)
	if err != nil {
		t.Fatalf("use test block schema: %v", err)
	}
	t.Cleanup(restore)
}

// BlockPointerSchema is the block schema in use (the shipped one, its callout and its requirement
// included, unless a test swapped it) with one test type in place of any earlier one of its name:
// a claim holding claimAttributes. Nil claimAttributes is a serves attribute pointing only at
// requirements, the shipped type of that name. No shipped type is named claim, so this schema
// shadows none of them. Called again after UseBlockSchema, it replaces the claim.
func BlockPointerSchema(claimAttributes pmdoc.BlockAttributes) pmdoc.BlockSchema {
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &schema); err != nil {
		panic("pmdoctest: decode the block schema: " + err.Error())
	}
	if claimAttributes == nil {
		claimAttributes = pmdoc.BlockAttributes{{
			Name:                 "serves",
			BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeBlockList, Targets: []string{"requirement"}},
		}}
	}
	schema.Types = slices.DeleteFunc(schema.Types, func(typ pmdoc.BlockTypeSchema) bool {
		return typ.Name == "claim"
	})
	schema.Types = append(schema.Types,
		pmdoc.BlockTypeSchema{Name: "claim", Content: pmdoc.BlockContentParagraphs, Render: "host", Attributes: claimAttributes},
	)
	return schema
}
