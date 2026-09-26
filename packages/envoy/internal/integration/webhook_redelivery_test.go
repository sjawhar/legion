package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/webhook"
)

// GitHub, Slack and Ghost Wispr redeliver a webhook under the delivery id it first carried (GitHub's
// X-GitHub-Delivery GUID, Slack's event_id, Ghost Wispr's X-GhostWispr-Delivery). When the first
// attempt already reached the stream — it was answered too slowly, or failed after publishing part
// of a fan-out — the redelivery must not put a second copy there. A different delivery id must.

const redeliverySecret = "redelivery-secret"

func TestWebhookRedelivery_AGitHubRedeliveryLandsOnceAndAnotherDeliveryLands(t *testing.T) {
	env := setupTestEnv(t)
	handler := webhook.GitHubHandler(redeliverySecret, "@legion", "", env.client, unusedCIRecorder(t))
	body := `{
		"action": "submitted",
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"},
		"pull_request": {"number": 7, "head": {"sha": "abcdef1234567890abcdef1234567890abcdef12"}},
		"review": {"state": "approved", "body": "ship it", "commit_id": "abcdef1234567890abcdef1234567890abcdef12", "user": {"login": "reviewer"}},
		"sender": {"login": "reviewer", "type": "User"}
	}`

	postGitHub(t, handler, "pull_request_review", "delivery-a", body)
	postGitHub(t, handler, "pull_request_review", "delivery-a", body) // GitHub's redelivery
	postGitHub(t, handler, "pull_request_review", "delivery-b", body)

	got := streamDedupeKeys(t, env, "notifications.github.acme.widgets.pr.7.review")
	if want := []string{"github.delivery-a", "github.delivery-b"}; !slices.Equal(got, want) {
		t.Fatalf("stream holds %v on the review topic, want %v", got, want)
	}
}

// One GitHub delivery fans out to several topics under one dedupe key (a comment that mentions the
// trigger): each copy lands once, and the redelivery adds none.
func TestWebhookRedelivery_AGitHubFanOutLandsOncePerTopic(t *testing.T) {
	env := setupTestEnv(t)
	handler := webhook.GitHubHandler(redeliverySecret, "@legion", "", env.client, unusedCIRecorder(t))
	body := `{
		"action": "created",
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"},
		"issue": {"number": 7, "title": "Widget", "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/7"}},
		"comment": {"body": "@legion please look", "user": {"login": "author"}},
		"sender": {"login": "author", "type": "User"}
	}`

	postGitHub(t, handler, "issue_comment", "delivery-c", body)
	postGitHub(t, handler, "issue_comment", "delivery-c", body) // GitHub's redelivery

	for _, topic := range []string{
		"notifications.github.acme.widgets.pr.7.mention",
		"notifications.github.acme.widgets.mention",
		"notifications.github.acme.widgets.pr.7.comment",
	} {
		if got, want := streamDedupeKeys(t, env, topic), []string{"github.delivery-c"}; !slices.Equal(got, want) {
			t.Errorf("stream holds %v on %s, want %v", got, topic, want)
		}
	}
}

func TestWebhookRedelivery_ASlackRetryLandsOnceAndAnotherEventLands(t *testing.T) {
	env := setupTestEnv(t)
	handler := webhook.SlackHandler(redeliverySecret, env.client)
	event := func(eventID string) string {
		return fmt.Sprintf(`{"type":"event_callback","team_id":"T1","event_id":%q,"event":{"type":"message","channel":"C1","user":"U1","text":"hello","ts":"1700000000.000100"}}`, eventID)
	}

	postSlack(t, handler, event("Ev1"))
	postSlack(t, handler, event("Ev1")) // Slack's retry
	postSlack(t, handler, event("Ev2"))

	got := streamDedupeKeys(t, env, "notifications.slack.T1.C1.message")
	if want := []string{"slack.Ev1", "slack.Ev2"}; !slices.Equal(got, want) {
		t.Fatalf("stream holds %v on the channel topic, want %v", got, want)
	}
}

func TestWebhookRedelivery_AGhostWisprRedeliveryLandsOnceAndAnotherDeliveryLands(t *testing.T) {
	env := setupTestEnv(t)
	handler := webhook.GhostWisprHandler(redeliverySecret, env.client)
	body := `{"event_type":"summary_ready","payload":{"session_id":"20260326041629","type":"summary_ready"}}`

	postGhostWispr(t, handler, "gw-1", body)
	postGhostWispr(t, handler, "gw-1", body) // Ghost Wispr's redelivery
	postGhostWispr(t, handler, "gw-2", body)

	got := streamDedupeKeys(t, env, contracts.GhostWisprSubject("20260326041629", "summary.ready"))
	if want := []string{"ghostwispr.gw-1", "ghostwispr.gw-2"}; !slices.Equal(got, want) {
		t.Fatalf("stream holds %v on the summary topic, want %v", got, want)
	}
}

// unusedCIRecorder fails the test if the handler records CI state: none of these events is a
// pull_request or CI event, so a call means the fixture is not the event it claims to be.
func unusedCIRecorder(t *testing.T) webhook.CIRecorder {
	t.Helper()
	fail := func(string) error {
		t.Errorf("unexpected CI record: the fixture is not the event it claims to be")
		return nil
	}
	return webhook.CIRecorderFuncs{
		RecordFunc:      func(contracts.CIObservation) error { return fail("check") },
		RecordSuiteFunc: func(contracts.CIObservation) error { return fail("suite") },
		RecordHeadFunc:  func(string, string, string, string, string) error { return fail("head") },
	}
}

func postGitHub(t *testing.T, handler http.Handler, event, delivery, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex(redeliverySecret, body))
	serveOK(t, handler, req)
}

func postSlack(t *testing.T, handler http.Handler, body string) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhook/slack", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hmacHex(redeliverySecret, "v0:"+timestamp+":"+body))
	serveOK(t, handler, req)
}

func postGhostWispr(t *testing.T, handler http.Handler, delivery, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", strings.NewReader(body))
	req.Header.Set("X-GhostWispr-Delivery", delivery)
	req.Header.Set("X-GhostWispr-Event", "summary_ready")
	req.Header.Set("X-GhostWispr-Signature", "sha256="+hmacHex(redeliverySecret, body))
	serveOK(t, handler, req)
}

// serveOK serves req and requires 200: the sender marks a delivery successful only then, so a
// redelivery the stream recognises must still be answered 200.
func serveOK(t *testing.T, handler http.Handler, req *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s answered %d %q, want 200", req.URL.Path, rec.Code, rec.Body.String())
	}
}

func hmacHex(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// streamDedupeKeys returns the dedupe key of every message the notification stream holds on
// subject, in stream order.
func streamDedupeKeys(t *testing.T, env *testEnv, subject string) []string {
	t.Helper()
	info, err := env.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("stream info for %s: %v", subject, err)
	}
	held := int(info.State.Subjects[subject])
	keys := make([]string, 0, held)
	if held == 0 {
		return keys
	}
	sub, err := env.client.JS().SubscribeSync(subject, natsgo.BindStream(bus.Stream), natsgo.OrderedConsumer(), natsgo.DeliverAll())
	if err != nil {
		t.Fatalf("read %s: %v", subject, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	for range held {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("read message %d of %d on %s: %v", len(keys)+1, held, subject, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Fatalf("decode message on %s: %v", subject, err)
		}
		keys = append(keys, item.DedupeKey)
	}
	return keys
}
