//go:build natslive

package core_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The live tests of the host grant: one nkey user holding HostPermissions, on the 2.10 image and on
// the hub's, each run with the hub's own server arguments. This branch's listener connects as that
// user with the environment host:Listener gives it, and a session connects as the same user, as
// every session on a host does.

const (
	liveMachineID = "example-host-a"
	liveSession   = "s1"
	liveRole      = "example-role"
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
				process, port := startListener(t, listener, uri, seed)
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

				// A role message reaches its holder through the listener's queue subscription, and the
				// session answers it with the receipt the listener waits for.
				postListener(t, process, port, "/v1/roles/set", `{"session_id":"`+liveSession+`","role":"`+liveRole+`"}`)
				postListener(t, process, port, "/v1/messages/publish", `{"topic":"`+contracts.RoleTopicPrefix+liveRole+`","message":"example role message"}`)
				forwarded, msg := nextEnvelope(t, process, received)
				require.Equal(t, contracts.RoleTopicPrefix+liveRole, forwarded.Topic)
				require.NotEmpty(t, msg.Reply, "the role forward carries no subject for its receipt")
				require.NoError(t, msg.Respond(nil))

				// A topic outside every stream subject reaches the session subscribed to it.
				adhoc, err := conn.SubscribeSync("notifications.team.>")
				require.NoError(t, err)
				require.NoError(t, conn.Flush())
				postListener(t, process, port, "/v1/messages/publish", `{"topic":"notifications.team.deploy-squad","message":"example ad-hoc message"}`)
				team, _ := nextEnvelope(t, process, adhoc)
				require.Equal(t, "notifications.team.deploy-squad", team.Topic)

				// Every allowed operation came first, so a violation for one would be the first error.
				require.NoError(t, conn.Publish("outside.example", nil))
				requireViolation(t, conn, errs, `Publish to "outside.example"`)
				// The server refused nothing else either, the listener's own operations included. Its
				// log names the refusal just made before that reading counts: one that found no
				// violation at all would pass for the wrong reason.
				for _, v := range violationsOnceLogged(t, server, "outside.example") {
					require.Equal(t, "outside.example", v[2], "the server refused what the grant should allow: %s", v[0])
				}
			})
			t.Run("without its durable's consumer create the listener never reaches healthy", func(t *testing.T) {
				t.Parallel()
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
// an ad-hoc topic it subscribes to on NATS, and is refused a publish its grant does not hold.
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
			postListener(t, process, port, "/v1/messages/publish", `{"topic":"`+contracts.RoleTopicPrefix+liveRole+`","message":"example role message"}`)
			forwarded, msg := nextEnvelope(t, process, received)
			require.Equal(t, contracts.RoleTopicPrefix+liveRole, forwarded.Topic)
			require.NotEmpty(t, msg.Reply, "the role forward carries no subject for its receipt")
			require.NoError(t, msg.Respond(nil))

			// A topic outside every stream subject, as an agent names one for its team, reaches the
			// session subscribed to it: the listener publishes it over core NATS, under its grant.
			adhoc, err := conn.SubscribeSync("notifications.team.>")
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			postListener(t, process, port, "/v1/messages/publish", `{"topic":"notifications.team.deploy-squad","message":"example ad-hoc message"}`)
			team, _ := nextEnvelope(t, process, adhoc)
			require.Equal(t, "notifications.team.deploy-squad", team.Topic)
			require.Equal(t, "example ad-hoc message", team.PayloadSummary)

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

// A user holding SessionPermissions alone, as sessions that share one user hold it, receives its
// notifications and publishes a reply, and the server refuses it a subscribe to the reply inboxes,
// on the 2.10 image and on the hub's: request/reply from such a session is refused by design.
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
		})
	}
}

// startSplitServer runs image with two users: the listener's, holding ListenerPermissions for
// liveMachineID with a seed, and the session user, holding SessionPermissions with no seed, as the
// server's no_auth_user. It returns the listener's seed, the server and its client URL.
func startSplitServer(t *testing.T, image string) (string, natsServer, string) {
	t.Helper()
	seed, public := testnats.User(t)
	_, sessionKey := testnats.User(t)
	listener, session := core.ListenerPermissions(liveMachineID), core.SessionPermissions()
	config, err := core.Authorization{Users: []core.User{
		{Name: liveMachineID, NKey: public, Publish: &listener.Publish, Subscribe: &listener.Subscribe, HasSeed: true},
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

// startListener runs the listener binary as machine liveMachineID against uri, with the environment
// host:Listener gives its container and the user's seed, and returns it with its port.
func startListener(t *testing.T, binary, uri, seed string) (*cmdtest.Process, int) {
	t.Helper()
	port := cmdtest.FreeTCPPort(t)
	return startListenerConfig(t, binary, uri, seed, core.ListenerConfig{MachineID: liveMachineID, Port: port}), port
}

// startListenerConfig runs the listener binary against uri with the environment config renders and
// the user's seed, so a test that asks what a component's own ListenerConfig does to the running
// listener starts it from that very config rather than from a hand-written environment.
func startListenerConfig(t *testing.T, binary, uri, seed string, config core.ListenerConfig) *cmdtest.Process {
	t.Helper()
	env, err := config.Env([]string{uri})
	require.NoError(t, err)
	cmd := exec.Command(binary)
	cmd.Env = append(env, core.EnvNATSSeed+"="+seed)
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

// postListener posts body to one of the listener's /v1 routes and requires a 200.
func postListener(t *testing.T, process *cmdtest.Process, port int, path, body string) {
	t.Helper()
	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(port)+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "POST %s: %s\n%s", path, answer, process.Output.String())
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
