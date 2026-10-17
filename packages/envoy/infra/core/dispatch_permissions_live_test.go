//go:build natslive

package core_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testpg"
)

const (
	dispatchMachineID = "example-dispatch"
	dispatchSessionID = "00000000-0000-4000-8000-000000000001"
	dispatchToken     = "example-agent-token"
	// dispatchHealthWindow is how long Dispatch has to migrate its schema and answer /healthz.
	dispatchHealthWindow = 120 * time.Second
	// deliveryCreate is the delivery durable's filtered consumer create, the permission the
	// negative control removes.
	deliveryCreate = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.>"
)

func TestDispatchPermissionsOnRealServers(t *testing.T) {
	dispatch := buildEnvoyCommand(t, "dispatch")
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			t.Run("envoy-dispatch needs no refused permission", func(t *testing.T) {
				t.Parallel()
				live := startLiveDispatch(t, dispatch, image.Ref, core.DispatchPermissions(dispatchMachineID))

				requireConsumerPushBound(t, live.js, bus.DispatchDeliveryDurable)
				requireConsumerPushBound(t, live.js, bus.DispatchJobDurable)
				requireDurablesAcknowledge(t, live.js)
				requireIssueEventPublished(t, live.process, live.conn, live.port)
				requireAgentStreamRelayed(t, live.conn, live.port)
				requireWebhookRedeliveryBucket(t, live.js, live.process, live.github)

				// Dispatch's own user holds no session entry: it is a direct publish to a session's
				// frames that is refused. (A delivery-events consumer it creates can still deliver
				// any payload onto them, and its _INBOX.> subscription receives every reply on the
				// server, so the refusal narrows nothing of the seed's reach.)
				frames := contracts.AgentStreamFramesSubject(dispatchSessionID)
				require.NoError(t, live.dispatchConn.Publish(frames, []byte(`{"v":1,"kind":"message","seq":2}`)))
				requireViolation(t, live.dispatchConn, live.dispatchErrs, `Publish to "`+frames+`"`)

				// Every allowed operation came first, so the two expected refusals are the only
				// violations the server logged: the frames publish above and the test client's canary.
				require.NoError(t, live.conn.Publish("outside.example", nil))
				requireViolation(t, live.conn, live.errs, `Publish to "outside.example"`)
				expected := []string{frames, "outside.example"}
				violationsOnceLogged(t, live.server, frames)
				for _, violation := range violationsOnceLogged(t, live.server, "outside.example") {
					require.Contains(t, expected, violation[2], "the server refused what a grant needs: %s", violation[0])
				}
			})
			t.Run("without the delivery consumer create Dispatch leaves only that intake unbound", func(t *testing.T) {
				t.Parallel()
				grant := core.DispatchPermissions(dispatchMachineID)
				before := len(grant.Publish.Allow)
				grant.Publish.Allow = slices.DeleteFunc(grant.Publish.Allow, func(subject string) bool {
					return subject == deliveryCreate
				})
				require.Len(t, grant.Publish.Allow, before-1, "the grant does not hold %s", deliveryCreate)
				live := startLiveDispatch(t, dispatch, image.Ref, grant)

				requireConsumerMissing(t, live.js, bus.DispatchDeliveryDurable)
				requireConsumerPushBound(t, live.js, bus.DispatchJobDurable)
				want := strings.TrimSuffix(deliveryCreate, ">") + "notifications.github.>"
				for _, violation := range violationsOnceLogged(t, live.server, want) {
					require.Equal(t, []string{"Publish", want}, violation[1:], "the server refused something else: %s", violation[0])
				}
				require.Contains(t, live.process.Output.String(), "dispatch delivery: create NATS consumer")
			})
		})
	}
}

// liveDispatch is one envoy-dispatch process started under a grant: the NATS server holding two
// users, Dispatch's (the grant) and the test client's (testClientPermissions), the fake GitHub App
// API behind Dispatch, the test client's connection, and a connection as Dispatch's own user for
// the checks of that grant itself.
type liveDispatch struct {
	server  natsServer
	github  *dispatchGitHub
	process *cmdtest.Process
	port    int
	// conn is the test client's connection: a second user, so no check leans on Dispatch's grant.
	conn *natsgo.Conn
	errs <-chan error
	js   natsgo.JetStreamContext
	// dispatchConn is a connection as Dispatch's own user.
	dispatchConn *natsgo.Conn
	dispatchErrs <-chan error
}

// startLiveDispatch starts a NATS server whose authorization holds Dispatch's user (grant) and the
// test client's (testClientPermissions), seeds the delivery settings in a fresh Postgres, starts
// envoy-dispatch against both, waits for it to answer healthy, and connects the test client as its
// own user and a second connection as Dispatch's.
func startLiveDispatch(t *testing.T, binary, image string, grant core.Permissions) liveDispatch {
	t.Helper()
	seed, clientSeed, server, uri := startDispatchServer(t, image, grant)
	databaseURL := testpg.URL(t)
	seedDispatchDeliverySettings(t, databaseURL)
	github := newDispatchGitHub(t)
	process, port := startDispatch(t, binary, databaseURL, uri, seed, github.server.URL)
	requireHealthy(t, process, port, dispatchHealthWindow, dispatchHealthy)
	conn, errs := connect(t, uri, clientSeed)
	js, err := conn.JetStream()
	require.NoError(t, err)
	dispatchConn, dispatchErrs := connect(t, uri, seed)
	return liveDispatch{server: server, github: github, process: process, port: port,
		conn: conn, errs: errs, js: js, dispatchConn: dispatchConn, dispatchErrs: dispatchErrs}
}

// startDispatchServer runs image with two users: Dispatch's, holding grant with its seed, and the
// test client's, holding testClientPermissions with a seed of its own. It returns both seeds, the
// server and its client URL.
func startDispatchServer(t *testing.T, image string, grant core.Permissions) (string, string, natsServer, string) {
	t.Helper()
	dispatchSeed, dispatchKey := testnats.User(t)
	clientSeed, clientKey := testnats.User(t)
	client := testClientPermissions()
	config, err := core.Authorization{Users: []core.User{
		{Name: dispatchMachineID, NKey: dispatchKey, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true},
		{Name: "example-test-client", NKey: clientKey, Publish: &client.Publish, Subscribe: &client.Subscribe, HasSeed: true},
	}}.Render(nil)
	require.NoError(t, err)
	server, uri := testnats.StartImageConfig(t, image, config, hubServerArgs, false)
	return dispatchSeed, clientSeed, server, uri
}

// testClientPermissions is the grant of the live test's own NATS user: SessionPermissions, which
// lets it play a session (publish its frames, subscribe notifications and its control subject),
// plus the exact calls its checks make, so it leans on nothing of Dispatch's grant: reply inboxes
// for its JetStream requests, each Dispatch durable's consumer info, a JetStream publish into the
// stream, and the redelivery bucket's reads, writes and watch — not the bucket's create, which is
// Dispatch's to make.
func testClientPermissions() core.Permissions {
	session := core.SessionPermissions()
	redelivery := "KV_" + bus.WebhookRedeliveryBucket
	publish := append(session.Publish.Allow,
		bus.NotificationTopicPrefix+">",
		"$JS.API.CONSUMER.INFO."+bus.Stream+"."+bus.DispatchDeliveryDurable,
		"$JS.API.CONSUMER.INFO."+bus.Stream+"."+bus.DispatchJobDurable,
		"$JS.API.STREAM.INFO."+redelivery,
		"$JS.API.DIRECT.GET."+redelivery+".>",
		"$JS.API.STREAM.MSG.GET."+redelivery,
		"$KV."+bus.WebhookRedeliveryBucket+".>",
		"$JS.API.CONSUMER.CREATE."+redelivery+".>",
		"$JS.API.CONSUMER.DELETE."+redelivery+".>",
		"$JS.FC."+redelivery+".>",
	)
	return core.Permissions{
		Publish:   core.Rules{Allow: publish},
		Subscribe: core.Rules{Allow: append(session.Subscribe.Allow, "_INBOX.>")},
	}
}

func seedDispatchDeliverySettings(t *testing.T, databaseURL string) {
	t.Helper()
	database, err := store.Open(t.Context(), databaseURL)
	require.NoError(t, err)
	defer database.Pool.Close()
	require.NoError(t, database.Migrate(t.Context()))
	_, err = delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/widgets",
		DeployWorkflowPath:   ".github/workflows/deploy.yml",
		ProductionJobNames:   []string{"release"},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors:    []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "dispatch-permissions-test"})
	require.NoError(t, err)
}

func startDispatch(t *testing.T, binary, databaseURL, natsURL, seed, githubURL string) (*cmdtest.Process, int) {
	t.Helper()
	home := t.TempDir()
	port := cmdtest.FreeTCPPort(t)
	_, pemText := githubapptest.Key(t)
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=" + dispatchToken,
		"DISPATCH_DEV_SIGNIN=1",
		"DISPATCH_INSECURE_COOKIE=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(port),
		"DISPATCH_SERVER_URL=" + dispatchURL(port, ""),
		"NATS_URLS=" + natsURL,
		core.EnvNATSSeed + "=" + seed,
		"DISPATCH_APP_CLIENT_ID=Iv1.e2efake",
		"DISPATCH_APP_PEM_B64=" + base64.StdEncoding.EncodeToString([]byte(pemText)),
		"DISPATCH_GITHUB_API_BASE=" + githubURL,
		"DISPATCH_WEB_DIST=" + home,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
	}
	return cmdtest.Start(t, "Dispatch", cmd), port
}

// dispatchHealthy is the answer Dispatch's /healthz gives once its schema is migrated.
func dispatchHealthy(status int, body string) bool {
	return status == http.StatusOK && strings.Contains(body, `"schema_version":`)
}

// requireConsumerPushBound waits up to 20 s for the durable to exist with a push subscriber bound.
func requireConsumerPushBound(t *testing.T, js natsgo.JetStreamContext, durable string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}, 20*time.Second, 100*time.Millisecond, "consumer %s was not push-bound within 20s", durable)
}

// requireConsumerMissing watches the durable for 20 s: every read must keep answering that no
// such consumer exists.
func requireConsumerMissing(t *testing.T, js natsgo.JetStreamContext, durable string) {
	t.Helper()
	require.Neverf(t, func() bool {
		_, err := js.ConsumerInfo(bus.Stream, durable)
		return !errors.Is(err, natsgo.ErrConsumerNotFound)
	}, 20*time.Second, 100*time.Millisecond,
		"consumer %s was created despite its missing permission, or its read failed", durable)
}

// requireDurablesAcknowledge publishes one message that both Dispatch durables' filters match and
// waits until each has acknowledged it: an envelope that does not decode, which the delivery
// intake acknowledges without calling GitHub and the job intake acknowledges on receipt. So each
// durable's $JS.ACK grant is exercised, and a missing one leaves the message pending.
func requireDurablesAcknowledge(t *testing.T, js natsgo.JetStreamContext) {
	t.Helper()
	ack, err := js.Publish("notifications.github.acme.widgets.job.1.completed", []byte("not an envelope"))
	require.NoError(t, err)
	for _, durable := range []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable} {
		require.Eventuallyf(t, func() bool {
			info, err := js.ConsumerInfo(bus.Stream, durable)
			return err == nil && info.AckFloor.Stream >= ack.Sequence && info.NumAckPending == 0
		}, 20*time.Second, 100*time.Millisecond,
			"consumer %s did not acknowledge stream sequence %d", durable, ack.Sequence)
	}
}

func requireIssueEventPublished(t *testing.T, process *cmdtest.Process, conn *natsgo.Conn, port int) {
	t.Helper()
	subscription, err := conn.SubscribeSync("notifications.dispatch.issue.>")
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	client := &http.Client{Timeout: 10 * time.Second}
	postDispatchJSON(t, client, port, "/api/v1/projects", `{"key":"ACME","name":"Acme","actor":{"kind":"session","id":"`+dispatchSessionID+`"}}`)
	postDispatchJSON(t, client, port, "/api/v1/issues", `{"project":"ACME","title":"example","actor":{"kind":"session","id":"`+dispatchSessionID+`"}}`)
	envelope, _ := nextEnvelope(t, process, subscription)
	require.True(t, strings.HasSuffix(envelope.Topic, ".issue.created"), "event topic %q", envelope.Topic)
}

func requireAgentStreamRelayed(t *testing.T, conn *natsgo.Conn, port int) {
	t.Helper()
	control, err := conn.SubscribeSync(contracts.AgentStreamControlSubject(dispatchSessionID))
	require.NoError(t, err)
	t.Cleanup(func() { _ = control.Unsubscribe() })
	require.NoError(t, conn.Flush())

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	human := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	signinCtx, cancelSignin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSignin()
	signin, err := http.NewRequestWithContext(signinCtx, http.MethodGet, dispatchURL(port, "/auth/_dev/signin?login=alice@example.com"), nil)
	require.NoError(t, err)
	response, err := human.Do(signin)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, response.StatusCode)
	_ = response.Body.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, dispatchURL(port, "/api/v1/agents/"+dispatchSessionID+"/stream"), nil)
	require.NoError(t, err)
	stream, err := human.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Body.Close() })
	require.Equal(t, http.StatusOK, stream.StatusCode)

	watch, err := control.NextMsg(10 * time.Second)
	require.NoError(t, err, "Dispatch sent no agent stream control message within 10s")
	require.Equal(t, `{"v":1,"type":"watch"}`, string(watch.Data))
	replay, err := control.NextMsg(10 * time.Second)
	require.NoError(t, err, "Dispatch sent no agent stream control message within 10s")
	require.Equal(t, `{"v":1,"type":"replay"}`, string(replay.Data))
	require.NoError(t, replay.Respond(nil))

	frame := `{"v":1,"kind":"message","seq":1}`
	require.NoError(t, conn.Publish(contracts.AgentStreamFramesSubject(dispatchSessionID), []byte(frame)))
	require.NoError(t, conn.Flush())
	requireSSEFrame(t, bufio.NewReader(stream.Body), frame)
}

func requireSSEFrame(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if line != "event: frame\n" {
			continue
		}
		data, err := reader.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "data: "+want+"\n", data)
		separator, err := reader.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "\n", separator)
		return
	}
}

func requireWebhookRedeliveryBucket(t *testing.T, js natsgo.JetStreamContext, process *cmdtest.Process, github *dispatchGitHub) {
	t.Helper()
	process.WaitForOutput(t, "dispatch: redelivering the GitHub App webhook's failed deliveries")
	github.requireCalled(t, "GET /app/hook/deliveries")
	kv, err := bus.OpenKeyValue(js, bus.WebhookRedeliveryBucket)
	require.NoError(t, err)
	_, err = kv.Put("test", []byte("value"))
	require.NoError(t, err)
	entry, err := kv.Get("test")
	require.NoError(t, err)
	require.Equal(t, []byte("value"), entry.Value())
	watch, err := kv.Watch("test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = watch.Stop() })
	select {
	case update := <-watch.Updates():
		require.NotNil(t, update)
		require.Equal(t, "test", update.Key())
	case <-time.After(10 * time.Second):
		t.Fatal("the redelivery bucket watch did not deliver its key")
	}
}

func postDispatchJSON(t *testing.T, client *http.Client, port int, path, body string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, dispatchURL(port, path), strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+dispatchToken)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode, "POST %s: %s", path, answer)
}

func dispatchURL(port int, path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + path
}

type dispatchGitHub struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
}

func newDispatchGitHub(t *testing.T) *dispatchGitHub {
	t.Helper()
	fake := &dispatchGitHub{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path := request.Method + " " + request.URL.Path
		fake.mu.Lock()
		fake.paths = append(fake.paths, path)
		fake.mu.Unlock()
		switch {
		case path == "GET /app":
			_, _ = fmt.Fprint(w, `{"id":1,"slug":"example","name":"Example"}`)
		case path == "GET /app/installations":
			_, _ = fmt.Fprint(w, `[{"id":1,"app_slug":"example","account":{"login":"example"},"permissions":{"actions":"read","pull_requests":"read"}}]`)
		case path == "GET /repos/acme/widgets/installation":
			_, _ = fmt.Fprint(w, `{"id":1,"app_slug":"example","permissions":{"actions":"read","pull_requests":"read"}}`)
		case path == "POST /app/installations/1/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"example-installation-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case path == "GET /installation/repositories":
			_, _ = fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`)
		case path == "POST /graphql":
			_, _ = fmt.Fprint(w, `{"data":{"search":{"issueCount":0,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}`)
		case strings.HasPrefix(path, "GET /repos/acme/widgets/actions/workflows/"):
			_, _ = fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
		case path == "GET /app/hook/deliveries":
			_, _ = fmt.Fprint(w, "[]")
		default:
			t.Errorf("unexpected GitHub App request %s", path)
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *dispatchGitHub) requireCalled(t *testing.T, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.paths, want)
	}, 20*time.Second, 100*time.Millisecond, "GitHub fake did not receive %s", want)
}
