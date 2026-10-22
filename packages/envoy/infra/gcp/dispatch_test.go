package gcp_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	gcpsdk "github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp"
	randomsdk "github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
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
	return runDispatchNamed(t, "example-dispatch", args, providers)
}

func runDispatchNamed(t *testing.T, name string, args *gcp.DispatchArgs, providers bool) (*pulumitest.Mocks, error) {
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
		_, err := gcp.NewDispatch(ctx, name, args, opts...)
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
	require.Equal(t, `{"TCP":{"443":{"HTTPS":true}},"Web":{"example-dispatch.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://example-dispatch.dispatch.svc.cluster.local:8766"}}}}}`, settings.ServeConfig)
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
		{name: "an invalid tailnet DNS name", edit: func(a *gcp.DispatchArgs) { a.TailnetDnsName = "Example-Tailnet.ts.net" }, err: []string{"tailnetDnsName", "ts.net"}},
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
		{name: "a namespace longer than a DNS label", edit: func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef(strings.Repeat("a", 64)) }, err: []string{"namespace", "DNS label"}},
		{name: "an allowed namespace longer than a DNS label", edit: func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{strings.Repeat("a", 64)} }, err: []string{"allowedNamespaces[0]", "DNS label"}},
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

// Every plain string input of the component has a row; the seed is found as bus.HoldsSeed finds
// it: any type, either case, whole, inside a longer value, or split. tailnetAuthKey,
// signInClientSecret and natsSeedSecret are Inputs, checked once they resolve
// (TestConstructRefusesASeedInADispatchInput), and natsUrl once it resolves too
// (TestDispatchRefusesABadNatsURLOnceItResolves).
func TestDispatchRefusesAnNKeySeedInAPlainInput(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	digest := "@sha256:" + strings.Repeat("0", 64)
	for _, row := range []struct {
		name, input string
		edit        func(*gcp.DispatchArgs)
	}{
		{"a seed as the region", "region", func(a *gcp.DispatchArgs) { a.Region = seed }},
		{"a seed as the hostname", "hostname", func(a *gcp.DispatchArgs) { a.Hostname = seed }},
		{"a hostname split by a hyphen", "hostname", func(a *gcp.DispatchArgs) { a.Hostname = splitSeed(lower, "-") }},
		{"a seed inside the tailnet DNS name", "tailnetDnsName", func(a *gcp.DispatchArgs) { a.TailnetDnsName = lower + ".ts.net" }},
		{"a seed in a tag", "tags[0]", func(a *gcp.DispatchArgs) { a.Tags = []string{"tag:" + seed} }},
		{"a lowercase seed in a tag", "tags[0]", func(a *gcp.DispatchArgs) { a.Tags = []string{"tag:" + lower} }},
		{"a seed in the Envoy image", "envoyImage", func(a *gcp.DispatchArgs) { a.EnvoyImage = "example.invalid/" + seed + digest }},
		{"a seed in the Tailscale image", "tailscaleImage", func(a *gcp.DispatchArgs) { a.TailscaleImage = "example.invalid/" + seed + digest }},
		{"a seed in the Cloud SQL proxy image", "cloudSqlProxyImage", func(a *gcp.DispatchArgs) { a.CloudSqlProxyImage = "example.invalid/" + seed + digest }},
		{"a seed as the sign-in client ID", "signInClientId", func(a *gcp.DispatchArgs) { a.SignInClientId = seed }},
		{"a seed inside the sign-in domain", "signInDomain", func(a *gcp.DispatchArgs) { a.SignInDomain = lower + ".example.com" }},
		{"a seed in the database version", "databaseVersion", func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef(seed) }},
		{"a seed in the database tier", "databaseTier", func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef(seed) }},
		{"a seed as the namespace", "namespace", func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef(lower) }},
		{"a seed in an allowed namespace", "allowedNamespaces[1]", func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", lower} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			require.EqualError(t, err, row.input+" holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}

// NewDispatch quotes the name in every refusal (its "%s %s: %w" wrapper) and registers the
// component under it, so a seed there is refused before anything registers, as NewNatsHub refuses
// one in its own name.
func TestDispatchRefusesASeedInTheComponentName(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name     string
		dispatch string
	}{
		{"a whole seed", seed},
		{"a seed inside a longer name", "dispatch-" + seed},
		{"a lowercase seed", strings.ToLower(seed)},
		{"a seed split by a hyphen", splitSeed(seed, "-")},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatchNamed(t, row.dispatch, dispatchArgs(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch: the component's name holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
}

// natsUrl is an Input, checked inside the ApplyT that renders the listener's environment: core's
// Env names its own inputs (natsUrls by index, socks5Proxy), neither of which gcp:Dispatch has,
// so the component maps each refusal onto natsUrl, a core.InputError, and wraps it with its type
// and name — it never panics, which would kill the provider process.
func TestDispatchRefusesABadNatsURLOnceItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	rows := []struct {
		name, url, want string
	}{
		{"a loopback URL", "nats://127.0.0.1:4222", "natsUrl names a local NATS server"},
		{"a URL holding a seed", "nats://example:" + seed + "@example-hub.example-tailnet.ts.net:4222", "natsUrl holds an nkey seed (not echoed)"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := gcp.DispatchListenerEnvError("example-dispatch", row.url)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "natsUrl", input.Input)
			require.ErrorContains(t, err, row.want)
			require.NotContains(t, err.Error(), "natsUrls")
			require.NotContains(t, err.Error(), "socks5Proxy")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)

			args := dispatchArgs()
			args.NatsUrl = pulumi.String(row.url)
			_, err = runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
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

	member := m.One(t, "gcp:secretmanager/secretIamMember:SecretIamMember")
	clusterURN := "urn:pulumi:s::p::" + dispatchType + "$gcp:container/cluster:Cluster::example-dispatch"
	require.Contains(t, member.RPC.GetDependencies(), clusterURN,
		"the seed accessor is granted after the cluster that creates its principal's pool")
	require.Contains(t, member.RPC.GetPropertyDependencies()["member"].GetUrns(), clusterURN,
		"the principal is built from the cluster's own Workload Identity pool, so it resolves only once the pool exists")
}

// The app and the listener mount the NATS seed, which the pod can read only once the seed
// accessor exists, so both Deployments register after it rather than racing IAM on the first
// deployment. The front door mounts no seed and waits for no grant.
func TestDispatchSeedMountingDeploymentsWaitForTheSeedAccessor(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	accessorURN := "urn:pulumi:s::p::" + dispatchType + "$gcp:secretmanager/secretIamMember:SecretIamMember::example-dispatch-nats-seed-accessor"
	for _, deployment := range []string{"example-dispatch", "example-dispatch-listener"} {
		require.Contains(t, m.Named(t, "kubernetes:apps/v1:Deployment", deployment).RPC.GetDependencies(), accessorURN,
			"%s mounts the seed, so it registers after the grant that lets it", deployment)
	}
	require.NotContains(t, m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-tailnet").RPC.GetDependencies(), accessorURN)
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

// deploymentPodTemplate returns one workload's pod template, with every output and secret
// wrapper off, the way the apiserver would see it.
func deploymentPodTemplate(t *testing.T, m *pulumitest.Mocks, deployment string) map[string]any {
	t.Helper()
	template := m.Named(t, "kubernetes:apps/v1:Deployment", deployment).Inputs["spec"].ObjectValue()["template"]
	return normalizedProperty(template).(map[string]any)
}

// podContainer returns the pod template's single container named name.
func podContainer(t *testing.T, template map[string]any, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, item := range template["spec"].(map[string]any)["containers"].([]any) {
		container := item.(map[string]any)
		if container["name"] == name {
			found = append(found, container)
		}
	}
	require.Len(t, found, 1, "one container named %s", name)
	return found[0]
}

// containerEnv indexes a container's environment by name, as the kubelet does: a plain variable
// maps to its value, and one from a Secret to its secretKeyRef.
func containerEnv(t *testing.T, container map[string]any) map[string]any {
	t.Helper()
	env := map[string]any{}
	for _, item := range container["env"].([]any) {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		require.NotContains(t, env, name, "the environment names %s once", name)
		if value, ok := entry["value"]; ok {
			env[name] = value
			continue
		}
		env[name] = entry["valueFrom"].(map[string]any)["secretKeyRef"]
	}
	return env
}

// A Deployment's name and its selector are immutable in place, and the consumer patches the app's
// Deployment by name: either one moving turns the consumer's preview into a replacement, which
// takes the app down, rather than an update.
func TestDispatchAppDeploymentKeepsItsNameAndSelector(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	deployment := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch")
	require.Equal(t, "example-dispatch", deployment.Name)
	inputs := deployment.Inputs.Mappable()
	require.Equal(t, "example-dispatch", inputs["metadata"].(map[string]any)["name"])
	spec := inputs["spec"].(map[string]any)
	require.Equal(t, map[string]any{"app": "example-dispatch"},
		spec["selector"].(map[string]any)["matchLabels"])
	require.Equal(t, map[string]any{"app": "example-dispatch"},
		deploymentPodTemplate(t, m, "example-dispatch")["metadata"].(map[string]any)["labels"],
		"the pods the selector matches are the pods this Deployment creates")
}

// One tailnet node holds the hostname and the single-use auth key's state, so the front door is
// recreated at one replica whatever the app's count; and a front-door pod wearing the app's label
// would be sent user traffic by Service example-dispatch.
func TestDispatchFrontDoorStaysSingleWhileTheAppScales(t *testing.T) {
	args := dispatchArgs()
	args.Replicas = pulumi.IntRef(3)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	spec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-tailnet").Inputs.Mappable()["spec"].(map[string]any)
	require.EqualValues(t, 1, spec["replicas"])
	strategy := spec["strategy"].(map[string]any)
	require.Equal(t, "Recreate", strategy["type"])
	require.NotContains(t, strategy, "rollingUpdate", "Kubernetes refuses a rollingUpdate block on a Recreate Deployment")

	template := deploymentPodTemplate(t, m, "example-dispatch-tailnet")
	labels := template["metadata"].(map[string]any)["labels"].(map[string]any)
	require.Equal(t, map[string]any{"app": "example-dispatch-tailnet"}, labels)
	require.NotEqual(t, "example-dispatch", labels["app"], "Service example-dispatch selects app=example-dispatch")
	pod := template["spec"].(map[string]any)
	require.Equal(t, "example-dispatch", pod["serviceAccountName"], "the state Secret's Role is bound to this account")
	require.Len(t, pod["containers"].([]any), 1)
	env := containerEnv(t, podContainer(t, template, "tailscale"))
	require.Equal(t, "example-dispatch-tailscale-state", env["TS_KUBE_SECRET"])
}

// The app's image reaching the front door's Deployment would roll the tailnet node, and with it
// the hostname every reader reaches Dispatch at, on every release of the app. Registering the same
// inputs twice is the only form a mock engine can state "the front door stays up" in.
func TestDispatchFrontDoorDoesNotRollOnTheAppsImage(t *testing.T) {
	first, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	args := dispatchArgs()
	args.EnvoyImage = "example.invalid/envoy@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	require.NotEqual(t, dispatchEnvoyImage, args.EnvoyImage)
	second, err := runDispatch(t, args, true)
	require.NoError(t, err)

	require.Equal(t,
		first.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-tailnet").Inputs,
		second.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-tailnet").Inputs,
		"the front door's Deployment carries nothing of the app's image")
	require.NotEqual(t,
		deploymentPodTemplate(t, first, "example-dispatch"),
		deploymentPodTemplate(t, second, "example-dispatch"),
		"the app's own pods carry the new image")
}

// requireWaitsForTheFrontDoor fails unless deployment is created after the front door's pod and
// its Service, the two a caller of the SOCKS5 proxy needs.
func requireWaitsForTheFrontDoor(t *testing.T, m *pulumitest.Mocks, deployment, dials string) {
	t.Helper()
	deps := m.Named(t, "kubernetes:apps/v1:Deployment", deployment).RPC.GetDependencies()
	require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$kubernetes:apps/v1:Deployment::example-dispatch-tailnet",
		dials+" is created after the pod running the SOCKS5 proxy")
	require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$kubernetes:core/v1:Service::example-dispatch-tailnet",
		dials+" dials the proxy at its Service, not at the pod")
}

// envoy-dispatch exits 1 when it cannot reach NATS through the proxy, and its /healthz answers 503
// until it connects, so an app pod started before the front door and its Service exist never goes
// ready and the rollout waits it out.
func TestDispatchAppWaitsForTheFrontDoor(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	requireWaitsForTheFrontDoor(t, m, "example-dispatch", "the app")
}

// envoy-listener dials the hub through the same proxy and exits on a failed initial connect
// (packages/envoy/cmd/listener/main.go), with no retry on that first attempt, so a listener pod
// started before the front door crash-loops into Kubernetes' backoff while the update waits out
// its rollout. It waits for the front door too, on a fresh create and on every replacement.
func TestDispatchListenerWaitsForTheFrontDoor(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	requireWaitsForTheFrontDoor(t, m, "example-dispatch-listener", "the listener")
}

// Two listeners with one machine id never both bind that id's exclusive durable, and a listener
// bound to 0.0.0.0 refuses to start without a bearer. The app reaches the listener and the proxy
// over the network now, each running in a pod of its own.
func TestDispatchListenerStaysSingleAndAuthenticated(t *testing.T) {
	args := dispatchArgs()
	args.Replicas = pulumi.IntRef(3)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	spec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-listener").Inputs.Mappable()["spec"].(map[string]any)
	require.EqualValues(t, 1, spec["replicas"])
	strategy := spec["strategy"].(map[string]any)
	require.Equal(t, "Recreate", strategy["type"])
	require.NotContains(t, strategy, "rollingUpdate")

	const socks5Proxy = "example-dispatch-tailnet.dispatch.svc.cluster.local:1055"
	listenerToken := map[string]any{"name": "example-dispatch-listener", "key": "ENVOY_API_TOKEN"}
	listener := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch-listener"), "envoy-listener"))
	require.Equal(t, "example-dispatch", listener["ENVOY_MACHINE_ID"])
	require.Equal(t, "0.0.0.0", listener["ENVOY_LISTEN_HOST"])
	require.Equal(t, listenerToken, listener["ENVOY_API_TOKEN"])
	require.Equal(t, socks5Proxy, listener["ENVOY_NATS_SOCKS5_PROXY"])

	app := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch"))
	require.Equal(t, "http://example-dispatch-listener.dispatch.svc.cluster.local:9020", app["ENVOY_URL"])
	require.Equal(t, listenerToken, app["ENVOY_TOKEN"])
	require.Equal(t, socks5Proxy, app["ENVOY_NATS_SOCKS5_PROXY"])
}

// A pod the apiserver has marked terminating leaves routing asynchronously, and it does so
// alongside its own shutdown rather than after it: the control plane takes the pod out of its
// EndpointSlice when the deletion timestamp appears, concurrently with the kubelet beginning
// graceful termination - the pre-stop hook first, SIGTERM once the hook returns - so the hook's
// sleep and the propagation of that removal to every node's proxy and to the front door's
// Tailscale Serve run at the same time, and the sleep is sized to outlast the propagation.
// envoy-dispatch closes its HTTP listener as the signal arrives
// (packages/envoy/cmd/dispatch/shutdown.go), so without the wait a pod still being sent requests
// would already have stopped answering them. The app's container therefore waits before it is
// signalled, and since the kubelet runs that wait inside the pod's one grace period, the pod asks
// for the wait on top of the server's own drain budget, so the wait costs the server nothing.
// This pins the pair the deployment renders; the inequality between the pod's budget and the
// server's shutdown is held in packages/envoy/cmd/dispatch/pod_grace_period_test.go, which reads
// stopGrace itself.
func TestDispatchAppPodKeepsServingUntilItsRemovalFromRoutingPropagates(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	// The 35 s is this 5 s wait plus the 30 s cmd/dispatch/shutdown.go's stopGrace builds the
	// server's shutdown for. Both numbers are spelled out here, since this test pins what the
	// component renders; packages/envoy/cmd/dispatch/pod_grace_period_test.go is where the
	// inequality between the pod's budget and that shutdown lives, held from the side that can
	// read stopGrace.
	const wait = 5
	template := deploymentPodTemplate(t, m, "example-dispatch")
	require.EqualValues(t, 35, template["spec"].(map[string]any)["terminationGracePeriodSeconds"])
	app := podContainer(t, template, "envoy-dispatch")
	require.Equal(t, map[string]any{"preStop": map[string]any{"exec": map[string]any{"command": []any{"sleep", strconv.Itoa(wait)}}}},
		app["lifecycle"])
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

// policyIngressRules returns one NetworkPolicy's ingress rules, failing when the policy covers
// other pods than its own workload, is not an Ingress policy, or carries another number of rules
// than count.
func policyIngressRules(t *testing.T, m *pulumitest.Mocks, name string, count int) []any {
	t.Helper()
	spec := m.Named(t, "kubernetes:networking.k8s.io/v1:NetworkPolicy", name).Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, map[string]any{"app": name}, spec["podSelector"].(map[string]any)["matchLabels"])
	require.Equal(t, []any{"Ingress"}, spec["policyTypes"])
	ingress := spec["ingress"].([]any)
	require.Len(t, ingress, count)
	return ingress
}

// ingressPods returns the app labels one ingress rule admits at port, failing when the rule admits
// another port, admits a namespace, or carries an empty from, which admits every pod in the
// cluster.
func ingressPods(t *testing.T, rule any, port int) []string {
	t.Helper()
	fields := rule.(map[string]any)
	ports := fields["ports"].([]any)
	require.Len(t, ports, 1)
	require.EqualValues(t, port, ports[0].(map[string]any)["port"])
	from := fields["from"].([]any)
	require.NotEmpty(t, from)
	apps := make([]string, 0, len(from))
	for _, item := range from {
		peer := item.(map[string]any)
		require.NotContains(t, peer, "namespaceSelector")
		require.NotContains(t, peer, "ipBlock")
		apps = append(apps, peer["podSelector"].(map[string]any)["matchLabels"].(map[string]any)["app"].(string))
	}
	return apps
}

// policyIngressPods returns the app labels a NetworkPolicy's single ingress rule admits at port.
func policyIngressPods(t *testing.T, m *pulumitest.Mocks, name string, port int) []string {
	t.Helper()
	return ingressPods(t, policyIngressRules(t, m, name, 1)[0], port)
}

// With no namespace named, the app's policy carries exactly one rule: the front door's. Serve runs
// in a pod of this namespace, which no namespace selector covers, so without that rule it reaches
// the app at no port at all; a rule with an empty from would admit every pod in the cluster.
func TestDispatchNetworkPolicyAdmitsNoNamespaceByDefault(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	require.Equal(t, []string{"example-dispatch-tailnet"}, policyIngressPods(t, m, "example-dispatch", 8766))
}

// With namespaces named, the shape the consuming stack passes, the app's policy carries those two
// rules and no other: the named namespaces on 8766, and the front door's pod on 8766, which Serve
// needs and which no namespace selector covers.
func TestDispatchNetworkPolicyAdmitsOnlyTheGivenNamespaces(t *testing.T) {
	args := dispatchArgs()
	args.AllowedNamespaces = []string{"envoy", "legion"}
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	rules := policyIngressRules(t, m, "example-dispatch", 2)
	namespaceRule := rules[0].(map[string]any)
	ports := namespaceRule["ports"].([]any)
	require.Len(t, ports, 1)
	require.EqualValues(t, 8766, ports[0].(map[string]any)["port"])
	from := namespaceRule["from"].([]any)
	require.Len(t, from, 2)
	var namespaces []string
	for _, peer := range from {
		selector := peer.(map[string]any)["namespaceSelector"].(map[string]any)
		labels := selector["matchLabels"].(map[string]any)
		namespaces = append(namespaces, labels["kubernetes.io/metadata.name"].(string))
	}
	require.ElementsMatch(t, []string{"envoy", "legion"}, namespaces)
	require.Equal(t, []string{"example-dispatch-tailnet"}, ingressPods(t, rules[1], 8766))
}

// Whatever reaches the proxy at 1055 gets tailnet egress as this node, so only the two workloads
// that dial the hub through it may; and only the app calls the listener's API at 9020.
func TestDispatchPoliciesFenceTheProxyAndTheListener(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"example-dispatch", "example-dispatch-listener"},
		policyIngressPods(t, m, "example-dispatch-tailnet", 1055))
	require.Equal(t, []string{"example-dispatch"},
		policyIngressPods(t, m, "example-dispatch-listener", 9020))
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

// requirePodGolden pins one workload's pod template: the design's record of what runs beside what,
// and of the addresses each container is given.
func requirePodGolden(t *testing.T, m *pulumitest.Mocks, deployment, golden string) {
	t.Helper()
	got, err := json.MarshalIndent(deploymentPodTemplate(t, m, deployment), "", "  ")
	require.NoError(t, err)
	require.Equal(t, readGolden(t, golden), string(got)+"\n")
}

// The app's pod holds envoy-dispatch and the cloud-sql-proxy native sidecar alone, and reaches the
// front door's proxy and the listener at their Services.
func TestDispatchPodMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", "dispatch-pod.golden")
}

// The front door is one tailnet node, in a pod labelled app=<hostname>-tailnet: Service <hostname>
// selects app=<hostname> and would send user traffic to a pod wearing that label.
func TestDispatchTailnetPodMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
}

// The listener serves on every address of its own pod, so it carries the bearer its API demands
// there.
func TestDispatchListenerPodMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
}
