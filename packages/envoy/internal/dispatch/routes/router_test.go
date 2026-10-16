package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// sharedAgentTokens is a DISPATCH_AGENT_TOKEN setting parsed as the server parses it at boot.
func sharedAgentTokens(t *testing.T, setting string) *auth.SharedAgentTokens {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(setting)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens(%q): %v", setting, err)
	}
	return tokens
}

type memorySessionStore struct {
	mu          sync.Mutex
	generations map[string]int64
}

func (s *memorySessionStore) CurrentSessionGeneration(_ context.Context, login string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation, found := s.generations[login]
	return generation, found, nil
}

func (s *memorySessionStore) EnsureSession(_ context.Context, login string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation, found := s.generations[login]
	if !found {
		s.generations[login] = 0
	}
	return generation, nil
}

func (s *memorySessionStore) RevokeSessions(_ context.Context, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations[login]++
	return nil
}

// memoryPeopleStore is auth.PeopleStore in memory. A sign-in under another subject than the one it
// holds for the address ends the address's sessions in sessions, as store.PgPeopleStore does; a
// store with no sessions refuses such a sign-in.
type memoryPeopleStore struct {
	mu       sync.Mutex
	people   map[string]auth.PersonMembership
	sessions *memorySessionStore
	// afterSignIn, when set, runs once after the next SignIn or SignInSubject returns.
	afterSignIn func()
}

func newMemoryPeople() *memoryPeopleStore {
	return &memoryPeopleStore{people: map[string]auth.PersonMembership{}}
}

func (s *memoryPeopleStore) Record(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.people[email]; !found {
		s.people[email] = auth.PersonMembership{}
	}
	return nil
}

func (s *memoryPeopleStore) runAfterSignIn() {
	// afterSignIn runs once, after a sign-in (SignIn or SignInSubject) has committed and released
	// the store: a test's window between a sign-in's commit and its cookie.
	if hook := s.afterSignIn; hook != nil {
		s.afterSignIn = nil
		hook()
	}
}

func (s *memoryPeopleStore) SignIn(_ context.Context, email, refreshToken string, confirmedAt time.Time) error {
	defer s.runAfterSignIn()
	s.mu.Lock()
	defer s.mu.Unlock()
	membership := s.people[email]
	membership.RefreshToken, membership.ConfirmedAt = refreshToken, confirmedAt
	s.people[email] = membership
	return nil
}

func (s *memoryPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken string, confirmedAt time.Time) error {
	defer s.runAfterSignIn()
	s.mu.Lock()
	defer s.mu.Unlock()
	membership, existed := s.people[email]
	if existed && membership.Subject != subject {
		if s.sessions == nil {
			return errors.New("memoryPeopleStore: a sign-in under another subject has no sessions to end")
		}
		if err := s.sessions.RevokeSessions(ctx, email); err != nil {
			return err
		}
	}
	membership.Subject = subject
	membership.RefreshToken, membership.ConfirmedAt = refreshToken, confirmedAt
	s.people[email] = membership
	return nil
}

func (s *memoryPeopleStore) Membership(_ context.Context, email string) (auth.PersonMembership, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	membership, found := s.people[email]
	return membership, found, nil
}

func (s *memoryPeopleStore) Confirm(_ context.Context, email, refreshToken string, confirmedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	membership := s.people[email]
	membership.RefreshToken, membership.ConfirmedAt = refreshToken, confirmedAt
	s.people[email] = membership
	return nil
}

func (s *memoryPeopleStore) ConfirmSubject(_ context.Context, email, subject, refreshToken string, confirmedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	membership := s.people[email]
	if membership.Subject != subject {
		return nil
	}
	membership.RefreshToken, membership.ConfirmedAt = refreshToken, confirmedAt
	s.people[email] = membership
	return nil
}

func (s *memoryPeopleStore) End(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if membership, found := s.people[email]; found {
		s.people[email] = auth.PersonMembership{Subject: membership.Subject}
	}
	return nil
}

func (s *memoryPeopleStore) get(email string) (auth.PersonMembership, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	membership, found := s.people[email]
	return membership, found
}

const (
	rigClientID = "dispatch-client"
	rigGroup    = "dispatch-members"
	rigEmail    = "a.b+c@d.example"
	rigDomain   = "example.com"
)

// signInRig is a Dispatch router signing people in through a fake sign-in issuer, with the
// cookie identity production runs and a clock the membership refresh reads.
type signInRig struct {
	handler  http.Handler
	ctx      *AppContext
	issuer   *oidctest.Issuer
	people   *memoryPeopleStore
	sessions *memorySessionStore
	mu       sync.Mutex
	now      time.Time
}

// newSignInRig is a router signing the members of rigGroup in through a fake Cognito sign-in pool.
func newSignInRig(t *testing.T, serverURL string) *signInRig {
	t.Helper()
	return newSignInRigFor(t, serverURL, identity.GroupRule{Group: rigGroup}, "")
}

// newDomainSignInRig is a router signing the people of the Workspace domain rigDomain in through a
// fake issuer that mints Google's claims.
func newDomainSignInRig(t *testing.T) *signInRig {
	t.Helper()
	return newSignInRigFor(t, "", identity.DomainRule{Domain: rigDomain}, rigDomain)
}

func newSignInRigFor(t *testing.T, serverURL string, rule identity.SignInRule, hostedDomain string) *signInRig {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), rigClientID, "dispatch-secret")
	flow, err := oidc.NewCodeFlow(context.Background(), issuer.URL(), rigClientID, "dispatch-secret", hostedDomain)
	if err != nil {
		t.Fatalf("NewCodeFlow: %v", err)
	}
	rig := &signInRig{issuer: issuer, people: newMemoryPeople(), sessions: &memorySessionStore{generations: map[string]int64{}}, now: time.Now()}
	rig.people.sessions = rig.sessions
	membership := &identity.Membership{People: rig.people, Sessions: rig.sessions, SignIn: flow, Rule: rule, Now: rig.clock}
	rig.ctx, err = BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		People:     rig.people,
		Sessions:   rig.sessions,
		Identity:   identity.CookieIdentity{SigningKey: "signing-key", Sessions: rig.sessions, Membership: membership},
		SignIn:     flow,
		SignInRule: rule,
		ServerURL:  serverURL,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	rig.handler, _ = New(rig.ctx)
	return rig
}

func (r *signInRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *signInRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// personClaims is a sign-in pool ID token's claims for a person the Google provider federated:
// its email claim names someone else, as a person can make it do.
func personClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "person-subject",
		"cognito:username": "ExampleIdP_A.B+C@D.example",
		"cognito:groups":   groups,
		"email":            "someone-else@d.example",
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	}
}

// start opens a sign-in and returns the pool's authorization URL and the browser's state cookie.
func (r *signInRig) start(t *testing.T, target string) (*url.URL, *http.Cookie) {
	t.Helper()
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusFound {
		t.Fatalf("start status: got %d body=%s, want %d", response.Code, response.Body.String(), http.StatusFound)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse start location: %v", err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("sign-in start cookies = %#v, want one state cookie", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != oauthStateCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != oauthStateMaxAge {
		t.Fatalf("sign-in state cookie = %#v, want HttpOnly SameSite=Lax short-lived nonce", cookie)
	}
	return location, cookie
}

// signIn runs a whole sign-in as the person claims names: start, the pool's sign-in, callback.
func (r *signInRig) signIn(t *testing.T, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	r.issuer.SignInAs(claims)
	authURL, stateCookie := r.start(t, "http://dispatch.test/auth/start?next=/issues/CORE-1")
	code, state := r.issuer.Authorize(t, authURL.String())
	callback := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	callback.AddCookie(stateCookie)
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, callback)
	return response
}

func sessionCookie(response *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			return cookie
		}
	}
	return nil
}

func (r *signInRig) whoami(cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// The person is named by the email their provider username carries, lowercased, never by the
// email claim; a member of the group gets a session and lands on the page they asked for.
func TestSignInNamesThePersonByTheirUsernameNotTheEmailClaim(t *testing.T) {
	rig := newSignInRig(t, "")
	response := rig.signIn(t, personClaims("other-group", rigGroup))
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/issues/CORE-1" {
		t.Fatalf("callback: status %d Location %q body=%s, want 302 to /issues/CORE-1", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	cookie := sessionCookie(response)
	if cookie == nil {
		t.Fatal("a member did not receive a session cookie")
	}
	whoami := rig.whoami(cookie)
	if whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"login":"`+rigEmail+`"`) {
		t.Fatalf("whoami: status %d body %s, want %s", whoami.Code, whoami.Body.String(), rigEmail)
	}
	membership, found := rig.people.get(rigEmail)
	if !found || membership.RefreshToken == "" {
		t.Fatalf("the sign-in kept no refresh token: %#v found=%t", membership, found)
	}
	if _, found := rig.people.get("someone-else@d.example"); found {
		t.Fatal("the email claim's address was recorded as a person")
	}
}

// Someone the pool vouches for but who is not in the group is told so in the browser and named in
// the log; they get neither a session nor a recorded sign-in.
func TestSignInRefusesSomeoneOutsideTheGroupWithPageAndLog(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	rig := newSignInRig(t, "")
	response := rig.signIn(t, personClaims("other-group"))
	if response.Code != http.StatusForbidden || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("callback: status %d Content-Type %q, want 403 text/html", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if !strings.Contains(body, rigEmail+" is not in the group that may use Dispatch") || !strings.Contains(body, `href="/auth/start"`) {
		t.Errorf("refusal page does not name the person and offer another sign-in: %s", body)
	}
	if sessionCookie(response) != nil {
		t.Error("someone outside the group received a session cookie")
	}
	if _, found := rig.people.get(rigEmail); found {
		t.Error("someone outside the group was recorded as signed in")
	}
	if logged := logs.String(); !strings.Contains(logged, "sign-in refused") || !strings.Contains(logged, rigEmail) {
		t.Errorf("refusal was not logged with the person: %q", logged)
	}
}

func TestSignInRefusesAUsernameNoProviderIssued(t *testing.T) {
	rig := newSignInRig(t, "")
	claims := personClaims(rigGroup)
	claims["cognito:username"] = "sami@d.example"
	delete(claims, "identities")
	response := rig.signIn(t, claims)
	if response.Code != http.StatusForbidden || sessionCookie(response) != nil {
		t.Fatalf("callback for a pool account: status %d cookie %v, want 403 and no session", response.Code, sessionCookie(response))
	}
}

// While the person stays a member, the session renews its membership at least hourly.
func TestSessionRenewsItsMembershipHourly(t *testing.T) {
	rig := newSignInRig(t, "")
	cookie := sessionCookie(rig.signIn(t, personClaims(rigGroup)))
	rig.advance(59 * time.Minute)
	if response := rig.whoami(cookie); response.Code != http.StatusOK || rig.issuer.Refreshes() != 0 {
		t.Fatalf("within the hour: status %d refreshes %d, want 200 and none", response.Code, rig.issuer.Refreshes())
	}
	rig.advance(2 * time.Minute)
	if response := rig.whoami(cookie); response.Code != http.StatusOK || rig.issuer.Refreshes() != 1 {
		t.Fatalf("after the hour: status %d refreshes %d, want 200 and one refresh", response.Code, rig.issuer.Refreshes())
	}
}

func TestSessionEndsWhenTheRefreshFails(t *testing.T) {
	rig := newSignInRig(t, "")
	cookie := sessionCookie(rig.signIn(t, personClaims(rigGroup)))
	rig.issuer.RevokeGrants()
	rig.advance(2 * time.Hour)
	if response := rig.whoami(cookie); response.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after the pool refused the refresh: status %d body %s, want 401", response.Code, response.Body.String())
	}
	if generation, _, _ := rig.sessions.CurrentSessionGeneration(context.Background(), rigEmail); generation != 1 {
		t.Fatalf("session generation = %d, want 1: every cookie of the person revoked", generation)
	}
}

func TestSessionEndsWhenThePersonLeavesTheGroup(t *testing.T) {
	rig := newSignInRig(t, "")
	cookie := sessionCookie(rig.signIn(t, personClaims(rigGroup)))
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
	rig.advance(2 * time.Hour)
	if response := rig.whoami(cookie); response.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after leaving the group: status %d body %s, want 401", response.Code, response.Body.String())
	}
	if again := rig.signIn(t, personClaims("other-group")); again.Code != http.StatusForbidden {
		t.Fatalf("signing in again outside the group: status %d, want 403", again.Code)
	}
}

// rigGoogleEmail is the person googleClaims names, as Dispatch names them: lowercased.
const rigGoogleEmail = "a.person@example.com"

// googleClaims is a Google ID token's claims for a verified account of the Workspace domain
// rigDomain under subject.
func googleClaims(subject string) map[string]any {
	return map[string]any{
		"sub":            subject,
		"email":          "A.Person@example.com",
		"email_verified": true,
		"hd":             rigDomain,
	}
}

// captureRouterLogs sends slog's default logger to a buffer until t ends.
func captureRouterLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// A verified account of the domain signs in: it gets a session, lands on the page it asked for, and
// is named by its lowercased email, with the account's subject recorded beside the refresh token.
func TestDomainSignInAdmitsAVerifiedAccountOfTheDomain(t *testing.T) {
	rig := newDomainSignInRig(t)
	response := rig.signIn(t, googleClaims("google-subject-1"))
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/issues/CORE-1" {
		t.Fatalf("callback: status %d Location %q body=%s, want 302 to /issues/CORE-1", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	cookie := sessionCookie(response)
	if cookie == nil {
		t.Fatal("a member of the domain did not receive a session cookie")
	}
	if whoami := rig.whoami(cookie); whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"login":"`+rigGoogleEmail+`"`) {
		t.Fatalf("whoami: status %d body %s, want %s", whoami.Code, whoami.Body.String(), rigGoogleEmail)
	}
	if membership, found := rig.people.get(rigGoogleEmail); !found || membership.RefreshToken == "" || membership.Subject != "google-subject-1" {
		t.Fatalf("the sign-in recorded %#v found=%t, want the refresh token and google-subject-1", membership, found)
	}
}

// An account outside the domain, one whose email Google has not verified, and one with no email
// each reach Dispatch's refusal page with their reason; none gets a session or a recorded sign-in,
// and each refusal is logged with the email and hd.
func TestDomainSignInRefusesWithItsReasonOnThePageAndInTheLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(claims map[string]any)
		page   string
		logged []string
	}{
		{"an account of another domain", func(claims map[string]any) { claims["hd"] = "other.example" },
			rigGoogleEmail + " is not in the domain that may use Dispatch", []string{"email=A.Person@example.com", "hd=other.example"}},
		{"an account outside any Workspace", func(claims map[string]any) { delete(claims, "hd") },
			rigGoogleEmail + " is not in the domain that may use Dispatch", []string{"email=A.Person@example.com", `hd=""`}},
		{"an email Google has not verified", func(claims map[string]any) { claims["email_verified"] = false },
			rigGoogleEmail + ": email not verified", []string{"email=A.Person@example.com", "hd=" + rigDomain}},
		{"no email", func(claims map[string]any) { delete(claims, "email") },
			"No email: this sign-in carries no email address", []string{`email=""`, "hd=" + rigDomain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureRouterLogs(t)
			rig := newDomainSignInRig(t)
			claims := googleClaims("google-subject-1")
			tc.change(claims)
			response := rig.signIn(t, claims)
			if response.Code != http.StatusForbidden || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("callback: status %d Content-Type %q, want 403 text/html", response.Code, response.Header().Get("Content-Type"))
			}
			if body := response.Body.String(); !strings.Contains(body, tc.page) || !strings.Contains(body, `href="/auth/start"`) {
				t.Errorf("refusal page does not say %q and offer another sign-in: %s", tc.page, body)
			}
			if sessionCookie(response) != nil {
				t.Error("a refused sign-in received a session cookie")
			}
			if len(rig.people.people) != 0 {
				t.Errorf("a refused sign-in was recorded: %#v", rig.people.people)
			}
			logged := logs.String()
			if !strings.Contains(logged, "sign-in refused") {
				t.Errorf("the refusal was not logged: %q", logged)
			}
			for _, want := range tc.logged {
				if !strings.Contains(logged, want) {
					t.Errorf("the refusal's log line does not carry %s: %q", want, logged)
				}
			}
		})
	}
}

// The hourly confirmation runs the domain rule on the refreshed token: a member's session renews,
// and an account that left the domain, changed its email, became another account, or whose refresh
// Google refused, loses every session.
func TestDomainSessionRenewsHourlyAndEndsWhenTheAccountNoLongerQualifies(t *testing.T) {
	rig := newDomainSignInRig(t)
	cookie := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
	rig.advance(61 * time.Minute)
	if response := rig.whoami(cookie); response.Code != http.StatusOK || rig.issuer.Refreshes() != 1 {
		t.Fatalf("after the hour: status %d refreshes %d, want 200 and one refresh", response.Code, rig.issuer.Refreshes())
	}
	for name, end := range map[string]func(issuer *oidctest.Issuer){
		"another domain": func(issuer *oidctest.Issuer) {
			issuer.UpdateGrants(func(c map[string]any) { c["hd"] = "other.example" })
		},
		"another email": func(issuer *oidctest.Issuer) {
			issuer.UpdateGrants(func(c map[string]any) { c["email"] = "b.person@example.com" })
		},
		"another Google account": func(issuer *oidctest.Issuer) {
			issuer.UpdateGrants(func(c map[string]any) { c["sub"] = "google-subject-2" })
		},
		"a refused refresh": func(issuer *oidctest.Issuer) { issuer.RevokeGrants() },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDomainSignInRig(t)
			cookie := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
			end(rig.issuer)
			rig.advance(2 * time.Hour)
			if response := rig.whoami(cookie); response.Code != http.StatusUnauthorized {
				t.Fatalf("whoami: status %d body %s, want 401", response.Code, response.Body.String())
			}
			if generation, _, _ := rig.sessions.CurrentSessionGeneration(context.Background(), rigGoogleEmail); generation != 1 {
				t.Fatalf("session generation = %d, want 1: every cookie of the person revoked", generation)
			}
		})
	}
}

// An address a Workspace admin gave to a new account: once the new account signs in, the old
// account's cookies answer 401 while the new one's work, even though the confirmation would pass
// on the new account's refresh token.
func TestDomainSignInUnderAnotherSubjectEndsTheAddressSessions(t *testing.T) {
	rig := newDomainSignInRig(t)
	first := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
	second := sessionCookie(rig.signIn(t, googleClaims("google-subject-2")))
	if first == nil || second == nil {
		t.Fatalf("sign-in cookies %v and %v, want both", first, second)
	}
	if response := rig.whoami(first); response.Code != http.StatusUnauthorized {
		t.Errorf("the old account's cookie: whoami %d %s, want 401", response.Code, response.Body.String())
	}
	if response := rig.whoami(second); response.Code != http.StatusOK {
		t.Errorf("the new account's cookie: whoami %d %s, want 200", response.Code, response.Body.String())
	}
	if membership, _ := rig.people.get(rigGoogleEmail); membership.Subject != "google-subject-2" {
		t.Errorf("recorded subject %q, want google-subject-2", membership.Subject)
	}
}

// A cookie from before the switch to domain mode — a row recording no subject at all, a
// Cognito-era, header- or dev-recorded row, with a session already live — stops working the
// first time the address signs in through the domain, even though no other Google account has
// taken it yet.
func TestDomainSignInEndsACookieFromBeforeTheSwitchToDomainMode(t *testing.T) {
	rig := newDomainSignInRig(t)
	if err := rig.people.SignIn(context.Background(), rigGoogleEmail, "old-refresh-token", rig.clock()); err != nil {
		t.Fatalf("sign in the pre-switch row: %v", err)
	}
	generation, err := rig.sessions.EnsureSession(context.Background(), rigGoogleEmail)
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	before, err := http.ParseSetCookie(auth.IssueSessionCookie(rigGoogleEmail, generation, "signing-key", true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	if response := rig.whoami(before); response.Code != http.StatusOK {
		t.Fatalf("the pre-switch cookie before signing in: whoami %d %s, want 200", response.Code, response.Body.String())
	}
	after := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
	if after == nil {
		t.Fatal("a member of the domain did not receive a session cookie")
	}
	if response := rig.whoami(before); response.Code != http.StatusUnauthorized {
		t.Errorf("the pre-switch cookie: whoami %d %s, want 401", response.Code, response.Body.String())
	}
	if response := rig.whoami(after); response.Code != http.StatusOK {
		t.Errorf("the domain sign-in's own cookie: whoami %d %s, want 200", response.Code, response.Body.String())
	}
	if membership, _ := rig.people.get(rigGoogleEmail); membership.Subject != "google-subject-1" {
		t.Errorf("recorded subject %q, want google-subject-1", membership.Subject)
	}
}

// The same account signing in again, from another browser, keeps its other sessions.
func TestDomainSignInUnderTheSameSubjectKeepsItsOtherSessions(t *testing.T) {
	rig := newDomainSignInRig(t)
	first := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
	second := sessionCookie(rig.signIn(t, googleClaims("google-subject-1")))
	for name, cookie := range map[string]*http.Cookie{"first": first, "second": second} {
		if response := rig.whoami(cookie); response.Code != http.StatusOK {
			t.Errorf("the %s cookie: whoami %d %s, want 200", name, response.Code, response.Body.String())
		}
	}
}

// A sign-in through a Cognito pool records no subject, though the pool's token carries one.
func TestGroupSignInRecordsNoSubject(t *testing.T) {
	rig := newSignInRig(t, "")
	if response := rig.signIn(t, personClaims(rigGroup)); response.Code != http.StatusFound {
		t.Fatalf("callback: status %d body %s, want 302", response.Code, response.Body.String())
	}
	if membership, found := rig.people.get(rigEmail); !found || membership.Subject != "" {
		t.Fatalf("the group sign-in recorded %#v, want no subject", membership)
	}
}

func TestOAuthStateCookieMatchesCookieMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		insecure bool
		secure   bool
	}{
		{name: "TLS default", secure: true},
		{name: "HTTP development mode", insecure: true, secure: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSignInRig(t, "")
			rig.ctx.InsecureCookie = tc.insecure
			_, nonce := rig.start(t, "http://dispatch.test/auth/start")
			response := rig.signIn(t, personClaims(rigGroup))
			if nonce.Secure != tc.secure {
				t.Fatalf("sign-in nonce Secure = %t, want %t", nonce.Secure, tc.secure)
			}
			issued := false
			for _, cookie := range response.Result().Cookies() {
				issued = issued || cookie.Name == "dsession"
				if cookie.Secure != tc.secure {
					t.Errorf("callback cookie %s Secure = %t, want %t", cookie.Name, cookie.Secure, tc.secure)
				}
			}
			if !issued {
				t.Errorf("callback set no dsession cookie: %v", response.Result().Cookies())
			}
		})
	}
}

// Another Google account's sign-in to the same address that commits after a sign-in has recorded
// its own account and before that sign-in's cookie is issued: the earlier sign-in gets no working
// session, since its cookie would carry the generation the later one advanced to.
func TestDomainSignInGetsNoSessionWhenAnotherAccountTakesTheAddressBeforeItsCookie(t *testing.T) {
	rig := newDomainSignInRig(t)
	rig.people.afterSignIn = func() {
		if err := rig.people.SignInSubject(context.Background(), rigGoogleEmail, "google-subject-2", "refresh-2", time.Now()); err != nil {
			t.Errorf("the other account's sign-in: %v", err)
		}
	}
	response := rig.signIn(t, googleClaims("google-subject-1"))
	if cookie := sessionCookie(response); cookie != nil {
		if whoami := rig.whoami(cookie); whoami.Code != http.StatusUnauthorized {
			t.Fatalf("the earlier account's cookie: whoami %d %s, want 401", whoami.Code, whoami.Body.String())
		}
	}
	if response.Code == http.StatusFound {
		t.Errorf("the callback answered 302 to %q, want a refusal: the address belongs to another account now", response.Header().Get("Location"))
	}
	if generation, _, _ := rig.sessions.CurrentSessionGeneration(context.Background(), rigGoogleEmail); generation != 1 {
		t.Errorf("session generation %d, want 1: the other account's sign-in ended the address's sessions", generation)
	}
}

func TestAuthStartWithoutSignInIsUnavailable(t *testing.T) {
	const want = "Google sign-in is not configured on this server (DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET, one of DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN)"
	handler, _ := newTestRouter(t)
	for _, target := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test"+target, nil))
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusServiceUnavailable || body.Code != "SIGNIN_UNCONFIGURED" || body.Error != want {
			t.Errorf("%s with no sign-in: status %d body %s (%v), want 503 SIGNIN_UNCONFIGURED with %q", target, response.Code, response.Body.String(), err, want)
		}
	}
}

// newTestRouter injects header identity without configuring sign-in.
func newTestRouter(t *testing.T) (http.Handler, *AppContext) {
	t.Helper()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, _ := New(ctx)
	return handler, ctx
}

func TestWhoamiUsesHeaderIdentity(t *testing.T) {
	handler, ctx := newTestRouter(t)
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", "Sami@D.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"sami@d.example"`) ||
		!strings.Contains(response.Body.String(), `"kind":"user"`) {
		t.Errorf("header response: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, found := ctx.People.(*memoryPeopleStore).get("sami@d.example"); !found {
		t.Error("the header's person was not recorded")
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil))
	if response.Code != http.StatusUnauthorized {
		t.Errorf("missing header status: got %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

// The GitHub proxy serves people only, reads only, and the GraphQL proxy no longer exists.
func TestGitHubProxyServesPeopleOnlyAndOnlyReads(t *testing.T) {
	handler, _ := newTestRouter(t)
	serve := func(method, target, person string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://dispatch.test"+target, nil)
		if person != "" {
			request.Header.Set("X-Dispatch-User", person)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := serve(http.MethodGet, "/api/github/rest/repos/acme/web/pulls/1", ""); response.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GitHub read: status %d, want 401", response.Code)
	}
	if response := serve(http.MethodPost, "/api/github/rest/repos/acme/web/pulls/1", "sami@d.example"); response.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST through the GitHub proxy: status %d, want 405", response.Code)
	}
	if response := serve(http.MethodGet, "/api/github/rest/repos/acme/web/pulls/1", "sami@d.example"); response.Code != http.StatusServiceUnavailable {
		t.Errorf("GitHub read with no App: status %d body %s, want 503", response.Code, response.Body.String())
	}
	if response := serve(http.MethodPost, "/api/github/graphql", "sami@d.example"); response.Code != http.StatusNotFound {
		t.Errorf("POST /api/github/graphql: status %d, want 404 (the GraphQL proxy is gone)", response.Code)
	}
}

func TestAuthStartCapsPendingStates(t *testing.T) {
	rig := newSignInRig(t, "")
	for requestNumber := range 1000 {
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
		if response.Code != http.StatusFound {
			t.Fatalf("start %d status: got %d, want %d", requestNumber, response.Code, http.StatusFound)
		}
	}
	overflow := httptest.NewRecorder()
	rig.handler.ServeHTTP(overflow, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
	if overflow.Code != http.StatusTooManyRequests {
		t.Fatalf("overflow status: got %d body=%s, want %d", overflow.Code, overflow.Body.String(), http.StatusTooManyRequests)
	}
}

func TestOAuthCallbackRejectsExpiredPendingState(t *testing.T) {
	rig := newSignInRig(t, "")
	router := &router{
		ctx: rig.ctx,
		pendingStates: map[string]pendingState{
			"expired": {next: "/", expiresAt: time.Now().Add(-time.Second)},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code=code&state=expired", nil)
	response := httptest.NewRecorder()
	router.authCallback(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid or expired state") {
		t.Fatalf("expired callback status: got %d body=%s", response.Code, response.Body.String())
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == oauthStateCookie && cookie.MaxAge < 0 {
			return
		}
	}
	t.Fatalf("expired callback did not clear the sign-in state cookie: %#v", response.Result().Cookies())
}

func TestAuthStartEvictsExpiredPendingStates(t *testing.T) {
	rig := newSignInRig(t, "")
	router := &router{ctx: rig.ctx, pendingStates: make(map[string]pendingState, maxPendingStates)}
	for token := range maxPendingStates {
		router.pendingStates[fmt.Sprintf("expired-%d", token)] = pendingState{expiresAt: time.Now().Add(-time.Second)}
	}
	response := httptest.NewRecorder()
	router.authStart(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
	if response.Code != http.StatusFound || len(router.pendingStates) != 1 {
		t.Fatalf("expired-state eviction: status=%d states=%d, want redirect with one fresh state", response.Code, len(router.pendingStates))
	}
}

func TestStaticHandlerServesSpaShellForUnknownRoute(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unroutable", nil))

	// An unknown path isn't a static asset, so the server hands back the SPA
	// shell (like any client-routed app) and lets the client router render its
	// own not-found view, instead of a bare JSON 404 the client never sees.
	if response.Code != http.StatusOK {
		t.Fatalf("unknown route status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != "<!doctype html>" {
		t.Fatalf("unknown route body: got %q, want dashboard shell", response.Body.String())
	}
}

func TestStaticHandlerServesIndexAtRoot(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("root status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != "<!doctype html>" {
		t.Fatalf("root body: got %q, want dashboard shell", response.Body.String())
	}
}

func TestStaticHandlerServesDistDirectorySpelledThroughDotDot(t *testing.T) {
	parent := t.TempDir()
	webDist := filepath.Join(parent, "web", "dist")
	for _, dir := range []string{webDist, filepath.Join(parent, "web", "build")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	// The directory as an operator can spell it from a sibling package, never cleaned:
	// DISPATCH_WEB_DIST="$PWD/../dispatch/web/dist" run from packages/envoy.
	sep := string(filepath.Separator)
	context.WebDistDir = filepath.Join(parent, "web", "build") + sep + ".." + sep + "dist"

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || response.Body.String() != "<!doctype html>" {
		t.Fatalf("root through %q: got %d %q, want 200 and the dashboard shell", context.WebDistDir, response.Code, response.Body.String())
	}
}

// The static handler is called directly, not through the router's mux: the mux redirects a path
// holding `..` before any handler runs, so only the handler's own rooted clean of the request path
// is under test here. A request whose `..` would climb out of the dist directory answers exactly as
// a file missing inside it does, so a file outside it is neither served nor told apart from one
// that is not there.
func TestStaticHandlerKeepsRequestsInsideTheDistDirectory(t *testing.T) {
	parent := t.TempDir()
	webDist := filepath.Join(parent, "dist")
	if err := os.MkdirAll(webDist, 0o700); err != nil {
		t.Fatalf("make %s: %v", webDist, err)
	}
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	_, context := newTestRouter(t)
	context.WebDistDir = webDist
	static := &router{ctx: context}
	serve := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		static.staticHandler(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}

	missing := serve("/absent.txt")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("a file missing from the dist directory: got %d %q, want 404", missing.Code, missing.Body.String())
	}
	for _, path := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/../absent.txt"} {
		t.Run(path, func(t *testing.T) {
			response := serve(path)
			if response.Code != missing.Code || response.Body.String() != missing.Body.String() {
				t.Fatalf("%s: got %d %q, want the dist directory's own missing-file answer %d %q", path, response.Code, response.Body.String(), missing.Code, missing.Body.String())
			}
		})
	}
}

func TestStaticHandlerServesSpaShellForBrowserDeepLink(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	for _, path := range []string{
		"/issues/CORE-1/not-a-tab",
		"/issues",
		// An artifact slug is user-controlled and may legitimately contain a
		// dot; it must never be misclassified as a missing static asset.
		"/issues/CORE-1/artifacts/notes.md",
	} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("deep link status for %s: got %d, want %d; body=%s", path, response.Code, http.StatusOK, response.Body.String())
			}
			if response.Body.String() != "<!doctype html>" {
				t.Fatalf("deep link body for %s: got %q, want dashboard shell", path, response.Body.String())
			}
		})
	}
}

func TestStaticHandlerRoutingRules(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	for _, tc := range []struct {
		path string
		want string // "404" | "shell"
	}{
		// Exact reserved roots: no static asset or SPA route lives here.
		{path: "/api", want: "404"},
		{path: "/auth", want: "404"},
		{path: "/ws", want: "404"},
		{path: "/assets", want: "404"},
		// Reserved subpaths.
		{path: "/api/v1/does-not-exist", want: "404"},
		{path: "/auth/does-not-exist", want: "404"},
		{path: "/ws/does-not-exist", want: "404"},
		{path: "/assets/missing.js", want: "404"},
		{path: "/healthz/extra", want: "404"},
		// An API path typed without its /api prefix is an API mistake, not a
		// browser route: it must never come back as the SPA shell.
		{path: "/v1", want: "404"},
		{path: "/v1/issues", want: "404"},
		// A path that merely starts with the same characters as a reserved
		// root, but isn't the root or a subpath of it, is a real browser
		// route and must still get the SPA shell.
		{path: "/apix", want: "shell"},
		// A missing static asset outside any reserved root.
		{path: "/favicon.png", want: "404"},
		// /healthz belongs to the process's outer mux (cmd/dispatch), mounted above this
		// router; the router has no health route of its own. The root stays reserved so a
		// probe that reaches the fallback gets a JSON 404 rather than a dashboard shell
		// that any 200-checking prober would read as healthy.
		{path: "/healthz", want: "404"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))

			switch tc.want {
			case "404":
				if response.Code != http.StatusNotFound {
					t.Fatalf("status for %s: got %d, want %d; body=%s", tc.path, response.Code, http.StatusNotFound, response.Body.String())
				}
				if response.Body.String() == "<!doctype html>" {
					t.Fatalf("%s served the SPA shell instead of a 404", tc.path)
				}
			case "shell":
				if response.Code != http.StatusOK {
					t.Fatalf("status for %s: got %d, want %d; body=%s", tc.path, response.Code, http.StatusOK, response.Body.String())
				}
				if response.Body.String() != "<!doctype html>" {
					t.Fatalf("%s body: got %q, want dashboard shell", tc.path, response.Body.String())
				}
			}
		})
	}
}

// The built dashboard's own files live under /assets; that root is reserved against the SPA
// fallback, never against serving the file.
func TestStaticHandlerServesBuiltAssets(t *testing.T) {
	webDist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(webDist, "assets"), 0o700); err != nil {
		t.Fatalf("make assets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDist, "assets", "index-abc123.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if response.Code != http.StatusOK || response.Body.String() != "console.log(1)" {
		t.Fatalf("asset: status %d body %q", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":\"not found\"}\n" {
		t.Fatalf("missing asset: status %d body %q", response.Code, response.Body.String())
	}
}

// A page's HTML names the hashed assets of the build that wrote it. A browser that keeps an
// index.html by heuristic freshness (it carries Last-Modified) runs an older build's page after a
// deploy and asks for assets the server no longer has, so every HTML answer, the SPA fallback
// included, makes the browser revalidate. Vite's hashed output never changes under its name, so
// it may be kept for a year without asking. A missing asset's 404 must never carry that, or the
// browser would keep the failure. Every other file keeps net/http's own answer. A file the build
// emits is served as its type, so a browser shows the notices text rather than downloading it.
func TestStaticHandlerCacheControl(t *testing.T) {
	webDist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(webDist, "assets"), 0o700); err != nil {
		t.Fatalf("make assets dir: %v", err)
	}
	for name, body := range map[string]string{
		"index.html":              "<!doctype html>",
		"favicon.svg":             "<svg/>",
		"THIRD_PARTY_NOTICES.txt": "notices",
		"assets/index-abc123.js":  "console.log(1)",
		"assets/index-abc123.css": "body{}",
		"assets/agent-abc123.png": "png",
	} {
		if err := os.WriteFile(filepath.Join(webDist, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	for _, tc := range []struct {
		path         string
		status       int
		cacheControl string
		// Checked when set.
		contentType string
	}{
		{path: "/", status: http.StatusOK, cacheControl: pageCacheControl},
		{path: "/issues/CORE-1", status: http.StatusOK, cacheControl: pageCacheControl},
		{path: "/agents/broadcasts/5f0c", status: http.StatusOK, cacheControl: pageCacheControl},
		{path: "/assets/index-abc123.js", status: http.StatusOK, cacheControl: assetCacheControl},
		{path: "/assets/index-abc123.css", status: http.StatusOK, cacheControl: assetCacheControl},
		{path: "/assets/index-missing.js", status: http.StatusNotFound, cacheControl: ""},
		{path: "/favicon.svg", status: http.StatusOK, cacheControl: ""},
		{path: "/THIRD_PARTY_NOTICES.txt", status: http.StatusOK, cacheControl: "", contentType: "text/plain; charset=utf-8"},
		{path: "/assets/agent-abc123.png", status: http.StatusOK, cacheControl: assetCacheControl, contentType: "image/png"},
		{path: "/favicon.ico", status: http.StatusOK, cacheControl: ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("%s: status %d, want %d; body=%s", tc.path, response.Code, tc.status, response.Body.String())
			}
			if got := response.Header().Values("Cache-Control"); strings.Join(got, ", ") != tc.cacheControl {
				t.Fatalf("%s: Cache-Control %q, want %q", tc.path, got, tc.cacheControl)
			}
			if got := response.Header().Get("Content-Type"); tc.contentType != "" && got != tc.contentType {
				t.Fatalf("%s: Content-Type %q, want %q", tc.path, got, tc.contentType)
			}
		})
	}
}

// The servers behind one load balancer can hold different builds, and a page's file time says
// nothing about which build wrote it: a rollback serves an older file. So a page revalidates by
// its bytes. The browser's own page is answered 304, still to be revalidated next time; any other
// page it holds - another build's, or one cached with only a Last-Modified, whatever that time -
// is answered with this server's page.
func TestStaticHandlerRevalidatesPagesByContent(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist
	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		for name, values := range header {
			request.Header[name] = values
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	first := get("/", nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" || first.Header().Get("Last-Modified") != "" {
		t.Fatalf("page: status %d ETag %q Last-Modified %q, want 200 with an ETag and no Last-Modified", first.Code, etag, first.Header().Get("Last-Modified"))
	}
	if shell := get("/issues/CORE-1", nil).Header().Get("ETag"); shell != etag {
		t.Fatalf("SPA shell ETag %q, want the page's %q", shell, etag)
	}

	same := get("/issues/CORE-1", http.Header{"If-None-Match": {etag}})
	if same.Code != http.StatusNotModified || same.Header().Get("Cache-Control") != pageCacheControl {
		t.Fatalf("revalidating this build's page: status %d Cache-Control %q, want 304 and %q", same.Code, same.Header().Get("Cache-Control"), pageCacheControl)
	}

	later := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	for name, header := range map[string]http.Header{
		"another build's page":            {"If-None-Match": {`"another-build"`}, "If-Modified-Since": {later}},
		"a page cached by its time alone": {"If-Modified-Since": {later}},
	} {
		t.Run(name, func(t *testing.T) {
			response := get("/", header)
			if response.Code != http.StatusOK || response.Body.String() != "<!doctype html>" {
				t.Fatalf("%s: status %d body %q, want 200 and this server's page", name, response.Code, response.Body.String())
			}
		})
	}
}

// An unknown path under a reserved root is a JSON 404 that names itself and points at the
// route index, whether or not the dashboard is built. A missing static asset outside the
// reserved roots stays a plain not-found.
func TestUnknownReservedPathIsJSONNotFoundWithHint(t *testing.T) {
	for name, webDist := range map[string]string{"with dashboard": t.TempDir(), "without dashboard": ""} {
		t.Run(name, func(t *testing.T) {
			if webDist != "" {
				if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
					t.Fatalf("write dashboard index: %v", err)
				}
			}
			handler, context := newTestRouter(t)
			context.WebDistDir = webDist

			for _, path := range []string{"/api/v1/does-not-exist", "/v1/issues"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusNotFound {
					t.Fatalf("%s status %d: %s", path, response.Code, response.Body.String())
				}
				if got := response.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("%s content type %q", path, got)
				}
				var body map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s body %q: %v", path, response.Body.String(), err)
				}
				want := map[string]string{
					"code":  "NOT_FOUND",
					"error": "no route for GET " + path,
					"hint":  "GET /api/v1 lists every route",
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("%s body = %v, want %v", path, body, want)
				}
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.png", nil))
			if webDist == "" {
				return
			}
			if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":\"not found\"}\n" {
				t.Fatalf("missing asset: status %d body %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestStaticHandlerServesFaviconIcoAsSvg(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDist, "favicon.svg"), []byte("<svg></svg>"), 0o600); err != nil {
		t.Fatalf("write favicon: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("favicon status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/svg+xml" {
		t.Fatalf("favicon content-type: got %q, want image/svg+xml", contentType)
	}
	if response.Body.String() != "<svg></svg>" {
		t.Fatalf("favicon body: got %q, want svg source", response.Body.String())
	}
}

func TestStaticHandlerFaviconIcoFallsBackToNoContent(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	if response.Code != http.StatusNoContent {
		t.Fatalf("favicon status: got %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

// A callback carrying a state another browser started is refused before any exchange.
func TestOAuthCallbackRejectsStateFromAnotherBrowser(t *testing.T) {
	rig := newSignInRig(t, "")
	rig.issuer.SignInAs(personClaims(rigGroup))
	authURL, _ := rig.start(t, "http://dispatch.test/auth/start")
	code, state := rig.issuer.Authorize(t, authURL.String())
	callback := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, callback)
	if response.Code != http.StatusBadRequest || sessionCookie(response) != nil {
		t.Fatalf("cross-browser callback status = %d body=%s, want 400 and no session", response.Code, response.Body.String())
	}
	if _, found := rig.people.get(rigEmail); found {
		t.Fatal("cross-browser callback recorded a sign-in")
	}
}

func TestSanitizeNextRejectsBrowserNormalizedExternalPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "relative path", raw: "/issues/CORE-1", want: "/issues/CORE-1"},
		{name: "backslash authority", raw: `/\evil.test/path`, want: "/"},
		{name: "encoded backslash authority", raw: "/%5Cevil.test/path", want: "/"},
		{name: "control character", raw: "/issues/\x00", want: "/"},
		{name: "absolute URL", raw: "https://evil.test/path", want: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeNext(tc.raw); got != tc.want {
				t.Fatalf("sanitizeNext(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestAuthStartUsesConfiguredCanonicalOriginForCallback(t *testing.T) {
	rig := newSignInRig(t, "https://dispatch.example/")
	request := httptest.NewRequest(http.MethodGet, "http://backend.internal/auth/start", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse sign-in redirect: %v", err)
	}
	if callback := location.Query().Get("redirect_uri"); callback != "https://dispatch.example/auth/callback" {
		t.Fatalf("sign-in callback URL = %q, want configured origin", callback)
	}
	if client := location.Query().Get("client_id"); client != rigClientID {
		t.Fatalf("sign-in client = %q, want %q", client, rigClientID)
	}
}

// A domain-mode server sends the browser to Google asking for offline access with consent, so the
// exchange carries a refresh token every time, and with the domain as a hint; a group-mode server's
// redirect carries none of the three.
func TestAuthStartAsksGoogleForOfflineAccessInTheDomain(t *testing.T) {
	// rig.start reads the redirect's Location.
	query := func(rig *signInRig) url.Values {
		t.Helper()
		location, _ := rig.start(t, "http://dispatch.test/auth/start")
		return location.Query()
	}
	domain := query(newDomainSignInRig(t))
	for name, want := range map[string]string{"access_type": "offline", "prompt": "consent", "hd": rigDomain, "client_id": rigClientID} {
		if got := domain.Get(name); got != want {
			t.Errorf("domain mode: GET /auth/start redirected with %s=%q, want %q", name, got, want)
		}
	}
	group := query(newSignInRig(t, ""))
	for _, name := range []string{"access_type", "prompt", "hd"} {
		if group.Has(name) {
			t.Errorf("group mode: GET /auth/start redirected with %s=%q, want none", name, group.Get(name))
		}
	}
}

// cookieRouter is a router on a cookie identity with no membership to confirm, and a session
// cookie of sami@d.example's current generation.
func cookieRouter(t *testing.T, serverURL string) (http.Handler, *http.Cookie, *memoryPeopleStore) {
	t.Helper()
	people := newMemoryPeople()
	if err := people.SignIn(context.Background(), "sami@d.example", "refresh-token", time.Now()); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	sessions := &memorySessionStore{generations: map[string]int64{"sami@d.example": 0}}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:  "signing-key",
		People:      people,
		Sessions:    sessions,
		Identity:    identity.CookieIdentity{SigningKey: "signing-key", Sessions: sessions},
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   serverURL,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("sami@d.example", 0, "signing-key", true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	handler, _ := New(ctx)
	return handler, cookie, people
}

func TestLogoutRevokesCopiedSessionCookieAndForgetsTheRefreshToken(t *testing.T) {
	handler, cookie, people := cookieRouter(t, "")
	logout := httptest.NewRequest(http.MethodPost, "http://dispatch.test/auth/logout", nil)
	logout.Header.Set("Origin", "http://dispatch.test")
	logout.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("logout status = %d body=%s, want %d", logoutResponse.Code, logoutResponse.Body.String(), http.StatusOK)
	}
	if membership, _ := people.get("sami@d.example"); membership.RefreshToken != "" {
		t.Fatalf("logout kept the refresh token: %#v", membership)
	}
	replay := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	replay.AddCookie(cookie)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("copied-cookie replay status = %d body=%s, want %d", replayResponse.Code, replayResponse.Body.String(), http.StatusUnauthorized)
	}
}

func TestCookieAuthenticatedUnsafeRequestsRequireSameOrigin(t *testing.T) {

	t.Run("foreign origin is forbidden", func(t *testing.T) {
		handler, cookie, _ := cookieRouter(t, "https://dispatch.example")
		request := httptest.NewRequest(http.MethodPost, "https://dispatch.example/auth/logout", nil)
		request.AddCookie(cookie)
		request.Header.Set("Origin", "https://other.dispatch.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("foreign-origin logout status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusForbidden)
		}
	})

	t.Run("same origin is permitted", func(t *testing.T) {
		handler, cookie, _ := cookieRouter(t, "https://dispatch.example")
		request := httptest.NewRequest(http.MethodPost, "https://dispatch.example/auth/logout", nil)
		request.AddCookie(cookie)
		request.Header.Set("Origin", "https://dispatch.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("same-origin logout status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusOK)
		}
	})

	t.Run("bearer caller without origin reaches its handler", func(t *testing.T) {
		handler, _, _ := cookieRouter(t, "https://dispatch.example")
		request := httptest.NewRequest(http.MethodPost, "https://dispatch.example/api/v1/issues", strings.NewReader(`{"actor":{"kind":"session","id":"worker"}}`))
		request.Header.Set("Authorization", "Bearer agent-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusForbidden {
			t.Fatalf("bearer request without origin was rejected as CSRF: body=%s", response.Body.String())
		}
	})
}

func (s *memoryPeopleStore) EndSubject(ctx context.Context, email, subject string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	membership, found := s.people[email]
	if !found || membership.Subject != subject {
		return false, nil
	}
	if s.sessions == nil {
		return false, errors.New("memoryPeopleStore: EndSubject has no sessions to end")
	}
	if err := s.sessions.RevokeSessions(ctx, email); err != nil {
		return false, err
	}
	s.people[email] = auth.PersonMembership{Subject: membership.Subject}
	return true, nil
}
