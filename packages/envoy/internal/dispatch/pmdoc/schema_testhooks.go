package pmdoc

import (
	"encoding/json"
	"fmt"
)

// SetBlockSchemaForTest makes value the block schema, as if the server had been built with it:
// every reader of this package, SchemaVersion, LinkKinds and the bytes SchemaJSON serves. It
// refuses value when it is not a valid schema, and changes nothing then. restore puts back the
// schema in use before. Tests reach it through pmdoctest.UseBlockSchema, which says when a test
// may call it.
func SetBlockSchemaForTest(value BlockSchema) (restore func(), err error) {
	if err := normalizeSchemaDefaults(&value); err != nil {
		return nil, fmt.Errorf("normalize test block schema: %w", err)
	}
	if err := validateBlockSchema(value); err != nil {
		return nil, fmt.Errorf("invalid test block schema: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode test block schema: %w", err)
	}
	previous, previousJSON := schema, blockSchemaJSON
	installSchema(value)
	blockSchemaJSON = encoded
	return func() {
		installSchema(previous)
		blockSchemaJSON = previousJSON
	}, nil
}
