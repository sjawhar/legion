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
	require.Contains(t, schema.Types, "envoy-infra:host:Watchdog")
	require.Contains(t, schema.Types, "envoy-infra:host:Adopt")
}

// The schema's object types are each component's own, and no others.
func TestSchemaHoldsTheComponentsTypesAlone(t *testing.T) {
	var schema struct {
		Types map[string]json.RawMessage `json:"types"`
	}
	require.NoError(t, json.Unmarshal([]byte(schemaJSON(t)), &schema))
	require.ElementsMatch(t, []string{
		"envoy-infra:host:Watchdog", "envoy-infra:host:Adopt",
		"envoy-infra:gcp:NatsUser", "envoy-infra:gcp:Permissions", "envoy-infra:gcp:Rules",
	}, slices.Collect(maps.Keys(schema.Types)))
}

func TestSchemaServesTheNatsHubComponentWithASecretAuthKey(t *testing.T) {
	type property struct {
		Type   string `json:"type"`
		Secret bool   `json:"secret"`
		Ref    string `json:"$ref"`
		Items  struct {
			Ref string `json:"$ref"`
		} `json:"items"`
	}
	var schema struct {
		Resources map[string]struct {
			IsComponent     bool                `json:"isComponent"`
			InputProperties map[string]property `json:"inputProperties"`
			RequiredInputs  []string            `json:"requiredInputs"`
			Properties      map[string]property `json:"properties"`
		} `json:"resources"`
		Types map[string]struct {
			Properties map[string]property `json:"properties"`
			Required   []string            `json:"required"`
		} `json:"types"`
	}
	require.NoError(t, json.Unmarshal([]byte(schemaJSON(t)), &schema))
	hub, ok := schema.Resources["envoy-infra:gcp:NatsHub"]
	require.True(t, ok, "the schema lists envoy-infra:gcp:NatsHub")
	require.True(t, hub.IsComponent)
	require.ElementsMatch(t, []string{
		"users", "tailnetAuthKey", "hostname", "tags", "zone", "machineType", "diskSizeGb", "natsImage",
		"tailscaleImage", "network", "subnetwork",
	}, slices.Collect(maps.Keys(hub.InputProperties)))
	require.ElementsMatch(t, []string{"users", "tailnetAuthKey", "hostname", "tags", "zone", "natsImage", "tailscaleImage"},
		hub.RequiredInputs)
	for name, in := range hub.InputProperties {
		require.Equal(t, name == "tailnetAuthKey", in.Secret, "only tailnetAuthKey is a secret input, not %s", name)
	}
	require.Equal(t, "#/types/envoy-infra:gcp:NatsUser", hub.InputProperties["users"].Items.Ref)
	require.ElementsMatch(t, []string{"natsUrl", "instanceGroup"}, slices.Collect(maps.Keys(hub.Properties)))

	// Generated SDKs append Args to an object type's name for its input type, so the Go names
	// NATSUserArgs, PermissionsArgs and RulesArgs would reach consumers doubled.
	user := schema.Types["envoy-infra:gcp:NatsUser"]
	require.Equal(t, "#/types/envoy-infra:gcp:Permissions", user.Properties["permissions"].Ref)
	require.Equal(t, "#/types/envoy-infra:gcp:Rules",
		schema.Types["envoy-infra:gcp:Permissions"].Properties["publish"].Ref)
	// core refuses an empty allow, so the schema requires it, and a generated SDK says so.
	require.ElementsMatch(t, []string{"allow"}, schema.Types["envoy-infra:gcp:Rules"].Required)
}
