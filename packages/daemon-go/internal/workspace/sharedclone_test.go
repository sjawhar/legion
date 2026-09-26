package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colorRunner runs every command through runner under a jj user configuration that colors all
// output, as an operator's `ui.color = "always"` does.
type colorRunner struct {
	Runner
	config string
}

func newColorRunner(t *testing.T, runner Runner) *colorRunner {
	t.Helper()
	config := filepath.Join(t.TempDir(), "color.toml")
	if err := os.WriteFile(config, []byte("[ui]\ncolor = \"always\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &colorRunner{Runner: runner, config: config}
}

func (r *colorRunner) Run(ctx context.Context, command Command) (Result, error) {
	command.Env = append(append([]string(nil), command.Env...), "JJ_CONFIG="+r.config)
	return r.Runner.Run(ctx, command)
}

// Every jj read provisioning and removal parse runs uncolored, whatever the jj configuration
// asks: a commit id wrapped in color codes is not a commit id. Under `ui.color = "always"` the
// issue's workspace is provisioned, removed, and provisioned again at its bookmark, and the second
// provisioning writes no jj setting the first one already wrote.
func TestProvisioningParsesUncoloredReadsUnderColorAlways(t *testing.T) {
	local := newLocalRunner(t)
	run := newColorRunner(t, local)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	commit := strings.TrimSpace(runSetup(t, first.Clone, "jj", "log", "-r", first.Bookmark, "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never"))
	if err := Remove(context.Background(), run, first); err != nil {
		t.Fatalf("remove: %v", err)
	}
	before := len(local.Calls())
	second, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if got := strings.TrimSpace(runSetup(t, second.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--color=never")); got != commit {
		t.Errorf("the second workspace starts at %s, want the bookmark's %s", got, commit)
	}
	for _, call := range local.Calls()[before:] {
		if commandWith(call.Argv, "jj", "config", "set") {
			t.Errorf("the second provision wrote a setting the first had written: %q", call.Argv)
		}
	}
}
