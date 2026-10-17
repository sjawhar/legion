package gcp_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"sync"
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

// testAppRSAKey is one throwaway RSA key for the whole test binary, never a real credential;
// 2048 bits, as GitHub App keys are, generated once because RSA generation is slow.
var testAppRSAKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

// testAppPrivateKeyPEM is that key as PKCS#8, one of the two encodings Dispatch boots on.
func testAppPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := testAppRSAKey()
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// testAppPrivateKeyPKCS1PEM is the same key as PKCS#1, the form GitHub serves an App's key in.
func testAppPrivateKeyPKCS1PEM(t *testing.T) string {
	t.Helper()
	key, err := testAppRSAKey()
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// dispatchBrokerAndAppArgs is dispatchArgs with both optional pairs set: the secrets-broker relay
// (agentSecretsUrl with agentSecretsToken) and the GitHub App (appClientId with appPrivateKey).
func dispatchBrokerAndAppArgs(t *testing.T) *gcp.DispatchArgs {
	t.Helper()
	args := dispatchArgs()
	args.AgentSecretsUrl = "https://example-broker.internal.example"
	args.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
	args.AppClientId = "example-app-client-id"
	args.AppPrivateKey = pulumi.String(testAppPrivateKeyPEM(t))
	return args
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
	require.Equal(t, `{"TCP":{"443":{"HTTPS":true}},"Web":{"example-dispatch.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8766"}}}}}`, settings.ServeConfig)
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
		{name: "no tailnet auth key", edit: func(a *gcp.DispatchArgs) { a.TailnetAuthKey = nil }, err: []string{"tailnetAuthKey", "required"}},
		{name: "no NATS URL", edit: func(a *gcp.DispatchArgs) { a.NatsUrl = nil }, err: []string{"natsUrl", "required"}},
		{name: "no NATS seed secret", edit: func(a *gcp.DispatchArgs) { a.NatsSeedSecret = nil }, err: []string{"natsSeedSecret", "required"}},
		{name: "no sign-in client secret", edit: func(a *gcp.DispatchArgs) { a.SignInClientSecret = nil }, err: []string{"signInClientSecret", "required"}},
		{name: "an agent secrets token without its URL", edit: func(a *gcp.DispatchArgs) { a.AgentSecretsToken = pulumi.String("example-agent-secrets-token") }, err: []string{"agentSecretsUrl", "required when agentSecretsToken is set"}},
		{name: "an agent secrets URL without its token", edit: func(a *gcp.DispatchArgs) { a.AgentSecretsUrl = "https://example-broker.internal.example" }, err: []string{"agentSecretsToken", "required when agentSecretsUrl is set"}},
		{name: "an agent secrets URL with a path", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example/v1"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no path"}},
		{name: "an agent secrets URL without a scheme", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "example-broker.internal.example"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "absolute http(s) URL"}},
		{name: "an agent secrets URL with a user and password", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://user:example-pass@example-broker.internal.example"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a user alone", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://user@example-broker.internal.example"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a query", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example?token=example-pass"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with an empty query", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example?"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with a fragment", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example#example-pass"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an agent secrets URL with an empty fragment", edit: func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl = "https://example-broker.internal.example#"
			a.AgentSecretsToken = pulumi.String("example-agent-secrets-token")
		}, err: []string{"agentSecretsUrl", "no user, password, query or fragment", "(not echoed)"}},
		{name: "an app client ID without its key", edit: func(a *gcp.DispatchArgs) { a.AppClientId = "example-app-client-id" }, err: []string{"appPrivateKey", "required when appClientId is set"}},
		{name: "an app private key without its client ID", edit: func(a *gcp.DispatchArgs) { a.AppPrivateKey = pulumi.String("example-key") }, err: []string{"appClientId", "required when appPrivateKey is set"}},
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
		{"a seed inside the agent secrets URL", "agentSecretsUrl", func(a *gcp.DispatchArgs) { a.AgentSecretsUrl = "https://" + lower + ".internal.example" }},
		{"a seed as the app client ID", "appClientId", func(a *gcp.DispatchArgs) { a.AppClientId = seed }},
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
		"random:index/randomPassword:RandomPassword":        3,
		"gcp:sql/database:Database":                         1,
		"gcp:sql/user:User":                                 1,
		"gcp:secretmanager/secretIamMember:SecretIamMember": 1,
		"gcp:projects/iAMMember:IAMMember":                  1,
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

// Dispatch exits when its first Postgres ping through the Auth Proxy fails, and the proxy cannot
// connect without roles/cloudsql.client, so the component grants the role itself: a consumer's
// grant built from the `principal` output resolves only after every child, the Deployment's
// readiness included, so the first deployment would deadlock. Cloud SQL has no per-instance IAM,
// so the grant is project-scoped with a condition naming this instance alone, and it waits for
// the cluster that creates the principal's pool, as the seed accessor does.
func TestDispatchGrantsCloudSQLClientOnItsInstanceAlone(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	member := m.One(t, "gcp:projects/iAMMember:IAMMember")
	inputs := member.Inputs.Mappable()
	require.Equal(t, "roles/cloudsql.client", inputs["role"])
	require.Equal(t, "example-project", inputs["project"])
	condition := inputs["condition"].(map[string]any)
	require.Equal(t,
		"resource.type == 'sqladmin.googleapis.com/Instance' && resource.name == 'projects/example-project/instances/example-dispatch'",
		condition["expression"])
	clusterURN := "urn:pulumi:s::p::" + dispatchType + "$gcp:container/cluster:Cluster::example-dispatch"
	require.Contains(t, member.RPC.GetDependencies(), clusterURN,
		"the grant is written after the cluster that creates its principal's pool")
	require.Contains(t, member.RPC.GetPropertyDependencies()["member"].GetUrns(), clusterURN,
		"the principal is built from the cluster's own Workload Identity pool, so it resolves only once the pool exists")
}

// The pod is ready only once the Auth Proxy can connect (roles/cloudsql.client) and the seed
// volume can mount (roles/secretmanager.secretAccessor), so the Deployment registers after both
// grants rather than racing IAM on the first deployment.
func TestDispatchDeploymentWaitsForBothGrants(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	deployment := m.One(t, "kubernetes:apps/v1:Deployment")
	for _, grant := range []string{
		"$gcp:projects/iAMMember:IAMMember::example-dispatch-cloudsql-client",
		"$gcp:secretmanager/secretIamMember:SecretIamMember::example-dispatch-nats-seed-accessor",
	} {
		require.Contains(t, deployment.RPC.GetDependencies(), "urn:pulumi:s::p::"+dispatchType+grant,
			"the Deployment's readiness needs the grant, so it registers after it")
	}
}

// GKE accepts only the project's own pool, named <project id>.svc.id.goog.
func TestDispatchClusterUsesTheProjectsWorkloadPool(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	config := m.One(t, "gcp:container/cluster:Cluster").Inputs.Mappable()["workloadIdentityConfig"].(map[string]any)
	require.Equal(t, "example-project.svc.id.goog", config["workloadPool"])
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

func TestDispatchServiceExposesPort8766(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	service := m.One(t, "kubernetes:core/v1:Service")
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

func TestDispatchNetworkPolicyAdmitsNoIngressByDefault(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	policy := m.One(t, "kubernetes:networking.k8s.io/v1:NetworkPolicy")
	spec := policy.Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, []any{"Ingress"}, spec["policyTypes"])
	require.Empty(t, spec["ingress"], "with no allowed namespaces the policy holds no rule, so it admits no ingress at all")
}

func TestDispatchNetworkPolicyAdmitsOnlyTheGivenNamespaces(t *testing.T) {
	args := dispatchArgs()
	args.AllowedNamespaces = []string{"envoy", "legion"}
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)

	policy := m.One(t, "kubernetes:networking.k8s.io/v1:NetworkPolicy")
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
	deployment := m.One(t, "kubernetes:apps/v1:Deployment")
	template := deployment.Inputs["spec"].ObjectValue()["template"]
	got, err := json.MarshalIndent(normalizedProperty(template), "", "  ")
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-pod.golden"), string(got)+"\n")
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

// appPrivateKey is an Input, checked once it resolves inside the ApplyT that renders
// DISPATCH_APP_PEM_B64, against the key rule Dispatch boots on (githubapp.parsePrivateKey,
// packages/envoy/internal/dispatch/githubapp/client.go: the first PEM block as PKCS#1, else
// PKCS#8, and it must be an *rsa.PrivateKey): a wrong paste — a path, the base64 of the PEM, a
// public key, garbage in a PEM frame, or a key of another algorithm — is refused naming
// appPrivateKey and never echoing it, so it fails the preview or update rather than the pod's
// boot.
func TestDispatchRefusesAnAppPrivateKeyDispatchCannotBootOn(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	sec1, err := x509.MarshalECPrivateKey(ecKey)
	require.NoError(t, err)
	ecPKCS8, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edPKCS8, err := x509.MarshalPKCS8PrivateKey(edKey)
	require.NoError(t, err)
	rsaKey, err := testAppRSAKey()
	require.NoError(t, err)
	rsaPublic, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)
	frame := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	for _, row := range []struct {
		name, key, want string
	}{
		{"no PEM block", "example-not-a-pem",
			"appPrivateKey does not parse as a PEM block (not echoed)"},
		{"garbage inside a PEM frame", frame("RSA PRIVATE KEY", []byte("example-garbage")),
			"appPrivateKey's PEM block is not a PKCS#1 or PKCS#8 RSA private key, which Dispatch's GitHub App needs to boot (not echoed)"},
		{"an RSA public key", frame("PUBLIC KEY", rsaPublic),
			"appPrivateKey's PEM block is not a PKCS#1 or PKCS#8 RSA private key, which Dispatch's GitHub App needs to boot (not echoed)"},
		{"a SEC1 EC private key", frame("EC PRIVATE KEY", sec1),
			"appPrivateKey's PEM block is not a PKCS#1 or PKCS#8 RSA private key, which Dispatch's GitHub App needs to boot (not echoed)"},
		{"PKCS#8 holding an EC key", frame("PRIVATE KEY", ecPKCS8),
			"appPrivateKey's PEM block holds an ECDSA key, and Dispatch's GitHub App needs an RSA private key to boot (not echoed)"},
		{"PKCS#8 holding an Ed25519 key", frame("PRIVATE KEY", edPKCS8),
			"appPrivateKey's PEM block holds an Ed25519 key, and Dispatch's GitHub App needs an RSA private key to boot (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchBrokerAndAppArgs(t)
			args.AppPrivateKey = pulumi.String(row.key)
			_, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			for line := range strings.Lines(row.key) {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					require.NotContains(t, err.Error(), trimmed, "the refusal echoes the key")
				}
			}
		})
	}
}

// A blank or whitespace-only value of any secret input Dispatch reads as a token or credential
// renders a Secret key Dispatch refuses only at boot (an empty token file, an empty PEM, an empty
// client secret, an empty auth key), so the component refuses it once the input resolves, naming
// the input and never the value: one rule, refuseDispatchSecret, which all four inputs' resolved
// checks run before anything else.
func TestDispatchRefusesABlankSecretOnceItResolves(t *testing.T) {
	for _, input := range []struct {
		name string
		edit func(*gcp.DispatchArgs, pulumi.StringInput)
	}{
		{"tailnetAuthKey", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.TailnetAuthKey = v }},
		{"signInClientSecret", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.SignInClientSecret = v }},
		{"agentSecretsToken", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.AgentSecretsToken = v }},
		{"appPrivateKey", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.AppPrivateKey = v }},
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

// stringProperty is value as the string it holds, unwrapped from any secret or output wrapper.
// The mock engine records a Secret's stringData values as plain strings whatever their marking
// (the existing DISPATCH_SIGNIN_CLIENT_SECRET records plain too), so secretness is not observable
// here; pulumi.ToSecret in refuseDispatchSeedInSecret and dispatchAppPEM carries it.
func stringProperty(t *testing.T, value resource.PropertyValue) string {
	t.Helper()
	unwrapped := unwrappedProperty(value)
	require.True(t, unwrapped.IsString(), "the value is not a string: %s", unwrapped.TypeString())
	return unwrapped.StringValue()
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

// Both pairs set, the <hostname>-dispatch Secret gains exactly two keys:
// DISPATCH_AGENT_SECRETS_TOKEN, the token the pod mounts as a file, and DISPATCH_APP_PEM_B64, the
// base64 of the PEM — with the key in either encoding Dispatch boots on, RSA as PKCS#8 or as
// PKCS#1 (the form GitHub serves an App's key in).
func TestDispatchSecretCarriesTheBrokerTokenAndTheAppKey(t *testing.T) {
	for _, row := range []struct{ name, pemText string }{
		{"an RSA key as PKCS#8", testAppPrivateKeyPEM(t)},
		{"an RSA key as PKCS#1", testAppPrivateKeyPKCS1PEM(t)},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchBrokerAndAppArgs(t)
			args.AppPrivateKey = pulumi.String(row.pemText)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			data := dispatchSecretData(t, m)
			require.ElementsMatch(t, []string{
				"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY", "DISPATCH_SIGNIN_CLIENT_SECRET",
				"DISPATCH_AGENT_SECRETS_TOKEN", "DISPATCH_APP_PEM_B64",
			}, secretDataKeys(data))
			require.Equal(t, "example-agent-secrets-token", stringProperty(t, data["DISPATCH_AGENT_SECRETS_TOKEN"]))
			decoded, err := base64.StdEncoding.DecodeString(stringProperty(t, data["DISPATCH_APP_PEM_B64"]))
			require.NoError(t, err)
			require.Equal(t, row.pemText, string(decoded))
		})
	}
}

// Unset, neither pair leaves a trace: the Secret keeps exactly its four keys, and
// TestDispatchPodMatchesGolden pins the pod template byte-identical to before the inputs existed.
func TestDispatchSecretCarriesNoBrokerOrAppKeyUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY", "DISPATCH_SIGNIN_CLIENT_SECRET",
	}, secretDataKeys(dispatchSecretData(t, m)))
}

// Both pairs set, the pod template gains exactly the broker env pair (DISPATCH_AGENT_SECRETS_URL
// and DISPATCH_AGENT_SECRETS_TOKEN_FILE), the app env pair (DISPATCH_APP_CLIENT_ID plain and
// DISPATCH_APP_PEM_B64 from the Secret), the read-only token mount and its one-key volume.
func TestDispatchPodWithBrokerAndAppMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchBrokerAndAppArgs(t), true)
	require.NoError(t, err)
	deployment := m.One(t, "kubernetes:apps/v1:Deployment")
	template := deployment.Inputs["spec"].ObjectValue()["template"]
	got, err := json.MarshalIndent(normalizedProperty(template), "", "  ")
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-pod-with-broker-and-app.golden"), string(got)+"\n")
}
