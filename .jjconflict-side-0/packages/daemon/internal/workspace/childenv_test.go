package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// git and jj, which run in a managed repository's checkout, start without either raw NATS seed the
// daemon holds.
func TestWorkspaceCommandsGetNoNATSSeed(t *testing.T) {
	t.Setenv("NATS_NKEY_SEED", "pane-seed-value")
	t.Setenv("NATS_DAEMON_NKEY_SEED", "daemon-seed-value")
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewRunner(10*time.Second, map[string]string{"git": git}).Run(context.Background(), Command{Argv: []string{"git", "status"}, Dir: t.TempDir(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(result.Stdout, "seed-value") || !strings.Contains(result.Stdout, "PATH=") {
		t.Errorf("git's environment carries a NATS seed (or is not the daemon's):\n%s", result.Stdout)
	}
}
