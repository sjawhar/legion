package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// `legion handoffs import --help` prints its usage and every flag, and exits 0.
func TestHandoffsImportHelpPrintsItsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoffs", "import", "--help"}, &out, &errb)
	if code != 0 || !strings.Contains(errb.String(), "usage: legion handoffs import [--config <legion.yaml>] [--dry-run]") ||
		!strings.Contains(errb.String(), "-dry-run") || !strings.Contains(errb.String(), "-config") {
		t.Fatalf("exit code = %d, stderr %q; want 0 and the usage with both flags", code, errb.String())
	}
}

// `legion handoffs` names its one subcommand, and an argument to the import is refused as start
// refuses it: the configuration is named only by --config.
func TestHandoffsRefusesAMissingSubcommandAndAnArgument(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoffs"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: legion handoffs import [flags]") {
		t.Fatalf("exit code = %d, stderr %q; want 2 and the subcommand usage", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"legion", "handoffs", "import", "legion.yaml"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unexpected argument "legion.yaml"`) {
		t.Fatalf("exit code = %d, stderr %q; want 2 naming the argument", code, errb.String())
	}
}

// A configuration that cannot be read is refused by name, before anything else is read.
func TestHandoffsImportRefusesAConfigurationItCannotRead(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoffs", "import", "--config", "/nonexistent/legion.yaml"}, &out, &errb)
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), "legion handoffs import: ") || !strings.Contains(errb.String(), "/nonexistent/legion.yaml") {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1 naming the file", code, out.String(), errb.String())
	}
}
