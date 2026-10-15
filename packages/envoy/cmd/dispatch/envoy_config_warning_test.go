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

func writeConfigFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The server refuses to start on the OpenCode-era user file alone, naming both paths and the move,
// whatever DISPATCH_SERVER_URL and NATS_URLS say.
func TestTheServerRefusesTheOldUserFile(t *testing.T) {
	home := t.TempDir()
	oldPath := filepath.Join(home, ".config", "opencode", "envoy.json")
	userPath := filepath.Join(home, ".config", "legion", "envoy.json")
	writeConfigFile(t, oldPath, `{"dispatch": {"serverUrl": "https://old.dispatch.example"}, "natsUrls": ["nats://old:4222"]}`)

	for _, values := range []map[string]string{
		{},
		{"DISPATCH_SERVER_URL": "https://environment.dispatch.example", "NATS_URLS": "nats://environment:4222"},
	} {
		cfg, err := loadEnvoyConfig(envGetter(values), config.LoadOptions{CWD: t.TempDir(), HomeDir: home})
		want := oldPath + " is not read: mkdir -p " + filepath.Dir(userPath) + " && mv " + oldPath + " " + userPath
		if err == nil || err.Error() != want {
			t.Errorf("with %v: got %+v, %v; want the refusal %q", values, cfg, err, want)
		}
	}
}

// A checkout's .opencode/envoy.json is not read: the server starts on the user file and logs one
// WARN naming the rename.
func TestTheServerWarnsOfTheOldCheckoutFile(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	oldPath := filepath.Join(cwd, ".opencode", "envoy.json")
	writeConfigFile(t, filepath.Join(home, ".config", "legion", "envoy.json"), `{"dispatch": {"serverUrl": "https://user.dispatch.example"}}`)
	writeConfigFile(t, oldPath, `{"dispatch": {"serverUrl": "https://old.dispatch.example"}, "natsUrls": ["nats://old:4222"]}`)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg, err := loadEnvoyConfig(envGetter(map[string]string{}), config.LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != "https://user.dispatch.example" || cfg.Envoy.NatsURLs != nil {
		t.Errorf("got %+v (dispatch %+v), want the user file's values alone", cfg.Envoy, cfg.Dispatch)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	move := oldPath + " is not read: mkdir -p " + filepath.Join(cwd, ".envoy") + " && mv " + oldPath + " " + filepath.Join(cwd, ".envoy", "envoy.json")
	if len(lines) != 1 || !strings.Contains(lines[0], `level=WARN msg="dispatch: a checkout's .opencode/envoy.json is not read" path=`+oldPath+` fix="`+move+`"`) {
		t.Errorf("log: got %q, want one WARN naming %s and the rename", logged.String(), oldPath)
	}
}
