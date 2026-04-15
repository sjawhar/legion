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
	// Clear OAuth vars by default
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", "")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", "")
	t.Setenv("ENVOY_TSNET_TAGS", "")
}

func setOAuthEnv(t *testing.T, enabled, hostname, stateDir, clientID, clientSecret, tags string) {
	t.Helper()
	for _, kv := range []struct{ key, val string }{
		{"ENVOY_TSNET_ENABLED", enabled},
		{"ENVOY_TSNET_HOSTNAME", hostname},
		{"ENVOY_TSNET_STATE_DIR", stateDir},
		{"ENVOY_TSNET_OAUTH_CLIENT_ID", clientID},
		{"ENVOY_TSNET_OAUTH_CLIENT_SECRET", clientSecret},
		{"ENVOY_TSNET_TAGS", tags},
		{"ENVOY_TSNET_AUTH_KEY", ""},
	} {
		t.Setenv(kv.key, kv.val)
	}
}

// --- Disabled tests ---

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

// --- Legacy auth key tests ---

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

// --- OAuth tests ---

func TestLoadConfig_OAuth_HappyPath(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"client-id-123", "tskey-client-secret-abc", "tag:envoy")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled")
	}
	// AuthKey should be the constructed OAuth key
	if !strings.HasPrefix(cfg.AuthKey, "tskey-client-secret-abc?") {
		t.Fatalf("expected auth key to start with OAuth secret, got %q", cfg.AuthKey)
	}
	if !strings.Contains(cfg.AuthKey, "ephemeral=false") {
		t.Fatalf("expected ephemeral=false in auth key, got %q", cfg.AuthKey)
	}
	if !strings.Contains(cfg.AuthKey, "preauthorized=true") {
		t.Fatalf("expected preauthorized=true in auth key, got %q", cfg.AuthKey)
	}
	if strings.Contains(cfg.AuthKey, "tags=") {
		t.Fatalf("tags should not be in auth key URL, got %q", cfg.AuthKey)
	}
	if len(cfg.Tags) != 1 || cfg.Tags[0] != "tag:envoy" {
		t.Fatalf("expected Tags=[tag:envoy], got %v", cfg.Tags)
	}
}

func TestLoadConfig_OAuth_MultipleTags(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"client-id-123", "tskey-client-secret-abc", "tag:envoy,tag:legion")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tags) != 2 || cfg.Tags[0] != "tag:envoy" || cfg.Tags[1] != "tag:legion" {
		t.Fatalf("expected Tags=[tag:envoy, tag:legion], got %v", cfg.Tags)
	}
}

func TestLoadConfig_OAuth_TagsWhitespace(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"client-id-123", "tskey-client-secret-abc", " tag:envoy , tag:legion ")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tags) != 2 || cfg.Tags[0] != "tag:envoy" || cfg.Tags[1] != "tag:legion" {
		t.Fatalf("expected trimmed Tags=[tag:envoy, tag:legion], got %v", cfg.Tags)
	}
}

func TestLoadConfig_OAuth_MissingSecret(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"client-id-123", "", "tag:envoy")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing OAuth secret")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_OAUTH_CLIENT_SECRET is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_OAuth_MissingClientID(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"", "tskey-client-secret-abc", "tag:envoy")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing OAuth client ID")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_OAUTH_CLIENT_ID is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_OAuth_MissingTags(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		"client-id-123", "tskey-client-secret-abc", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing tags")
	}
	if !strings.Contains(err.Error(), "ENVOY_TSNET_TAGS is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_OAuth_ConflictWithLegacy(t *testing.T) {
	t.Setenv("ENVOY_TSNET_ENABLED", "true")
	t.Setenv("ENVOY_TSNET_HOSTNAME", "envoy-test")
	t.Setenv("ENVOY_TSNET_STATE_DIR", "/var/lib/test")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", "client-id-123")
	t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", "tskey-client-secret-abc")
	t.Setenv("ENVOY_TSNET_TAGS", "tag:envoy")
	t.Setenv("ENVOY_TSNET_AUTH_KEY", "tskey-auth-legacy")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for OAuth + legacy conflict")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_OAuth_WhitespaceInCredentials(t *testing.T) {
	setOAuthEnv(t, "true", "envoy-test", "/var/lib/test",
		" client-id-123 ", " tskey-secret-abc ", " tag:envoy ")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(cfg.AuthKey, "tskey-secret-abc?") {
		t.Fatalf("expected trimmed OAuth secret as prefix, got %q", cfg.AuthKey)
	}
}
