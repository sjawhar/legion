package tsnet

import (
	"os"
	"strings"
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
	// Clear OAuth env vars by default so they don't leak between tests.
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", "")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", "")
	t.Setenv("ENVOY_TSNET_TAGS", "")
}

func setOAuthEnv(t *testing.T, enabled, hostname, stateDir, clientID, clientSecret, tags string) {
	t.Helper()
	t.Setenv("ENVOY_TSNET_ENABLED", enabled)
	t.Setenv("ENVOY_TSNET_HOSTNAME", hostname)
	t.Setenv("ENVOY_TSNET_STATE_DIR", stateDir)
	t.Setenv("ENVOY_TSNET_AUTH_KEY", "")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", clientID)
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", clientSecret)
	t.Setenv("ENVOY_TSNET_TAGS", tags)
}

// --- Disabled / basic tests (unchanged) ---

func TestLoadConfig_Disabled_DefaultEmpty(t *testing.T) {
	for _, key := range []string{
		"ENVOY_TSNET_ENABLED", "ENVOY_TSNET_HOSTNAME", "ENVOY_TSNET_STATE_DIR",
		"ENVOY_TSNET_AUTH_KEY", "ENVOY_TSNET_OAUTH_CLIENT_ID",
		"ENVOY_TSNET_OAUTH_CLIENT_SECRET", "ENVOY_TSNET_TAGS",
	} {
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

// --- Legacy auth key tests ---

func TestLoadConfig_LegacyAuthKey_AllFields(t *testing.T) {
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

func TestLoadConfig_LegacyAuthKey_Optional(t *testing.T) {
	setTsnetEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthKey != "" {
		t.Fatalf("expected empty auth key, got %q", cfg.AuthKey)
	}
}

// --- OAuth credential tests ---

func TestLoadConfig_OAuth_AllFields(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test",
		"tsid-client-abc123", "tskey-client-secret-xyz", "tag:envoy")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled")
	}
	expected := "tskey-client-secret-xyz?ephemeral=false&preauthorized=true&tags=tag:envoy"
	if cfg.AuthKey != expected {
		t.Fatalf("expected auth key %q, got %q", expected, cfg.AuthKey)
	}
}

func TestLoadConfig_OAuth_MultipleTags(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test",
		"tsid-client-abc123", "tskey-client-secret-xyz", "tag:envoy,tag:server")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "tskey-client-secret-xyz?ephemeral=false&preauthorized=true&tags=tag:envoy,tag:server"
	if cfg.AuthKey != expected {
		t.Fatalf("expected auth key %q, got %q", expected, cfg.AuthKey)
	}
}

func TestLoadConfig_OAuth_MissingClientID(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test",
		"", "tskey-client-secret-xyz", "tag:envoy")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing OAuth client ID")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_OAUTH_CLIENT_ID") {
		t.Fatalf("expected error about client ID, got: %s", err.Error())
	}
}

func TestLoadConfig_OAuth_MissingClientSecret(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test",
		"tsid-client-abc123", "", "tag:envoy")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing OAuth client secret")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_OAUTH_CLIENT_SECRET") {
		t.Fatalf("expected error about client secret, got: %s", err.Error())
	}
}

func TestLoadConfig_OAuth_MissingTags(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-listener-test", "/var/lib/tsnet/test",
		"tsid-client-abc123", "tskey-client-secret-xyz", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing tags")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_TAGS") {
		t.Fatalf("expected error about tags, got: %s", err.Error())
	}
}

func TestLoadConfig_OAuth_ConflictWithLegacyKey(t *testing.T) {
	t.Setenv("ENVOY_TSNET_ENABLED", "true")
	t.Setenv("ENVOY_TSNET_HOSTNAME", "envoy-listener-test")
	t.Setenv("ENVOY_TSNET_STATE_DIR", "/var/lib/tsnet/test")
	t.Setenv("ENVOY_TSNET_AUTH_KEY", "tskey-auth-test123")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", "tsid-client-abc123")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", "tskey-client-secret-xyz")
	t.Setenv("ENVOY_TSNET_TAGS", "tag:envoy")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for conflicting auth methods")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got: %s", err.Error())
	}
}

func TestLoadConfig_OAuth_WhitespaceHandling(t *testing.T) {
	setOAuthEnv(t, " true ", " envoy-test ", " /var/lib/test ",
		" tsid-client-abc ", " tskey-secret-xyz ", " tag:envoy ")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "tskey-secret-xyz?ephemeral=false&preauthorized=true&tags=tag:envoy"
	if cfg.AuthKey != expected {
		t.Fatalf("expected auth key %q, got %q", expected, cfg.AuthKey)
	}
}
