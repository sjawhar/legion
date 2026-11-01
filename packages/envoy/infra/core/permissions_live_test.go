//go:build natslive

package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The live tests of the host grant: one nkey user holding HostPermissions, on the 2.10 image and on
// the hub's, each run with the hub's own server arguments. This branch's listener connects as that
// user with the environment host:Listener gives it, subscribeAllTopics on, since this is the one
// grant that allows it, and a session connects as the same user, as every session on a host does.

const (
	liveMachineID = "example-host-a"
	liveSession   = "s1"
	// liveServingSession is a session the listener reaches by POSTing to its serving port rather
	// than over NATS.
	liveServingSession = "s-serving"
	liveRole           = "example-role"
	// liveHealthWindow is how long the listener has to answer "healthy" (its durable bound under
	// the grant), and how long a negative control watches one that must not.
	liveHealthWindow = 60 * time.Second
)

// hubServerArgs are the arguments the GCP hub runs nats-server with.
var hubServerArgs = []string{"-js", "--store_dir", "/data", "-m", "8222"}

// serverViolation matches a permissions violation nats-server logs, capturing its kind (Publish or
// Subscription) and the subject it names.
var serverViolation = regexp.MustCompile(`(Publish|Subscription) Violation - .*?Subject "([^"]*)"`)

// natsServer is a NATS test container, read for what its server logged.
type natsServer interface {
	Logs(context.Context) (io.ReadCloser, error)
}

func TestHostPermissionsOnRealServers(t *testing.T) {
	listener := buildEnvoyCommand(t, "listener")
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			t.Run("the listener reaches healthy and a session as the same user receives", func(t *testing.T) {
				t.Parallel()
				seed, server, uri := startGrantServer(t, image.Ref, core.HostPermissions(liveMachineID))
				held := cmdtest.HoldPort(t)
				port := held.Port
				// The opt-in has the listener also subscribe to notifications.>, which this grant
				// allows: the check below that the server refused nothing it was not provoked to
				// covers that subscription too.
				process := startListenerConfig(t, listener, uri, seed, held, core.ListenerConfig{
					MachineID: liveMachineID, HostBridge: "127.0.0.1", SubscribeAllTopics: true,
				})
				t.Logf("healthy /healthz body: %s", requireHealthy(t, process, port, liveHealthWindow, listenerHealthy))

				conn, errs := connect(t, uri, seed)
				received, err := conn.SubscribeSync(contracts.AgentSubject(liveSession))
				require.NoError(t, err)
				_, err = conn.SubscribeSync(contracts.AgentStreamControlSubject(liveSession))
				require.NoError(t, err)
				require.NoError(t, conn.Publish(contracts.AgentStreamFramesSubject(liveSession), []byte(`[]`)))
				require.NoError(t, conn.Flush())

				postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+liveSession+`","topics":[],"self_subscribed":true}`)
				postListener(t, process, port, "/v1/messages/send", `{"target_session":"`+liveSession+`","message":"example message"}`)
				sent, _ := nextEnvelope(t, process, received)
				require.Equal(t, contracts.AgentSubject(liveSession), sent.Topic)
				require.Equal(t, "example message", sent.PayloadSummary)

				// A role message reaches its holder from the listener's publish route, which answers
				// once the session returned the receipt the route waits for.
				postListener(t, process, port, "/v1/roles/set", `{"session_id":"`+liveSession+`","role":"`+liveRole+`"}`)
				forwarded := sendRoleMessage(t, process, port, received, `{"topic":"`+contracts.RoleTopicPrefix+liveRole+`","message":"example role message"}`)
				require.Equal(t, contracts.RoleTopicPrefix+liveRole, forwarded.Topic)

				// A topic outside every stream subject reaches the session subscribed to it.
				adhoc, err := conn.SubscribeSync("notifications.team.>")
				require.NoError(t, err)
				require.NoError(t, conn.Flush())
				postListener(t, process, port, "/v1/messages/publish", `{"topic":"notifications.team.deploy-squad","message":"example ad-hoc message"}`)
				team, _ := nextEnvelope(t, process, adhoc)
				require.Equal(t, "notifications.team.deploy-squad", team.Topic)

				// The same topic, published by a session over its own connection, which the listener's
				// /v1 never sees, reaches a session served on this machine through the listener's
				// subscription to every notification topic. The host's set holds no session
				// consumer's call, so the registration is answered core, reason no_grant, and the
				// serving session is pushed from the listener's own durable, as before consumer
				// delivery.
				serving := startServingPort(t, liveServingSession)
				answer := postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+liveServingSession+
					`","topics":["notifications.team.deploy-squad"],"port":`+strconv.Itoa(serving.port)+`,"host":"127.0.0.1"}`)
				require.Contains(t, answer, `"mode":"core"`, "the registration's answer: %s", answer)
				require.Contains(t, answer, `"reason":"no_grant"`, "the registration's answer: %s", answer)
				item := contracts.Envelope{
					EventID:        "evt-live-topics",
					Source:         "agent",
					SourceEventID:  "evt-live-topics",
					SourceSession:  liveSession,
					Topic:          "notifications.team.deploy-squad",
					DedupeKey:      "permissions-live-test.evt-live-topics",
					IssuedAt:       contracts.NowMillis(),
					PayloadSummary: "example message from another session",
					Payload:        "example message from another session",
					TraceID:        "trace-evt-live-topics",
				}
				data, err := json.Marshal(item)
				require.NoError(t, err)
				require.NoError(t, conn.Publish(item.Topic, data))
				require.NoError(t, conn.Flush())
				serving.requirePrompt(t, process, item)

				// Every allowed operation came first, so a violation for one would be the first error.
				require.NoError(t, conn.Publish("outside.example", nil))
				requireViolation(t, conn, errs, `Publish to "outside.example"`)
				// The server refused nothing else either, the listener's own operations included,
				// beside the session consumers' calls this set does not hold, which the listener
				// probes once an episode (its consumer list when it gains the durable, and the
				// registration's consumer info). Its log names the refusal just made before that
				// reading counts: one that found no violation at all would pass for the wrong
				// reason.
				for _, v := range violationsOnceLogged(t, server, "outside.example") {
					require.True(t, v[2] == "outside.example" || sessionConsumerCall(v[2]),
						"the server refused what the grant should allow: %s", v[0])
				}
			})
			t.Run("without its durable's consumer create the listener never reaches healthy", func(t *testing.T) {
				t.Parallel()
				// The host's set holds the durable's own create and no session consumer's
				// wildcard, so removing that one entry leaves the durable uncreatable.
				const removed = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-example-host-a"
				grant := core.HostPermissions(liveMachineID)
				held := len(grant.Publish.Allow)
				grant.Publish.Allow = slices.DeleteFunc(grant.Publish.Allow, func(entry string) bool { return entry == removed })
				require.Len(t, grant.Publish.Allow, held-1, "the grant does not hold %s", removed)

				seed, server, uri := startGrantServer(t, image.Ref, grant)
				process, port := startListener(t, listener, uri, seed)
				requireNeverHealthy(t, port)
				// The removed subject is what the server refused, and all it refused: the listener
				// stopped short of healthy for the grant's reason.
				logged := violationsOnceLogged(t, server, removed)
				for _, v := range logged {
					require.Equal(t, []string{"Publish", removed}, v[1:], "the server refused something else too: %s", v[0])
				}
				t.Logf("server violation (%d alike): %s", len(logged), logged[0][0])
				require.Contains(t, process.Output.String(), "subscribe failed, retrying")
			})
		})
	}
}

// The grants a hub with a listener of its own renders: the listener connects as one user holding
// ListenerPermissions, and a session presents no credential and becomes the session user, holding
// SessionPermissions as the server's no_auth_user. The listener reaches healthy, the session
// registers through it and receives a send, answers a role message's receipt, receives a publish to
// an ad-hoc topic it subscribes to on NATS, reads back through /v1/replay a send stored while it had
// no subscription open, and is refused a publish its grant does not hold.
func TestListenerAndCredentialFreeSessionsOnRealServers(t *testing.T) {
	listener := buildEnvoyCommand(t, "listener")
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			seed, server, uri := startSplitServer(t, image.Ref)
			process, port := startListener(t, listener, uri, seed)
			t.Logf("healthy /healthz body: %s", requireHealthy(t, process, port, liveHealthWindow, listenerHealthy))

			conn, errs := connect(t, uri, "")
			received, err := conn.SubscribeSync(contracts.AgentSubject(liveSession))
			require.NoError(t, err)
			_, err = conn.SubscribeSync(contracts.AgentStreamControlSubject(liveSession))
			require.NoError(t, err)
			require.NoError(t, conn.Publish(contracts.AgentStreamFramesSubject(liveSession), []byte(`[]`)))
			require.NoError(t, conn.Flush())

			postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+liveSession+`","topics":[],"self_subscribed":true}`)
			postListener(t, process, port, "/v1/messages/send", `{"target_session":"`+liveSession+`","message":"example message"}`)
			sent, _ := nextEnvelope(t, process, received)
			require.Equal(t, contracts.AgentSubject(liveSession), sent.Topic)
			require.Equal(t, "example message", sent.PayloadSummary)

			postListener(t, process, port, "/v1/roles/set", `{"session_id":"`+liveSession+`","role":"`+liveRole+`"}`)
			forwarded := sendRoleMessage(t, process, port, received, `{"topic":"`+contracts.RoleTopicPrefix+liveRole+`","message":"example role message"}`)
			require.Equal(t, contracts.RoleTopicPrefix+liveRole, forwarded.Topic)

			// A topic outside every stream subject, as an agent names one for its team, reaches the
			// session subscribed to it: the listener publishes it over core NATS, under its grant.
			adhoc, err := conn.SubscribeSync("notifications.team.>")
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			postListener(t, process, port, "/v1/messages/publish", `{"topic":"notifications.team.deploy-squad","message":"example ad-hoc message"}`)
			team, _ := nextEnvelope(t, process, adhoc)
			require.Equal(t, "notifications.team.deploy-squad", team.Topic)
			require.Equal(t, "example ad-hoc message", team.PayloadSummary)

			// A send to a registered session with no subscription open is stored, and the listener's
			// replay reads it back with its grant's stored-message get: a first call without after
			// starts the cursor at the stream's last sequence, and a call after that cursor returns
			// the send.
			const offline = "s3"
			offlineSubject := contracts.AgentSubject(offline)
			postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+offline+`","topics":[],"self_subscribed":true}`)
			cursor := replaySubject(t, process, port, offline, offlineSubject, nil)
			require.Empty(t, cursor.Refused)
			require.Empty(t, cursor.Messages)
			postListener(t, process, port, "/v1/messages/send", `{"target_session":"`+offline+`","message":"example missed message"}`)
			missed := replaySubject(t, process, port, offline, offlineSubject, &cursor.NextAfter)
			require.Empty(t, missed.Refused)
			require.False(t, missed.More)
			require.Len(t, missed.Messages, 1, "the replay after %d returned another count", cursor.NextAfter)
			require.Greater(t, missed.Messages[0].Seq, cursor.NextAfter)
			var replayed contracts.Envelope
			require.NoError(t, json.Unmarshal([]byte(missed.Messages[0].Data), &replayed))
			require.Equal(t, offlineSubject, replayed.Topic)
			require.Equal(t, "example missed message", replayed.PayloadSummary)

			// A session may not publish to another session's inbox directly; only the listener does.
			other := contracts.AgentSubject("s2")
			require.NoError(t, conn.Publish(other, nil))
			requireViolation(t, conn, errs, `Publish to "`+other+`"`)
			for _, v := range violationsOnceLogged(t, server, other) {
				require.Equal(t, other, v[2], "the server refused what a grant should allow: %s", v[0])
			}
		})
	}
}

// The hub's grants with the opt-in on: the listener, holding ListenerPermissions, may subscribe
// only to the role lanes and the reply inboxes, so the server refuses its subscription to
// notifications.>, and the listener ends its start, exit 1, with an ERROR naming
// ENVOY_SUBSCRIBE_ALL_TOPICS, rather than run delivering none of the topics it was told to. That
// subscription is all the server refused. This is why gcp:NatsHub's and gcp:Dispatch's listeners,
// whose grants are this one and one built on it, never set the opt-in.
func TestListenerPermissionsRefuseTheSubscriptionToEveryTopicOnRealServers(t *testing.T) {
	listener := buildEnvoyCommand(t, "listener")
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			seed, server, uri := startSplitServer(t, image.Ref)
			held := cmdtest.HoldPort(t)
			process := startListenerConfig(t, listener, uri, seed, held, core.ListenerConfig{
				MachineID: liveMachineID, SubscribeAllTopics: true,
			})
			process.WaitExit(t, "its start")
			output := process.Output.String()
			require.Equal(t, 1, process.Cmd.ProcessState.ExitCode(), output)
			require.True(t, slices.ContainsFunc(strings.Split(output, "\n"), func(line string) bool {
				var record map[string]any
				return json.Unmarshal([]byte(line), &record) == nil && record["level"] == "ERROR" &&
					strings.Contains(fmt.Sprint(record["msg"]), "ENVOY_SUBSCRIBE_ALL_TOPICS")
			}), "no ERROR names ENVOY_SUBSCRIBE_ALL_TOPICS:\n%s", output)

			logged := violationsOnceLogged(t, server, "notifications.>")
			require.Len(t, logged, 1, "the server refused more than the subscription: %q", logged)
			require.Equal(t, []string{"Subscription", "notifications.>"}, logged[0][1:], logged[0][0])
		})
	}
}

// The grant of a listener's own NATS user, which no session shares (ListenerHostPermissions, a
// gcp:NatsHub user with listenerMachineId): the one set under which a listener both keeps its
// sessions' consumers and runs with ENVOY_SUBSCRIBE_ALL_TOPICS on. The listener reaches healthy,
// answers a registration consumer rather than core, relays a stored send to the session on its
// relay subject, and delivers to a session served on its machine a topic the stream does not
// store, which only the subscription to every notification topic carries. The server refused
// nothing but the publish provoked at the end: under this set, unlike the host's, no session
// consumer's call is refused.
func TestListenerHostPermissionsOnRealServers(t *testing.T) {
	listener := buildEnvoyCommand(t, "listener")
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			seed, server, uri := startSplitGrantServer(t, image.Ref, core.ListenerHostPermissions(liveMachineID))
			held := cmdtest.HoldPort(t)
			port := held.Port
			process := startListenerConfig(t, listener, uri, seed, held, core.ListenerConfig{
				MachineID: liveMachineID, HostBridge: "127.0.0.1", SubscribeAllTopics: true,
			})
			t.Logf("healthy /healthz body: %s", requireHealthy(t, process, port, liveHealthWindow, listenerHealthy))

			// The session connects as the credential-free session user, as a session on another
			// machine does, and reads its relay subject, which consumer delivery sends it on.
			conn, errs := connect(t, uri, "")
			relayed, err := conn.SubscribeSync(bus.DeliverSubject(liveSession))
			require.NoError(t, err)
			require.NoError(t, conn.Flush())

			answer := postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+liveSession+
				`","topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"live-epoch"}}`)
			require.Contains(t, answer, `"mode":"consumer"`, "the registration's answer: %s", answer)
			require.Contains(t, answer, `"subject":"`+bus.DeliverSubject(liveSession)+`"`, "the registration's answer: %s", answer)

			postListener(t, process, port, "/v1/messages/send", `{"target_session":"`+liveSession+`","message":"example message"}`)
			sent, frame := nextEnvelope(t, process, relayed)
			require.NotEmpty(t, frame.Reply, "the relay frame carries no subject for its receipt")
			require.NoError(t, frame.Respond(nil))
			require.Equal(t, contracts.AgentSubject(liveSession), sent.Topic)
			require.Equal(t, "example message", sent.PayloadSummary)

			// A topic the stream does not store, published over core NATS by another program
			// holding this user, reaches a session served on this machine through the listener's
			// subscription to every notification topic, which this set allows and
			// ListenerPermissions does not. No session publishes it: the session user's grant
			// holds no notification topic.
			serving := startServingPort(t, liveServingSession)
			postListener(t, process, port, "/v1/interests/subscribe", `{"session_id":"`+liveServingSession+
				`","topics":["notifications.team.deploy-squad"],"port":`+strconv.Itoa(serving.port)+`,"host":"127.0.0.1"}`)
			item := contracts.Envelope{
				EventID:        "evt-live-listener-host",
				Source:         "agent",
				SourceEventID:  "evt-live-listener-host",
				SourceSession:  liveSession,
				Topic:          "notifications.team.deploy-squad",
				DedupeKey:      "permissions-live-test.evt-live-listener-host",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "example message from another session",
				Payload:        "example message from another session",
				TraceID:        "trace-evt-live-listener-host",
			}
			data, err := json.Marshal(item)
			require.NoError(t, err)
			publisher, _ := connect(t, uri, seed)
			require.NoError(t, publisher.Publish(item.Topic, data))
			require.NoError(t, publisher.Flush())
			serving.requirePrompt(t, process, item)

			// Every allowed operation came first, so a violation for one would be the first error.
			// A session may not publish to another session's inbox directly.
			other := contracts.AgentSubject("s2")
			require.NoError(t, conn.Publish(other, nil))
			requireViolation(t, conn, errs, `Publish to "`+other+`"`)
			for _, v := range violationsOnceLogged(t, server, other) {
				require.Equal(t, other, v[2], "the server refused what the grant should allow: %s", v[0])
			}
		})
	}
}

// A user holding SessionPermissions alone, as sessions that share one user hold it, receives its
// notifications and publishes a reply, and the server refuses it a subscribe to the reply inboxes
// and a JetStream API request, on the 2.10 image and on the hub's: request/reply from such a
// session is refused by design, and it reads missed notifications only through the listener.
func TestSessionPermissionsOnRealServers(t *testing.T) {
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			seed, server, uri := startGrantServer(t, image.Ref, core.SessionPermissions())
			conn, errs := connect(t, uri, seed)
			_, err := conn.SubscribeSync(contracts.AgentSubject(liveSession))
			require.NoError(t, err)
			require.NoError(t, conn.Publish(natsgo.InboxPrefix+"example.1", nil))
			_, err = conn.SubscribeSync(natsgo.InboxPrefix + ">")
			require.NoError(t, err)
			requireViolation(t, conn, errs, `Subscription to "_INBOX.>"`)
			// The inbox subscribe is all the server refused: the notification subscribe and the reply
			// came first and were allowed.
			for _, v := range violationsOnceLogged(t, server, "_INBOX.>") {
				require.Equal(t, []string{"Subscription", "_INBOX.>"}, v[1:], "the server refused what the set allows: %s", v[0])
			}
			// No JetStream API call is the session's: the stored-message get the listener's replay
			// makes is refused it, so a session reads what it missed only through the listener.
			const replayRead = "$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS"
			require.NoError(t, conn.Publish(replayRead, nil))
			requireViolation(t, conn, errs, `Publish to "`+replayRead+`"`)
			refused := [][]string{{"Subscription", "_INBOX.>"}, {"Publish", replayRead}}
			for _, v := range violationsOnceLogged(t, server, replayRead) {
				require.Contains(t, refused, v[1:], "the server refused what the set allows: %s", v[0])
			}
		})
	}
}

// startSplitServer runs image with two users: the listener's, holding ListenerPermissions for
// liveMachineID with a seed, and the session user, holding SessionPermissions with no seed, as the
// server's no_auth_user. It returns the listener's seed, the server and its client URL.
func startSplitServer(t *testing.T, image string) (string, natsServer, string) {
	t.Helper()
	return startSplitGrantServer(t, image, core.ListenerPermissions(liveMachineID))
}

// startSplitGrantServer runs image with two users: the listener's, holding grant with a seed, and
// the session user, holding SessionPermissions with no seed, as the server's no_auth_user, as a
// hub renders them. grant is the set the hub's user carries: ListenerPermissions for its own
// listener, ListenerHostPermissions for a listener's own user elsewhere.
func startSplitGrantServer(t *testing.T, image string, grant core.Permissions) (string, natsServer, string) {
	t.Helper()
	seed, public := testnats.User(t)
	_, sessionKey := testnats.User(t)
	session := core.SessionPermissions()
	config, err := core.Authorization{Users: []core.User{
		{Name: liveMachineID, NKey: public, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true},
		{Name: "example-sessions", NKey: sessionKey, Publish: &session.Publish, Subscribe: &session.Subscribe},
	}, NoAuthUser: "example-sessions"}.Render(nil)
	require.NoError(t, err)
	server, uri := testnats.StartImageConfig(t, image, config, hubServerArgs, true)
	return seed, server, uri
}

// buildEnvoyCommand builds packages/envoy/cmd/<name> from this branch as Envoy's own jobs do, in
// the workspace of the repository's root go.work, and returns the binary envoy-<name>.
func buildEnvoyCommand(t *testing.T, name string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "envoy-"+name)
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Join("..", "..", "cmd", name)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build envoy-%s: %s", name, out)
	return binary
}

// startGrantServer runs image with one nkey user holding grant as the server's whole authorization,
// and returns the user's seed, the server and its client URL.
func startGrantServer(t *testing.T, image string, grant core.Permissions) (string, natsServer, string) {
	t.Helper()
	seed, public := testnats.User(t)
	config, err := core.Authorization{Users: []core.User{{
		Name:      liveMachineID,
		NKey:      public,
		Publish:   &grant.Publish,
		Subscribe: &grant.Subscribe,
		HasSeed:   true,
	}}}.Render(nil)
	require.NoError(t, err)
	server, uri := testnats.StartImageConfig(t, image, config, hubServerArgs, false)
	return seed, server, uri
}

// servingPort is a session's fake serve process: an httptest server on 127.0.0.1 that records the
// text of each prompt the listener POSTs to /session/<id>/prompt_async and answers 204.
type servingPort struct {
	port    int
	prompts chan string
}

// startServingPort serves sessionID's prompt_async route until the test ends. A request to any
// other method or path fails the test: the listener reached the port with something other than a
// prompt for this session.
func startServingPort(t *testing.T, sessionID string) *servingPort {
	t.Helper()
	serving := &servingPort{prompts: make(chan string, 16)}
	path := "/session/" + sessionID + "/prompt_async"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("%s's serving port was sent %s %s, want POST %s", sessionID, r.Method, r.URL.Path, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode the prompt sent to %s: %v", sessionID, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var text strings.Builder
		for _, part := range body.Parts {
			text.WriteString(part.Text)
		}
		serving.prompts <- text.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	require.NoError(t, err)
	serving.port = port
	return serving
}

// requirePrompt waits up to 10 s for a prompt that names item's topic and carries its message.
func (p *servingPort) requirePrompt(t *testing.T, process *cmdtest.Process, item contracts.Envelope) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case text := <-p.prompts:
			if strings.Contains(text, "Topic: "+item.Topic) && strings.Contains(text, item.Payload) {
				return
			}
			t.Logf("a prompt other than the one awaited: %s", text)
		case <-deadline:
			t.Fatalf("no prompt naming %s and carrying %q reached the serving port within 10s:\n%s",
				item.Topic, item.Payload, process.Output.String())
		}
	}
}

// startListener runs the listener binary as machine liveMachineID against uri, with the environment
// host:Listener gives its container and the user's seed, on a held loopback port, and returns it
// with its port.
func startListener(t *testing.T, binary, uri, seed string) (*cmdtest.Process, int) {
	t.Helper()
	held := cmdtest.HoldPort(t)
	return startListenerConfig(t, binary, uri, seed, held, core.ListenerConfig{MachineID: liveMachineID}), held.Port
}

// startListenerConfig runs the listener binary against uri with the environment config renders and
// the user's seed, so a test that asks what a component's own ListenerConfig does to the running
// listener starts it from that very config rather than from a hand-written environment. The
// listener serves on held, which it inherits as ENVOY_LISTEN_FD, so no other program can take the
// port before it serves; config's Port is held's port, set here, and the listener checks the two
// agree.
func startListenerConfig(t *testing.T, binary, uri, seed string, held *cmdtest.HeldPort, config core.ListenerConfig) *cmdtest.Process {
	t.Helper()
	config.Port = held.Port
	env, err := config.Env([]string{uri})
	require.NoError(t, err)
	cmd := exec.Command(binary)
	cmd.Env = append(env, core.EnvNATSSeed+"="+seed)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return cmdtest.Start(t, "the listener", cmd)
}

// healthz returns the status and body /healthz answers on port.
func healthz(port int) (int, string, error) {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// listenerHealthy is the answer the listener's /healthz gives once its durable is bound.
func listenerHealthy(_ int, body string) bool { return strings.Contains(body, `"status":"healthy"`) }

// requireHealthy waits up to window for /healthz to answer a response healthy accepts and returns
// that body, failing at once when process exits first.
func requireHealthy(t *testing.T, process *cmdtest.Process, port int, window time.Duration, healthy func(status int, body string) bool) string {
	t.Helper()
	deadline := time.Now().Add(window)
	var status int
	var body string
	var err error
	for time.Now().Before(deadline) {
		if status, body, err = healthz(port); err == nil && healthy(status, body) {
			return body
		}
		select {
		case <-process.Exited:
			t.Fatalf("the process exited before /healthz answered healthy:\n%s", process.Output.String())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("/healthz did not answer healthy within %s: last status %d, body %q, error %v\n%s",
		window, status, body, err, process.Output.String())
	return ""
}

// requireNeverHealthy watches /healthz for liveHealthWindow and fails if it ever answers
// "healthy".
func requireNeverHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(liveHealthWindow)
	for time.Now().Before(deadline) {
		if status, body, err := healthz(port); err == nil && listenerHealthy(status, body) {
			t.Fatalf("the listener answered \"healthy\": %s", body)
		}
		time.Sleep(time.Second)
	}
}

// postListener posts body to one of the listener's /v1 routes, requires a 200 and returns the
// answer.
func postListener(t *testing.T, process *cmdtest.Process, port int, path, body string) string {
	t.Helper()
	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(port)+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "POST %s: %s\n%s", path, answer, process.Output.String())
	return string(answer)
}

// replayResult is the part of one subject's /v1/replay result the live tests read.
type replayResult struct {
	Subject  string `json:"subject"`
	Messages []struct {
		Seq  uint64 `json:"seq"`
		Data string `json:"data"`
	} `json:"messages"`
	NextAfter uint64 `json:"next_after"`
	More      bool   `json:"more"`
	Refused   string `json:"refused"`
}

// replaySubject asks /v1/replay for one subject of session, after after when it is not nil, and
// returns that subject's result.
func replaySubject(t *testing.T, process *cmdtest.Process, port int, session, subject string, after *uint64) replayResult {
	t.Helper()
	type request struct {
		Subject string  `json:"subject"`
		After   *uint64 `json:"after,omitempty"`
	}
	body, err := json.Marshal(map[string]any{"session_id": session, "subjects": []request{{Subject: subject, After: after}}})
	require.NoError(t, err)
	answer := postListener(t, process, port, "/v1/replay", string(body))
	var replay struct {
		Subjects []replayResult `json:"subjects"`
	}
	require.NoError(t, json.Unmarshal([]byte(answer), &replay), answer)
	require.Len(t, replay.Subjects, 1, answer)
	require.Equal(t, subject, replay.Subjects[0].Subject, answer)
	return replay.Subjects[0]
}

// sendRoleMessage publishes body, a role message, through the listener's publish route and answers
// its forward as the holder's session does: the route answers only once the receipt arrived, so the
// forward is read on received and receipted while the publish waits. It requires the 200 and
// returns the forward.
func sendRoleMessage(t *testing.T, process *cmdtest.Process, port int, received *natsgo.Subscription, body string) contracts.Envelope {
	t.Helper()
	type answer struct {
		status int
		body   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(port)+"/v1/messages/publish", "application/json", strings.NewReader(body))
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		text, err := io.ReadAll(resp.Body)
		answered <- answer{status: resp.StatusCode, body: string(text), err: err}
	}()
	forwarded, msg := nextEnvelope(t, process, received)
	require.NotEmpty(t, msg.Reply, "the role forward carries no subject for its receipt")
	require.NoError(t, msg.Respond(nil))
	select {
	case got := <-answered:
		require.NoError(t, got.err)
		require.Equal(t, http.StatusOK, got.status, "POST /v1/messages/publish: %s\n%s", got.body, process.Output.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("the publish route did not answer within 10s after the receipt:\n%s", process.Output.String())
	}
	return forwarded
}

// postListenerHost posts body to one of the listener's routes over loopback under the request Host
// host, and returns the status and the body it answered, requiring nothing: it is for the checks
// that read the answer itself, which the listener's host guard decides.
func postListenerHost(t *testing.T, port int, path, body, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(answer)
}

// nextEnvelope returns the next message sub receives within 10 s, with the envelope it carries.
func nextEnvelope(t *testing.T, process *cmdtest.Process, sub *natsgo.Subscription) (contracts.Envelope, *natsgo.Msg) {
	t.Helper()
	msg, err := sub.NextMsg(10 * time.Second)
	require.NoError(t, err, "no message reached %s:\n%s", sub.Subject, process.Output.String())
	var envelope contracts.Envelope
	require.NoError(t, json.Unmarshal(msg.Data, &envelope))
	return envelope, msg
}

// serverLog returns everything the server has logged so far.
func serverLog(t *testing.T, server natsServer) string {
	t.Helper()
	logs, err := server.Logs(context.Background())
	require.NoError(t, err)
	defer logs.Close()
	text, err := io.ReadAll(logs)
	require.NoError(t, err)
	return string(text)
}

// violations returns each permissions violation the server has logged so far, as the matched
// text, its kind and its subject.
func violations(t *testing.T, server natsServer) [][]string {
	t.Helper()
	return serverViolation.FindAllStringSubmatch(serverLog(t, server), -1)
}

// violationsOnceLogged waits up to 10 s for the server to log a violation naming subject, then
// returns every violation it has logged.
func violationsOnceLogged(t *testing.T, server natsServer, subject string) [][]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		logged := violations(t, server)
		if slices.ContainsFunc(logged, func(v []string) bool { return v[2] == subject }) {
			return logged
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server logged no violation naming %s within 10s:\n%s", subject, serverLog(t, server))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sessionConsumerCall reports whether a refused subject is one of the session consumers' calls
// (core.ListenerPermissions holds them; the host and Dispatch sets do not): a consumer API call on
// the stream naming a consumer other than this machine's durable, the stream's consumer list, or
// an acknowledgement on such a consumer. A listener under a set without them probes once an
// episode and serves its sessions over core NATS.
func sessionConsumerCall(subject string) bool {
	durable := bus.ListenerDurable(liveMachineID)
	for _, call := range []string{"CREATE", "INFO", "DELETE", "MSG.NEXT"} {
		if rest, ok := strings.CutPrefix(subject, "$JS.API.CONSUMER."+call+"."+bus.Stream+"."); ok {
			return rest != durable && !strings.HasPrefix(rest, durable+".")
		}
	}
	if subject == "$JS.API.CONSUMER.LIST."+bus.Stream {
		return true
	}
	rest, ok := strings.CutPrefix(subject, "$JS.ACK."+bus.Stream+".")
	return ok && !strings.HasPrefix(rest, durable+".")
}

// The hub's grants for session consumers, as the listener's bus client manages them: the listener's
// user creates a session's consumer, pulls a stored message from it and acks it, restarts it at its
// floor with nothing awaiting an ack (its info and an update in place) and restarts it again with a
// message awaiting one (its info, delete and create); the credential-free session user is refused a
// consumer create; and the listener's user still subscribes its role lanes in its machine's queue
// group, as a listener of an earlier release does, beside the group every listener shares. Those
// refusals are all the server refused.
func TestListenerPermissionsManageSessionConsumersOnRealServers(t *testing.T) {
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			seed, server, uri := startSplitServer(t, image.Ref)
			// The listener's user, as its bus clients read it from their environment.
			environment := bus.WithEnvironment(func(name string) (string, bool) {
				switch name {
				case "NATS_NKEY_SEED":
					return seed, true
				case "NATS_NKEY_SEED_FILE":
					return "", false
				}
				return os.LookupEnv(name)
			})
			owner, err := bus.ConnectOwningStream([]string{uri}, environment)
			require.NoError(t, err)
			t.Cleanup(owner.Close)
			consumers, err := bus.ConnectSessionConsumers([]string{uri}, environment)
			require.NoError(t, err)
			t.Cleanup(consumers.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			agent := contracts.AgentSubject(liveSession)
			filters := []string{agent, "notifications.github.example.repo.>"}
			start, err := consumers.Ensure(ctx, liveSession, filters, 0)
			require.NoError(t, err)
			require.True(t, start.Created)
			item := contracts.Envelope{
				EventID:        "evt-session-consumer",
				Source:         "agent",
				SourceEventID:  "evt-session-consumer",
				Topic:          agent,
				DedupeKey:      "permissions-live-test.evt-session-consumer",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "example stored message",
				TraceID:        "trace-evt-session-consumer",
			}
			require.NoError(t, owner.Publish(item))
			pull, err := consumers.Pull(ctx, liveSession)
			require.NoError(t, err)
			msg, err := pull.Next(ctx)
			require.NoError(t, err)
			require.Equal(t, agent, msg.Subject)
			require.NoError(t, msg.Ack())
			pull.Stop()
			// An ack is a publish the server answers nothing to; the floor shows it was taken.
			require.Eventually(t, func() bool {
				info, err := consumers.Info(ctx, liveSession)
				return err == nil && info.AckFloor.Stream == msg.StreamSeq
			}, 10*time.Second, 50*time.Millisecond, "the ack of %d never reached the consumer", msg.StreamSeq)
			restarted, err := consumers.RestartAtFloor(ctx, liveSession, filters)
			require.NoError(t, err)
			require.Equal(t, msg.StreamSeq+1, restarted.StartSeq)
			// A restart with a message awaiting an ack deletes the consumer and creates it again at
			// the floor, which is the one call of the set that needs the delete grant.
			unacked := item
			unacked.EventID = "evt-session-consumer-unacked"
			unacked.SourceEventID = unacked.EventID
			unacked.DedupeKey = "permissions-live-test." + unacked.EventID
			require.NoError(t, owner.Publish(unacked))
			pull, err = consumers.Pull(ctx, liveSession)
			require.NoError(t, err)
			held, err := pull.Next(ctx)
			require.NoError(t, err)
			require.Equal(t, msg.StreamSeq+1, held.StreamSeq)
			pull.Stop()
			recreated, err := consumers.RestartAtFloor(ctx, liveSession, filters)
			require.NoError(t, err)
			require.True(t, recreated.Created)
			require.Equal(t, msg.StreamSeq+1, recreated.StartSeq)

			// The listener's role lanes: the shared queue group, and its machine's, which a listener of
			// an earlier release subscribes in.
			lane, queue, err := bus.ConnectRoleLane([]string{uri}, liveMachineID, func(*natsgo.Msg) {}, environment)
			require.NoError(t, err)
			t.Cleanup(lane.Close)
			require.Equal(t, bus.RoleLaneQueue, queue)
			listenerConn, listenerErrs := connect(t, uri, seed)
			_, err = listenerConn.QueueSubscribe(contracts.RoleTopicPrefix+">", bus.ListenerQueue(liveMachineID), func(*natsgo.Msg) {})
			require.NoError(t, err)
			require.NoError(t, listenerConn.Flush())
			select {
			case err := <-listenerErrs:
				t.Fatalf("the server refused the listener's user something: %v", err)
			default:
			}

			// The session user may not create a consumer.
			sessionConn, sessionErrs := connect(t, uri, "")
			const create = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.s-s2"
			require.NoError(t, sessionConn.Publish(create, []byte(`{}`)))
			requireViolation(t, sessionConn, sessionErrs, `Publish to "`+create+`"`)
			for _, v := range violationsOnceLogged(t, server, create) {
				require.Equal(t, []string{"Publish", create}, v[1:], "the server refused what a grant should allow: %s", v[0])
			}
		})
	}
}
