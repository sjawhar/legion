package gcp_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	gcpsdk "github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	hubType         = "envoy-infra:gcp:NatsHub"
	gcpProviderType = "pulumi:providers:gcp"
	accountType     = "gcp:serviceaccount/account:Account"
	secretType      = "gcp:secretmanager/secret:Secret"
	versionType     = "gcp:secretmanager/secretVersion:SecretVersion"
	accessorType    = "gcp:secretmanager/secretIamMember:SecretIamMember"
	networkType     = "gcp:compute/network:Network"
	firewallType    = "gcp:compute/firewall:Firewall"
	healthCheckType = "gcp:compute/healthCheck:HealthCheck"
	templateType    = "gcp:compute/instanceTemplate:InstanceTemplate"
	groupType       = "gcp:compute/instanceGroupManager:InstanceGroupManager"

	// Two user public keys, the placeholders core's tests use, and the keys of the listener's user
	// and the session user, whose seeds nobody kept.
	keyA        = "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO"
	keyB        = "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG"
	keyListener = "UBZEZDPQNFYMZ4J4XOLTB2IN5PBPZ6FZEGY55R623A4GPBPZLTQFKDYV"
	keySessions = "UCNPQVMRWOOJWCD7XDVLH6MEOVFBMTV4LAQSKZYROSETYJVFAWDMNKVA"

	authKey        = "tskey-auth-example-0123456789"
	natsImage      = "example.invalid/nats@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tailscaleImage = "example.invalid/tailscale@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	listenerImage  = "example.invalid/envoy@sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	listenerSeed   = "example-hub-listener-seed"

	// hubURN is the URN of the hub run constructs, the parent of every child; constructURN is the
	// one the plugin's Construct gives it under integration.NewServer's mocks.
	hubURN       = "urn:pulumi:s::p::" + hubType + "::example-hub"
	constructURN = "urn:pulumi:stack::project::" + hubType + "::example-hub"
)

// newMocks is the mock engine with the outputs the hub reads, as the gcp provider computes them:
// an account's email and member, a network's and a health check's self link, a template's unique
// self link and a group's name.
func newMocks() *pulumitest.Mocks {
	return &pulumitest.Mocks{Computed: func(args pulumi.MockResourceArgs) (string, resource.PropertyMap) {
		out := resource.PropertyMap{}
		switch args.TypeToken {
		case accountType:
			email := args.Inputs["accountId"].StringValue() + "@example-project.iam.gserviceaccount.com"
			out["email"] = resource.NewProperty(email)
			out["member"] = resource.NewProperty("serviceAccount:" + email)
		case networkType, healthCheckType:
			out["selfLink"] = resource.NewProperty(args.Name + "-self-link")
		case templateType:
			out["selfLinkUnique"] = resource.NewProperty(args.Name + "-self-link-unique")
		case groupType:
			out["name"] = resource.NewProperty(args.Name + "-group")
		}
		return "", out
	}}
}

// children returns every registration but the hub and the test's provider.
func children(m *pulumitest.Mocks) []pulumitest.Registration {
	var out []pulumitest.Registration
	for _, r := range m.Registrations() {
		if r.Type != hubType && r.Type != gcpProviderType {
			out = append(out, r)
		}
	}
	return out
}

// run constructs a hub named example-hub from args, with a gcp provider when withProvider is set,
// and returns what the mocks saw and the run's error. check, when not nil, runs on the constructed
// hub inside the program.
func run(t *testing.T, args *gcp.NatsHubArgs, check func(*gcp.NatsHub), withProvider bool) (*pulumitest.Mocks, error) {
	t.Helper()
	return runNamed(t, "example-hub", args, check, withProvider)
}

// runNamed is run with the hub's name as a parameter, for the tests that refuse a seed there.
func runNamed(t *testing.T, name string, args *gcp.NatsHubArgs, check func(*gcp.NatsHub), withProvider bool) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	err := m.Run(func(ctx *pulumi.Context) error {
		var opts []pulumi.ResourceOption
		if withProvider {
			prov, err := gcpsdk.NewProvider(ctx, "gcp-example", &gcpsdk.ProviderArgs{})
			if err != nil {
				return err
			}
			opts = append(opts, pulumi.Providers(prov))
		}
		hub, err := gcp.NewNatsHub(ctx, name, args, opts...)
		if err != nil {
			return err
		}
		if check != nil {
			check(hub)
		}
		return nil
	})
	return m, err
}

// exampleArgs are a hub serving one host by its machine id and one user with explicit
// permissions, running a listener of its own and admitting a client with no credential as the
// session user, on placeholders.
func exampleArgs() *gcp.NatsHubArgs {
	return &gcp.NatsHubArgs{
		Users: []gcp.NATSUserArgs{
			{Name: "example-host-a", NKey: keyA, MachineID: pulumi.StringRef("example-host-a")},
			{Name: "example-operator", NKey: keyB, Permissions: &gcp.PermissionsArgs{
				Publish:   nats.RulesArgs{Allow: []string{">"}},
				Subscribe: nats.RulesArgs{Allow: []string{">"}, Deny: []string{"$SYS.>"}},
			}},
		},
		TailnetAuthKey: pulumi.String(authKey),
		Hostname:       "example-hub",
		Tags:           []string{"tag:example-host"},
		Zone:           "us-central1-a",
		NatsImage:      natsImage,
		TailscaleImage: tailscaleImage,
		Listener:       &gcp.HubListenerArgs{Image: listenerImage, SeedSecret: listenerSeed, NKey: keyListener},
		SessionUser:    &gcp.SessionUserArgs{Name: "example-sessions", NKey: keySessions},
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}

// inputsOf returns a registration's inputs as plain Go values, secretData left out: it is checked
// as a secret on its own.
func inputsOf(r pulumitest.Registration) map[string]any {
	in := r.Inputs.Mappable()
	delete(in, "secretData")
	return in
}

// child is one child's expected type, name and recorded inputs.
type child struct {
	typ, name string
	inputs    map[string]any
}

// wantChildren are the children exampleArgs register, with network and subnetwork the hub's inputs
// ("" for none given): with no network, the hub creates one and the firewall and the template name
// its self link.
func wantChildren(t *testing.T, network, subnetwork string) []child {
	t.Helper()
	created := network == ""
	if created {
		network = "example-hub-network-self-link"
	}
	email := "example-hub-hub@example-project.iam.gserviceaccount.com"
	nic := map[string]any{"network": network, "accessConfigs": []any{map[string]any{}}}
	if subnetwork != "" {
		nic["subnetwork"] = subnetwork
	}
	children := []child{
		{accountType, "example-hub-hub", map[string]any{"accountId": "example-hub-hub"}},
		{secretType, "example-hub-tailnet-auth-key", map[string]any{
			"secretId":    "example-hub-tailnet-auth-key",
			"replication": map[string]any{"auto": map[string]any{}},
		}},
		{versionType, "example-hub-tailnet-auth-key", map[string]any{"secret": "example-hub-tailnet-auth-key-id"}},
		{accessorType, "example-hub-tailnet-auth-key-accessor", map[string]any{
			"secretId": "example-hub-tailnet-auth-key-id",
			"role":     "roles/secretmanager.secretAccessor",
			"member":   "serviceAccount:" + email,
		}},
		{accessorType, "example-hub-listener-seed-accessor", map[string]any{
			"secretId": listenerSeed,
			"role":     "roles/secretmanager.secretAccessor",
			"member":   "serviceAccount:" + email,
		}},
		{firewallType, "example-hub-health-checks", map[string]any{
			"network":      network,
			"allows":       []any{map[string]any{"protocol": "tcp", "ports": []any{"8222"}}},
			"sourceRanges": []any{"130.211.0.0/22", "35.191.0.0/16"},
			"targetTags":   []any{"example-hub-hub"},
		}},
		{healthCheckType, "example-hub-healthz", map[string]any{
			"httpHealthCheck":    map[string]any{"port": float64(8222), "requestPath": "/healthz"},
			"checkIntervalSec":   float64(10),
			"unhealthyThreshold": float64(6),
		}},
		{templateType, "example-hub", map[string]any{
			"namePrefix":  "example-hub-",
			"machineType": "e2-small",
			"disks": []any{
				map[string]any{"sourceImage": "projects/cos-cloud/global/images/family/cos-stable", "boot": true},
				map[string]any{"deviceName": "data", "boot": false, "autoDelete": false, "diskSizeGb": float64(20), "diskType": "pd-balanced"},
			},
			"networkInterfaces": []any{nic},
			"serviceAccount": map[string]any{
				"email":  email,
				"scopes": []any{"https://www.googleapis.com/auth/cloud-platform"},
			},
			"tags":     []any{"example-hub-hub"},
			"metadata": map[string]any{"user-data": readGolden(t, "cloud-config.golden")},
		}},
		{groupType, "example-hub", map[string]any{
			"baseInstanceName": "example-hub",
			"zone":             "us-central1-a",
			"targetSize":       float64(1),
			"versions":         []any{map[string]any{"instanceTemplate": "example-hub-self-link-unique"}},
			"statefulDisks":    []any{map[string]any{"deviceName": "data", "deleteRule": "NEVER"}},
			"autoHealingPolicies": map[string]any{
				"healthCheck":     "example-hub-healthz-self-link",
				"initialDelaySec": float64(573),
			},
			"updatePolicy": map[string]any{
				"type":                "PROACTIVE",
				"minimalAction":       "REPLACE",
				"replacementMethod":   "RECREATE",
				"maxSurgeFixed":       float64(0),
				"maxUnavailableFixed": float64(1),
			},
			"waitForInstances":       true,
			"waitForInstancesStatus": "STABLE",
		}},
	}
	if created {
		children = append(children, child{networkType, "example-hub-network", map[string]any{"autoCreateSubnetworks": true}})
	}
	return children
}

// requireChildren requires the registrations to be exactly want's children, each with its inputs,
// a child of the hub at URN parent on the gcp provider the hub was given, and requires the auth key
// to reach the secret version alone, as a secret. unknown names, by type, the inputs that must be
// unknown, as those taken from an unknown input are under preview; want's value for them is not
// compared.
func requireChildren(t *testing.T, m *pulumitest.Mocks, parent string, want []child, unknown map[string][]string) {
	t.Helper()
	got := children(m)
	gotNames := make([]string, 0, len(got))
	for _, r := range got {
		gotNames = append(gotNames, r.Type+"::"+r.Name)
	}
	wantNames := make([]string, 0, len(want))
	for _, c := range want {
		wantNames = append(wantNames, c.typ+"::"+c.name)
	}
	require.ElementsMatch(t, wantNames, gotNames, "the hub's children")
	for _, c := range want {
		r := named(t, m, c.typ, c.name)
		in := inputsOf(r)
		for _, field := range unknown[c.typ] {
			require.True(t, r.Inputs[resource.PropertyKey(field)].ContainsUnknowns(),
				"the %s's %s comes from an unknown input", c.typ, field)
			delete(in, field)
			delete(c.inputs, field)
		}
		require.Equal(t, c.inputs, in, "the inputs of the %s", c.typ)
		require.Equal(t, parent, r.RPC.GetParent(), "the %s's parent is the hub", c.typ)
		require.Contains(t, r.RPC.GetProvider(), "::"+gcpProviderType+"::gcp-example::",
			"the %s uses the gcp provider the hub was given", c.typ)
	}
	requireAuthKeyOnlyInTheSecretVersion(t, m)
}

// requireAuthKeyOnlyInTheSecretVersion requires the secret version's secretData to be the auth
// key, as a secret, unknown under preview, and no plain value of any registration to hold it.
func requireAuthKeyOnlyInTheSecretVersion(t *testing.T, m *pulumitest.Mocks) {
	t.Helper()
	data, secret := pulumitest.Plain(m.One(t, versionType).Inputs["secretData"])
	require.True(t, secret, "the secret version's secretData is a secret")
	if !data.IsComputed() {
		require.Equal(t, authKey, data.StringValue())
	}
	for _, r := range m.Registrations() {
		for _, s := range plainStrings(resource.NewProperty(r.Inputs)) {
			require.NotContains(t, s, authKey, "a plain input of %s %s holds the auth key", r.Type, r.Name)
		}
	}
}

// named returns the single registration of type typ and name name.
func named(t *testing.T, m *pulumitest.Mocks, typ, name string) pulumitest.Registration {
	t.Helper()
	var found []pulumitest.Registration
	for _, r := range m.Registrations() {
		if r.Type == typ && r.Name == name {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1, "registrations of %s %s", typ, name)
	return found[0]
}

// plainStrings returns every string v holds outside a secret.
func plainStrings(v resource.PropertyValue) []string {
	switch {
	case v.IsSecret():
		return nil
	case v.IsString():
		return []string{v.StringValue()}
	case v.IsArray():
		var out []string
		for _, e := range v.ArrayValue() {
			out = append(out, plainStrings(e)...)
		}
		return out
	case v.IsObject():
		var out []string
		for _, e := range v.ObjectValue() {
			out = append(out, plainStrings(e)...)
		}
		return out
	case v.IsOutput():
		if v.OutputValue().Secret {
			return nil
		}
		return plainStrings(v.OutputValue().Element)
	}
	return nil
}

// allStrings returns every string v holds, a secret's included.
func allStrings(v resource.PropertyValue) []string {
	switch {
	case v.IsSecret():
		return allStrings(v.SecretValue().Element)
	case v.IsString():
		return []string{v.StringValue()}
	case v.IsArray():
		var out []string
		for _, e := range v.ArrayValue() {
			out = append(out, allStrings(e)...)
		}
		return out
	case v.IsObject():
		var out []string
		for _, e := range v.ObjectValue() {
			out = append(out, allStrings(e)...)
		}
		return out
	case v.IsOutput():
		return allStrings(v.OutputValue().Element)
	}
	return nil
}

// await returns the value o resolves to. It runs inside the program, so the value is read on the
// test's goroutine, after the engine resolved it.
func await(t *testing.T, o pulumi.StringOutput) string {
	t.Helper()
	out, err := internals.UnsafeAwaitOutput(t.Context(), o)
	require.NoError(t, err)
	require.True(t, out.Known, "the output is known")
	return out.Value.(string)
}

func TestNatsHubRegistersEveryChildWithTheListedInputs(t *testing.T) {
	var natsURL, instanceGroup string
	m, err := run(t, exampleArgs(), func(h *gcp.NatsHub) {
		natsURL, instanceGroup = await(t, h.NatsUrl), await(t, h.InstanceGroup)
	}, true)
	require.NoError(t, err)

	require.Equal(t, "example-hub", m.One(t, hubType).Name)
	requireChildren(t, m, hubURN, wantChildren(t, "", ""), nil)
	require.Equal(t, "nats://example-hub:4222", natsURL)
	require.Equal(t, "example-hub-group", instanceGroup, "instanceGroup is the group's name")
}

func TestNatsHubDaemonUserGetsDaemonPermissions(t *testing.T) {
	args := exampleArgs()
	args.Users = []gcp.NATSUserArgs{{Name: "example-daemon", NKey: keyA, Daemon: true}}
	args.Listener = nil
	args.SessionUser = nil

	doc, _, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)
	grant := core.DaemonPermissions()
	want, err := core.Authorization{Users: []core.User{{
		Name: "example-daemon", NKey: keyA, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true,
	}}}.Render([]string{"Rendered by envoy-infra. Public keys only."})
	require.NoError(t, err)
	require.Equal(t, want, fileOf(t, doc, "/etc/nats/nats-server.conf"))
}

func TestNatsHubOnTheConsumersNetworkCreatesNone(t *testing.T) {
	args := exampleArgs()
	args.Network = pulumi.StringPtr("example-network")
	args.Subnetwork = pulumi.StringPtr("example-subnetwork")
	m, err := run(t, args, nil, true)
	require.NoError(t, err)

	requireChildren(t, m, hubURN, wantChildren(t, "example-network", "example-subnetwork"), nil)
	require.NotContains(t, m.Types(), networkType)
}

func TestNatsHubNetworkFromAnOutputReachesTheFirewallAndTheTemplate(t *testing.T) {
	args := exampleArgs()
	args.Network = pulumi.String("example-network").ToStringOutput().ToStringPtrOutput()
	m, err := run(t, args, nil, true)
	require.NoError(t, err)

	requireChildren(t, m, hubURN, wantChildren(t, "example-network", ""), nil)
}

func TestNatsHubGroupWaitsForTheKeyAndItsGrant(t *testing.T) {
	m, err := run(t, exampleArgs(), nil, true)
	require.NoError(t, err)

	deps := m.One(t, groupType).RPC.GetDependencies()
	child := "urn:pulumi:s::p::" + hubType + "$"
	require.Contains(t, deps, child+versionType+"::example-hub-tailnet-auth-key",
		"the VM is created after the key it fetches at boot")
	require.Contains(t, deps, child+accessorType+"::example-hub-tailnet-auth-key-accessor",
		"the VM is created after the grant that lets it read the key")
	require.Contains(t, deps, child+accessorType+"::example-hub-listener-seed-accessor",
		"the VM is created after the grant that lets its listener read its seed")
}

func TestNatsHubMachineTypeAndDiskReachTheTemplate(t *testing.T) {
	args := exampleArgs()
	args.MachineType = pulumi.StringRef("e2-medium")
	args.DiskType = pulumi.StringRef("pd-ssd")
	args.DiskSizeGb = pulumi.IntRef(50)
	m, err := run(t, args, nil, true)
	require.NoError(t, err)

	in := m.One(t, templateType).Inputs.Mappable()
	require.Equal(t, "e2-medium", in["machineType"])
	data := in["disks"].([]any)[1].(map[string]any)
	require.Equal(t, float64(50), data["diskSizeGb"])
	require.Equal(t, "pd-ssd", data["diskType"])
	require.Equal(t, float64(632), m.One(t, groupType).Inputs.Mappable()["autoHealingPolicies"].(map[string]any)["initialDelaySec"])
}

// The group's initial delay covers the first boot's full read of the data disk and its format:
// 300 + ceil(1.25 * (S + S/64 + 1024) / R) seconds, S the disk's size in MiB and R the read rate
// assumed for its type, 100 MiB/s for pd-balanced and 200 MiB/s for pd-ssd. The rows are the
// default, the smallest disk, and the largest disk of each type whose delay fits the group's 3600 s.
// pulumi up waits for the group to be stable, which its VM is once healthy, so the group's create
// and update timeouts are that delay after the provider's own 15 minutes.
func TestNatsHubInitialDelayCoversTheFirstBootsFullRead(t *testing.T) {
	for _, row := range []struct {
		diskType *string
		size     *int
		delay    float64
		timeout  string
	}{
		{nil, nil, 573, "24m33s"},
		{pulumi.StringRef("pd-balanced"), pulumi.IntRef(1), 326, "20m26s"},
		{pulumi.StringRef("pd-balanced"), pulumi.IntRef(252), 3589, "1h14m49s"},
		{pulumi.StringRef("pd-ssd"), pulumi.IntRef(1), 313, "20m13s"},
		{pulumi.StringRef("pd-ssd"), pulumi.IntRef(506), 3596, "1h14m56s"},
	} {
		args := exampleArgs()
		args.DiskType, args.DiskSizeGb = row.diskType, row.size
		m, err := run(t, args, nil, true)
		require.NoError(t, err)
		group := m.One(t, groupType)
		got := group.Inputs.Mappable()["autoHealingPolicies"].(map[string]any)["initialDelaySec"]
		require.Equal(t, row.delay, got, "diskType %v, diskSizeGb %v", row.diskType, row.size)
		timeouts := group.RPC.GetCustomTimeouts()
		require.NotNil(t, timeouts, "diskType %v, diskSizeGb %v", row.diskType, row.size)
		require.Equal(t, row.timeout, timeouts.GetCreate(), "diskType %v, diskSizeGb %v", row.diskType, row.size)
		require.Equal(t, row.timeout, timeouts.GetUpdate(), "diskType %v, diskSizeGb %v", row.diskType, row.size)
		require.Empty(t, timeouts.GetDelete(), "the delete keeps the provider's timeout")
	}
}

func TestNatsHubRefusesAMissingProviderBeforeRegistering(t *testing.T) {
	m, err := run(t, exampleArgs(), nil, false)
	require.ErrorContains(t, err, `envoy-infra:gcp:NatsHub needs a "gcp" provider in its providers option`)
	require.Empty(t, m.Types())
}

// The resolver refuses every invalid input, naming it, with no engine; the constructor's own test
// below shows a refusal stops it before anything registers.
func TestNatsHubResolverRefusesInvalidInputs(t *testing.T) {
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	cases := []resolverRefusal{
		{
			name: "a hostname ending in a hyphen",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "ab-" },
			err:  []string{`hostname "ab-"`, "^[a-z][a-z0-9-]{0,24}[a-z0-9]$"},
		},
		{
			name: "a one-character hostname",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "a" },
			err:  []string{`hostname "a"`, "^[a-z][a-z0-9-]{0,24}[a-z0-9]$"},
		},
		{
			name: "a 27-character hostname",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "a" + strings.Repeat("b", 26) },
			err:  []string{`hostname "a` + strings.Repeat("b", 26) + `"`},
		},
		{
			name: "a hostname starting with a digit",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "1hub" },
			err:  []string{`hostname "1hub"`},
		},
		{
			name: "a hostname with an uppercase letter",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "Example-hub" },
			err:  []string{`hostname "Example-hub"`},
		},
		{
			name: "no tags",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = nil },
			err:  []string{"tags", "at least one"},
		},
		{
			name: "a tag without its prefix",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"example-host"} },
			err:  []string{`tags[0] "example-host"`, "^tag:[A-Za-z][A-Za-z0-9-]*$"},
		},
		{
			name: "a tag starting with a digit",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:example-host", "tag:1host"} },
			err:  []string{`tags[1] "tag:1host"`},
		},
		{
			name: "a tag with a comma",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:a,tag:b"} },
			err:  []string{`tags[0] "tag:a,tag:b"`},
		},
		{
			name: "no zone",
			edit: func(a *gcp.NatsHubArgs) { a.Zone = "" },
			err:  []string{"zone is required"},
		},
		{
			name: "a NATS image without a digest",
			edit: func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/nats:2.12" },
			err:  []string{`natsImage "example.invalid/nats:2.12"`, "@sha256:"},
		},
		{
			name: "a Tailscale image without a digest",
			edit: func(a *gcp.NatsHubArgs) { a.TailscaleImage = "example.invalid/tailscale:latest" },
			err:  []string{`tailscaleImage "example.invalid/tailscale:latest"`, "@sha256:"},
		},
		{
			name: "an image whose digest is short",
			edit: func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/nats@sha256:0123" },
			err:  []string{`natsImage "example.invalid/nats@sha256:0123"`},
		},
		{
			name: "an image with a space",
			edit: func(a *gcp.NatsHubArgs) {
				a.NatsImage = "example.invalid/nats --privileged x" + natsImage[len("example.invalid/nats"):]
			},
			err: []string{"natsImage"},
		},
		{
			name: "an empty machine type",
			edit: func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef("") },
			err:  []string{"machineType"},
		},
		{
			name: "a zero disk size",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(0) },
			err:  []string{"diskSizeGb 0"},
		},
		{
			name: "a pd-balanced disk one GiB past the largest whose read fits the delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(253) },
			err:  []string{"diskSizeGb 253", "pd-balanced", "100 MiB/s", "3600", "the largest diskSizeGb that fits is 252"},
		},
		{
			name: "a pd-ssd disk one GiB past the largest whose read fits the delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType, a.DiskSizeGb = pulumi.StringRef("pd-ssd"), pulumi.IntRef(507) },
			err:  []string{"diskSizeGb 507", "pd-ssd", "200 MiB/s", "the largest diskSizeGb that fits is 506"},
		},
		{
			name: "a disk size past any delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(1 << 62) },
			err:  []string{"diskSizeGb 4611686018427387904", "the largest diskSizeGb that fits is 252"},
		},
		{
			name: "a pd-standard disk",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-standard") },
			err:  []string{`diskType "pd-standard"`, "0.12 MiB/s per GiB", "3600"},
		},
		{
			name: "a hyperdisk-balanced disk",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("hyperdisk-balanced") },
			err:  []string{`diskType "hyperdisk-balanced"`, "E2", "account team"},
		},
		{
			name: "another disk type",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-extreme") },
			err:  []string{`diskType "pd-extreme"`, "pd-balanced or pd-ssd"},
		},
		{
			name: "an empty disk type",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("") },
			err:  []string{"diskType is empty", "pd-balanced"},
		},
		{
			name: "no tailnet auth key",
			edit: func(a *gcp.NatsHubArgs) { a.TailnetAuthKey = nil },
			err:  []string{"tailnetAuthKey is required"},
		},
		{
			name: "a subnetwork without a network",
			edit: func(a *gcp.NatsHubArgs) { a.Subnetwork = pulumi.StringPtr("example-subnetwork") },
			err:  []string{"subnetwork", "network"},
		},
		{
			name: "no users and no listener",
			edit: func(a *gcp.NatsHubArgs) { a.Users, a.Listener = nil, nil },
			err:  []string{"users: at least one user is required, or a listener"},
		},
		{
			name: "a user naming both a machine id and permissions",
			edit: func(a *gcp.NatsHubArgs) {
				a.Users[0].Permissions = &gcp.PermissionsArgs{
					Publish: nats.RulesArgs{Allow: []string{">"}}, Subscribe: nats.RulesArgs{Allow: []string{">"}},
				}
			},
			err: []string{`users[0]: machineId and permissions are both set`},
		},
		{
			name: "a user naming both a machine id and daemon",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].Daemon = true },
			err:  []string{`users[0]: machineId and daemon are both set`},
		},
		{
			name: "a user naming both daemon and permissions",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Daemon = true },
			err:  []string{`users[1]: daemon and permissions are both set`},
		},
		{
			name: "a user naming neither a machine id nor permissions",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Permissions = nil },
			err:  []string{`users[1]: name machineId, daemon, or permissions`},
		},
		{
			name: "a user with an empty machine id",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef("") },
			err:  []string{`users[0]: machineId is empty`},
		},
		{
			name: "a user without a public key",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].NKey = "" },
			err:  []string{`users[0]: nkey is required`},
		},
		{
			name: "a user whose public key is a seed",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].NKey = string(seed) },
			err:  []string{`users[0] "example-host-a"`, "never a seed"},
		},
		{
			name: "a user with an invalid name",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Name = "Example Operator" },
			err:  []string{`users[1] "Example Operator"`, "name"},
		},
		{
			name: "two users of one name",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Name = "example-host-a" },
			err:  []string{`users[1] "example-host-a"`, "repeats users[0]"},
		},
		{
			name: "explicit permissions with an empty allow list",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Permissions.Subscribe.Allow = nil },
			err:  []string{`users[1] "example-operator"`, "subscribe.allow"},
		},
	}
	requireResolverRefusals(t, cases)
}

// A refused input stops the constructor, named as the hub and its name, before it registers
// anything beyond the test's provider.
func TestNatsHubRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := exampleArgs()
	args.Hostname = "ab-"
	m, err := run(t, args, nil, true)
	require.ErrorContains(t, err, `envoy-infra:gcp:NatsHub example-hub: hostname "ab-"`)
	require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
}

// A seed in any input the hub itself renders, registers or quotes in a refusal is refused naming
// the input by its path, never the value: the hostname and the tags, which the hub's own refusals
// quote and the user data holds; the zone, machine type and disk type, which reach the group and
// the template; the images, which reach the user data; and a user's machine id, which the hub turns
// into that user's rules. The seed is found as bus.HoldsSeed finds it: any type, either case, whole,
// inside a longer value, split by '.', '-', '_' or whitespace, or percent-encoded. A user's name and
// rule entries are core's to refuse (TestNatsHubPassesOnCoresSeedRefusalOfUsers); a seed as a
// user's nkey is core's to refuse, unquoted (the resolver table's "a user whose public key is a
// seed"); and tailnetAuthKey, a secret Input, is checked once it resolves
// (TestConstructRefusesASeedAsTheTailnetAuthKey).
func TestNatsHubRefusesASeedInAnyPlainInput(t *testing.T) {
	seed, _ := testnats.User(t)
	inside := "example-" + seed
	lower := strings.ToLower(seed)
	digest := "@sha256:" + strings.Repeat("0", 64)
	requireSeedRefusals(t, seed, []seedRefusal{
		{"a seed as the hostname", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = seed }},
		{"a seed in a tag", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + seed} }},
		{"a seed inside the zone", "zone", func(a *gcp.NatsHubArgs) { a.Zone = inside }},
		{"a seed inside the machine type", "machineType", func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef(inside) }},
		{"a seed inside the disk type", "diskType", func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef(inside) }},
		{"a lowercase disk type", "diskType", func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-" + lower) }},
		{"a seed in the NATS image", "natsImage", func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/" + seed + digest }},
		{"a seed in the Tailscale image", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + seed + digest
		}},
		{"a seed inside a machine id", "users[0].machineId", func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef(inside) }},
		// A reader or a decoder takes a seed in lowercase, or split by '.', '-', '_' or whitespace, as
		// the seed.
		{"a lowercase seed as the hostname", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = lower }},
		{"a hostname split by a hyphen", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = splitSeed(lower, "-") }},
		{"a lowercase seed in a tag", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + lower} }},
		{"a tag split by a hyphen", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + splitSeed(seed, "-")} }},
		{"a lowercase zone", "zone", func(a *gcp.NatsHubArgs) { a.Zone = "us-" + lower }},
		{"a zone split by a hyphen", "zone", func(a *gcp.NatsHubArgs) { a.Zone = splitSeed(lower, "-") }},
		{"a lowercase machine type", "machineType", func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef(lower) }},
		{"a machine type split by an underscore", "machineType", func(a *gcp.NatsHubArgs) {
			a.MachineType = pulumi.StringRef(splitSeed(seed, "_"))
		}},
		{"a lowercase NATS image", "natsImage", func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/" + lower + digest }},
		{"a NATS image split across path parts", "natsImage", func(a *gcp.NatsHubArgs) {
			a.NatsImage = "example.invalid/" + splitSeed(lower, ".") + digest
		}},
		{"a lowercase Tailscale image", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + lower + digest
		}},
		{"a Tailscale image split by a hyphen", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + splitSeed(lower, "-") + digest
		}},
		{"a lowercase machine id", "users[0].machineId", func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef(lower) }},
		{"a machine id split by a space", "users[0].machineId", func(a *gcp.NatsHubArgs) {
			a.Users[0].MachineID = pulumi.StringRef(splitSeed(seed, " "))
		}},
	})
}

// core.Authorization refuses a seed in a user's name or a rule entry before any refusal of its own
// quotes it, naming the path and never the value, and the hub passes that refusal on as it is.
// The hub's own refusals of a user name it by its index alone, so a seed in the name never reaches
// one: they come before core reads the names.
func TestNatsHubPassesOnCoresSeedRefusalOfUsers(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	for _, row := range []struct {
		name string
		edit func(*gcp.NatsHubArgs)
		want string
	}{
		{"a seed as a user's name", func(a *gcp.NatsHubArgs) { a.Users[1].Name = seed },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a lowercase seed as a user's name", func(a *gcp.NatsHubArgs) { a.Users[1].Name = lower },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a user's name split by a hyphen", func(a *gcp.NatsHubArgs) { a.Users[1].Name = splitSeed(lower, "-") },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a seed in a publish allow entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Publish.Allow = []string{"notifications." + seed}
		}, `users[1] "example-operator": publish.allow[0] holds an nkey seed (not echoed)`},
		{"a seed percent-encoded in a subscribe deny entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Subscribe.Deny = []string{percentEncoded(seed)}
		}, `users[1] "example-operator": subscribe.deny[0] holds an nkey seed (not echoed)`},
		{"a lowercase seed in a publish deny entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Publish.Deny = []string{"notifications." + lower}
		}, `users[1] "example-operator": publish.deny[0] holds an nkey seed (not echoed)`},
		{"a subscribe allow entry split across subject tokens", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Subscribe.Allow = []string{"notifications." + splitSeed(seed, ".")}
		}, `users[1] "example-operator": subscribe.allow[0] holds an nkey seed (not echoed)`},
		// The hub's own refusals of a user come first, and name it by its index alone.
		{"a seed as the name of a user with no nkey", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].NKey = seed, ""
		}, "users[1]: nkey is required"},
		{"a seed as the name of a user naming both a machine id and permissions", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].MachineID = seed, pulumi.StringRef("example-host-b")
		}, "users[1]: machineId and permissions are both set; name one"},
		{"a seed as the name of a user naming neither", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].Permissions = lower, nil
		}, "users[1]: name machineId, daemon, or permissions"},
		{"a seed as the name of a user with an empty machine id", func(a *gcp.NatsHubArgs) {
			a.Users[0].Name, a.Users[0].MachineID = seed, pulumi.StringRef("")
		}, "users[0]: machineId is empty"},
		{"a seed as the session user's name", func(a *gcp.NatsHubArgs) { a.SessionUser.Name = lower },
			"sessionUser: name holds an nkey seed (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.EqualError(t, err, row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}

// network and subnetwork may come from another resource, so the hub checks them when they are
// known, as the firewall's and the template's inputs resolve: a seed in either fails the run
// naming it, and no registration holds it.
func TestNatsHubRefusesASeedInItsNetworkWhenItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name, input string
		edit        func(*gcp.NatsHubArgs)
	}{
		{"a seed in the network", "network", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("projects/example-project/global/networks/" + seed)
		}},
		{"a seed in the subnetwork", "subnetwork", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("example-network")
			a.Subnetwork = pulumi.String("example-" + seed)
		}},
		{"a lowercase network", "network", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("projects/example-project/global/networks/" + strings.ToLower(seed))
		}},
		{"a subnetwork split by a hyphen", "subnetwork", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("example-network")
			a.Subnetwork = pulumi.String(splitSeed(strings.ToLower(seed), "-"))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			m, err := run(t, args, nil, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:NatsHub example-hub: "+row.input+
				": an nkey user seed, which is never an input of the hub (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range plainStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}

// NewNatsHub quotes the name in every refusal (its "%s %s: %w" wrapper) and registers the
// component under it, so a seed there is refused before anything registers, as host:Listener and
// the AWS components refuse one in their own names.
func TestNatsHubRefusesASeedInTheComponentName(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name string
		hub  string
	}{
		{"a whole seed", seed},
		{"a seed inside a longer name", "hub-" + seed},
		{"a lowercase seed", strings.ToLower(seed)},
		{"a seed split by a hyphen", splitSeed(seed, "-")},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runNamed(t, row.hub, exampleArgs(), nil, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:NatsHub: the component's name holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
		})
	}
}

// percentEncoded spells every byte of text as %XX.
func percentEncoded(text string) string {
	var b strings.Builder
	for i := range len(text) {
		fmt.Fprintf(&b, "%%%02X", text[i])
	}
	return b.String()
}

// splitSeed is seed with sep between its halves.
func splitSeed(seed, sep string) string { return seed[:len(seed)/2] + sep + seed[len(seed)/2:] }

// The names GCP gets from the hostname, for the shortest and longest hostname the rule admits:
// each fits its own rule. The rules: a service account id is 6-30 characters, RFC 1035
// (IAM's serviceAccounts.create); a secret id is up to 255 of [a-zA-Z0-9_-] (Secret Manager's
// secrets.create); a network tag is RFC 1035 and up to 63 characters (Compute Engine's
// instances.setTags); a managed group's base instance name is RFC 1035 and up to 58 characters
// (instanceGroupManagers.insert); an instance template's namePrefix gets the provider's full
// 26-character suffix only up to 37 characters (pulumi-gcp v10.1.0 InstanceTemplateArgs). The
// network, firewall, health check and group are autonamed: pulumi-gcp v10.1.0 appends "-" and 7
// random characters to the logical name (provider/resources.go lowercaseAutoName and
// SetAutonaming), and each must stay RFC 1035 within 63 characters. The tailnet hostname and
// natsUrl's host are a DNS label.
func TestNatsHubDerivedNamesFitGCPForTheShortestAndLongestHostname(t *testing.T) {
	rfc1035 := func(max int) *regexp.Regexp {
		return regexp.MustCompile(fmt.Sprintf(`^[a-z](?:[-a-z0-9]{0,%d}[a-z0-9])?$`, max-2))
	}
	accountID := regexp.MustCompile(`^[a-z](?:[-a-z0-9]{4,28}[a-z0-9])$`)
	secretID := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)
	baseInstanceName := regexp.MustCompile(`^[a-z][-a-z0-9]{0,57}$`)
	dnsLabel := regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

	for _, hostname := range []string{"ab", "a" + strings.Repeat("b0", 12) + "c"} {
		t.Run(fmt.Sprintf("%d characters", len(hostname)), func(t *testing.T) {
			args := exampleArgs()
			args.Hostname = hostname
			var natsURL string
			m, err := run(t, args, func(h *gcp.NatsHub) { natsURL = await(t, h.NatsUrl) }, true)
			require.NoError(t, err)

			require.Regexp(t, accountID, m.One(t, accountType).Inputs["accountId"].StringValue())
			require.Regexp(t, secretID, m.One(t, secretType).Inputs["secretId"].StringValue())
			template := m.One(t, templateType).Inputs.Mappable()
			tag := template["tags"].([]any)[0].(string)
			require.Regexp(t, rfc1035(63), tag)
			require.Equal(t, []any{tag}, m.One(t, firewallType).Inputs.Mappable()["targetTags"],
				"the firewall targets the template's tag")
			require.LessOrEqual(t, len(template["namePrefix"].(string)), 37)
			require.Regexp(t, rfc1035(63), template["namePrefix"].(string)+strings.Repeat("a", 26))
			require.Regexp(t, baseInstanceName, m.One(t, groupType).Inputs["baseInstanceName"].StringValue())
			for _, typ := range []string{networkType, firewallType, healthCheckType, groupType} {
				require.Regexp(t, rfc1035(63), m.One(t, typ).Name+"-abc1234", "the autoname of the %s", typ)
			}
			require.Regexp(t, dnsLabel, hostname)
			require.Equal(t, "nats://"+hostname+":4222", natsURL)
		})
	}
}

// resolverRefusal is one invalid input: edit makes it of exampleArgs, and the resolver's refusal
// contains each of err.
type resolverRefusal struct {
	name string
	edit func(*gcp.NatsHubArgs)
	err  []string
}

// requireResolverRefusals runs each case through the resolver, which must refuse it naming each
// of its err and never quote the tailnet auth key.
func requireResolverRefusals(t *testing.T, cases []resolverRefusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := exampleArgs()
			c.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.Error(t, err)
			for _, want := range c.err {
				require.ErrorContains(t, err, want)
			}
			require.NotContains(t, err.Error(), authKey)
		})
	}
}

// seedRefusal is one plain input holding seed: edit puts it there, and input is the path the
// refusal names.
type seedRefusal struct {
	name, input string
	edit        func(*gcp.NatsHubArgs)
}

// requireSeedRefusals runs each row through the resolver, which must refuse it naming the input
// by its path and never quoting the seed.
func requireSeedRefusals(t *testing.T, seed string, rows []seedRefusal) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.EqualError(t, err, row.input+": an nkey user seed, which is never an input of the hub (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}
