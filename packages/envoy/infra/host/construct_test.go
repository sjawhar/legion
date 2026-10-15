package host_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// The plugin's Construct copies a consumer's inputs into ListenerArgs, and refuses a plain-typed
// field whose input carries a dependency or is unknown (Pulumi SDK v3.265.0
// go/pulumi/provider.go:395, :765-766), a path pulumi.WithMocks never takes. natsUrls and seed
// are the inputs a consumer takes from other resources or stacks, so these tests send them as
// outputs of another resource, then unknown as under preview.

// upstream is the resource natsUrls and seed come from.
var upstream = resource.URN("urn:pulumi:s::p::example:index:Hub::hub")

// construct serves the plugin in process with host's mocks behind it, constructs a listener named
// example-host-a from inputs with a docker provider, and returns what the mocks saw and
// Construct's error.
func construct(t *testing.T, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	_, err := pulumitest.Construct(t, m, listenerType, "example-host-a", inputs, dryRun, map[tokens.Package]p.ProviderReference{
		"docker": {Urn: resource.URN("urn:pulumi:s::p::pulumi:providers:docker::docker-host"), ID: "docker-host-id"},
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

	container := m.One(t, containerType)
	require.Equal(t, exampleContainer, withoutEnvs(container))
	envs, secret := envsOf(t, container)
	require.ElementsMatch(t, append(slices.Clone(exampleEnvs), "NATS_NKEY_SEED=SUEXAMPLESEED"), envs)
	require.True(t, secret, "envs holding the seed are secret")
	require.Contains(t, container.RPC.GetPropertyDependencies()["envs"].GetUrns(), string(upstream),
		"the container's envs depend on the resource natsUrls and seed come from")
}

func TestConstructTakesUnknownNatsUrlsAndSeedUnderPreview(t *testing.T) {
	inputs := plainInputs()
	inputs["natsUrls"] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	inputs["seed"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	m, err := construct(t, inputs, true)
	require.NoError(t, err)

	container := m.One(t, containerType)
	require.Equal(t, exampleContainer, withoutEnvs(container))
	envs, _ := pulumitest.Plain(container.Inputs["envs"])
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
	require.Empty(t, m.Types())
}

// seedValue is a user seed (nkeys v0.4.15 README.md line 45).
const seedValue = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"

// constructNamed serves the plugin in process with host's mocks behind it and constructs a
// listener named name from inputs, with a docker and a command provider, as a consumer's program
// does, and returns what the mocks saw and Construct's error.
func constructNamed(t *testing.T, name string, inputs map[string]property.Value) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	_, err := pulumitest.Construct(t, m, listenerType, name, inputs, false, map[tokens.Package]p.ProviderReference{
		"docker":  {Urn: resource.URN("urn:pulumi:s::p::pulumi:providers:docker::docker-host"), ID: "docker-host-id"},
		"command": {Urn: resource.URN("urn:pulumi:s::p::pulumi:providers:command::command-host"), ID: "command-host-id"},
	})
	return m, err
}

// object is a property map of string fields.
func object(fields map[string]string) property.Value {
	m := map[string]property.Value{}
	for k, v := range fields {
		m[k] = property.New(v)
	}
	return property.New(property.NewMap(m))
}

// A listener refuses an nkey user seed in each of its inputs but seed, through the plugin's
// Construct as a consumer's program reaches it, naming the input and never quoting the seed, and
// no resource it registers carries the seed in its inputs or aliases. machineId, hostBridge and
// natsUrls are refused by core, the rest by the listener. Each row's clean value, in the same
// place, constructs the listener. seed is the one input that is a seed, and is accepted.
func TestConstructRefusesASeedInEveryInputButSeed(t *testing.T) {
	const url = "nats://hub.internal.example:4222"
	rows := []struct {
		name  string
		setup func(inputs map[string]property.Value, v string) string // returns the listener's name
		want  string
		clean string
	}{
		{"the listener's name", func(_ map[string]property.Value, v string) string { return "host-" + v },
			listenerType + ": the listener's name holds an nkey user seed (not echoed)", "example"},
		{"machineId", func(in map[string]property.Value, v string) string {
			in["machineId"] = property.New("host-" + v)
			return "example-host-a"
		}, "machineId holds an nkey user seed (not echoed)", "example"},
		{"natsUrls", func(in map[string]property.Value, v string) string {
			in["natsUrls"] = property.New(property.NewArray([]property.Value{
				property.New(url), property.New("nats://example:" + v + "@hub.internal.example:4222"),
			}))
			return "example-host-a"
		}, "natsUrls[1] holds an nkey user seed (not echoed)", "password"},
		{"image", func(in map[string]property.Value, v string) string {
			in["image"] = property.New("ghcr.io/example/envoy:" + v)
			return "example-host-a"
		}, "image holds an nkey user seed (not echoed)", "example"},
		{"containerName", func(in map[string]property.Value, v string) string {
			in["containerName"] = property.New("envoy-" + v)
			return "example-host-a"
		}, "containerName holds an nkey user seed (not echoed)", "example"},
		{"hostBridge", func(in map[string]property.Value, v string) string {
			in["hostBridge"] = property.New("bridge-" + v + ".example")
			return "example-host-a"
		}, "hostBridge holds an nkey user seed (not echoed)", "example"},
		{"watchdog.host", func(in map[string]property.Value, v string) string {
			in["watchdog"] = object(map[string]string{"host": "host-" + v + ".example"})
			return "example-host-a"
		}, "watchdog.host holds an nkey user seed (not echoed)", "example"},
		{"adopt.container", func(in map[string]property.Value, v string) string {
			in["adopt"] = object(map[string]string{"container": "envoy-listener-" + v})
			return "example-host-a"
		}, "adopt.container holds an nkey user seed (not echoed)", "example"},
		{"adopt.image", func(in map[string]property.Value, v string) string {
			in["adopt"] = object(map[string]string{"image": "envoy-image-" + v})
			return "example-host-a"
		}, "adopt.image holds an nkey user seed (not echoed)", "example"},
		{"adopt.watchdog", func(in map[string]property.Value, v string) string {
			in["watchdog"] = object(map[string]string{"host": "example-host-a"})
			in["adopt"] = object(map[string]string{"watchdog": "envoy-watchdog-" + v})
			return "example-host-a"
		}, "adopt.watchdog holds an nkey user seed (not echoed)", "example"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			inputs := plainInputs()
			inputs["natsUrls"] = property.New(property.NewArray([]property.Value{property.New(url)}))
			name := row.setup(inputs, seedValue)
			m, err := constructNamed(t, name, inputs)
			require.Error(t, err, "a seed is refused")
			require.NotContains(t, err.Error(), seedValue, "the refusal quotes the seed")
			require.ErrorContains(t, err, row.want)
			for _, r := range m.Registrations() {
				require.NotContains(t, r.Name+fmt.Sprint(r.Inputs.Mappable()), seedValue, "%s %s carries the seed", r.Type, r.Name)
				for _, alias := range r.RPC.GetAliases() {
					require.NotContains(t, alias.String(), seedValue, "%s %s's alias carries the seed", r.Type, r.Name)
				}
			}

			inputs = plainInputs()
			inputs["natsUrls"] = property.New(property.NewArray([]property.Value{property.New(url)}))
			name = row.setup(inputs, row.clean)
			m, err = constructNamed(t, name, inputs)
			require.NoError(t, err, "the clean value constructs the listener")
			container := m.One(t, containerType)
			var carried []string
			for _, r := range m.Registrations() {
				carried = append(carried, r.Name+fmt.Sprint(r.Inputs.Mappable()))
				for _, alias := range r.RPC.GetAliases() {
					carried = append(carried, alias.String())
				}
			}
			require.Contains(t, strings.Join(carried, "\n"), row.clean, "the clean value reaches a resource")
			require.NotEmpty(t, container.Name)
		})
	}
}

// seed is the one input that is a seed: it is accepted, and goes into the container's environment
// as a secret.
func TestConstructAcceptsASeedAsSeed(t *testing.T) {
	inputs := plainInputs()
	inputs["natsUrls"] = property.New(property.NewArray([]property.Value{property.New("nats://hub.internal.example:4222")}))
	inputs["seed"] = property.New(seedValue).WithSecret(true)
	m, err := constructNamed(t, "example-host-a", inputs)
	require.NoError(t, err)
	envs, secret := envsOf(t, m.One(t, containerType))
	require.Contains(t, envs, "NATS_NKEY_SEED="+seedValue)
	require.True(t, secret, "envs holding the seed are secret")
}
