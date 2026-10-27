package pmdoc

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
