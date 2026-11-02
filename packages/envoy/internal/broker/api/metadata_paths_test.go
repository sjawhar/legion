// This file is package api rather than api_test for one reason: it reads the broker's own route
// table (routes()) and the path constants the metadata document and Register are written
// against, neither of which is worth exporting so a test can see it.
package api

import (
	"net/http"
	"testing"
)

// TestTheMetadataPathsAreRoutesTheBrokerMounts walks the paths handlers_metadata.go names — the
// ones the document hands a client and the one Register leaves out when the broker serves no
// device grant — and fails on any that is not a row of the broker's own table. The rows must
// spell their patterns as literals, since the generated reference reads them as source, so this
// is what keeps a renamed route from leaving the document pointing at a 404.
func TestTheMetadataPathsAreRoutesTheBrokerMounts(t *testing.T) {
	mounted := map[string]string{}
	for _, route := range routes() {
		mounted[route.Pattern] = route.Method
	}
	for _, row := range []struct{ path, method string }{
		{metadataPath, http.MethodGet},
		{deviceAuthorizationPath, http.MethodPost},
		{tokenPath, http.MethodPost},
		{introspectionPath, http.MethodPost},
		{revocationPath, http.MethodPost},
	} {
		method, found := mounted[row.path]
		if !found {
			t.Errorf("%s is named in the metadata document or in Register, but the route table has no row for it", row.path)
			continue
		}
		if method != row.method {
			t.Errorf("%s is mounted as %s, want %s", row.path, method, row.method)
		}
	}
}
