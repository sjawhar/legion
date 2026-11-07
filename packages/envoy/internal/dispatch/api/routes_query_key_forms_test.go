package api

import (
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// The query-key walk of routes_query_keys_test.go is what ties a parameters struct to the keys
// its handlers read, so a key it cannot see is a key that leaves the emitted document and every
// generated client with nothing failing. It reads Get and Has on a url.Values, and an index of a
// url.Values held by a name. This package reads a query key in two more forms, and both are
// already in it:
//
//   - an index of the call itself, r.URL.Query()["since"] — asks.go, message_delivery.go and
//     references.go;
//   - r.FormValue, which http.Request fills from the URL's query as well as the body — artifacts.go.
//
// Measured at 4c525aba05e9: a handler given `_, _ = r.URL.Query()["totally_new_key"]`, and one
// given `_ = r.FormValue("another_new_key")`, each leave
// TestEveryQueryKeyAHandlerReadsIsNamedByItsParameters green.
//
// So this states the walk's contract over every form the package reads a key in. Each line of
// the source below is a form taken from the package, and each key must come back.
func TestTheQueryKeyWalkSeesEveryFormThisPackageReadsAKeyIn(t *testing.T) {
	const source = `package api

import "net/http"

func (s *server) sample(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	_ = query.Get("by_get")
	_ = query.Has("by_has")
	_ = query["by_named_index"]
	_, _ = r.URL.Query()["by_call_index"]
	_ = r.URL.Query().Get("by_call_get")
	_ = r.FormValue("by_form_value")
}
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatalf("parsing the sample: %v", err)
	}

	keys, dynamic := queryKeysRead(parsed)
	for _, key := range []string{
		"by_get",
		"by_has",
		"by_named_index",
		"by_call_index",
		"by_call_get",
		"by_form_value",
	} {
		if !keys[key] {
			t.Errorf("the walk does not see the query key %q, so a handler reading it would drop the "+
				"key from the generated document with nothing failing", key)
		}
	}
	if len(dynamic) != 0 {
		t.Errorf("the walk reported %v unresolved, and every read of the sample names its key outright", slices.Clone(dynamic))
	}
}
