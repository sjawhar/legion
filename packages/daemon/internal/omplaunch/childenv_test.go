package omplaunch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `mise where` starts without either raw NATS seed the daemon holds.
func TestMiseWhereGetsNoNATSSeed(t *testing.T) {
	t.Setenv("NATS_NKEY_SEED", "pane-seed-value")
	t.Setenv("NATS_DAEMON_NKEY_SEED", "daemon-seed-value")
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	mise := filepath.Join(dir, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nenv >"+dump+"\necho "+dir+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = resolveMiseOmp(mise, "github:acme/omp@1") // the tool dir has no omp; the environment is the point
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("mise never ran: %v", err)
	}
	if strings.Contains(string(seen), "seed-value") || !strings.Contains(string(seen), "PATH=") {
		t.Errorf("mise where's environment carries a NATS seed (or is not the daemon's):\n%s", seen)
	}
}
