package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// A send or publish the stream refused because it holds its byte limit (bus.ErrStreamFull) was
// answered: the server said the stream is full. So it is not a bus that did not answer, and the
// answer must not say it was: the body does not open with busUnansweredPrefix, which sends an
// operator looking for an outage, and the request record's outcome is not bus_unavailable, the
// bucket an operator reads as "the bus went quiet". It is still worth retrying once age or an
// operator has made room, so it keeps the 503 and the Retry-After that ask for that retry, and its
// body names the full stream.
//
// What it does not prove: that the server's refusal arrives as bus.ErrStreamFull. That is
// internal/bus's own ground (streamFull); here the error is built as streamFull builds it.
func TestAFullStreamIsAnsweredAsFullNotAsABusThatDidNotAnswer(t *testing.T) {
	subject := contracts.RoleTopicPrefix + "reviewer"
	full := fmt.Errorf("%w: %w", bus.ErrStreamFull,
		&nats.APIError{Code: http.StatusServiceUnavailable, ErrorCode: 10077, Description: "maximum bytes exceeded"})

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_full_target"},
	} {
		t.Run(route.name, func(t *testing.T) {
			recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
					rec.refuseBusUnavailable(w, full)
				}
			}, postJSON("/v1/messages/publish", "{}"))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: a full stream takes the retry once it has room; body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got == "" {
				t.Fatalf("Retry-After is unset, want one: the same send is taken once the stream has room")
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
				t.Fatalf("the refusal reads %q: it says the bus did not answer, but the bus answered that the stream is full", refusal.Error)
			}
			if !strings.Contains(refusal.Error, bus.ErrStreamFull.Error()) {
				t.Fatalf("the refusal reads %q, want it to name %q", refusal.Error, bus.ErrStreamFull)
			}
			if record["outcome"] == "bus_unavailable" {
				t.Fatalf("the request record's outcome is bus_unavailable, which an operator reads as a bus that did not answer; a full stream answered: record = %v", record)
			}
		})
	}
}
