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
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

type tokenSource struct {
	started chan struct{}
	release chan struct{}
	role    appauth.AppRole
	owner   string
}

func (s *tokenSource) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	s.role = role
	s.owner = owner
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
		Supervisor:    h.supervisor,
		BootTokens:    h.tokens,
		Project:       testProject,
		OperatorToken: testOperatorToken,
		Grants:        grants,
		Tokens:        tokens,
		GitHubOwner:   "acme",
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
	// An App installation belongs to the repository's owner, never to Legion's project token.
	if source.owner != "acme" {
		t.Fatalf("token minted for owner %q, want the configured repository owner acme", source.owner)
	}
}

// One bash command's grant serves every credential it asks for: `legion gh` run twice, and the git
// credential helper git calls more than once, all redeem the same grant until it expires.
func TestCredentialRoutesServeOneGrantUntilItExpires(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h := newCredentialHarnessWithGrants(t, &tokenSource{}, credential.New(func() time.Time { return now }))
	grant := liveGrant(t, h, claim.RoleImplementer)
	for _, route := range []string{"/legion/v1/gh-token", "/legion/v1/gh-token", "/legion/v1/git-credential", "/legion/v1/git-credential"} {
		if recorder := h.request(http.MethodPost, route, GrantCredentialRequest{GrantID: grant.GrantID}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s with a live grant = %d: %s", route, recorder.Code, recorder.Body)
		}
	}
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil),
		http.StatusForbidden, "GRANT_EXPIRED")
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: "never-minted"}, nil),
		http.StatusForbidden, "GRANT_UNAVAILABLE")
}

// A grant belongs to the registration that minted it: once the claim's capability is replaced, the
// same grant redeems no credential.
func TestCredentialRoutesRefuseAGrantWhoseClaimWasReplaced(t *testing.T) {
	h := newCredentialHarness(t, &tokenSource{})
	grant := liveGrant(t, h, claim.RoleImplementer)
	if first := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil); first.Code != http.StatusOK {
		t.Fatalf("gh-token before replacement = %d: %s", first.Code, first.Body)
	}
	machine, ok := h.supervisor.Machine("legion-legion-legion-208-implementer")
	if !ok {
		t.Fatal("implementer claim is not supervised")
	}
	h.registered(h.bootToken(machine.Claim().Token), "ses_implementer")
	for _, route := range []string{"/legion/v1/gh-token", "/legion/v1/git-credential"} {
		assertFailure(t, h.request(http.MethodPost, route, GrantCredentialRequest{GrantID: grant.GrantID}, nil),
			http.StatusForbidden, "GRANT_REVOKED")
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

// A claim's capability belongs to its registered, running agent. When the process dies (before the
// relaunch registers), is suspended at the end of its phase, or is stopped, the old secret mints no
// grant and a grant it already minted redeems nothing, as the shipped daemon revokes a session's
// capability and grants on death, retirement, and teardown.
func TestAClaimLeavingItsRunningStatesRevokesItsSecretAndGrants(t *testing.T) {
	for _, exit := range []string{"died", "suspended", "stopped"} {
		t.Run(exit, func(t *testing.T) {
			h := newCredentialHarness(t, &tokenSource{})
			token, boot := h.launch("LEGION-208", claim.RoleImplementer)
			registration := h.registered(boot, "ses_implementer")
			machine, _ := h.supervisor.Machine(token)
			if err := machine.Handle(h.ctx, supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: "ses_implementer"}); err != nil {
				t.Fatalf("ready: %v", err)
			}
			mint := func() *httptest.ResponseRecorder {
				return h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{SessionID: "ses_implementer", Secret: registration.Secret, Tree: "LEGION-208", Issue: "LEGION-208"}, nil)
			}
			minted := mint()
			if minted.Code != http.StatusOK {
				t.Fatalf("mint while running = %d: %s", minted.Code, minted.Body)
			}
			var grant GrantResponse
			decodeInto(t, minted, &grant)

			switch exit {
			case "died":
				h.relaunch(token)
			case "suspended":
				if err := machine.Handle(h.ctx, supervise.RequestSuspend{Claim: token}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
			case "stopped":
				if err := machine.Handle(h.ctx, supervise.RequestStop{Claim: token}); err != nil {
					t.Fatalf("stop: %v", err)
				}
			}
			assertFailure(t, mint(), http.StatusForbidden, "INVALID_SESSION_SECRET")
			assertFailure(t, h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil),
				http.StatusForbidden, "GRANT_REVOKED")
			if stored := h.stored(token); len(stored.CapabilityHash) != 0 {
				t.Fatalf("stored claim after %s keeps capability hash %x", exit, stored.CapabilityHash)
			}
		})
	}
}
