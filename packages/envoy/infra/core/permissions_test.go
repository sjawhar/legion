package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// listenerPublish is the listener's publish grant for example-host-a, written out by hand from
// Envoy's names as they stand: the notification stream's subjects, the role lanes, every exception
// lane, the stream and its durable, then each bucket in turn, then the interest bucket's purge.
var listenerPublish = []string{
	"notifications.agent.>",
	"notifications.dispatch.>",
	"notifications.github.>",
	"notifications.slack.>",
	"notifications.legion.>",
	"notifications.ghostwispr.>",
	"notifications.whatsapp.>",
	"notifications.envoy.exceptions.notifications.agent.>",
	"notifications.role.>",
	"notifications.envoy.exceptions.notifications.>",
	"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
	"$JS.API.STREAM.CREATE.ENVOY_NOTIFICATIONS",
	"$JS.API.STREAM.UPDATE.ENVOY_NOTIFICATIONS",
	"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.listener-example-host-a",
	"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-example-host-a",
	"$JS.ACK.ENVOY_NOTIFICATIONS.listener-example-host-a.>",
	"$JS.API.INFO",
	"$JS.API.STREAM.INFO.KV_envoy_interests",
	"$JS.API.STREAM.CREATE.KV_envoy_interests",
	"$JS.API.DIRECT.GET.KV_envoy_interests.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_interests",
	"$KV.envoy_interests.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_interests.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_interests.>",
	"$JS.FC.KV_envoy_interests.>",
	"$JS.API.STREAM.INFO.KV_envoy_roles",
	"$JS.API.STREAM.CREATE.KV_envoy_roles",
	"$JS.API.DIRECT.GET.KV_envoy_roles.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_roles",
	"$KV.envoy_roles.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_roles.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_roles.>",
	"$JS.FC.KV_envoy_roles.>",
	"$JS.API.STREAM.INFO.KV_envoy_sessions",
	"$JS.API.STREAM.CREATE.KV_envoy_sessions",
	"$JS.API.DIRECT.GET.KV_envoy_sessions.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_sessions",
	"$KV.envoy_sessions.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_sessions.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_sessions.>",
	"$JS.FC.KV_envoy_sessions.>",
	"$JS.API.STREAM.INFO.KV_envoy_ci_state",
	"$JS.API.STREAM.CREATE.KV_envoy_ci_state",
	"$JS.API.DIRECT.GET.KV_envoy_ci_state.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_ci_state",
	"$KV.envoy_ci_state.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_ci_state.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_ci_state.>",
	"$JS.FC.KV_envoy_ci_state.>",
	"$JS.API.STREAM.PURGE.KV_envoy_interests",
}

func TestListenerPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish},
		Subscribe: core.Rules{Allow: []string{"_INBOX.>", "notifications.role.> envoy-listener-example-host-a"}},
	}, core.ListenerPermissions("example-host-a"))
}

// A space in the machine id is a hyphen in the durable and the queue, as the listener names them.
func TestListenerPermissionsNameTheDurableAndQueueOfAMachineIDWithASpace(t *testing.T) {
	p := core.ListenerPermissions("example host")
	require.Contains(t, p.Publish.Allow, "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-example-host")
	require.Contains(t, p.Publish.Allow, "$JS.ACK.ENVOY_NOTIFICATIONS.listener-example-host.>")
	require.Contains(t, p.Subscribe.Allow, "notifications.role.> envoy-listener-example-host")
}

func TestSessionPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: []string{"_INBOX.>", "agentstream.*.frames"}},
		Subscribe: core.Rules{Allow: []string{"notifications.>", "agentstream.*.control"}},
	}, core.SessionPermissions())
}

func TestHostPermissionsAreTheListenersThenTheSessions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: append(append([]string{}, listenerPublish...), "_INBOX.>", "agentstream.*.frames")},
		Subscribe: core.Rules{Allow: []string{
			"_INBOX.>", "notifications.role.> envoy-listener-example-host-a",
			"notifications.>", "agentstream.*.control",
		}},
	}, core.HostPermissions("example-host-a"))
}

// nats-server takes a repeated entry, so a duplicate would be noise in every rendered block; none of
// the sets holds one, the union included.
func TestNoSetRepeatsAnEntry(t *testing.T) {
	sets := map[string]core.Permissions{
		"listener": core.ListenerPermissions("example-host-a"),
		"session":  core.SessionPermissions(),
		"host":     core.HostPermissions("example-host-a"),
	}
	for name, p := range sets {
		for direction, allow := range map[string][]string{"publish": p.Publish.Allow, "subscribe": p.Subscribe.Allow} {
			seen := map[string]bool{}
			for _, entry := range allow {
				require.False(t, seen[entry], "%s %s repeats %q", name, direction, entry)
				seen[entry] = true
			}
		}
	}
}

// Each call returns lists of its own, so a caller that edits one, as a consumer narrowing a grant
// does, changes no later call's.
func TestASetIsTheCallersToChange(t *testing.T) {
	first := core.HostPermissions("example-host-a")
	first.Publish.Allow[0] = "edited"
	first.Subscribe.Allow[0] = "edited"
	second := core.HostPermissions("example-host-a")
	require.Equal(t, "notifications.agent.>", second.Publish.Allow[0])
	require.Equal(t, "_INBOX.>", second.Subscribe.Allow[0])
	session := core.SessionPermissions()
	session.Publish.Allow[0] = "edited"
	require.Equal(t, "_INBOX.>", core.SessionPermissions().Publish.Allow[0])
}

// A set drops into a User as its rules, and the authorization renders it.
func TestASetIsAUsersRules(t *testing.T) {
	p := core.HostPermissions("example-host-a")
	a := core.Authorization{Users: []core.User{{
		Name:      "example-host-a",
		NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
		Publish:   &p.Publish,
		Subscribe: &p.Subscribe,
		HasSeed:   true,
	}}}
	_, err := a.Render(nil)
	require.NoError(t, err)
}
