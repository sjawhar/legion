//go:build natslive

package core_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testpg"
)

const (
	dispatchMachineID = "example-dispatch"
	dispatchSessionID = "00000000-0000-4000-8000-000000000001"
	dispatchToken     = "example-agent-token"
)

func TestDispatchPermissionsOnRealServers(t *testing.T) {
	dispatch := buildDispatch(t)
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Run("envoy-dispatch needs no refused permission", func(t *testing.T) {
				runDispatchPermissions(t, dispatch, image.Ref, false)
			})
			t.Run("without the delivery consumer create Dispatch leaves only that intake unbound", func(t *testing.T) {
				runDispatchPermissions(t, dispatch, image.Ref, true)
			})
		})
	}
}

func runDispatchPermissions(t *testing.T, dispatch, image string, removeDeliveryCreate bool) {
	t.Helper()
	grant := core.DispatchPermissions(dispatchMachineID)
	const deliveryCreate = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.>"
	if removeDeliveryCreate {
		before := len(grant.Publish.Allow)
		grant.Publish.Allow = slices.DeleteFunc(grant.Publish.Allow, func(subject string) bool {
			return subject == deliveryCreate
		})
		require.Len(t, grant.Publish.Allow, before-1, "the grant does not hold %s", deliveryCreate)
	}

	seed, server, uri := startHostServer(t, image, grant)
	databaseURL := testpg.URL(t)
	seedDispatchDeliverySettings(t, databaseURL)
	github := newDispatchGitHub(t)
	process, port := startDispatch(t, dispatch, databaseURL, uri, seed, github.server.URL)
	requireDispatchHealthy(t, process, port)

	conn, errs := connect(t, uri, seed)
	js, err := conn.JetStream()
	require.NoError(t, err)

	if removeDeliveryCreate {
		requireConsumerMissing(t, js, bus.DispatchDeliveryDurable)
		requireConsumerPushBound(t, js, bus.DispatchJobDurable)
		want := strings.TrimSuffix(deliveryCreate, ">") + "notifications.github.>"
		for _, violation := range violationsOnceLogged(t, server, want) {
			require.Equal(t, []string{"Publish", want}, violation[1:], "the server refused something else: %s", violation[0])
		}
		require.Contains(t, process.Output.String(), "dispatch delivery: create NATS consumer")
		return
	}

	requireConsumerPushBound(t, js, bus.DispatchDeliveryDurable)
	requireConsumerPushBound(t, js, bus.DispatchJobDurable)
	requireDurablesAcknowledge(t, js)
	requireIssueEventPublished(t, conn, port)
	requireAgentStreamRelayed(t, conn, port)
	requireWebhookRedeliveryBucket(t, js, process, github)

	require.NoError(t, conn.Publish("outside.example", nil))
	requireViolation(t, conn, errs, `Publish to "outside.example"`)
	for _, violation := range violationsOnceLogged(t, server, "outside.example") {
		require.Equal(t, "outside.example", violation[2], "the server refused what Dispatch needs: %s", violation[0])
	}
}

func buildDispatch(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "envoy-dispatch")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Join("..", "..", "cmd", "dispatch")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build Dispatch: %s", out)
	return binary
}

func seedDispatchDeliverySettings(t *testing.T, databaseURL string) {
	t.Helper()
	database, err := store.Open(t.Context(), databaseURL)
	require.NoError(t, err)
	t.Cleanup(database.Pool.Close)
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=" + dispatchToken,
		"DISPATCH_DEV_SIGNIN=1",
		"DISPATCH_INSECURE_COOKIE=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(port),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + strconv.Itoa(port),
		"NATS_URLS=" + natsURL,
		"NATS_NKEY_SEED=" + seed,
		"DISPATCH_APP_CLIENT_ID=Iv1.e2efake",
		"DISPATCH_APP_PEM_B64=" + base64.StdEncoding.EncodeToString(pemText),
		"DISPATCH_GITHUB_API_BASE=" + githubURL,
		"DISPATCH_WEB_DIST=" + home,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
	}
	return cmdtest.Start(t, "Dispatch", cmd), port
}

func requireDispatchHealthy(t *testing.T, process *cmdtest.Process, port int) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var body string
	var err error
	for time.Now().Before(deadline) {
		var response *http.Response
		response, err = http.Get(dispatchURL(port, "/healthz"))
		if err == nil {
			bytes, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil {
				body = string(bytes)
				if response.StatusCode == http.StatusOK && strings.Contains(body, `"schema_version":`) {
					return
				}
			}
		}
		select {
		case <-process.Exited:
			t.Fatalf("Dispatch exited before it became healthy:\n%s", process.Output.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("Dispatch did not answer /healthz with schema_version within 120s: last body %q, error %v\n%s", body, err, process.Output.String())
}

func requireConsumerPushBound(t *testing.T, js natsgo.JetStreamContext, durable string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err == nil && info.PushBound {
			return
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("consumer %s was not push-bound within 20s: %v", durable, last)
}

func requireConsumerMissing(t *testing.T, js natsgo.JetStreamContext, durable string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, err := js.ConsumerInfo(bus.Stream, durable)
		if err == nil {
			t.Fatalf("consumer %s was created despite its missing permission", durable)
		}
		if !strings.Contains(err.Error(), "consumer not found") {
			t.Fatalf("read consumer %s: %v", durable, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
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
		deadline := time.Now().Add(20 * time.Second)
		var info *natsgo.ConsumerInfo
		for time.Now().Before(deadline) {
			info, err = js.ConsumerInfo(bus.Stream, durable)
			if err == nil && info.AckFloor.Stream >= ack.Sequence && info.NumAckPending == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.NoError(t, err)
		require.GreaterOrEqual(t, info.AckFloor.Stream, ack.Sequence, "consumer %s did not acknowledge stream sequence %d", durable, ack.Sequence)
		require.Zero(t, info.NumAckPending, "consumer %s holds unacknowledged messages", durable)
	}
}

func requireIssueEventPublished(t *testing.T, conn *natsgo.Conn, port int) {
	t.Helper()
	subscription, err := conn.SubscribeSync("notifications.dispatch.issue.>")
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	client := &http.Client{Timeout: 10 * time.Second}
	postDispatchJSON(t, client, port, "/api/v1/projects", `{"key":"ACME","name":"Acme","actor":{"kind":"session","id":"`+dispatchSessionID+`"}}`, http.StatusCreated, "Bearer "+dispatchToken)
	postDispatchJSON(t, client, port, "/api/v1/issues", `{"project":"ACME","title":"example","actor":{"kind":"session","id":"`+dispatchSessionID+`"}}`, http.StatusCreated, "Bearer "+dispatchToken)
	message, err := subscription.NextMsg(10 * time.Second)
	require.NoError(t, err)
	var envelope contracts.Envelope
	require.NoError(t, json.Unmarshal(message.Data, &envelope))
	require.True(t, strings.HasSuffix(envelope.Topic, ".issue.created"), "event topic %q", envelope.Topic)
}

// nextControl is the next agent stream control message Dispatch sent, failing the test after 10s:
// a refused control publish never arrives.
func nextControl(t *testing.T, control <-chan *natsgo.Msg) *natsgo.Msg {
	t.Helper()
	select {
	case msg := <-control:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch sent no agent stream control message within 10s")
		return nil
	}
}

func requireAgentStreamRelayed(t *testing.T, conn *natsgo.Conn, port int) {
	t.Helper()
	control := make(chan *natsgo.Msg, 2)
	subscription, err := conn.ChanSubscribe(contracts.AgentStreamControlSubject(dispatchSessionID), control)
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, conn.Flush())

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	human := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := human.Get(dispatchURL(port, "/auth/_dev/signin?login=alice@example.com"))
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

	watch := nextControl(t, control)
	require.Equal(t, `{"v":1,"type":"watch"}`, string(watch.Data))
	replay := nextControl(t, control)
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
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := js.KeyValue(bus.WebhookRedeliveryBucket); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
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

func postDispatchJSON(t *testing.T, client *http.Client, port int, path, body string, want int, authorization string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, dispatchURL(port, path), strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", authorization)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, want, response.StatusCode, "POST %s: %s", path, answer)
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
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		called := slices.Contains(f.paths, want)
		f.mu.Unlock()
		if called {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("GitHub fake did not receive %s; got %v", want, f.paths)
}
