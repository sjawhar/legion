package core_test

import (
	"encoding/json"
	"slices"
	"strings"
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

// A user holding SessionPermissions alone publishes a reply and never subscribes to a reply inbox,
// so it may answer a request but not make one: sessions that share one user holding the set would
// otherwise each receive every other session's replies. The rules are read from the rendered block,
// as the server reads them. A host's user subscribes to the inboxes for its listener, which the same
// check finds.
func TestSessionPermissionsRefuseASubscribeToReplyInboxes(t *testing.T) {
	publish, subscribe := renderedRules(t, core.SessionPermissions())
	require.Contains(t, publish, "_INBOX.>", "a session publishes the receipt a role delivery waits for")
	for _, entry := range subscribe {
		require.False(t, admitsInbox(entry), "the session's subscribe entry %q admits a reply inbox", entry)
	}
	_, host := renderedRules(t, core.HostPermissions("example-host-a"))
	require.True(t, slices.ContainsFunc(host, admitsInbox), "the host's set subscribes to the inboxes, for its listener")
}

// renderedRules renders one user holding p and returns the publish and subscribe allow lists its
// line of the block holds.
func renderedRules(t *testing.T, p core.Permissions) (publish, subscribe []string) {
	t.Helper()
	const key = "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO"
	text, err := core.Authorization{Users: []core.User{{
		Name: "example-session", NKey: key, Publish: &p.Publish, Subscribe: &p.Subscribe, HasSeed: true,
	}}}.Render(nil)
	require.NoError(t, err)
	var user struct {
		Permissions struct {
			Publish, Subscribe struct{ Allow []string }
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `"nkey": "`+key+`"`) {
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(line)), &user), line)
			return user.Permissions.Publish.Allow, user.Permissions.Subscribe.Allow
		}
	}
	t.Fatalf("the rendered block holds no line for the user:\n%s", text)
	return nil, nil
}

// admitsInbox reports whether a subscribe allow entry, a subject pattern and an optional queue
// group after a space, admits any subject under _INBOX.: its first token is _INBOX, * or >.
func admitsInbox(entry string) bool {
	first, _, _ := strings.Cut(strings.Fields(entry)[0], ".")
	return first == "_INBOX" || first == "*" || first == ">"
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
		"daemon":   core.DaemonPermissions(),
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
	daemon := core.DaemonPermissions()
	daemon.Publish.Allow[0] = "edited"
	daemon.Subscribe.Allow[0] = "edited"
	require.Equal(t, "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", core.DaemonPermissions().Publish.Allow[0])
	require.Equal(t, "notifications.envoy.exceptions.notifications.role.>", core.DaemonPermissions().Subscribe.Allow[0])
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

// The expected daemon grant is written out literally rather than built from Envoy's subject names,
// so a change to the grant or to the names it is built from fails here until reviewed.
func TestDaemonPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: []string{
			"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
			"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>",
			"$JS.ACK.ENVOY_NOTIFICATIONS.>",
		}},
		Subscribe: core.Rules{Allow: []string{
			"notifications.envoy.exceptions.notifications.role.>",
			"_INBOX.>",
		}},
	}, core.DaemonPermissions())
}
