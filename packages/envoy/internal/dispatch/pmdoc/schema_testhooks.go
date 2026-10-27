package pmdoc

import (
	"encoding/json"
	"testing"
)

// UseBlockSchemaForTest makes value the block schema for the rest of t, as if the server had been
// built with it: every reader of this package, SchemaVersion, LinkKinds and the bytes SchemaJSON
// serves. It fails t when value is not a valid schema. The schema in use before is restored by a
// t.Cleanup, so a test calls this before it builds any server or service: cleanups run last in
// first out, so the restore runs after the server's own cleanup has stopped its room and
// settlement goroutines, which read the schema (a defer would restore it first and race them). A
// test that calls it does not call t.Parallel.
func UseBlockSchemaForTest(t testing.TB, value BlockSchema) {
	t.Helper()
	if err := normalizeSchemaDefaults(&value); err != nil {
		t.Fatalf("normalize test block schema: %v", err)
	}
	if err := validateBlockSchema(value); err != nil {
		t.Fatalf("invalid test block schema: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode test block schema: %v", err)
	}
	previous, previousJSON := schema, blockSchemaJSON
	installSchema(value)
	blockSchemaJSON = encoded
	t.Cleanup(func() {
		installSchema(previous)
		blockSchemaJSON = previousJSON
	})
}
