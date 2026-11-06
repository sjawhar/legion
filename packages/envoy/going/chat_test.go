package going_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

const (
	testToken    = "test-bearer-never-in-errors"
	testQuestion = "who-asked-this-never-in-errors"
)

// staticTokens is the keyless form of credential a test configures: one token, or one failure.
type staticTokens struct {
	token string
	err   error
}

func (s staticTokens) Token(context.Context) (string, error) { return s.token, s.err }

// testClient builds an HTTPClient against model's base, with a static token as its credential
// unless cfg carries a source of its own.
func testClient(t *testing.T, model *goingtest.Model, cfg going.HTTPConfig) *going.HTTPClient {
	t.Helper()
	if cfg.Endpoint == "" {
		cfg.Endpoint = model.URL + "/openai/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-test"
	}
	if cfg.Tokens == nil {
		cfg.Tokens = staticTokens{token: testToken}
	}
	client, err := going.NewHTTPClient(cfg)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return client
}

// testRequest is the one-message turn most tests send, carrying the question a failure must
// never name.
func testRequest() going.ChatRequest {
	return going.ChatRequest{Messages: []going.Message{{Role: "user", Content: testQuestion}}}
}

// TestChatPostsToTheRouteAppendedToTheBase checks the URL one call reaches: the route appended to
// the configured base, the api-version query present only when one is configured, and the
// configured model named in the body.
func TestChatPostsToTheRouteAppendedToTheBase(t *testing.T) {
	for _, tc := range []struct {
		name       string
		apiVersion string
		wantQuery  string
	}{
		{"with an api-version", "2024-10-21", "api-version=2024-10-21"},
		{"without one", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, goingtest.Reply{Text: "an answer"})
			client := testClient(t, model, going.HTTPConfig{APIVersion: tc.apiVersion, Model: "my-deployment"})
			if _, err := client.Chat(t.Context(), testRequest(), nil); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			requests := model.Requests()
			if len(requests) != 1 {
				t.Fatalf("model served %d calls, want 1", len(requests))
			}
			got := requests[0]
			if got.Path != "/openai/v1/chat/completions" || got.RawQuery != tc.wantQuery {
				t.Errorf("call reached %s?%s, want /openai/v1/chat/completions?%s", got.Path, got.RawQuery, tc.wantQuery)
			}
			if got.Model != "my-deployment" {
				t.Errorf("request model = %q, want my-deployment", got.Model)
			}
			want := []going.Message{{Role: "user", Content: testQuestion}}
			if !reflect.DeepEqual(got.Messages, want) {
				t.Errorf("request messages = %+v, want %+v", got.Messages, want)
			}
			if len(got.Tools) != 0 {
				t.Errorf("request tools = %+v, want none for a request that carries none", got.Tools)
			}
		})
	}
}

func TestChatSignsWithTheTokenSourcesToken(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Text: "an answer"})
	client := testClient(t, model, going.HTTPConfig{Tokens: staticTokens{token: "entra-access-token"}})
	if _, err := client.Chat(t.Context(), testRequest(), nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := model.Requests()[0].Authorization; got != "Bearer entra-access-token" {
		t.Errorf("Authorization = %q, want Bearer entra-access-token", got)
	}
}

// TestChatRereadsTheTokenFileOnEveryCall proves going.FileTokenSource takes the bearer the file
// holds now: the file is rewritten between two calls, and the second call presents the new
// token.
func TestChatRereadsTheTokenFileOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	model := goingtest.NewModel(t, goingtest.Reply{Text: "an answer"})
	client := testClient(t, model, going.HTTPConfig{Tokens: going.FileTokenSource{Path: path}})
	if _, err := client.Chat(t.Context(), testRequest(), nil); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	if _, err := client.Chat(t.Context(), testRequest(), nil); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	requests := model.Requests()
	if len(requests) != 2 {
		t.Fatalf("model served %d calls, want 2", len(requests))
	}
	if requests[0].Authorization != "Bearer first-token" || requests[1].Authorization != "Bearer second-token" {
		t.Errorf("Authorization = %q then %q, want Bearer first-token then Bearer second-token",
			requests[0].Authorization, requests[1].Authorization)
	}
}

// TestChatHandsEachDeltaOnAsItArrives: the content reaches onText in the model's own order, in
// the pieces the model streamed, and the first piece arrives a pause before the turn ends, which
// is what lets a reader see the answer while the model is still writing it.
func TestChatHandsEachDeltaOnAsItArrives(t *testing.T) {
	const answer = "Dispatch keeps the broadcast order on the issue itself."
	const pause = 20 * time.Millisecond
	model := goingtest.NewModel(t, goingtest.Reply{Text: answer, Chunk: 8, Pause: pause})
	client := testClient(t, model, going.HTTPConfig{})
	var deltas []string
	var firstAt time.Duration
	start := time.Now()
	calls, err := client.Chat(t.Context(), testRequest(), func(delta string) error {
		if len(deltas) == 0 {
			firstAt = time.Since(start)
		}
		deltas = append(deltas, delta)
		return nil
	})
	finishedAt := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("tool calls = %+v, want none for a turn that answered", calls)
	}
	if len(deltas) < 2 {
		t.Fatalf("deltas = %q, want the answer in several pieces", deltas)
	}
	if got := strings.Join(deltas, ""); got != answer {
		t.Errorf("joined deltas = %q, want %q", got, answer)
	}
	if finishedAt-firstAt < pause {
		t.Errorf("first delta arrived %s in, the turn ended %s in; want the first at least one %s pause before the end",
			firstAt, finishedAt, pause)
	}
}

// TestChatReturnsTheToolCallsTheTurnEndedWith: a turn that calls a tool returns it whole, with
// the arguments joined from the frames that carried them, and nothing reaches onText.
func TestChatReturnsTheToolCallsTheTurnEndedWith(t *testing.T) {
	want := going.ToolCall{
		ID:        "call_1",
		Name:      contracts.GoingSearchTool,
		Arguments: `{"query":"who owns the broadcast order"}`,
	}
	model := goingtest.NewModel(t, goingtest.Reply{ToolCalls: []going.ToolCall{want}, Chunk: 4})
	client := testClient(t, model, going.HTTPConfig{})
	tool := going.Tool{
		Name:        contracts.GoingSearchTool,
		Description: "Search Dispatch again.",
		Parameters:  map[string]any{"type": "object"},
	}
	request := testRequest()
	request.Tools = []going.Tool{tool}
	calls, err := client.Chat(t.Context(), request, func(string) error {
		t.Error("onText was called for a turn that ended in tool calls")
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !reflect.DeepEqual(calls, []going.ToolCall{want}) {
		t.Errorf("tool calls = %+v, want %+v", calls, []going.ToolCall{want})
	}
	if got := model.Requests()[0].Tools; !reflect.DeepEqual(got, []going.Tool{tool}) {
		t.Errorf("request tools = %+v, want %+v", got, []going.Tool{tool})
	}
}

// TestChatRefusesANonSuccessStatus: the failure names the status, and carries neither the bearer
// nor the question.
func TestChatRefusesANonSuccessStatus(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Status: http.StatusInternalServerError})
	client := testClient(t, model, going.HTTPConfig{})
	_, err := client.Chat(t.Context(), testRequest(), nil)
	if err == nil {
		t.Fatal("Chat: want an error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("Chat error = %v, want it to name the status", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testQuestion) {
		t.Errorf("Chat error = %v, want neither the token nor the question in it", err)
	}
}

// TestChatEndsWithTheCallersContext: a model that never answers holds the response open, and the
// call ends when the caller's own deadline does rather than waiting out the client's.
func TestChatEndsWithTheCallersContext(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Silent: true})
	client := testClient(t, model, going.HTTPConfig{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.Chat(ctx, testRequest(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Chat error = %v, want the caller's deadline", err)
	}
}

// TestChatRefusesAnUnusableTokenFile: a file that cannot be read and one that holds nothing are
// both failures naming the path, and neither makes a call.
func TestChatRefusesAnUnusableTokenFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"a missing file", filepath.Join(dir, "no-such-token")},
		{"an empty file", empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, goingtest.Reply{Text: "an answer"})
			client := testClient(t, model, going.HTTPConfig{Tokens: going.FileTokenSource{Path: tc.path}})
			_, err := client.Chat(t.Context(), testRequest(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("Chat error = %v, want it to name %s", err, tc.path)
			}
			if model.Calls() != 0 {
				t.Errorf("model served %d calls, want none without a bearer", model.Calls())
			}
		})
	}
}

// TestChatFailsWithoutACallWhenSignInFails proves a token source's failure stops the call: no
// unauthenticated request goes out in its place.
func TestChatFailsWithoutACallWhenSignInFails(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Text: "an answer"})
	client := testClient(t, model, going.HTTPConfig{Tokens: staticTokens{err: errors.New("simulated sign-in failure")}})
	_, err := client.Chat(t.Context(), testRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "simulated sign-in failure") {
		t.Fatalf("Chat error = %v, want the sign-in failure", err)
	}
	if model.Calls() != 0 {
		t.Errorf("model served %d calls, want none after sign-in failed", model.Calls())
	}
}

// The identity lower-cases the endpoint's host, as embed.AzureClient's does, so a vector or an
// answer is never attributed to two spellings of one resource. The host here is a placeholder
// rather than a real inference domain: no test of this issue names one, since none may call a
// cloud model.
func TestHTTPClientIdentityIsThePlaceTheHostAndTheModel(t *testing.T) {
	client, err := going.NewHTTPClient(going.HTTPConfig{
		Place:    "azure",
		Endpoint: "https://Example-Resource.Models.Example/openai/v1/",
		Model:    "gpt-5-chat",
		Tokens:   going.FileTokenSource{Path: "/run/secrets/chat-token"},
	})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if got, want := client.Identity(), "azure:example-resource.models.example/gpt-5-chat"; got != want {
		t.Errorf("Identity() = %q, want %q", got, want)
	}
}

func TestNewHTTPClientRefusesAMisshapenConfig(t *testing.T) {
	valid := going.HTTPConfig{
		Place:    "endpoint",
		Endpoint: "https://models.example.com/v1",
		Model:    "gpt-test",
		Tokens:   going.FileTokenSource{Path: "/run/secrets/chat-token"},
	}
	if _, err := going.NewHTTPClient(valid); err != nil {
		t.Fatalf("NewHTTPClient(a valid config): %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*going.HTTPConfig)
	}{
		{"no endpoint", func(c *going.HTTPConfig) { c.Endpoint = "" }},
		{"relative endpoint", func(c *going.HTTPConfig) { c.Endpoint = "models.example.com/v1" }},
		{"unparsable endpoint", func(c *going.HTTPConfig) { c.Endpoint = "https://exa mple/%zz" }},
		{"endpoint with a query", func(c *going.HTTPConfig) { c.Endpoint += "?api-version=1" }},
		{"endpoint with a fragment", func(c *going.HTTPConfig) { c.Endpoint += "#models" }},
		{"no model", func(c *going.HTTPConfig) { c.Model = "" }},
		{"no credential", func(c *going.HTTPConfig) { c.Tokens = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.change(&cfg)
			if _, err := going.NewHTTPClient(cfg); err == nil {
				t.Errorf("NewHTTPClient(%s): want an error, got nil", tc.name)
			}
		})
	}
}

// TestChatNeverFollowsARedirectWithTheCredential: a model service that answers 3xx - or anyone
// able to answer for it - must not have this process present the deployment's bearer to a host
// it was never configured with. net/http strips Authorization across domains but strips neither
// x-api-key nor api-key, so the client refuses the redirect instead: the second host is never
// called, and the call fails on the 302 as it fails on any other non-2xx.
func TestChatNeverFollowsARedirectWithTheCredential(t *testing.T) {
	var elsewhere struct {
		calls         int
		authorization string
		apiKey        string
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.calls++
		elsewhere.authorization = r.Header.Get("Authorization")
		elsewhere.apiKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/openai/v1/chat/completions", http.StatusFound)
	}))
	defer redirecting.Close()

	client, err := going.NewHTTPClient(going.HTTPConfig{
		Place:      "endpoint",
		Endpoint:   redirecting.URL + "/openai/v1",
		Model:      "gpt-test",
		Tokens:     staticTokens{token: testToken},
		HTTPClient: redirecting.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	_, err = client.Chat(t.Context(), testRequest(), nil)
	if elsewhere.calls != 0 {
		t.Fatalf("the other host served %d calls (Authorization %q, api-key %q), want none: no redirect is followed",
			elsewhere.calls, elsewhere.authorization, elsewhere.apiKey)
	}
	if err == nil {
		t.Fatal("Chat followed the redirect and answered no error, want the 302 refused")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("Chat failed with %q, want no credential in it", err)
	}
}

// TestNewHTTPClientLeavesTheCallersClientAlone: the client a caller passes is the caller's, so
// refusing redirects is set on a copy of it. A test's own httptest client would otherwise stop
// following redirects for everything else that test does with it.
func TestNewHTTPClientLeavesTheCallersClientAlone(t *testing.T) {
	caller := &http.Client{}
	if _, err := going.NewHTTPClient(going.HTTPConfig{
		Place:      "endpoint",
		Endpoint:   "https://models.example.com/v1",
		Model:      "gpt-test",
		Tokens:     staticTokens{token: testToken},
		HTTPClient: caller,
	}); err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if caller.CheckRedirect != nil {
		t.Error("NewHTTPClient wrote CheckRedirect onto the caller's own client, want it set on a copy")
	}
}
