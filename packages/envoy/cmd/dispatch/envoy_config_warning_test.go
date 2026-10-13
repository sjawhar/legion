package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/config"
)

// The server starts on the OpenCode-era user file alone, and logs one WARN naming it, so the
// production log can count the deployments that still need the move.
func TestTheServerStartsOnTheOldUserFileWithAWarning(t *testing.T) {
	home := t.TempDir()
	oldPath := filepath.Join(home, ".config", "opencode", "envoy.json")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte(`{"dispatch": {"serverUrl": "https://old.dispatch.example"}, "natsUrls": ["nats://old:4222"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg, err := loadEnvoyConfig(envGetter(map[string]string{}), config.LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != "https://old.dispatch.example" || strings.Join(cfg.Envoy.NatsURLs, ",") != "nats://old:4222" {
		t.Errorf("got %+v (dispatch %+v), want the old file's values", cfg.Envoy, cfg.Dispatch)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `level=WARN msg="dispatch: read the OpenCode-era envoy.json" path=`+oldPath) {
		t.Errorf("log: got %q, want one WARN naming %s", logged.String(), oldPath)
	}
}
