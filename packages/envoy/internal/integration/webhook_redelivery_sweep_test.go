package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The whole path: GitHub delivers to the listener's GitHub webhook, the delivery fails, the sweep
// asks GitHub to redeliver it, and GitHub re-sends the original request to the listener.
//   - A delivery the listener refused while starting (503) is recovered: the stream then holds it.
//   - A delivery that reached the stream but that GitHub recorded as failed (the reply came too
//     late) is redelivered, and the stream still holds it once.
//   - A delivery the listener refused as malformed (400: a body over its cap) is never asked for
//     again, and the sweep says so.
func TestWebhookRedelivery_TheSweepRecoversFailedDeliveriesThroughTheListener(t *testing.T) {
	env := setupTestEnv(t)
	handler := webhook.GitHubHandler(redeliverySecret, "@legion", "", env.client, unusedCIRecorder(t))
	var started atomic.Bool
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !started.Load() {
			http.Error(w, `{"error":"service starting"}`, http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(listener.Close)
	send := func(attempt githubapptest.Attempt) int {
		req, err := http.NewRequest(http.MethodPost, listener.URL+"/webhook/github", bytes.NewReader(attempt.Payload))
		if err != nil {
			t.Fatalf("build delivery: %v", err)
		}
		req.Header.Set("X-GitHub-Delivery", attempt.GUID)
		req.Header.Set("X-GitHub-Event", attempt.Event)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex(redeliverySecret, string(attempt.Payload)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("deliver %s: %v", attempt.GUID, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	key, pemText := githubapptest.Key(t)
	github := githubapptest.NewWebhook(t, &key.PublicKey, "Iv1.integration", send)

	review := []byte(`{
		"action": "submitted",
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"},
		"pull_request": {"number": 7, "head": {"sha": "abcdef1234567890abcdef1234567890abcdef12"}},
		"review": {"state": "approved", "body": "ship it", "commit_id": "abcdef1234567890abcdef1234567890abcdef12", "user": {"login": "reviewer"}},
		"sender": {"login": "reviewer", "type": "User"}
	}`)
	whileStarting := githubapptest.Attempt{GUID: "delivery-while-starting", Event: "pull_request_review", Payload: review}
	if code := send(whileStarting); code != http.StatusServiceUnavailable {
		t.Fatalf("delivery while starting answered %d, want 503", code)
	}
	refused := github.Record(whileStarting, "submitted", 7, http.StatusServiceUnavailable, time.Now().Add(-3*time.Minute))

	started.Store(true)
	tooLate := githubapptest.Attempt{GUID: "delivery-too-late", Event: "pull_request_review", Payload: review}
	if code := send(tooLate); code != http.StatusOK {
		t.Fatalf("delivery after start answered %d, want 200", code)
	}
	late := github.Record(tooLate, "submitted", 7, http.StatusGatewayTimeout, time.Now().Add(-2*time.Minute))

	oversized := githubapptest.Attempt{GUID: "delivery-oversized", Event: "push", Payload: []byte(`{"ref":"refs/heads/main","padding":"` + strings.Repeat("x", 1<<20) + `","repository":{"name":"widgets","owner":{"login":"acme"}}}`)}
	if code := send(oversized); code != http.StatusBadRequest {
		t.Fatalf("oversized delivery answered %d, want 400", code)
	}
	github.Record(oversized, "", 7, http.StatusBadRequest, time.Now().Add(-time.Minute))

	state, err := redeliver.OpenState(env.client.JS())
	if err != nil {
		t.Fatalf("open redelivery state: %v", err)
	}
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.integration", ClientSecret: "secret", PEM: pemText}, github.URL())
	if err != nil {
		t.Fatalf("App client: %v", err)
	}
	var logs bytes.Buffer
	sweeper := &redeliver.Sweeper{GitHub: client, State: state, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got, want := slices.Sorted(slices.Values(github.Requests())), slices.Sorted(slices.Values([]int64{refused.ID, late.ID})); !slices.Equal(got, want) {
		t.Fatalf("redelivery requests %v, want the two 5xx deliveries %v", got, want)
	}
	got := streamDedupeKeys(t, env, "notifications.github.acme.widgets.pr.7.review")
	if want := []string{"github.delivery-too-late", "github.delivery-while-starting"}; !slices.Equal(got, want) {
		t.Fatalf("stream holds %v on the review topic, want %v", got, want)
	}
	for _, row := range github.Log() {
		if row.Redelivery && row.StatusCode != http.StatusOK {
			t.Fatalf("GitHub recorded redelivery %d of %s as %d, want 200", row.ID, row.GUID, row.StatusCode)
		}
	}
	if !strings.Contains(logs.String(), `msg="webhook delivery refused terminally"`) || !strings.Contains(logs.String(), "guid=delivery-oversized") {
		t.Fatalf("no terminal refusal line for the oversized delivery:\n%s", logs.String())
	}
}
