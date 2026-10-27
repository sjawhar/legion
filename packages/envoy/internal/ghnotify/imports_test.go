package ghnotify

import (
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestImportsAreModular: the poller is an optional module, so it imports five Envoy packages and
// no other (R13, A13's static half). internal/bus above all: the listener hands it a publisher,
// and a poller that reached for the bus could not be left out of a listener that has no token.
func TestImportsAreModular(t *testing.T) {
	allowed := map[string]bool{
		"github.com/sjawhar/envoy/internal/webhook":   true,
		"github.com/sjawhar/envoy/internal/contracts": true,
		"github.com/sjawhar/envoy/internal/id":        true,
		"github.com/sjawhar/envoy/internal/logging":   true,
		"github.com/sjawhar/envoy/internal/metrics":   true,
	}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s: %v", name, spec.Path.Value, err)
			}
			if strings.HasPrefix(path, "github.com/sjawhar/envoy/") && !allowed[path] {
				t.Errorf("%s imports %s; the poller may import only %v", name, path, slices.Sorted(maps.Keys(allowed)))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go file was parsed")
	}
}
