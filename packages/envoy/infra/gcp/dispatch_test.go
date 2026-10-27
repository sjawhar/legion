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
	// dispatchToolsImage is frontDoorToolsImage: the envoy image at a digest of its own, as a
	// consumer pins it, so the app's image moving leaves it where it is.
	dispatchToolsImage = "example.invalid/envoy@sha256:99887766554433221100ffeeddccbbaa99887766554433221100ffeeddccbbaa"
)

func dispatchArgs() *gcp.DispatchArgs {
	return &gcp.DispatchArgs{
		Region:              "us-central1",
		Hostname:            "example-dispatch",
		TailnetDnsName:      "example-tailnet.ts.net",
		Tags:                []string{"tag:example-dispatch"},
		TailnetAuthKey:      pulumi.String("tskey-auth-example-0123456789"),
		TailnetService:      pulumi.String("svc:example-dispatch"),
		EnvoyImage:          dispatchEnvoyImage,
		TailscaleImage:      dispatchTailscaleImage,
		CloudSqlProxyImage:  dispatchProxyImage,
		FrontDoorToolsImage: dispatchToolsImage,
		NatsUrl:             pulumi.String("nats://example-hub.example-tailnet.ts.net:4222"),
		NatsSeedSecret:      pulumi.String("projects/example-project/secrets/example-nats-seed"),
		SignInClientId:      "example-client-id",
		SignInClientSecret:  pulumi.String("example-client-secret"),
		SignInDomain:        "example.com",
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
	require.Equal(t, "db-custom-4-15360", settings.DatabaseTier)
	require.True(t, settings.DeletionProtection)
	require.Equal(t, "dispatch", settings.Namespace)
	require.Equal(t, 1, settings.Replicas)
	require.Equal(t, "Recreate", settings.StrategyType)
	require.Nil(t, settings.RollingUpdate, "one replica is recreated, never rolled")
}

// Unset, appCpu and appMemory are the 1 CPU and 4Gi the app container always had, as requests and
// limits of envoy-dispatch, so a deployment that sets neither keeps its app container's size
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

// proxyContainer is the app pod template's cloud-sql-proxy native sidecar.
func proxyContainer(t *testing.T, template map[string]any) map[string]any {
	t.Helper()
	proxy := template["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any)
	require.Equal(t, "cloud-sql-proxy", proxy["name"])
	return proxy
}

// Unset, proxyCpu and proxyMemory size the cloud-sql-proxy sidecar for one server pod's pools,
// 1 CPU and 2Gi as its request and its limit, the sizes the proxy's own Kubernetes sidecar
// example requests: every database call of the pod, up to 32 connections, passes through its TLS.
func TestDispatchProxyContainerDefaultsToOneCPUAnd2Gi(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.Equal(t, appResources("1", "2Gi"), proxyContainer(t, deploymentPodTemplate(t, m, "example-dispatch"))["resources"])
}

// The quantity forms Kubernetes reads are taken as written, up to Autopilot's bounds for the pod,
// judged on the two containers' sum with the proxy's resolved size. With proxyCpu 200m and
// proxyMemory 256Mi, 300m and 256Mi make the 500m and 512Mi floor, 29800m and 112384Mi the 30 CPU
// and 110Gi ceiling, and 800m and 6400Mi exactly 6.5 GiB per CPU. With the proxy's default 1 CPU
// and 2Gi, 29 and 108Gi make the ceiling, 1 and 11Gi exactly 6.5 GiB per CPU, and 100m and 256Mi
// sit above the floor; and the proxy takes any size that keeps the pod inside them. A decimal
// exponent inside e-9 to e18 is valued exactly, its mantissa included.
func TestDispatchAcceptsAppResourcesWithinAutopilotsBounds(t *testing.T) {
	for _, row := range []struct{ cpu, memory, proxyCPU, proxyMemory string }{
		{"300m", "512Mi", "200m", "256Mi"},
		{"300m", "256Mi", "200m", "256Mi"},
		{"1.5", "6Gi", "200m", "256Mi"},
		{"2", "8589934592", "200m", "256Mi"},
		{"4", "16G", "200m", "256Mi"},
		{"29800m", "112384Mi", "200m", "256Mi"},
		{"2e0", "0.5e10", "200m", "256Mi"},
		{"800m", "6400Mi", "200m", "256Mi"},
		{"1", "4500000k", "200m", "256Mi"},
		{"1000000000e-9", "0.000000004e18", "200m", "256Mi"},
		{"29", "108Gi", "", ""},
		{"1", "11Gi", "", ""},
		{"100m", "256Mi", "", ""},
		{"1", "4Gi", "250m", "1Gi"},
		{"1", "4Gi", "4", "8Gi"},
		{"1", "4Gi", "2500m", "1536Mi"},
	} {
		t.Run(row.cpu+"/"+row.memory+"/"+row.proxyCPU+"/"+row.proxyMemory, func(t *testing.T) {
			args := dispatchArgs()
			args.AppCpu = pulumi.StringRef(row.cpu)
			args.AppMemory = pulumi.StringRef(row.memory)
			if row.proxyCPU != "" {
				args.ProxyCpu = pulumi.StringRef(row.proxyCPU)
				args.ProxyMemory = pulumi.StringRef(row.proxyMemory)
			}
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
		{name: "an unpinned front-door tools image", edit: func(a *gcp.DispatchArgs) { a.FrontDoorToolsImage = "example.invalid/envoy:latest" }, err: []string{"frontDoorToolsImage", "@sha256:"}},
		{name: "no front-door tools image", edit: func(a *gcp.DispatchArgs) { a.FrontDoorToolsImage = "" }, err: []string{"frontDoorToolsImage", "pinned by digest"}},
		{name: "no region", edit: func(a *gcp.DispatchArgs) { a.Region = "" }, err: []string{"region", "required"}},
		{name: "no sign-in client ID", edit: func(a *gcp.DispatchArgs) { a.SignInClientId = "" }, err: []string{"signInClientId", "required"}},
		{name: "an invalid sign-in domain", edit: func(a *gcp.DispatchArgs) { a.SignInDomain = "Example.COM" }, err: []string{"signInDomain", "lowercase DNS name"}},
		{name: "an empty database version", edit: func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef("") }, err: []string{"databaseVersion", "empty"}},
		{name: "an empty database tier", edit: func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef("") }, err: []string{"databaseTier is empty", "leave it unset for db-custom-4-15360"}},
		{name: "an invalid namespace", edit: func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef("Dispatch") }, err: []string{"namespace", "DNS label"}},
		{name: "an invalid allowed namespace", edit: func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", "Legion"} }, err: []string{"allowedNamespaces[1]", "DNS label"}},
		{name: "a namespace longer than a DNS label", edit: func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef(strings.Repeat("a", 64)) }, err: []string{"namespace", "DNS label"}},
		{name: "an allowed namespace longer than a DNS label", edit: func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{strings.Repeat("a", 64)} }, err: []string{"allowedNamespaces[0]", "DNS label"}},
		{name: "no tailnet auth key", edit: func(a *gcp.DispatchArgs) { a.TailnetAuthKey = nil }, err: []string{"tailnetAuthKey", "required"}},
		{name: "no tailnet service", edit: func(a *gcp.DispatchArgs) { a.TailnetService = nil }, err: []string{"tailnetService", "required"}},
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
		{name: "fewer than one front-door replica", edit: func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(0) }, err: []string{"frontDoorReplicas 0", "at least 1"}},
		{name: "fewer than one listener replica", edit: func(a *gcp.DispatchArgs) { a.ListenerReplicas = pulumi.IntRef(-1) }, err: []string{"listenerReplicas -1", "at least 1"}},
		{name: "a pprof port of 0", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(0) }, err: []string{"pprofPort 0", "1-65535", "leave it unset"}},
		{name: "a negative pprof port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(-1) }, err: []string{"pprofPort -1", "1-65535"}},
		{name: "a pprof port past 65535", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(65536) }, err: []string{"pprofPort 65536", "1-65535"}},
		{name: "a pprof port on envoy-dispatch's own port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(8766) }, err: []string{"pprofPort 8766", "envoy-dispatch"}},
		{name: "a pprof port on cloud-sql-proxy's port", edit: func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(5432) }, err: []string{"pprofPort 5432", "cloud-sql-proxy"}},
		{name: "an empty priority class", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("") }, err: []string{"priorityClassName is empty", "leave it unset for no priority class"}},
		{name: "a priority class that is no DNS subdomain", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("Example_Priority") }, err: []string{`priorityClassName "Example_Priority"`, "DNS subdomain"}},
		{name: "a priority class with trailing whitespace", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("example-priority ") }, err: []string{`priorityClassName "example-priority "`, "DNS subdomain"}},
		{name: "a priority class longer than 253 characters", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef(strings.Repeat("a.", 127) + "a") }, err: []string{"priorityClassName", "DNS subdomain"}},
		{name: "a system cluster-critical priority class", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("system-cluster-critical") }, err: []string{`priorityClassName "system-cluster-critical"`, "reserves for its own system components"}},
		{name: "a system node-critical priority class", edit: func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("system-node-critical") }, err: []string{`priorityClassName "system-node-critical"`, "reserves for its own system components"}},
		{name: "a disruption budget of no pod", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(0) }, err: []string{"appPdbMinAvailable 0", "from 1 to replicas (1)", "leave it unset for no PodDisruptionBudget"}},
		{name: "a negative disruption budget", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(-1) }, err: []string{"appPdbMinAvailable -1", "from 1 to replicas (1)"}},
		{name: "a disruption budget above the default replica count", edit: func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(2) }, err: []string{"appPdbMinAvailable 2", "from 1 to replicas (1)"}},
		{name: "a disruption budget above the replica count", edit: func(a *gcp.DispatchArgs) {
			a.Replicas = pulumi.IntRef(3)
			a.AppPdbMinAvailable = pulumi.IntRef(4)
		}, err: []string{"appPdbMinAvailable 4", "from 1 to replicas (3)"}},
		{name: "a front-door budget of no pod", edit: func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(0) }, err: []string{"frontDoorPdbMinAvailable 0", "from 1 to frontDoorReplicas (2)", "leave it unset for no PodDisruptionBudget"}},
		{name: "a front-door budget above the default front-door count", edit: func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(3) }, err: []string{"frontDoorPdbMinAvailable 3", "from 1 to frontDoorReplicas (2)"}},
		{name: "a front-door budget above the front-door count", edit: func(a *gcp.DispatchArgs) {
			a.FrontDoorReplicas = pulumi.IntRef(1)
			a.FrontDoorPdbMinAvailable = pulumi.IntRef(2)
		}, err: []string{"frontDoorPdbMinAvailable 2", "from 1 to frontDoorReplicas (1)"}},
		{name: "a front-door budget measured against the app's count", edit: func(a *gcp.DispatchArgs) {
			a.Replicas = pulumi.IntRef(5)
			a.FrontDoorPdbMinAvailable = pulumi.IntRef(4)
		}, err: []string{"frontDoorPdbMinAvailable 4", "from 1 to frontDoorReplicas (2)"}},
		{name: "a listener budget of no pod", edit: func(a *gcp.DispatchArgs) { a.ListenerPdbMinAvailable = pulumi.IntRef(0) }, err: []string{"listenerPdbMinAvailable 0", "from 1 to listenerReplicas (1)", "leave it unset for no PodDisruptionBudget"}},
		{name: "a listener budget above the default listener count", edit: func(a *gcp.DispatchArgs) { a.ListenerPdbMinAvailable = pulumi.IntRef(2) }, err: []string{"listenerPdbMinAvailable 2", "from 1 to listenerReplicas (1)"}},
		{name: "a listener budget above the listener count", edit: func(a *gcp.DispatchArgs) {
			a.ListenerReplicas = pulumi.IntRef(3)
			a.ListenerPdbMinAvailable = pulumi.IntRef(4)
		}, err: []string{"listenerPdbMinAvailable 4", "from 1 to listenerReplicas (3)"}},
		{name: "a lowercase database availability type", edit: func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef("regional") }, err: []string{`databaseAvailabilityType "regional"`, "ZONAL or REGIONAL"}},
		{name: "an empty database availability type", edit: func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef("") }, err: []string{`databaseAvailabilityType ""`, "ZONAL or REGIONAL", "leave it unset for ZONAL"}},
		{name: "an unknown database availability type", edit: func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef("MULTI_REGIONAL") }, err: []string{`databaseAvailabilityType "MULTI_REGIONAL"`, "ZONAL or REGIONAL"}},
		{name: "an app CPU that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("two") }, err: []string{`appCpu "two"`, "Kubernetes quantity"}},
		{name: "an app CPU with a memory suffix in the wrong case", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("1gi") }, err: []string{`appCpu "1gi"`, "Kubernetes quantity"}},
		{name: "an app memory that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("4 GiB") }, err: []string{`appMemory "4 GiB"`, "Kubernetes quantity"}},
		{name: "an empty app CPU", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("") }, err: []string{"appCpu is empty", "leave it unset for 1"}},
		{name: "an empty app memory", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("") }, err: []string{"appMemory is empty", "leave it unset for 4Gi"}},
		{name: "a negative app CPU", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("-1") }, err: []string{`appCpu "-1"`, "more than 0"}},
		{name: "a zero app memory", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("0") }, err: []string{`appMemory "0"`, "more than 0"}},
		{name: "an app CPU below Autopilot's pod floor with the proxy's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("250m")
			a.ProxyCpu = pulumi.StringRef("200m")
		}, err: []string{`appCpu "250m" and proxyCpu "200m"`, "at least 500m together", "Autopilot raises"}},
		{name: "an app memory below Autopilot's pod floor with the proxy's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("500m")
			a.AppMemory = pulumi.StringRef("128Mi")
			a.ProxyMemory = pulumi.StringRef("256Mi")
		}, err: []string{`appMemory "128Mi" and proxyMemory "256Mi"`, "at least 512Mi together", "Autopilot raises"}},
		{name: "an app CPU above Autopilot's pod ceiling", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("30")
			a.AppMemory = pulumi.StringRef("64Gi")
		}, err: []string{`appCpu "30" and proxyCpu "1"`, "at most 30 CPU together", "rejects"}},
		// 29.5 CPU fit beside the 200m the proxy had; beside its 1 CPU the pod requests 30.5.
		{name: "an app CPU above Autopilot's pod ceiling only with the proxy's default", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("29.5")
			a.AppMemory = pulumi.StringRef("64Gi")
		}, err: []string{`appCpu "29.5" and proxyCpu "1"`, "at most 30 CPU together", "rejects"}},
		{name: "an app memory above Autopilot's pod ceiling", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("20")
			a.AppMemory = pulumi.StringRef("110Gi")
		}, err: []string{`appMemory "110Gi" and proxyMemory "2Gi"`, "at most 110Gi together", "rejects"}},
		// 109Gi fit beside the 256Mi the proxy had; beside its 2Gi the pod requests 111Gi.
		{name: "an app memory above Autopilot's pod ceiling only with the proxy's default", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("20")
			a.AppMemory = pulumi.StringRef("109Gi")
		}, err: []string{`appMemory "109Gi" and proxyMemory "2Gi"`, "at most 110Gi together", "rejects"}},
		{name: "app CPU raised without memory to match", edit: func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("8") }, err: []string{`appCpu "8", appMemory "4Gi", proxyCpu "1" and proxyMemory "2Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "app memory raised without CPU to match", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("32Gi") }, err: []string{`appCpu "1", appMemory "32Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "app memory just above 6.5 GiB per CPU with the proxy's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("800m")
			a.AppMemory = pulumi.StringRef("6401Mi")
			a.ProxyCpu = pulumi.StringRef("200m")
			a.ProxyMemory = pulumi.StringRef("256Mi")
		}, err: []string{`appCpu "800m", appMemory "6401Mi", proxyCpu "200m" and proxyMemory "256Mi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		// 1 CPU and 11Gi were refused beside the 200m and 256Mi the proxy had (9.4 GiB per CPU);
		// beside its 1 CPU and 2Gi the pod is 2 CPU and 13Gi, exactly 6.5, and 11265Mi tips it over.
		{name: "app memory just above 6.5 GiB per CPU with the proxy's default", edit: func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("11265Mi") }, err: []string{`appCpu "1", appMemory "11265Mi", proxyCpu "1" and proxyMemory "2Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
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
		{name: "a proxy CPU that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("one") }, err: []string{`proxyCpu "one"`, "Kubernetes quantity"}},
		{name: "a proxy memory that is not a quantity", edit: func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("2 GiB") }, err: []string{`proxyMemory "2 GiB"`, "Kubernetes quantity"}},
		{name: "a proxy CPU with trailing whitespace", edit: func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("1 ") }, err: []string{`proxyCpu "1 "`, "Kubernetes quantity"}},
		{name: "a proxy memory with an exponent outside the suffixes' range", edit: func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("2e19") }, err: []string{`proxyMemory "2e19"`, "decimal exponent outside e-9 to e18"}},
		{name: "an empty proxy CPU", edit: func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("") }, err: []string{"proxyCpu is empty", "leave it unset for 1"}},
		{name: "an empty proxy memory", edit: func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("") }, err: []string{"proxyMemory is empty", "leave it unset for 2Gi"}},
		// A negative proxy size beside a large app size would pass the pod's floor on the sum.
		{name: "a negative proxy CPU", edit: func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("-100m") }, err: []string{`proxyCpu "-100m"`, "more than 0"}},
		{name: "a zero proxy memory", edit: func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("0Mi") }, err: []string{`proxyMemory "0Mi"`, "more than 0"}},
		{name: "a proxy CPU below Autopilot's pod floor with the app's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("300m")
			a.AppMemory = pulumi.StringRef("512Mi")
			a.ProxyCpu = pulumi.StringRef("100m")
			a.ProxyMemory = pulumi.StringRef("256Mi")
		}, err: []string{`appCpu "300m" and proxyCpu "100m"`, "at least 500m together", "Autopilot raises"}},
		{name: "a proxy memory below Autopilot's pod floor with the app's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("300m")
			a.AppMemory = pulumi.StringRef("256Mi")
			a.ProxyCpu = pulumi.StringRef("200m")
			a.ProxyMemory = pulumi.StringRef("255Mi")
		}, err: []string{`appMemory "256Mi" and proxyMemory "255Mi"`, "at least 512Mi together", "Autopilot raises"}},
		{name: "a proxy CPU above Autopilot's pod ceiling with the app's", edit: func(a *gcp.DispatchArgs) {
			a.ProxyCpu = pulumi.StringRef("29001m")
			a.ProxyMemory = pulumi.StringRef("60Gi")
		}, err: []string{`appCpu "1" and proxyCpu "29001m"`, "at most 30 CPU together", "rejects"}},
		{name: "a proxy memory above Autopilot's pod ceiling with the app's", edit: func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("10")
			a.ProxyCpu = pulumi.StringRef("10")
			a.ProxyMemory = pulumi.StringRef("107Gi")
		}, err: []string{`appMemory "4Gi" and proxyMemory "107Gi"`, "at most 110Gi together", "rejects"}},
		{name: "proxy CPU raised without memory to match", edit: func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("8") }, err: []string{`appCpu "1", appMemory "4Gi", proxyCpu "8" and proxyMemory "2Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{name: "proxy memory raised without CPU to match", edit: func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("10Gi") }, err: []string{`appCpu "1", appMemory "4Gi", proxyCpu "1" and proxyMemory "10Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
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
		{"a seed in the front-door tools image", "frontDoorToolsImage", func(a *gcp.DispatchArgs) { a.FrontDoorToolsImage = "example.invalid/" + seed + digest }},
		{"a seed as the sign-in client ID", "signInClientId", func(a *gcp.DispatchArgs) { a.SignInClientId = seed }},
		{"a seed inside the sign-in domain", "signInDomain", func(a *gcp.DispatchArgs) { a.SignInDomain = lower + ".example.com" }},
		{"a seed in the database version", "databaseVersion", func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef(seed) }},
		{"a seed in the database tier", "databaseTier", func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef(seed) }},
		{"a seed as the database availability type", "databaseAvailabilityType", func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef(seed) }},
		{"a seed as the app CPU", "appCpu", func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef(seed) }},
		{"a seed as the app memory", "appMemory", func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef(seed) }},
		{"a seed as the proxy CPU", "proxyCpu", func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef(seed) }},
		{"a lowercase seed as the proxy memory", "proxyMemory", func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef(lower) }},
		{"a seed as the priority class", "priorityClassName", func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef(lower) }},
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

// dispatchWorkloads are the component's three Deployments, each with the input that sets its
// replica count and the one that sets its PodDisruptionBudget.
var dispatchWorkloads = []struct {
	deployment       string
	replicas, budget func(*gcp.DispatchArgs, int)
}{
	{"example-dispatch",
		func(a *gcp.DispatchArgs, n int) { a.Replicas = pulumi.IntRef(n) },
		func(a *gcp.DispatchArgs, n int) { a.AppPdbMinAvailable = pulumi.IntRef(n) }},
	{"example-dispatch-tailnet",
		func(a *gcp.DispatchArgs, n int) { a.FrontDoorReplicas = pulumi.IntRef(n) },
		func(a *gcp.DispatchArgs, n int) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(n) }},
	{"example-dispatch-listener",
		func(a *gcp.DispatchArgs, n int) { a.ListenerReplicas = pulumi.IntRef(n) },
		func(a *gcp.DispatchArgs, n int) { a.ListenerPdbMinAvailable = pulumi.IntRef(n) }},
}

// deploymentSpec returns one Deployment's spec, with every output and secret wrapper off.
func deploymentSpec(t *testing.T, m *pulumitest.Mocks, deployment string) map[string]any {
	t.Helper()
	return normalizedProperty(m.Named(t, "kubernetes:apps/v1:Deployment", deployment).Inputs["spec"]).(map[string]any)
}

// requireStrategy fails unless deployment runs replicas pods, recreated at one replica, with no
// rollingUpdate block (Kubernetes refuses one on a Recreate Deployment), and rolled above it, one
// new pod up before any old one goes down.
func requireStrategy(t *testing.T, m *pulumitest.Mocks, deployment string, replicas int) {
	t.Helper()
	spec := deploymentSpec(t, m, deployment)
	require.EqualValues(t, replicas, spec["replicas"], deployment)
	if replicas == 1 {
		require.Equal(t, map[string]any{"type": "Recreate"}, spec["strategy"], deployment)
		return
	}
	require.Equal(t, map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"maxUnavailable": float64(0), "maxSurge": float64(1)},
	}, spec["strategy"], deployment)
}

// Each Deployment takes its strategy from its own replica count: recreated at one replica, since a
// roll runs the outgoing and incoming builds together, and rolled above it. By default the front
// door runs two pods and rolls, so no single pod carries the tailnet name; the app and the
// listener run one each and are recreated. One workload's count leaves the other two Deployments'
// specs as the defaults render them.
func TestDispatchEachWorkloadRecreatesAtOneReplicaAndRollsAbove(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	requireStrategy(t, defaults, "example-dispatch", 1)
	requireStrategy(t, defaults, "example-dispatch-tailnet", 2)
	requireStrategy(t, defaults, "example-dispatch-listener", 1)
	for _, workload := range dispatchWorkloads {
		for _, replicas := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s at %d", workload.deployment, replicas), func(t *testing.T) {
				args := dispatchArgs()
				workload.replicas(args, replicas)
				m, err := runDispatch(t, args, true)
				require.NoError(t, err)
				requireStrategy(t, m, workload.deployment, replicas)
				for _, other := range dispatchWorkloads {
					if other.deployment != workload.deployment {
						require.Equal(t, deploymentSpec(t, defaults, other.deployment), deploymentSpec(t, m, other.deployment), other.deployment)
					}
				}
			})
		}
	}
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
// takes that workload down, rather than an update. The same holds for the front door's SOCKS5
// Service, which the app and the listener dial by name; and a pod of the front door or the
// listener wearing the app's label would be sent user traffic by Service example-dispatch.
func TestDispatchDeploymentsKeepTheirNamesAndSelectors(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	for _, workload := range dispatchWorkloads {
		deployment := m.Named(t, "kubernetes:apps/v1:Deployment", workload.deployment)
		require.Equal(t, workload.deployment, deployment.Name)
		inputs := deployment.Inputs.Mappable()
		require.Equal(t, workload.deployment, inputs["metadata"].(map[string]any)["name"])
		labels := map[string]any{"app": workload.deployment}
		require.Equal(t, labels, deploymentSpec(t, m, workload.deployment)["selector"].(map[string]any)["matchLabels"])
		require.Equal(t, labels, deploymentPodTemplate(t, m, workload.deployment)["metadata"].(map[string]any)["labels"],
			"the pods the selector matches are the pods this Deployment creates")
	}
	socks5 := m.Named(t, "kubernetes:core/v1:Service", "example-dispatch-tailnet").Inputs.Mappable()
	require.Equal(t, "example-dispatch-tailnet", socks5["metadata"].(map[string]any)["name"])
	spec := socks5["spec"].(map[string]any)
	require.Equal(t, map[string]any{"app": "example-dispatch-tailnet"}, spec["selector"], "the SOCKS5 Service selects the front door's pods")
	require.Equal(t, []any{map[string]any{"port": float64(1055), "targetPort": float64(1055)}}, spec["ports"])
}

// The app's image reaching the front door's Deployment would roll the front door's pods on every
// release of the app. cert-publish runs on frontDoorToolsImage, an input of its own, so no
// front-door container takes the app's image. Registering the same inputs twice is the only form a
// mock engine can state "the front door stays up" in.
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

// frontDoorEnv indexes one front-door container's environment by name: a plain variable maps to
// its value, and one from a Secret or a field to its valueFrom.
func frontDoorEnv(t *testing.T, container map[string]any) map[string]any {
	t.Helper()
	env := map[string]any{}
	for _, item := range container["env"].([]any) {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		require.NotContains(t, env, name, "the environment names %s once", name)
		if value, ok := entry["value"]; ok {
			env[name] = value
		} else {
			env[name] = entry["valueFrom"]
		}
	}
	return env
}

// podNamed is a valueFrom that reads the pod's own name.
var podNamed = map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}

// By default the front door runs two pods and rolls them one new pod at a time with none missing,
// so no single pod carries the tailnet name or the SOCKS5 proxy; the SOCKS5 Service selects exactly
// the pods the Deployment creates, each of which runs the proxy.
func TestDispatchFrontDoorRunsTwoRollingPodsBehindItsSOCKS5Service(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	requireStrategy(t, m, "example-dispatch-tailnet", 2)
	template := deploymentPodTemplate(t, m, "example-dispatch-tailnet")
	spec := m.Named(t, "kubernetes:core/v1:Service", "example-dispatch-tailnet").Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, template["metadata"].(map[string]any)["labels"], spec["selector"], "the SOCKS5 Service selects the front door's pods")
	require.Equal(t, []any{map[string]any{"port": float64(1055), "targetPort": float64(1055)}}, spec["ports"])
	require.Equal(t, "0.0.0.0:1055", frontDoorEnv(t, podContainer(t, template, "tailscale"))["TS_SOCKS5_SERVER"],
		"every front-door pod runs the proxy, on every address")
}

// Each front-door pod is a tailnet node of its own hosting the Tailscale Service: its own
// ServiceAccount; tailscaled's state on the pod's emptyDir in a directory below the mount point,
// which tailscaled can make 0700 (the kubelet owns the mount point, and tailscaled falls back to
// an in-memory store when its chmod fails); a node named after the pod; no Kubernetes Secret
// holding node state; no advertisement by containerboot itself, the advertiser deciding it; the
// SOCKS5 proxy on every pod; a grace period for the preStop drain and containerboot's shutdown;
// readiness from the advertiser's ready file alone. Its containers are cert-seed (init), tailscale,
// cert-publish on frontDoorToolsImage and the advertiser, every one as the envoy image's uid with
// no privilege, together requesting 700m and 768Mi.
func TestDispatchFrontDoorPodHostsTheTailscaleService(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	template := deploymentPodTemplate(t, m, "example-dispatch-tailnet")
	pod := template["spec"].(map[string]any)
	require.Equal(t, "example-dispatch-tailnet", pod["serviceAccountName"])
	require.EqualValues(t, 60, pod["terminationGracePeriodSeconds"])
	require.Equal(t, map[string]any{
		"runAsUser": float64(999), "runAsGroup": float64(999), "runAsNonRoot": true,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}, pod["securityContext"])
	require.Equal(t, []any{
		map[string]any{"name": "state", "emptyDir": map[string]any{}},
		map[string]any{"name": "serve", "configMap": map[string]any{"name": "example-dispatch-serve"}},
		map[string]any{"name": "tls", "secret": map[string]any{"secretName": "example-dispatch-tailnet-tls", "optional": false}},
	}, pod["volumes"])

	containers := podProbes(t, m, "example-dispatch-tailnet")
	names := func(field string) []string {
		var out []string
		for _, item := range pod[field].([]any) {
			out = append(out, item.(map[string]any)["name"].(string))
		}
		return out
	}
	require.Equal(t, []string{"cert-seed"}, names("initContainers"))
	require.Equal(t, []string{"tailscale", "cert-publish", "advertiser"}, names("containers"))
	for name, want := range map[string]struct{ image, cpu, memory string }{
		"cert-seed":    {dispatchTailscaleImage, "100m", "128Mi"},
		"tailscale":    {dispatchTailscaleImage, "500m", "512Mi"},
		"cert-publish": {dispatchToolsImage, "100m", "128Mi"},
		"advertiser":   {dispatchTailscaleImage, "100m", "128Mi"},
	} {
		container := containers[name]
		require.Equal(t, want.image, container["image"], name)
		require.Equal(t, map[string]any{"requests": map[string]any{"cpu": want.cpu, "memory": want.memory}, "limits": map[string]any{"cpu": want.cpu, "memory": want.memory}},
			container["resources"], name)
		require.Equal(t, map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
			container["securityContext"], name)
		require.Contains(t, container["volumeMounts"], map[string]any{"name": "state", "mountPath": "/var/lib/frontdoor"}, "%s mounts the state volume", name)
		if name != "tailscale" {
			require.NotContains(t, container, "readinessProbe", "only the tailscale container's readiness counts")
		}
	}
	for _, name := range []string{"cert-seed", "cert-publish"} {
		require.Contains(t, containers[name]["volumeMounts"], map[string]any{"name": "tls", "mountPath": "/etc/frontdoor-tls", "readOnly": true}, name)
	}

	tailscale := containers["tailscale"]
	env := frontDoorEnv(t, tailscale)
	stateDir := env["TS_STATE_DIR"].(string)
	require.Equal(t, "/var/lib/frontdoor/tailscale", stateDir)
	require.Equal(t, stateDir, env["TS_LOGS_DIR"])
	for _, item := range tailscale["volumeMounts"].([]any) {
		require.NotEqual(t, stateDir, item.(map[string]any)["mountPath"], "TS_STATE_DIR is no mount point, which the pod's uid cannot chmod")
	}
	require.Equal(t, podNamed, env["TS_HOSTNAME"], "each pod registers under its own name")
	require.Equal(t, "", env["TS_KUBE_SECRET"], "no Kubernetes Secret holds a node's state")
	require.Equal(t, "false", env["TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT"], "containerboot advertises nothing by itself")
	require.Equal(t, "0.0.0.0:1055", env["TS_SOCKS5_SERVER"])
	require.Equal(t, "/etc/tailscale/serve.json", env["TS_SERVE_CONFIG"])
	require.Contains(t, tailscale["volumeMounts"], map[string]any{"name": "serve", "mountPath": "/etc/tailscale"})
	require.Equal(t, "/var/lib/frontdoor/tailscaled.sock", env["TS_SOCKET"])
	require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "example-dispatch-tailnet-auth-key", "key": "TS_AUTHKEY"}}, env["TS_AUTHKEY"])
	require.Equal(t, "--advertise-tags=tag:example-dispatch", env["TS_EXTRA_ARGS"])
	require.Equal(t, map[string]any{
		"exec":          map[string]any{"command": []any{"/bin/sh", "-c", "test -e /var/lib/frontdoor/service-ready"}},
		"periodSeconds": float64(5), "failureThreshold": float64(1),
	}, tailscale["readinessProbe"], "ready only while the advertiser's ready file exists")
	require.Contains(t, tailscale, "livenessProbe")
	require.Contains(t, tailscale, "startupProbe")
	preStop := tailscale["lifecycle"].(map[string]any)["preStop"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	require.Contains(t, preStop[2], "serve drain svc:example-dispatch", "the preStop hook drains the Service")
	require.Contains(t, preStop[2], "logout", "the preStop hook logs the node out")

	publish := containers["cert-publish"]
	require.Equal(t, map[string]any{"HOSTNAME": podNamed}, frontDoorEnv(t, publish), "the claim gate keys its entries on the pod's own name")
	script := publish["args"].([]any)[0].(string)
	for _, want := range []string{"legion.dev/certificate-claims", "legion.dev/front-door-nodes", "published a later certificate",
		"url=https://kubernetes.default.svc/api/v1/namespaces/dispatch/secrets/example-dispatch-tailnet-tls"} {
		require.Contains(t, script, want, "cert-publish runs the claim gate and the publishing loop")
	}
	advertiser := containers["advertiser"]["args"].([]any)[0].(string)
	require.Contains(t, advertiser, "certificate=example-dispatch.example-tailnet.ts.net\n", "the advertiser orders the Service name's certificate")
	require.Contains(t, advertiser, "flag_file=/var/lib/frontdoor/advertise-service\n", "the advertiser advertises only the node the gate's flag names")
	require.Contains(t, advertiser, "service=svc:example-dispatch\n")
	require.Contains(t, containers["cert-seed"]["args"].([]any)[0], "seeded the cached certificate")
}

// The front door writes one object, its TLS Secret, through a Role that names that Secret alone
// and binds the front door's own ServiceAccount alone; no Role lets any pod write a tailnet state
// Secret, and no container names one. The app and the listener keep ServiceAccount <hostname>,
// which the serviceAccountSubject output names (TestConstructOutputsTheServiceAccountIssuerAndSubject).
func TestDispatchFrontDoorAloneWritesItsTLSSecret(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	const roleType, bindingType = "kubernetes:rbac.authorization.k8s.io/v1:Role", "kubernetes:rbac.authorization.k8s.io/v1:RoleBinding"
	require.Len(t, registrationsOf(m, roleType), 1, "the component registers one Role")
	require.Len(t, registrationsOf(m, bindingType), 1, "the component registers one RoleBinding")
	role := m.Named(t, roleType, "example-dispatch-tailnet-tls").Inputs.Mappable()
	require.Equal(t, []any{map[string]any{
		"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"example-dispatch-tailnet-tls"},
		"verbs": []any{"get", "update", "patch"},
	}}, role["rules"])
	binding := m.Named(t, bindingType, "example-dispatch-tailnet-tls").Inputs.Mappable()
	require.Equal(t, map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "example-dispatch-tailnet-tls"}, binding["roleRef"])
	require.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "example-dispatch-tailnet", "namespace": "dispatch"}}, binding["subjects"])
	for _, r := range append(registrationsOf(m, roleType), registrationsOf(m, bindingType)...) {
		for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
			require.NotContains(t, s, "tailscale-state", "%s %s", r.Type, r.Name)
		}
	}

	var accounts []string
	for _, r := range registrationsOf(m, "kubernetes:core/v1:ServiceAccount") {
		accounts = append(accounts, r.Name)
	}
	require.ElementsMatch(t, []string{"example-dispatch", "example-dispatch-tailnet"}, accounts)
	for _, workload := range dispatchWorkloads {
		want := "example-dispatch"
		if workload.deployment == "example-dispatch-tailnet" {
			want = "example-dispatch-tailnet"
		}
		require.Equal(t, want, deploymentPodTemplate(t, m, workload.deployment)["spec"].(map[string]any)["serviceAccountName"], workload.deployment)
		for _, container := range podProbes(t, m, workload.deployment) {
			env, _ := container["env"].([]any)
			for _, item := range env {
				entry := item.(map[string]any)
				if entry["name"] == "TS_KUBE_SECRET" {
					require.Equal(t, "", entry["value"], "%s names no Kubernetes Secret for tailnet state", workload.deployment)
				}
			}
		}
	}
}

// The TLS Secret holds what cert-publish writes and nothing the program declares: it is registered
// with no data and with its data ignored, so no preview plans to undo a published certificate, and
// no pod template carries its checksum, so a renewed certificate rolls no pod.
func TestDispatchFrontDoorTLSSecretHoldsOnlyWhatThePodsPublish(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	secret := m.Named(t, "kubernetes:core/v1:Secret", "example-dispatch-tailnet-tls")
	require.Equal(t, "example-dispatch-tailnet-tls", secret.Inputs.Mappable()["metadata"].(map[string]any)["name"])
	require.NotContains(t, secret.Inputs, resource.PropertyKey("data"))
	require.NotContains(t, secret.Inputs, resource.PropertyKey("stringData"))
	require.ElementsMatch(t, []string{"data", "stringData"}, secret.RPC.GetIgnoreChanges())
	for _, workload := range dispatchWorkloads {
		annotations, _ := deploymentPodTemplate(t, m, workload.deployment)["metadata"].(map[string]any)["annotations"].(map[string]any)
		for key := range annotations {
			require.NotContains(t, key, "example-dispatch-tailnet-tls", workload.deployment)
		}
	}
	require.Contains(t, m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch-tailnet").RPC.GetDependencies(),
		"urn:pulumi:s::p::"+dispatchType+"$kubernetes:core/v1:Secret::example-dispatch-tailnet-tls",
		"the front door's pods mount the Secret, so the Deployment waits for it")
}

// tailnetService names the Tailscale Service the front door hosts, and the pods host
// svc:<hostname>, whose name the certificate and the URL carry: once it resolves to anything else,
// the run fails naming the input and never the value.
func TestDispatchRefusesATailnetServiceOtherThanItsOwn(t *testing.T) {
	for _, value := range []string{"svc:another-service", "another-service", "svc:example-dispatch-other"} {
		t.Run(value, func(t *testing.T) {
			args := dispatchArgs()
			args.TailnetService = pulumi.String(value)
			_, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: tailnetService must be svc:example-dispatch")
			require.NotContains(t, err.Error(), value)
		})
	}
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

// A listener bound to 0.0.0.0 refuses to start without a bearer. The app reaches the listener and
// the proxy over the network, each running in pods of its own.
func TestDispatchListenerIsAuthenticated(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

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

// A stopping listener pod keeps answering /v1 while its removal from Service example-dispatch-
// listener reaches every node's proxy: it waits 5 s before it is signalled, and its 35 s grace
// period covers that wait, the listener's own stop of about 26 s, and a margin.
func TestDispatchListenerKeepsServingUntilItsRemovalFromRoutingPropagates(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	template := deploymentPodTemplate(t, m, "example-dispatch-listener")
	require.EqualValues(t, 35, template["spec"].(map[string]any)["terminationGracePeriodSeconds"])
	require.Equal(t, map[string]any{"preStop": map[string]any{"exec": map[string]any{"command": []any{"sleep", "5"}}}},
		podContainer(t, template, "envoy-listener")["lifecycle"])
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

// Whatever reaches the proxy at 1055 gets tailnet egress as a front-door node, on every front-door
// pod, so only the two workloads that dial the hub through it may; and only the app calls the
// listener's API at 9020.
func TestDispatchPoliciesFenceTheProxyAndTheListener(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"example-dispatch", "example-dispatch-listener"},
		policyIngressPods(t, m, "example-dispatch-tailnet", 1055))
	require.Equal(t, []string{"example-dispatch"},
		policyIngressPods(t, m, "example-dispatch-listener", 9020))
}

// The front door's serve config, ConfigMap example-dispatch-serve's serve.json, hosts the
// Tailscale Service svc:example-dispatch, terminating TLS for its name on 443 and proxying to the
// app's Service.
func TestDispatchRenderingsMatchGoldens(t *testing.T) {
	require.Equal(t, readGolden(t, "dispatch-kubeconfig.golden"),
		gcp.DispatchKubeconfig("example-endpoint", "ZXhhbXBsZS1jYQ==", "example-dispatch"))
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
}

// serveConfig is the front door's serve.json as ConfigMap <hostname>-serve holds it.
func serveConfig(t *testing.T, m *pulumitest.Mocks) string {
	t.Helper()
	data := normalizedProperty(m.Named(t, "kubernetes:core/v1:ConfigMap", "example-dispatch-serve").Inputs["data"]).(map[string]any)
	require.Len(t, data, 1, "the ConfigMap holds serve.json alone")
	return data["serve.json"].(string)
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

// A front-door pod is a tailnet node of its own hosting the Tailscale Service, in a pod labelled
// app=<hostname>-tailnet: Service <hostname> selects app=<hostname> and would send user traffic to
// a pod wearing that label.
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

// Every workload's pods keep apart at any replica count, over that Deployment's own selector:
// required anti-affinity on the node, so a node's loss takes one pod at most, and a zone spread
// that leaves a pod Pending rather than stacking a zone, counting a second zone before Autopilot
// has a node there (minDomains 2). Old and new revisions count together (no matchLabelKeys), so a
// surge pod lands away from the pods it replaces.
func TestDispatchSpreadsEveryWorkloadAcrossNodesAndZones(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	for _, workload := range dispatchWorkloads {
		selector := map[string]any{"matchLabels": map[string]any{"app": workload.deployment}}
		require.Equal(t, selector, deploymentSpec(t, m, workload.deployment)["selector"])
		pod := deploymentPodTemplate(t, m, workload.deployment)["spec"].(map[string]any)
		require.Equal(t, map[string]any{"podAntiAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{"labelSelector": selector, "topologyKey": "kubernetes.io/hostname"}},
		}}, pod["affinity"], workload.deployment)
		require.Equal(t, []any{map[string]any{
			"labelSelector":     selector,
			"topologyKey":       "topology.kubernetes.io/zone",
			"maxSkew":           float64(1),
			"minDomains":        float64(2),
			"whenUnsatisfiable": "DoNotSchedule",
		}}, pod["topologySpreadConstraints"], workload.deployment)
	}
}

// podChecksums returns a pod template's checksum/ annotations.
func podChecksums(t *testing.T, m *pulumitest.Mocks, deployment string) map[string]any {
	t.Helper()
	annotations, _ := deploymentPodTemplate(t, m, deployment)["metadata"].(map[string]any)["annotations"].(map[string]any)
	checksums := map[string]any{}
	for key, value := range annotations {
		if strings.HasPrefix(key, "checksum/") {
			checksums[key] = value
		}
	}
	return checksums
}

// A running container never re-reads a Secret it reads through its environment, so each pod
// template carries checksum/<secret> for exactly the component Secrets its environment reads, set
// to that Secret's resourceVersion (the mocks compute <name>-resource-version), and its Deployment
// registers after the Secret: a changed Secret rolls the pods that read it, and no other. The
// app reads its own Secret and ENVOY_TOKEN from the listener's; the listener its own; the front
// door the tailnet auth key.
func TestDispatchPodsRollWhenTheSecretsTheyReadChange(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	for deployment, secrets := range map[string][]string{
		"example-dispatch":          {"example-dispatch-dispatch", "example-dispatch-listener"},
		"example-dispatch-listener": {"example-dispatch-listener"},
		"example-dispatch-tailnet":  {"example-dispatch-tailnet-auth-key"},
	} {
		want := map[string]any{}
		deps := m.Named(t, "kubernetes:apps/v1:Deployment", deployment).RPC.GetDependencies()
		for _, secret := range secrets {
			want["checksum/"+secret] = secret + "-resource-version"
			require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$kubernetes:core/v1:Secret::"+secret, deployment)
		}
		require.Equal(t, want, podChecksums(t, m, deployment), deployment)
	}
}

// podProbes returns every container of a pod template, init containers included, by name.
func podProbes(t *testing.T, m *pulumitest.Mocks, deployment string) map[string]map[string]any {
	t.Helper()
	pod := deploymentPodTemplate(t, m, deployment)["spec"].(map[string]any)
	containers := map[string]map[string]any{}
	for _, field := range []string{"initContainers", "containers"} {
		items, _ := pod[field].([]any)
		for _, item := range items {
			container := item.(map[string]any)
			containers[container["name"].(string)] = container
		}
	}
	return containers
}

// A liveness probe restarts a container, so it fails only when the process does not answer at
// all: Dispatch's and the listener's /healthz answer 503 while Postgres or NATS is down, and a
// liveness or startup probe failing on that would turn a dependency outage into a restart of
// every pod. So no liveness or startup command passes curl -f (any short-option cluster holding f,
// or --fail) and none is an httpGet on /healthz. Every serving container of the app and the
// listener has both probes; the listener's readiness allows the 6 s its /healthz may take to
// answer; and the app's startup probe allows 30 minutes, since envoy-dispatch migrates the
// database before it listens and nothing bounds a migration's run time.
func TestDispatchLivenessFailsOnlyWhenAProcessDoesNotAnswer(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	for _, workload := range dispatchWorkloads {
		for name, container := range podProbes(t, m, workload.deployment) {
			for _, kind := range []string{"startupProbe", "livenessProbe"} {
				probe, ok := container[kind].(map[string]any)
				if !ok {
					continue
				}
				if exec, ok := probe["exec"].(map[string]any); ok {
					for _, arg := range exec["command"].([]any) {
						arg := arg.(string)
						shortCluster := strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--")
						require.False(t, (shortCluster && strings.Contains(arg, "f")) || arg == "--fail",
							"%s's %s %s passes curl %s", workload.deployment, name, kind, arg)
					}
				}
				if get, ok := probe["httpGet"].(map[string]any); ok {
					require.NotEqual(t, "/healthz", get["path"], "%s's %s %s", workload.deployment, name, kind)
				}
			}
		}
	}
	for deployment, serving := range map[string][]string{
		"example-dispatch":          {"cloud-sql-proxy", "envoy-dispatch"},
		"example-dispatch-listener": {"envoy-listener"},
	} {
		containers := podProbes(t, m, deployment)
		for _, name := range serving {
			require.Contains(t, containers[name], "livenessProbe", "%s's %s", deployment, name)
			require.Contains(t, containers[name], "startupProbe", "%s's %s", deployment, name)
		}
	}
	ready := podProbes(t, m, "example-dispatch-listener")["envoy-listener"]["readinessProbe"].(map[string]any)
	require.GreaterOrEqual(t, ready["timeoutSeconds"], float64(6))
	startup := podProbes(t, m, "example-dispatch")["envoy-dispatch"]["startupProbe"].(map[string]any)
	require.GreaterOrEqual(t, startup["periodSeconds"].(float64)*startup["failureThreshold"].(float64), float64(30*60))
	proxy := podProbes(t, m, "example-dispatch")["cloud-sql-proxy"]
	require.Subset(t, proxy["args"], []any{"--health-check", "--http-address=0.0.0.0", "--http-port=9801"},
		"the proxy serves the endpoints its probes call")
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
		// The front door's TLS Secret is registered with no data at all (its pods write it).
		if data, ok := registration.Inputs["stringData"]; ok {
			require.NotContains(t, secretDataKeys(unwrappedProperty(data).ObjectValue()), "DISPATCH_AGENT_SECRETS_TOKEN")
			require.NotContains(t, secretDataKeys(unwrappedProperty(data).ObjectValue()), "DISPATCH_APP_PEM_B64")
		}
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
// its default 1 CPU and 2Gi, and the front door's and the listener's pods match their goldens.
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
	require.Equal(t, appResources("1", "2Gi"), proxyContainer(t, got)["resources"])
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
}

// proxyCpu and proxyMemory size the cloud-sql-proxy sidecar and nothing else, as both its request
// and its limit: the app's pod template is the default's with that one container's resources
// changed, so envoy-dispatch keeps its 1 CPU and 4Gi, and the front door's and the listener's pods
// match their goldens.
func TestDispatchProxyResourcesReachCloudSQLProxyAlone(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, row := range []struct{ cpu, memory string }{{"2", "4Gi"}, {"500m", "1536Mi"}} {
		t.Run(row.cpu+"/"+row.memory, func(t *testing.T) {
			args := dispatchArgs()
			args.ProxyCpu = pulumi.StringRef(row.cpu)
			args.ProxyMemory = pulumi.StringRef(row.memory)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			got := deploymentPodTemplate(t, m, "example-dispatch")
			require.Equal(t, appResources(row.cpu, row.memory), proxyContainer(t, got)["resources"])
			require.Equal(t, appResources("1", "4Gi"), podContainer(t, got, "envoy-dispatch")["resources"])
			want := deploymentPodTemplate(t, defaults, "example-dispatch")
			proxyContainer(t, want)["resources"] = appResources(row.cpu, row.memory)
			require.Equal(t, want, got)
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
		})
	}
}

// A size Autopilot would raise or reject, judged on envoy-dispatch's and cloud-sql-proxy's sum,
// is refused by the resolver, before the component registers, so nothing beyond the test's
// providers exists.
func TestDispatchRefusesAppResourcesBeforeRegistering(t *testing.T) {
	for _, row := range []struct {
		edit func(*gcp.DispatchArgs)
		err  string
	}{
		{func(a *gcp.DispatchArgs) {
			a.AppCpu = pulumi.StringRef("100m")
			a.ProxyCpu = pulumi.StringRef("200m")
		}, `appCpu "100m" and proxyCpu "200m" must make at least 500m together`},
		{func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("30") }, `appCpu "1" and proxyCpu "30" must make at most 30 CPU together`},
		{func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("0") }, `proxyMemory "0" must be more than 0`},
	} {
		t.Run(row.err, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
		})
	}
}

const (
	podDisruptionBudgetType = "kubernetes:policy/v1:PodDisruptionBudget"
	priorityClassType       = "kubernetes:scheduling.k8s.io/v1:PriorityClass"
)

// safeToEvict reports whether deployment's pod template carries the cluster autoscaler's
// safe-to-evict annotation, failing when it carries another value than "false".
func safeToEvict(t *testing.T, m *pulumitest.Mocks, deployment string) bool {
	t.Helper()
	annotations, _ := deploymentPodTemplate(t, m, deployment)["metadata"].(map[string]any)["annotations"].(map[string]any)
	value, ok := annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]
	if ok {
		require.Equal(t, "false", value, deployment)
	}
	return ok
}

// Unset, priorityClassName and the three budgets leave no trace: no PodDisruptionBudget, no
// PriorityClass read, no pod naming a priority class, and every pod template carrying
// safe-to-evict "false", so the autoscaler never evicts a pod no budget protects.
func TestDispatchRegistersNoBudgetOrPriorityClassUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.Zero(t, countRegistrations(m, podDisruptionBudgetType))
	require.Zero(t, countRegistrations(m, priorityClassType))
	for _, workload := range dispatchWorkloads {
		require.NotContains(t, deploymentPodTemplate(t, m, workload.deployment)["spec"], "priorityClassName", workload.deployment)
		require.True(t, safeToEvict(t, m, workload.deployment), workload.deployment)
	}
}

// requireKubernetesChild asserts that r is a child of the component on its own Kubernetes
// provider, waiting on the cluster, as AGENTS.md has every Kubernetes child do.
func requireKubernetesChild(t *testing.T, r pulumitest.Registration) {
	t.Helper()
	require.Equal(t, "urn:pulumi:s::p::"+dispatchType+"::example-dispatch", r.RPC.GetParent())
	require.Contains(t, r.RPC.GetProvider(), "pulumi:providers:kubernetes::example-dispatch-kubernetes::")
	require.Contains(t, r.RPC.GetDependencies(), "urn:pulumi:s::p::"+dispatchType+"$gcp:container/cluster:Cluster::example-dispatch")
}

// priorityClassName reaches every pod of the three Deployments: each template is the default's
// with priorityClassName added. The component creates no PriorityClass; it reads the named one
// once, and all three Deployments wait on that read, so a class the cluster lacks fails the update
// before any pod is replaced.
func TestDispatchPriorityClassReachesEveryPod(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, class := range []string{"example-priority", "example.priority"} {
		t.Run(class, func(t *testing.T) {
			args := dispatchArgs()
			args.PriorityClassName = pulumi.StringRef(class)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			require.Equal(t, 1, countRegistrations(m, priorityClassType), "the class is read once")
			read := m.Named(t, priorityClassType, "example-dispatch-priority-class")
			require.Equal(t, class, read.ID)
			require.Nil(t, read.RPC, "the PriorityClass is read, never registered")
			require.Equal(t, "urn:pulumi:s::p::"+dispatchType+"::example-dispatch", read.ReadRPC.GetParent())
			require.Contains(t, read.ReadRPC.GetProvider(), "pulumi:providers:kubernetes::example-dispatch-kubernetes::")
			require.Contains(t, read.ReadRPC.GetDependencies(), "urn:pulumi:s::p::"+dispatchType+"$gcp:container/cluster:Cluster::example-dispatch")
			for _, workload := range dispatchWorkloads {
				want := deploymentPodTemplate(t, defaults, workload.deployment)
				want["spec"].(map[string]any)["priorityClassName"] = class
				require.Equal(t, want, deploymentPodTemplate(t, m, workload.deployment), workload.deployment)
				deps := m.Named(t, "kubernetes:apps/v1:Deployment", workload.deployment).RPC.GetDependencies()
				require.Contains(t, deps, "urn:pulumi:s::p::"+dispatchType+"$"+priorityClassType+"::example-dispatch-priority-class", workload.deployment)
			}
			require.ElementsMatch(t, append(defaults.Types(), priorityClassType), m.Types())
		})
	}
}

// requireBudget fails unless one PodDisruptionBudget, named for deployment, in namespace, covers
// that Deployment's pods by the selector the Deployment itself matches, with minAvailable and
// Kubernetes' default unhealthyPodEvictionPolicy, as a Kubernetes child of the component.
func requireBudget(t *testing.T, m *pulumitest.Mocks, deployment, namespace string, minAvailable int) {
	t.Helper()
	registration := m.Named(t, podDisruptionBudgetType, deployment)
	requireKubernetesChild(t, registration)
	budget := registration.Inputs.Mappable()
	require.Equal(t, map[string]any{"name": deployment, "namespace": namespace}, budget["metadata"])
	require.Equal(t, map[string]any{
		"minAvailable": float64(minAvailable),
		"selector":     map[string]any{"matchLabels": map[string]any{"app": deployment}},
	}, budget["spec"])
	require.Equal(t, deploymentSpec(t, m, deployment)["selector"], budget["spec"].(map[string]any)["selector"])
}

// Each budget input registers one PodDisruptionBudget over its own Deployment's pods alone, and
// that Deployment's template drops safe-to-evict, which the budget replaces, and is otherwise the
// default's; the other two Deployments keep the annotation and get no budget.
func TestDispatchEachBudgetCoversItsOwnPods(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, workload := range dispatchWorkloads {
		for _, row := range []struct {
			replicas, minAvailable int
			namespace              string
		}{{1, 1, ""}, {3, 2, ""}, {3, 3, ""}, {2, 1, "example-namespace"}} {
			t.Run(fmt.Sprintf("%s %d of %d in %q", workload.deployment, row.minAvailable, row.replicas, row.namespace), func(t *testing.T) {
				args := dispatchArgs()
				workload.replicas(args, row.replicas)
				workload.budget(args, row.minAvailable)
				namespace := "dispatch"
				if row.namespace != "" {
					args.Namespace = pulumi.StringRef(row.namespace)
					namespace = row.namespace
				}
				m, err := runDispatch(t, args, true)
				require.NoError(t, err)

				require.Equal(t, 1, countRegistrations(m, podDisruptionBudgetType))
				requireBudget(t, m, workload.deployment, namespace, row.minAvailable)
				for _, other := range dispatchWorkloads {
					require.Equal(t, other.deployment != workload.deployment, safeToEvict(t, m, other.deployment),
						"safe-to-evict on %s exactly when no budget covers it", other.deployment)
				}
				if row.namespace == "" {
					want := deploymentPodTemplate(t, defaults, workload.deployment)
					delete(want["metadata"].(map[string]any)["annotations"].(map[string]any), "cluster-autoscaler.kubernetes.io/safe-to-evict")
					require.Equal(t, want, deploymentPodTemplate(t, m, workload.deployment))
					require.ElementsMatch(t, append(defaults.Types(), podDisruptionBudgetType), m.Types())
				}
			})
		}
	}
}

// All three budgets set, each Deployment has its own, under its own name and selector, and no
// template carries safe-to-evict.
func TestDispatchBudgetsOnEveryWorkloadStayDistinct(t *testing.T) {
	args := dispatchArgs()
	for _, workload := range dispatchWorkloads {
		workload.replicas(args, 3)
		workload.budget(args, 2)
	}
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	require.Equal(t, 3, countRegistrations(m, podDisruptionBudgetType))
	for _, workload := range dispatchWorkloads {
		requireBudget(t, m, workload.deployment, "dispatch", 2)
		require.False(t, safeToEvict(t, m, workload.deployment), workload.deployment)
	}
}

// A budget its Deployment's replica count cannot meet is refused by the resolver, naming its own
// input, before the component registers, so nothing beyond the test's providers exists.
func TestDispatchRefusesABudgetBeforeRegistering(t *testing.T) {
	for _, row := range []struct {
		edit func(*gcp.DispatchArgs)
		err  string
	}{
		{func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(2) }, "appPdbMinAvailable 2 must be from 1 to replicas (1)"},
		{func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(3) }, "frontDoorPdbMinAvailable 3 must be from 1 to frontDoorReplicas (2)"},
		{func(a *gcp.DispatchArgs) { a.ListenerPdbMinAvailable = pulumi.IntRef(2) }, "listenerPdbMinAvailable 2 must be from 1 to listenerReplicas (1)"},
	} {
		t.Run(row.err, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType}, m.Types())
		})
	}
}

// databaseAvailabilityType reaches the Cloud SQL instance's settings: ZONAL, the instance's value
// before the input existed, when unset, and REGIONAL, a standby in a second zone, when set.
func TestDispatchDatabaseAvailabilityTypeReachesTheInstance(t *testing.T) {
	for _, row := range []struct {
		value *string
		want  string
	}{{nil, "ZONAL"}, {pulumi.StringRef("ZONAL"), "ZONAL"}, {pulumi.StringRef("REGIONAL"), "REGIONAL"}} {
		t.Run(row.want, func(t *testing.T) {
			args := dispatchArgs()
			args.DatabaseAvailabilityType = row.value
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			settings := m.One(t, "gcp:sql/databaseInstance:DatabaseInstance").Inputs.Mappable()["settings"].(map[string]any)
			require.Equal(t, row.want, settings["availabilityType"])
		})
	}
}

// Unset, databaseTier reaches the Cloud SQL instance's settings as db-custom-4-15360, 4 vCPU and
// 15 GB, whose default max_connections of 500 holds 15 server pods of 32 connections each; set, it
// reaches them as written.
func TestDispatchDatabaseTierReachesTheInstance(t *testing.T) {
	for _, row := range []struct {
		value *string
		want  string
	}{{nil, "db-custom-4-15360"}, {pulumi.StringRef("db-custom-2-7680"), "db-custom-2-7680"}} {
		t.Run(row.want, func(t *testing.T) {
			args := dispatchArgs()
			args.DatabaseTier = row.value
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			settings := m.One(t, "gcp:sql/databaseInstance:DatabaseInstance").Inputs.Mappable()["settings"].(map[string]any)
			require.Equal(t, row.want, settings["tier"])
		})
	}
}
