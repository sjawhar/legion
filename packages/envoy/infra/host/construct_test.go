package host_test

import (
	"slices"
	"strings"
	"testing"
	"unicode"

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

// carriedText is every string in inputs, one per line, secrets and nested values included: what a
// registration carries to the engine and the provider.
func carriedText(inputs resource.PropertyMap) string {
	var texts []string
	var walk func(v resource.PropertyValue)
	walk = func(v resource.PropertyValue) {
		switch {
		case v.IsString():
			texts = append(texts, v.StringValue())
		case v.IsSecret():
			walk(v.SecretValue().Element)
		case v.IsOutput():
			walk(v.OutputValue().Element)
		case v.IsArray():
			for _, e := range v.ArrayValue() {
				walk(e)
			}
		case v.IsObject():
			for k, e := range v.ObjectValue() {
				texts = append(texts, string(k))
				walk(e)
			}
		}
	}
	walk(resource.NewObjectProperty(inputs))
	return strings.Join(texts, "\n")
}

// A listener refuses an nkey seed in each of its inputs but seed, through the plugin's
// Construct as a consumer's program reaches it, naming the input and never quoting the seed, and
// no resource it registers carries the seed in its inputs or aliases. machineId, hostBridge and
// natsUrls are refused by core, the rest by the listener. Each row's clean value, in the same
// place, constructs the listener. seed is the one input that is a seed, and is accepted.
func TestConstructRefusesASeedInEveryInputButSeed(t *testing.T) {
	const url = "nats://hub.internal.example:4222"
	// split cuts v in two DNS labels; joined is text in either case with '.', '-', '_' and
	// whitespace removed, as a reader joins a seed split over them.
	split := func(v string) string { return v[:len(v)/2] + "." + v[len(v)/2:] }
	joined := func(text string) string {
		return strings.Map(func(r rune) rune {
			if strings.ContainsRune(".-_ \t\r\n", r) {
				return -1
			}
			return unicode.ToUpper(r)
		}, text)
	}
	shown := func(text string) bool { return strings.Contains(joined(text), seedValue) }
	rows := []struct {
		name  string
		setup func(inputs map[string]property.Value, v string) string // returns the listener's name
		want  string
		clean string
	}{
		{"the listener's name", func(_ map[string]property.Value, v string) string { return "host-" + v },
			listenerType + ": the listener's name holds an nkey seed (not echoed)", "example"},
		{"the listener's name, lowercase", func(_ map[string]property.Value, v string) string { return "host-" + strings.ToLower(v) },
			listenerType + ": the listener's name holds an nkey seed (not echoed)", "example"},
		{"machineId", func(in map[string]property.Value, v string) string {
			in["machineId"] = property.New("host-" + v)
			return "example-host-a"
		}, "machineId holds an nkey seed (not echoed)", "example"},
		{"natsUrls", func(in map[string]property.Value, v string) string {
			in["natsUrls"] = property.New(property.NewArray([]property.Value{
				property.New(url), property.New("nats://example:" + v + "@hub.internal.example:4222"),
			}))
			return "example-host-a"
		}, "natsUrls[1] holds an nkey seed (not echoed)", "password"},
		{"image", func(in map[string]property.Value, v string) string {
			in["image"] = property.New("ghcr.io/example/envoy:" + v)
			return "example-host-a"
		}, "image holds an nkey seed (not echoed)", "example"},
		{"image, lowercase", func(in map[string]property.Value, v string) string {
			in["image"] = property.New("ghcr.io/example/envoy:" + strings.ToLower(v))
			return "example-host-a"
		}, "image holds an nkey seed (not echoed)", "example"},
		{"containerName", func(in map[string]property.Value, v string) string {
			in["containerName"] = property.New("envoy-" + v)
			return "example-host-a"
		}, "containerName holds an nkey seed (not echoed)", "example"},
		{"hostBridge", func(in map[string]property.Value, v string) string {
			in["hostBridge"] = property.New("bridge-" + v + ".example")
			return "example-host-a"
		}, "hostBridge holds an nkey seed (not echoed)", "example"},
		{"hostBridge, split across DNS labels", func(in map[string]property.Value, v string) string {
			in["hostBridge"] = property.New(split(v) + ".example")
			return "example-host-a"
		}, "hostBridge holds an nkey seed (not echoed)", "cleanlabels"},
		{"watchdog.host", func(in map[string]property.Value, v string) string {
			in["watchdog"] = object(map[string]string{"host": "host-" + v + ".example"})
			return "example-host-a"
		}, "watchdog.host holds an nkey seed (not echoed)", "example"},
		{"watchdog.host, split across DNS labels", func(in map[string]property.Value, v string) string {
			in["watchdog"] = object(map[string]string{"host": split(v) + ".example"})
			return "example-host-a"
		}, "watchdog.host holds an nkey seed (not echoed)", "cleanlabels"},
		{"adopt.container", func(in map[string]property.Value, v string) string {
			in["adopt"] = object(map[string]string{"container": "envoy-listener-" + v})
			return "example-host-a"
		}, "adopt.container holds an nkey seed (not echoed)", "example"},
		{"adopt.image", func(in map[string]property.Value, v string) string {
			in["adopt"] = object(map[string]string{"image": "envoy-image-" + v})
			return "example-host-a"
		}, "adopt.image holds an nkey seed (not echoed)", "example"},
		{"adopt.watchdog", func(in map[string]property.Value, v string) string {
			in["watchdog"] = object(map[string]string{"host": "example-host-a"})
			in["adopt"] = object(map[string]string{"watchdog": "envoy-watchdog-" + v})
			return "example-host-a"
		}, "adopt.watchdog holds an nkey seed (not echoed)", "example"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			inputs := plainInputs()
			inputs["natsUrls"] = property.New(property.NewArray([]property.Value{property.New(url)}))
			name := row.setup(inputs, seedValue)
			m, err := constructNamed(t, name, inputs)
			for _, r := range m.Registrations() {
				require.False(t, shown(r.Name+"\n"+carriedText(r.Inputs)), "%s %s carries the seed", r.Type, r.Name)
				for _, alias := range r.RPC.GetAliases() {
					require.False(t, shown(alias.String()), "%s %s's alias carries the seed", r.Type, r.Name)
				}
			}
			require.Error(t, err, "a seed is refused")
			require.False(t, shown(err.Error()), "the refusal quotes the seed")
			require.ErrorContains(t, err, row.want)

			inputs = plainInputs()
			inputs["natsUrls"] = property.New(property.NewArray([]property.Value{property.New(url)}))
			name = row.setup(inputs, row.clean)
			m, err = constructNamed(t, name, inputs)
			require.NoError(t, err, "the clean value constructs the listener")
			container := m.One(t, containerType)
			var carried []string
			for _, r := range m.Registrations() {
				carried = append(carried, r.Name, carriedText(r.Inputs))
				for _, alias := range r.RPC.GetAliases() {
					carried = append(carried, alias.String())
				}
			}
			require.Contains(t, joined(strings.Join(carried, "\n")), joined(row.clean), "the clean value reaches a resource")
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
