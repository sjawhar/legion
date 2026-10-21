package gcp_test

import (
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
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
	for key, value := range map[string]property.Value{
		"tailnetAuthKey":     property.New("tskey-auth-example-0123456789").WithSecret(true),
		"natsUrl":            property.New("nats://example-hub.example-tailnet.ts.net:4222"),
		"natsSeedSecret":     property.New("projects/example-project/secrets/example-nats-seed"),
		"signInClientSecret": property.New("example-client-secret").WithSecret(true),
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
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
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
