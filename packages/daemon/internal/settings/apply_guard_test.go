package settings

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// settingsArms is the one source file of this module allowed to apply a settings change: one
// call site per arm, both in Keeper.
const settingsArms = "internal/settings/keeper.go"

// TestEachSettingsArmHasOneApplyCallSite enforces decision D2: the file under the daemon and the
// settings route are two ways in through one code path. Every settings change — the file read
// again, an operator's or Dispatch's call, and the boot's restore of what the store kept — goes
// through config.Live.Apply from Keeper and nowhere else.
//
// A second call site would be a second resolution path, and the whole promise of the one holder
// rests on there not being one: a route change meets exactly the refusals a file change meets
// because both resolve the whole document through the loader, the store of route changes is
// written where the route applies and nowhere else, and the provenance the state reports is the
// one observer's. A caller that resolved a configuration of its own, or applied a change without
// recording it, would leave the daemon running on settings its store and its state disagree
// with, and nothing would say so.
//
// Its reach is every non-test Go file of the module: a call written `live.Apply(config.SourceFile,
// …)`, whatever the receiver is named. A test applies changes directly on purpose — that is how
// internal/config and internal/daemon drive the holder — so test sources are outside it.
func TestEachSettingsArmHasOneApplyCallSite(t *testing.T) {
	sites := map[string][]string{}
	files := token.NewFileSet()
	root := "../.."
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || name == "testdata" ||
				(strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(files, path, source, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector || selector.Sel.Name != "Apply" {
				return true
			}
			source := render(files, call.Args[0])
			if source != "config.SourceFile" && source != "config.SourceRoute" {
				return true
			}
			site := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(path, root+"/")))
			sites[source] = append(sites[source], site+":"+files.Position(call.Pos()).String())
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range []string{"config.SourceFile", "config.SourceRoute"} {
		found := sites[arm]
		slices.Sort(found)
		if len(found) != 1 || !strings.HasPrefix(found[0], settingsArms+":") {
			t.Errorf("the %s arm is applied at %d call site(s), %v; want exactly one, in %s.\n"+
				"Both arms of the daemon's settings go through internal/settings' Keeper: the file "+
				"through File, the route and the boot's restore through applyRoute. A second call site "+
				"is a second resolution path, and the store of route changes, the state's provenance "+
				"and the log of what the daemon applied are all written where that one path runs.",
				arm, len(found), found, settingsArms)
		}
	}
}

// render is one expression as it is written in the source.
func render(files *token.FileSet, node ast.Node) string {
	var written strings.Builder
	if err := printer.Fprint(&written, files, node); err != nil {
		return ""
	}
	return written.String()
}
