package gcp_test

import (
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
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
	})
}

// A seed in the listener's image or seed secret, which the user data holds, is refused naming the
// input by its path, never the value, as TestNatsHubRefusesASeedInAnyPlainInput refuses one in the
// hub's other plain inputs.
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
	})
}
