package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	gcpsdk "github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp"
	randomsdk "github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	dispatchType       = "envoy-infra:gcp:Dispatch"
	randomProviderType = "pulumi:providers:random"

	dispatchEnvoyImage     = "example.invalid/envoy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dispatchTailscaleImage = "example.invalid/tailscale@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	dispatchProxyImage     = "example.invalid/cloud-sql-proxy@sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

func dispatchArgs() *gcp.DispatchArgs {
	return &gcp.DispatchArgs{
		Region:             "us-central1",
		Hostname:           "example-dispatch",
		TailnetDnsName:     "example-tailnet.ts.net",
		Tags:               []string{"tag:example-dispatch"},
		TailnetAuthKey:     pulumi.String("tskey-auth-example-0123456789"),
		EnvoyImage:         dispatchEnvoyImage,
		TailscaleImage:     dispatchTailscaleImage,
		CloudSqlProxyImage: dispatchProxyImage,
		NatsUrl:            pulumi.String("nats://example-hub.example-tailnet.ts.net:4222"),
		NatsSeedSecret:     pulumi.String("projects/example-project/secrets/example-nats-seed"),
		SignInClientId:     "example-client-id",
		SignInClientSecret: pulumi.String("example-client-secret"),
		SignInDomain:       "example.com",
	}
}

func runDispatch(t *testing.T, args *gcp.DispatchArgs, providers bool) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	err := m.Run(func(ctx *pulumi.Context) error {
		var opts []pulumi.ResourceOption
		if providers {
			gcpProvider, err := gcpsdk.NewProvider(ctx, "gcp-example", &gcpsdk.ProviderArgs{})
			if err != nil {
				return err
			}
			randomProvider, err := randomsdk.NewProvider(ctx, "random-example", &randomsdk.ProviderArgs{})
			if err != nil {
				return err
			}
			opts = append(opts, pulumi.Providers(gcpProvider, randomProvider))
		}
		_, err := gcp.NewDispatch(ctx, "example-dispatch", args, opts...)
		return err
	})
	return m, err
}

func TestDispatchSettingsApplyDefaults(t *testing.T) {
	settings, err := gcp.ResolveDispatch(dispatchArgs())
	require.NoError(t, err)
	require.Equal(t, "POSTGRES_16", settings.DatabaseVersion)
	require.Equal(t, "db-custom-1-3840", settings.DatabaseTier)
	require.True(t, settings.DeletionProtection)
	require.Equal(t, "dispatch", settings.Namespace)
	require.Equal(t, 1, settings.Replicas)
	require.Equal(t, "Recreate", settings.StrategyType)
	require.Nil(t, settings.RollingUpdate, "one replica is recreated, never rolled")
	require.Equal(t, `{"TCP":{"443":{"HTTPS":true}},"Web":{"example-dispatch.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8766"}}}}}`, settings.ServeConfig)
}

// Above one replica the operator has accepted two overlapping envoy-dispatch processes, so the
// Deployment rolls a new pod up before taking an old one down.
func TestDispatchSeveralReplicasRollWithoutGoingUnavailable(t *testing.T) {
	args := dispatchArgs()
	args.Replicas = pulumi.IntRef(3)
	settings, err := gcp.ResolveDispatch(args)
	require.NoError(t, err)
	require.Equal(t, 3, settings.Replicas)
	require.Equal(t, "RollingUpdate", settings.StrategyType)
	require.Equal(t, &gcp.DispatchRollingUpdate{MaxUnavailable: 0, MaxSurge: 1}, settings.RollingUpdate)
}

func TestDispatchResolverRefusesInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		edit func(*gcp.DispatchArgs)
		err  []string
	}{
		{name: "an invalid hostname", edit: func(a *gcp.DispatchArgs) { a.Hostname = "dispatch-" }, err: []string{"hostname", "^[a-z][a-z0-9-]{0,24}[a-z0-9]$"}},
		{name: "an invalid tailnet DNS name", edit: func(a *gcp.DispatchArgs) { a.TailnetDnsName = "Example.ts.net" }, err: []string{"tailnetDnsName", "ts.net"}},
		{name: "no tags", edit: func(a *gcp.DispatchArgs) { a.Tags = nil }, err: []string{"tags", "at least one"}},
		{name: "an invalid tag", edit: func(a *gcp.DispatchArgs) { a.Tags = []string{"example-dispatch"} }, err: []string{"tags[0]", "tag:"}},
		{name: "an unpinned Envoy image", edit: func(a *gcp.DispatchArgs) { a.EnvoyImage = "example.invalid/envoy:latest" }, err: []string{"envoyImage", "@sha256:"}},
		{name: "an unpinned Tailscale image", edit: func(a *gcp.DispatchArgs) { a.TailscaleImage = "example.invalid/tailscale:latest" }, err: []string{"tailscaleImage", "@sha256:"}},
		{name: "an unpinned Cloud SQL proxy image", edit: func(a *gcp.DispatchArgs) { a.CloudSqlProxyImage = "example.invalid/cloud-sql-proxy:latest" }, err: []string{"cloudSqlProxyImage", "@sha256:"}},
		{name: "no region", edit: func(a *gcp.DispatchArgs) { a.Region = "" }, err: []string{"region", "required"}},
		{name: "no sign-in client ID", edit: func(a *gcp.DispatchArgs) { a.SignInClientId = "" }, err: []string{"signInClientId", "required"}},
		{name: "an invalid sign-in domain", edit: func(a *gcp.DispatchArgs) { a.SignInDomain = "Example.COM" }, err: []string{"signInDomain", "lowercase DNS name"}},
		{name: "an empty database version", edit: func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef("") }, err: []string{"databaseVersion", "empty"}},
		{name: "an empty database tier", edit: func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef("") }, err: []string{"databaseTier", "empty"}},
		{name: "an invalid namespace", edit: func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef("Dispatch") }, err: []string{"namespace", "DNS label"}},
		{name: "an invalid allowed namespace", edit: func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", "Legion"} }, err: []string{"allowedNamespaces[1]", "DNS label"}},
		{name: "no tailnet auth key", edit: func(a *gcp.DispatchArgs) { a.TailnetAuthKey = nil }, err: []string{"tailnetAuthKey", "required"}},
		{name: "no NATS URL", edit: func(a *gcp.DispatchArgs) { a.NatsUrl = nil }, err: []string{"natsUrl", "required"}},
		{name: "no NATS seed secret", edit: func(a *gcp.DispatchArgs) { a.NatsSeedSecret = nil }, err: []string{"natsSeedSecret", "required"}},
		{name: "no sign-in client secret", edit: func(a *gcp.DispatchArgs) { a.SignInClientSecret = nil }, err: []string{"signInClientSecret", "required"}},
		{name: "fewer than one replica", edit: func(a *gcp.DispatchArgs) { a.Replicas = pulumi.IntRef(0) }, err: []string{"replicas", "at least 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := dispatchArgs()
			c.edit(args)
			_, err := gcp.ResolveDispatch(args)
			require.Error(t, err)
			for _, want := range c.err {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

func TestDispatchRefusesInvalidInputBeforeRegistering(t *testing.T) {
	args := dispatchArgs()
	args.Hostname = "dispatch-"
	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, `envoy-infra:gcp:Dispatch example-dispatch: hostname`)
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
}

func TestDispatchRequiresGCPAndRandomProvidersBeforeRegistering(t *testing.T) {
	m := newMocks()
	err := m.Run(func(ctx *pulumi.Context) error {
		gcpProvider, err := gcpsdk.NewProvider(ctx, "gcp-example", &gcpsdk.ProviderArgs{})
		if err != nil {
			return err
		}
		_, err = gcp.NewDispatch(ctx, "example-dispatch", dispatchArgs(), pulumi.Providers(gcpProvider))
		return err
	})
	require.ErrorContains(t, err, `envoy-infra:gcp:Dispatch needs a "random" provider in its providers option`)
	require.Equal(t, []string{gcpProviderType}, m.Types())
}

func TestDispatchRefusesAnNKeySeedInAPlainInput(t *testing.T) {
	key, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := key.Seed()
	require.NoError(t, err)

	args := dispatchArgs()
	args.Region = string(seed)
	_, err = gcp.ResolveDispatch(args)
	require.ErrorContains(t, err, "region")
	require.ErrorContains(t, err, "nkey seed")
	require.NotContains(t, err.Error(), string(seed))
}

func TestDispatchRefusesAnNKeySeedInAnAllowedNamespace(t *testing.T) {
	key, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := key.Seed()
	require.NoError(t, err)

	args := dispatchArgs()
	args.AllowedNamespaces = []string{"envoy", string(seed)}
	_, err = gcp.ResolveDispatch(args)
	require.ErrorContains(t, err, "allowedNamespaces[1]")
	require.ErrorContains(t, err, "nkey seed")
	require.NotContains(t, err.Error(), string(seed))
}

func TestDispatchRegistersItsCloudResources(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	want := map[string]int{
		"gcp:compute/network:Network":                       1,
		"gcp:compute/subnetwork:Subnetwork":                 1,
		"gcp:compute/router:Router":                         1,
		"gcp:compute/routerNat:RouterNat":                   1,
		"gcp:compute/globalAddress:GlobalAddress":           1,
		"gcp:servicenetworking/connection:Connection":       1,
		"gcp:container/cluster:Cluster":                     1,
		"gcp:sql/databaseInstance:DatabaseInstance":         1,
		"random:index/randomPassword:RandomPassword":        4,
		"gcp:sql/database:Database":                         1,
		"gcp:sql/user:User":                                 1,
		"gcp:secretmanager/secretIamMember:SecretIamMember": 1,
	}
	got := map[string]int{}
	for _, registration := range m.Registrations() {
		if _, wanted := want[registration.Type]; wanted {
			got[registration.Type]++
		}
	}
	require.Equal(t, want, got)
}

// The accessor's member is a principal in the cluster's Workload Identity pool, which Google
// creates with the cluster; IAM refuses a member of a pool that does not exist yet.
func TestDispatchSeedAccessorWaitsForTheCluster(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	deps := m.One(t, "gcp:secretmanager/secretIamMember:SecretIamMember").RPC.GetDependencies()
	require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$gcp:container/cluster:Cluster::example-dispatch",
		"the seed accessor is granted after the cluster that creates its principal's pool")
}

// GKE accepts only the project's own pool, named <project id>.svc.id.goog.
func TestDispatchClusterUsesTheProjectsWorkloadPool(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	config := m.One(t, "gcp:container/cluster:Cluster").Inputs.Mappable()["workloadIdentityConfig"].(map[string]any)
	require.Equal(t, "example-project.svc.id.goog", config["workloadPool"])
}

// The dispatch cluster syncs Secret Manager secrets into Kubernetes Secrets (Secret Sync), with
// rotation on so a new secret version reaches the synced Secret without an apply.
func TestDispatchClusterEnablesSecretSync(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	sync, ok := m.One(t, "gcp:container/cluster:Cluster").Inputs.Mappable()["secretSyncConfig"].(map[string]any)
	require.True(t, ok, "the cluster registers secretSyncConfig")
	require.Equal(t, true, sync["enabled"])
	rotation, ok := sync["rotationConfig"].(map[string]any)
	require.True(t, ok, "secretSyncConfig registers rotationConfig")
	require.Equal(t, true, rotation["enabled"])
}

func countRegistrations(m *pulumitest.Mocks, typ string) int {
	var n int
	for _, registration := range m.Registrations() {
		if registration.Type == typ {
			n++
		}
	}
	return n
}

// With existingNamespace, the component registers no Namespace, another program owning it, and
// every other registration is the default's.
func TestDispatchExistingNamespaceRegistersNoNamespace(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	args := dispatchArgs()
	yes := true
	args.ExistingNamespace = &yes
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	require.Zero(t, countRegistrations(m, "kubernetes:core/v1:Namespace"))

	var want []string
	for _, typ := range defaults.Types() {
		if typ != "kubernetes:core/v1:Namespace" {
			want = append(want, typ)
		}
	}
	require.ElementsMatch(t, want, m.Types())
}

func TestDispatchCreatesItsNamespaceByDefault(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.Equal(t, 1, countRegistrations(m, "kubernetes:core/v1:Namespace"))
}

func TestDispatchRegistersKubernetesResources(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	var kubernetesResources int
	for _, registration := range m.Registrations() {
		if strings.HasPrefix(registration.Type, "kubernetes:") {
			kubernetesResources++
		}
	}
	require.Positive(t, kubernetesResources)
}

// The listener binds 0.0.0.0 in a pod of its own, where its API refuses to start without a
// bearer, so its credential is a Secret of its own: the listener's pod must not name the Secret
// holding DATABASE_URL and DISPATCH_SIGNING_KEY.
func TestDispatchListenerSecretHoldsOnlyItsAPIToken(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	secret := m.Named(t, "kubernetes:core/v1:Secret", "example-dispatch-listener")
	require.Equal(t, "example-dispatch-listener", secret.Inputs.Mappable()["metadata"].(map[string]any)["name"])
	// The mock engine computes a password's result as <name>-result, so the value names the
	// listener's own token and no other password the component registers.
	data, _ := pulumitest.Plain(secret.Inputs["stringData"])
	require.Equal(t, map[string]any{"ENVOY_API_TOKEN": "example-dispatch-listener-token-result"}, normalizedProperty(data))
}

// One replica is the default, and its Deployment carries no rollingUpdate at all: Kubernetes
// refuses a rollingUpdate block on a Recreate Deployment.
func TestDispatchDeploymentRecreatesOneReplicaByDefault(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	spec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").Inputs.Mappable()["spec"].(map[string]any)
	require.EqualValues(t, 1, spec["replicas"])
	strategy := spec["strategy"].(map[string]any)
	require.Equal(t, "Recreate", strategy["type"])
	require.NotContains(t, strategy, "rollingUpdate")
}

func TestDispatchDeploymentRollsSeveralReplicas(t *testing.T) {
	args := dispatchArgs()
	args.Replicas = pulumi.IntRef(3)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	spec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").Inputs.Mappable()["spec"].(map[string]any)
	require.EqualValues(t, 3, spec["replicas"])
	strategy := spec["strategy"].(map[string]any)
	require.Equal(t, "RollingUpdate", strategy["type"])
	rollingUpdate := strategy["rollingUpdate"].(map[string]any)
	require.EqualValues(t, 0, rollingUpdate["maxUnavailable"])
	require.EqualValues(t, 1, rollingUpdate["maxSurge"])
}

func TestDispatchServiceExposesPort8766(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	service := m.Named(t, "kubernetes:core/v1:Service", "example-dispatch")
	inputs := service.Inputs.Mappable()
	metadata := inputs["metadata"].(map[string]any)
	require.Equal(t, "example-dispatch", metadata["name"])
	spec := inputs["spec"].(map[string]any)
	require.Equal(t, map[string]any{"app": "example-dispatch"}, spec["selector"])
	ports := spec["ports"].([]any)
	require.Len(t, ports, 1)
	port := ports[0].(map[string]any)
	require.EqualValues(t, 8766, port["port"])
	require.EqualValues(t, 8766, port["targetPort"])
}

func TestDispatchNetworkPolicyAdmitsNoNamespaceByDefault(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	policy := m.Named(t, "kubernetes:networking.k8s.io/v1:NetworkPolicy", "example-dispatch")
	spec := policy.Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, []any{"Ingress"}, spec["policyTypes"])
	require.Empty(t, spec["ingress"], "an ingress rule with no from would admit every pod in the cluster")
}

func TestDispatchNetworkPolicyAdmitsOnlyTheGivenNamespaces(t *testing.T) {
	args := dispatchArgs()
	args.AllowedNamespaces = []string{"envoy", "legion"}
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	policy := m.Named(t, "kubernetes:networking.k8s.io/v1:NetworkPolicy", "example-dispatch")
	spec := policy.Inputs.Mappable()["spec"].(map[string]any)
	ingress := spec["ingress"].([]any)[0].(map[string]any)
	from := ingress["from"].([]any)
	require.Len(t, from, 2)
	var namespaces []string
	for _, peer := range from {
		selector := peer.(map[string]any)["namespaceSelector"].(map[string]any)
		labels := selector["matchLabels"].(map[string]any)
		namespaces = append(namespaces, labels["kubernetes.io/metadata.name"].(string))
	}
	require.ElementsMatch(t, []string{"envoy", "legion"}, namespaces)
}

func TestDispatchRenderingsMatchGoldens(t *testing.T) {
	require.Equal(t, readGolden(t, "dispatch-kubeconfig.golden"),
		gcp.DispatchKubeconfig("example-endpoint", "ZXhhbXBsZS1jYQ==", "example-dispatch"))
	settings, err := gcp.ResolveDispatch(dispatchArgs())
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), settings.ServeConfig)
}

func normalizedProperty(value resource.PropertyValue) any {
	switch {
	case value.IsSecret():
		return "<secret>"
	case value.IsComputed():
		return "<unknown>"
	case value.IsNull():
		return nil
	case value.IsBool():
		return value.BoolValue()
	case value.IsNumber():
		return value.NumberValue()
	case value.IsString():
		return value.StringValue()
	case value.IsArray():
		values := value.ArrayValue()
		out := make([]any, len(values))
		for i, item := range values {
			out[i] = normalizedProperty(item)
		}
		return out
	case value.IsObject():
		out := map[string]any{}
		for key, item := range value.ObjectValue() {
			out[string(key)] = normalizedProperty(item)
		}
		return out
	case value.IsOutput():
		output := value.OutputValue()
		if output.Secret {
			return "<secret>"
		}
		if !output.Known {
			return "<unknown>"
		}
		return normalizedProperty(output.Element)
	default:
		panic(value.TypeString())
	}
}

func TestDispatchPodMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	deployment := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch")
	template := deployment.Inputs["spec"].ObjectValue()["template"]
	got, err := json.MarshalIndent(normalizedProperty(template), "", "  ")
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-pod.golden"), string(got)+"\n")
}
