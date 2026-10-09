// packages/envoy/internal/broker/config/config_test.go
package config

import (
	"maps"
	"os"
	"strconv"
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

// TestLoadReadsTheRegisteredServices pins BROKER_SERVICES: whitespace-separated
// name=<service-account subject> entries, none when unset. A name is one a machine login's service
// can take ([a-z0-9-]{1,64}) and not the owner tag's own "shared"; a subject is a Kubernetes
// service account's (system:serviceaccount:<namespace>:<name>), the only subject a pod's projected
// token can carry. An entry with no "=", an empty side, an invalid name or subject, or a name given
// twice refuses to start naming the entry, since the broker could never serve that service a secret
// or would have to pick one of two subjects.
func TestLoadReadsTheRegisteredServices(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceAccounts != nil {
		t.Fatalf("ServiceAccounts = %v, want nil when BROKER_SERVICES is unset", cfg.ServiceAccounts)
	}

	e := validEnv()
	e["BROKER_SERVICES"] = " example-service=system:serviceaccount:example:example-sa\tother-service=system:serviceaccount:other:other-sa\n"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"example-service": "system:serviceaccount:example:example-sa",
		"other-service":   "system:serviceaccount:other:other-sa",
	}
	if !maps.Equal(cfg.ServiceAccounts, want) {
		t.Fatalf("ServiceAccounts = %v, want %v", cfg.ServiceAccounts, want)
	}

	for _, bad := range []string{
		"other-service",
		"=system:serviceaccount:other:other-sa",
		"other-service=",
		"Other-Service=system:serviceaccount:other:other-sa",
		"other_service=system:serviceaccount:other:other-sa",
		"ada@example.com=system:serviceaccount:other:other-sa",
		"shared=system:serviceaccount:other:other-sa",
		strings.Repeat("a", 65) + "=system:serviceaccount:other:other-sa",
		"other-service=other-sa",
		"other-service=system:serviceaccount:other",
		"other-service=system:serviceaccount:Other:other-sa",
		"example-service=system:serviceaccount:example:another-sa",
	} {
		e := validEnv()
		e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa " + bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_SERVICES with %q: err = %v, want a refusal naming BROKER_SERVICES and the entry", bad, err)
		}
	}

	// One service account bound to two names would make a pod on it whichever service its login
	// claims, and a login's service name is the machine's own claim, so each account proves one
	// service only.
	e = validEnv()
	e["BROKER_SERVICES"] = "a=system:serviceaccount:x:y b=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") ||
		!strings.Contains(err.Error(), "system:serviceaccount:x:y") || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("BROKER_SERVICES binding one account to two names: err = %v, want a refusal naming the account and both names", err)
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

// rdsHost is an Amazon RDS cluster endpoint of the form the deployment's URL names.
const rdsHost = "example-cluster.cluster-abcdefghijkl.us-west-2.rds.amazonaws.com"

// rdsBundle is the RDS CA bundle the broker image ships, as the repository vendors it.
const rdsBundle = "../../../docker/rds-global-bundle.pem"

// verifiedRDSURL is an IAM-form URL that verifies the server: a user, no password, an RDS host,
// sslmode=verify-full and the bundle as sslrootcert.
const verifiedRDSURL = "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets?sslmode=verify-full&sslrootcert=" + rdsBundle

// isolatePostgresDefaults keeps the process's libpq defaults out of a test: pgx reads PG*
// variables and ~/.postgresql/root.crt beneath what the URL says, so a developer's own would
// change what a URL verifies.
func isolatePostgresDefaults(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(name, "")
	}
}

// TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL pins when the broker signs in to its database
// with an IAM token: a URL naming a user and no password whose host is an Amazon RDS endpoint.
// A password in the URL, the password placeholder, a URL naming no user, and a passwordless URL
// to any other host (local trust auth) connect as given, as they did before.
func TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL(t *testing.T) {
	isolatePostgresDefaults(t)
	for _, tc := range []struct {
		name, url, password string
		iam                 bool
	}{
		{name: "an RDS endpoint, a user and no password", url: verifiedRDSURL, iam: true},
		{name: "an RDS endpoint spelled in capitals", url: strings.Replace(verifiedRDSURL, rdsHost, strings.ToUpper(rdsHost), 1), iam: true},
		{name: "the user as a query parameter", url: "postgres://" + rdsHost + ":5432/agent_secrets?user=agent_secrets_broker&sslmode=verify-full&sslrootcert=" + rdsBundle, iam: true},
		{name: "a password in the URL", url: "postgres://agent_secrets:secret@" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a password as a query parameter", url: verifiedRDSURL + "&password=secret"},
		{name: "the password placeholder", url: "postgres://agent_secrets:${BROKER_DATABASE_PASSWORD}@" + rdsHost + ":5432/agent_secrets?sslmode=require", password: "p@ss"},
		{name: "no user", url: "postgres://" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a local passwordless URL", url: "postgres://broker@127.0.0.1:5432/broker?sslmode=disable"},
		{name: "a host that only starts with the RDS suffix", url: "postgres://broker@db.rds.amazonaws.com.example.net:5432/broker?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			if tc.password != "" {
				e["BROKER_DATABASE_PASSWORD"] = tc.password
			}
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabaseIAM != tc.iam {
				t.Fatalf("DatabaseIAM = %v for %s, want %v", cfg.DatabaseIAM, tc.url, tc.iam)
			}
		})
	}
}

// TestLoadRefusesAnIAMURLThatDoesNotVerifyTheServer pins that an IAM token, a password good for
// 15 minutes, never goes over a link that has not verified the server: an IAM-form URL that pgx
// would not read as one host with sslmode=verify-full and a root certificate pool from
// sslrootcert refuses to start, naming the host and not the URL. sslmode=require encrypts and
// verifies nothing, and a repeated sslmode takes its last value, as libpq does.
func TestLoadRefusesAnIAMURLThatDoesNotVerifyTheServer(t *testing.T) {
	isolatePostgresDefaults(t)
	base := "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets"
	for name, tc := range map[string]struct{ url, reason string }{
		"sslmode=require":                          {base + "?sslmode=require", "its sslmode is not verify-full"},
		"sslmode=require with sslrootcert":         {base + "?sslmode=require&sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"no sslmode":                               {base + "?sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"sslmode=disable":                          {base + "?sslmode=disable", "its sslmode is not verify-full"},
		"sslmode=verify-ca":                        {base + "?sslmode=verify-ca&sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"verify-full without sslrootcert":          {base + "?sslmode=verify-full", "it names no sslrootcert"},
		"a later sslmode undoing verify-full":      {verifiedRDSURL + "&sslmode=require", "its sslmode is not verify-full"},
		"a second host":                            {"postgres://agent_secrets_broker@" + rdsHost + ":5432,other.example.net:5432/agent_secrets?sslmode=verify-full&sslrootcert=" + rdsBundle, "it names another host too"},
		"an sslrootcert that names no file":        {base + "?sslmode=verify-full&sslrootcert=/nonexistent/rds-bundle.pem", "unable to read CA file"},
		"an sslrootcert that holds no certificate": {base + "?sslmode=verify-full&sslrootcert=config_test.go", "unable to add CA to cert pool"},
	} {
		t.Run(name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			cfg, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load accepted %s (DatabaseIAM %v); want a refusal naming the host", tc.url, cfg.DatabaseIAM)
			}
			for _, want := range []string{"BROKER_DATABASE_URL", rdsHost, tc.reason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "postgres://") {
				t.Errorf("refusal %q quotes the URL; it should name the host alone", err)
			}
		})
	}
}
