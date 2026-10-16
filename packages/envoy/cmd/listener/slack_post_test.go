package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/slackapi"
)

// slackStandIn is Slack's Web API as the listener's route reaches it: every chat.postMessage it is
// sent, and the one answer it gives each.
type slackStandIn struct {
	answer string
	status int

	mu    sync.Mutex
	posts []slackStandInPost
}

type slackStandInPost struct {
	authorization string
	body          map[string]string
}

func (s *slackStandIn) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat.postMessage" {
			t.Errorf("Slack was sent %s %s, want chat.postMessage", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("chat.postMessage body %q: %v", raw, err)
		}
		s.mu.Lock()
		s.posts = append(s.posts, slackStandInPost{authorization: r.Header.Get("Authorization"), body: body})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		status := s.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, s.answer)
	})
}

// slackPostRoute is the listener's /v1 mux with poster as its Slack client, as main builds it.
func slackPostRoute(poster slackPoster) http.Handler {
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{slack: poster}, "test-machine", logging.New("test"))
	return mux
}

func postSlack(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/slack/post", strings.NewReader(body)))
	return recorder
}

// The route posts the reply into its thread with the listener's bot token and answers Slack's ts.
func TestSlackPostPostsTheReplyIntoItsThreadAndAnswersSlacksTS(t *testing.T) {
	slack := &slackStandIn{answer: `{"ok":true,"channel":"C00000000","ts":"1700000000.000200"}`}
	server := httptest.NewServer(slack.handler(t))
	defer server.Close()
	route := slackPostRoute(slackapi.New(server.URL+"/api", "xoxb-test-token", server.Client()))

	recorder := postSlack(t, route, `{"channel":"C00000000","thread_ts":"1700000000.000100","text":"Filed as ACME-12."}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer slackPostResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer %s: %v", recorder.Body, err)
	}
	if answer != (slackPostResponse{Channel: "C00000000", ThreadTS: "1700000000.000100", TS: "1700000000.000200"}) {
		t.Errorf("answer = %+v, want the channel, the thread and Slack's ts", answer)
	}
	if len(slack.posts) != 1 {
		t.Fatalf("Slack saw %d posts, want 1", len(slack.posts))
	}
	post := slack.posts[0]
	if post.authorization != "Bearer xoxb-test-token" || post.body["channel"] != "C00000000" ||
		post.body["thread_ts"] != "1700000000.000100" || post.body["text"] != "Filed as ACME-12." {
		t.Errorf("Slack was sent %+v, want the reply in its thread under the bot token", post)
	}
}

// Slack's own refusal is answered 502 with Slack's code, verbatim, and nothing else posts.
func TestSlackPostAnswersSlacksRefusalVerbatim(t *testing.T) {
	slack := &slackStandIn{answer: `{"ok":false,"error":"not_in_channel"}`}
	server := httptest.NewServer(slack.handler(t))
	defer server.Close()
	route := slackPostRoute(slackapi.New(server.URL+"/api", "xoxb-test-token", server.Client()))

	recorder := postSlack(t, route, `{"channel":"C00000000","thread_ts":"1700000000.000100","text":"hello"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", recorder.Code, recorder.Body)
	}
	var refused apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refused); err != nil || refused.Error != "Slack refused chat.postMessage: not_in_channel" {
		t.Fatalf("body = %s, want Slack's not_in_channel verbatim", recorder.Body)
	}
	if len(slack.posts) != 1 {
		t.Errorf("Slack saw %d posts, want the one", len(slack.posts))
	}
}

// A listener with no bot token answers 503 naming the variable that turns posting on, before it
// reads the request.
func TestSlackPostWithoutABotTokenIsServiceUnavailableNamingTheVariable(t *testing.T) {
	recorder := postSlack(t, slackPostRoute(nil), `{"channel":"C00000000","thread_ts":"1.2","text":"hello"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "ENVOY_SLACK_BOT_TOKEN") {
		t.Errorf("body = %s, want it to name ENVOY_SLACK_BOT_TOKEN", recorder.Body)
	}
}

// The request is refused before Slack is called when a field is missing or unknown, the text is
// longer than contracts.SlackPostTextMax UTF-16 units, or the method is not POST.
func TestSlackPostRefusesABadRequestBeforeCallingSlack(t *testing.T) {
	slack := &slackStandIn{answer: `{"ok":true,"channel":"C00000000","ts":"1.3"}`}
	server := httptest.NewServer(slack.handler(t))
	defer server.Close()
	route := slackPostRoute(slackapi.New(server.URL+"/api", "xoxb-test-token", server.Client()))
	overByOne := strings.Repeat("a", contracts.SlackPostTextMax+1)
	// Characters outside the Basic Multilingual Plane are two UTF-16 units each.
	overInUnits := strings.Repeat("😀", contracts.SlackPostTextMax/2+1)

	for _, tc := range []struct {
		name, body, want string
	}{
		{name: "no channel", body: `{"thread_ts":"1.2","text":"hi"}`, want: "channel is required"},
		{name: "no thread", body: `{"channel":"C00000000","text":"hi"}`, want: "thread_ts is required"},
		{name: "no text", body: `{"channel":"C00000000","thread_ts":"1.2","text":"  "}`, want: "text is required"},
		{name: "an unknown field", body: `{"channel":"C00000000","threadTs":"1.2","text":"hi"}`, want: `invalid json: json: unknown field "threadTs"`},
		{name: "text one over the cap", body: `{"channel":"C00000000","thread_ts":"1.2","text":"` + overByOne + `"}`, want: "text is 1 characters over the 4000-character limit (4001/4000)"},
		{name: "text over the cap in UTF-16 units", body: `{"channel":"C00000000","thread_ts":"1.2","text":"` + overInUnits + `"}`, want: "text is 2 characters over the 4000-character limit (4002/4000)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := postSlack(t, route, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", recorder.Code, recorder.Body)
			}
			var refused apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refused); err != nil || refused.Error != tc.want {
				t.Fatalf("body = %s, want %q", recorder.Body, tc.want)
			}
		})
	}
	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/slack/post", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", recorder.Code)
	}
	if len(slack.posts) != 0 {
		t.Errorf("Slack saw %d posts, want none", len(slack.posts))
	}
	atCap := postSlack(t, route, `{"channel":"C00000000","thread_ts":"1.2","text":"`+strings.Repeat("a", contracts.SlackPostTextMax)+`"}`)
	if atCap.Code != http.StatusOK {
		t.Errorf("text at the cap: status = %d, want 200; body %s", atCap.Code, atCap.Body)
	}
}

// The bot token turns posting on; ENVOY_SLACK_API_URL moves the Web API, and one that is not an
// absolute http(s) URL refuses the start rather than every post.
func TestSlackPosterFromTheEnvironment(t *testing.T) {
	env := func(pairs map[string]string) func(string) string {
		return func(name string) string { return pairs[name] }
	}
	if poster, err := slackPosterFromEnv(env(nil)); err != nil || poster != nil {
		t.Fatalf("no token: poster %v, error %v; want neither", poster, err)
	}
	if poster, err := slackPosterFromEnv(env(map[string]string{"ENVOY_SLACK_BOT_TOKEN": "  xoxb-test  "})); err != nil || poster == nil {
		t.Fatalf("a token: poster %v, error %v; want a poster", poster, err)
	}
	for _, bad := range []string{"slack.com/api", "ftp://slack.example/api", "http://"} {
		_, err := slackPosterFromEnv(env(map[string]string{"ENVOY_SLACK_BOT_TOKEN": "xoxb-test", "ENVOY_SLACK_API_URL": bad}))
		if err == nil || !strings.Contains(err.Error(), "ENVOY_SLACK_API_URL") {
			t.Errorf("ENVOY_SLACK_API_URL=%q: error %v, want a refusal naming the variable", bad, err)
		}
	}
	if _, err := slackPosterFromEnv(env(map[string]string{"ENVOY_SLACK_API_URL": "http://127.0.0.1:1/api"})); err == nil ||
		!strings.Contains(err.Error(), "ENVOY_SLACK_API_URL is set without ENVOY_SLACK_BOT_TOKEN") {
		t.Errorf("a URL with no token: error %v, want the refusal", err)
	}
}
