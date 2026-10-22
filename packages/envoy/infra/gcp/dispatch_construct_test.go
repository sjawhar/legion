package gcp_test

import (
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const dispatchConstructURN = "urn:pulumi:stack::project::envoy-infra:gcp:Dispatch::example-dispatch"

func dispatchConstructInputs() map[string]property.Value {
	return map[string]property.Value{
		"region":             property.New("us-central1"),
		"hostname":           property.New("example-dispatch"),
		"tailnetDnsName":     property.New("example-tailnet.ts.net"),
		"tags":               property.New(property.NewArray([]property.Value{property.New("tag:example-dispatch")})),
		"envoyImage":         property.New(dispatchEnvoyImage),
		"tailscaleImage":     property.New(dispatchTailscaleImage),
		"cloudSqlProxyImage": property.New(dispatchProxyImage),
		"signInClientId":     property.New("example-client-id"),
		"signInDomain":       property.New("example.com"),
	}
}

// setConstructApp sets the four GitHub App inputs, which come together.
func setConstructApp(inputs map[string]property.Value) {
	inputs["appClientId"] = property.New("example-app-client-id")
	inputs["appId"] = property.New("123456")
	inputs["appPrivateKeySecretName"] = property.New("example-app-key")
	inputs["appPrivateKeySecretKey"] = property.New("private-key.b64")
}

func constructDispatch(t *testing.T, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, p.ConstructResponse, error) {
	t.Helper()
	m := newMocks()
	response, err := pulumitest.Construct(t, m, dispatchType, "example-dispatch", inputs, dryRun, map[tokens.Package]p.ProviderReference{
		"gcp":    {Urn: resource.URN("urn:pulumi:s::p::" + gcpProviderType + "::gcp-example"), ID: "gcp-example-id"},
		"random": {Urn: resource.URN("urn:pulumi:s::p::" + randomProviderType + "::random-example"), ID: "random-example-id"},
	})
	return m, response, err
}

func TestConstructTakesDispatchInputsFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
	setConstructApp(inputs)
	for key, value := range map[string]property.Value{
		"tailnetAuthKey":          property.New("tskey-auth-example-0123456789").WithSecret(true),
		"natsUrl":                 property.New("nats://example-hub.example-tailnet.ts.net:4222"),
		"natsSeedSecret":          property.New("projects/example-project/secrets/example-nats-seed"),
		"signInClientSecret":      property.New("example-client-secret").WithSecret(true),
		"agentSecretsTokenSecret": property.New("projects/example-project/secrets/example-agent-secrets-token"),
	} {
		inputs[key] = value.WithDependencies([]resource.URN{upstream})
	}
	m, response, err := constructDispatch(t, inputs, false)
	require.NoError(t, err)
	require.NotEmpty(t, m.Registrations())
	url, ok := response.State.GetOk("url")
	require.True(t, ok)
	require.Equal(t, "https://example-dispatch.example-tailnet.ts.net", url.AsString())
}

func TestConstructAcceptsUnknownDispatchInputsUnderPreview(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
	setConstructApp(inputs)
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret", "agentSecretsTokenSecret"} {
		inputs[key] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	}
	_, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
}

func TestConstructRefusesDispatchPlainInputFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["hostname"] = property.New("example-dispatch").WithDependencies([]resource.URN{upstream})
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
	}
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "hostname")
	require.Empty(t, m.Types())
}

// replicas is a plain *int: Construct refuses another resource's output there, as it does for
// every plain input, before anything registers.
func TestConstructRefusesDispatchReplicasFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["replicas"] = property.New(3.0).WithDependencies([]resource.URN{upstream})
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
	}
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "replicas")
	require.Empty(t, m.Types())
}

// tailnetAuthKey, signInClientSecret, natsSeedSecret and agentSecretsTokenSecret are Inputs, which
// the component reads only once they resolve: a value holding an nkey seed is refused then, naming
// the input and never the value, so no child is registered with it, plainly or as a secret. Each
// arrives as another resource's output, as a consumer's does.
func TestConstructRefusesASeedInADispatchInput(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name, input string
		value       property.Value
	}{
		{"a seed inside the tailnet auth key", "tailnetAuthKey", property.New("tskey-auth-" + seed).WithSecret(true)},
		{"a lowercase seed as the sign-in client secret", "signInClientSecret", property.New(strings.ToLower(seed)).WithSecret(true)},
		{"a seed inside the agent secrets token secret id", "agentSecretsTokenSecret", property.New("projects/example-project/secrets/" + seed)},
		{"a seed inside the NATS seed secret id", "natsSeedSecret", property.New("projects/example-project/secrets/" + seed)},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := dispatchConstructInputs()
			inputs["tailnetAuthKey"] = property.New("tskey-auth-example-0123456789").WithSecret(true)
			inputs["natsUrl"] = property.New("nats://example-hub.example-tailnet.ts.net:4222")
			inputs["natsSeedSecret"] = property.New("projects/example-project/secrets/example-nats-seed")
			inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
			inputs["agentSecretsTokenSecret"] = property.New("projects/example-project/secrets/example-agent-secrets-token")
			setConstructApp(inputs)
			inputs["signInClientSecret"] = property.New("example-client-secret").WithSecret(true)
			inputs[row.input] = row.value.WithDependencies([]resource.URN{upstream})
			m, _, err := constructDispatch(t, inputs, false)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.input+" holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}
