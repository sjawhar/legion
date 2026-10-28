package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	randomPasswordType = "random:index/randomPassword:RandomPassword"
	kubeSecretType     = "kubernetes:core/v1:Secret"
	childURN           = "urn:pulumi:s::p::" + dispatchType + "$"
)

// registeredResource is what a preview compares of one registration: its inputs, parent,
// provider, dependencies and property dependencies, each list sorted, since the SDK resolves them
// concurrently.
type registeredResource struct {
	Inputs               resource.PropertyMap
	Parent, Provider     string
	Dependencies         []string
	PropertyDependencies map[string][]string
	ReadID               string
}

// registrationsByName is every registration of m keyed by its type and name, each once.
func registrationsByName(t *testing.T, m *pulumitest.Mocks) map[string]registeredResource {
	t.Helper()
	out := map[string]registeredResource{}
	for _, r := range m.Registrations() {
		key := r.Type + "::" + r.Name
		require.NotContains(t, out, key, "%s registers twice", key)
		entry := registeredResource{Inputs: r.Inputs}
		if r.RPC != nil {
			entry.Parent, entry.Provider = r.RPC.GetParent(), r.RPC.GetProvider()
			entry.Dependencies = slices.Sorted(slices.Values(r.RPC.GetDependencies()))
			entry.PropertyDependencies = map[string][]string{}
			for property, deps := range r.RPC.GetPropertyDependencies() {
				entry.PropertyDependencies[property] = slices.Sorted(slices.Values(deps.GetUrns()))
			}
		} else {
			entry.ReadID = r.ID
		}
		out[key] = entry
	}
	return out
}

// agentTokenPasswords is the names of the RandomPasswords holding an agent token.
func agentTokenPasswords(m *pulumitest.Mocks) []string {
	var names []string
	for _, r := range m.Registrations() {
		if r.Type == randomPasswordType && strings.HasPrefix(r.Name, "example-dispatch-agent-token") {
			names = append(names, r.Name)
		}
	}
	return names
}

// dispatchAgentTokenValue is DISPATCH_AGENT_TOKEN as the <hostname>-dispatch Secret holds it.
func dispatchAgentTokenValue(t *testing.T, m *pulumitest.Mocks) string {
	t.Helper()
	return unwrappedProperty(dispatchSecretData(t, m)["DISPATCH_AGENT_TOKEN"]).StringValue()
}

// dispatchSecretPasswords is the agent-token RandomPasswords the <hostname>-dispatch Secret's
// stringData depends on, so the Secret is written only once each exists.
func dispatchSecretPasswords(t *testing.T, m *pulumitest.Mocks) []string {
	t.Helper()
	var names []string
	for _, urn := range m.Named(t, kubeSecretType, "example-dispatch-dispatch").RPC.GetPropertyDependencies()["stringData"].GetUrns() {
		if name, ok := strings.CutPrefix(urn, childURN+randomPasswordType+"::"); ok && strings.HasPrefix(name, "example-dispatch-agent-token") {
			names = append(names, name)
		}
	}
	return names
}

// A stack that sets neither input, or sets them to their defaults, registers what the component
// registered before rotation existed: the one RandomPassword <hostname>-agent-token with the same
// arguments and no keepers, and DISPATCH_AGENT_TOKEN its result alone. Every registration's
// inputs, parent, provider, dependencies and property dependencies are equal between the unset
// and the explicit defaults, so a preview of either against the other's state shows no change.
// (The same snapshot, taken of the component before these inputs existed, matched byte for byte;
// the pod goldens pin every pod template.)
func TestDispatchAgentTokenUnsetRendersWhatItAlwaysDid(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	require.Equal(t, []string{"example-dispatch-agent-token"}, agentTokenPasswords(defaults))
	password := defaults.Named(t, randomPasswordType, "example-dispatch-agent-token")
	require.Equal(t, resource.PropertyMap{"length": resource.NewProperty(48.0), "special": resource.NewProperty(false)}, password.Inputs,
		"the token's arguments are the ones it was created with: any change would replace it")
	require.Equal(t, "example-dispatch-agent-token-result", dispatchAgentTokenValue(t, defaults))
	require.Equal(t, []string{"example-dispatch-agent-token"}, dispatchSecretPasswords(t, defaults))
	requirePodGolden(t, defaults, "example-dispatch", "dispatch-pod.golden")

	settings, err := gcp.ResolveDispatch(dispatchArgs())
	require.NoError(t, err)
	require.Zero(t, settings.AgentTokenGeneration)
	require.False(t, settings.KeepPreviousAgentToken)

	explicit := dispatchArgs()
	explicit.AgentTokenGeneration = pulumi.IntRef(0)
	explicit.KeepPreviousAgentToken = pulumi.BoolRef(false)
	m, err := runDispatch(t, explicit, true)
	require.NoError(t, err)
	require.Equal(t, registrationsByName(t, defaults), registrationsByName(t, m))
}

// Each state of a rotation, as the README's procedure walks through it: the generation raised
// with the previous token kept, then the previous cleared, by leaving keepPreviousAgentToken unset
// or setting it false. The current generation's RandomPassword is registered, and the previous one
// only while it is kept, each with the arguments generation 0 always had, so a kept token is the
// very resource the stack's state holds; DISPATCH_AGENT_TOKEN is the current token, then the kept
// one, and the Secret waits for both. Every other registration, every pod template among them, is
// the default's: the app rolls onto the new tokens through the Secret's checksum, which the
// Secret's replacement changes (TestDispatchPodsRollWhenTheSecretsTheyReadChange and the README).
func TestDispatchAgentTokenRotationStates(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	defaultArguments := defaults.Named(t, randomPasswordType, "example-dispatch-agent-token").Inputs
	before := registrationsByName(t, defaults)

	for _, row := range []struct {
		name       string
		generation int
		keep       *bool
		passwords  []string
		token      string
	}{
		{
			name: "generation 1 keeping generation 0", generation: 1, keep: pulumi.BoolRef(true),
			passwords: []string{"example-dispatch-agent-token", "example-dispatch-agent-token-1"},
			token:     "example-dispatch-agent-token-1-result example-dispatch-agent-token-result",
		},
		{
			name: "generation 1 with generation 0 cleared (unset)", generation: 1,
			passwords: []string{"example-dispatch-agent-token-1"},
			token:     "example-dispatch-agent-token-1-result",
		},
		{
			name: "generation 1 with generation 0 cleared (false)", generation: 1, keep: pulumi.BoolRef(false),
			passwords: []string{"example-dispatch-agent-token-1"},
			token:     "example-dispatch-agent-token-1-result",
		},
		{
			name: "generation 2 keeping generation 1", generation: 2, keep: pulumi.BoolRef(true),
			passwords: []string{"example-dispatch-agent-token-1", "example-dispatch-agent-token-2"},
			token:     "example-dispatch-agent-token-2-result example-dispatch-agent-token-1-result",
		},
		{
			name: "generation 2 with generation 1 cleared", generation: 2,
			passwords: []string{"example-dispatch-agent-token-2"},
			token:     "example-dispatch-agent-token-2-result",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.AgentTokenGeneration = pulumi.IntRef(row.generation)
			args.KeepPreviousAgentToken = row.keep
			settings, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
			require.Equal(t, row.generation, settings.AgentTokenGeneration)
			require.Equal(t, row.keep != nil && *row.keep, settings.KeepPreviousAgentToken)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)

			require.ElementsMatch(t, row.passwords, agentTokenPasswords(m))
			for _, name := range row.passwords {
				require.Equal(t, defaultArguments, m.Named(t, randomPasswordType, name).Inputs, name)
			}
			require.Equal(t, row.token, dispatchAgentTokenValue(t, m))
			require.ElementsMatch(t, row.passwords, dispatchSecretPasswords(t, m), "the Secret is written once every token it holds exists")

			after := registrationsByName(t, m)
			for key := range before {
				if strings.HasPrefix(key, randomPasswordType+"::example-dispatch-agent-token") || key == kubeSecretType+"::example-dispatch-dispatch" {
					continue
				}
				require.Equal(t, before[key], after[key], key)
			}
			require.Len(t, after, len(before)-1+len(row.passwords), "only agent tokens are added or dropped")
		})
	}
}
