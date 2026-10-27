package gcp_test

import (
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// clusteredResolvedSeedInputs are gcp:ClusteredNatsHub's string Inputs, which may come from
// another resource and so are checked for a seed once they resolve
// (TestClusteredNatsHubRefusesABlankOrSeededSecretOnceItResolves), not by the resolver.
var clusteredResolvedSeedInputs = []string{"kubeconfig", "tailnetAuthKey"}

// constructClustered serves the plugin in process with the mocks behind it and constructs a
// clustered hub named example-clustered-hub from inputs, as a consumer's program does. The
// component takes no provider of the consumer's: it builds its own Kubernetes provider.
func constructClustered(t *testing.T, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, p.ConstructResponse, error) {
	t.Helper()
	m := newMocks()
	resp, err := pulumitest.Construct(t, m, clusteredHubType, "example-clustered-hub", inputs, dryRun, nil)
	return m, resp, err
}

// stringArray is values as a property array.
func stringArray(values ...string) property.Value {
	array := make([]property.Value, len(values))
	for i, v := range values {
		array[i] = property.New(v)
	}
	return property.New(property.NewArray(array))
}

// clusteredPlainInputs are clusteredArgs' plain fields, as a consumer sends them, the users
// plainInputs sends to gcp:NatsHub among them.
func clusteredPlainInputs() map[string]property.Value {
	return map[string]property.Value{
		"namespace":        property.New("envoy"),
		"name":             property.New("envoy-nats"),
		"serviceName":      property.New("envoy-nats"),
		"natsImage":        property.New(natsImage),
		"reloaderImage":    property.New(reloaderImage),
		"exporterImage":    property.New(exporterImage),
		"clientNamespaces": stringArray("envoy", "dispatch"),
		"users":            plainInputs()["users"],
		"sessionUser":      plainInputs()["sessionUser"],
		"tailnet": property.New(property.NewMap(map[string]property.Value{
			"serviceName":    property.New("envoy-hub"),
			"tailnetDnsName": property.New("example-tailnet.ts.net"),
			"tags":           stringArray("tag:envoy-hub-host"),
			"tailscaleImage": property.New(tailscaleImage),
			"listener": property.New(property.NewMap(map[string]property.Value{
				"image":          property.New(listenerImage),
				"nkey":           property.New(keyListener),
				"seedSecretName": property.New("envoy-hub-listener-seed"),
				"seedSecretKey":  property.New("seed"),
			})),
		})),
	}
}

// requireClusteredOutputs requires Construct's state to hold the hub's outputs, known and plain,
// the tailnet URLs only with tailnet.
func requireClusteredOutputs(t *testing.T, resp p.ConstructResponse, tailnet bool) {
	t.Helper()
	want := map[string]string{
		"serviceUrl":      "nats://envoy-nats.envoy.svc.cluster.local:4222",
		"statefulSetName": "envoy-nats",
	}
	if tailnet {
		want["tailnetNatsUrl"] = "nats://envoy-hub.example-tailnet.ts.net:4222"
		want["tailnetEnvoyUrl"] = "http://envoy-hub.example-tailnet.ts.net:9020"
	}
	for key, value := range want {
		got, ok := resp.State.GetOk(key)
		require.True(t, ok, "the hub's state holds %s", key)
		require.False(t, got.IsComputed(), "%s is known: it comes from plain inputs alone", key)
		require.False(t, got.Secret(), "%s is no secret", key)
		require.Equal(t, value, got.AsString(), key)
	}
	if !tailnet {
		for _, key := range []string{"tailnetNatsUrl", "tailnetEnvoyUrl"} {
			got, ok := resp.State.GetOk(key)
			require.True(t, !ok || got.IsNull(), "%s is unset without tailnet, not %v", key, got)
		}
	}
	selector, ok := resp.State.GetOk("podSelector")
	require.True(t, ok, "the hub's state holds podSelector")
	require.False(t, selector.IsComputed())
	labels := map[string]string{}
	for key, value := range selector.AsMap().All {
		labels[key] = value.AsString()
	}
	require.Equal(t, map[string]string{
		"app.kubernetes.io/name": "nats", "app.kubernetes.io/instance": "envoy-nats", "app.kubernetes.io/component": "nats",
	}, labels)
}

// The plugin's Construct copies a consumer's inputs into ClusteredNatsHubArgs. kubeconfig and
// tailnetAuthKey are the inputs a consumer takes from its own resources (the cluster's
// kubeconfig, a generated key), so they arrive as another resource's secret outputs.
func TestConstructTakesTheKubeconfigAndAuthKeyFromAnotherResource(t *testing.T) {
	inputs := clusteredPlainInputs()
	inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true).WithDependencies([]resource.URN{upstream})
	inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true).WithDependencies([]resource.URN{upstream})
	m, resp, err := constructClustered(t, inputs, false)
	require.NoError(t, err)
	require.ElementsMatch(t, clusteredHubTypes, m.Types())
	value, secret := pulumitest.Plain(m.Named(t, kubernetesProviderType, "envoy-nats-kubernetes").Inputs["kubeconfig"])
	require.True(t, secret, "the kubeconfig reaches the hub's provider as a secret")
	require.Equal(t, kubeconfig, value.StringValue())
	requireClusteredOutputs(t, resp, true)
}

// At preview the kubeconfig and the key may be unknown (the cluster and the key are created in
// the same update): the resolver judges them by being set, and the outputs stay known.
func TestConstructTakesAnUnknownKubeconfigAndAuthKeyUnderPreview(t *testing.T) {
	inputs := clusteredPlainInputs()
	inputs["kubeconfig"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	inputs["tailnetAuthKey"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	_, resp, err := constructClustered(t, inputs, true)
	require.NoError(t, err)
	requireClusteredOutputs(t, resp, true)
}

// Without tailnet the hub takes no tailnetAuthKey and outputs no tailnet URL.
func TestConstructTakesAHubWithoutTailnet(t *testing.T) {
	inputs := clusteredPlainInputs()
	delete(inputs, "tailnet")
	inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true).WithDependencies([]resource.URN{upstream})
	_, resp, err := constructClustered(t, inputs, false)
	require.NoError(t, err)
	requireClusteredOutputs(t, resp, false)

	inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true)
	m, _, err := constructClustered(t, inputs, false)
	require.ErrorContains(t, err, "envoy-infra:gcp:ClusteredNatsHub example-clustered-hub: tailnetAuthKey is given without tailnet")
	require.Empty(t, m.Types())
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the hub registers anything, the integer and nested ones too.
func TestConstructRefusesAClusteredHubPlainFieldFromAnotherResource(t *testing.T) {
	for input, value := range map[string]property.Value{
		"name":         property.New("envoy-nats"),
		"natsReplicas": property.New(3.0),
		"natsMemory":   property.New("4Gi"),
	} {
		t.Run(input, func(t *testing.T) {
			inputs := clusteredPlainInputs()
			inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true)
			inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true)
			inputs[input] = value.WithDependencies([]resource.URN{upstream})
			m, _, err := constructClustered(t, inputs, false)
			require.ErrorContains(t, err, input)
			require.Empty(t, m.Types())
		})
	}
}

// tailnet.replicas is an integer inside an object input, which provider's fraction test, walking
// the top-level inputs, does not send: the plugin refuses a fraction there too, naming its path,
// before the component registers anything, and takes a whole number sent as a float.
func TestConstructRefusesAFractionalTailnetReplicaCount(t *testing.T) {
	for value, want := range map[float64]string{2.5: "tailnet.replicas 2.5 must be a whole number", 3.0: ""} {
		inputs := clusteredPlainInputs()
		inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true)
		inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true)
		inputs["tailnet"] = property.New(inputs["tailnet"].AsMap().Set("replicas", property.New(value)))
		m, _, err := constructClustered(t, inputs, false)
		if want == "" {
			require.NoError(t, err)
			continue
		}
		require.EqualError(t, err, clusteredHubType+" example-clustered-hub: "+want)
		require.Empty(t, m.Types())
	}
}

// kubeconfig and tailnetAuthKey are secret Inputs, which the hub reads only once they resolve,
// through the one rule gcp:Dispatch's credentials run: a blank or whitespace-only value, which no
// credential is, and a value holding an nkey seed are refused then, naming the input and never
// the value, and the value stays secret. Each arrives as another resource's output under the
// mocks, as a consumer's does.
func TestClusteredNatsHubRefusesABlankOrSeededSecretOnceItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, input := range clusteredResolvedSeedInputs {
		for _, row := range []struct{ name, value, want string }{
			{"empty", "", input + " is blank, which no credential is"},
			{"whitespace only", " \n\t", input + " is blank, which no credential is"},
			{"a seed", seed, input + " holds an nkey seed (not echoed)"},
			{"a lowercase seed", strings.ToLower(seed), input + " holds an nkey seed (not echoed)"},
			{"a seed split by hyphens", splitSeed(seed, "-"), input + " holds an nkey seed (not echoed)"},
		} {
			t.Run(input+", "+row.name, func(t *testing.T) {
				m := newMocks()
				var awaited error
				require.NoError(t, m.Run(func(ctx *pulumi.Context) error {
					_, awaited = internals.UnsafeAwaitOutput(t.Context(), gcp.RefuseClusteredHubSecret(pulumi.String(row.value), "example-clustered-hub", input))
					return nil
				}))
				require.EqualError(t, awaited, "envoy-infra:gcp:ClusteredNatsHub example-clustered-hub: "+row.want)
				require.NotContains(t, strings.ToUpper(awaited.Error()), seed)
			})
		}
		t.Run(input+", a usable value", func(t *testing.T) {
			m := newMocks()
			require.NoError(t, m.Run(func(ctx *pulumi.Context) error {
				out, err := internals.UnsafeAwaitOutput(t.Context(), gcp.RefuseClusteredHubSecret(pulumi.String(authKey), "example-clustered-hub", input))
				require.NoError(t, err)
				require.True(t, out.Secret, "the value stays secret")
				require.Equal(t, authKey, out.Value)
				return nil
			}))
		})
	}
}

// The hub builds its provider from kubeconfig only through that check: a blank kubeconfig and
// one holding an nkey seed, each another resource's secret output sent through Construct, fail
// the update naming kubeconfig and never the value, and no registration carries the value.
func TestConstructRefusesABlankOrSeededKubeconfig(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct{ name, value, want string }{
		{"blank", " \n\t", "kubeconfig is blank, which no credential is"},
		{"a seed inside it", kubeconfig + "# " + strings.ToLower(seed) + "\n", "kubeconfig holds an nkey seed (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := clusteredPlainInputs()
			inputs["kubeconfig"] = property.New(row.value).WithSecret(true).WithDependencies([]resource.URN{upstream})
			inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true)
			m, _, err := constructClustered(t, inputs, false)
			require.ErrorContains(t, err, clusteredHubType+" example-clustered-hub: "+row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.NotContains(t, m.Types(), kubernetesProviderType, "no provider is built from it")
			for _, r := range m.Registrations() {
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}
