package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// testAzureKey is the API key azureEmbedEnvironment sets, which no log line or refusal may carry.
const testAzureKey = "azure-key-value"

// azureEmbedEnvironment is a boot environment (signInEnvironment's) whose meaning search embeds
// through Azure with the key form of credential, with overrides applied; an override of "" leaves
// the variable empty, which is unset.
func azureEmbedEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		"DISPATCH_EMBEDDER":                      "azure",
		"DISPATCH_AZURE_EMBED_ENDPOINT":          "https://embed.example/models",
		"DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE": "300000",
		"DISPATCH_AZURE_EMBED_KEY":               testAzureKey,
	}
	for name, value := range overrides {
		values[name] = value
	}
	return signInEnvironment(values)
}

// azureKeyless is the keyless form of Azure credential, in place of azureEmbedEnvironment's key.
var azureKeyless = map[string]string{
	"DISPATCH_AZURE_EMBED_KEY":            "",
	"DISPATCH_AZURE_TENANT_ID":            "tenant-id",
	"DISPATCH_AZURE_CLIENT_ID":            "client-id",
	"DISPATCH_AZURE_FEDERATED_TOKEN_FILE": "/var/run/secrets/azure-federation/token",
}

// withOverrides is base with overrides applied over it, base itself unchanged.
func withOverrides(base, overrides map[string]string) map[string]string {
	values := map[string]string{}
	for name, value := range base {
		values[name] = value
	}
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

// capturedLog sends the default logger to the returned buffer for the rest of the test.
func capturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

// azureEmbedderFor is the embedder newEmbedder builds for azureEmbedEnvironment(overrides).
func azureEmbedderFor(t *testing.T, overrides map[string]string) embed.Embedder {
	t.Helper()
	embedder, err := newEmbedder(context.Background(), resolveWith(t, azureEmbedEnvironment(overrides)).Embedder)
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	return embedder
}

// Meaning search's place is checked whole at boot: each shape newEmbedder could not build, or a
// setting it would silently ignore, refuses, naming the setting and never the key.
func TestResolveBootConfigRefusesEveryEmbedderShapeItCannotUse(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	blank := writeFile(t, "blank-key", " \n")
	endpoint := func(value string) map[string]string {
		return azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_ENDPOINT": value})
	}
	quota := func(value string) map[string]string {
		return azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE": value})
	}
	const endpointShape = "(expected an absolute https URL with a host and no query or fragment, the base URL the embeddings route is appended to)"
	const quotaShape = "(expected a positive integer, the deployment's tokens-per-minute quota)"
	type refusal struct {
		name   string
		env    map[string]string
		naming []string
	}
	cases := []refusal{
		{"an unknown place", signInEnvironment(map[string]string{"DISPATCH_EMBEDDER": "openai"}),
			[]string{`DISPATCH_EMBEDDER="openai" (expected bedrock or azure)`}},
		{"a place in capitals", azureEmbedEnvironment(map[string]string{"DISPATCH_EMBEDDER": "Azure"}),
			[]string{`DISPATCH_EMBEDDER="Azure" (expected bedrock or azure)`}},
		{"azure without an endpoint", endpoint(""),
			[]string{"DISPATCH_AZURE_EMBED_ENDPOINT is required when DISPATCH_EMBEDDER is azure"}},
		{"a plain-http endpoint", endpoint("http://embed.example/models"),
			[]string{`DISPATCH_AZURE_EMBED_ENDPOINT="http://embed.example/models" ` + endpointShape}},
		{"a relative endpoint", endpoint("embed.example/models"),
			[]string{`DISPATCH_AZURE_EMBED_ENDPOINT="embed.example/models" ` + endpointShape}},
		{"an endpoint with no host", endpoint("https:///models"),
			[]string{`DISPATCH_AZURE_EMBED_ENDPOINT="https:///models" ` + endpointShape}},
		{"an endpoint with a query", endpoint("https://embed.example/models?api-version=2024-05-01-preview"),
			[]string{"DISPATCH_AZURE_EMBED_ENDPOINT=", endpointShape}},
		{"an endpoint with an empty query", endpoint("https://embed.example/models?"),
			[]string{`DISPATCH_AZURE_EMBED_ENDPOINT="https://embed.example/models?" ` + endpointShape}},
		{"an endpoint with a fragment", endpoint("https://embed.example/models#embeddings"),
			[]string{`DISPATCH_AZURE_EMBED_ENDPOINT="https://embed.example/models#embeddings" ` + endpointShape}},
		{"azure without a quota", quota(""),
			[]string{"DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE is required when DISPATCH_EMBEDDER is azure"}},
		{"a zero quota", quota("0"), []string{`DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE="0" ` + quotaShape}},
		{"a negative quota", quota("-300000"), []string{`DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE="-300000" ` + quotaShape}},
		{"a fractional quota", quota("1.5"), []string{`DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE="1.5" ` + quotaShape}},
		{"a quota that is not a number", quota("300k"), []string{`DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE="300k" ` + quotaShape}},
		{"both credential forms", azureEmbedEnvironment(withOverrides(azureKeyless, map[string]string{"DISPATCH_AZURE_EMBED_KEY": testAzureKey})),
			[]string{"DISPATCH_EMBEDDER=azure takes one credential, and both are set", "DISPATCH_AZURE_EMBED_KEY", "DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE set"}},
		{"the key and part of the keyless sign-in", azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_CLIENT_ID": "client-id"}),
			[]string{"DISPATCH_EMBEDDER=azure takes one credential, and both are set", "DISPATCH_AZURE_CLIENT_ID set"}},
		{"neither credential form", azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_KEY": ""}),
			[]string{"DISPATCH_EMBEDDER=azure needs a credential: the key (DISPATCH_AZURE_EMBED_KEY_FILE or DISPATCH_AZURE_EMBED_KEY), or the keyless sign-in (DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE)"}},
		{"a tenant alone", azureEmbedEnvironment(withOverrides(azureKeyless, map[string]string{"DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""})),
			[]string{"the keyless Azure sign-in needs DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE: DISPATCH_AZURE_TENANT_ID set, DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE missing"}},
		{"the keyless sign-in without its token file", azureEmbedEnvironment(withOverrides(azureKeyless, map[string]string{"DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""})),
			[]string{"DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID set, DISPATCH_AZURE_FEDERATED_TOKEN_FILE missing"}},
		{"an unreadable key file", azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_KEY": "", "DISPATCH_AZURE_EMBED_KEY_FILE": missing}),
			[]string{"DISPATCH_AZURE_EMBED_KEY_FILE names " + missing + ", which could not be read, and DISPATCH_AZURE_EMBED_KEY is not read in its place"}},
		{"a blank key file beside a bare key", azureEmbedEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_KEY_FILE": blank}),
			[]string{"DISPATCH_AZURE_EMBED_KEY_FILE names " + blank + ", which is empty, and DISPATCH_AZURE_EMBED_KEY is not read in its place"}},
	}
	// Every DISPATCH_AZURE_* variable, a _FILE form included, is refused under Bedrock, whether
	// DISPATCH_EMBEDDER says so or is unset.
	for _, variable := range []string{
		"DISPATCH_AZURE_EMBED_ENDPOINT", "DISPATCH_AZURE_EMBED_DEPLOYMENT", "DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE",
		"DISPATCH_AZURE_EMBED_KEY", "DISPATCH_AZURE_EMBED_KEY_FILE", "DISPATCH_AZURE_TENANT_ID", "DISPATCH_AZURE_CLIENT_ID",
		"DISPATCH_AZURE_FEDERATED_TOKEN_FILE",
	} {
		cases = append(cases,
			refusal{variable + " under the default", signInEnvironment(map[string]string{variable: "x"}),
				[]string{variable + " set, but DISPATCH_EMBEDDER is unset, which is bedrock: the DISPATCH_AZURE_* settings are read only with DISPATCH_EMBEDDER=azure"}},
			refusal{variable + " under bedrock", signInEnvironment(map[string]string{"DISPATCH_EMBEDDER": "bedrock", variable: "x"}),
				[]string{variable + " set, but DISPATCH_EMBEDDER is bedrock"}},
		)
	}
	cases = append(cases, refusal{"the whole Azure configuration under bedrock", azureEmbedEnvironment(map[string]string{"DISPATCH_EMBEDDER": "bedrock"}),
		[]string{"DISPATCH_AZURE_EMBED_ENDPOINT, DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE, DISPATCH_AZURE_EMBED_KEY set, but DISPATCH_EMBEDDER is bedrock"}})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveBootConfig(envGetter(tc.env))
			if err == nil {
				t.Fatalf("resolveBootConfig: no error, want a refusal naming %q", tc.naming)
			}
			for _, want := range tc.naming {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), testAzureKey) {
				t.Errorf("err = %v carries the key", err)
			}
		})
	}
}

// A deployment that sets nothing new boots Bedrock exactly as before: with no AWS region, meaning
// search is off with the warning it always logged, never a refusal; with one, the Bedrock client.
func TestNewEmbedderBuildsBedrockWhenNothingNewIsSet(t *testing.T) {
	boot := resolveWith(t, nil)
	if boot.Embedder != (embedderConfig{Place: embedderBedrock}) {
		t.Fatalf("Embedder = %+v, want Bedrock and nothing else", boot.Embedder)
	}
	if explicit := resolveWith(t, map[string]string{"DISPATCH_EMBEDDER": " bedrock "}); explicit.Embedder != boot.Embedder {
		t.Errorf("DISPATCH_EMBEDDER=bedrock: Embedder = %+v, want the default's %+v", explicit.Embedder, boot.Embedder)
	}
	noFile := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", noFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", noFile)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_REGION", "")

	logged := capturedLog(t)
	embedder, err := newEmbedder(context.Background(), boot.Embedder)
	if err != nil || embedder != nil {
		t.Fatalf("no AWS region: newEmbedder = %v, %v, want no embedder and no error", embedder, err)
	}
	if !strings.Contains(logged.String(), `level=WARN msg="dispatch: meaning search unavailable" error="embed: needs an AWS region`) {
		t.Errorf("no AWS region: logged %q, want the meaning-search-unavailable warning", logged.String())
	}

	t.Setenv("AWS_REGION", "us-east-1")
	logged.Reset()
	embedder, err = newEmbedder(context.Background(), boot.Embedder)
	if err != nil || embedder == nil || embedder.Identity() != embed.BedrockModel {
		t.Fatalf("AWS_REGION set: newEmbedder = %v, %v, want the Bedrock client of %s", embedder, err, embed.BedrockModel)
	}
	if logged.Len() != 0 {
		t.Errorf("AWS_REGION set: logged %q, want nothing", logged.String())
	}
}

// Either Azure credential builds the Azure client, named by the endpoint's host and the
// deployment, and logs one line naming the place and the identity, never the key.
func TestNewEmbedderBuildsAzureFromEitherCredential(t *testing.T) {
	for credential, env := range map[string]map[string]string{
		"key":     azureEmbedEnvironment(nil),
		"keyless": azureEmbedEnvironment(azureKeyless),
	} {
		t.Run(credential, func(t *testing.T) {
			logged := capturedLog(t)
			embedder, err := newEmbedder(context.Background(), resolveWith(t, env).Embedder)
			if err != nil {
				t.Fatalf("newEmbedder: %v", err)
			}
			if _, ok := embedder.(*embed.AzureClient); !ok {
				t.Fatalf("newEmbedder built %T, want *embed.AzureClient", embedder)
			}
			const identity = "azure:embed.example/embed-v-4-0"
			if got := embedder.Identity(); got != identity {
				t.Errorf("Identity() = %q, want %q", got, identity)
			}
			if got := embedder.Limits().BackgroundTokensPerMinute; got != 200_000 {
				t.Errorf("BackgroundTokensPerMinute = %d, want two thirds of the 300,000 quota", got)
			}
			want := `level=INFO msg="dispatch: meaning search embeds through Azure AI Foundry" place=azure identity=` + identity + " credential=" + credential + "\n"
			if line := logged.String(); !strings.HasSuffix(line, want) || strings.Count(line, "\n") != 1 || strings.Contains(line, testAzureKey) {
				t.Errorf("logged %q, want the one line %q", line, want)
			}
		})
	}
}
