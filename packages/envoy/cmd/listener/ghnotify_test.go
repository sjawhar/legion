package main

// The listener's half of the GitHub notifications poller: a listener without its token runs none
// and sends GitHub nothing, one with it reports the poller's health on /healthz without letting a
// degraded poller touch its own status or its webhooks, and a lookback past 48 hours is refused
// with the webhooks served as before.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/ghnotify"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The poller publishes through the listener's own bus client, which is the one thing the listener
// gives it that it cannot build itself. A client that no longer reports JetStream's duplicate
// verdict would make the poller count every replayed comment as a new event.
var _ ghnotify.Publisher = (*bus.Client)(nil)

// fakeGitHubAPI stands in for api.github.com for a listener under test: it records every request
// it receives, so a test can prove the listener sent none, and answers each with answer.
type fakeGitHubAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func startFakeGitHubAPI(t *testing.T, answer http.HandlerFunc) *fakeGitHubAPI {
	t.Helper()
	fake := &fakeGitHubAPI{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, r.Method+" "+r.URL.RequestURI())
		fake.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(fake.Server.Close)
	return fake
}

// received is every request the fake has answered, as "<method> <target>" lines.
func (f *fakeGitHubAPI) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// listenerHealth is the listener's /healthz answer, with the poller's own field.
type listenerHealth struct {
	Status              string `json:"status"`
	GithubNotifications *struct {
		Status        string `json:"status"`
		LastSuccess   string `json:"last_success"`
		Unreadable24h int    `json:"unreadable_24h"`
	} `json:"github_notifications"`
}

// health reads /healthz once.
func (p *listenerProcess) health(t *testing.T) listenerHealth {
	t.Helper()
	status, answer := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(p.port)+"/healthz"))
	if status != http.StatusOK {
		t.Fatalf("/healthz: status %d %q, want 200", status, answer)
	}
	var health listenerHealth
	if err := json.Unmarshal([]byte(answer), &health); err != nil {
		t.Fatalf("decode /healthz %q: %v", answer, err)
	}
	return health
}

// notificationsWebhookEnv mounts the GitHub webhook route, so a test can prove the listener still
// takes a delivery while its poller is degraded or refused.
var notificationsWebhookEnv = []string{
	"ENVOY_WEBHOOKS=github",
	"ENVOY_GITHUB_WEBHOOK_SECRET=" + notificationsWebhookSecret,
	"ENVOY_REVIEWER_APP_ID=1",
}

const notificationsWebhookSecret = "notifications-webhook-secret"

// postGitHubPush sends one signed push delivery to the listener's GitHub webhook route and
// requires a 200, then requires its envelope on the stream: the webhooks serve as before.
func postGitHubPush(t *testing.T, listener *listenerProcess, client *bus.Client, repo, delivery string) {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("b", 40) + `","before":"` + strings.Repeat("a", 40) + `",` +
		`"pusher":{"name":"example-author"},"sender":{"login":"example-author","type":"User"},` +
		`"repository":{"name":"` + repo + `","owner":{"login":"acme"},"full_name":"acme/` + repo + `"}}`)
	mac := hmac.New(sha256.New, []byte(notificationsWebhookSecret))
	mac.Write(body)
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if status, answer := do(t, request); status != http.StatusOK {
		t.Fatalf("POST /webhook/github: status %d %q, want 200\n%s", status, answer, listener.Output.String())
	}
	message, err := client.JS().GetLastMsg(bus.Stream, contracts.GithubSubject("acme", repo, "push.branch.main"))
	if err != nil {
		t.Fatalf("the accepted push is not on the stream: %v\n%s", err, listener.Output.String())
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		t.Fatalf("decode the published push: %v", err)
	}
	if envelope.SourceEventID != delivery {
		t.Fatalf("the stream's last push is delivery %q, want %q", envelope.SourceEventID, delivery)
	}
}

// errorRecords is every JSON record in the listener's output logged at ERROR. The listener's own
// packages log JSON through its handler; internal/bus and the stdlib log package keep Go's text
// format, which is not a record and is not counted.
func errorRecords(p *listenerProcess) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(p.Output.String(), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

// TestListenerWithoutTokenStartsNoPoller: the poller is optional, and every listener but the one
// its deployment gives the token to runs none. With ENVOY_GITHUB_NOTIFICATIONS_API_BASE naming a
// server that records every request it receives and no token set, the listener reaches a healthy
// start, sends that server nothing, logs no poller start line, and answers /healthz with no
// github_notifications field.
func TestListenerWithoutTokenStartsNoPoller(t *testing.T) {
	uri := testnats.URL(t)
	github := startFakeGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "ghnotify-no-token",
		"ENVOY_GITHUB_NOTIFICATIONS_API_BASE="+github.URL)
	listener.waitHealthy(t)

	// The listener is past the poller's start site by now; two seconds is the window A13 names for
	// a poll that would have been made.
	time.Sleep(2 * time.Second)
	if requests := github.received(); len(requests) != 0 {
		t.Errorf("a listener with no token sent GitHub %d requests: %v", len(requests), requests)
	}
	if strings.Contains(listener.Output.String(), notificationsPollerStartedLine) {
		t.Errorf("a listener with no token logged %q:\n%s", notificationsPollerStartedLine, listener.Output.String())
	}
	if field := listener.health(t).GithubNotifications; field != nil {
		t.Errorf("/healthz carries github_notifications %+v on a listener with no poller", *field)
	}
}

// TestListenerWithTokenReportsHealth: a poller that cannot read GitHub is a report, never the
// listener's own health. Against a GitHub that answers 401 to everything, with the degraded window
// shortened to two seconds, /healthz carries github_notifications "degraded" while the listener
// itself stays healthy, the webhook route still takes a delivery and publishes it, the 401 is one
// ERROR line, and the process is still running.
func TestListenerWithTokenReportsHealth(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	github := startFakeGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), "ghnotify-401",
		append([]string{
			"ENVOY_GITHUB_NOTIFICATIONS_TOKEN=notifications-token",
			"ENVOY_GITHUB_NOTIFICATIONS_API_BASE=" + github.URL,
			"ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER=2s",
		}, notificationsWebhookEnv...)...)
	listener.waitHealthy(t)
	listener.WaitForOutput(t, notificationsPollerStartedLine)

	var health listenerHealth
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		health = listener.health(t)
		if health.GithubNotifications != nil && health.GithubNotifications.Status == "degraded" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if health.GithubNotifications == nil || health.GithubNotifications.Status != "degraded" {
		t.Fatalf("/healthz github_notifications = %+v, want a degraded poller:\n%s", health.GithubNotifications, listener.Output.String())
	}
	if health.Status != "healthy" {
		t.Errorf("/healthz status = %q while its poller is degraded, want healthy", health.Status)
	}

	postGitHubPush(t, listener, client, "ghnotify-degraded", "delivery-ghnotify-degraded")

	if records := errorRecords(listener); len(records) != 1 {
		t.Errorf("the listener logged %d ERROR records for one 401, want exactly 1: %v", len(records), records)
	}
	select {
	case <-listener.Exited:
		t.Fatalf("the listener exited while its poller was degraded:\n%s", listener.Output.String())
	default:
	}
}

// TestListenerRefusesLookbackOver48h: a lookback past 48 hours could republish events older than
// the stream's 72-hour duplicate window, so the listener starts no poller for one. It logs one
// ERROR line naming the value, sends GitHub nothing, and serves its webhooks as before.
func TestListenerRefusesLookbackOver48h(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	github := startFakeGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), "ghnotify-lookback",
		append([]string{
			"ENVOY_GITHUB_NOTIFICATIONS_TOKEN=notifications-token",
			"ENVOY_GITHUB_NOTIFICATIONS_API_BASE=" + github.URL,
			"ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK=49h",
		}, notificationsWebhookEnv...)...)
	listener.waitHealthy(t)

	records := errorRecords(listener)
	refusals := 0
	for _, record := range records {
		if record["msg"] == notificationsPollerRefusedLine {
			refusals++
			if value, _ := record[notificationsLookbackVar].(string); value != "49h" {
				t.Errorf("the refusal names %s=%q, want 49h: %v", notificationsLookbackVar, value, record)
			}
		}
	}
	if refusals != 1 {
		t.Fatalf("the listener logged %d %q records for a 49h lookback, want exactly 1: %v", refusals, notificationsPollerRefusedLine, records)
	}
	if strings.Contains(listener.Output.String(), notificationsPollerStartedLine) {
		t.Errorf("a refused lookback still started a poller:\n%s", listener.Output.String())
	}
	if requests := github.received(); len(requests) != 0 {
		t.Errorf("a refused poller sent GitHub %d requests: %v", len(requests), requests)
	}
	if field := listener.health(t).GithubNotifications; field != nil {
		t.Errorf("/healthz carries github_notifications %+v for a refused poller", *field)
	}

	postGitHubPush(t, listener, client, "ghnotify-lookback", "delivery-ghnotify-lookback")
}
