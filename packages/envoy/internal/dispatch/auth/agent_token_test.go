package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestBearerToken(t *testing.T) {
	for _, test := range []struct {
		name string
		// authorization is the header's value; empty sends no Authorization header.
		authorization string
		token         string
		present       bool
	}{
		{name: "no header", authorization: "", token: "", present: false},
		{name: "a blank header", authorization: " \t ", token: "", present: false},
		{name: "a bearer", authorization: "Bearer agent-token", token: "agent-token", present: true},
		{name: "a bearer in whitespace", authorization: " Bearer agent-token\t", token: "agent-token", present: true},
		{name: "an empty bearer", authorization: "Bearer ", token: "", present: true},
		{name: "a lowercase scheme", authorization: "bearer agent-token", token: "", present: true},
		{name: "two spaces after the scheme", authorization: "Bearer  agent-token", token: " agent-token", present: true},
		{name: "a tab after the scheme", authorization: "Bearer\tagent-token", token: "", present: true},
		{name: "no space after the scheme", authorization: "Beareragent-token", token: "", present: true},
		{name: "no scheme", authorization: "agent-token", token: "", present: true},
		{name: "another scheme", authorization: "Basic x", token: "", present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			token, present := BearerToken(request)
			if token != test.token || present != test.present {
				t.Fatalf("BearerToken(Authorization %q) = (%q, %t), want (%q, %t)", test.authorization, token, present, test.token, test.present)
			}
		})
	}
}

// sharedTokens is setting parsed as the server parses DISPATCH_AGENT_TOKEN at boot.
func sharedTokens(t *testing.T, setting string) *SharedAgentTokens {
	t.Helper()
	tokens, err := ParseSharedAgentTokens(setting)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens(%q): %v", setting, err)
	}
	return tokens
}

func TestMatchesSharedAgentToken(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		token      string
		want       bool
	}{
		{name: "exact shared token", configured: "agent-token", token: "agent-token", want: true},
		{name: "a value in whitespace", configured: " agent-token\n", token: "agent-token", want: true},
		{name: "empty bearer", configured: "agent-token", token: "", want: false},
		{name: "truncated bearer", configured: "agent-token", token: "agent-toke", want: false},
		{name: "longer bearer", configured: "agent-token", token: "agent-tokenx", want: false},
		{name: "different bearer", configured: "agent-token", token: "agent-tokem", want: false},
		{name: "the first of two values", configured: "new-token old-token", token: "new-token", want: true},
		{name: "the second of two values", configured: "new-token old-token", token: "old-token", want: true},
		{name: "every value of three, a line apart: the last", configured: "new-token\nmid-token\nold-token", token: "old-token", want: true},
		{name: "every value of three, a line apart: the middle", configured: "new-token\nmid-token\nold-token", token: "mid-token", want: true},
		{name: "a value the list does not hold", configured: "new-token old-token", token: "other-token", want: false},
		{name: "one value short of a listed one", configured: "new-token old-token", token: "old-toke", want: false},
		{name: "the whole setting as one bearer", configured: "new-token old-token", token: "new-token old-token", want: false},
		{name: "empty bearer against two values", configured: "new-token old-token", token: "", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokens := sharedTokens(t, test.configured)
			if got := MatchesSharedAgentToken(httptest.NewRequest(http.MethodGet, "/", nil), test.token, tokens); got != test.want {
				t.Fatalf("MatchesSharedAgentToken(%q) against %q = %t, want %t", test.token, test.configured, got, test.want)
			}
		})
	}
	t.Run("no list", func(t *testing.T) {
		for _, token := range []string{"agent-token", ""} {
			if MatchesSharedAgentToken(httptest.NewRequest(http.MethodGet, "/", nil), token, nil) {
				t.Errorf("MatchesSharedAgentToken(%q) against no list = true, want false", token)
			}
		}
	})
}

// The setting is refused at boot for an empty entry or a repeated one, naming the entry by its
// position and never by its value, since the refusal reaches the boot log.
func TestParseSharedAgentTokensRefusesAnEmptyOrRepeatedEntry(t *testing.T) {
	for _, test := range []struct {
		setting string
		want    string
	}{
		{setting: "", want: "entry 1 of 1 is empty"},
		{setting: "new-token  old-token", want: "entry 2 of 3 is empty"},
		{setting: "new-token\r\nold-token", want: "entry 2 of 3 is empty"},
		{setting: "new-token\n\nold-token", want: "entry 2 of 3 is empty"},
		{setting: "new-token old-token new-token", want: "entry 3 of 3 repeats entry 1"},
		{setting: "new-token old-token old-token", want: "entry 3 of 3 repeats entry 2"},
	} {
		tokens, err := ParseSharedAgentTokens(test.setting)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ParseSharedAgentTokens(%q) = %v, %v; want a refusal naming %q", test.setting, tokens, err, test.want)
			continue
		}
		for _, value := range []string{"new-token", "old-token"} {
			if strings.Contains(err.Error(), value) {
				t.Errorf("ParseSharedAgentTokens(%q) refusal %q names the value %q", test.setting, err, value)
			}
		}
	}
}

// warnings routes the default logger into a buffer until the test ends, and returns the lines
// logged so far, decoded.
func warnings(t *testing.T) func() []map[string]any {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var lines []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
			if line == "" {
				continue
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			lines = append(lines, decoded)
		}
		return lines
	}
}

// A bearer that matches a value after the first is logged with the rightmost X-Forwarded-For
// address, the User-Agent and the path, once per address and User-Agent every ten minutes; the
// first value is never logged, and no line holds a token.
func TestAPreviousTokenIsLoggedOncePerCallerEveryTenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logged := warnings(t)
		tokens := sharedTokens(t, "new-token old-token")
		authenticate := func(path, forwardedFor, userAgent, token string) {
			t.Helper()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = "192.0.2.4:51000"
			if forwardedFor != "" {
				request.Header.Set("X-Forwarded-For", forwardedFor)
			}
			request.Header.Set("User-Agent", userAgent)
			if !MatchesSharedAgentToken(request, token, tokens) {
				t.Fatalf("%s with %q was refused", path, token)
			}
		}
		lineCount := func(want int, after string) []map[string]any {
			t.Helper()
			lines := logged()
			if len(lines) != want {
				t.Fatalf("after %s: %d log lines %v, want %d", after, len(lines), lines, want)
			}
			return lines
		}

		authenticate("/api/v1/whoami", "198.51.100.9, 203.0.113.7", "agent/1", "new-token")
		lineCount(0, "the first value")

		authenticate("/api/v1/whoami", "198.51.100.9, 203.0.113.7", "agent/1", "old-token")
		line := lineCount(1, "the second value")[0]
		for key, want := range map[string]any{
			"level":      "WARN",
			"entry":      float64(2),
			"address":    "203.0.113.7",
			"user_agent": "agent/1",
			"path":       "/api/v1/whoami",
		} {
			if line[key] != want {
				t.Errorf("log line %s = %v, want %v (line %v)", key, line[key], want, line)
			}
		}

		// The same address and User-Agent on another path, and with another hop before the load
		// balancer's, are the same caller within the window.
		authenticate("/ws/doc/room", "203.0.113.7", "agent/1", "old-token")
		authenticate("/api/v1/issues", "10.0.0.1, 203.0.113.7", "agent/1", "old-token")
		lineCount(1, "the same caller again")

		authenticate("/api/v1/issues", "203.0.113.7", "agent/2", "old-token")
		lineCount(2, "another User-Agent")
		authenticate("/api/v1/issues", "203.0.113.8", "agent/1", "old-token")
		lineCount(3, "another address")
		authenticate("/api/v1/issues", "", "agent/1", "old-token")
		if lines := lineCount(4, "no X-Forwarded-For"); lines[3]["address"] != "192.0.2.4" {
			t.Errorf("without X-Forwarded-For, address = %v, want the connection's 192.0.2.4", lines[3]["address"])
		}

		time.Sleep(previousTokenLogWindow - time.Second)
		authenticate("/api/v1/whoami", "203.0.113.7", "agent/1", "old-token")
		lineCount(4, "the same caller a second short of ten minutes")
		time.Sleep(time.Second)
		authenticate("/api/v1/whoami", "203.0.113.7", "agent/1", "old-token")
		lineCount(5, "the same caller ten minutes on")
		authenticate("/api/v1/whoami", "203.0.113.7", "agent/1", "new-token")
		lines := lineCount(5, "the first value ten minutes on")

		encoded, err := json.Marshal(lines)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"new-token", "old-token"} {
			if bytes.Contains(encoded, []byte(token)) {
				t.Errorf("the log holds the token %q: %s", token, encoded)
			}
		}
	})
}

// The caller writes the address, the User-Agent and the path, so the previous-token log keeps a
// bounded slice of each and remembers a bounded number of callers: one past the bound is logged on
// each request rather than remembered.
func TestThePreviousTokenLogIsBounded(t *testing.T) {
	logged := warnings(t)
	tokens := sharedTokens(t, "new-token old-token")
	authenticate := func(userAgent, path string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Forwarded-For", "203.0.113.7")
		request.Header.Set("User-Agent", userAgent)
		if !MatchesSharedAgentToken(request, "old-token", tokens) {
			t.Fatalf("old-token was refused")
		}
	}

	authenticate(strings.Repeat("u", 1000), "/"+strings.Repeat("p", 1000))
	line := logged()[0]
	if userAgent, path := line["user_agent"].(string), line["path"].(string); len(userAgent) != previousTokenCallerBytes || len(path) != previousTokenCallerBytes {
		t.Errorf("logged a %d-byte user agent and a %d-byte path, want each cut to %d", len(userAgent), len(path), previousTokenCallerBytes)
	}

	for caller := len(tokens.logged); caller < previousTokenCallers; caller++ {
		authenticate(fmt.Sprintf("agent/%d", caller), "/api/v1/whoami")
	}
	before := len(logged())
	authenticate("one-past-the-bound", "/api/v1/whoami")
	authenticate("one-past-the-bound", "/api/v1/whoami")
	if remembered, lines := len(tokens.logged), len(logged())-before; remembered != previousTokenCallers || lines != 2 {
		t.Errorf("past the bound: %d callers remembered and %d lines for two requests of one caller, want %d and 2", remembered, lines, previousTokenCallers)
	}
}
