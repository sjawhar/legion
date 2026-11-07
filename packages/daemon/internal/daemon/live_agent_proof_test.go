//go:build e2e

// What the two live proofs that drive real Oh My Pi panes share below the daemon: the model
// stand-in each pane talks to, the provider answers those stand-ins write, and the overlay
// writers that point a pane at one. The two are provider_error_retry_live_test.go, the driver of
// scripts/e2e/provider-error-retry.sh, and claim_compaction_live_test.go, the driver of
// scripts/e2e/claim-compaction.sh.
//
// A stand-in's handler is its own proof's, because the two differ in what decides an answer: the
// retry proof's three answer by the provider name each is given in the run's models.yml, and the
// compaction proof's four answer by what the step before them set, a turn's request apart from a
// compaction's summary request. What they hold in common is here: the HTTP server, the bodies
// they were sent, and the three answers both of them write — the overloaded status, a refused
// request, and the Messages stream of a turn that ends.
package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// ---- the model stand-ins -------------------------------------------------------------------------

// standinModelID is the model id every stand-in serves, under its own provider name.
const standinModelID = "-model"

// standinModel is the one model a stand-in serves, as a selector.
func standinModel(provider string) string { return provider + "/" + provider + standinModelID }

// modelStandin is what both proofs' provider endpoints hold: the provider name the run's
// models.yml gives this one, the URL a pane reaches it at, and the body of every model request
// it has answered, which a step counts. The handler is the proof's own and reads each request
// through take.
type modelStandin struct {
	name string
	url  string

	mu       sync.Mutex
	requests []string
}

// serve starts handler as this stand-in's endpoint, closed when the test ends, and records the
// URL the run's models.yml gives the provider.
func (s *modelStandin) serve(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s.url = server.URL
}

// take is what a stand-in's handler does with a request before it answers it: a request that is
// no model call at all — Oh My Pi lists a provider's models with a GET — is answered an empty
// listing here and take says there is nothing to answer; every other request has its body read
// and recorded, and the handler answers that body.
func (s *modelStandin) take(w http.ResponseWriter, req *http.Request) (string, bool) {
	if req.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
		return "", false
	}
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	s.requests = append(s.requests, string(body))
	s.mu.Unlock()
	return string(body), true
}

// count is how many model requests the stand-in has answered.
func (s *modelStandin) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// requestsHolding is how many of them carried marker, which is how many of them are a turn of the
// task rather than one of Oh My Pi's own side requests.
func (s *modelStandin) requestsHolding(marker string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := 0
	for _, body := range s.requests {
		if strings.Contains(body, marker) {
			held++
		}
	}
	return held
}

// ---- the answers both proofs' stand-ins write ------------------------------------------------------

// writeOverloaded answers one model request 529 with `retry-after: 3600`. The header is over the
// Oh My Pi client's own cap for a retry delay (maxRetryDelayMs, 60 s), which declines the retry
// and surfaces the status untouched (@oh-my-pi/pi-ai src/providers/anthropic-client.ts), so one
// of the daemon's sends is one request here and the turn ends in a provider error rather than
// after a round of the host's own retries.
func writeOverloaded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(529)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
}

// writeInvalidRequest answers one model request 400 with body, which carries an
// `invalid_request_error`: no word of such a body is one Oh My Pi reads as transient, so the turn
// error is reported `retryable: false` with status 400 and is retried by nobody. A 5xx is retried
// instead, by the summarizer until the daemon's own ten-minute bound runs out, which is why a
// refused summary is a client error here too. Each proof passes the body its own live run has
// measured: the retry proof's malformed provider the bare object, and a pane of the compaction
// proof Anthropic's error envelope (anthropicRefusal).
func writeInvalidRequest(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, body)
}

// anthropicRefusal is Anthropic's own error envelope around an `invalid_request_error` of
// message, which is the shape a request to an `anthropic-messages` provider is refused in.
func anthropicRefusal(message string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, message)
}

// streamAnthropicText answers one Anthropic Messages request with a single text block, in the
// events the plugins' own pane harness scripts (packages/pi-shared/test/omp-harness.ts:383-448,
// `messageStream`).
func streamAnthropicText(w http.ResponseWriter, provider, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"answered","type":"message","role":"assistant",`+
		`"model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`,
		provider+standinModelID))
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":16}}`)
	event("message_stop", `{"type":"message_stop"}`)
}

// ---- the overlays a pane is told through PI_CONFIG_FILES -------------------------------------------

func writeProofFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeRoleOverlay writes one claim's own pane overlay (`role_config_files`): the single model
// that claim's stand-in serves, for every model role, so no role of the session reaches a model
// the run answers nothing for and the claim's chain has no fallback below it. The operator's own
// overlay, which every pane names before this one, is each proof's own, since the two set
// different keys in it.
func writeRoleOverlay(t *testing.T, path, model string) {
	t.Helper()
	var body strings.Builder
	body.WriteString("# The role's own pane overlay (role_config_files): one model, so its chain has no fallback.\nmodelRoles:\n")
	for _, role := range []string{"default", "smol", "slow", "vision", "plan", "commit", "tiny", "task", "advisor", "review", "oracle", "deep"} {
		fmt.Fprintf(&body, "  %s: %s\n", role, model)
	}
	writeProofFile(t, path, body.String())
}
