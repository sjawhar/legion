package gcp_test

import (
	"encoding/json"
	"fmt"
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

// setApp sets the four GitHub App inputs, which come together.
func setApp(a *gcp.DispatchArgs) {
	a.AppClientId = "example-app-client-id"
	a.AppId = "123456"
	a.AppPrivateKeySecretName = "example-app-key"
	a.AppPrivateKeySecretKey = "private-key.b64"
}

// dispatchBrokerAndAppArgs is dispatchArgs with both optional groups set: the secrets-broker relay
// (agentSecretsUrl with agentSecretsAudience) and the GitHub App (appClientId, appId and the
// existing Kubernetes Secret and key that hold the private key).
func dispatchBrokerAndAppArgs(t *testing.T) *gcp.DispatchArgs {
	t.Helper()
	args := dispatchArgs()
	args.AgentSecretsUrl = "https://example-broker.internal.example"
	args.AgentSecretsAudience = "example-broker-audience"
	setApp(args)
	return args
}

const (
	azureEmbedEndpoint  = "https://example-resource.services.ai.azure.com/models"
	azureEmbedKeySecret = "projects/example-project/secrets/example-azure-embed-key"
	azureTenantID       = "example-azure-tenant-id"
	azureClientID       = "example-azure-client-id"
	azureIssuer         = "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-dispatch"
)

// setAzureEndpoint sets the Azure embedder's endpoint and quota, which come together, and no
// credential.
func setAzureEndpoint(a *gcp.DispatchArgs) {
	a.AzureEmbedEndpoint = pulumi.String(azureEmbedEndpoint)
	a.AzureEmbedTokensPerMinute = pulumi.IntRef(300000)
}

// setAzureKey sets the Azure embedder with the key form of credential, the Secret Manager path of
// the key, and no deployment, which Dispatch then defaults.
func setAzureKey(a *gcp.DispatchArgs) {
	setAzureEndpoint(a)
	a.AzureEmbedKeySecret = pulumi.String(azureEmbedKeySecret)
}

// setAzureFederation sets the Azure embedder with the keyless form of credential, a deployment
// named, and the default audience.
func setAzureFederation(a *gcp.DispatchArgs) {
	setAzureEndpoint(a)
	a.AzureEmbedDeployment = pulumi.String("example-embed-deployment")
	a.AzureTenantId = pulumi.String(azureTenantID)
	a.AzureClientId = pulumi.String(azureClientID)
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

// Unset, appCpu and appMemory are the 1 CPU and 4Gi the app container always had, as requests and
// limits of envoy-dispatch, so a deployment that sets neither sees no change to its pod
// (TestDispatchPodMatchesGolden pins the whole template).
func TestDispatchAppContainerDefaultsToOneCPUAnd4Gi(t *testing.T) {
	settings, err := gcp.ResolveDispatch(dispatchArgs())
	require.NoError(t, err)
	require.Equal(t, "1", settings.AppCPU)
	require.Equal(t, "4Gi", settings.AppMemory)
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	container := podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch")
	require.Equal(t, appResources("1", "4Gi"), container["resources"])
}

// The quantity forms Kubernetes reads are taken as written, up to Autopilot's bounds for the pod:
// with cloud-sql-proxy's 200m and 256Mi, 300m and 256Mi make the 500m and 512Mi floor, 29800m
// and 112384Mi the 30 CPU and 110Gi ceiling, and 800m and 6400Mi exactly 6.5 GiB per CPU. A
// decimal exponent inside e-9 to e18 is valued exactly, its mantissa included.
func TestDispatchAcceptsAppResourcesWithinAutopilotsBounds(t *testing.T) {
	for _, row := range []struct{ cpu, memory string }{
		{"300m", "512Mi"},
		{"300m", "256Mi"},
		{"1.5", "6Gi"},
		{"2", "8589934592"},
		{"4", "16G"},
		{"29800m", "112384Mi"},
		{"2e0", "0.5e10"},
		{"800m", "6400Mi"},
		{"1", "4500000k"},
		{"1000000000e-9", "0.000000004e18"},
	} {
		t.Run(row.cpu+"/"+row.memory, func(t *testing.T) {
			args := dispatchArgs()
			args.AppCpu = pulumi.StringRef(row.cpu)
			args.AppMemory = pulumi.StringRef(row.memory)
			settings, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
			require.Equal(t, row.cpu, settings.AppCPU)
			require.Equal(t, row.memory, settings.AppMemory)
		})
	}
}

// Unset, pprofPort renders no DISPATCH_PPROF_PORT, so Dispatch starts no profiler and a deployment
// that leaves it unset renders the pod it always had (TestDispatchPodMatchesGolden pins it).
func TestDispatchRendersNoProfilerByDefault(t *testing.T) {
	settings, err := gcp.ResolveDispatch(dispatchArgs())
	require.NoError(t, err)
	require.Zero(t, settings.PprofPort)
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.NotContains(t, containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch")), "DISPATCH_PPROF_PORT")
}

// pprofPort sets DISPATCH_PPROF_PORT on the envoy-dispatch container alone, where Dispatch serves
// its profiler on 127.0.0.1 at that port: the app's pod template is the default's with that one
// variable added to that one container, and the front door's and the listener's pods match their
// goldens.
func TestDispatchPprofPortReachesTheAppContainerAlone(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, port := range []int{1, 6060, 65535} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			args := dispatchArgs()
			args.PprofPort = pulumi.IntRef(port)
			settings, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
			require.Equal(t, port, settings.PprofPort)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			got := deploymentPodTemplate(t, m, "example-dispatch")
			require.Equal(t, strconv.Itoa(port), containerEnv(t, podContainer(t, got, "envoy-dispatch"))["DISPATCH_PPROF_PORT"])
			want := deploymentPodTemplate(t, defaults, "example-dispatch")
			app := podContainer(t, want, "envoy-dispatch")
			app["env"] = append(app["env"].([]any), map[string]any{"name": "DISPATCH_PPROF_PORT", "value": strconv.Itoa(port)})
			require.Equal(t, want, got)
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
		})
	}
}

// A pprofPort Dispatch would refuse, or one the app pod already binds, is refused by the resolver
// before the component registers, so nothing beyond the test's providers exists.
func TestDispatchRefusesAPprofPortBeforeRegistering(t *testing.T) {
	args := dispatchArgs()
	args.PprofPort = pulumi.IntRef(70000)
	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: pprofPort 70000 must be a port number 1-65535")
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
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
		{name: "an agent secrets URL with a path", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example/v1"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no path"}},
		{name: "an agent secrets URL without a scheme", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "example-broker.internal.example"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "absolute http(s) URL"}},
		{name: "an agent secrets URL with a user and password", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://user:example-pass@example-broker.internal.example"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a user alone", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://user@example-broker.internal.example"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a query", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example?token=example-pass"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with an empty query", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example?"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a fragment", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example#example-pass"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with an empty fragment", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example#"
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets audience without its URL", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsAudience = "example-broker-audience"
		}, err: []string{"agentSecretsUrl", "required when agentSecretsAudience is set"}},
		{name: "an agent secrets URL without its audience", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example"
		}, err: []string{"agentSecretsAudience", "required when agentSecretsUrl is set"}},
		{name: "an app client ID alone", edit: func(a *gcp.DispatchArgs) { a.AppClientId = "example-app-client-id" }, err: []string{"appId, appPrivateKeySecretName, appPrivateKeySecretKey", "required when appClientId is set"}},
		{name: "an app private key secret name alone", edit: func(a *gcp.DispatchArgs) { a.AppPrivateKeySecretName = "example-app-key" }, err: []string{"appClientId, appId, appPrivateKeySecretKey", "required when appPrivateKeySecretName is set"}},
		{name: "an app without its ID", edit: func(a *gcp.DispatchArgs) {
			a.AppClientId = "example-app-client-id"
			a.AppPrivateKeySecretName = "example-app-key"
			a.AppPrivateKeySecretKey = "key.pem"
		}, err: []string{"appId", "required when appClientId is set"}},
		{name: "a non-numeric app ID", edit: func(a *gcp.DispatchArgs) { setApp(a); a.AppId = "12ab" }, err: []string{"appId", "numeric"}},
		{name: "a zero app ID", edit: func(a *gcp.DispatchArgs) { setApp(a); a.AppId = "0" }, err: []string{"appId", "numeric"}},
		{name: "an invalid app private key secret name", edit: func(a *gcp.DispatchArgs) { setApp(a); a.AppPrivateKeySecretName = "Example_Key" }, err: []string{"appPrivateKeySecretName", "DNS subdomain"}},
		{name: "an invalid app private key secret key", edit: func(a *gcp.DispatchArgs) { setApp(a); a.AppPrivateKeySecretKey = "bad/key" }, err: []string{"appPrivateKeySecretKey", "must match"}},
		{name: "fewer than one replica", edit: func(a *gcp.DispatchArgs) { a.Replicas = pulumi.IntRef(0) }, err: []string{"replicas", "at least 1"}},
		{name: "a pprof port of 0", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(0) }, err: []string{"pprofPort 0", "1-65535", "leave it unset"}},
		{name: "a negative pprof port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(-1) }, err: []string{"pprofPort -1", "1-65535"}},
		{name: "a pprof port past 65535", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(65536) }, err: []string{"pprofPort 65536", "1-65535"}},
		{name: "a pprof port on envoy-dispatch's own port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(8766) }, err: []string{"pprofPort 8766", "envoy-dispatch"}},
		{name: "a pprof port on cloud-sql-proxy's port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(5432) }, err: []string{"pprofPort 5432", "cloud-sql-proxy"}},
		{name: "an empty app priority class", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef("") }, err: []string{"appPriorityClassName is empty", "leave it unset for no priority class"}},
		{name: "an app priority class that is no DNS subdomain", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef("Example_Priority") }, err: []string{`appPriorityClassName "Example_Priority"`, "DNS subdomain"}},
		{name: "an app priority class with trailing whitespace", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef("example-priority ") }, err: []string{`appPriorityClassName "example-priority "`, "DNS subdomain"}},
		{name: "an app priority class longer than 253 characters", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef(strings.Repeat("a.", 127) + "a") }, err: []string{"appPriorityClassName", "DNS subdomain"}},
		{name: "a system cluster-critical app priority class", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef("system-cluster-critical") }, err: []string{`appPriorityClassName "system-cluster-critical"`, "reserves for its own system components"}},
		{name: "a system node-critical app priority class", edit: func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef("system-node-critical") }, err: []string{`appPriorityClassName "system-node-critical"`, "reserves for its own system components"}},
		{name: "a disruption budget of no pod", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(0) }, err: []string{"appPdbMinAvailable 0", "from 1 to replicas (1)", "leave it unset for no PodDisruptionBudget"}},
		{name: "a negative disruption budget", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(-1) }, err: []string{"appPdbMinAvailable -1", "from 1 to replicas (1)"}},
		{name: "a disruption budget above the default replica count", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(2) }, err: []string{"appPdbMinAvailable 2", "from 1 to replicas (1)"}},
		{name: "a disruption budget above the replica count", edit: func(a *gcp.DispatchArgs) {
			a.Replicas = pulumi.IntRef(3)
			a.AppPdbMinAvailable = pulumi.IntRef(4)
		}, err: []string{"appPdbMinAvailable 4", "from 1 to replicas (3)"}},
		{name: "an app CPU that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("two") }, err: []string{`appCpu "two"`, "Kubernetes quantity"}},
		{name: "an app CPU with a memory suffix in the wrong case", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("1gi") }, err: []string{`appCpu "1gi"`, "Kubernetes quantity"}},
		{name: "an app memory that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("4 GiB") }, err: []string{`appMemory "4 GiB"`, "Kubernetes quantity"}},
		{name: "an empty app CPU", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("") }, err: []string{"appCpu is empty", "leave it unset for 1"}},
		{name: "an empty app memory", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("") }, err: []string{"appMemory is empty", "leave it unset for 4Gi"}},
		{name: "a negative app CPU", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("-1") }, err: []string{`appCpu "-1"`, "at least 300m", "less than 500m", "Autopilot raises it"}},
		{name: "an app CPU below Autopilot's pod floor", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("250m") }, err: []string{`appCpu "250m"`, "at least 300m", "cloud-sql-proxy's 200m", "less than 500m", "Autopilot raises it"}},
		{name: "an app memory below Autopilot's pod floor", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("500m")
			a.AppMemory = pulumi.StringRef("128Mi")
		}, err: []string{`appMemory "128Mi"`, "at least 256Mi", "cloud-sql-proxy's 256Mi", "less than 512Mi", "Autopilot raises it"}},
		{name: "an app CPU above Autopilot's pod ceiling", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("30")
			a.AppMemory = pulumi.StringRef("64Gi")
		}, err: []string{`appCpu "30"`, "at most 29800m", "above 30", "rejects"}},
		{name: "an app memory above Autopilot's pod ceiling", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("20")
			a.AppMemory = pulumi.StringRef("110Gi")
		}, err: []string{`appMemory "110Gi"`, "at most 112384Mi", "above 110Gi", "rejects"}},
		{name: "app CPU raised without memory to match", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("8") }, err: []string{`appCpu "8"`, `appMemory "4Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "app memory raised without CPU to match", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("32Gi") }, err: []string{`appCpu "1"`, `appMemory "32Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "app memory just above 6.5 GiB per CPU", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("800m")
			a.AppMemory = pulumi.StringRef("6401Mi")
		}, err: []string{`appCpu "800m"`, `appMemory "6401Mi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "an app CPU with leading whitespace", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef(" 1") }, err: []string{`appCpu " 1"`, "Kubernetes quantity"}},
		{name: "an app CPU with trailing whitespace", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("1 ") }, err: []string{`appCpu "1 "`, "Kubernetes quantity"}},
		{name: "an app memory with leading whitespace", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef(" 4Gi") }, err: []string{`appMemory " 4Gi"`, "Kubernetes quantity"}},
		{name: "an app memory with a trailing line break", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("4Gi\n") }, err: []string{`appMemory "4Gi\n"`, "Kubernetes quantity"}},
		// Kubernetes reads this as 100 CPU: the mantissa makes up for the exponent.
		{name: "an app CPU whose exponent a small mantissa makes up for", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("0.000000000000000000000000000000000001e38")
		}, err: []string{`appCpu "0.000000000000000000000000000000000001e38"`, "decimal exponent outside e-9 to e18"}},
		// Kubernetes reads this as 4 CPU, a size the pod could have with 6Gi.
		{name: "an app CPU of 4 written with a large exponent", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("0.0000000000000000000000000000000000004e37")
			a.AppMemory = pulumi.StringRef("6Gi")
		}, err: []string{`appCpu "0.0000000000000000000000000000000000004e37"`, "decimal exponent outside e-9 to e18"}},
		// Kubernetes reads this as 10 CPU.
		{name: "an app CPU whose negative exponent a long mantissa makes up for", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("10000000000000000000000000000000000000000e-39")
		}, err: []string{`appCpu "10000000000000000000000000000000000000000e-39"`, "decimal exponent outside e-9 to e18"}},
		{name: "an app memory with an exponent past int's range", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("1e99999999999999999999") }, err: []string{`appMemory "1e99999999999999999999"`, "decimal exponent outside e-9 to e18"}},
		{name: "an Azure endpoint without its quota", edit: func(a *gcp.DispatchArgs) {
			setAzureKey(a)
			a.AzureEmbedTokensPerMinute = nil
		}, err: []string{"azureEmbedTokensPerMinute is required when azureEmbedEndpoint is set"}},
		{name: "a zero Azure quota", edit: func(a *gcp.DispatchArgs) { setAzureKey(a); a.AzureEmbedTokensPerMinute = pulumi.IntRef(0) }, err: []string{"azureEmbedTokensPerMinute 0", "positive integer"}},
		{name: "a negative Azure quota", edit: func(a *gcp.DispatchArgs) { setAzureKey(a); a.AzureEmbedTokensPerMinute = pulumi.IntRef(-1) }, err: []string{"azureEmbedTokensPerMinute -1", "positive integer"}},
		{name: "an Azure deployment without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureEmbedDeployment = pulumi.String("example-embed-deployment") }, err: []string{"azureEmbedEndpoint is required when azureEmbedDeployment is set"}},
		{name: "an Azure quota without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureEmbedTokensPerMinute = pulumi.IntRef(300000) }, err: []string{"azureEmbedEndpoint is required when azureEmbedTokensPerMinute is set"}},
		{name: "an Azure key without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureEmbedKeySecret = pulumi.String(azureEmbedKeySecret) }, err: []string{"azureEmbedEndpoint is required when azureEmbedKeySecret is set"}},
		{name: "an Azure tenant without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureTenantId = pulumi.String(azureTenantID) }, err: []string{"azureEmbedEndpoint is required when azureTenantId is set"}},
		{name: "an Azure client without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureClientId = pulumi.String(azureClientID) }, err: []string{"azureEmbedEndpoint is required when azureClientId is set"}},
		{name: "an Azure audience without the endpoint", edit: func(a *gcp.DispatchArgs) { a.AzureFederationAudience = pulumi.StringRef("api://example-audience") }, err: []string{"azureEmbedEndpoint is required when azureFederationAudience is set"}},
		{name: "both Azure credential forms", edit: func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureEmbedKeySecret = pulumi.String(azureEmbedKeySecret)
		}, err: []string{"azureEmbedKeySecret is set with azureTenantId or azureClientId", "exactly one"}},
		{name: "an Azure endpoint without a credential", edit: setAzureEndpoint, err: []string{"azureEmbedEndpoint needs one credential: azureEmbedKeySecret, or azureTenantId with azureClientId"}},
		{name: "an Azure tenant without its client", edit: func(a *gcp.DispatchArgs) {
			setAzureEndpoint(a)
			a.AzureTenantId = pulumi.String(azureTenantID)
		}, err: []string{"azureClientId is required when azureTenantId is set"}},
		{name: "an Azure client without its tenant", edit: func(a *gcp.DispatchArgs) {
			setAzureEndpoint(a)
			a.AzureClientId = pulumi.String(azureClientID)
		}, err: []string{"azureTenantId is required when azureClientId is set"}},
		{name: "an Azure audience with the key form", edit: func(a *gcp.DispatchArgs) {
			setAzureKey(a)
			a.AzureFederationAudience = pulumi.StringRef("api://example-audience")
		}, err: []string{"azureFederationAudience is set without azureTenantId and azureClientId"}},
		{name: "an empty Azure audience", edit: func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureFederationAudience = pulumi.StringRef("")
		}, err: []string{"azureFederationAudience is empty", "api://AzureADTokenExchange"}},
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
			require.NotContains(t, err.Error(), "example-pass", "a refusal never quotes the URL")
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
// (TestConstructRefusesASeedInADispatchInput), as are the Azure Inputs
// (TestConstructRefusesASeedInAnAzureInput), and natsUrl once it resolves too
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
		{"a seed as the app CPU", "appCpu", func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef(seed) }},
		{"a seed as the app memory", "appMemory", func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef(seed) }},
		{"a seed as the app priority class", "appPriorityClassName", func(a *gcp.DispatchArgs) { a.AppPriorityClassName = pulumi.StringRef(lower) }},
		{"a seed as the namespace", "namespace", func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef(lower) }},
		{"a seed in an allowed namespace", "allowedNamespaces[1]", func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", lower} }},
		{"a seed inside the agent secrets URL", "agentSecretsUrl", func(a *gcp.DispatchArgs) { a.AgentSecretsUrl = "https://" + lower + ".internal.example" }},
		{"a seed inside the agent secrets audience", "agentSecretsAudience", func(a *gcp.DispatchArgs) { a.AgentSecretsAudience = "https://" + lower + ".internal.example" }},
		{"a seed as the app client ID", "appClientId", func(a *gcp.DispatchArgs) { a.AppClientId = seed }},
		{"a seed as the app ID", "appId", func(a *gcp.DispatchArgs) { a.AppId = seed }},
		{"a seed as the app private key secret name", "appPrivateKeySecretName", func(a *gcp.DispatchArgs) { a.AppPrivateKeySecretName = lower }},
		{"a seed as the app private key secret key", "appPrivateKeySecretKey", func(a *gcp.DispatchArgs) { a.AppPrivateKeySecretKey = seed }},
		{"a seed in the Azure federation audience", "azureFederationAudience", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureFederationAudience = pulumi.StringRef("api://" + lower)
		}},
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
// creates with the cluster; IAM may refuse a member of a pool that does not exist yet (unproven:
// the plan's hardening ledger), so the grant waits for the cluster.
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

// Without the annotation the cluster autoscaler evicts a pod whenever it consolidates its node,
// and the one-replica front door takes Dispatch off the tailnet until the replacement is ready:
// every Deployment the component registers, now the app, the front door and the listener, carries it.
func TestDispatchPodsAreNotEvictedForScaleDown(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	var deployments []string
	for _, r := range m.Registrations() {
		if r.Type != "kubernetes:apps/v1:Deployment" {
			continue
		}
		deployments = append(deployments, r.Name)
		annotations, _ := deploymentPodTemplate(t, m, r.Name)["metadata"].(map[string]any)["annotations"].(map[string]any)
		require.Equal(t, "false", annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"],
			"Deployment %s's pod template lacks cluster-autoscaler.kubernetes.io/safe-to-evict: \"false\"", r.Name)
	}
	require.ElementsMatch(t, []string{"example-dispatch", "example-dispatch-tailnet", "example-dispatch-listener"}, deployments)
}

// Pulumi's deletionProtection stops only a Pulumi delete; the instance's own setting stops the API
// and the console too, and the same input drives both.
func TestDispatchProtectsItsInstanceServerSide(t *testing.T) {
	for _, protect := range []bool{true, false} {
		args := dispatchArgs()
		args.DeletionProtection = &protect
		m, err := runDispatch(t, args, true)
		require.NoError(t, err)
		inputs := m.One(t, "gcp:sql/databaseInstance:DatabaseInstance").Inputs.Mappable()
		require.Equal(t, protect, inputs["deletionProtection"])
		require.Equal(t, protect, inputs["settings"].(map[string]any)["deletionProtectionEnabled"])
		policy := "DELETE"
		if protect {
			policy = "PREVENT"
		}
		require.Equal(t, policy, m.One(t, "gcp:sql/database:Database").Inputs.Mappable()["deletionPolicy"])
	}
}

// Dispatch itself trims one trailing slash off DISPATCH_AGENT_SECRETS_URL, so the component
// renders the value as Dispatch reads it.
func TestDispatchTrimsTheAgentSecretsUrlsTrailingSlash(t *testing.T) {
	args := dispatchBrokerAndAppArgs(t)
	args.AgentSecretsUrl = "https://example-broker.internal.example/"
	settings, err := gcp.ResolveDispatch(args)
	require.NoError(t, err)
	require.Equal(t, "https://example-broker.internal.example", settings.AgentSecretsURL)
}

// A blank or whitespace-only value of any secret input Dispatch reads as a credential renders a
// Secret key Dispatch refuses only at boot (an empty client secret, an empty auth key), so the
// component refuses it once the input resolves, naming the input and never the value: one rule,
// refuseDispatchSecret, which both inputs' resolved checks run before anything else.
func TestDispatchRefusesABlankSecretOnceItResolves(t *testing.T) {
	for _, input := range []struct {
		name string
		edit func(*gcp.DispatchArgs, pulumi.StringInput)
	}{
		{"tailnetAuthKey", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.TailnetAuthKey = v }},
		{"signInClientSecret", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.SignInClientSecret = v }},
	} {
		for _, row := range []struct{ name, value string }{
			{"empty", ""},
			{"whitespace only", " \n\t"},
		} {
			t.Run(input.name+", "+row.name, func(t *testing.T) {
				args := dispatchBrokerAndAppArgs(t)
				input.edit(args, pulumi.String(row.value))
				_, err := runDispatch(t, args, true)
				require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+input.name+
					" is blank, which no credential is")
			})
		}
	}
}

func unwrappedProperty(value resource.PropertyValue) resource.PropertyValue {
	for {
		switch {
		case value.IsSecret():
			value = value.SecretValue().Element
		case value.IsOutput():
			value = value.OutputValue().Element
		default:
			return value
		}
	}
}

// registrationsOf is every registration of type typ, in order.
func registrationsOf(m *pulumitest.Mocks, typ string) []pulumitest.Registration {
	var out []pulumitest.Registration
	for _, r := range m.Registrations() {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// dispatchSecretData is the <hostname>-dispatch Secret's stringData as the mocks recorded it.
func dispatchSecretData(t *testing.T, m *pulumitest.Mocks) resource.PropertyMap {
	t.Helper()
	for _, registration := range m.Registrations() {
		if registration.Type == "kubernetes:core/v1:Secret" && registration.Name == "example-dispatch-dispatch" {
			return unwrappedProperty(registration.Inputs["stringData"]).ObjectValue()
		}
	}
	t.Fatal("no example-dispatch-dispatch Secret registered")
	return nil
}

func secretDataKeys(data resource.PropertyMap) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, string(key))
	}
	return keys
}

// Neither the broker token nor the App key is ever component input, so no Secret, StringData or
// registered input of the component holds either's material: the sentinels below are what a
// consumer's secret store would hold, and only references to them are inputs.
func TestDispatchHoldsNoBrokerTokenOrAppKeyMaterial(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY", "DISPATCH_SIGNIN_CLIENT_SECRET",
	}, secretDataKeys(dispatchSecretData(t, m)))
	for _, registration := range m.Registrations() {
		if registration.Type != "kubernetes:core/v1:Secret" {
			continue
		}
		for _, s := range allStrings(resource.NewProperty(registration.Inputs)) {
			require.NotContains(t, s, "BEGIN", "%s holds PEM material", registration.Name)
			require.NotContains(t, s, "agent-secrets-token-value", "%s holds the broker token", registration.Name)
		}
		require.NotContains(t, secretDataKeys(unwrappedProperty(registration.Inputs["stringData"]).ObjectValue()), "DISPATCH_AGENT_SECRETS_TOKEN")
		require.NotContains(t, secretDataKeys(unwrappedProperty(registration.Inputs["stringData"]).ObjectValue()), "DISPATCH_APP_PEM_B64")
	}
}

// Unset, neither pair leaves a trace: the Secret keeps exactly its four keys, the NATS seed's is
// the one accessor, and TestDispatchPodMatchesGolden pins the pod template byte-identical to before
// the inputs existed.
func TestDispatchSecretCarriesNoBrokerOrAppKeyUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY", "DISPATCH_SIGNIN_CLIENT_SECRET",
	}, secretDataKeys(dispatchSecretData(t, m)))
	require.Len(t, registrationsOf(m, "gcp:secretmanager/secretIamMember:SecretIamMember"), 1, "only the NATS seed's accessor exists")
}

// The broker relay stores no secret: Dispatch's token is projected by the kubelet, so with it set
// the pods' principal gains no Secret Manager grant and the component no SecretProviderClass beyond
// the NATS seed's, and the Deployment waits on no grant beyond that seed's.
func TestDispatchStoresNoBrokerTokenWhenSet(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	members := registrationsOf(m, "gcp:secretmanager/secretIamMember:SecretIamMember")
	require.Len(t, members, 1)
	require.Equal(t, "example-dispatch-nats-seed-accessor", members[0].Name)
	var classes []string
	for _, r := range m.Registrations() {
		if r.Inputs["kind"].IsString() && r.Inputs["kind"].StringValue() == "SecretProviderClass" {
			classes = append(classes, r.Name)
		}
	}
	require.Equal(t, []string{"example-dispatch-nats-seed"}, classes)
}

// Both pairs set, the pod template gains exactly the broker env pair (DISPATCH_AGENT_SECRETS_URL
// and DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE), the app env triple (DISPATCH_APP_CLIENT_ID and
// DISPATCH_APP_ID plain, DISPATCH_APP_PEM_B64 from the consumer's Secret), and the projected
// ServiceAccount token's own volume, for the broker's audience and an hour's lifetime, mounted
// into envoy-dispatch alone.
func TestDispatchPodWithBrokerAndAppMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", "dispatch-pod-with-broker-and-app.golden")
}

// With a broker configured, the app's container carries what Dispatch boots on: Dispatch requires
// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE whenever DISPATCH_AGENT_SECRETS_URL is set, refuses to
// start while either removed shared-bearer setting (DISPATCH_AGENT_SECRETS_TOKEN or its _FILE) is
// set, and presents the file's contents to the broker as its own Kubernetes ServiceAccount token.
// So the file it names lies on a projected serviceAccountToken volume mounted into envoy-dispatch,
// which the kubelet rotates, never on a volume holding a secret every reader shares.
func TestDispatchWithABrokerPresentsItsOwnServiceAccountToken(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	template := deploymentPodTemplate(t, m, "example-dispatch")
	container := podContainer(t, template, "envoy-dispatch")
	env := containerEnv(t, container)
	require.Contains(t, env, "DISPATCH_AGENT_SECRETS_URL")
	for _, removed := range []string{"DISPATCH_AGENT_SECRETS_TOKEN", "DISPATCH_AGENT_SECRETS_TOKEN_FILE"} {
		require.NotContains(t, env, removed, "Dispatch refuses to start while %s is set", removed)
	}
	path, ok := env["DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"].(string)
	require.True(t, ok, "Dispatch refuses to start with DISPATCH_AGENT_SECRETS_URL set and no DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE")
	var volumeName string
	for _, item := range container["volumeMounts"].([]any) {
		mount := item.(map[string]any)
		mountPath := strings.TrimSuffix(mount["mountPath"].(string), "/") + "/"
		if strings.HasPrefix(path, mountPath) {
			volumeName = mount["name"].(string)
		}
	}
	require.NotEmpty(t, volumeName, "no volume mounted into envoy-dispatch holds %s", path)
	var projected bool
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		volume := item.(map[string]any)
		if volume["name"] != volumeName {
			continue
		}
		sources, _ := volume["projected"].(map[string]any)["sources"].([]any)
		for _, source := range sources {
			if _, ok := source.(map[string]any)["serviceAccountToken"]; ok {
				projected = true
			}
		}
	}
	require.True(t, projected, "%s lies on volume %s, which projects no ServiceAccount token", path, volumeName)
}

// With both groups set, the listener's and the front door's pods are exactly as without them: the
// broker relay and the GitHub App reach the app's envoy-dispatch container alone.
func TestDispatchBrokerAndAppLeaveTheOtherWorkloadsUnchanged(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
}

// A refused Azure configuration is refused by the resolver, before the component registers, so
// nothing beyond the test's providers exists.
func TestDispatchRefusesAnAzureMisconfigurationBeforeRegistering(t *testing.T) {
	args := dispatchArgs()
	setAzureEndpoint(args)
	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: azureEmbedEndpoint needs one credential")
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
}

// Unset, the Azure inputs leave no trace: no key SecretProviderClass and no accessor beyond the
// seed's; TestDispatchPodMatchesGolden pins the pod template byte-identical to before they existed.
func TestDispatchRegistersNoAzureResourceUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, r := range m.Registrations() {
		require.NotContains(t, r.Name, "azure")
	}
}

// azureKeyAccessorURN is the key form's accessor, which the app's Deployment waits for.
const azureKeyAccessorURN = "urn:pulumi:s::p::" + dispatchType + "$gcp:secretmanager/secretIamMember:SecretIamMember::example-dispatch-azure-embed-key-accessor"

// The key form adds exactly one per-secret accessor for the pod's principal, on the key's secret
// alone, and the app's Deployment waits for it; the keyless form adds none. The key itself is never
// an input: no Secret the component writes gains a key.
func TestDispatchGrantsTheAzureKeyAccessorOnlyWithTheKeyForm(t *testing.T) {
	args := dispatchArgs()
	setAzureKey(args)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	var members []string
	for _, r := range registrationsOf(m, "gcp:secretmanager/secretIamMember:SecretIamMember") {
		members = append(members, r.Name)
	}
	require.ElementsMatch(t, []string{"example-dispatch-nats-seed-accessor", "example-dispatch-azure-embed-key-accessor"}, members)
	inputs := m.Named(t, "gcp:secretmanager/secretIamMember:SecretIamMember", "example-dispatch-azure-embed-key-accessor").Inputs.Mappable()
	require.Equal(t, azureEmbedKeySecret, inputs["secretId"])
	require.Equal(t, "roles/secretmanager.secretAccessor", inputs["role"])
	require.Equal(t, "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/example-project.svc.id.goog/subject/ns/dispatch/sa/example-dispatch", inputs["member"])
	require.Contains(t, m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").RPC.GetDependencies(), azureKeyAccessorURN)
	for _, other := range []string{"example-dispatch-listener", "example-dispatch-tailnet"} {
		require.NotContains(t, m.Named(t, "kubernetes:apps/v1:Deployment", other).RPC.GetDependencies(), azureKeyAccessorURN,
			"%s mounts no Azure key", other)
	}
	require.ElementsMatch(t, []string{
		"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY", "DISPATCH_SIGNIN_CLIENT_SECRET",
	}, secretDataKeys(dispatchSecretData(t, m)))

	args = dispatchArgs()
	setAzureFederation(args)
	m, err = runDispatch(t, args, true)
	require.NoError(t, err)
	require.Len(t, registrationsOf(m, "gcp:secretmanager/secretIamMember:SecretIamMember"), 1, "only the NATS seed's accessor exists")
}

// The key's SecretProviderClass is its own: provider gke, the secret's path with /versions/latest,
// written to the file key, which the pod mounts at /var/run/secrets/azure-embed. The keyless form
// registers none.
func TestDispatchRegistersTheAzureKeySecretProviderClassOnlyWithTheKeyForm(t *testing.T) {
	args := dispatchArgs()
	setAzureKey(args)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	class := m.Named(t, "kubernetes:secrets-store.csi.x-k8s.io/v1:SecretProviderClass", "example-dispatch-azure-embed-key").Inputs.Mappable()
	require.Equal(t, "example-dispatch-azure-embed-key", class["metadata"].(map[string]any)["name"])
	spec := class["spec"].(map[string]any)
	require.Equal(t, "gke", spec["provider"])
	require.Equal(t, "- resourceName: "+azureEmbedKeySecret+"/versions/latest\n  path: key\n", spec["parameters"].(map[string]any)["secrets"])

	args = dispatchArgs()
	setAzureFederation(args)
	m, err = runDispatch(t, args, true)
	require.NoError(t, err)
	for _, r := range m.Registrations() {
		require.NotEqual(t, "example-dispatch-azure-embed-key", r.Name)
	}
}

// The key form: the pod template gains exactly DISPATCH_EMBEDDER, the endpoint, the quota and
// DISPATCH_AZURE_EMBED_KEY_FILE, with the key's own CSI volume mounted into envoy-dispatch alone;
// no deployment is named, so Dispatch defaults it.
func TestDispatchPodWithAzureKeyMatchesGolden(t *testing.T) {
	args := dispatchArgs()
	setAzureKey(args)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", "dispatch-pod-with-azure-key.golden")
}

// The keyless form: the pod template gains DISPATCH_EMBEDDER, the endpoint, the quota, the
// deployment, the tenant and client, and DISPATCH_AZURE_FEDERATED_TOKEN_FILE naming a projected
// service-account token for api://AzureADTokenExchange, an hour long, mounted into envoy-dispatch
// alone.
func TestDispatchPodWithAzureFederationMatchesGolden(t *testing.T) {
	args := dispatchArgs()
	setAzureFederation(args)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", "dispatch-pod-with-azure-federation.golden")
}

// azureFederationVolume is the keyless form's projected volume in the app's pod template.
func azureFederationVolume(t *testing.T, template map[string]any) map[string]any {
	t.Helper()
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		if volume := item.(map[string]any); volume["name"] == "azure-federation" {
			return volume
		}
	}
	t.Fatal("no azure-federation volume")
	return nil
}

// azureFederationAudience gives the projected token its audience, for a federated credential that
// expects another than Entra's default.
func TestDispatchProjectsTheTokenForTheGivenAudience(t *testing.T) {
	args := dispatchArgs()
	setAzureFederation(args)
	args.AzureFederationAudience = pulumi.StringRef("api://example-audience")
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	volume := azureFederationVolume(t, deploymentPodTemplate(t, m, "example-dispatch"))
	require.Equal(t, map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{
		"audience": "api://example-audience", "expirationSeconds": 3600.0, "path": "token",
	}}}}, volume["projected"])
}

// Either form reaches the app's envoy-dispatch container alone: the listener's and the front
// door's pods are exactly as without it.
func TestDispatchAzureLeavesTheOtherWorkloadsUnchanged(t *testing.T) {
	for name, set := range map[string]func(*gcp.DispatchArgs){"key": setAzureKey, "federation": setAzureFederation} {
		t.Run(name, func(t *testing.T) {
			args := dispatchArgs()
			set(args)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
		})
	}
}

// appResources is a container's resources as the component renders them: equal requests and limits.
func appResources(cpu, memory string) map[string]any {
	values := map[string]any{"cpu": cpu, "memory": memory}
	return map[string]any{"requests": values, "limits": values}
}

// appCpu and appMemory size the app's envoy-dispatch container and nothing else: the app's pod
// template is the default's with that one container's resources changed, so cloud-sql-proxy keeps
// its 200m and 256Mi, and the front door's and the listener's pods match their goldens.
func TestDispatchAppResourcesReachEnvoyDispatchAlone(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	args := dispatchArgs()
	args.AppCpu = pulumi.StringRef("2")
	args.AppMemory = pulumi.StringRef("8Gi")
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	got := deploymentPodTemplate(t, m, "example-dispatch")
	require.Equal(t, appResources("2", "8Gi"), podContainer(t, got, "envoy-dispatch")["resources"])
	want := deploymentPodTemplate(t, defaults, "example-dispatch")
	podContainer(t, want, "envoy-dispatch")["resources"] = appResources("2", "8Gi")
	require.Equal(t, want, got)
	proxy := got["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any)
	require.Equal(t, "cloud-sql-proxy", proxy["name"])
	require.Equal(t, appResources("200m", "256Mi"), proxy["resources"])
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
}

// A size Autopilot would raise or reject is refused by the resolver, before the component
// registers, so nothing beyond the test's providers exists.
func TestDispatchRefusesAppResourcesBeforeRegistering(t *testing.T) {
	args := dispatchArgs()
	args.AppCpu = pulumi.StringRef("100m")
	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, `envoy-infra:gcp:Dispatch example-dispatch: appCpu "100m" must be at least 300m`)
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
}

const (
	podDisruptionBudgetType = "kubernetes:policy/v1:PodDisruptionBudget"
	priorityClassType       = "kubernetes:scheduling.k8s.io/v1:PriorityClass"
)

// Unset, appPriorityClassName and appPdbMinAvailable leave the component as it was before they
// existed: no PodDisruptionBudget, no PriorityClass read, and an app pod naming no priority class
// (TestDispatchPodMatchesGolden pins the whole template).
func TestDispatchRegistersNoBudgetOrPriorityClassUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.Zero(t, countRegistrations(m, podDisruptionBudgetType))
	require.Zero(t, countRegistrations(m, priorityClassType))
	require.NotContains(t, deploymentPodTemplate(t, m, "example-dispatch")["spec"], "priorityClassName")
}

// requireKubernetesChild asserts that r is a child of the component on its own Kubernetes
// provider, waiting on the cluster, as AGENTS.md has every Kubernetes child do.
func requireKubernetesChild(t *testing.T, r pulumitest.Registration) {
	t.Helper()
	require.Equal(t, "urn:pulumi:s::p::"+dispatchType+"::example-dispatch", r.RPC.GetParent())
	require.Contains(t, r.RPC.GetProvider(), "pulumi:providers:kubernetes::example-dispatch-kubernetes::")
	require.Contains(t, r.RPC.GetDependencies(), "urn:pulumi:s::p::"+dispatchType+"$gcp:container/cluster:Cluster::example-dispatch")
}

// appPriorityClassName reaches the app's pod and nothing else: its template is the default's with
// priorityClassName added, and the front door's and the listener's pods match their goldens. The
// component creates no PriorityClass; it reads the named one, and the app Deployment waits on
// that read, so a class the cluster lacks fails the update before any pod is replaced.
func TestDispatchAppPriorityClassReachesTheAppPodAlone(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, class := range []string{"example-priority", "example.priority"} {
		t.Run(class, func(t *testing.T) {
			args := dispatchArgs()
			args.AppPriorityClassName = pulumi.StringRef(class)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			want := deploymentPodTemplate(t, defaults, "example-dispatch")
			want["spec"].(map[string]any)["priorityClassName"] = class
			require.Equal(t, want, deploymentPodTemplate(t, m, "example-dispatch"))
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")

			read := m.Named(t, priorityClassType, "example-dispatch-priority-class")
			require.Equal(t, class, read.ID)
			require.Nil(t, read.RPC, "the PriorityClass is read, never registered")
			require.Equal(t, "urn:pulumi:s::p::"+dispatchType+"::example-dispatch", read.ReadRPC.GetParent())
			require.Contains(t, read.ReadRPC.GetProvider(), "pulumi:providers:kubernetes::example-dispatch-kubernetes::")
			require.Contains(t, read.ReadRPC.GetDependencies(), "urn:pulumi:s::p::"+dispatchType+"$gcp:container/cluster:Cluster::example-dispatch")
			deps := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").RPC.GetDependencies()
			require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$"+priorityClassType+"::example-dispatch-priority-class")
			require.ElementsMatch(t, append(defaults.Types(), priorityClassType), m.Types())
		})
	}
}

// appPdbMinAvailable registers one PodDisruptionBudget, named for the app, in its namespace,
// selecting the app's pods by the label the app Deployment's own selector matches, and no other
// workload's, with Kubernetes' default unhealthyPodEvictionPolicy. It is a Kubernetes child of the
// component like every other, and the app's pod template is unchanged.
func TestDispatchAppPdbCoversTheAppPods(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, row := range []struct {
		replicas, minAvailable int
		namespace              string
	}{{1, 1, ""}, {3, 2, ""}, {3, 3, ""}, {1, 1, "example-namespace"}} {
		t.Run(fmt.Sprintf("%d of %d in %q", row.minAvailable, row.replicas, row.namespace), func(t *testing.T) {
			args := dispatchArgs()
			args.Replicas = pulumi.IntRef(row.replicas)
			args.AppPdbMinAvailable = pulumi.IntRef(row.minAvailable)
			namespace := "dispatch"
			if row.namespace != "" {
				args.Namespace = pulumi.StringRef(row.namespace)
				namespace = row.namespace
			}
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			registration := m.Named(t, podDisruptionBudgetType, "example-dispatch")
			requireKubernetesChild(t, registration)
			budget := registration.Inputs.Mappable()
			require.Equal(t, map[string]any{"name": "example-dispatch", "namespace": namespace}, budget["metadata"])
			require.Equal(t, map[string]any{
				"minAvailable": float64(row.minAvailable),
				"selector":     map[string]any{"matchLabels": map[string]any{"app": "example-dispatch"}},
			}, budget["spec"])
			deploymentSpec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").Inputs.Mappable()["spec"].(map[string]any)
			require.Equal(t, deploymentSpec["selector"], budget["spec"].(map[string]any)["selector"])
			if row.namespace == "" {
				require.Equal(t, deploymentPodTemplate(t, defaults, "example-dispatch"), deploymentPodTemplate(t, m, "example-dispatch"))
				require.ElementsMatch(t, append(defaults.Types(), podDisruptionBudgetType), m.Types())
			}
		})
	}
}

// A budget the app's replica count cannot meet is refused by the resolver, before the component
// registers, so nothing beyond the test's providers exists.
func TestDispatchRefusesAnAppPdbBeforeRegistering(t *testing.T) {
	args := dispatchArgs()
	args.AppPdbMinAvailable = pulumi.IntRef(2)
	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, `envoy-infra:gcp:Dispatch example-dispatch: appPdbMinAvailable 2 must be from 1 to replicas (1)`)
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
}
