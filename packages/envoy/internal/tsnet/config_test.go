package tsnet

import (
	"os"
	"testing"
)

func setTsnetEnv(t *testing.T, enabled, hostname, stateDir, authKey string) {
	t.Helper()
	for _, kv := range []struct{ key, val string }{
		{"ENVOY_TSNET_ENABLED", enabled},
		{"ENVOY_TSNET_HOSTNAME", hostname},
		{"ENVOY_TSNET_STATE_DIR", stateDir},
		{"ENVOY_TSNET_AUTH_KEY", authKey},
	} {
		t.Setenv(kv.key, kv.val)
	}
}

func TestLoadConfig_Disabled_DefaultEmpty(t *testing.T) {
	// All env vars unset — should default to disabled.
	for _, key := range []string{"ENVOY_TSNET_ENABLED", "ENVOY_TSNET_HOSTNAME", "ENVOY_TSNET_STATE_DIR", "ENVOY_TSNET_AUTH_KEY"} {
		os.Unsetenv(key)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled when env empty")
	}
}

func TestLoadConfig_Disabled_ExplicitFalse(t *testing.T) {
	setTsnetEnv(t, "false", "", "", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled with explicit false")
	}
}

func TestLoadConfig_Disabled_Zero(t *testing.T) {
	setTsnetEnv(t, "0", "", "", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled with 0")
	}
}

func TestLoadConfig_Enabled_AllFields(t *testing.T) {
	setTsnetEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test", "tskey-auth-test123")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled")
	}
	if cfg.Hostname != "envoy-listener-test" {
		t.Fatalf("expected hostname 'envoy-listener-test', got %q", cfg.Hostname)
	}
	if cfg.StateDir != "/var/lib/tsnet/test" {
		t.Fatalf("expected state dir '/var/lib/tsnet/test', got %q", cfg.StateDir)
	}
	if cfg.AuthKey != "tskey-auth-test123" {
		t.Fatalf("expected auth key 'tskey-auth-test123', got %q", cfg.AuthKey)
	}
}

func TestLoadConfig_Enabled_One(t *testing.T) {
	setTsnetEnv(t, "1", "envoy-listener-test", "/var/lib/tsnet/test", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled with 1")
	}
}

func TestLoadConfig_Enabled_AuthKeyOptional(t *testing.T) {
	setTsnetEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthKey != "" {
		t.Fatalf("expected empty auth key, got %q", cfg.AuthKey)
	}
}

func TestLoadConfig_Enabled_MissingHostname(t *testing.T) {
	setTsnetEnv(t, "true", "", "/var/lib/tsnet/test", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing hostname")
	}
	if got := err.Error(); got != "ENVOY_TSNET_HOSTNAME is required when ENVOY_TSNET_ENABLED=true" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestLoadConfig_Enabled_MissingStateDir(t *testing.T) {
	setTsnetEnv(t, "true", "envoy-listener-test", "", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing state dir")
	}
	if got := err.Error(); got != "ENVOY_TSNET_STATE_DIR is required when ENVOY_TSNET_ENABLED=true" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestLoadConfig_InvalidEnabledValue(t *testing.T) {
	setTsnetEnv(t, "yes", "host", "/dir", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid enabled value")
	}
}

func TestLoadConfig_WhitespaceHandling(t *testing.T) {
	setTsnetEnv(t, " true ", " envoy-test ", " /var/lib/test ", " tskey-123 ")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Hostname != "envoy-test" {
		t.Fatalf("expected trimmed hostname, got %q", cfg.Hostname)
	}
	if cfg.StateDir != "/var/lib/test" {
		t.Fatalf("expected trimmed state dir, got %q", cfg.StateDir)
	}
	if cfg.AuthKey != "tskey-123" {
		t.Fatalf("expected trimmed auth key, got %q", cfg.AuthKey)
	}
}
