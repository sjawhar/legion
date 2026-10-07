package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSlackCommandHelper re-enters this test binary after the fake agent-secrets command grants a
// token to its child. Production runs its own binary instead, so it reaches run normally.
func TestSlackCommandHelper(t *testing.T) {
	if os.Getenv("LEGION_SLACK_COMMAND_HELPER") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		os.Exit(2)
	}
	os.Exit(run(context.Background(), append([]string{"legion"}, os.Args[separator+1:]...), os.Stdout, os.Stderr))
}

// The public Slack verb obtains the bot token only in agent-secrets' child, posts top-level and
// threaded messages through chat.postMessage, and reads a report's thread through
// conversations.replies. LEGION_SLACK_API_URL makes the wire contract observable without Slack.
func TestSlackPostsRepliesAndReadsThreadsThroughAgentSecrets(t *testing.T) {
	var posts []map[string]string
	var thread url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-bot-token" {
			t.Errorf("Authorization = %q, want the token in the agent-secrets child", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/chat.postMessage":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("post = %s content-type %q, want POST application/json", r.Method, r.Header.Get("Content-Type"))
			}
			var post map[string]string
			if err := json.NewDecoder(r.Body).Decode(&post); err != nil {
				t.Errorf("decode chat.postMessage: %v", err)
			}
			posts = append(posts, post)
			_, _ = fmt.Fprint(w, `{"ok":true,"channel":"C0REPORT","ts":"171234.000100"}`)
		case "/api/conversations.replies":
			if r.Method != http.MethodGet {
				t.Errorf("conversations.replies method = %s, want GET", r.Method)
			}
			thread = r.URL.Query()
			_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"171234.000000","text":"report"},{"ts":"171234.000100","text":"reply"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	trace := filepath.Join(t.TempDir(), "agent-secrets-reasons")
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	agentSecrets := filepath.Join(bin, "agent-secrets")
	script := `#!/bin/sh
if [ "$1" != "LEGION_SLACK_BOT_TOKEN" ] || [ "$2" != "--reason" ] || [ -z "$3" ] || [ "$4" != "--" ]; then
  exit 99
fi
printf '%s\n' "$3" >> "$AGENT_SECRETS_TRACE"
shift 4
child=$1
shift
LEGION_SLACK_BOT_TOKEN=test-bot-token exec "$child" -test.run=TestSlackCommandHelper -- "$@"
`
	if err := os.WriteFile(agentSecrets, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("AGENT_SECRETS_TRACE", trace)
	t.Setenv("LEGION_SLACK_API_URL", api.URL+"/api")
	t.Setenv("LEGION_SLACK_COMMAND_HELPER", "1")

	runSlack := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "slack"}, args...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	if code, stdout, stderr := runSlack("post", "--channel", "C0REPORT", "--text", "new report"); code != 0 || stdout != "{\"channel\":\"C0REPORT\",\"ts\":\"171234.000100\"}\n" || stderr != "" {
		t.Fatalf("legion slack post = %d, stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runSlack("reply", "--channel", "C0REPORT", "--thread-ts", "171234.000000", "--text", "filed"); code != 0 || stdout != "{\"channel\":\"C0REPORT\",\"ts\":\"171234.000100\"}\n" || stderr != "" {
		t.Fatalf("legion slack reply = %d, stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runSlack("read", "--channel", "C0REPORT", "--thread-ts", "171234.000000"); code != 0 || !strings.Contains(stdout, `"text":"report"`) || stderr != "" {
		t.Fatalf("legion slack read = %d, stdout %q stderr %q", code, stdout, stderr)
	}

	wantPosts := []map[string]string{
		{"channel": "C0REPORT", "text": "new report"},
		{"channel": "C0REPORT", "thread_ts": "171234.000000", "text": "filed"},
	}
	if len(posts) != len(wantPosts) {
		t.Fatalf("chat.postMessage calls = %d, want %d", len(posts), len(wantPosts))
	}
	for i := range wantPosts {
		if !maps.Equal(posts[i], wantPosts[i]) {
			t.Errorf("chat.postMessage %d = %#v, want %#v", i, posts[i], wantPosts[i])
		}
	}
	if got := thread.Get("channel"); got != "C0REPORT" {
		t.Errorf("conversations.replies channel = %q, want C0REPORT", got)
	}
	if got := thread.Get("ts"); got != "171234.000000" {
		t.Errorf("conversations.replies ts = %q, want 171234.000000", got)
	}
	reasons, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read agent-secrets calls: %v", err)
	}
	if got := strings.Split(strings.TrimSpace(string(reasons)), "\n"); len(got) != 3 || got[0] != "Post a message as the Legion Slack app" || got[1] != "Reply in a Legion Slack thread" || got[2] != "Read a Legion Slack thread" {
		t.Errorf("agent-secrets reasons = %q", got)
	}
}

