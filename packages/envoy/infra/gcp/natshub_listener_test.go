package gcp_test

import (
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The three values of a hub listener's broker check: the secrets broker, the audience of the
// VM's own Google identity token, and the origin a client reaches this listener at.
const (
	brokerURL      = "https://example-broker.internal.example"
	brokerAudience = "example-broker-audience"
	listenerOrigin = "http://example-hub.example-tailnet.ts.net:9020"
)

// A hub without a listener registers no grant on a seed secret, and its group waits for the key's
// grant alone.
func TestNatsHubWithoutAListenerGrantsNoSeedSecret(t *testing.T) {
	args := exampleArgs()
	args.Listener = nil
	m, err := run(t, args, nil, true)
	require.NoError(t, err)

	var accessors []string
	for _, r := range m.Registrations() {
		if r.Type == accessorType {
			accessors = append(accessors, r.Name)
		}
	}
	require.Equal(t, []string{"example-hub-tailnet-auth-key-accessor"}, accessors)
	deps := m.One(t, groupType).RPC.GetDependencies()
	require.NotContains(t, strings.Join(deps, "\n"), "listener-seed-accessor")
}

// A hub that runs no listener serves no HTTP interface, so the tailnet DNS name is nobody's
// address there: the hub resolves whether the name is set, where its shape is still checked, or
// left out entirely.
func TestNatsHubWithoutAListenerNeedsNoTailnetDnsName(t *testing.T) {
	for _, row := range []struct{ name, tailnet string }{
		{"with a tailnet DNS name", "example-tailnet.ts.net"},
		{"without one", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			args.Listener, args.TailnetDnsName = nil, row.tailnet
			require.NoError(t, gcp.ResolveNatsHub(args))
		})
	}
}

// The resolver refuses every invalid listener or session user input, naming it.
func TestNatsHubResolverRefusesInvalidListenerAndSessionUserInputs(t *testing.T) {
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	requireResolverRefusals(t, []resolverRefusal{
		{
			name: "a listener image without a digest",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.Image = "example.invalid/envoy:latest" },
			err:  []string{`listener.image "example.invalid/envoy:latest"`, "@sha256:"},
		},
		{
			name: "a listener image with a space",
			edit: func(a *gcp.NatsHubArgs) {
				a.Listener.Image = "example.invalid/envoy --privileged x" + listenerImage[len("example.invalid/envoy"):]
			},
			err: []string{"listener.image"},
		},
		{
			name: "a listener seed secret that is a path",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.SeedSecret = "projects/example-project/secrets/seed" },
			err:  []string{`listener.seedSecret "projects/example-project/secrets/seed"`, "secret id"},
		},
		{
			name: "an empty listener seed secret",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.SeedSecret = "" },
			err:  []string{`listener.seedSecret ""`},
		},
		{
			name: "the tailnet auth key's secret as the listener's seed secret",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.SeedSecret = "example-hub-tailnet-auth-key" },
			err:  []string{`listener.seedSecret "example-hub-tailnet-auth-key"`, "tailnet auth key"},
		},
		{
			name: "a listener on a hub with no tailnet DNS name",
			edit: func(a *gcp.NatsHubArgs) { a.TailnetDnsName = "" },
			err:  []string{"tailnetDnsName is required", "its listener answers on"},
		},
		// The hub renders its own address as the first entry of the list the listener reads, but a
		// refusal names the list the operator wrote and the index in it: an operator told
		// allowedHosts[1] for their first entry would look for an entry they never wrote.
		{
			name: "a listener allowed host carrying a port",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.AllowedHosts = []string{"hub.example.com:9020"} },
			err:  []string{`listener.allowedHosts[0] is "hub.example.com:9020"`, "no port, scheme or path"},
		},
		{
			name: "a listener allowed host carrying a scheme",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.AllowedHosts = []string{"https://hub.example.com"} },
			err:  []string{`listener.allowedHosts[0] is "https://hub.example.com"`},
		},
		{
			name: "a wildcard listener allowed host, after one that is fine",
			edit: func(a *gcp.NatsHubArgs) {
				a.Listener.AllowedHosts = []string{"hub.example.com", "*.example.com"}
			},
			err: []string{`listener.allowedHosts[1] is "*.example.com"`},
		},
		{
			name: "a listener without a public key",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.NKey = "" },
			err:  []string{"listener.nkey is required"},
		},
		{
			name: "a listener whose public key is a seed",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.NKey = string(seed) },
			err:  []string{`listener "example-hub"`, "never a seed"},
		},
		{
			name: "a listener whose key a user holds",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.NKey = keyA },
			err:  []string{`listener "example-hub"`, `nkey repeats users[0] "example-host-a"`},
		},
		{
			name: "a user named after the hub, which the listener's user is",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Name = "example-hub" },
			err:  []string{`listener "example-hub"`, "name repeats users[1]"},
		},
		{
			name: "a session user without a public key",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.NKey = "" },
			err:  []string{"sessionUser.nkey is required"},
		},
		{
			name: "a session user whose public key is a seed",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.NKey = string(seed) },
			err:  []string{`sessionUser "example-sessions"`, "never a seed"},
		},
		{
			name: "a session user with an invalid name",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.Name = "Example Sessions" },
			err:  []string{`sessionUser "Example Sessions"`, "name"},
		},
		{
			name: "a session user whose key the listener holds",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.NKey = keyListener },
			err:  []string{`sessionUser "example-sessions"`, `nkey repeats listener "example-hub"`},
		},
		// The listener reads its broker check as one all-or-nothing set and refuses a partial
		// one when it boots, which the group would answer by recreating a VM that cannot
		// start, so the hub refuses it while Pulumi previews, naming the input left out and
		// the one that asks for it.
		{
			name: "a broker URL without an audience or an origin",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.AgentSecretsUrl = brokerURL },
			err:  []string{"listener.agentSecretsAudience is required when listener.agentSecretsUrl is set"},
		},
		{
			name: "a broker URL and an audience without an origin",
			edit: func(a *gcp.NatsHubArgs) {
				a.Listener.AgentSecretsUrl, a.Listener.AgentSecretsAudience = brokerURL, brokerAudience
			},
			err: []string{"listener.publicUrl is required when listener.agentSecretsUrl is set"},
		},
		{
			name: "an audience with no broker to present it to",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.AgentSecretsAudience = brokerAudience },
			err:  []string{"listener.agentSecretsUrl is required when listener.agentSecretsAudience is set"},
		},
		{
			name: "an origin with no broker",
			edit: func(a *gcp.NatsHubArgs) { a.Listener.PublicUrl = listenerOrigin },
			err:  []string{"listener.agentSecretsUrl is required when listener.publicUrl is set"},
		},
		// Each is rendered as one KEY=value argument of the listener's docker run command,
		// which a space would split, and the listener trims what it reads, so a value that is
		// whitespace alone is no value there.
		{
			name: "a broker URL carrying a space",
			edit: func(a *gcp.NatsHubArgs) {
				a.Listener.AgentSecretsUrl = brokerURL + " --privileged"
				a.Listener.AgentSecretsAudience, a.Listener.PublicUrl = brokerAudience, listenerOrigin
			},
			err: []string{"listener.agentSecretsUrl holds whitespace or a control character", "ENVOY_AGENT_SECRETS_URL"},
		},
		{
			name: "an origin carrying a line break",
			edit: func(a *gcp.NatsHubArgs) {
				a.Listener.AgentSecretsUrl, a.Listener.AgentSecretsAudience = brokerURL, brokerAudience
				a.Listener.PublicUrl = listenerOrigin + "\nENVEOF"
			},
			err: []string{"listener.publicUrl holds whitespace or a control character", "ENVOY_PUBLIC_URL"},
		},
	})
}

// A seed in the listener's image, seed secret or broker check, each of which the user data
// holds, is refused naming the input by its path, never the value, as
// TestNatsHubRefusesASeedInAnyPlainInput refuses one in the hub's other plain inputs.
func TestNatsHubRefusesASeedInTheListenersInputs(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	digest := "@sha256:" + strings.Repeat("0", 64)
	requireSeedRefusals(t, seed, []seedRefusal{
		{"a seed in the listener's image", "listener.image", func(a *gcp.NatsHubArgs) {
			a.Listener.Image = "example.invalid/" + lower + digest
		}},
		{"a seed as the listener's seed secret", "listener.seedSecret", func(a *gcp.NatsHubArgs) { a.Listener.SeedSecret = seed }},
		{"a listener seed secret split by a hyphen", "listener.seedSecret", func(a *gcp.NatsHubArgs) {
			a.Listener.SeedSecret = splitSeed(lower, "-")
		}},
		{"a seed as the listener's broker URL", "listener.agentSecretsUrl", func(a *gcp.NatsHubArgs) {
			a.Listener.AgentSecretsUrl = "https://" + strings.ToLower(seed) + ".internal.example"
			a.Listener.AgentSecretsAudience, a.Listener.PublicUrl = brokerAudience, listenerOrigin
		}},
		{"a seed as the listener's broker audience", "listener.agentSecretsAudience", func(a *gcp.NatsHubArgs) {
			a.Listener.AgentSecretsUrl, a.Listener.PublicUrl = brokerURL, listenerOrigin
			a.Listener.AgentSecretsAudience = seed
		}},
		{"a seed in the listener's own origin, split by a hyphen", "listener.publicUrl", func(a *gcp.NatsHubArgs) {
			a.Listener.AgentSecretsUrl, a.Listener.AgentSecretsAudience = brokerURL, brokerAudience
			a.Listener.PublicUrl = "http://" + splitSeed(lower, "-") + ".example-tailnet.ts.net:9020"
		}},
	})
}
