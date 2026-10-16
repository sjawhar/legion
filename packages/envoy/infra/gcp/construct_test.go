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

// The plugin's Construct copies a consumer's inputs into NatsHubArgs, and refuses a plain-typed
// field whose input carries a dependency or is unknown (Pulumi SDK v3.265.0
// go/pulumi/provider.go:764-776), a path pulumi.WithMocks never takes. tailnetAuthKey, network
// and subnetwork are the inputs a consumer takes from its own resources, so these tests send each
// as an output of another resource, then unknown as under preview.

// upstream is the resource tailnetAuthKey, network and subnetwork come from.
var upstream = resource.URN("urn:pulumi:s::p::example:index:Network::upstream")

// construct serves the plugin in process with gcp's mocks behind it, constructs a hub named
// example-hub from inputs with a gcp provider, and returns what the mocks saw, Construct's
// response and its error.
func construct(t *testing.T, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, p.ConstructResponse, error) {
	t.Helper()
	m := newMocks()
	resp, err := pulumitest.Construct(t, m, hubType, "example-hub", inputs, dryRun, map[tokens.Package]p.ProviderReference{
		"gcp": {Urn: resource.URN("urn:pulumi:s::p::" + gcpProviderType + "::gcp-example"), ID: "gcp-example-id"},
	})
	return m, resp, err
}

// plainInputs are exampleArgs' plain fields, as a consumer sends them.
func plainInputs() map[string]property.Value {
	return map[string]property.Value{
		"users": property.New(property.NewArray([]property.Value{
			property.New(property.NewMap(map[string]property.Value{
				"name":      property.New("example-host-a"),
				"nkey":      property.New(keyA),
				"machineId": property.New("example-host-a"),
			})),
			property.New(property.NewMap(map[string]property.Value{
				"name": property.New("example-operator"),
				"nkey": property.New(keyB),
				"permissions": property.New(property.NewMap(map[string]property.Value{
					"publish": property.New(property.NewMap(map[string]property.Value{
						"allow": property.New(property.NewArray([]property.Value{property.New(">")})),
					})),
					"subscribe": property.New(property.NewMap(map[string]property.Value{
						"allow": property.New(property.NewArray([]property.Value{property.New(">")})),
						"deny":  property.New(property.NewArray([]property.Value{property.New("$SYS.>")})),
					})),
				})),
			})),
		})),
		"hostname":       property.New("example-hub"),
		"tags":           property.New(property.NewArray([]property.Value{property.New("tag:example-host")})),
		"zone":           property.New("us-central1-a"),
		"natsImage":      property.New(natsImage),
		"tailscaleImage": property.New(tailscaleImage),
	}
}

// requireNatsURL requires Construct's state to hold natsUrl, known and plain.
func requireNatsURL(t *testing.T, resp p.ConstructResponse) {
	t.Helper()
	natsURL, ok := resp.State.GetOk("natsUrl")
	require.True(t, ok, "the hub's state holds natsUrl")
	require.False(t, natsURL.IsComputed(), "natsUrl is known: it comes from hostname alone")
	require.False(t, natsURL.Secret(), "natsUrl is no secret")
	require.Equal(t, "nats://example-hub:4222", natsURL.AsString())
}

func TestConstructTakesTheAuthKeyAndNetworkFromAnotherResource(t *testing.T) {
	inputs := plainInputs()
	inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true).WithDependencies([]resource.URN{upstream})
	inputs["network"] = property.New("example-network").WithDependencies([]resource.URN{upstream})
	inputs["subnetwork"] = property.New("example-subnetwork").WithDependencies([]resource.URN{upstream})
	m, resp, err := construct(t, inputs, false)
	require.NoError(t, err)

	requireChildren(t, m, constructURN, wantChildren(t, "example-network", "example-subnetwork"), nil)
	requireNatsURL(t, resp)
	for typ, field := range map[string]string{
		versionType:  "secretData",
		firewallType: "network",
		templateType: "networkInterfaces",
	} {
		require.Contains(t, m.One(t, typ).RPC.GetPropertyDependencies()[field].GetUrns(), string(upstream),
			"the %s's %s depends on the resource its input comes from", typ, field)
	}
}

func TestConstructTakesAnUnknownAuthKeyAndNetworkUnderPreview(t *testing.T) {
	inputs := plainInputs()
	inputs["tailnetAuthKey"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	inputs["network"] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	inputs["subnetwork"] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	m, resp, err := construct(t, inputs, true)
	require.NoError(t, err)

	// The children are the ones a known network records; only the inputs taken from the
	// unknown network and auth key are unknown.
	requireChildren(t, m, constructURN, wantChildren(t, "example-network", "example-subnetwork"),
		map[string][]string{firewallType: {"network"}, templateType: {"networkInterfaces"}})
	data, _ := pulumitest.Plain(m.One(t, versionType).Inputs["secretData"])
	require.True(t, data.IsComputed(), "the secret version's secretData is unknown, not %s", data.TypeString())
	requireNatsURL(t, resp)
}

// tailnetAuthKey is a secret Input, which the hub reads only once it resolves: a value holding an
// nkey seed is refused then, naming the input and never the value, so no Secret Manager version
// is written with it and no registration carries it, plainly or as a secret. The key arrives as a
// secret from another resource, as a consumer's does, and in each spelling the detector reads.
func TestConstructRefusesASeedAsTheTailnetAuthKey(t *testing.T) {
	seed, _ := testnats.User(t)
	for name, value := range map[string]string{
		"a seed":                  seed,
		"a lowercase seed":        strings.ToLower(seed),
		"a seed inside a tskey":   "tskey-auth-" + seed,
		"a seed split by hyphens": splitSeed(seed, "-"),
	} {
		t.Run(name, func(t *testing.T) {
			inputs := plainInputs()
			inputs["tailnetAuthKey"] = property.New(value).WithSecret(true).WithDependencies([]resource.URN{upstream})
			m, _, err := construct(t, inputs, false)
			require.ErrorContains(t, err, "envoy-infra:gcp:NatsHub example-hub: tailnetAuthKey: an nkey user seed, "+
				"which is never an input of the hub (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the hub registers anything. This is the path the two tests above prove tailnetAuthKey, network
// and subnetwork stay clear of.
func TestConstructRefusesAPlainFieldFromAnotherResource(t *testing.T) {
	inputs := plainInputs()
	inputs["hostname"] = property.New("example-hub").WithDependencies([]resource.URN{upstream})
	inputs["tailnetAuthKey"] = property.New(authKey).WithSecret(true)
	m, _, err := construct(t, inputs, false)
	require.ErrorContains(t, err, "hostname")
	require.Empty(t, m.Types())
}
