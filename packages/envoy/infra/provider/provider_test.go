package provider_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	pproperty "github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/infra/provider"
	"github.com/sjawhar/envoy/internal/testnats"
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

type property struct {
	Type   string `json:"type"`
	Secret bool   `json:"secret"`
	Plain  bool   `json:"plain"`
	Ref    string `json:"$ref"`
	Items  struct {
		Ref string `json:"$ref"`
	} `json:"items"`
}

// schema is the part of the plugin's schema the tests read.
type schema struct {
	PluginDownloadURL string `json:"pluginDownloadURL"`
	Language          struct {
		Go struct {
			ImportBasePath string `json:"importBasePath"`
		} `json:"go"`
	} `json:"language"`
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

// servedSchema is schemaJSON decoded.
func servedSchema(t *testing.T) schema {
	t.Helper()
	var s schema
	require.NoError(t, json.Unmarshal([]byte(schemaJSON(t)), &s))
	return s
}

func TestSchemaNamesTheReleaseDownloadURLAndTheGoImportPath(t *testing.T) {
	s := servedSchema(t)
	require.Equal(t, "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}", s.PluginDownloadURL)
	require.Equal(t, "github.com/sjawhar/envoy/infra/sdk/go/envoyinfra", s.Language.Go.ImportBasePath)
}

// The schema's types are exactly the components' own, the rules every NATS component shares under
// nats, which names no cloud.
func TestSchemaTypes(t *testing.T) {
	require.ElementsMatch(t, []string{
		"envoy-infra:host:Watchdog", "envoy-infra:host:Adopt",
		"envoy-infra:aws:Authorization", "envoy-infra:aws:User", "envoy-infra:nats:Rules",
		"envoy-infra:gcp:NatsUser", "envoy-infra:gcp:Permissions", "envoy-infra:gcp:HubListener", "envoy-infra:gcp:SessionUser",
	}, slices.Collect(maps.Keys(servedSchema(t).Types)))
}

func TestSchemaServesTheListenerComponentWithASecretSeed(t *testing.T) {
	listener, ok := servedSchema(t).Resources["envoy-infra:host:Listener"]
	require.True(t, ok, "the schema lists envoy-infra:host:Listener")
	require.True(t, listener.IsComponent)
	require.True(t, listener.InputProperties["seed"].Secret, "seed is a secret input")

	// Generated SDKs append Args to an object type's name for its input type, so a type named
	// WatchdogArgs would reach consumers as WatchdogArgsArgs.
	require.Equal(t, "#/types/envoy-infra:host:Watchdog", listener.InputProperties["watchdog"].Ref)
	require.Equal(t, "#/types/envoy-infra:host:Adopt", listener.InputProperties["adopt"].Ref)
}

func TestSchemaServesTheNatsHubComponentWithASecretAuthKey(t *testing.T) {
	s := servedSchema(t)
	hub, ok := s.Resources["envoy-infra:gcp:NatsHub"]
	require.True(t, ok, "the schema lists envoy-infra:gcp:NatsHub")
	require.True(t, hub.IsComponent)
	require.ElementsMatch(t, []string{
		"users", "tailnetAuthKey", "hostname", "tailnetDnsName", "tags", "zone", "machineType", "diskType",
		"diskSizeGb", "natsImage", "tailscaleImage", "network", "subnetwork", "listener", "sessionUser",
	}, slices.Collect(maps.Keys(hub.InputProperties)))
	require.ElementsMatch(t, []string{"tailnetAuthKey", "hostname", "tags", "zone", "natsImage", "tailscaleImage"},
		hub.RequiredInputs)
	for name, in := range hub.InputProperties {
		require.Equal(t, name == "tailnetAuthKey", in.Secret, "only tailnetAuthKey is a secret input, not %s", name)
	}
	require.Equal(t, "#/types/envoy-infra:gcp:NatsUser", hub.InputProperties["users"].Items.Ref)
	require.Equal(t, "#/types/envoy-infra:gcp:HubListener", hub.InputProperties["listener"].Ref)
	require.Equal(t, "#/types/envoy-infra:gcp:SessionUser", hub.InputProperties["sessionUser"].Ref)
	// The listener's whole property set is pinned, not its required inputs alone, so a property
	// added to it cannot stay invisible here.
	require.ElementsMatch(t, []string{"image", "seedSecret", "nkey", "allowedHosts"},
		slices.Collect(maps.Keys(s.Types["envoy-infra:gcp:HubListener"].Properties)))
	require.ElementsMatch(t, []string{"image", "seedSecret", "nkey"}, s.Types["envoy-infra:gcp:HubListener"].Required)
	require.ElementsMatch(t, []string{"name", "nkey"}, s.Types["envoy-infra:gcp:SessionUser"].Required)
	require.ElementsMatch(t, []string{"natsUrl", "instanceGroup"}, slices.Collect(maps.Keys(hub.Properties)))

	// Generated SDKs append Args to an object type's name for its input type, so the Go names
	// NATSUserArgs, PermissionsArgs and RulesArgs would reach consumers doubled.
	user := s.Types["envoy-infra:gcp:NatsUser"]
	require.Equal(t, "#/types/envoy-infra:gcp:Permissions", user.Properties["permissions"].Ref)
	// One Rules type and one key field name for every NATS user input, the AWS components' and the
	// hub's, so a consumer running both writes them alike.
	require.Contains(t, user.Properties, "nkey")
	require.NotContains(t, user.Properties, "publicKey")
	require.Contains(t, user.Properties, "dispatchMachineId")
	require.Equal(t, "boolean", user.Properties["daemon"].Type)
	for _, direction := range []string{"publish", "subscribe"} {
		require.Equal(t, "#/types/envoy-infra:nats:Rules",
			s.Types["envoy-infra:gcp:Permissions"].Properties[direction].Ref)
	}
	// core refuses an empty allow, so the schema requires it, and a generated SDK says so.
	require.ElementsMatch(t, []string{"allow"}, s.Types["envoy-infra:nats:Rules"].Required)
}

func TestSchemaServesTheDispatchComponentWithItsSecrets(t *testing.T) {
	dispatch, ok := servedSchema(t).Resources["envoy-infra:gcp:Dispatch"]
	require.True(t, ok, "the schema lists envoy-infra:gcp:Dispatch")
	require.True(t, dispatch.IsComponent)
	require.True(t, dispatch.InputProperties["tailnetAuthKey"].Secret)
	require.True(t, dispatch.InputProperties["signInClientSecret"].Secret)
	require.Contains(t, dispatch.InputProperties, "agentSecretsAudience")
	require.False(t, dispatch.InputProperties["agentSecretsAudience"].Secret)
	require.NotContains(t, dispatch.InputProperties, "agentSecretsTokenSecret")
	require.NotContains(t, dispatch.InputProperties, "agentSecretsToken")
	require.NotContains(t, dispatch.InputProperties, "appPrivateKey")
	for _, input := range []string{"appClientId", "appId", "appPrivateKeySecretName", "appPrivateKeySecretKey"} {
		require.False(t, dispatch.InputProperties[input].Secret, input)
	}
	// The Azure embedder's inputs are references and identifiers, never the key: the key form
	// takes a Secret Manager path, the keyless form a tenant and a client id.
	for _, input := range []string{
		"azureEmbedEndpoint", "azureEmbedDeployment", "azureEmbedTokensPerMinute", "azureEmbedKeySecret",
		"azureTenantId", "azureClientId", "azureFederationAudience",
	} {
		require.Contains(t, dispatch.InputProperties, input)
		require.False(t, dispatch.InputProperties[input].Secret, input)
		require.NotContains(t, dispatch.RequiredInputs, input)
	}
	require.NotContains(t, dispatch.InputProperties, "azureEmbedKey")
	// The profiler's port is an optional plain integer.
	require.Equal(t, "integer", dispatch.InputProperties["pprofPort"].Type)
	require.False(t, dispatch.InputProperties["pprofPort"].Secret)
	require.NotContains(t, dispatch.RequiredInputs, "pprofPort")
	// Query Insights is an optional plain boolean.
	require.Equal(t, "boolean", dispatch.InputProperties["databaseQueryInsights"].Type)
	require.False(t, dispatch.InputProperties["databaseQueryInsights"].Secret)
	require.NotContains(t, dispatch.RequiredInputs, "databaseQueryInsights")
	// The app container's size is two optional plain strings, Kubernetes quantities.
	for _, input := range []string{"appCpu", "appMemory"} {
		require.Equal(t, "string", dispatch.InputProperties[input].Type, input)
		require.False(t, dispatch.InputProperties[input].Secret, input)
		require.NotContains(t, dispatch.RequiredInputs, input)
	}
	// The app pod's disruption protection is an optional plain string and an optional plain integer.
	for input, typ := range map[string]string{"appPriorityClassName": "string", "appPdbMinAvailable": "integer"} {
		require.Equal(t, typ, dispatch.InputProperties[input].Type, input)
		require.True(t, dispatch.InputProperties[input].Plain, input)
		require.False(t, dispatch.InputProperties[input].Secret, input)
		require.NotContains(t, dispatch.RequiredInputs, input)
	}
	require.True(t, dispatch.Properties["agentToken"].Secret)
	for _, output := range []string{"serviceAccountIssuer", "serviceAccountSubject"} {
		require.Equal(t, "string", dispatch.Properties[output].Type, output)
		require.False(t, dispatch.Properties[output].Secret, output)
	}
}

func TestSchemaServesTheNatsServiceComponentWithNoSecretInput(t *testing.T) {
	s := servedSchema(t)
	nats, ok := s.Resources["envoy-infra:aws:NatsService"]
	require.True(t, ok, "the schema lists envoy-infra:aws:NatsService")
	require.True(t, nats.IsComponent)
	// A consumer owns its secrets; the component takes references to them.
	for name, input := range nats.InputProperties {
		require.False(t, input.Secret, "%s is not a secret input", name)
	}
	// Generated SDKs append Args to an object type's name for its input type.
	require.Equal(t, "#/types/envoy-infra:aws:Authorization", nats.InputProperties["authorization"].Ref)
	// A user's rules are the shared nats:Rules.
	for _, direction := range []string{"publish", "subscribe"} {
		require.Equal(t, "#/types/envoy-infra:nats:Rules", s.Types["envoy-infra:aws:User"].Properties[direction].Ref, "aws:User's %s", direction)
	}
	// Every input is required but the authorization and what belongs to it, and the optional
	// settings.
	require.ElementsMatch(t, []string{
		"authorization", "configHeader", "configPath", "tailnetEphemeral", "waitForSteadyState", "adopt",
	}, optionalInputs(nats.InputProperties, nats.RequiredInputs))
}

func TestSchemaServesTheWebhookListenerComponentWithNoSecretInput(t *testing.T) {
	listener, ok := servedSchema(t).Resources["envoy-infra:aws:WebhookListener"]
	require.True(t, ok, "the schema lists envoy-infra:aws:WebhookListener")
	require.True(t, listener.IsComponent)
	// A consumer owns its secrets; the component takes references to them.
	for name, input := range listener.InputProperties {
		require.False(t, input.Secret, "%s is not a secret input", name)
	}
	// Every input is required but the seed, each webhook's inputs, the opt-in to every
	// notification topic and adopt.
	require.ElementsMatch(t, []string{
		"seedSecretArn", "githubWebhookSecretArn", "slackSigningSecretArn", "reviewerAppId", "mentionTrigger",
		"subscribeAllTopics", "adopt",
	}, optionalInputs(listener.InputProperties, listener.RequiredInputs))
	require.Equal(t, "boolean", listener.InputProperties["subscribeAllTopics"].Type)
}

// optionalInputs are the names in properties that required does not list.
func optionalInputs[V any](properties map[string]V, required []string) []string {
	var optional []string
	for name := range properties {
		if !slices.Contains(required, name) {
			optional = append(optional, name)
		}
	}
	return optional
}

// Every integer input of every component the plugin serves refuses a fraction through Construct,
// as a typed SDK or Pulumi YAML program sends one, naming the component and the input, before the
// component registers anything. Each input is sent alone: the refusal runs before the component
// decodes its inputs, so no other input need be valid. The inputs come from the served schema, so
// an integer input is covered the day it is added.
func TestConstructRefusesAFractionInEveryIntegerInput(t *testing.T) {
	var covered int
	for typ, component := range servedSchema(t).Resources {
		for input, spec := range component.InputProperties {
			if spec.Type != "integer" {
				continue
			}
			covered++
			t.Run(typ+"/"+input, func(t *testing.T) {
				m := &pulumitest.Mocks{}
				_, err := pulumitest.Construct(t, m, typ, "example", map[string]pproperty.Value{input: pproperty.New(2.5)}, false, nil)
				require.EqualError(t, err, typ+" example: "+input+" 2.5 must be a whole number")
				require.Empty(t, m.Types())
			})
			// The check's refusal quotes the component's name, so a name holding an nkey seed is
			// left to the component, which refuses it first without quoting it.
			t.Run(typ+"/"+input+"/a seed as the name", func(t *testing.T) {
				seed, _ := testnats.User(t)
				m := &pulumitest.Mocks{}
				_, err := pulumitest.Construct(t, m, typ, seed, map[string]pproperty.Value{input: pproperty.New(2.5)}, false, nil)
				require.ErrorContains(t, err, "nkey seed")
				require.NotContains(t, err.Error(), seed)
				require.Empty(t, m.Types())
			})
		}
	}
	// aws:NatsService logRetentionDays; aws:WebhookListener logRetentionDays and natsReplicas;
	// gcp:Dispatch replicas, pprofPort and azureEmbedTokensPerMinute; gcp:NatsHub diskSizeGb;
	// host:Listener port.
	require.GreaterOrEqual(t, covered, 8)
}
