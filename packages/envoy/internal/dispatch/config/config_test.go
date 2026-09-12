package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	keys := []string{"DISPATCH_SERVER_URL", "NATS_URLS"}
	previous := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		previous[key], present[key] = os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	for _, key := range keys {
		if present[key] {
			if err := os.Setenv(key, previous[key]); err != nil {
				panic(err)
			}
		} else if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	os.Exit(code)
}

func TestLoadMergesUserAndRepo(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"natsUrls": ["nats://user:4222"],
		"dispatch": {"enabled": true, "serverUrl": "https://user.example"}
	}`)
	mustWrite(t, filepath.Join(cwd, ".opencode", "envoy.json"), `{
		"dispatch": {"serverUrl": "https://repo.example"}
	}`)
	cfg, err := Load(LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil {
		t.Fatalf("missing dispatch block")
	}
	if cfg.Dispatch.ServerURL != "https://repo.example" {
		t.Errorf("serverUrl: got %q", cfg.Dispatch.ServerURL)
	}
	if !cfg.Dispatch.Enabled {
		t.Errorf("enabled should remain true (fell through from user config)")
	}
	if len(cfg.NatsURLs) != 1 || cfg.NatsURLs[0] != "nats://user:4222" {
		t.Errorf("natsUrls: %+v", cfg.NatsURLs)
	}
}

func TestLoadEnvironmentOverridesMergedConfig(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"natsUrls": ["nats://user:4222"],
		"dispatch": {"enabled": true, "serverUrl": "https://user.example"}
	}`)
	mustWrite(t, filepath.Join(cwd, ".opencode", "envoy.json"), `{
		"natsUrls": ["nats://repo:4222"],
		"dispatch": {"serverUrl": "https://repo.example"}
	}`)

	for _, tc := range []struct {
		name        string
		environment map[string]string
		serverURL   string
		natsURLs    []string
	}{
		{
			name:      "no environment preserves merged config",
			serverURL: "https://repo.example",
			natsURLs:  []string{"nats://repo:4222"},
		},
		{
			name: "environment overrides both files",
			environment: map[string]string{
				"DISPATCH_SERVER_URL": "https://environment.example",
				"NATS_URLS":           "nats://environment-one:4222, nats://environment-two:4222",
			},
			serverURL: "https://environment.example",
			natsURLs:  []string{"nats://environment-one:4222", "nats://environment-two:4222"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.environment {
				t.Setenv(key, value)
			}
			cfg, err := Load(LoadOptions{CWD: cwd, HomeDir: home})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != tc.serverURL {
				t.Fatalf("dispatch.serverUrl: got %+v, want %q", cfg.Dispatch, tc.serverURL)
			}
			if strings.Join(cfg.NatsURLs, ",") != strings.Join(tc.natsURLs, ",") {
				t.Errorf("natsUrls: got %q, want %q", cfg.NatsURLs, tc.natsURLs)
			}
		})
	}
}

func TestLoadRejectsInvalidEnvironmentOverrides(t *testing.T) {
	for _, tc := range []struct {
		name        string
		environment map[string]string
		variable    string
	}{
		{name: "empty server URL", environment: map[string]string{"DISPATCH_SERVER_URL": ""}, variable: "DISPATCH_SERVER_URL"},
		{name: "non-http scheme", environment: map[string]string{"DISPATCH_SERVER_URL": "ftp://dispatch.example"}, variable: "DISPATCH_SERVER_URL"},
		{name: "userinfo", environment: map[string]string{"DISPATCH_SERVER_URL": "https://user@dispatch.example"}, variable: "DISPATCH_SERVER_URL"},
		{name: "query", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example?next=oauth"}, variable: "DISPATCH_SERVER_URL"},
		{name: "fragment", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example#oauth"}, variable: "DISPATCH_SERVER_URL"},
		{name: "path", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example/auth/callback"}, variable: "DISPATCH_SERVER_URL"},
		{name: "empty NATS URLs", environment: map[string]string{"NATS_URLS": ""}, variable: "NATS_URLS"},
		{name: "empty NATS URL element", environment: map[string]string{"NATS_URLS": "nats://one:4222, "}, variable: "NATS_URLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.environment {
				t.Setenv(key, value)
			}
			_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.variable)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("error: got %q, want %s", err, tc.variable)
			}
		})
	}
}

func TestLoadRejectsRemovedDefaultRepoKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"defaultRepo": "user/repo"}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.defaultRepo key")
	}
	if !strings.Contains(err.Error(), "dispatch.defaultRepo") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsRemovedAppClientIDKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"appClientId": "user-id"}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.appClientId key")
	}
	if !strings.Contains(err.Error(), "dispatch.appClientId") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsInvalidServerURL(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"serverUrl": ""}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for an invalid serverUrl")
	}
}

func TestLoadMissingFilesReturnEmpty(t *testing.T) {
	cfg, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected non-nil config")
	}
	if cfg.Dispatch != nil {
		t.Errorf("expected nil dispatch, got %+v", cfg.Dispatch)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
