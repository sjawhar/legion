package host_test

import (
	"slices"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/provider"
)

// The plugin's Construct copies a consumer's inputs into ListenerArgs, and refuses a plain-typed
// field whose input carries a dependency or is unknown (Pulumi SDK v3.265.0
// go/pulumi/provider.go:395, :765-766), a path pulumi.WithMocks never takes. natsUrls and seed
// are the inputs a consumer takes from other resources or stacks, so these tests send them as
// outputs of another resource, then unknown as under preview.

// upstream is the resource natsUrls and seed come from.
var upstream = resource.URN("urn:pulumi:s::p::example:index:Hub::hub")

// construct serves the plugin in process with the mocks behind it, constructs a listener named
// example-host-a from inputs with a docker provider, and returns what the mocks saw and
// Construct's error.
func construct(t *testing.T, inputs map[string]property.Value, dryRun bool) (*mocks, error) {
	t.Helper()
	prov, err := provider.New()
	require.NoError(t, err)
	m := &mocks{}
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov), integration.WithMocks(m))
	require.NoError(t, err)
	_, err = server.Construct(p.ConstructRequest{
		Urn:    resource.NewURN("s", "p", "", tokens.Type(listenerType), "example-host-a"),
		DryRun: dryRun,
		Inputs: property.NewMap(inputs),
		Providers: map[tokens.Package]p.ProviderReference{
			"docker": {Urn: resource.URN("urn:pulumi:s::p::pulumi:providers:docker::docker-host"), ID: "docker-host-id"},
		},
	})
	return m, err
}

// plainInputs are exampleArgs' plain fields, as a consumer sends them.
func plainInputs() map[string]property.Value {
	return map[string]property.Value{
		"machineId":  property.New("example-host-a"),
		"image":      property.New(exampleImage),
		"hostBridge": property.New("127.0.0.1"),
	}
}

func TestConstructTakesNatsUrlsAndSeedFromAnotherResource(t *testing.T) {
	inputs := plainInputs()
	inputs["natsUrls"] = property.New(property.NewArray([]property.Value{
		property.New("nats://hub.internal.example:4222"),
	})).WithDependencies([]resource.URN{upstream})
	inputs["seed"] = property.New("SUEXAMPLESEED").WithSecret(true).WithDependencies([]resource.URN{upstream})
	m, err := construct(t, inputs, false)
	require.NoError(t, err)

	container := m.one(t, containerType)
	require.Equal(t, exampleContainer, withoutEnvs(container))
	envs, secret := envsOf(t, container)
	require.ElementsMatch(t, append(slices.Clone(exampleEnvs), "NATS_NKEY_SEED=SUEXAMPLESEED"), envs)
	require.True(t, secret, "envs holding the seed are secret")
	require.Contains(t, container.rpc.GetPropertyDependencies()["envs"].GetUrns(), string(upstream),
		"the container's envs depend on the resource natsUrls and seed come from")
}

func TestConstructTakesUnknownNatsUrlsAndSeedUnderPreview(t *testing.T) {
	inputs := plainInputs()
	inputs["natsUrls"] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	inputs["seed"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	m, err := construct(t, inputs, true)
	require.NoError(t, err)

	container := m.one(t, containerType)
	require.Equal(t, exampleContainer, withoutEnvs(container))
	envs, _ := plain(container.inputs["envs"])
	require.True(t, envs.IsComputed(), "envs from unknown inputs are unknown, not %s", envs.TypeString())
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the listener registers anything. This is the path the two tests above prove natsUrls and seed
// stay clear of.
func TestConstructRefusesAPlainFieldFromAnotherResource(t *testing.T) {
	inputs := plainInputs()
	inputs["machineId"] = property.New("example-host-a").WithDependencies([]resource.URN{upstream})
	inputs["natsUrls"] = property.New(property.NewArray([]property.Value{
		property.New("nats://hub.internal.example:4222"),
	}))
	m, err := construct(t, inputs, false)
	require.ErrorContains(t, err, "machineId")
	require.Empty(t, m.types())
}
