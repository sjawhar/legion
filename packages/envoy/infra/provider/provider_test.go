package provider_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/provider"
)

// schemaJSON serves the plugin in process and returns the schema it answers GetSchema with.
func schemaJSON(t *testing.T) string {
	t.Helper()
	prov, err := provider.New()
	require.NoError(t, err)
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov))
	require.NoError(t, err)
	resp, err := server.GetSchema(p.GetSchemaRequest{})
	require.NoError(t, err)
	return resp.Schema
}

func TestSchemaNamesTheReleaseDownloadURLAndTheGoImportPath(t *testing.T) {
	var schema struct {
		PluginDownloadURL string `json:"pluginDownloadURL"`
		Language          struct {
			Go struct {
				ImportBasePath string `json:"importBasePath"`
			} `json:"go"`
		} `json:"language"`
	}
	require.NoError(t, json.Unmarshal([]byte(schemaJSON(t)), &schema))
	require.Equal(t, "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}",
		schema.PluginDownloadURL)
	require.Equal(t, "github.com/sjawhar/envoy/infra/sdk/go/envoyinfra", schema.Language.Go.ImportBasePath)
}

func TestSchemaServesTheListenerComponentWithASecretSeed(t *testing.T) {
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
	require.NoError(t, json.Unmarshal([]byte(schemaJSON(t)), &schema))
	listener, ok := schema.Resources["envoy-infra:host:Listener"]
	require.True(t, ok, "the schema lists envoy-infra:host:Listener")
	require.True(t, listener.IsComponent)
	require.True(t, listener.InputProperties["seed"].Secret, "seed is a secret input")

	// Generated SDKs append Args to an object type's name for its input type, so a type named
	// WatchdogArgs would reach consumers as WatchdogArgsArgs.
	require.Equal(t, "#/types/envoy-infra:host:Watchdog", listener.InputProperties["watchdog"].Ref)
	require.Equal(t, "#/types/envoy-infra:host:Adopt", listener.InputProperties["adopt"].Ref)
	require.ElementsMatch(t, []string{"envoy-infra:host:Watchdog", "envoy-infra:host:Adopt"},
		slices.Collect(maps.Keys(schema.Types)))
}
