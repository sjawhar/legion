package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A listener that refuses Dispatch's ENVOY_TOKEN (a bad or rotated token) answers 401 to every
// request. That is the listener's answer, not a take-over: no successor will accept the same
// token a few seconds later, so the send settles failed on its first try instead of retrying the
// registry read for the whole take-over window.
func TestAListenerRefusingTheTokenSettlesTheSendAtOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database := newTargetedMessageHandler(t, listener.server.URL)

	unauthorized := takeOverAnswer{takeOverRegistry, answerJSON(http.StatusUnauthorized, `{"error":"unauthorized"}`)}
	script := make([]takeOverAnswer, 64)
	for i := range script {
		script[i] = unauthorized
	}
	listener.play(script...)

	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
		map[string]any{"body": "Token refused", "delivery": "steer"}, "alice")
	took := time.Since(started)
	if response.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
	}
	message := decodeBody[model.Message](t, response)
	if len(message.Deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want attempt 1 alone", message.Deliveries)
	}
	attempt := message.Deliveries[0]
	if attempt.State != "failed" || attempt.Error == nil || !strings.Contains(*attempt.Error, "unauthorized") {
		t.Fatalf("attempt = %#v, want failed with the listener's unauthorized answer", attempt)
	}
	tries := listener.requestsTo(takeOverRegistry)
	if tries != 1 || took > 2*time.Second {
		t.Fatalf("a listener answering 401 unauthorized was asked for the registry %d times over %v, want once "+
			"and settled at once: a refused token is the listener's answer, not a take-over to wait out "+
			"(envoy Client.Session maps the 401 to ErrUnavailable, which the take-over wait retries)", tries, took)
	}
	if states := attemptStates(t, database, "message_deliveries", "message_id", message.ID); !slices.Equal(states, []string{"1:failed"}) {
		t.Fatalf("attempts = %v, want attempt 1 alone, failed", states)
	}
}
