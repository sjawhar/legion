package gcp_test

import (
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

// The plugin's Construct copies a consumer's inputs into NatsHubArgs, and refuses a plain-typed
// field whose input carries a dependency or is unknown (Pulumi SDK v3.265.0
// go/pulumi/provider.go:764-776), a path pulumi.WithMocks never takes. tailnetAuthKey, network
// and subnetwork are the inputs a consumer takes from its own resources, so these tests send each
// as an output of another resource, then unknown as under preview.

// upstream is the resource tailnetAuthKey, network and subnetwork come from.
var upstream = resource.URN("urn:pulumi:s::p::example:index:Network::upstream")

// construct serves the plugin in process with the mocks behind it, constructs a hub named
// example-hub from inputs with a gcp provider, and returns what the mocks saw, Construct's
// response and its error.
func construct(t *testing.T, inputs map[string]property.Value, dryRun bool) (*mocks, p.ConstructResponse, error) {
	t.Helper()
	prov, err := provider.New()
	require.NoError(t, err)
	m := &mocks{}
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov), integration.WithMocks(m))
	require.NoError(t, err)
	resp, err := server.Construct(p.ConstructRequest{
		Urn:    resource.NewURN("s", "p", "", tokens.Type(hubType), "example-hub"),
		DryRun: dryRun,
		Inputs: property.NewMap(inputs),
		Providers: map[tokens.Package]p.ProviderReference{
			"gcp": {Urn: resource.URN("urn:pulumi:s::p::" + gcpProviderType + "::gcp-example"), ID: "gcp-example-id"},
		},
	})
	return m, resp, err
}

// plainInputs are exampleArgs' plain fields, as a consumer sends them.
func plainInputs() map[string]property.Value {
	return map[string]property.Value{
		"users": property.New(property.NewArray([]property.Value{
			property.New(property.NewMap(map[string]property.Value{
				"name":      property.New("example-host-a"),
				"publicKey": property.New(keyA),
				"machineId": property.New("example-host-a"),
			})),
			property.New(property.NewMap(map[string]property.Value{
				"name":      property.New("example-operator"),
				"publicKey": property.New(keyB),
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

	requireChildren(t, m, constructURN, wantChildren(t, "example-network", "example-subnetwork"))
	requireNatsURL(t, resp)
	for typ, field := range map[string]string{
		versionType:  "secretData",
		firewallType: "network",
		templateType: "networkInterfaces",
	} {
		require.Contains(t, m.one(t, typ).rpc.GetPropertyDependencies()[field].GetUrns(), string(upstream),
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
	want := wantChildren(t, "example-network", "example-subnetwork")
	gotNames, wantNames := []string{}, []string{}
	for _, r := range m.children() {
		gotNames = append(gotNames, r.typ+"::"+r.name)
	}
	for _, c := range want {
		wantNames = append(wantNames, c.typ+"::"+c.name)
	}
	require.ElementsMatch(t, wantNames, gotNames, "the hub's children")
	unknown := map[string][]string{firewallType: {"network"}, templateType: {"networkInterfaces"}}
	for _, c := range want {
		r := m.one(t, c.typ)
		in := inputsOf(r)
		for _, field := range unknown[c.typ] {
			require.True(t, r.inputs[resource.PropertyKey(field)].ContainsUnknowns(),
				"the %s's %s comes from the unknown network", c.typ, field)
			delete(in, field)
			delete(c.inputs, field)
		}
		require.Equal(t, c.inputs, in, "the inputs of the %s", c.typ)
		require.Equal(t, constructURN, r.rpc.GetParent(), "the %s's parent is the hub", c.typ)
		require.Contains(t, r.rpc.GetProvider(), "::"+gcpProviderType+"::gcp-example::",
			"the %s uses the gcp provider the hub was given", c.typ)
	}
	data, secret := plain(m.one(t, versionType).inputs["secretData"])
	require.True(t, secret, "the unknown auth key stays a secret")
	require.True(t, data.IsComputed(), "the secret version's secretData is unknown, not %s", data.TypeString())
	requireNatsURL(t, resp)
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
	require.Empty(t, m.types())
}
