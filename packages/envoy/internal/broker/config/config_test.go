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

func TestLoadRequiresDatabaseAndDispatch(t *testing.T) {
	_, err := Load(env(map[string]string{"BROKER_PUBLIC_URL": "https://secrets.dev1.internal.trajectorylabs.com"}))
	if err == nil || !strings.Contains(err.Error(), "BROKER_DATABASE_URL") {
		t.Fatalf("expected BROKER_DATABASE_URL refusal, got %v", err)
	}
}

func TestLoadReadsTokenFileAheadOfValue(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/token"
	if err := os.WriteFile(path, []byte("  tok-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env(map[string]string{
		"BROKER_DATABASE_URL":        "postgres://x",
		"BROKER_PUBLIC_URL":          "https://secrets.dev1.internal.trajectorylabs.com",
		"BROKER_DISPATCH_URL":        "https://dispatch.dev1.internal.trajectorylabs.com",
		"BROKER_DISPATCH_TOKEN_FILE": path,
		"BROKER_DISPATCH_TOKEN":      "ignored",
		"BROKER_DISPATCH_PROJECT":    "AGENTC",
		"BROKER_RULES_FILE":          dir + "/rules.yaml",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DispatchToken != "tok-from-file" {
		t.Fatalf("token file must win: %q", cfg.DispatchToken)
	}
	if cfg.LeaseSeconds != 900 || cfg.AskPollSeconds != 5 || cfg.ProofSkewSeconds != 60 || cfg.MaxGrantSeconds != 43200 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadRefusesBothRulesSources(t *testing.T) {
	_, err := Load(env(map[string]string{
		"BROKER_DATABASE_URL": "postgres://x", "BROKER_PUBLIC_URL": "https://s", "BROKER_DISPATCH_URL": "https://d",
		"BROKER_DISPATCH_TOKEN": "t", "BROKER_DISPATCH_PROJECT": "AGENTC",
		"BROKER_RULES_FILE": "/r.yaml", "BROKER_RULES_S3_URI": "s3://dev1-agent-secrets-rules/rules.yaml",
	}))
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("expected exactly-one refusal, got %v", err)
	}
}
