package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// webhookSecret signs every delivery these tests send.
	webhookSecret = "frozen-bus-secret"
	// githubRecordWithin is how long a CI delivery may take to answer a bus that stopped answering:
	// the CI store's recordWait (4 s) and a margin, inside GitHub's 10 s.
	githubRecordWithin = 5 * time.Second
	// slackWithin is the Slack route's slackAnswerBudget (2.5 s) and a margin: Slack takes an event
	// it has no answer to within 3 s as failed.
	slackWithin = 3 * time.Second
	// callerGivesUpAfter is when the caller of a Ghost Wispr or GitHub publish drops its request,
	// and callerGoneWithin how soon after its start the route must then have answered: before the
	// bus's own 5 s bound.
	callerGivesUpAfter = time.Second
	callerGoneWithin   = 2 * time.Second
)

// frozenWebhookBus is a NATS server of its own, which the test can freeze, and the listener's bus
// client on it.
type frozenWebhookBus struct {
	server *testnats.Server
	client *bus.Client
}

func newFrozenWebhookBus(t *testing.T) *frozenWebhookBus {
	t.Helper()
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	return &frozenWebhookBus{server: server, client: client}
}

// freeze stops the server's traffic in both directions while every connection stays up, until the
// test ends.
func (b *frozenWebhookBus) freeze(t *testing.T) {
	t.Helper()
	t.Cleanup(b.server.Freeze(t))
}

// serveDelivery serves req and reports how long the handler took to answer.
func serveDelivery(handler http.Handler, req *http.Request) (*httptest.ResponseRecorder, time.Duration) {
	recorder := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(recorder, req)
	return recorder, time.Since(started)
}

func githubDelivery(event, delivery, body string) *http.Request {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func slackDelivery(body string) *http.Request {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte("v0:" + timestamp + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/slack", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func ghostWisprDelivery(delivery, body string) *http.Request {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", strings.NewReader(body))
	req.Header.Set("X-GhostWispr-Delivery", delivery)
	req.Header.Set("X-GhostWispr-Event", "summary_ready")
	req.Header.Set("X-GhostWispr-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

// wantDeliveryServed asserts the route's 200 "ok".
func wantDeliveryServed(t *testing.T, route string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("%s with the bus answering: status %d, body %q; want 200 ok", route, recorder.Code, recorder.Body.String())
	}
}

// wantDeliveryFailed asserts the answer a delivery the listener could not record or publish has
// always had (webhook's deliveryFailed): 503 "service unavailable", inside within.
func wantDeliveryFailed(t *testing.T, route string, recorder *httptest.ResponseRecorder, elapsed, within time.Duration) {
	t.Helper()
	if elapsed >= within {
		t.Fatalf("%s answered after %s (status %d, body %q), want inside %s - a bus that stopped answering must be answered inside the caller's wait",
			route, elapsed, recorder.Code, recorder.Body.String(), within)
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != "service unavailable\n" {
		t.Fatalf("%s with the bus silent: status %d, body %q; want 503 service unavailable", route, recorder.Code, recorder.Body.String())
	}
	t.Logf("%s with the bus silent: 503 in %s", route, elapsed)
}

// A check_suite or check_run delivery writes its commit's CI record, and nats.go's KV reads and
// writes take no context and wait out the JetStream MaxWait (10 s), as long as GitHub's own wait for
// an answer. On a bus that went silent without closing the connection the delivery must still get
// the answer a record that failed always got, 503, inside the CI store's 4 s record wait. The
// negative control is a delivery recorded on the same store before the freeze.
func TestAGitHubCIDeliveryAnswersAFrozenBusInsideTheRecordWait(t *testing.T) {
	frozen := newFrozenWebhookBus(t)
	store, err := cistore.Open(frozen.client.Conn, logging.New("test"), cistore.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the CI store: %v", err)
	}
	t.Cleanup(store.StopWatch)
	ready, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(ready); err != nil {
		t.Fatalf("wait for the CI cache: %v", err)
	}
	handler := webhook.GitHubHandler(webhookSecret, "@legion", "", frozen.client, store)
	checkSuite := func(number string) string {
		return `{"action":"completed","check_suite":{"id":900,"status":"completed","conclusion":"success",` +
			`"updated_at":"2026-10-26T03:01:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","app":{"id":77},` +
			`"pull_requests":[{"number":` + number + `}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`
	}
	checkRun := `{"action":"completed","check_run":{"id":987654321,"name":"unit-tests","status":"completed","conclusion":"failure",` +
		`"completed_at":"2026-10-26T03:00:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","check_suite":{"id":900},` +
		`"pull_requests":[{"number":43}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`

	recorder, _ := serveDelivery(handler, githubDelivery("check_suite", "delivery-control", checkSuite("41")))
	wantDeliveryServed(t, "a check_suite delivery", recorder)

	frozen.freeze(t)
	for _, delivery := range []struct{ event, body string }{
		{"check_suite", checkSuite("42")},
		{"check_run", checkRun},
	} {
		recorder, elapsed := serveDelivery(handler, githubDelivery(delivery.event, "delivery-frozen-"+delivery.event, delivery.body))
		wantDeliveryFailed(t, "a "+delivery.event+" delivery", recorder, elapsed, githubRecordWithin)
	}
}

// A webhook's publish waits for the bus within the request: Slack's within the 3 s Slack waits for
// an answer, Ghost Wispr's and a GitHub event's until their caller drops the request. Each used
// the bus's own 5 s bound alone, so a bus that went silent was answered after Slack had given up,
// and after the others' callers had gone. Each must still get the answer a failed publish always
// got, 503. The negative control is each route served on the same bus before the freeze.
func TestTheWebhookPublishesAnswerAFrozenBusInsideTheirCallersWait(t *testing.T) {
	frozen := newFrozenWebhookBus(t)
	slack := webhook.SlackHandler(webhookSecret, frozen.client)
	ghostWispr := webhook.GhostWisprHandler(webhookSecret, frozen.client)
	github := webhook.GitHubHandler(webhookSecret, "@legion", "", frozen.client, noCI{})
	slackEvent := func(eventID string) string {
		return `{"type":"event_callback","event_id":"` + eventID + `","team_id":"T01234567",` +
			`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello world","ts":"1700000000.000100"}}`
	}
	summary := `{"event_type":"summary_ready","payload":{"session_id":"20261026041629","type":"summary_ready"}}`
	comment := func(id string) string {
		return `{"action":"created","repository":{"name":"widgets","owner":{"login":"acme"},"full_name":"acme/widgets"},` +
			`"issue":{"number":7,"title":"t","html_url":"https://github.com/acme/widgets/issues/7"},` +
			`"comment":{"id":` + id + `,"body":"no mention here","html_url":"https://github.com/acme/widgets/issues/7#issuecomment-` + id + `","user":{"login":"someone","type":"User"}},` +
			`"sender":{"login":"someone","type":"User"}}`
	}

	recorder, _ := serveDelivery(slack, slackDelivery(slackEvent("Ev_control")))
	wantDeliveryServed(t, "a Slack event", recorder)
	recorder, _ = serveDelivery(ghostWispr, ghostWisprDelivery("gw-control", summary))
	wantDeliveryServed(t, "a Ghost Wispr event", recorder)
	recorder, _ = serveDelivery(github, githubDelivery("issue_comment", "delivery-comment-control", comment("101")))
	wantDeliveryServed(t, "a GitHub issue_comment", recorder)

	frozen.freeze(t)
	t.Run("slack", func(t *testing.T) {
		recorder, elapsed := serveDelivery(slack, slackDelivery(slackEvent("Ev_frozen")))
		wantDeliveryFailed(t, "a Slack event", recorder, elapsed, slackWithin)
	})
	for _, route := range []struct {
		name    string
		handler http.Handler
		req     *http.Request
	}{
		{"a Ghost Wispr event", ghostWispr, ghostWisprDelivery("gw-frozen", summary)},
		{"a GitHub issue_comment", github, githubDelivery("issue_comment", "delivery-comment-frozen", comment("102"))},
	} {
		t.Run(route.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), callerGivesUpAfter)
			defer cancel()
			recorder, elapsed := serveDelivery(route.handler, route.req.WithContext(ctx))
			wantDeliveryFailed(t, route.name, recorder, elapsed, callerGoneWithin)
		})
	}
}
