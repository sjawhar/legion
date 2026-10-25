// packages/envoy/internal/broker/config/config_test.go
package config

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/secrets"
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
		"BROKER_MACHINE_LOGIN_URL":   "https://dispatch.internal.example/credentials/machine",
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

// TestLoadRequiresMachineLoginURL: the page a person types a machine login's code into is
// required, and must be an absolute http(s) URL, since the device grant names it to the machine.
func TestLoadRequiresMachineLoginURL(t *testing.T) {
	for _, v := range []string{"", "/credentials/machine", "dispatch.example/credentials/machine", "ftp://dispatch.example/x", "https:///x"} {
		e := validEnv()
		e["BROKER_MACHINE_LOGIN_URL"] = v
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_MACHINE_LOGIN_URL") {
			t.Errorf("Load(BROKER_MACHINE_LOGIN_URL=%q) = %v, want a refusal naming it", v, err)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.MachineLoginURL != "https://dispatch.internal.example/credentials/machine" {
		t.Fatalf("Load = (%q, %v), want the machine login URL", cfg.MachineLoginURL, err)
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

// TestLoadRefusesMaxGrantSeconds pins that deployments cannot silently turn their explicit grant
// cap into a session-length grant after that setting is removed.
func TestLoadRefusesMaxGrantSeconds(t *testing.T) {
	e := validEnv()
	e["BROKER_MAX_GRANT_SECONDS"] = "3600"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_MAX_GRANT_SECONDS is removed; ") {
		t.Fatalf("Load(BROKER_MAX_GRANT_SECONDS set) = %v, want a refusal naming BROKER_MAX_GRANT_SECONDS", err)
	}
}

// TestLoadRefusesTheUIToken pins that the removed shared bearer fails loudly, by either name:
// the routes it authenticated take the calling service's own ServiceAccount token now, so a
// deployment still setting it is one whose callers were never listed.
func TestLoadRefusesTheUIToken(t *testing.T) {
	for _, name := range []string{"BROKER_UI_TOKEN", "BROKER_UI_TOKEN_FILE"} {
		e := validEnv()
		e[name] = "whatever"
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), name+" is removed; ") || !strings.Contains(err.Error(), "BROKER_API_ACCOUNTS") {
			t.Fatalf("Load(%s set) = %v, want a refusal naming %s and BROKER_API_ACCOUNTS", name, err, name)
		}
	}
}

// TestLoadDefaultsTheIntegerSettings pins every seconds setting's default, the values a
// deployment that sets none runs on.
func TestLoadDefaultsTheIntegerSettings(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaseSeconds != 900 || cfg.ProofSkewSeconds != 60 || cfg.AccessTokenSeconds != 900 ||
		cfg.LauncherCredentialSeconds != 2592000 || cfg.SweepSeconds != 5 || cfg.DrainSeconds != 5 {
		t.Fatalf("defaults wrong: LeaseSeconds=%d ProofSkewSeconds=%d AccessTokenSeconds=%d LauncherCredentialSeconds=%d SweepSeconds=%d DrainSeconds=%d",
			cfg.LeaseSeconds, cfg.ProofSkewSeconds, cfg.AccessTokenSeconds, cfg.LauncherCredentialSeconds, cfg.SweepSeconds, cfg.DrainSeconds)
	}
}

// TestLoadReadsTheDrainSeconds pins BROKER_DRAIN_SECONDS, the wait the broker serves out after
// SIGTERM while answering /healthz with 503: 0 is allowed, since a local run has no EndpointSlice
// to propagate and the wait would be pure delay, and a value past two minutes, a negative one or
// one that is not a whole number refuses to start naming the variable, rather than leaving the
// drain at a length nobody chose.
func TestLoadReadsTheDrainSeconds(t *testing.T) {
	for _, good := range []struct {
		value string
		want  int
	}{{"0", 0}, {"30", 30}, {"120", 120}} {
		e := validEnv()
		e["BROKER_DRAIN_SECONDS"] = good.value
		cfg, err := Load(env(e))
		if err != nil || cfg.DrainSeconds != good.want {
			t.Fatalf("Load(BROKER_DRAIN_SECONDS=%s) = %d, %v; want %d", good.value, cfg.DrainSeconds, err, good.want)
		}
	}
	for _, bad := range []string{"121", "-1", "five", "5s", "5.0", " "} {
		e := validEnv()
		e["BROKER_DRAIN_SECONDS"] = bad
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_DRAIN_SECONDS must be a whole number between 0 and 120") {
			t.Fatalf("Load(BROKER_DRAIN_SECONDS=%q) = %v, want a refusal naming BROKER_DRAIN_SECONDS and its range", bad, err)
		}
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
// name=<service-account subject> entries keyed by subject, none when unset. A name is one a
// machine login's service can take ([a-z0-9-]{1,64}) and not the owner tag's own "shared"; a
// subject is a Kubernetes service account's (system:serviceaccount:<namespace>:<name>), the only
// subject a pod's projected token can carry. One service may be named for several accounts. An
// entry with no "=", an empty side, an invalid name or subject, or an account given twice refuses
// to start naming the entry or the account, since the broker could never serve that service a
// secret or the account would prove more than one entry.
func TestLoadReadsTheRegisteredServices(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServicesByAccount != nil {
		t.Fatalf("ServicesByAccount = %v, want nil when BROKER_SERVICES is unset", cfg.ServicesByAccount)
	}

	e := validEnv()
	e["BROKER_SERVICES"] = " example-service=system:serviceaccount:example:example-sa\tother-service=system:serviceaccount:other:other-sa\n"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:other:other-sa":     "other-service",
	}
	if !maps.Equal(cfg.ServicesByAccount, want) {
		t.Fatalf("ServicesByAccount = %v, want %v", cfg.ServicesByAccount, want)
	}

	// One service may run its pods as several accounts, each of which proves it.
	e = validEnv()
	e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa example-service=system:serviceaccount:example:another-sa"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:example:another-sa": "example-service",
	}
	if !maps.Equal(cfg.ServicesByAccount, want) {
		t.Fatalf("ServicesByAccount with two accounts for one service = %v, want %v", cfg.ServicesByAccount, want)
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

	// The same account named twice for one service is a mistake in the setting, refused naming the
	// account and the service.
	e = validEnv()
	e["BROKER_SERVICES"] = "a=system:serviceaccount:x:y a=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES names the service account system:serviceaccount:x:y twice for \"a\"; each account proves one service, so name it once") {
		t.Errorf("BROKER_SERVICES naming one account twice for one service: err = %v, want a refusal naming the account twice for the service", err)
	}

	// A namespace's default service account proves nothing: every pod in that namespace whose spec
	// names no account runs as it, and any such pod can mount a projected token for the broker's
	// audience, so binding a service to it would hand the service's secrets to all of them.
	for _, entry := range []string{"legion-daemon=system:serviceaccount:legion:default", "x=system:serviceaccount:kube-system:default"} {
		e = validEnv()
		e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa " + entry
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") || !strings.Contains(err.Error(), strconv.Quote(entry)) ||
			!strings.Contains(err.Error(), "default service account") {
			t.Errorf("BROKER_SERVICES with %q: err = %v, want a refusal naming the entry and that it is a namespace's default service account", entry, err)
		}
	}
	// An account merely named like it is a dedicated account of its own.
	e = validEnv()
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:default-worker other=system:serviceaccount:default:other-sa"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("BROKER_SERVICES naming dedicated accounts whose name or namespace contains default: %v", err)
	}
}

// TestLoadReadsTheServiceLogins pins BROKER_SERVICE_LOGINS: whitespace-separated
// <service>=<ServiceAccount subject> entries keyed by subject, none when unset; one service may have
// several accounts, an account is named once and exactly (no wildcard), and the setting needs a
// Kubernetes token issuer to trust.
func TestLoadReadsTheServiceLogins(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceLogins != nil {
		t.Fatalf("ServiceLogins = %v, want nil when BROKER_SERVICE_LOGINS is unset", cfg.ServiceLogins)
	}
	withIssuer := func() map[string]string {
		e := validEnv()
		e["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
		e["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
		return e
	}
	e := withIssuer()
	e["BROKER_SERVICE_LOGINS"] = " legion-daemon=system:serviceaccount:legion-system:legion-daemon-a\nlegion-daemon=system:serviceaccount:legion-system:legion-daemon-b "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system:serviceaccount:legion-system:legion-daemon-a": "legion-daemon",
		"system:serviceaccount:legion-system:legion-daemon-b": "legion-daemon",
	}
	if !maps.Equal(cfg.ServiceLogins, want) {
		t.Fatalf("ServiceLogins = %v, want %v", cfg.ServiceLogins, want)
	}

	for _, bad := range []string{
		"legion-daemon", "=system:serviceaccount:a:b", "shared=system:serviceaccount:a:b", "Legion=system:serviceaccount:a:b",
		"legion-daemon=system:serviceaccount:legion-system:*", "legion-daemon=system:serviceaccount:*:legion-daemon", "legion-daemon=legion-daemon-a",
	} {
		e := withIssuer()
		e["BROKER_SERVICE_LOGINS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICE_LOGINS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_SERVICE_LOGINS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withIssuer()
	e["BROKER_SERVICE_LOGINS"] = "a=system:serviceaccount:x:y b=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_SERVICE_LOGINS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	// A namespace's default account proves nothing, as BROKER_SERVICES refuses it, and the account
	// BROKER_SERVICES binds for a service's pods is one every such pod holds a token for, so neither
	// logs a service in.
	e = withIssuer()
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:default"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICE_LOGINS") || !strings.Contains(err.Error(), "default service account") {
		t.Errorf("BROKER_SERVICE_LOGINS naming a default account: err = %v, want a refusal naming the setting and the default account", err)
	}
	e = withIssuer()
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:legion-worker"
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:legion-daemon-a legion-daemon=system:serviceaccount:legion:legion-worker"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICE_LOGINS") || !strings.Contains(err.Error(), `"legion-daemon=system:serviceaccount:legion:legion-worker"`) || !strings.Contains(err.Error(), "BROKER_SERVICES") {
		t.Errorf("BROKER_SERVICE_LOGINS naming the pods' account: err = %v, want a refusal naming the entry and BROKER_SERVICES", err)
	}
	e = withIssuer()
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:legion-worker"
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:legion-daemon-a"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("BROKER_SERVICE_LOGINS beside a BROKER_SERVICES binding another account: %v", err)
	}
	e = validEnv()
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:legion-daemon-a"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_SERVICE_LOGINS with no token issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
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
	if err != nil || cfg.SecretsPrefix != "example/agent-secrets/" || cfg.SecretsKMSKey != validEnv()["BROKER_SECRETS_KMS_KEY_ARN"] {
		t.Fatalf("Load(valid) = %+v, %v", cfg, err)
	}
}

// TestLoadReadsTheSecretStore pins BROKER_SECRETS_STORE: aws-secrets-manager when unset, so
// every deployment from before the variable is unchanged; gcp-secret-manager with its own prefix
// form (the project by its number, a secret-id prefix ending in "-") and BROKER_SECRETS_KMS_KEY_NAME
// in place of the ARN; each store refusing the other store's key variable, so a deployment moved
// between stores cannot keep the old key set and mean nothing by it; and any other name refused
// naming both.
func TestLoadReadsTheSecretStore(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.SecretsStore != secrets.StoreAWS || cfg.SecretsKMSKey != validEnv()["BROKER_SECRETS_KMS_KEY_ARN"] {
		t.Fatalf("Load(unset store) = %+v, %v; want %s", cfg, err, secrets.StoreAWS)
	}

	gcp := func() map[string]string {
		e := validEnv()
		e["BROKER_SECRETS_STORE"] = secrets.StoreGCP
		e["BROKER_SECRETS_PREFIX"] = "projects/000000000000/secrets/agent-secrets-"
		delete(e, "BROKER_SECRETS_KMS_KEY_ARN")
		e["BROKER_SECRETS_KMS_KEY_NAME"] = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
		return e
	}
	cfg, err = Load(env(gcp()))
	if err != nil || cfg.SecretsStore != secrets.StoreGCP || cfg.SecretsPrefix != "projects/000000000000/secrets/agent-secrets-" || cfg.SecretsKMSKey != gcp()["BROKER_SECRETS_KMS_KEY_NAME"] {
		t.Fatalf("Load(gcp) = %+v, %v", cfg, err)
	}

	e := gcp()
	e["BROKER_MINT_TARGETS"] = "ANY_OLD_VALUE"
	if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_MINT_TARGETS is removed; ") {
		t.Fatalf("Load(BROKER_MINT_TARGETS) = %v, want a refusal naming the removed setting", err)
	}

	for name, tc := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"an unknown store": {func(e map[string]string) { e["BROKER_SECRETS_STORE"] = "vault" }, "BROKER_SECRETS_STORE must be aws-secrets-manager or gcp-secret-manager"},
		"gcp with the ARN set": {func(e map[string]string) {
			e["BROKER_SECRETS_KMS_KEY_ARN"] = validEnv()["BROKER_SECRETS_KMS_KEY_ARN"]
		}, "BROKER_SECRETS_KMS_KEY_ARN is not read with BROKER_SECRETS_STORE=gcp-secret-manager; set BROKER_SECRETS_KMS_KEY_NAME instead"},
		"gcp with no key name": {func(e map[string]string) { delete(e, "BROKER_SECRETS_KMS_KEY_NAME") }, "BROKER_SECRETS_KMS_KEY_NAME is required"},
		"gcp with a project id": {func(e map[string]string) {
			e["BROKER_SECRETS_PREFIX"] = "projects/example-project/secrets/agent-secrets-"
		}, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with an AWS prefix": {func(e map[string]string) { e["BROKER_SECRETS_PREFIX"] = "example/agent-secrets/" }, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with no trailing dash": {func(e map[string]string) {
			e["BROKER_SECRETS_PREFIX"] = "projects/000000000000/secrets/agent-secrets"
		}, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with a regional key": {func(e map[string]string) {
			e["BROKER_SECRETS_KMS_KEY_NAME"] = "projects/example-project/locations/us-west1/keyRings/r/cryptoKeys/k"
		}, "BROKER_SECRETS_KMS_KEY_NAME: a Cloud KMS key name is projects/"},
	} {
		e := gcp()
		tc.edit(e)
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("Load(%s) = %v, want a refusal opening %q", name, err, tc.want)
		}
	}

	aws := validEnv()
	aws["BROKER_SECRETS_KMS_KEY_NAME"] = gcp()["BROKER_SECRETS_KMS_KEY_NAME"]
	if _, err := Load(env(aws)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_KMS_KEY_NAME is not read with BROKER_SECRETS_STORE=aws-secrets-manager; set BROKER_SECRETS_KMS_KEY_ARN instead") {
		t.Fatalf("Load(aws with the key name set) = %v, want a refusal naming both", err)
	}
}

// TestLoadRefusesTheDatabasePassword pins that the broker substitutes no database password into
// its URL: a deployment still setting BROKER_DATABASE_PASSWORD, one whose BROKER_DATABASE_URL still
// names the ${BROKER_DATABASE_PASSWORD} placeholder, and one doing both refuse to start, naming the
// variable and why (on RDS the broker signs in by IAM token, and a URL to any other database
// carries its own password), and never print the password or the URL.
func TestLoadRefusesTheDatabasePassword(t *testing.T) {
	const password = "s3cret-database-password"
	const placeholderURL = "postgres://broker:${BROKER_DATABASE_PASSWORD}@db.internal.example:5432/broker"
	for _, tc := range []struct {
		name, url, password, opens string
	}{
		{name: "the variable alone", url: "postgres://broker:" + password + "@db.internal.example:5432/broker", password: password, opens: "BROKER_DATABASE_PASSWORD is removed; "},
		{name: "the placeholder alone", url: placeholderURL, opens: "BROKER_DATABASE_URL names the removed ${BROKER_DATABASE_PASSWORD} placeholder; "},
		{name: "both", url: placeholderURL, password: password, opens: "BROKER_DATABASE_PASSWORD is removed; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			if tc.password != "" {
				e["BROKER_DATABASE_PASSWORD"] = tc.password
			}
			_, err := Load(env(e))
			if err == nil {
				t.Fatal("Load accepted it; want a refusal naming BROKER_DATABASE_PASSWORD and why it is gone")
			}
			refusal := err.Error()
			if !strings.HasPrefix(refusal, tc.opens) {
				t.Errorf("refusal %q does not open %q", refusal, tc.opens)
			}
			for _, why := range []string{"by IAM token", "https://sjawhar.github.io/legion/broker/operate/#signing-in-to-rds-by-iam-token", "goes in the URL itself"} {
				if !strings.Contains(refusal, why) {
					t.Errorf("refusal %q does not say %q", refusal, why)
				}
			}
			for _, secret := range []string{password, "db.internal.example"} {
				if strings.Contains(refusal, secret) {
					t.Errorf("refusal %q prints %q, part of the password or the URL", refusal, secret)
				}
			}
		})
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
// A password in the URL, a URL naming no user, and a passwordless URL to any other host (local
// trust auth) connect as given, with the password, if any, the URL or libpq's defaults supply.
func TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL(t *testing.T) {
	isolatePostgresDefaults(t)
	for _, tc := range []struct {
		name, url string
		iam       bool
	}{
		{name: "an RDS endpoint, a user and no password", url: verifiedRDSURL, iam: true},
		{name: "an RDS endpoint spelled in capitals", url: strings.Replace(verifiedRDSURL, rdsHost, strings.ToUpper(rdsHost), 1), iam: true},
		{name: "the user as a query parameter", url: "postgres://" + rdsHost + ":5432/agent_secrets?user=agent_secrets_broker&sslmode=verify-full&sslrootcert=" + rdsBundle, iam: true},
		{name: "a password in the URL", url: "postgres://agent_secrets:secret@" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a password as a query parameter", url: verifiedRDSURL + "&password=secret"},
		{name: "a password with a semicolon, which net/url drops from the query and pgx reads", url: verifiedRDSURL + "&password=sec;ret"},
		{name: "no user", url: "postgres://" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a local passwordless URL", url: "postgres://broker@127.0.0.1:5432/broker?sslmode=disable"},
		{name: "a host with the RDS suffix in the middle of its name", url: "postgres://broker@db.rds.amazonaws.com.example.net:5432/broker?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
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

// TestLoadLeavesAPasswordLibpqSuppliesToSignIn pins that a passwordless RDS URL whose password
// comes from libpq's defaults, PGPASSWORD or a passfile, which pgx reads beneath the URL, signs in
// with that password rather than a token.
func TestLoadLeavesAPasswordLibpqSuppliesToSignIn(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"PGPASSWORD": func(t *testing.T) { t.Setenv("PGPASSWORD", "from-the-environment") },
		"a passfile": func(t *testing.T) {
			passfile := t.TempDir() + "/pgpass"
			if err := os.WriteFile(passfile, []byte("*:*:*:agent_secrets_broker:from-the-passfile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PGPASSFILE", passfile)
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolatePostgresDefaults(t)
			setup(t)
			e := validEnv()
			e["BROKER_DATABASE_URL"] = verifiedRDSURL
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabaseIAM {
				t.Fatalf("DatabaseIAM = true with a password from %s; pgx would sign in with that password", name)
			}
		})
	}
}

// TestLoadRefusesPGSSLROOTCERTSystemForAnIAMURL pins that the system pool named through
// PGSSLROOTCERT, which pgx reads beneath the URL, is refused as the URL's own sslrootcert=system
// is: it holds no RDS CA, so every sign-in would fail its TLS handshake after a clean boot.
func TestLoadRefusesPGSSLROOTCERTSystemForAnIAMURL(t *testing.T) {
	isolatePostgresDefaults(t)
	t.Setenv("PGSSLROOTCERT", "system")
	e := validEnv()
	e["BROKER_DATABASE_URL"] = "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets?sslmode=verify-full"
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), rdsHost) || !strings.Contains(err.Error(), "sslrootcert=system") {
		t.Fatalf("Load with PGSSLROOTCERT=system = %v, want a refusal naming the host and sslrootcert=system", err)
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
		"sslrootcert=system":                       {base + "?sslmode=verify-full&sslrootcert=system", "sslrootcert=system"},
		"sslrootcert=system under sslmode=require": {base + "?sslmode=require&sslrootcert=system", "sslrootcert=system"},
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

// withK8sIssuer is validEnv with the cluster issuer every ServiceAccount token setting needs.
func withK8sIssuer() map[string]string {
	e := validEnv()
	e["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
	e["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
	return e
}

// TestLoadReadsTheAPIAccounts pins BROKER_API_ACCOUNTS: whitespace-separated <caller>=<account>
// entries keyed by account, none when unset, and refusals naming the entry for one that is not
// of that form, a namespace's default account and an account named twice. It needs the cluster
// issuer, since the broker verifies those callers' tokens against it.
func TestLoadReadsTheAPIAccounts(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAccounts != nil {
		t.Fatalf("APIAccounts = %v, want nil when BROKER_API_ACCOUNTS is unset", cfg.APIAccounts)
	}
	e := withK8sIssuer()
	e["BROKER_API_ACCOUNTS"] = " dispatch=system:serviceaccount:example-ns:example-dispatch\nlistener=system:serviceaccount:example-ns:example-listener "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system:serviceaccount:example-ns:example-dispatch": "dispatch",
		"system:serviceaccount:example-ns:example-listener": "listener",
	}
	if !maps.Equal(cfg.APIAccounts, want) {
		t.Fatalf("APIAccounts = %v, want %v", cfg.APIAccounts, want)
	}
	for _, bad := range []string{
		"dispatch", "=system:serviceaccount:a:b", "Dispatch=system:serviceaccount:a:b",
		"dispatch=example-dispatch", "dispatch=system:serviceaccount:example-ns:default",
	} {
		e := withK8sIssuer()
		e["BROKER_API_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_API_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_API_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withK8sIssuer()
	e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:x:y listener=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_API_ACCOUNTS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	e = validEnv()
	e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_API_ACCOUNTS with no token issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadReadsTheAccessTokenLifetime pins BROKER_ACCESS_TOKEN_SECONDS: whole seconds, 900 by
// default, and a refusal naming the variable and its range for anything else.
func TestLoadReadsTheAccessTokenLifetime(t *testing.T) {
	e := validEnv()
	e["BROKER_ACCESS_TOKEN_SECONDS"] = "30"
	cfg, err := Load(env(e))
	if err != nil || cfg.AccessTokenSeconds != 30 {
		t.Fatalf("Load(BROKER_ACCESS_TOKEN_SECONDS=30) = (%d, %v), want 30", cfg.AccessTokenSeconds, err)
	}
	for _, bad := range []string{"0", "3601", "fifteen", "-1", "900.0"} {
		e := validEnv()
		e["BROKER_ACCESS_TOKEN_SECONDS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_ACCESS_TOKEN_SECONDS") {
			t.Errorf("BROKER_ACCESS_TOKEN_SECONDS = %q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}

// TestLoadReadsTheExchangeAccounts pins BROKER_EXCHANGE_ACCOUNTS: whitespace-separated
// <account>=<project>=<person>[=<session>] entries in order, none when unset, a session that may
// itself carry "=", and refusals naming the entry for one that is not of that form, has a blank
// field, names a namespace's default account or lists an account twice. It needs the cluster
// issuer and the exchange audience, whose tokens its pods present.
func TestLoadReadsTheExchangeAccounts(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeAccounts != nil {
		t.Fatalf("ExchangeAccounts = %v, want nil when BROKER_EXCHANGE_ACCOUNTS is unset", cfg.ExchangeAccounts)
	}
	withExchange := func() map[string]string {
		e := withK8sIssuer()
		e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	e := withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com\n" +
		" system:serviceaccount:example-ns:example-daemon=EXAMPLE=ada@example.com=legion-daemon:EXAMPLE=1 "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.AccountEntry{
		{Account: "system:serviceaccount:example-ns:example-worker", Project: "EXAMPLE", Person: "ada@example.com"},
		{Account: "system:serviceaccount:example-ns:example-daemon", Project: "EXAMPLE", Person: "ada@example.com", Session: "legion-daemon:EXAMPLE=1"},
	}
	if !slices.Equal(cfg.ExchangeAccounts, want) {
		t.Fatalf("ExchangeAccounts = %v, want %v", cfg.ExchangeAccounts, want)
	}
	for _, bad := range []string{
		"system:serviceaccount:example-ns:example-worker", "system:serviceaccount:example-ns:example-worker=EXAMPLE",
		"system:serviceaccount:example-ns:example-worker==ada@example.com", "=EXAMPLE=ada@example.com",
		"example-worker=EXAMPLE=ada@example.com", "system:serviceaccount:example-ns:default=EXAMPLE=ada@example.com",
	} {
		e := withExchange()
		e["BROKER_EXCHANGE_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:x:y=A=ada@example.com system:serviceaccount:x:y=B=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS with no exchange audience: err = %v, want a refusal naming BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
	e = validEnv()
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_K8S_AUDIENCE with no cluster issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadReadsTheExchangeWorkflows pins BROKER_EXCHANGE_WORKFLOWS:
// <repository>=<workflow>=<ref>=<project>=<person>[=<session>] entries in order, none when unset,
// and refusals naming the entry for one that is not of that form or lists a workflow twice. It
// needs GitHub's issuer and audience, whose tokens its runs present.
func TestLoadReadsTheExchangeWorkflows(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeWorkflows != nil {
		t.Fatalf("ExchangeWorkflows = %v, want nil when BROKER_EXCHANGE_WORKFLOWS is unset", cfg.ExchangeWorkflows)
	}
	withGitHub := func() map[string]string {
		e := validEnv()
		e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
		e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	e := withGitHub()
	e["BROKER_EXCHANGE_WORKFLOWS"] = "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE=ada@example.com=release=1"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.WorkflowEntry{{
		Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main",
		Project: "EXAMPLE", Person: "ada@example.com", Session: "release=1",
	}}
	if !slices.Equal(cfg.ExchangeWorkflows, want) {
		t.Fatalf("ExchangeWorkflows = %v, want %v", cfg.ExchangeWorkflows, want)
	}
	for _, bad := range []string{
		"example-org/example-repo", "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE",
		"example-org/example-repo=.github/workflows/release.yaml==EXAMPLE=ada@example.com",
	} {
		e := withGitHub()
		e["BROKER_EXCHANGE_WORKFLOWS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_WORKFLOWS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_WORKFLOWS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withGitHub()
	entry := "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main"
	e["BROKER_EXCHANGE_WORKFLOWS"] = entry + "=A=ada@example.com " + entry + "=B=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), entry) {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS naming one workflow twice: err = %v, want a refusal naming the workflow", err)
	}
	e = validEnv()
	e["BROKER_EXCHANGE_WORKFLOWS"] = entry + "=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS with no GitHub issuer: err = %v, want a refusal naming BROKER_EXCHANGE_GITHUB_ISSUER", err)
	}
}

// TestLoadRefusesAHalfConfiguredGitHubIssuer pins that GitHub's issuer and audience are
// both-or-neither, as the cluster's pair is: half a pair would register a token type whose tokens
// could not be checked.
func TestLoadRefusesAHalfConfiguredGitHubIssuer(t *testing.T) {
	for _, half := range []string{"BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE"} {
		e := validEnv()
		e[half] = "https://token.actions.githubusercontent.com"
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_AUDIENCE") {
			t.Errorf("Load(%s alone) = %v, want a refusal naming both halves", half, err)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.ExchangeGitHubIssuer != "" || cfg.ExchangeGitHubAudience != "" {
		t.Fatalf("Load with neither half = (%q, %q, %v), want no GitHub issuer and no error", cfg.ExchangeGitHubIssuer, cfg.ExchangeGitHubAudience, err)
	}
}

// signInSecret is the client secret the sign-in tests' secret file holds; no error Load answers
// may carry it.
const signInSecret = "do-not-print-this-client-secret"

// withSignIn is validEnv with all four of the sign-in client's settings, its secret in a file of
// t's own holding signInSecret between blank lines and spaces, which Load trims.
func withSignIn(t *testing.T) map[string]string {
	t.Helper()
	secretFile := t.TempDir() + "/client-secret"
	if err := os.WriteFile(secretFile, []byte("\n  "+signInSecret+"  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := validEnv()
	e["BROKER_SIGNIN_ISSUER"] = "https://accounts.example.com"
	e["BROKER_SIGNIN_CLIENT_ID"] = "broker-client"
	e["BROKER_SIGNIN_CLIENT_SECRET_FILE"] = secretFile
	e["BROKER_SIGNIN_DOMAIN"] = "example.com"
	return e
}

// signInSettings are the four sign-in settings set together or not at all.
var signInSettings = []string{"BROKER_SIGNIN_ISSUER", "BROKER_SIGNIN_CLIENT_ID", "BROKER_SIGNIN_CLIENT_SECRET_FILE", "BROKER_SIGNIN_DOMAIN"}

// TestLoadReadsTheSignInClient pins the sign-in client with all four settings: each value as
// given, the secret read from its file and trimmed, a session of 86400 seconds by default and of
// BROKER_SIGNIN_SESSION_SECONDS when set, and an http issuer accepted on a loopback host.
func TestLoadReadsTheSignInClient(t *testing.T) {
	cfg, err := Load(env(withSignIn(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := SignIn{Issuer: "https://accounts.example.com", ClientID: "broker-client", ClientSecret: signInSecret, Domain: "example.com", SessionLifetime: 86400 * time.Second}
	if cfg.SignIn == nil || *cfg.SignIn != want {
		t.Fatalf("SignIn = %+v, want %+v", cfg.SignIn, want)
	}
	e := withSignIn(t)
	e["BROKER_SIGNIN_SESSION_SECONDS"] = "600"
	if cfg, err := Load(env(e)); err != nil || cfg.SignIn == nil || cfg.SignIn.SessionLifetime != 10*time.Minute {
		t.Fatalf("Load(BROKER_SIGNIN_SESSION_SECONDS=600) = (%+v, %v), want a 10-minute session", cfg.SignIn, err)
	}
	for _, issuer := range []string{"http://127.0.0.1:5556", "http://localhost:5556/dex", "http://[::1]:5556"} {
		e := withSignIn(t)
		e["BROKER_SIGNIN_ISSUER"] = issuer
		if cfg, err := Load(env(e)); err != nil || cfg.SignIn == nil || cfg.SignIn.Issuer != issuer {
			t.Errorf("Load(BROKER_SIGNIN_ISSUER=%q) = (%+v, %v), want sign-in on with that issuer", issuer, cfg.SignIn, err)
		}
	}
}

// TestLoadLeavesSignInOffWhenUnset pins that a broker with none of the sign-in settings boots
// with sign-in off.
func TestLoadLeavesSignInOffWhenUnset(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.SignIn != nil {
		t.Fatalf("Load with no sign-in settings = (%+v, %v), want sign-in off and no error", cfg.SignIn, err)
	}
}

// TestLoadRefusesAPartialSignIn pins the all-or-none rule over every partial set of the four
// settings: each refuses, naming every setting it lacks.
func TestLoadRefusesAPartialSignIn(t *testing.T) {
	for mask := 1; mask < 1<<len(signInSettings)-1; mask++ {
		e := withSignIn(t)
		var missing []string
		for i, name := range signInSettings {
			if mask&(1<<i) == 0 {
				delete(e, name)
				missing = append(missing, name)
			}
		}
		_, err := Load(env(e))
		if err == nil {
			t.Errorf("Load without %v = nil error, want a refusal naming them", missing)
			continue
		}
		_, named, _ := strings.Cut(err.Error(), "missing ")
		if named != strings.Join(missing, ", ") {
			t.Errorf("Load without %v = %v, want a refusal naming exactly those as missing", missing, err)
		}
		if strings.Contains(err.Error(), signInSecret) {
			t.Errorf("Load without %v = %v, which carries the client secret", missing, err)
		}
	}
}

// TestLoadRefusesASignInSessionOutOfRange pins BROKER_SIGNIN_SESSION_SECONDS's range, 1 to 86400,
// and its refusal while sign-in is off, where it would set nothing.
func TestLoadRefusesASignInSessionOutOfRange(t *testing.T) {
	for _, bad := range []string{"86401", "0", "-1", "a day"} {
		e := withSignIn(t)
		e["BROKER_SIGNIN_SESSION_SECONDS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_SESSION_SECONDS") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_SESSION_SECONDS = %q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
	e := validEnv()
	e["BROKER_SIGNIN_SESSION_SECONDS"] = "3600"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_SESSION_SECONDS") {
		t.Fatalf("BROKER_SIGNIN_SESSION_SECONDS with sign-in off: err = %v, want a refusal naming it", err)
	}
}

// TestLoadRefusesAnUnusableSignInSecretFile pins that a secret file that cannot be read, or holds
// only blank space, refuses naming BROKER_SIGNIN_CLIENT_SECRET_FILE, never falling back to
// another source.
func TestLoadRefusesAnUnusableSignInSecretFile(t *testing.T) {
	dir := t.TempDir()
	blank := dir + "/blank"
	if err := os.WriteFile(blank, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"absent": dir + "/absent", "a directory": dir, "blank": blank} {
		e := withSignIn(t)
		e["BROKER_SIGNIN_CLIENT_SECRET_FILE"] = path
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET_FILE") {
			t.Errorf("a secret file that is %s: err = %v, want a refusal naming BROKER_SIGNIN_CLIENT_SECRET_FILE", name, err)
		}
	}
}

// TestLoadRefusesThePlainSignInSecret pins that the secret is never taken from the environment:
// BROKER_SIGNIN_CLIENT_SECRET refuses, naming the _FILE form, whether or not the file is set too,
// and the refusal does not carry its value.
func TestLoadRefusesThePlainSignInSecret(t *testing.T) {
	for name, e := range map[string]map[string]string{"with the file": withSignIn(t), "alone": validEnv()} {
		e["BROKER_SIGNIN_CLIENT_SECRET"] = signInSecret
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET ") || !strings.Contains(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET_FILE") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_CLIENT_SECRET %s: err = %v, want a refusal naming it and BROKER_SIGNIN_CLIENT_SECRET_FILE, without its value", name, err)
		}
	}
}

// TestLoadRefusesASignInIssuerThatIsNotHTTPS pins that the issuer, which a sign-in's code
// exchange sends the client secret to, is an absolute https URL, with http allowed only to a
// loopback host.
func TestLoadRefusesASignInIssuerThatIsNotHTTPS(t *testing.T) {
	for _, bad := range []string{"http://accounts.example.com", "http://10.0.0.1:5556", "http://127.0.0.1.example.com", "accounts.example.com", "ftp://accounts.example.com", "https://"} {
		e := withSignIn(t)
		e["BROKER_SIGNIN_ISSUER"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_ISSUER") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_ISSUER = %q: err = %v, want a refusal naming it", bad, err)
		}
	}
}
