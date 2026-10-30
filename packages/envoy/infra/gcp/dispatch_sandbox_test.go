package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// dispatchSandboxArgs are the inputs of a sandbox Dispatch: the tailnet group and the sign-in
// group left unset, which is what asks for the mode, and the three inputs the mode then needs.
// The NATS hub runs in the pods' own cluster, since without a front door there is no SOCKS5 proxy
// to reach a tailnet one through, and natsUrl names that hub's Service rather than a local
// address, which the pods would read as themselves. allowedNamespaces is the app's only ingress
// here, so it names the one namespace whose pods reach Dispatch. envoyImage, cloudSqlProxyImage
// and natsSeedSecret are the sandbox's as much as the tailnet's: the app's pod runs those two
// images and mounts that seed in either mode.
func dispatchSandboxArgs() *gcp.DispatchArgs {
	return &gcp.DispatchArgs{
		Region:             "us-central1",
		Hostname:           "example-dispatch",
		EnvoyImage:         dispatchEnvoyImage,
		CloudSqlProxyImage: dispatchProxyImage,
		NatsInCluster:      pulumi.BoolRef(true),
		NatsUrl:            pulumi.String(inClusterNatsURL),
		NatsSeedSecret:     pulumi.String("projects/example-project/secrets/example-nats-seed"),
		AllowedNamespaces:  []string{"legion"},
	}
}

// dispatchSandboxURL is where a sandbox Dispatch is reached: its own Service in its namespace, on
// the 8766 that Service publishes, over http because that Service terminates no TLS.
const dispatchSandboxURL = "http://example-dispatch.dispatch.svc.cluster.local:8766"

// runDispatchSandbox runs args through the mock engine with both providers, which a sandbox needs
// as the default mode does: it builds the same database and the same generated passwords. check,
// when not nil, runs on the constructed component inside the program, where its outputs resolve.
func runDispatchSandbox(t *testing.T, args *gcp.DispatchArgs, check func(*gcp.Dispatch)) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	err := m.Run(func(ctx *pulumi.Context) error {
		gcpProvider, err := newGCPProvider(ctx)
		if err != nil {
			return err
		}
		randomProvider, err := newRandomProvider(ctx)
		if err != nil {
			return err
		}
		dispatch, err := gcp.NewDispatch(ctx, "example-dispatch", args, pulumi.Providers(gcpProvider, randomProvider))
		if err != nil {
			return err
		}
		if check != nil {
			check(dispatch)
		}
		return nil
	})
	return m, err
}

// dispatchSandboxFrontDoor is the front door, as the default mode's registration golden holds it:
// the tailnet pods' Deployment, the Service the two NATS clients dial their SOCKS5 proxy at, the
// policy fencing that proxy, the account those pods run as, the auth key they join the tailnet
// with, the Secret they keep their certificate in and the Role and RoleBinding they write it
// with, and the ConfigMap holding the serve.json they serve Dispatch by. Each resource is named
// in full rather than matched on "tailnet": <hostname>-serve carries no such word, and a match on
// one would leave the sandbox free to build the front door's serve configuration.
var dispatchSandboxFrontDoor = []string{
	"kubernetes:apps/v1:Deployment example-dispatch-tailnet",
	"kubernetes:core/v1:Service example-dispatch-tailnet",
	"kubernetes:networking.k8s.io/v1:NetworkPolicy example-dispatch-tailnet",
	"kubernetes:core/v1:ServiceAccount example-dispatch-tailnet",
	"kubernetes:core/v1:Secret example-dispatch-tailnet-auth-key",
	"kubernetes:core/v1:Secret example-dispatch-tailnet-tls",
	"kubernetes:rbac.authorization.k8s.io/v1:Role example-dispatch-tailnet-tls",
	"kubernetes:rbac.authorization.k8s.io/v1:RoleBinding example-dispatch-tailnet-tls",
	"kubernetes:core/v1:ConfigMap example-dispatch-serve",
}

// dispatchSandboxGolden is the whole sandbox declaration: the default mode's registration golden
// less the front door's resources, each under the name and the parent the default mode gives it.
// Reading the rest from that golden rather than writing it out again is what holds the two modes
// to one declaration: a resource the default mode renames, reparents, adds or drops moves this
// expectation with it. The front-door resources are removed by name and the removals counted, so
// one of them renamed fails here rather than passing as a resource the sandbox never had.
func dispatchSandboxGolden(t *testing.T) string {
	t.Helper()
	var kept, dropped []string
	for _, line := range strings.Split(readGolden(t, "dispatch-registrations.golden"), "\n") {
		if line == "" {
			continue
		}
		registration, _, ok := strings.Cut(line, " parent=")
		require.True(t, ok, "%q names the parent it was registered under", line)
		if slices.Contains(dispatchSandboxFrontDoor, registration) {
			dropped = append(dropped, registration)
			continue
		}
		kept = append(kept, line+"\n")
	}
	require.ElementsMatch(t, dispatchSandboxFrontDoor, dropped, "the golden holds every front-door resource under the name this file names")
	return strings.Join(kept, "")
}

// Without the tailnet inputs the component builds no front door: no tailnet Deployment, Service,
// policy or account, no auth key or certificate Secret and neither RBAC object its pods write that
// certificate with, and no serve ConfigMap. Everything else it declares is what the default mode
// declares, under the same names and parents, so a consumer that later sets the tailnet inputs
// gains the front door rather than replacing its cluster, its database and its app. The
// comparison below is of the whole registration set, so a front-door resource registering, or any
// other resource going missing, is a line it does not hold and fails naming it.
func TestDispatchSandboxRegistrations(t *testing.T) {
	m, err := runDispatchSandbox(t, dispatchSandboxArgs(), nil)
	require.NoError(t, err)
	require.Equal(t, dispatchSandboxGolden(t), dispatchRegistrations(m))
}

// With no front door hosting a tailnet name, Dispatch is reached at its own Service inside the
// cluster, and that is what it tells a reader it is at: the url output a consumer reads and the
// DISPATCH_SERVER_URL the app builds its own links from are one address, so a browser the app
// sends somewhere and a client the output sends somewhere arrive at the same server.
func TestDispatchSandboxServesInTheCluster(t *testing.T) {
	var url string
	m, err := runDispatchSandbox(t, dispatchSandboxArgs(), func(dispatch *gcp.Dispatch) {
		url = await(t, dispatch.Url)
	})
	require.NoError(t, err)

	require.Equal(t, dispatchSandboxURL, url)
	require.Equal(t, url, appEnv(t, m)["DISPATCH_SERVER_URL"], "the output and the app agree on where Dispatch is")
}

// Without the sign-in inputs the app runs no browser sign-in at all: it is given no sign-in
// setting to read, and the client secret is not a key of its Secret, so nothing in the cluster
// holds an OAuth credential the deployment never got.
func TestDispatchSandboxRendersNoSignIn(t *testing.T) {
	m, err := runDispatchSandbox(t, dispatchSandboxArgs(), nil)
	require.NoError(t, err)

	for name := range appEnv(t, m) {
		require.False(t, strings.HasPrefix(name, "DISPATCH_SIGNIN_"), "%s configures browser sign-in", name)
	}
	require.ElementsMatch(t, []string{"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY"},
		secretDataKeys(dispatchSecretData(t, m)))
}

// The hub a sandbox's pods use runs in their own cluster, and both NATS clients dial it
// themselves: the app and the listener carry its Service as their NATS URL and neither names a
// SOCKS5 proxy at all, the key being absent rather than empty, since the binaries read an empty
// one as a proxy to use. There is no front door to run such a proxy here.
func TestDispatchSandboxDialsTheInClusterHub(t *testing.T) {
	m, err := runDispatchSandbox(t, dispatchSandboxArgs(), nil)
	require.NoError(t, err)

	for container, env := range dispatchNatsEnvs(t, m) {
		require.Equal(t, inClusterNatsURL, env["NATS_URLS"], container)
		require.NotContains(t, env, "ENVOY_NATS_SOCKS5_PROXY", "%s dials the hub itself", container)
	}
}

// With no front door the app's policy is its only ingress, and allowedNamespaces is all of it:
// one rule on 8766, admitting the pods of each named namespace and nothing else. The front door's
// own rule is gone with the pods it admitted, so no namespace reaches Dispatch by being named
// there, and a rule with an empty from, which would admit every pod in the cluster, is not what
// the mode renders.
func TestDispatchSandboxAdmitsOnlyTheAllowedNamespaces(t *testing.T) {
	args := dispatchSandboxArgs()
	args.AllowedNamespaces = []string{"envoy", "legion"}
	m, err := runDispatchSandbox(t, args, nil)
	require.NoError(t, err)

	rule := policyIngressRules(t, m, "example-dispatch", 1)[0].(map[string]any)
	ports := rule["ports"].([]any)
	require.Len(t, ports, 1)
	require.EqualValues(t, 8766, ports[0].(map[string]any)["port"])
	from := rule["from"].([]any)
	namespaces := make([]string, 0, len(from))
	for _, peer := range from {
		fields := peer.(map[string]any)
		require.NotContains(t, fields, "podSelector", "no front-door pod exists to admit")
		require.NotContains(t, fields, "ipBlock")
		namespaces = append(namespaces, fields["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)["kubernetes.io/metadata.name"].(string))
	}
	require.Equal(t, args.AllowedNamespaces, namespaces)
}

// Every input a sandbox cannot make good is refused by its own name, so a consumer learns what to
// correct rather than having its value accepted and quietly dropped: the two inputs the mode needs
// when they are missing, the two that size and protect a front door that is not built, and a
// sign-in input, which needs an origin an OAuth provider takes as a redirect and so needs the
// tailnet inputs. The resolver is what refuses, and it needs no engine.
func TestDispatchSandboxRefusesWhatItDoesNotBuild(t *testing.T) {
	for _, row := range []struct {
		name, input string
		set         func(*gcp.DispatchArgs)
	}{
		{"natsInCluster unset", "natsInCluster", func(a *gcp.DispatchArgs) { a.NatsInCluster = nil }},
		{"natsInCluster false", "natsInCluster", func(a *gcp.DispatchArgs) { a.NatsInCluster = pulumi.BoolRef(false) }},
		{"allowedNamespaces empty", "allowedNamespaces", func(a *gcp.DispatchArgs) { a.AllowedNamespaces = nil }},
		{"frontDoorReplicas", "frontDoorReplicas", func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(2) }},
		{"frontDoorPdbMinAvailable", "frontDoorPdbMinAvailable", func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(1) }},
		{"signInClientId", "signInClientId", func(a *gcp.DispatchArgs) { a.SignInClientId = "example-client-id" }},
		{"signInClientSecret", "signInClientSecret", func(a *gcp.DispatchArgs) {
			a.SignInClientSecret = pulumi.String("example-client-secret")
		}},
		{"signInDomain", "signInDomain", func(a *gcp.DispatchArgs) { a.SignInDomain = "example.com" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchSandboxArgs()
			row.set(args)
			_, err := gcp.ResolveDispatch(args)
			require.ErrorContains(t, err, row.input)
		})
	}
	// The same inputs as a sandbox consumer sends them, which is what the mode is.
	_, err := gcp.ResolveDispatch(dispatchSandboxArgs())
	require.NoError(t, err)
}

// dispatchSandboxGroupInput is one input of a group a consumer sets together or not at all: its
// schema name, and what sets that input alone, holding a value a consumer would plausibly send.
type dispatchSandboxGroupInput struct {
	input string
	set   func(*gcp.DispatchArgs)
}

// dispatchSandboxTailnetGroup is the tailnet group: the name Dispatch is served at, the tags its
// front-door nodes carry, the key they join the tailnet with, the Tailscale Service they host, and
// the two images their pods run.
var dispatchSandboxTailnetGroup = []dispatchSandboxGroupInput{
	{"tailnetDnsName", func(a *gcp.DispatchArgs) { a.TailnetDnsName = "example-tailnet.ts.net" }},
	{"tags", func(a *gcp.DispatchArgs) { a.Tags = []string{"tag:example-dispatch"} }},
	{"tailnetAuthKey", func(a *gcp.DispatchArgs) { a.TailnetAuthKey = pulumi.String("tskey-auth-example-0123456789") }},
	{"tailnetService", func(a *gcp.DispatchArgs) { a.TailnetService = pulumi.String("svc:example-dispatch") }},
	{"tailscaleImage", func(a *gcp.DispatchArgs) { a.TailscaleImage = dispatchTailscaleImage }},
	{"frontDoorToolsImage", func(a *gcp.DispatchArgs) { a.FrontDoorToolsImage = dispatchToolsImage }},
}

// dispatchSandboxSignInGroup is the sign-in group: the Google client Dispatch hands a browser to,
// and the domain whose accounts it admits.
var dispatchSandboxSignInGroup = []dispatchSandboxGroupInput{
	{"signInClientId", func(a *gcp.DispatchArgs) { a.SignInClientId = "example-client-id" }},
	{"signInClientSecret", func(a *gcp.DispatchArgs) {
		a.SignInClientSecret = pulumi.String("example-client-secret")
	}},
	{"signInDomain", func(a *gcp.DispatchArgs) { a.SignInDomain = "example.com" }},
}

// setDispatchTailnetGroup sets the whole tailnet group, the Dispatch a consumer reaches over its
// tailnet, from the group's own list, so a tailnet input added later is set here too.
func setDispatchTailnetGroup(args *gcp.DispatchArgs) {
	for _, in := range dispatchSandboxTailnetGroup {
		in.set(args)
	}
}

// A group set in part is refused naming every input of that group still unset, so a consumer
// learns its whole shortfall in one run rather than setting one input, being refused for the next,
// and repeating that until the group is whole. The tailnet group is judged against a sandbox,
// where one of its inputs is what asks for a front door; the sign-in group against a whole tailnet
// group, since a sign-in input with no tailnet is refused for being sent at all
// (TestDispatchSandboxRefusesWhatItDoesNotBuild).
func TestDispatchPartialTailnetGroupNamesEveryMissingInput(t *testing.T) {
	for _, group := range []struct {
		name   string
		rest   func(*gcp.DispatchArgs)
		inputs []dispatchSandboxGroupInput
	}{
		{"tailnet", nil, dispatchSandboxTailnetGroup},
		{"sign-in", setDispatchTailnetGroup, dispatchSandboxSignInGroup},
	} {
		for _, in := range group.inputs {
			t.Run(group.name+" "+in.input, func(t *testing.T) {
				args := dispatchSandboxArgs()
				if group.rest != nil {
					group.rest(args)
				}
				in.set(args)
				_, err := gcp.ResolveDispatch(args)
				require.Error(t, err)
				for _, other := range group.inputs {
					if other.input != in.input {
						require.ErrorContains(t, err, other.input, "the refusal names %s, which is unset too", other.input)
					}
				}
			})
		}
	}
}
