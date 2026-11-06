package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// azureChatEnvironment is a boot environment (signInEnvironment's) whose answerer is a chat
// deployment of an Azure AI Foundry resource, signed in with the shared federated credential,
// with overrides applied; an override of "" leaves the variable empty, which is unset.
func azureChatEnvironment(overrides map[string]string) map[string]string {
	values := withOverrides(map[string]string{
		"DISPATCH_ANSWERER":                     "azure",
		"DISPATCH_AZURE_CHAT_ENDPOINT":          "https://chat.example/models",
		"DISPATCH_AZURE_CHAT_DEPLOYMENT":        "chat-deployment",
		"DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE": "200000",
	}, azureFederated)
	return signInEnvironment(withOverrides(values, overrides))
}

// endpointChatEnvironment is a boot environment whose answerer is an OpenAI-shaped chat
// completions service, with overrides applied.
func endpointChatEnvironment(overrides map[string]string) map[string]string {
	return signInEnvironment(withOverrides(map[string]string{
		"DISPATCH_ANSWERER":        "endpoint",
		"DISPATCH_CHAT_ENDPOINT":   "https://chat.example/v1",
		"DISPATCH_CHAT_TOKEN_FILE": "/var/run/secrets/chat/token",
		"DISPATCH_CHAT_MODEL":      "chat-model",
	}, overrides))
}

// Each place resolves whole, and carries nothing of the other: the Azure form the deployment, its
// quota and the shared sign-in, the endpoint form the model and the token file's path.
func TestResolveAnswererResolvesEachPlaceWhole(t *testing.T) {
	azure := resolveWith(t, azureChatEnvironment(map[string]string{"DISPATCH_AZURE_CHAT_API_VERSION": "2024-05-01-preview"})).Answerer
	want := answererConfig{
		Place:           placeAnswererAzure,
		Endpoint:        "https://chat.example/models",
		Model:           "chat-deployment",
		APIVersion:      "2024-05-01-preview",
		Scope:           embed.DefaultEntraScope,
		TokensPerMinute: 200_000,
		TenantID:        "tenant-id",
		ClientID:        "client-id",
		FederatedToken:  "/var/run/secrets/azure-federation/token",
	}
	if azure != want {
		t.Errorf("azure answerer = %+v, want %+v", azure, want)
	}
	endpoint := resolveWith(t, endpointChatEnvironment(nil)).Answerer
	want = answererConfig{
		Place:     placeAnswererEndpoint,
		Endpoint:  "https://chat.example/v1",
		Model:     "chat-model",
		TokenFile: "/var/run/secrets/chat/token",
	}
	if endpoint != want {
		t.Errorf("endpoint answerer = %+v, want %+v", endpoint, want)
	}
	if unset := resolveWith(t, nil).Answerer; unset != (answererConfig{}) {
		t.Errorf("no DISPATCH_ANSWERER: answerer = %+v, want no model configured", unset)
	}
}

// The answerer's place is checked whole at boot: each shape no client could be built from, and
// each setting the chosen place would silently ignore, refuses, naming the setting.
func TestResolveBootConfigRefusesEveryAnswererShapeItCannotUse(t *testing.T) {
	azureEndpoint := func(value string) map[string]string {
		return azureChatEnvironment(map[string]string{"DISPATCH_AZURE_CHAT_ENDPOINT": value})
	}
	chatEndpoint := func(value string) map[string]string {
		return endpointChatEnvironment(map[string]string{"DISPATCH_CHAT_ENDPOINT": value})
	}
	quota := func(value string) map[string]string {
		return azureChatEnvironment(map[string]string{"DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE": value})
	}
	const endpointShape = "(expected an absolute https URL with a host and no query or fragment, the base URL the chat completions route is appended to)"
	const quotaShape = "(expected a positive integer, the deployment's tokens-per-minute quota)"
	type refusal struct {
		name   string
		env    map[string]string
		naming []string
	}
	cases := []refusal{
		{"an unknown place", signInEnvironment(map[string]string{"DISPATCH_ANSWERER": "openai"}),
			[]string{`DISPATCH_ANSWERER="openai" (expected azure or endpoint)`}},
		{"a place in capitals", azureChatEnvironment(map[string]string{"DISPATCH_ANSWERER": "Azure"}),
			[]string{`DISPATCH_ANSWERER="Azure" (expected azure or endpoint)`}},
		{"azure without an endpoint", azureEndpoint(""),
			[]string{"DISPATCH_AZURE_CHAT_ENDPOINT is required when DISPATCH_ANSWERER is azure"}},
		{"a plain-http azure endpoint", azureEndpoint("http://chat.example/models"),
			[]string{`DISPATCH_AZURE_CHAT_ENDPOINT="http://chat.example/models" ` + endpointShape}},
		{"a relative azure endpoint", azureEndpoint("chat.example/models"),
			[]string{`DISPATCH_AZURE_CHAT_ENDPOINT="chat.example/models" ` + endpointShape}},
		{"an azure endpoint with no host", azureEndpoint("https:///models"),
			[]string{`DISPATCH_AZURE_CHAT_ENDPOINT="https:///models" ` + endpointShape}},
		{"an azure endpoint with a query", azureEndpoint("https://chat.example/models?api-version=2024-05-01-preview"),
			[]string{"DISPATCH_AZURE_CHAT_ENDPOINT=", endpointShape}},
		{"an azure endpoint with a fragment", azureEndpoint("https://chat.example/models#chat"),
			[]string{`DISPATCH_AZURE_CHAT_ENDPOINT="https://chat.example/models#chat" ` + endpointShape}},
		{"azure without a deployment", azureChatEnvironment(map[string]string{"DISPATCH_AZURE_CHAT_DEPLOYMENT": ""}),
			[]string{"DISPATCH_AZURE_CHAT_DEPLOYMENT is required when DISPATCH_ANSWERER is azure"}},
		{"azure without a quota", quota(""),
			[]string{"DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE is required when DISPATCH_ANSWERER is azure"}},
		{"a zero quota", quota("0"), []string{`DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE="0" ` + quotaShape}},
		{"a negative quota", quota("-200000"), []string{`DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE="-200000" ` + quotaShape}},
		{"a fractional quota", quota("1.5"), []string{`DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE="1.5" ` + quotaShape}},
		{"a quota that is not a number", quota("200k"), []string{`DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE="200k" ` + quotaShape}},
		{"azure without the sign-in", signInEnvironment(withOverrides(azureChatEnvironment(nil), map[string]string{
			"DISPATCH_AZURE_TENANT_ID": "", "DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": "",
		})), []string{"DISPATCH_ANSWERER=azure needs the keyless Azure sign-in (DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE): the chat form takes no API key"}},
		{"a tenant alone", azureChatEnvironment(map[string]string{"DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""}),
			[]string{"the keyless Azure sign-in needs DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE: DISPATCH_AZURE_TENANT_ID set, DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE missing"}},
		{"the sign-in without its token file", azureChatEnvironment(map[string]string{"DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""}),
			[]string{"DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID set, DISPATCH_AZURE_FEDERATED_TOKEN_FILE missing"}},
		{"endpoint without an endpoint", chatEndpoint(""),
			[]string{"DISPATCH_CHAT_ENDPOINT is required when DISPATCH_ANSWERER is endpoint"}},
		{"a plain-http endpoint", chatEndpoint("http://chat.example/v1"),
			[]string{`DISPATCH_CHAT_ENDPOINT="http://chat.example/v1" ` + endpointShape}},
		{"a relative endpoint", chatEndpoint("chat.example/v1"),
			[]string{`DISPATCH_CHAT_ENDPOINT="chat.example/v1" ` + endpointShape}},
		{"an endpoint with a query", chatEndpoint("https://chat.example/v1?model=chat-model"),
			[]string{"DISPATCH_CHAT_ENDPOINT=", endpointShape}},
		{"endpoint without a token file", endpointChatEnvironment(map[string]string{"DISPATCH_CHAT_TOKEN_FILE": ""}),
			[]string{"DISPATCH_CHAT_TOKEN_FILE is required when DISPATCH_ANSWERER is endpoint"}},
		{"endpoint without a model", endpointChatEnvironment(map[string]string{"DISPATCH_CHAT_MODEL": ""}),
			[]string{"DISPATCH_CHAT_MODEL is required when DISPATCH_ANSWERER is endpoint"}},
	}
	// Each group is refused by the place that does not read it, whether that place is the other
	// one or no answerer at all.
	for _, variable := range []string{
		"DISPATCH_AZURE_CHAT_ENDPOINT", "DISPATCH_AZURE_CHAT_DEPLOYMENT", "DISPATCH_AZURE_CHAT_API_VERSION",
		"DISPATCH_AZURE_CHAT_SCOPE", "DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE",
	} {
		cases = append(cases,
			refusal{variable + " with no answerer", signInEnvironment(map[string]string{variable: "x"}),
				[]string{variable + " set, but DISPATCH_ANSWERER is unset: the DISPATCH_AZURE_CHAT_* settings are read only with DISPATCH_ANSWERER=azure"}},
			refusal{variable + " under endpoint", endpointChatEnvironment(map[string]string{variable: "x"}),
				[]string{variable + " set, but DISPATCH_ANSWERER is endpoint: the DISPATCH_AZURE_CHAT_* settings are read only with DISPATCH_ANSWERER=azure"}},
		)
	}
	for _, variable := range []string{"DISPATCH_CHAT_ENDPOINT", "DISPATCH_CHAT_TOKEN_FILE", "DISPATCH_CHAT_MODEL"} {
		cases = append(cases,
			refusal{variable + " with no answerer", signInEnvironment(map[string]string{variable: "x"}),
				[]string{variable + " set, but DISPATCH_ANSWERER is unset: the DISPATCH_CHAT_* settings are read only with DISPATCH_ANSWERER=endpoint"}},
			refusal{variable + " under azure", azureChatEnvironment(map[string]string{variable: "x"}),
				[]string{variable + " set, but DISPATCH_ANSWERER is azure: the DISPATCH_CHAT_* settings are read only with DISPATCH_ANSWERER=endpoint"}},
		)
	}
	cases = append(cases,
		refusal{"the whole Azure chat configuration with no answerer", azureChatEnvironment(map[string]string{"DISPATCH_ANSWERER": ""}),
			[]string{"DISPATCH_AZURE_CHAT_ENDPOINT, DISPATCH_AZURE_CHAT_DEPLOYMENT, DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE set, but DISPATCH_ANSWERER is unset"}},
		refusal{"the whole endpoint configuration with no answerer", endpointChatEnvironment(map[string]string{"DISPATCH_ANSWERER": ""}),
			[]string{"DISPATCH_CHAT_ENDPOINT, DISPATCH_CHAT_TOKEN_FILE, DISPATCH_CHAT_MODEL set, but DISPATCH_ANSWERER is unset"}})

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
		})
	}
}

// Each place builds the client it names, identified by its endpoint's host and the model that
// answers, and logs exactly one line naming the place and that identity and no credential: the
// key to reading the boot of a deployment that answers questions. A deployment that configured
// no model builds none and says so, and boots.
func TestNewAnswererBuildsEachPlaceAndLogsTheOneLine(t *testing.T) {
	for place, env := range map[string]map[string]string{
		placeAnswererAzure:    azureChatEnvironment(nil),
		placeAnswererEndpoint: endpointChatEnvironment(nil),
	} {
		t.Run(place, func(t *testing.T) {
			cfg := resolveWith(t, env).Answerer
			logged := capturedLog(t)
			answerer, err := newAnswerer(cfg)
			if err != nil {
				t.Fatalf("newAnswerer: %v", err)
			}
			if answerer == nil {
				t.Fatal("newAnswerer built no answerer, want the client the settings name")
			}
			// The identity is read where a reader of a boot reads it: the one logged line. It
			// is the client's own (going.HTTPClient.Identity), which is why nothing between
			// here and that client carries it.
			identity := place + ":chat.example/" + cfg.Model
			want := `level=INFO msg="dispatch: answers questions with a configured text model" place=` + place + " identity=" + identity + "\n"
			line := logged.String()
			if !strings.HasSuffix(line, want) || strings.Count(line, "\n") != 1 {
				t.Fatalf("logged %q, want the one line %q", line, want)
			}
			for _, credential := range []string{cfg.TokenFile, cfg.TenantID, cfg.ClientID, cfg.FederatedToken} {
				if credential != "" && strings.Contains(line, credential) {
					t.Errorf("logged %q, which names the credential %q", line, credential)
				}
			}
		})
	}

	logged := capturedLog(t)
	answerer, err := newAnswerer(resolveWith(t, nil).Answerer)
	if err != nil || answerer != nil {
		t.Fatalf("no DISPATCH_ANSWERER: newAnswerer = %v, %v, want no answerer and no error", answerer, err)
	}
	want := `level=WARN msg="dispatch: no text model configured, so this server cannot answer questions" setting=DISPATCH_ANSWERER` + "\n"
	if line := logged.String(); !strings.HasSuffix(line, want) || strings.Count(line, "\n") != 1 {
		t.Fatalf("logged %q, want the one line %q", line, want)
	}
}
