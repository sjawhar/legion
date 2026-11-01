package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// UseManifestForTest makes manifest, a schema manifest in blocks.json's format, the one this
// package reads and writes by, in place of the embedded one, until restore is called. A test uses
// it to act as a build whose manifest declares an element the embedded one does not, or numbers it
// differently. GET /api/v1/schema/blocks keeps serving the embedded file (SchemaJSON). The
// package's readers do not lock the manifest, so no other goroutine may use the package while it
// is replaced or restored.
func UseManifestForTest(manifest []byte) (restore func(), err error) {
	compiled, err := compileManifest(manifest)
	if err != nil {
		return nil, err
	}
	previous := active
	compiled.install()
	return previous.install, nil
}

// SetBlockSchemaForTest makes value the block schema, as if the server had been built with it:
// every reader of this package, SchemaVersion, LinkKinds and the bytes SchemaJSON serves. It
// refuses value when it is not a valid manifest, and changes nothing then. restore puts back the
// manifest in use before. Tests reach it through pmdoctest.UseBlockSchema, which says when a test
// may call it. It differs from UseManifestForTest in taking the manifest as a value rather than
// as bytes, and in swapping the bytes SchemaJSON serves with it.
func SetBlockSchemaForTest(value BlockSchema) (restore func(), err error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode test block schema: %w", err)
	}
	compiled, err := compileManifest(encoded)
	if err != nil {
		return nil, fmt.Errorf("test block schema: %w", err)
	}
	previous, previousJSON := active, blockSchemaJSON
	compiled.install()
	blockSchemaJSON = encoded
	return func() {
		previous.install()
		blockSchemaJSON = previousJSON
	}, nil
}

// UseStagedBlockForTest makes the embedded manifest with one more typed block the manifest this
// package reads and writes by, until restore is called (UseManifestForTest): a block named name,
// of block content, rendered by the host, which a build reads from schema version 2 and may
// create from schema version creatableSince, as a build that stages an element declares one. A
// test uses it to act as a build before that element became creatable (creatableSince above
// SchemaVersion), or as the build it became creatable in.
func UseStagedBlockForTest(name string, creatableSince int) (restore func(), err error) {
	var manifest map[string]any
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		return nil, fmt.Errorf("decode the embedded manifest: %w", err)
	}
	types, ok := manifest["types"].([]any)
	if !ok {
		return nil, errors.New("the embedded manifest declares no types array")
	}
	manifest["types"] = append(types, map[string]any{
		"name": name, "content": "block+", "render": "host", "attributes": map[string]any{},
		"since": 2, "creatable_since": creatableSince,
	})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode the manifest staging the typed block %q: %w", name, err)
	}
	return UseManifestForTest(encoded)
}
