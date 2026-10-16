package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSlack is Slack's Web API as chat.postMessage answers it: it records each request and answers
// with answer's status and body.
type fakeSlack struct {
	t        *testing.T
	status   int
	answer   string
	requests []recorded
}

type recorded struct {
	path, authorization, contentType string
	body                             map[string]string
}

func (f *fakeSlack) serve() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("read the request body: %v", err)
		}
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("the request body %q is not a JSON object of strings: %v", raw, err)
		}
		f.requests = append(f.requests, recorded{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: body,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.answer)
	}))
	f.t.Cleanup(server.Close)
	return server
}

// A post is one chat.postMessage under the base URL, with the bot token as its bearer and the
// reply's channel, thread and text as its JSON body; Slack's ts is the answer.
func TestPostMessageSendsTheReplyWithTheBotTokenAndAnswersSlacksTS(t *testing.T) {
	slack := &fakeSlack{t: t, status: http.StatusOK, answer: `{"ok":true,"channel":"C00000000","ts":"1700000000.000200","message":{"text":"hi"}}`}
	server := slack.serve()
	client := New(server.URL+"/api", "xoxb-test-token", server.Client())

	posted, err := client.PostMessage(context.Background(), Message{Channel: "C00000000", ThreadTS: "1700000000.000100", Text: "On it."})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if posted != (Posted{Channel: "C00000000", TS: "1700000000.000200"}) {
		t.Errorf("posted = %+v, want Slack's channel and ts", posted)
	}
	if len(slack.requests) != 1 {
		t.Fatalf("Slack saw %d requests, want 1", len(slack.requests))
	}
	got := slack.requests[0]
	if got.path != "/api/chat.postMessage" || got.authorization != "Bearer xoxb-test-token" || got.contentType != "application/json; charset=utf-8" {
		t.Errorf("request = %s, Authorization %q, Content-Type %q; want chat.postMessage with the bot token as a JSON body", got.path, got.authorization, got.contentType)
	}
	want := map[string]string{"channel": "C00000000", "thread_ts": "1700000000.000100", "text": "On it."}
	if len(got.body) != len(want) || got.body["channel"] != want["channel"] || got.body["thread_ts"] != want["thread_ts"] || got.body["text"] != want["text"] {
		t.Errorf("body = %v, want %v", got.body, want)
	}
}

// Slack refuses with HTTP 200 and `ok: false`; its error code comes back as it is, as does the code
// of a refusal with another status (a 429's `ratelimited`).
func TestPostMessageReturnsSlacksErrorCodeVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer string
		code   string
	}{
		{name: "ok false", status: http.StatusOK, answer: `{"ok":false,"error":"not_in_channel"}`, code: "not_in_channel"},
		{name: "rate limited", status: http.StatusTooManyRequests, answer: `{"ok":false,"error":"ratelimited"}`, code: "ratelimited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := (&fakeSlack{t: t, status: tc.status, answer: tc.answer}).serve()
			_, err := New(server.URL, "xoxb-test-token", server.Client()).PostMessage(context.Background(), Message{Channel: "C00000000", ThreadTS: "1.2", Text: "x"})
			var refused *Error
			if !errors.As(err, &refused) || refused.Code != tc.code || refused.Method != "chat.postMessage" {
				t.Fatalf("PostMessage error = %#v, want Slack's %q", err, tc.code)
			}
			if want := "Slack refused chat.postMessage: " + tc.code; err.Error() != want {
				t.Errorf("error text = %q, want %q", err, want)
			}
		})
	}
}

// An answer that is not Slack's JSON (a proxy's error page) names the status and the start of the
// body, and is no Slack refusal; nor is an ok answer that carries no ts.
func TestPostMessageRefusesAnAnswerThatIsNotSlacks(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		status             int
	}{
		{name: "an error page", status: http.StatusBadGateway, answer: "<html>bad gateway</html>", want: "chat.postMessage answered HTTP 502 with a body that is not Slack's JSON: <html>bad gateway</html>"},
		{name: "ok without a ts", status: http.StatusOK, answer: `{"ok":true}`, want: "chat.postMessage answered ok with no ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := (&fakeSlack{t: t, status: tc.status, answer: tc.answer}).serve()
			_, err := New(server.URL, "xoxb-test-token", server.Client()).PostMessage(context.Background(), Message{Channel: "C00000000", ThreadTS: "1.2", Text: "x"})
			var refused *Error
			if err == nil || errors.As(err, &refused) || err.Error() != tc.want {
				t.Fatalf("PostMessage error = %v, want %q and no Slack refusal", err, tc.want)
			}
		})
	}
}

// A Slack that does not answer within the caller's deadline is an error that says the reply may
// have been posted, since Slack may have taken it before the deadline passed.
func TestPostMessageThatTimesOutSaysTheReplyMayHaveBeenPosted(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := New(server.URL, "xoxb-test-token", server.Client()).PostMessage(ctx, Message{Channel: "C00000000", ThreadTS: "1.2", Text: "x"})
	if err == nil || !errors.Is(err, ErrUnanswered) || !strings.Contains(err.Error(), "may have been posted") {
		t.Fatalf("PostMessage error = %v, want ErrUnanswered saying the reply may have been posted", err)
	}
	if strings.Contains(err.Error(), "xoxb-test-token") {
		t.Fatalf("the error %q quotes the bot token", err)
	}
}
