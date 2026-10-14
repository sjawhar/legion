package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/provider"
)

func TestSchemaNamesTheReleaseDownloadURLAndTheGoImportPath(t *testing.T) {
	prov, err := provider.New()
	require.NoError(t, err)
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov))
	require.NoError(t, err)

	resp, err := server.GetSchema(p.GetSchemaRequest{})
	require.NoError(t, err)

	var schema struct {
		PluginDownloadURL string `json:"pluginDownloadURL"`
		Language          struct {
			Go struct {
				ImportBasePath string `json:"importBasePath"`
			} `json:"go"`
		} `json:"language"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Schema), &schema))
	require.Equal(t, "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}",
		schema.PluginDownloadURL)
	require.Equal(t, "github.com/sjawhar/envoy/infra/sdk/go/envoyinfra", schema.Language.Go.ImportBasePath)
}

func TestSchemaServesTheListenerComponentWithASecretSeed(t *testing.T) {
	prov, err := provider.New()
	require.NoError(t, err)
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov))
	require.NoError(t, err)

	resp, err := server.GetSchema(p.GetSchemaRequest{})
	require.NoError(t, err)

	var schema struct {
		Resources map[string]struct {
			IsComponent     bool `json:"isComponent"`
			InputProperties map[string]struct {
				Secret bool   `json:"secret"`
				Ref    string `json:"$ref"`
			} `json:"inputProperties"`
		} `json:"resources"`
		Types map[string]json.RawMessage `json:"types"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Schema), &schema))
	listener, ok := schema.Resources["envoy-infra:host:Listener"]
	require.True(t, ok, "the schema lists envoy-infra:host:Listener")
	require.True(t, listener.IsComponent)
	require.True(t, listener.InputProperties["seed"].Secret, "seed is a secret input")

	// Generated SDKs append Args to an object type's name for its input type, so a type named
	// WatchdogArgs would reach consumers as WatchdogArgsArgs.
	require.Equal(t, "#/types/envoy-infra:host:Watchdog", listener.InputProperties["watchdog"].Ref)
	require.Equal(t, "#/types/envoy-infra:host:Adopt", listener.InputProperties["adopt"].Ref)
	var hostTypes []string
	for token := range schema.Types {
		if strings.HasPrefix(token, "envoy-infra:host:") {
			hostTypes = append(hostTypes, token)
		}
	}
	require.ElementsMatch(t, []string{"envoy-infra:host:Watchdog", "envoy-infra:host:Adopt"}, hostTypes)
}

func TestSchemaServesTheNatsServiceComponentWithNoSecretInput(t *testing.T) {
	prov, err := provider.New()
	require.NoError(t, err)
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov))
	require.NoError(t, err)

	resp, err := server.GetSchema(p.GetSchemaRequest{})
	require.NoError(t, err)

	var schema struct {
		Resources map[string]struct {
			IsComponent     bool `json:"isComponent"`
			InputProperties map[string]struct {
				Secret bool   `json:"secret"`
				Ref    string `json:"$ref"`
			} `json:"inputProperties"`
		} `json:"resources"`
		Types map[string]json.RawMessage `json:"types"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Schema), &schema))
	nats, ok := schema.Resources["envoy-infra:aws:NatsService"]
	require.True(t, ok, "the schema lists envoy-infra:aws:NatsService")
	require.True(t, nats.IsComponent)
	// A consumer owns its secrets; the component takes references to them.
	for name, input := range nats.InputProperties {
		require.False(t, input.Secret, "%s is not a secret input", name)
	}

	// Generated SDKs append Args to an object type's name for its input type.
	require.Equal(t, "#/types/envoy-infra:aws:Authorization", nats.InputProperties["authorization"].Ref)
	var awsTypes []string
	for token := range schema.Types {
		if strings.HasPrefix(token, "envoy-infra:aws:") {
			awsTypes = append(awsTypes, token)
		}
	}
	require.ElementsMatch(t, []string{"envoy-infra:aws:Authorization", "envoy-infra:aws:User", "envoy-infra:aws:Rules"}, awsTypes)
}
