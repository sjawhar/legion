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

type tokenSource struct{}

// Token leases the role's App: the implement App is legion-implementer[bot], the review App
// legion-reviewer[bot].
func (tokenSource) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	name := "legion-implementer[bot]"
	if role == appauth.Review {
		name = "legion-reviewer[bot]"
	}
	return appauth.Lease{Token: "installation-token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: name}}, nil
}

func newCredentialHarness(t *testing.T, tokens appauth.Tokens) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor:    h.supervisor,
		BootTokens:    h.tokens,
		Project:       testProject,
		OperatorToken: testOperatorToken,
		Controller:    h.store,
		Grants:        credential.New(nil),
		Tokens:        tokens,
		GitHubOwner:   "acme",
	}).Handler
	return h
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

// A claim's capability belongs to its registered, running agent. When the process dies (before the
// relaunch registers), is suspended (its issue's close, or an operator), or is stopped, the old secret mints no
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
			assertFailure(t, h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: "LEGION-208", Status: "todo"}, nil),
				http.StatusForbidden, "GRANT_REVOKED")
			if stored := h.stored(token); len(stored.CapabilityHash) != 0 {
				t.Fatalf("stored claim after %s keeps capability hash %x", exit, stored.CapabilityHash)
			}
		})
	}
}
