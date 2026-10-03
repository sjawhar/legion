// packages/envoy/internal/broker/config/config.go
// Package config reads the broker's BROKER_* environment. A missing required variable, an
// unreadable or empty _FILE, a value out of range, both rules sources at once, or a removed
// variable refuses to start naming the variable.
//
// Each Config field's doc comment opens with the variables it reads and a colon; the broker's
// generated configuration reference (scripts/docs/broker/refgen) is built from those comments
// and refuses a variable Load reads that no field documents.
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
	// BROKER_LISTEN_ADDR: the host:port the broker listens on.
	ListenAddr string
	// BROKER_DATABASE_URL, BROKER_DATABASE_PASSWORD: the Postgres connection URL. Required. The
	// broker applies its own migrations at startup. A literal ${BROKER_DATABASE_PASSWORD} in the
	// URL is replaced with BROKER_DATABASE_PASSWORD, URL-escaped; setting either without the
	// other is refused.
	DatabaseURL string
	// BROKER_PUBLIC_URL: the broker's own address as its callers reach it, an absolute URL with no
	// path. Required. Every signed proof and request object names it, so a client's
	// AGENT_SECRETS_URL must be exactly this.
	PublicURL string
	// BROKER_UI_TOKEN, BROKER_UI_TOKEN_FILE: the bearer token Dispatch's server presents on the
	// broker's approval routes, given directly or as a file whose trimmed contents win over the
	// variable. Required. It vouches for the approver each decision names, so only Dispatch's
	// server may hold it.
	UIToken string
	// BROKER_RULES_FILE: a local rules file, for development; the broker then reads secret values
	// from BROKER_FAKE_SECRETS_FILE. Set exactly one of this and BROKER_RULES_S3_URI.
	RulesFile string
	// BROKER_RULES_S3_URI: the rules file as s3://<bucket>/<key>; the broker then reads secret
	// values from AWS Secrets Manager. Set exactly one of this and BROKER_RULES_FILE.
	RulesS3URI string
	// BROKER_RULES_RELOAD_SECONDS: how often the broker rereads the rules. A reload that does not
	// parse is logged and the previous rules stay in force.
	RulesReloadSeconds int
	// BROKER_K8S_OIDC_ISSUER: the issuer of the Kubernetes service-account tokens pods enroll
	// with. Set it with BROKER_K8S_OIDC_AUDIENCE, or neither, in which case no pod can enroll.
	K8sOIDCIssuer string
	// BROKER_K8S_OIDC_AUDIENCE: the audience a pod's projected service-account token must carry.
	K8sOIDCAudience string
	// BROKER_ENVOY_URL: an Envoy listener the broker notifies, best effort, when a session's
	// pending request expires undecided. Unset, the broker sends nothing.
	EnvoyURL string
	// BROKER_ENVOY_TOKEN, BROKER_ENVOY_TOKEN_FILE: the bearer token for BROKER_ENVOY_URL, given
	// directly or as a file whose trimmed contents win over the variable; read only when
	// BROKER_ENVOY_URL is set.
	EnvoyToken string
	// BROKER_LEASE_SECONDS: an enrollment's lease. A session renews it while it runs; one whose
	// lease runs out is no longer enrolled and its calls are refused.
	LeaseSeconds int
	// BROKER_PROOF_SKEW_SECONDS: how far the issue time of a signed proof or request object may
	// differ from the broker's clock.
	ProofSkewSeconds int
	// BROKER_MAX_GRANT_SECONDS: the longest a grant lives; a grant lives the shortest of this and
	// each granted secret's max_lifetime_seconds.
	MaxGrantSeconds int
	// BROKER_LAUNCHER_CREDENTIAL_SECONDS: how long a machine login's credential lasts once
	// approved; past it the machine logs in again, with a new key, a new code and a new approval.
	LauncherCredentialSeconds int
	// BROKER_SWEEP_SECONDS: how often the broker ends the enrollments whose lease lapsed and expires
	// the pending requests and machine logins nobody decided in time.
	SweepSeconds int
	// BROKER_TRUSTED_PROXY_HEADER: the request header (e.g. X-Forwarded-For) whose last entry the
	// machine-login rate limiter takes as the caller's address. Unset, it uses the connection's
	// own address, which is right only when nothing proxies the broker. Set it only when every
	// request passes through your own reverse proxy, which appends that entry; otherwise a caller
	// can forge the header and pick its own rate-limit bucket.
	TrustedProxyHeader string
}

// noDispatchCredential is why the broker's Dispatch variables are gone: it asks and issues nothing
// in Dispatch.
const noDispatchCredential = "the broker holds no Dispatch credential"

// removedVars are environment variables the broker no longer reads. A stale deployment still
// setting one must fail loudly rather than silently running on configuration that means nothing
// any more.
var removedVars = []struct{ name, reason string }{
	{"BROKER_DISPATCH_URL", noDispatchCredential},
	{"BROKER_DISPATCH_TOKEN", noDispatchCredential},
	{"BROKER_DISPATCH_TOKEN_FILE", noDispatchCredential},
	{"BROKER_DISPATCH_PROJECT", noDispatchCredential},
	{"BROKER_ASK_POLL_SECONDS", noDispatchCredential},
	{"BROKER_UI_ORIGIN", "approval is by Dispatch login, so the broker checks no WebAuthn origin"},
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
	for _, removed := range removedVars {
		if getenv(removed.name) != "" {
			return Config{}, fmt.Errorf("%s is removed; %s", removed.name, removed.reason)
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
		RulesFile:          getenv("BROKER_RULES_FILE"),
		RulesS3URI:         getenv("BROKER_RULES_S3_URI"),
		K8sOIDCIssuer:      getenv("BROKER_K8S_OIDC_ISSUER"),
		K8sOIDCAudience:    getenv("BROKER_K8S_OIDC_AUDIENCE"),
		EnvoyURL:           getenv("BROKER_ENVOY_URL"),
		TrustedProxyHeader: getenv("BROKER_TRUSTED_PROXY_HEADER"),
	}
	for _, req := range []struct{ name, value string }{
		{"BROKER_DATABASE_URL", cfg.DatabaseURL}, {"BROKER_PUBLIC_URL", cfg.PublicURL},
	} {
		if strings.TrimSpace(req.value) == "" {
			return Config{}, fmt.Errorf("%s is required", req.name)
		}
	}
	if parsed, err := url.Parse(cfg.PublicURL); err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" {
		return Config{}, fmt.Errorf("BROKER_PUBLIC_URL must be an absolute URL with no path: %q", cfg.PublicURL)
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
