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
	UIOrigin           string
	UIToken            string
	RulesFile          string
	RulesS3URI         string
	RulesReloadSeconds int
	K8sOIDCIssuer      string
	K8sOIDCAudience    string
	EnvoyURL           string
	EnvoyToken         string
	LeaseSeconds       int
	ProofSkewSeconds   int
	MaxGrantSeconds    int
	// LauncherCredentialSeconds is a machine login's minted launcher credential lifetime
	// (BROKER_LAUNCHER_CREDENTIAL_SECONDS).
	LauncherCredentialSeconds int
	// SweepSeconds is the Sweeper's tick interval (BROKER_SWEEP_SECONDS).
	SweepSeconds int
	// TrustedProxyHeader is the request header the launcher-credential rate limiter trusts for
	// the caller's real address (BROKER_TRUSTED_PROXY_HEADER), e.g. "X-Forwarded-For". Empty (the
	// default) means the broker is reached directly, so it keys on r.RemoteAddr as before. Set it
	// only when every request truly passes through your own trusted reverse proxy first —
	// otherwise a caller can forge the header and pick its own rate-limit bucket.
	TrustedProxyHeader string
}

// removedVars are AGENTC-393 v9's removed Dispatch environment variables: the broker holds no
// Dispatch credential and asks/issues nothing, so a stale deployment still setting one of these
// must fail loudly rather than silently running with a Dispatch dependency it no longer has.
var removedVars = []string{
	"BROKER_DISPATCH_URL",
	"BROKER_DISPATCH_TOKEN",
	"BROKER_DISPATCH_TOKEN_FILE",
	"BROKER_DISPATCH_PROJECT",
	"BROKER_ASK_POLL_SECONDS",
}

// databasePasswordPlaceholder is substituted in BROKER_DATABASE_URL with the URL-escaped value of
// BROKER_DATABASE_PASSWORD, so the password itself never has to be pre-escaped by whoever sets the
// URL. Naming the placeholder without the variable, or the variable without the placeholder, is
// refused naming both: a silently-unsubstituted placeholder would try to connect to a literal
// "${BROKER_DATABASE_PASSWORD}" password, and a silently-unused password is a stale, ignored
// configuration entry.
const databasePasswordPlaceholder = "${BROKER_DATABASE_PASSWORD}"

func substituteDatabasePassword(rawURL string, getenv func(string) string) (string, error) {
	password := getenv("BROKER_DATABASE_PASSWORD")
	hasPlaceholder := strings.Contains(rawURL, databasePasswordPlaceholder)
	switch {
	case hasPlaceholder && password == "":
		return "", fmt.Errorf("BROKER_DATABASE_URL names %s but BROKER_DATABASE_PASSWORD is not set", databasePasswordPlaceholder)
	case !hasPlaceholder && password != "":
		return "", fmt.Errorf("BROKER_DATABASE_PASSWORD is set but BROKER_DATABASE_URL does not name %s", databasePasswordPlaceholder)
	case hasPlaceholder:
		return strings.ReplaceAll(rawURL, databasePasswordPlaceholder, url.QueryEscape(password)), nil
	default:
		return rawURL, nil
	}
}

func Load(getenv func(string) string) (Config, error) {
	for _, name := range removedVars {
		if getenv(name) != "" {
			return Config{}, fmt.Errorf("%s is removed; the broker holds no Dispatch credential (AGENTC-393 v9)", name)
		}
	}
	databaseURL, err := substituteDatabasePassword(getenv("BROKER_DATABASE_URL"), getenv)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:         orDefault(getenv("BROKER_LISTEN_ADDR"), "127.0.0.1:13380"),
		DatabaseURL:        databaseURL,
		PublicURL:          getenv("BROKER_PUBLIC_URL"),
		UIOrigin:           getenv("BROKER_UI_ORIGIN"),
		RulesFile:          getenv("BROKER_RULES_FILE"),
		RulesS3URI:         getenv("BROKER_RULES_S3_URI"),
		K8sOIDCIssuer:      getenv("BROKER_K8S_OIDC_ISSUER"),
		K8sOIDCAudience:    getenv("BROKER_K8S_OIDC_AUDIENCE"),
		EnvoyURL:           getenv("BROKER_ENVOY_URL"),
		TrustedProxyHeader: getenv("BROKER_TRUSTED_PROXY_HEADER"),
	}
	for _, req := range []struct{ name, value string }{
		{"BROKER_DATABASE_URL", cfg.DatabaseURL}, {"BROKER_PUBLIC_URL", cfg.PublicURL}, {"BROKER_UI_ORIGIN", cfg.UIOrigin},
	} {
		if strings.TrimSpace(req.value) == "" {
			return Config{}, fmt.Errorf("%s is required", req.name)
		}
	}
	for _, u := range []struct{ name, value string }{
		{"BROKER_PUBLIC_URL", cfg.PublicURL}, {"BROKER_UI_ORIGIN", cfg.UIOrigin},
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
	token, err := secretValue(getenv, "BROKER_UI_TOKEN")
	if err != nil {
		return Config{}, err
	}
	if token == "" {
		return Config{}, fmt.Errorf("BROKER_UI_TOKEN_FILE or BROKER_UI_TOKEN is required")
	}
	cfg.UIToken = token
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
		{"BROKER_PROOF_SKEW_SECONDS", &cfg.ProofSkewSeconds, 60, 300},
		{"BROKER_MAX_GRANT_SECONDS", &cfg.MaxGrantSeconds, 43200, 43200},
		{"BROKER_RULES_RELOAD_SECONDS", &cfg.RulesReloadSeconds, 300, 3600},
		{"BROKER_LAUNCHER_CREDENTIAL_SECONDS", &cfg.LauncherCredentialSeconds, 604800, 2592000},
		{"BROKER_SWEEP_SECONDS", &cfg.SweepSeconds, 5, 60},
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
