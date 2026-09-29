package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// During a rolling deploy the replacement listener cannot bind its durable consumer until the task
// it replaces lets go of it, which takes the old task's deregistration and shutdown; the load
// balancer already sends the replacement webhooks by then. A webhook needs NATS and the CI store,
// not the durable, so the replacement serves it: GitHub does not redeliver a refused delivery on
// its own. /v1 still waits for every dependency.
func TestAWebhookIsServedWhileAnotherTaskHoldsTheDurable(t *testing.T) {
	const (
		machineID = "webhook-bind-window"
		secret    = "bind-window-secret"
		delivery  = "delivery-bind-window"
	)
	client, err := bus.ConnectOwningStream([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	consumer := "listener-" + machineID
	_ = client.JS().DeleteConsumer(bus.Stream, consumer)
	t.Cleanup(func() { _ = client.JS().DeleteConsumer(bus.Stream, consumer) })
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: natsgo.NewInbox()}
	applyListenerConsumerPolicy(&config, bus.StreamSubjects())
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	// The task being replaced: it holds the durable's push binding until it stops.
	holder, err := client.JS().Subscribe("", func(msg *natsgo.Msg) { _ = msg.Ack() }, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable as the old task: %v", err)
	}

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), machineID,
		"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET="+secret, "ENVOY_REVIEWER_APP_ID=1")
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(listener.output.String(), "subscribe failed, retrying") {
		select {
		case <-listener.exited:
			t.Fatalf("the listener exited before it met the bound durable:\n%s", listener.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never met the bound durable:\n%s", listener.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("b", 40) + `","before":"` + strings.Repeat("a", 40) + `",` +
		`"pusher":{"name":"example-author"},"sender":{"login":"example-author","type":"User"},` +
		`"repository":{"name":"bind-window","owner":{"login":"acme"},"full_name":"acme/bind-window"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, answer := do(t, request)
	if status != http.StatusOK {
		t.Fatalf("webhook while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.output.String())
	}
	message, err := client.JS().GetLastMsg(bus.Stream, "notifications.github.acme.bind-window.push.branch.main")
	if err != nil {
		t.Fatalf("the accepted push is not on the stream: %v", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		t.Fatalf("decode the published push: %v", err)
	}
	if envelope.SourceEventID != delivery {
		t.Fatalf("the stream's last push is delivery %q, want %q", envelope.SourceEventID, delivery)
	}

	sessions, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/sessions", nil)
	if err != nil {
		t.Fatalf("sessions request: %v", err)
	}
	sessions.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if status, answer := do(t, sessions); status != http.StatusServiceUnavailable || !strings.Contains(answer, "service starting") {
		t.Fatalf("/v1 before the durable binds: status %d %q, want 503 service starting", status, answer)
	}

	if err := holder.Unsubscribe(); err != nil {
		t.Fatalf("release the durable as the old task: %v", err)
	}
	listener.waitHealthy(t)
}

// A listener built before the KV key check stored role claims, interests, sessions and CI records
// under keys this one refuses to write (testnats.LegacyKeys), and every listener on that NATS reads
// the first three buckets as it starts, and the CI bucket when it mounts the GitHub webhook route.
// It must start healthy over them and serve the ordinary keys beside them: one stored role must not
// keep every listener from starting.
func TestTheListenerStartsOverKeysAnEarlierListenerStored(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	resetListenerTestState(t, client.Conn)
	t.Cleanup(func() { resetListenerTestState(t, client.Conn); clearKVBucket(t, client.Conn, cistore.Bucket) })
	bucket := func(name string, ttl time.Duration) natsgo.KeyValue {
		t.Helper()
		kv, err := client.JS().KeyValue(name)
		if errors.Is(err, natsgo.ErrBucketNotFound) {
			kv, err = client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: name, TTL: ttl, Storage: natsgo.FileStorage})
		}
		if err != nil {
			t.Fatalf("open bucket %s: %v", name, err)
		}
		return kv
	}
	put := func(kv natsgo.KeyValue, key string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s value: %v", kv.Bucket(), err)
		}
		if _, err := kv.Put(key, data); err != nil {
			t.Fatalf("store the %d-byte %s key: %v", len(key), kv.Bucket(), err)
		}
	}
	now := time.Now().UnixMilli()
	roles, interests := bucket(store.RoleBucket, 0), bucket(store.Bucket, 0)
	sessions, records := bucket(session.SessionBucket, 5*time.Minute), bucket(cistore.Bucket, 0)
	put(roles, "reviewer", store.RoleClaim{HolderSessionID: "ses_live", ClaimedAt: now})
	put(interests, "ses_live", store.Interest{SessionID: "ses_live", MachineID: "startup-legacy", Topics: []string{contracts.AgentSubject("ses_live")}, UpdatedAt: now})
	put(sessions, "ses_live", session.SessionEntry{Port: 1, MachineID: "startup-legacy", UpdatedAt: now})
	for _, kv := range []natsgo.KeyValue{roles, interests, sessions, records} {
		readable, unreadable := testnats.LegacyKeys(kv.Bucket(), "k")
		for _, key := range []string{readable, unreadable} {
			switch kv {
			case roles:
				put(kv, key, store.RoleClaim{HolderSessionID: "ses_gone", ClaimedAt: now})
			case interests:
				put(kv, key, store.Interest{SessionID: key, MachineID: "earlier", UpdatedAt: now})
			case sessions:
				put(kv, key, session.SessionEntry{Port: 2, MachineID: "earlier", UpdatedAt: now})
			default:
				put(kv, key, cistore.State{Owner: "acme", Repo: "widgets", Number: "7", SHA: strings.Repeat("c", 40)})
			}
		}
	}

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), "startup-legacy", githubWebhookEnv...)
	listener.waitHealthy(t)
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/roles/reviewer", nil)
	if err != nil {
		t.Fatalf("role request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if status, answer := do(t, request); status != http.StatusOK || !strings.Contains(answer, `"holder":"ses_live"`) {
		t.Fatalf("GET /v1/roles/reviewer beside the earlier listener's keys: status %d %q, want 200 naming ses_live\n%s", status, answer, listener.output.String())
	}
}

// githubWebhookEnv mounts the listener's GitHub webhook route, the one route that uses the CI store.
var githubWebhookEnv = []string{"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET=ci-route-secret", "ENVOY_REVIEWER_APP_ID=1"}

// seedDueCIRecord creates the CI bucket as the listener creates it and stores one commit whose
// checks finished a minute ago and have not settled, so any summary loop reading the bucket settles
// it on its next tick. It returns the bucket and the record's key.
func seedDueCIRecord(t *testing.T, client *bus.Client) (natsgo.KeyValue, string) {
	t.Helper()
	kv, err := client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: cistore.Bucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("create the CI bucket: %v", err)
	}
	sha := strings.Repeat("d", 40)
	key := cistore.Key("acme", "widgets", "7", sha)
	record := `{"owner":"acme","repo":"widgets","number":"7","sha":"` + sha + `",` +
		`"checks":{"build":{"name":"build","check_run_id":42,"url":"https://github.com/acme/widgets/runs/42","status":"completed","conclusion":"success","observed_at":"2026-09-28T13:00:00Z"}},` +
		`"suites":{},"last_event_at":` + strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10) + `,` +
		`"generation":1,"emitted_count":0,"settled_emitted":false,"schema":1}`
	if _, err := kv.Put(key, []byte(record)); err != nil {
		t.Fatalf("store the CI record: %v", err)
	}
	return kv, key
}

// ciChecksSubject is the settlement topic of seedDueCIRecord's commit.
var ciChecksSubject = contracts.GithubSubject("acme", "widgets", "pr.7.checks")

// ciSettled reports whether the stream holds a settlement of seedDueCIRecord's commit.
func ciSettled(t *testing.T, client *bus.Client) bool {
	t.Helper()
	_, err := client.JS().GetLastMsg(bus.Stream, ciChecksSubject)
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read the stream's last %s: %v", ciChecksSubject, err)
	}
	return true
}

// A listener that mounts no GitHub webhook route receives no check webhook, so it has nothing to
// record in the CI bucket and leaves it alone: no watch (whose initial scan of every record, 62 MB
// in production, is what a listener reaching NATS over a relayed link cannot take), no summary loop
// (the listeners that receive GitHub webhooks settle every record), and a /healthz that calls the CI
// cache not applicable instead of waiting on it. Seeded with a record any summary loop would settle,
// it creates no consumer on the CI bucket's stream, publishes no settlement, and reports healthy.
func TestAListenerWithoutAGitHubRouteLeavesTheCIBucketAlone(t *testing.T) {
	client := setupTestNATS(t)
	t.Cleanup(client.Close)
	kv, key := seedDueCIRecord(t, client)

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), "no-github-route")
	listener.waitHealthy(t)
	// The positive control, TestAListenerWithAGitHubRouteSettlesTheCIBucket, settles this seed
	// within a few one-second ticks of turning healthy; five seconds covers them.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		consumers := 0
		for range client.JS().ConsumerNames("KV_" + cistore.Bucket) {
			consumers++
		}
		if consumers != 0 {
			t.Fatalf("the CI bucket's stream has %d consumers under a listener with no GitHub route, want 0: it watches the bucket\n%s", consumers, listener.output.String())
		}
		if ciSettled(t, client) {
			t.Fatalf("a listener with no GitHub route published a settlement on %s: it runs the CI summary loop\n%s", ciChecksSubject, listener.output.String())
		}
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read the seeded CI record: %v", err)
	}
	if entry.Revision() != 1 {
		t.Fatalf("the seeded CI record is at revision %d, want 1: a listener with no GitHub route wrote it", entry.Revision())
	}

	status, health := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz"))
	if status != http.StatusOK || !strings.Contains(health, `"status":"healthy"`) || !strings.Contains(health, `"ci_cache":"not_applicable"`) {
		t.Fatalf("/healthz under a listener with no GitHub route: %d %s, want 200 healthy with ci_cache not_applicable", status, health)
	}
	if _, metrics := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/metrics")); strings.Contains(metrics, "envoy_ci_legacy_records_held") {
		t.Fatalf("/metrics under a listener with no GitHub route carries envoy_ci_legacy_records_held, which only a summary loop sets:\n%s", metrics)
	}
}

// A listener that mounts the GitHub webhook route opens the CI store and runs the summary loop, so
// the record seedDueCIRecord leaves settles: the settlement reaches the stream and the record is
// marked settled. It is the positive control for the test above.
func TestAListenerWithAGitHubRouteSettlesTheCIBucket(t *testing.T) {
	client := setupTestNATS(t)
	t.Cleanup(client.Close)
	kv, key := seedDueCIRecord(t, client)

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), "github-route", githubWebhookEnv...)
	listener.waitHealthy(t)
	healthy := time.Now()
	for !ciSettled(t, client) {
		if time.Since(healthy) > 15*time.Second {
			t.Fatalf("a listener with the GitHub route published no settlement on %s within 15s of turning healthy\n%s", ciChecksSubject, listener.output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("settled %s after turning healthy", time.Since(healthy).Round(10*time.Millisecond))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read the seeded CI record: %v", err)
		}
		var state cistore.State
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			t.Fatalf("decode the seeded CI record: %v", err)
		}
		if state.SettledEmitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the settled CI record is not marked settled: %s", entry.Value())
		}
	}
	if _, health := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz")); strings.Contains(health, "ci_cache") {
		t.Fatalf("/healthz under a listener with the GitHub route: %s, want no ci_cache field, as before", health)
	}
}

// mustRequest builds an unauthenticated request.
func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return request
}

// do sends request and returns its status and body.
func do(t *testing.T, request *http.Request) (int, string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, string(answer)
}

// The packages the listener calls into log through the default slog logger, and the SRE reads
// restarts with JSON-keyed queries. So a line from internal/store has to arrive in the listener's
// own format, carrying this machine's id, not in Go's text format beside it: a text line is one
// no query keyed on msg or machine_id can see. The role restore's count is the line that made
// this matter (it exists so a restart can be read at all), and the same default carries
// internal/bus and the reapers.
func TestListenerLogsFromOtherPackagesAreJSONWithTheMachineID(t *testing.T) {
	const machineID = "default-logger-json"
	client, err := bus.ConnectOwningStream([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	if _, err := registry.SetRole("ses_"+machineID, machineID, "role-"+machineID, false); err != nil {
		t.Fatalf("claim a role for the restart to restore: %v", err)
	}

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), machineID)
	listener.waitHealthy(t)

	var restored map[string]any
	for line := range strings.SplitSeq(listener.output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record["msg"] == "restored role claims" {
			restored = record
			break
		}
	}
	if restored == nil {
		t.Fatalf("internal/store's role-restore line is not a JSON record in the listener's output:\n%s", listener.output.String())
	}
	if restored["machine_id"] != machineID {
		t.Fatalf("the role-restore record's machine_id = %v, want %q; a query keyed on it cannot find this restart", restored["machine_id"], machineID)
	}
	if _, ok := restored["restored"].(float64); !ok {
		t.Fatalf("the role-restore record has no numeric restored field: %v", restored)
	}
	if _, ok := restored["keys"].(float64); !ok {
		t.Fatalf("the role-restore record has no numeric keys field: %v", restored)
	}
}
