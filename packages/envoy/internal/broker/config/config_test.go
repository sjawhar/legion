// packages/envoy/internal/broker/config/config_test.go
package config

import (
	"os"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// validEnv is a complete, minimal valid environment: every test starts from a copy of this and
// overrides or deletes just the variable(s) under test.
func validEnv() map[string]string {
	return map[string]string{
		"BROKER_DATABASE_URL":        "postgres://x",
		"BROKER_PUBLIC_URL":          "https://secrets.internal.example",
		"BROKER_UI_TOKEN":            "ui-token",
		"BROKER_SECRETS_PREFIX":      "example/agent-secrets/",
		"BROKER_SECRETS_KMS_KEY_ARN": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	e := validEnv()
	delete(e, "BROKER_DATABASE_URL")
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "BROKER_DATABASE_URL") {
		t.Fatalf("expected BROKER_DATABASE_URL refusal, got %v", err)
	}
}

// TestLoadRefusesDispatchVariables pins that the removed Dispatch variables fail loudly rather
// than being silently ignored: the broker holds no Dispatch credential, so a stale deployment env
// still setting one must refuse to start, naming the variable.
func TestLoadRefusesDispatchVariables(t *testing.T) {
	e := validEnv()
	e["BROKER_DISPATCH_URL"] = "https://dispatch.internal.example"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_DISPATCH_URL is removed; ") {
		t.Fatalf("Load(BROKER_DISPATCH_URL set) = %v, want a refusal naming BROKER_DISPATCH_URL", err)
	}
}

// TestLoadRefusesUIOrigin pins that BROKER_UI_ORIGIN, the WebAuthn origin approval by Dispatch
// login removed, fails loudly rather than being silently ignored: a deployment still setting it
// was built for the key-signature design and must be updated, not run half-migrated.
func TestLoadRefusesUIOrigin(t *testing.T) {
	e := validEnv()
	e["BROKER_UI_ORIGIN"] = "https://secrets-ui.internal.example"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_UI_ORIGIN is removed; ") {
		t.Fatalf("Load(BROKER_UI_ORIGIN set) = %v, want a refusal naming BROKER_UI_ORIGIN", err)
	}
}

func TestLoadRequiresUIToken(t *testing.T) {
	e := validEnv()
	delete(e, "BROKER_UI_TOKEN")
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_UI_TOKEN") {
		t.Fatalf("expected BROKER_UI_TOKEN refusal, got %v", err)
	}
}

func TestLoadReadsUITokenFileAheadOfValue(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/token"
	if err := os.WriteFile(path, []byte("  tok-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := validEnv()
	e["BROKER_UI_TOKEN_FILE"] = path
	e["BROKER_UI_TOKEN"] = "ignored"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIToken != "tok-from-file" {
		t.Fatalf("token file must win: got %d-byte token, want the %d-byte file value", len(cfg.UIToken), len("tok-from-file"))
	}
	if cfg.LeaseSeconds != 900 || cfg.ProofSkewSeconds != 60 || cfg.MaxGrantSeconds != 43200 ||
		cfg.LauncherCredentialSeconds != 604800 || cfg.SweepSeconds != 5 {
		t.Fatalf("defaults wrong: LeaseSeconds=%d ProofSkewSeconds=%d MaxGrantSeconds=%d LauncherCredentialSeconds=%d SweepSeconds=%d",
			cfg.LeaseSeconds, cfg.ProofSkewSeconds, cfg.MaxGrantSeconds, cfg.LauncherCredentialSeconds, cfg.SweepSeconds)
	}
}

// TestLoadReadsTrustedProxyHeaderOptionally pins that BROKER_TRUSTED_PROXY_HEADER is optional
// (unset means "" — the rate limiter keys on r.RemoteAddr) and, when set, passes through
// verbatim as the header name to trust.
func TestLoadReadsTrustedProxyHeaderOptionally(t *testing.T) {
	base := validEnv()
	cfg, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxyHeader != "" {
		t.Fatalf("TrustedProxyHeader = %q, want empty when BROKER_TRUSTED_PROXY_HEADER is unset", cfg.TrustedProxyHeader)
	}

	withHeader := validEnv()
	withHeader["BROKER_TRUSTED_PROXY_HEADER"] = "X-Forwarded-For"
	cfg, err = Load(env(withHeader))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxyHeader != "X-Forwarded-For" {
		t.Fatalf("TrustedProxyHeader = %q, want X-Forwarded-For", cfg.TrustedProxyHeader)
	}
}

// TestLoadRefusesTheRulesFileVariables pins that a deployment still configured for the rules file
// refuses to start naming the variable, rather than running with its rules silently ignored.
func TestLoadRefusesTheRulesFileVariables(t *testing.T) {
	for name, value := range map[string]string{
		"BROKER_RULES_FILE":           "/r.yaml",
		"BROKER_RULES_S3_URI":         "s3://bucket/agent-secret-rules.yaml",
		"BROKER_RULES_RELOAD_SECONDS": "300",
	} {
		e := validEnv()
		e[name] = value
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), name+" is removed; ") || !strings.Contains(err.Error(), "tags") {
			t.Fatalf("Load(%s set) = %v, want a refusal naming it and the tags that replace it", name, err)
		}
	}
}

// TestLoadRequiresTheNamespaceAndItsKey pins that the broker refuses to start without the
// namespace it serves and the key every secret in it must be on, or with either in a form the
// loader cannot use: a prefix that is not a name prefix ending in "/", or a key named any way but
// by its ARN (an alias can be repointed, and a bare key id names no account).
func TestLoadRequiresTheNamespaceAndItsKey(t *testing.T) {
	for name, value := range map[string]string{
		"BROKER_SECRETS_PREFIX":      "",
		"BROKER_SECRETS_KMS_KEY_ARN": "",
	} {
		e := validEnv()
		e[name] = value
		if _, err := Load(env(e)); err == nil || err.Error() != name+" is required" {
			t.Fatalf("Load(%s unset) = %v, want %q", name, err, name+" is required")
		}
	}
	for _, prefix := range []string{"example/agent-secrets", "/example/agent-secrets/", "example/agent secrets/"} {
		e := validEnv()
		e["BROKER_SECRETS_PREFIX"] = prefix
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_PREFIX must be") {
			t.Fatalf("Load(BROKER_SECRETS_PREFIX=%q) = %v, want a refusal naming it", prefix, err)
		}
	}
	for _, key := range []string{"alias/agent-secrets", "1234abcd-12ab-34cd-56ef-1234567890ab", "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets"} {
		e := validEnv()
		e["BROKER_SECRETS_KMS_KEY_ARN"] = key
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_KMS_KEY_ARN must be") {
			t.Fatalf("Load(BROKER_SECRETS_KMS_KEY_ARN=%q) = %v, want a refusal naming it", key, err)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.SecretsPrefix != "example/agent-secrets/" || cfg.SecretsKMSKeyARN != validEnv()["BROKER_SECRETS_KMS_KEY_ARN"] {
		t.Fatalf("Load(valid) = %+v, %v", cfg, err)
	}
}

// TestDatabasePasswordPlaceholderSubstitutesEscaped pins the ${BROKER_DATABASE_PASSWORD}
// substitution: the password is URL-escaped in place, so a password containing reserved URL
// characters (@, :, /) is carried correctly rather than corrupting the URL's own structure.
func TestDatabasePasswordPlaceholderSubstitutesEscaped(t *testing.T) {
	e := validEnv()
	e["BROKER_DATABASE_URL"] = "postgres://broker:${BROKER_DATABASE_PASSWORD}@db.internal:5432/agent_secrets"
	e["BROKER_DATABASE_PASSWORD"] = "p@ss:w/rd"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://broker:p%40ss%3Aw%2Frd@db.internal:5432/agent_secrets"
	if cfg.DatabaseURL != want {
		t.Fatalf("DatabaseURL = %q, want %q", cfg.DatabaseURL, want)
	}
}

func TestDatabasePasswordPlaceholderWithoutVariableRefused(t *testing.T) {
	e := validEnv()
	e["BROKER_DATABASE_URL"] = "postgres://broker:${BROKER_DATABASE_PASSWORD}@db.internal:5432/agent_secrets"
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "BROKER_DATABASE_PASSWORD is not set") {
		t.Fatalf("expected a refusal naming the missing password, got %v", err)
	}
}

func TestDatabasePasswordVariableWithoutPlaceholderRefused(t *testing.T) {
	e := validEnv()
	e["BROKER_DATABASE_PASSWORD"] = "unused"
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Fatalf("expected a refusal naming the unused password, got %v", err)
	}
}
