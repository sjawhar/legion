package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
)

type tokenSource struct {
	started chan struct{}
	release chan struct{}
	role    appauth.AppRole
}

func (s *tokenSource) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	s.role = role
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return appauth.Lease{Token: "installation-token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: "legion-implementer[bot]"}}, nil
}
func newCredentialHarness(t *testing.T, tokens appauth.Tokens) *harness {
	return newCredentialHarnessWithGrants(t, tokens, credential.New(nil))
}

func newCredentialHarnessWithGrants(t *testing.T, tokens appauth.Tokens, grants *credential.Grants) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor,
		BootTokens: h.tokens,
		Project: testProject,
		OperatorToken: testOperatorToken,
		Grants: grants,
		Tokens: tokens,
	}).Handler
	return h
}

func liveGrant(t *testing.T, h *harness, role claim.Role) GrantResponse {
	t.Helper()
	_, boot := h.launch("LEGION-208", role)
	registration := h.registered(boot, "ses_"+string(role))
	recorder := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: "ses_" + string(role),
		Secret:    registration.Secret,
		Tree:      "LEGION-208",
		Issue:     "LEGION-208",
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(t, recorder, &grant)
	return grant
}

func controllerGrant(t *testing.T, h *harness) GrantResponse {
	recorder := h.request(http.MethodPost, "/legion/v1/grants", map[string]any{}, http.Header{"Authorization": {"Bearer " + testOperatorToken}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("controller grant = %d: %s", recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(t, recorder, &grant)
	return grant
}

func TestControllerGrantIsRefusedByEveryRepositoryCredentialRoute(t *testing.T) {
	h := newCredentialHarness(t, &tokenSource{})
	for _, route := range []string{"/legion/v1/gh-token", "/legion/v1/git-credential", "/legion/v1/provisioning-credential"} {
		t.Run(route, func(t *testing.T) {
			grant := controllerGrant(t, h)
			recorder := h.request(http.MethodPost, route, GrantCredentialRequest{GrantID: grant.GrantID}, nil)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("%s = %d: %s", route, recorder.Code, recorder.Body)
			}
			var got Failure
			decodeInto(t, recorder, &got)
			if got.Code != "CONTROLLER_HAS_NO_REPOSITORY" {
				t.Fatalf("%s code = %q, want CONTROLLER_HAS_NO_REPOSITORY", route, got.Code)
			}
		})
	}
}


func TestProvisioningCredentialUsesImplementAppForArchitect(t *testing.T) {
	source := &tokenSource{}
	h := newCredentialHarness(t, source)
	grant := liveGrant(t, h, claim.RoleArchitect)
	recorder := h.request(http.MethodPost, "/legion/v1/provisioning-credential", GrantCredentialRequest{GrantID: grant.GrantID}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("provisioning credential = %d: %s", recorder.Code, recorder.Body)
	}
	if source.role != appauth.Implement {
		t.Fatalf("provisioning app role = %q, want %q", source.role, appauth.Implement)
	}
}

func TestCredentialRoutesRefuseExpiredAndUsedGrants(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h := newCredentialHarnessWithGrants(t, &tokenSource{}, credential.New(func() time.Time { return now }))
	expired := liveGrant(t, h, claim.RoleImplementer)
	now = now.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		grant GrantResponse
		code  string
	}{
		{name: "expired", grant: expired, code: "GRANT_EXPIRED"},
		{name: "used", grant: liveGrant(t, h, claim.RoleReviewer), code: "GRANT_USED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "used" {
				first := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: tc.grant.GrantID}, nil)
				if first.Code != http.StatusOK {
					t.Fatalf("first redemption = %d: %s", first.Code, first.Body)
				}
			}
			recorder := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: tc.grant.GrantID}, nil)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("redemption = %d: %s", recorder.Code, recorder.Body)
			}
			var got Failure
			decodeInto(t, recorder, &got)
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
		})
	}
}
func TestGitHubTokenRefusesClaimReplacedDuringTokenAwait(t *testing.T) {
	source := &tokenSource{started: make(chan struct{}), release: make(chan struct{})}
	h := newCredentialHarness(t, source)
	grant := liveGrant(t, h, claim.RoleImplementer)

	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answer <- h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil)
	}()

	// The first grant lookup has passed and the GitHub source is deliberately blocked. Repeating
	// registration on the live claim replaces its capability before the route may return a token.
	<-source.started
	machine, ok := h.supervisor.Machine("legion-legion-legion-208-implementer")
	if !ok {
		t.Fatal("implementer claim is not supervised")
	}
	h.registered(h.bootToken(machine.Claim().Token), "ses_implementer")
	close(source.release)

	recorder := <-answer
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("gh-token after replacement = %d: %s", recorder.Code, recorder.Body)
	}
	var got Failure
	decodeInto(t, recorder, &got)
	if got.Code != "GRANT_REVOKED" {
		t.Fatalf("code = %q, want GRANT_REVOKED", got.Code)
	}
}
