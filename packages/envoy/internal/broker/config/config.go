// packages/envoy/internal/broker/config/config.go
// Package config reads the broker's BROKER_* environment. A missing required variable, an
// unreadable file, or two sources for one value refuses to start naming the variable.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/oidc"
)

type Config struct {
	ListenAddr         string
	DatabaseURL        string
	PublicURL          string
	DispatchURL        string
	DispatchToken      string
	DispatchProject    string
	RulesFile          string
	RulesS3URI         string
	RulesReloadSeconds int
	K8sOIDCIssuer      string
	K8sOIDCAudience    string
	EnvoyURL           string
	EnvoyToken         string
	LeaseSeconds       int
	AskPollSeconds     int
	ProofSkewSeconds   int
	MaxGrantSeconds    int
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:      orDefault(getenv("BROKER_LISTEN_ADDR"), "127.0.0.1:13380"),
		DatabaseURL:     getenv("BROKER_DATABASE_URL"),
		PublicURL:       getenv("BROKER_PUBLIC_URL"),
		DispatchURL:     getenv("BROKER_DISPATCH_URL"),
		DispatchProject: getenv("BROKER_DISPATCH_PROJECT"),
		RulesFile:       getenv("BROKER_RULES_FILE"),
		RulesS3URI:      getenv("BROKER_RULES_S3_URI"),
		K8sOIDCIssuer:   getenv("BROKER_K8S_OIDC_ISSUER"),
		K8sOIDCAudience: getenv("BROKER_K8S_OIDC_AUDIENCE"),
		EnvoyURL:        getenv("BROKER_ENVOY_URL"),
	}
	for _, req := range []struct{ name, value string }{
		{"BROKER_DATABASE_URL", cfg.DatabaseURL}, {"BROKER_PUBLIC_URL", cfg.PublicURL},
		{"BROKER_DISPATCH_URL", cfg.DispatchURL}, {"BROKER_DISPATCH_PROJECT", cfg.DispatchProject},
	} {
		if strings.TrimSpace(req.value) == "" {
			return Config{}, fmt.Errorf("%s is required", req.name)
		}
	}
	for _, u := range []struct{ name, value string }{
		{"BROKER_PUBLIC_URL", cfg.PublicURL}, {"BROKER_DISPATCH_URL", cfg.DispatchURL},
	} {
		parsed, err := url.Parse(u.value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" {
			return Config{}, fmt.Errorf("%s must be an absolute URL with no path: %q", u.name, u.value)
		}
	}
	if (cfg.RulesFile == "") == (cfg.RulesS3URI == "") {
		return Config{}, fmt.Errorf("exactly one of BROKER_RULES_FILE and BROKER_RULES_S3_URI must be set")
	}
	if cfg.RulesS3URI != "" && !strings.HasPrefix(cfg.RulesS3URI, "s3://") {
		return Config{}, fmt.Errorf("BROKER_RULES_S3_URI must be s3://<bucket>/<key>, got %q", cfg.RulesS3URI)
	}
	issuer, audience, err := oidc.ConfigFromEnv(getenv, "BROKER_K8S_OIDC_ISSUER", "BROKER_K8S_OIDC_AUDIENCE")
	if err != nil {
		return Config{}, err
	}
	cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience = issuer, audience
	token, err := secretValue(getenv, "BROKER_DISPATCH_TOKEN")
	if err != nil {
		return Config{}, err
	}
	if token == "" {
		return Config{}, fmt.Errorf("BROKER_DISPATCH_TOKEN_FILE or BROKER_DISPATCH_TOKEN is required")
	}
	cfg.DispatchToken = token
	if cfg.EnvoyURL != "" {
		if cfg.EnvoyToken, err = secretValue(getenv, "BROKER_ENVOY_TOKEN"); err != nil {
			return Config{}, err
		}
	}
	ints := []struct {
		name string
		dst  *int
		def  int
		max  int
	}{
		{"BROKER_LEASE_SECONDS", &cfg.LeaseSeconds, 900, 3600},
		{"BROKER_ASK_POLL_SECONDS", &cfg.AskPollSeconds, 5, 60},
		{"BROKER_PROOF_SKEW_SECONDS", &cfg.ProofSkewSeconds, 60, 300},
		{"BROKER_MAX_GRANT_SECONDS", &cfg.MaxGrantSeconds, 43200, 43200},
		{"BROKER_RULES_RELOAD_SECONDS", &cfg.RulesReloadSeconds, 300, 3600},
	}
	for _, i := range ints {
		raw := getenv(i.name)
		if raw == "" {
			*i.dst = i.def
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > i.max {
			return Config{}, fmt.Errorf("%s must be a whole number between 1 and %d, got %q", i.name, i.max, raw)
		}
		*i.dst = n
	}
	return cfg, nil
}

// secretValue reads NAME_FILE (trimmed contents) ahead of NAME; a set-but-unreadable or blank
// file is an error naming both, never a fallback.
func secretValue(getenv func(string) string, name string) (string, error) {
	if path := getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE names %s, which could not be read: %w", name, path, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("%s_FILE names %s, which is empty", name, path)
		}
		return value, nil
	}
	return getenv(name), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
