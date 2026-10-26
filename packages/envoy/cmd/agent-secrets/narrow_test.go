package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// narrowBroker is a fake broker for the exec form's --github-* flags: every request is granted at
// once as grant-<name>, and each release records the body it was sent. A grant of OLD_APP answers
// as a broker from before narrowing (no github_app in the answer, the value owner-wide); any other
// grant echoes the github_app it was sent and answers "narrowed" for one, "owner-wide" for none.
type narrowBroker struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string // each release's body, "" for none
}

func newNarrowBroker(t *testing.T) *narrowBroker {
	t.Helper()
	b := &narrowBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Request string `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		obj, err := record.VerifyRequestObject(body.Request, b.srv.URL, time.Minute, time.Now())
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name := obj.Details[0].Identifier
		writeJSON(w, map[string]any{
			"request_id": "req-" + strings.ToLower(name), "state": "granted",
			"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
			"grant_id": "grant-" + strings.ToLower(name), "record_id": nil, "approver": nil, "lifetime_seconds": nil,
		})
	})
	mux.HandleFunc("POST /v1/grants/{id}/values", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		name := strings.ToUpper(strings.TrimPrefix(r.PathValue("id"), "grant-"))
		var sent struct {
			GitHubApp json.RawMessage `json:"github_app"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &sent); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		answer := map[string]any{"values": map[string]string{name: "owner-wide"}, "expires_at": nil}
		switch {
		case sent.GitHubApp == nil || name == "OLD_APP":
		case name == "OTHER_APP":
			// A broker that applied another narrowing than the one sent: the owner alone.
			var asked GitHubNarrowing
			_ = json.Unmarshal(sent.GitHubApp, &asked)
			answer["values"] = map[string]string{name: "narrowed-otherwise"}
			answer["github_app"] = GitHubNarrowing{Owner: asked.Owner}
		default:
			answer["values"] = map[string]string{name: "narrowed"}
			answer["github_app"] = sent.GitHubApp
		}
		writeJSON(w, answer)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// take returns the release bodies the broker recorded since the last take, and forgets them.
func (b *narrowBroker) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	bodies := b.bodies
	b.bodies = nil
	return bodies
}

// TestTheGitHubFlagsNarrowTheRelease: the exec form's --github-owner, --github-repo and
// --github-read-only send the release a github_app narrowing, the owner lowercased and the
// repositories sorted, each once, and run the command once the broker answers that it applied that
// narrowing. Without them the release carries no body at all, as before.
func TestTheGitHubFlagsNarrowTheRelease(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		name string
		args []string
		body string
		ran  string
	}{
		{
			"every flag",
			[]string{"APP", "--github-owner", "Acme-Org", "--github-repo", "widgets", "--github-repo=proofs", "--github-repo", "widgets", "--github-read-only"},
			`{"github_app":{"owner":"acme-org","repositories":["proofs","widgets"],"read_only":true}}`,
			"narrowed",
		},
		{"the owner and read-only", []string{"APP", "--github-owner=ada", "--github-read-only"}, `{"github_app":{"owner":"ada","read_only":true}}`, "narrowed"},
		{"no flag", []string{"APP"}, "", "owner-wide"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, "--", "sh", "-c", `echo "ran $APP"`)
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			bodies := b.take()
			if exit != 0 || stdout != "ran "+tc.ran+"\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 running with the %s value", exit, stdout, stderr, tc.ran)
			}
			if len(bodies) != 1 || strings.TrimSpace(bodies[0]) != tc.body {
				t.Fatalf("release bodies = %q, want one release carrying %q", bodies, tc.body)
			}
		})
	}
}

// TestANarrowingTheBrokerDidNotApplyRunsNothing: a broker from before narrowing ignores the
// release's body and answers the owner-wide token with no github_app, so the exec form exits 1
// saying so and runs nothing, rather than handing the command a token wider than it asked for.
func TestANarrowingTheBrokerDidNotApplyRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"OLD_APP", "--github-owner", "ada", "--github-read-only", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "did not narrow") || !strings.Contains(stderr, "nothing was run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 saying the broker did not narrow and nothing was run", exit, stdout, stderr)
	}
	if bodies := b.take(); len(bodies) != 1 || !strings.Contains(bodies[0], `"github_app"`) {
		t.Fatalf("release bodies = %q, want the one narrowed release", bodies)
	}
}

// TestANarrowingTheBrokerAppliedOtherwiseRunsNothing: an answer naming another narrowing than the
// one asked for (here the owner alone, without the read-only limit) is no narrowing the command
// asked for, so the exec form exits 1 and runs nothing.
func TestANarrowingTheBrokerAppliedOtherwiseRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"OTHER_APP", "--github-owner", "ada", "--github-read-only", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "did not narrow") || !strings.Contains(stderr, "nothing was run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 saying the broker did not narrow as asked and nothing was run", exit, stdout, stderr)
	}
}

// TestAGitHubNarrowingWithoutAnOwnerExitsTwo: --github-repo or --github-read-only narrows one
// account's token, so without --github-owner it is a usage error, and so is a --github-owner that
// names no account (an unset variable a helper passed), which would otherwise send no narrowing at
// all; nothing reaches the broker.
func TestAGitHubNarrowingWithoutAnOwnerExitsTwo(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t)
	keyDir := newKeyDir(t)
	for _, flags := range [][]string{{"--github-repo", "widgets"}, {"--github-read-only"}, {"--github-owner", ""}, {"--github-owner="}, {"--github-owner", "  ", "--github-read-only"}} {
		args := append(append([]string{"APP"}, flags...), "--", "sh", "-c", "echo ran-the-child")
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
		if exit != 2 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "--github-owner") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want exit 2 naming --github-owner", flags, exit, stdout, stderr)
		}
		if bodies := b.take(); len(bodies) != 0 {
			t.Fatalf("%v: the broker was asked for a release: %q", flags, bodies)
		}
	}
}
