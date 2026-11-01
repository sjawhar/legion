package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// narrowingServer is a test broker whose github-app minter records the narrowing each release
// hands it and answers "narrowed" for one and "owner-wide" for none, with one session holding a
// grant of the github-app secret APP_KEY.
type narrowingServer struct {
	*testServer
	key          *ecdsa.PrivateKey
	enrollmentID string
	grantID      string

	mu     sync.Mutex
	minted []*secrets.GitHubNarrowing
}

func newNarrowingServer(t *testing.T) *narrowingServer {
	t.Helper()
	s := &narrowingServer{}
	s.testServer = newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Minters = map[string]requests.Minter{
			policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.minted = append(s.minted, req.GitHubNarrowing)
				if req.GitHubNarrowing != nil && slices.Contains(req.GitHubNarrowing.Repositories, "missing") {
					return "", fmt.Errorf("%w: GitHub answered HTTP 422 to the narrowed token for ada: likely a repository it names (missing) that the installation does not reach", secrets.ErrMintNarrowingRefused)
				}
				if req.GitHubNarrowing != nil {
					return "narrowed", nil
				}
				return "owner-wide", nil
			},
		}
	})
	s.Secrets.Put(policytest.KindSecret("APP_KEY", testApprover, policy.TierAgent, policy.KindGitHubApp, "stored-app-key"))
	s.Secrets.Put(policytest.Secret("PLAIN_KEY", testApprover, policy.TierAgent, "plain-v1"))
	for _, name := range []string{"APP_KEY", "PLAIN_KEY"} {
		if status, body := s.req(t, http.MethodPost, "/v1/secrets/"+name+"/reread", nil, nil); status != http.StatusOK {
			t.Fatalf("reread %s = %d %s", name, status, body)
		}
	}
	s.enrollmentID, s.key = s.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	s.grantID = s.grant(t, "APP_KEY")
	return s
}

// grant requests name for the session and answers the automatic grant's id.
func (s *narrowingServer) grant(t *testing.T, name string) string {
	t.Helper()
	created := s.requestAs(t, s.key, s.enrollmentID, name)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests for %s = %+v, want an automatic grant", name, created)
	}
	return *created.GrantID
}

// take returns the narrowings the minter was handed since the last take, and forgets them.
func (s *narrowingServer) take() []*secrets.GitHubNarrowing {
	s.mu.Lock()
	defer s.mu.Unlock()
	minted := s.minted
	s.minted = nil
	return minted
}

// values releases grantID with body (nil for none) and answers the status and raw answer.
func (s *narrowingServer) values(t *testing.T, grantID string, body any) (int, []byte) {
	t.Helper()
	return s.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", body)
}

// TestAReleaseNarrowsAGitHubAppToken: POST /v1/grants/{id}/values with a github_app body hands the
// github-app minter that narrowing, its owner lowercased and its repositories sorted and distinct,
// and answers the narrowing it applied as github_app. A release with no body, or an empty object,
// is today's: no narrowing reaches the minter and the answer carries no github_app.
func TestAReleaseNarrowsAGitHubAppToken(t *testing.T) {
	s := newNarrowingServer(t)

	status, body := s.values(t, s.grantID, map[string]any{"github_app": map[string]any{
		"owner": "Ada", "repositories": []string{"widgets", "proofs", "widgets"}, "read_only": true,
	}})
	if status != http.StatusOK {
		t.Fatalf("narrowed POST values = %d %s, want 200", status, body)
	}
	answer := decode[struct {
		Values    map[string]string `json:"values"`
		GitHubApp map[string]any    `json:"github_app"`
	}](t, body)
	if answer.Values["APP_KEY"] != "narrowed" {
		t.Fatalf("narrowed values = %v, want the narrowed token", answer.Values)
	}
	wantEcho := map[string]any{"owner": "ada", "repositories": []any{"proofs", "widgets"}, "read_only": true}
	if !reflect.DeepEqual(answer.GitHubApp, wantEcho) {
		t.Fatalf("github_app = %v, want %v", answer.GitHubApp, wantEcho)
	}
	want := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}, ReadOnly: true}
	if minted := s.take(); len(minted) != 1 || !reflect.DeepEqual(minted[0], want) {
		t.Fatalf("the minter's narrowings = %v, want %+v", minted, want)
	}

	for _, release := range []struct {
		name string
		body any
	}{{"no body", nil}, {"an empty object", map[string]any{}}} {
		status, body := s.values(t, s.grantID, release.body)
		if status != http.StatusOK {
			t.Fatalf("POST values with %s = %d %s, want 200", release.name, status, body)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("POST values with %s answered %s: %v", release.name, body, err)
		}
		if _, ok := raw["github_app"]; ok || !strings.Contains(string(raw["values"]), `"owner-wide"`) {
			t.Fatalf("POST values with %s = %s, want the owner-wide token and no github_app", release.name, body)
		}
		if minted := s.take(); len(minted) != 1 || minted[0] != nil {
			t.Fatalf("the minter's narrowings for %s = %v, want none", release.name, minted)
		}
	}
}

// TestABadNarrowingIs400AndMintsNothing: a github_app narrowing policy.ParseGitHubNarrowing refuses
// (its own test holds the rules) or one holding a field the broker does not read is refused 400
// INVALID_NARROWING, naming the field as github_app.<field>; a repositories list present but empty
// is refused rather than read as every repository, which only JSON's [] against an absent field
// shows; a narrowing of a grant that holds no github-app secret is refused 400
// NARROWING_NOT_APPLICABLE. None runs a minter.
func TestABadNarrowingIs400AndMintsNothing(t *testing.T) {
	s := newNarrowingServer(t)
	for _, tc := range []struct {
		name      string
		narrowing map[string]any
		mentions  string
	}{
		{"a repository with its owner", map[string]any{"owner": "ada", "repositories": []string{"ada/proofs"}}, "github_app.repositories names ada's repositories by name alone"},
		{"an empty repository list", map[string]any{"owner": "ada", "repositories": []string{}}, "github_app.repositories is empty"},
		{"a field the broker does not read", map[string]any{"owner": "ada", "permissions": map[string]string{"administration": "write"}}, "permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.values(t, s.grantID, map[string]any{"github_app": tc.narrowing})
			reply := decode[wireError](t, body)
			if status != http.StatusBadRequest || reply.Code != "INVALID_NARROWING" || !strings.Contains(reply.Error, tc.mentions) {
				t.Fatalf("POST values = %d %+v, want 400 INVALID_NARROWING naming %q", status, reply, tc.mentions)
			}
			if minted := s.take(); len(minted) != 0 {
				t.Fatalf("the minter ran %d times for a refused narrowing, want none", len(minted))
			}
		})
	}

	plain := s.grant(t, "PLAIN_KEY")
	status, body := s.values(t, plain, map[string]any{"github_app": map[string]any{"owner": "ada", "read_only": true}})
	if reply := decode[wireError](t, body); status != http.StatusBadRequest || reply.Code != "NARROWING_NOT_APPLICABLE" {
		t.Fatalf("narrowed POST values of a stored secret's grant = %d %+v, want 400 NARROWING_NOT_APPLICABLE", status, reply)
	}
	if strings.Contains(string(body), "plain-v1") {
		t.Fatalf("the refused narrowing answered the stored value: %s", body)
	}
}

// TestANarrowingTheSecretCannotServeIsTheCallersAndNotAMintFailure: a narrowing to an account the
// secret's github-owners tag does not list is refused 403 NARROWING_OWNER_NOT_LISTED before any
// minter runs, and one GitHub refuses (a repository it names that the installation does not
// reach) 422 NARROWING_REFUSED; neither releases a value or answers the 502 MINT_FAILED of an
// App that failed, and each is logged at WARN as a refused narrowing, never as the operator's
// mint failure.
func TestANarrowingTheSecretCannotServeIsTheCallersAndNotAMintFailure(t *testing.T) {
	logged := policytest.CaptureLog(t)
	s := newNarrowingServer(t)
	for _, tc := range []struct {
		name      string
		narrowing map[string]any
		status    int
		code      string
		mentions  string
		minted    int
	}{
		{"an unlisted owner", map[string]any{"owner": "Outsider", "read_only": true}, http.StatusForbidden, "NARROWING_OWNER_NOT_LISTED", "outsider", 0},
		{"a repository GitHub refuses", map[string]any{"owner": "ada", "repositories": []string{"missing"}}, http.StatusUnprocessableEntity, "NARROWING_REFUSED", "missing", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.values(t, s.grantID, map[string]any{"github_app": tc.narrowing})
			reply := decode[wireError](t, body)
			if status != tc.status || reply.Code != tc.code || !strings.Contains(reply.Error, tc.mentions) || strings.Contains(string(body), `"values"`) {
				t.Fatalf("POST values = %d %s, want %d %s naming %q and no value", status, body, tc.status, tc.code, tc.mentions)
			}
			if minted := s.take(); len(minted) != tc.minted {
				t.Fatalf("the minter ran %d times, want %d", len(minted), tc.minted)
			}
		})
	}
	if strings.Contains(logged.String(), requests.MintFailedMessage) {
		t.Fatalf("a refused narrowing was logged as a mint failure: %s", logged.String())
	}
	warned := 0
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "WARN "+requests.NarrowingRefusedMessage) && strings.Contains(line, "name=APP_KEY") {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("logged %d %q lines naming APP_KEY at WARN, want 2: %s", warned, requests.NarrowingRefusedMessage, logged.String())
	}
}
