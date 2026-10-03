package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	once    sync.Once
	role    appauth.AppRole
	owner   string
}

// Token leases the role's App: the implement App is legion-implementer[bot], the review App
// legion-reviewer[bot]. A source with started blocks its first lease until release closes.
func (s *tokenSource) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	s.role = role
	s.owner = owner
	if s.started != nil {
		s.once.Do(func() {
			close(s.started)
			<-s.release
		})
	}
	name := "legion-implementer[bot]"
	if role == appauth.Review {
		name = "legion-reviewer[bot]"
	}
	return appauth.Lease{Token: "installation-token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: name}}, nil
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
		Controller:    h.store,
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

// reviewUnavailable is a token source whose review App cannot be leased.
type reviewUnavailable struct{ tokenSource }

func (s *reviewUnavailable) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	if role == appauth.Review {
		return appauth.Lease{}, errors.New("the review App is not installed for acme")
	}
	return s.tokenSource.Token(ctx, role, owner)
}

// gh-token names each of Legion's role Apps beside the caller's own, keyed by App role, so `legion
// threads resolve` can tell a Legion App's review thread from any other bot's and knows which
// login is the review App's. When any App's login cannot be read the logins are left out, and the
// command then applies no bot-thread rule; the caller's token still comes back.
// A Legion App whose login cannot be read turns the bot-thread rule off for that answer, so the
// daemon says so in its log, once: every `legion gh` call asks for a token, and a minute of GitHub
// failing must not write a line per call.
func TestAnUnreadableLegionAppLoginIsLoggedAtMostOnceAMinute(t *testing.T) {
	h := newHarness(t)
	var logged bytes.Buffer
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Tokens: &reviewUnavailable{}, GitHubOwner: "acme",
		Log: slog.New(slog.NewTextHandler(&logged, nil)),
	}).Handler
	grant := liveGrant(t, h, claim.RoleImplementer)
	for range 3 {
		if recorder := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("gh-token = %d: %s", recorder.Code, recorder.Body)
		}
	}
	if got := strings.Count(logged.String(), "could not read a Legion App's login"); got != 1 {
		t.Fatalf("logged the unreadable login %d times over three calls, want once:\n%s", got, logged.String())
	}
}

func TestGitHubTokenNamesEveryLegionAppLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source appauth.Tokens
		want   map[appauth.AppRole]string
	}{
		{"both Apps leased", &tokenSource{}, map[appauth.AppRole]string{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}},
		{"the review App unavailable", &reviewUnavailable{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCredentialHarness(t, tc.source)
			grant := liveGrant(t, h, claim.RoleImplementer)
			recorder := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("gh-token = %d: %s", recorder.Code, recorder.Body)
			}
			var got GitHubTokenResponse
			decodeInto(t, recorder, &got)
			if got.Token != "installation-token" || got.AppLogin != "legion-implementer[bot]" || !maps.Equal(got.LegionAppLogins, tc.want) {
				t.Fatalf("gh-token = %+v, want the implementer's token and Legion App logins %v", got, tc.want)
			}
		})
	}
}
