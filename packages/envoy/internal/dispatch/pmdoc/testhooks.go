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

// UseStagedBlockForTest makes the embedded manifest, its version raised by one and one more typed
// block declared, the manifest this package reads and writes by, until restore is called
// (UseManifestForTest): a block named name, of block content, rendered by the host, read from
// the raised version and created from it when creatable, or from the version after it when not,
// as the build that introduces an element declares it. A test uses it to act as that build,
// before the element became creatable or as the build it became creatable in. The version is
// raised because an element is read from the version that introduces it, and this build's own
// version introduces the elements it ships.
func UseStagedBlockForTest(name string, creatable bool) (restore func(), err error) {
	var manifest map[string]any
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		return nil, fmt.Errorf("decode the embedded manifest: %w", err)
	}
	version, ok := manifest["version"].(float64)
	if !ok {
		return nil, errors.New("the embedded manifest declares no version")
	}
	types, ok := manifest["types"].([]any)
	if !ok {
		return nil, errors.New("the embedded manifest declares no types array")
	}
	staged := int(version) + 1
	creatableSince := staged + 1
	if creatable {
		creatableSince = staged
	}
	manifest["version"] = staged
	manifest["types"] = append(types, map[string]any{
		"name": name, "content": "block+", "render": "host", "attributes": map[string]any{},
		"since": staged, "creatable_since": creatableSince,
	})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode the manifest staging the typed block %q: %w", name, err)
	}
	return UseManifestForTest(encoded)
}
