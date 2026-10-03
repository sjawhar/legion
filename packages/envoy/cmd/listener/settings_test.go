package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// environmentReaders are the functions that read the process environment, by package.
var environmentReaders = map[string][]string{
	"os":      {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
	"syscall": {"Getenv", "Environ"},
}

// environmentReads walks the non-test Go files under roots and returns each reference to an
// environment reader as "<file name> <package>.<name>", whatever name the file imports the package
// under, and each dot import of a reader's package as "<file name> import . <package>", since with
// the package's names unqualified a read cannot be told from a call to the file's own function.
func environmentReads(roots ...string) ([]string, error) {
	var reads []string
	fileSet := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			packages := map[string]string{}
			for _, spec := range file.Imports {
				importPath, _ := strconv.Unquote(spec.Path.Value)
				if _, reads := environmentReaders[importPath]; !reads {
					continue
				}
				name := importPath
				if spec.Name != nil {
					name = spec.Name.Name
				}
				if name == "." {
					reads = append(reads, filepath.Base(path)+" import . "+importPath)
				}
				packages[name] = importPath
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); ok {
					if importPath, ok := packages[pkg.Name]; ok && slices.Contains(environmentReaders[importPath], selector.Sel.Name) {
						reads = append(reads, filepath.Base(path)+" "+importPath+"."+selector.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return reads, nil
}

// The listener reads its environment in one place: processSettings, over the settings table. Any
// other reference to the process environment in cmd/listener or the two packages that read its
// settings is a reader the table, `envoy-listener settings` and the generated settings reference do
// not list.
func TestNoReaderBypassesTheSettingsTable(t *testing.T) {
	reads, err := environmentReads(".", filepath.Join("..", "..", "internal", "config"), filepath.Join("..", "..", "internal", "webhook"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"settings.go os.LookupEnv"}; !slices.Equal(reads, want) {
		t.Errorf("environment reads outside the settings table: %q, want only %q (processSettings). Add the variable as a row of the settings table (cmd/listener/settings.go) and hand its reader the value", reads, want)
	}
}

// The guard sees a read however the file imports its package: under another name, or with no
// name at all, where the read is a bare Getenv.
func TestTheGuardSeesAReadUnderAnyImportName(t *testing.T) {
	for name, probe := range map[string]string{
		"an aliased import": "package probe\n\nimport environment \"os\"\n\nfunc probe() string { return environment.Getenv(\"ENVOY_PROBE\") }\n",
		"a dot import":      "package probe\n\nimport . \"os\"\n\nfunc probe() string { return Getenv(\"ENVOY_PROBE\") }\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(probe), 0o644); err != nil {
				t.Fatal(err)
			}
			reads, err := environmentReads(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(reads) != 1 || !strings.HasPrefix(reads[0], "probe.go ") {
				t.Errorf("environment reads %q, want the one in probe.go", reads)
			}
		})
	}
}

func TestSettingsTableNamesEachVariableOnce(t *testing.T) {
	name := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	seen := map[string]bool{}
	for _, row := range settings {
		variables := []string{row.Name}
		if row.File {
			variables = append(variables, row.fileVariable())
		}
		for _, variable := range variables {
			if seen[variable] {
				t.Errorf("%s is in the settings table twice", variable)
			}
			seen[variable] = true
		}
		if !name.MatchString(row.Name) {
			t.Errorf("%q is not an environment variable name", row.Name)
		}
		if row.Required != "yes" && row.Required != "no" && !strings.HasPrefix(row.Required, "when ") {
			t.Errorf("%s: Required %q, want yes, no, or when <condition>", row.Name, row.Required)
		}
		if row.Description == "" || strings.Contains(row.Description, "\n") {
			t.Errorf("%s: Description %q, want one line", row.Name, row.Description)
		}
	}
}

func TestSettingValuesRefuseAVariableTheTableDoesNotList(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "ENVOY_NOT_A_SETTING") {
			t.Fatalf("recovered %v, want a panic naming ENVOY_NOT_A_SETTING", recovered)
		}
	}()
	readSettings(func(string) (string, bool) { return "x", true }).get("ENVOY_NOT_A_SETTING")
}

// settingsFrom is the table read from values alone, never the process environment.
func settingsFrom(values map[string]string) settingValues {
	return readSettings(func(name string) (string, bool) {
		value, set := values[name]
		return value, set
	})
}

// Every read main makes asks only for settings the table lists, on every branch: a name it does
// not list panics at startup, before the listener serves anything. The bus's connect asks for its
// NATS credential and reach whatever the URL, so it is held to the table too.
func TestEveryReaderAsksOnlyForSettingsTheTableLists(t *testing.T) {
	env := settingsFrom(map[string]string{
		"ENVOY_MACHINE_ID":            "settings-test",
		"NATS_URLS":                   testnats.URL(t),
		"PORT":                        "9021",
		"ENVOY_WEBHOOKS":              "github,slack,ghostwispr",
		"ENVOY_GITHUB_WEBHOOK_SECRET": "github-secret",
		"ENVOY_REVIEWER_APP_ID":       "1",
		"ENVOY_SLACK_SIGNING_SECRET":  "slack-secret",
		"ENVOY_HOST_BRIDGE":           "127.0.0.1",
	})
	read, err := readListenerSettings(env)
	if err != nil {
		t.Fatalf("readListenerSettings: %v", err)
	}
	if read.service.MachineID != "settings-test" || read.service.Port != 9021 || read.hostBridge != "127.0.0.1" {
		t.Fatalf("readListenerSettings read %+v, not the settings it was handed", read)
	}
	if read.webhooks.GitHub == nil || read.webhooks.Slack == nil || read.webhooks.GhostWispr == nil {
		t.Fatalf("readListenerSettings read webhooks %+v, want all three routes", read.webhooks)
	}
	client, err := bus.ConnectOwningStream(read.service.NATSURLs, bus.WithReplicas(read.service.NATSReplicas), bus.WithEnvironment(env.lookup))
	if err != nil {
		t.Fatalf("connect through the settings table: %v", err)
	}
	client.Close()
}
