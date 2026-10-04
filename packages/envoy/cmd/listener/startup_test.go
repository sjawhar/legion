package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
// balancer and the private DNS name already send the replacement traffic by then. Neither a webhook
// nor /v1 reads the durable, so the replacement serves both, and runs its role lane, while it waits:
// GitHub does not redeliver a refused delivery on its own, and a refused /v1 call is a failed
// Dispatch delivery or a lost publish. /healthz stays 200 "starting" until the durable binds: a
// healthy answer there would let ECS take the replacement for ready before it fans anything out,
// and a 503 would keep ECS from ever stopping the task that holds the durable.
func TestTheV1APIIsServedWhileAnotherTaskHoldsTheDurable(t *testing.T) {
	const (
		machineID = "v1-bind-window"
		secret    = "bind-window-secret"
		delivery  = "delivery-bind-window"
		target    = "ses_bind_window_target"
		holderID  = "ses_bind_window_holder"
		role      = "bind-window-role"
	)
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	old, _ := durableHeldElsewhere(t, uri, "listener-"+machineID)

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), machineID,
		"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET="+secret, "ENVOY_REVIEWER_APP_ID=1")
	listener.waitForOutput(t, "subscribe failed, retrying")

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

	if status, answer := callListener(t, listener.port, http.MethodGet, "/v1/sessions", ""); status != http.StatusOK {
		t.Fatalf("GET /v1/sessions while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.output.String())
	}
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+target+`","topics":[],"self_subscribed":true}`)
	status, answer = callListener(t, listener.port, http.MethodPost, "/v1/messages/send", `{"target_session":"`+target+`","message":"sent while the durable is held"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages/send while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.output.String())
	}
	var sent contracts.Envelope
	if err := json.Unmarshal([]byte(answer), &sent); err != nil {
		t.Fatalf("decode the send answer %q: %v", answer, err)
	}
	message, err = client.JS().GetLastMsg(bus.Stream, contracts.AgentSubject(target))
	if err != nil {
		t.Fatalf("the sent message is not on the stream: %v", err)
	}
	var stored contracts.Envelope
	if err := json.Unmarshal(message.Data, &stored); err != nil {
		t.Fatalf("decode the stored message: %v", err)
	}
	if stored.EventID != sent.EventID {
		t.Fatalf("the stream's last message for %s is event %q, want the sent %q", target, stored.EventID, sent.EventID)
	}

	// The role lane is open too: a role message reaches its holder through this listener's queue
	// subscription before the durable binds.
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+holderID+`","topics":[],"self_subscribed":true}`)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+holderID+`","role":"`+role+`"}`)
	frames := receiveAsSession(t, client, holderID)
	status, answer = callListener(t, listener.port, http.MethodPost, "/v1/messages/publish",
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"role message while the durable is held","source_session":"ses_bind_window_sender"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages/publish to a role while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.output.String())
	}
	select {
	case forwarded := <-frames:
		if forwarded.Topic != contracts.RoleTopicPrefix+role || !strings.HasPrefix(forwarded.DedupeKey, roleForwardDedupePrefix) {
			t.Fatalf("the holder received topic %q dedupe key %q, want the role forward of %s", forwarded.Topic, forwarded.DedupeKey, contracts.RoleTopicPrefix+role)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the role holder received no forward while another task holds the durable:\n%s", listener.output.String())
	}

	// Negative control: the replacement is neither healthy nor unhealthy while it waits. Healthy
	// would let ECS stop the old task before this one fans anything out; a 503 would never let ECS
	// stop it at all, and the container health check (curl -sf) would kill this one.
	status, answer = do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz"))
	if status != http.StatusOK || !strings.Contains(answer, `"status":"starting"`) {
		t.Fatalf("/healthz while another task holds the durable: status %d %q, want 200 starting", status, answer)
	}

	// The old task exits, which releases its binding.
	old.Close()
	listener.waitHealthy(t)
	listener.waitForOutput(t, "durable bound")
}

// Both tasks of one machine id run during a rolling deploy, and the replacement's role lane opens
// before its durable binds, so both subscribe notifications.role.> at once. They are one core-NATS
// queue group (envoy-listener-<machine id>), and NATS hands each message to one member of a group,
// so a role message reaches its holder once whichever task takes it: no publish is forwarded by
// both. A re-publish under the same dedupe key can land on the other task, whose dedupe cache has
// not seen it, and is forwarded again; the holder's pump drops that copy by its dedupe key, as it
// drops the copy each other machine's listener forwards today.
func TestARoleMessageReachesItsHolderOnceAcrossTwoTasksOfOneMachine(t *testing.T) {
	const (
		machineID = "two-tasks"
		holderID  = "ses_two_tasks_holder"
		role      = "two-tasks-role"
		messages  = 64
	)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	binary := buildListener(t)
	uri := client.Conn.ConnectedUrl()
	old := startListenerProcess(t, binary, uri, machineID)
	old.waitHealthy(t)
	replacement := startListenerProcess(t, binary, uri, machineID)
	replacement.waitForOutput(t, "subscribe failed, retrying")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("old task:\n%s\nreplacement:\n%s", old.output.String(), replacement.output.String())
		}
	})

	postListener(t, replacement.port, "/v1/interests/subscribe", `{"session_id":"`+holderID+`","topics":[],"self_subscribed":true}`)
	postListener(t, replacement.port, "/v1/roles/set", `{"session_id":"`+holderID+`","role":"`+role+`"}`)
	frames := receiveAsSession(t, client, holderID)
	// The old task reads the claim from the role bucket and the holder from its session cache, which
	// can trail the bucket and not yet hold the session the replacement just registered; it then reads
	// the holder from the session bucket itself (roleHolderSession), so its first lookup answers the
	// claim rather than releasing it as lapsed.
	if status, answer := callListener(t, old.port, http.MethodGet, "/v1/roles/"+role, ""); status != http.StatusOK {
		t.Fatalf("the old task's lookup of %s right after the replacement accepted %s's claim: status %d %s, want 200", role, holderID, status, answer)
	}

	publish := func(body string) contracts.Envelope {
		t.Helper()
		status, answer := callListener(t, old.port, http.MethodPost, "/v1/messages/publish", body)
		if status != http.StatusOK {
			t.Fatalf("POST /v1/messages/publish %s: status %d %q", body, status, answer)
		}
		var published contracts.Envelope
		if err := json.Unmarshal([]byte(answer), &published); err != nil {
			t.Fatalf("decode the publish answer %q: %v", answer, err)
		}
		return published
	}
	for index := range messages {
		publish(`{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"two tasks message ` + strconv.Itoa(index) + `","source_session":"ses_two_tasks_sender"}`)
	}
	keys := map[string]int{}
	for received := range messages {
		select {
		case frame := <-frames:
			if !strings.HasPrefix(frame.DedupeKey, roleForwardDedupePrefix) {
				t.Fatalf("the holder received dedupe key %q, want one prefixed %s", frame.DedupeKey, roleForwardDedupePrefix)
			}
			keys[frame.DedupeKey]++
		case <-time.After(10 * time.Second):
			t.Fatalf("the holder received %d of %d role messages", received, messages)
		}
	}
	select {
	case frame := <-frames:
		t.Fatalf("the holder received a frame past the %d published: %+v", messages, frame)
	case <-time.After(time.Second):
	}
	if len(keys) != messages {
		t.Fatalf("the holder received %d distinct dedupe keys over %d frames, want %d: a publish was forwarded twice", len(keys), messages, messages)
	}
	roleForwards := func(p *listenerProcess) int {
		return countRecords(p, "listener role forwarded", func(record map[string]any) bool {
			return record["topic"] == contracts.RoleTopicPrefix+role
		})
	}
	byOld, byReplacement := roleForwards(old), roleForwards(replacement)
	if byOld+byReplacement != messages {
		t.Fatalf("the two tasks logged %d + %d role forwards, want %d in all", byOld, byReplacement, messages)
	}
	// NATS spreads a group's messages over its members; all 64 on one has probability 2^-63.
	if byOld == 0 || byReplacement == 0 {
		t.Fatalf("the old task forwarded %d and the replacement %d of %d: the replacement's role lane is not a member of the machine's queue group before its durable binds", byOld, byReplacement, messages)
	}

	// A re-publish under one dedupe key: either the task that forwarded the first copy skips it,
	// or the other task forwards it again, for the holder's pump to drop.
	const repeatKey = "two-tasks-repeat"
	events := map[string]bool{}
	for range 2 {
		published := publish(`{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"two tasks repeat","source_session":"ses_two_tasks_sender","dedupe_key":"` + repeatKey + `"}`)
		events[published.EventID] = true
	}
	ofRepeat := func(record map[string]any) bool { return events[record["event_id"].(string)] }
	handled := func() int {
		total := 0
		for _, p := range []*listenerProcess{old, replacement} {
			total += countRecords(p, "listener role forwarded", ofRepeat) + countRecords(p, "listener role dedupe skip", ofRepeat)
		}
		return total
	}
	waitFor(t, 10*time.Second, "the two tasks to handle the repeated key twice", func() bool { return handled() == 2 })
	repeats := 0
	for collecting := true; collecting; {
		select {
		case frame := <-frames:
			if frame.DedupeKey != roleForwardDedupePrefix+repeatKey {
				t.Fatalf("the holder received dedupe key %q, want %s", frame.DedupeKey, roleForwardDedupePrefix+repeatKey)
			}
			repeats++
		case <-time.After(time.Second):
			collecting = false
		}
	}
	if repeats != 1 && repeats != 2 {
		t.Fatalf("the holder received %d frames for the repeated key, want 1 or 2", repeats)
	}

	if err := old.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the old task: %v", err)
	}
	old.waitExit(t, "SIGTERM")
	replacement.waitHealthy(t)
	replacement.waitForOutput(t, "durable bound")
}

// receiveAsSession stands in for session sessionID's agent pump on its agent subject: it answers
// each frame with the empty receipt the pump returns and hands the frame's envelope to the channel.
func receiveAsSession(t *testing.T, client *bus.Client, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	frames := make(chan contracts.Envelope, 256)
	subscription, err := client.Conn.Subscribe(contracts.AgentSubject(sessionID), func(msg *natsgo.Msg) {
		var envelope contracts.Envelope
		_ = json.Unmarshal(msg.Data, &envelope)
		_ = msg.Respond(nil)
		frames <- envelope
	})
	if err != nil {
		t.Fatalf("subscribe as %s: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription as %s: %v", sessionID, err)
	}
	return frames
}

// callListener calls a listener /v1 route as the test's shared-token caller and returns its status
// and body.
func callListener(t *testing.T, port int, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return do(t, request)
}

// countRecords counts the JSON records in the listener's output whose msg is name and that match
// accepts.
func countRecords(p *listenerProcess, name string, match func(map[string]any) bool) int {
	count := 0
	for line := range strings.SplitSeq(p.output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["msg"] != name {
			continue
		}
		if match(record) {
			count++
		}
	}
	return count
}

// A listener built before the KV key check stored role claims, interests, sessions and CI records
// under keys this one refuses to write (testnats.LegacyKeys), and every listener on that NATS reads
// the first three buckets as it starts, and the CI bucket when it mounts the GitHub webhook route.
// It must start healthy over them and serve the ordinary keys beside them: one stored role must not
// keep every listener from starting.
func TestTheListenerStartsOverKeysAnEarlierListenerStored(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
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

// Two packages the listener calls into log two different ways, and both are load-bearing.
// internal/store's and internal/kvwatch's lines exist for the SRE to query a restart (the role
// restore and every cache's warm-up cost), so they go through the listener's own handler and
// arrive as JSON records with this machine's id. internal/bus's lines, and the stdlib log
// package's, must stay in Go's text format: the deployed CloudWatch metric filters for publish
// failures, webhook refusals and dropped stream subjects are space-delimited patterns anchored on
// that format's date and time prefix (defined in the deployment repository's listener infrastructure), so
// routing them into the JSON handler with slog.SetDefault stops three alarms without failing
// anything. This test holds both halves, so reintroducing that SetDefault reds it. The listener
// mounts the GitHub route, so all three caches warm up.
func TestListenerLogsFromOtherPackagesKeepTheirHandlers(t *testing.T) {
	const machineID = "logger-split"
	container, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	if _, err := registry.SetRole("ses_"+machineID, machineID, "role-"+machineID, false); err != nil {
		t.Fatalf("claim a role for the restart to restore: %v", err)
	}
	registry.StopWatch()
	client.Close()

	listener := startListenerProcess(t, buildListener(t), uri, machineID, githubWebhookEnv...)
	listener.waitHealthy(t)

	restored := listenerJSONRecord(t, listener, "restored role claims")
	if restored == nil {
		t.Fatalf("internal/store's role-restore line is not a JSON record in the listener's output:\n%s", listener.output.String())
	}
	if restored["machine_id"] != machineID {
		t.Fatalf("the role-restore record's machine_id = %v, want %q; a query keyed on it cannot find this restart", restored["machine_id"], machineID)
	}
	if _, ok := restored["restored"].(float64); !ok {
		t.Fatalf("the role-restore record has no numeric restored field: %v", restored)
	}
	if _, ok := restored["delete_markers"].(float64); !ok {
		t.Fatalf("the role-restore record has no numeric delete_markers field: %v", restored)
	}
	// Every cache's warm-up line, which names what a restart spent before it served. The CI
	// cache's readiness is not on the healthy path, so its line can arrive just after.
	for _, cache := range []string{"interest registry", "session registry", "cistore"} {
		msg := cache + " cache warm-up"
		warmUp := listenerJSONRecord(t, listener, msg)
		for deadline := time.Now().Add(30 * time.Second); warmUp == nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			warmUp = listenerJSONRecord(t, listener, msg)
		}
		if warmUp == nil {
			t.Fatalf("internal/kvwatch's %q line is not a JSON record in the listener's output:\n%s", msg, listener.output.String())
		}
		if warmUp["machine_id"] != machineID {
			t.Fatalf("the %q record's machine_id = %v, want %q", msg, warmUp["machine_id"], machineID)
		}
	}

	// internal/bus logs its connection lines through the default logger. Taking the server away is
	// how this test gets one to read.
	if err := container.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop the listener's NATS: %v", err)
	}
	textLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} .*envoy nats disconnected`)
	deadline := time.Now().Add(30 * time.Second)
	for !textLine.MatchString(listener.output.String()) {
		if time.Now().After(deadline) {
			if listenerJSONRecord(t, listener, "envoy nats disconnected") != nil {
				t.Fatalf("internal/bus's disconnect line is a JSON record: the default logger was replaced, and the deployed text metric filters for publish failures, webhook refusals and dropped stream subjects no longer match:\n%s", listener.output.String())
			}
			t.Fatalf("no disconnect line in Go's text format after the server stopped:\n%s", listener.output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// listenerJSONRecord is the first line of the listener's output that parses as a JSON object whose
// msg is name, or nil when no line does.
func listenerJSONRecord(t *testing.T, p *listenerProcess, name string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(p.output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record["msg"] == name {
			return record
		}
	}
	return nil
}
