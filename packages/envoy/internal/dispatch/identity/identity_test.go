package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

type testSessionStore struct {
	mu                     sync.Mutex
	generations            map[string]int64
	currentGenerationCalls int
	ensureSessionCalls     int
}

func (s *testSessionStore) CurrentSessionGeneration(_ context.Context, login string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentGenerationCalls++
	generation, found := s.generations[login]
	return generation, found, nil
}

func (s *testSessionStore) EnsureSession(_ context.Context, login string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSessionCalls++
	generation, found := s.generations[login]
	if !found {
		s.generations[login] = 0
	}
	return generation, nil
}

func (s *testSessionStore) RevokeSessions(_ context.Context, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations[login]++
	return nil
}

func (s *testSessionStore) generation(login string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generations[login]
}

// testPeopleStore is auth.PeopleStore in memory. A sign-in under another subject than the one it
// holds for the address ends the address's sessions in sessions, as store.PgPeopleStore does. now
// stands in for the database's clock: it dates each sign-in and confirmation, and Membership
// answers a confirmation's age by it; nil is time.Now.
type testPeopleStore struct {
	mu       sync.Mutex
	people   map[string]testPerson
	sessions *testSessionStore
	now      func() time.Time
}

// testPerson is one person's row in testPeopleStore: ConfirmedAt is the store clock's time of
// the last confirmation, zero for none.
type testPerson struct {
	RefreshToken string
	Subject      string
	ConfirmedAt  time.Time
}

func newTestPeopleStore(sessions *testSessionStore) *testPeopleStore {
	return &testPeopleStore{people: map[string]testPerson{}, sessions: sessions}
}

func (s *testPeopleStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *testPeopleStore) Record(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.people[email]; !found {
		s.people[email] = testPerson{}
	}
	return nil
}

func (s *testPeopleStore) SignIn(_ context.Context, email, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person := s.people[email]
	person.RefreshToken, person.ConfirmedAt = refreshToken, s.clock()
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, existed := s.people[email]
	if existed && person.Subject != subject {
		if err := s.sessions.RevokeSessions(ctx, email); err != nil {
			return err
		}
	}
	person.Subject = subject
	person.RefreshToken, person.ConfirmedAt = refreshToken, s.clock()
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) Membership(_ context.Context, email string) (auth.PersonMembership, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	membership := auth.PersonMembership{RefreshToken: person.RefreshToken, Subject: person.Subject}
	if !person.ConfirmedAt.IsZero() {
		membership.Confirmed, membership.ConfirmedAge = true, s.clock().Sub(person.ConfirmedAt)
	}
	return membership, found, nil
}

func (s *testPeopleStore) Confirm(_ context.Context, email, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person := s.people[email]
	person.RefreshToken, person.ConfirmedAt = refreshToken, s.clock()
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) ConfirmSubject(_ context.Context, email, subject, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person := s.people[email]
	if person.Subject != subject {
		return nil
	}
	person.RefreshToken, person.ConfirmedAt = refreshToken, s.clock()
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) End(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if person, found := s.people[email]; found {
		s.people[email] = testPerson{Subject: person.Subject}
	}
	return nil
}

func (s *testPeopleStore) get(email string) (testPerson, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	return person, found
}

// testSigningKey is the one-key set the package's session cookies are signed with.
var testSigningKey = auth.SingleSigningKey("signing-key")

func cookieRequest(t *testing.T, email string, generation int64) *http.Request {
	t.Helper()
	cookie := strings.SplitN(auth.IssueSessionCookie(email, generation, testSigningKey, true), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: name, Value: value})
	return request
}

// A test or local harness header names a person by email; Dispatch's form of it is lowercased,
// and the person is recorded as having signed in, which is what makes them assignable.
func TestHeaderIdentityNamesThePersonLowercasedAndRecordsThem(t *testing.T) {
	people := newTestPeopleStore(nil)
	identity := HeaderIdentity{Header: "X-Dispatch-User", People: people}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dispatch-User", " Sami@Example.com ")
	email, err := identity.Login(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if email != "sami@example.com" {
		t.Errorf("login: got %q, want sami@example.com", email)
	}
	if _, found := people.get("sami@example.com"); !found {
		t.Errorf("the person the header named was not recorded: %#v", people.people)
	}
}

func TestHeaderIdentityRejectsAMissingHeader(t *testing.T) {
	people := newTestPeopleStore(nil)
	identity := HeaderIdentity{Header: "X-Dispatch-User", People: people}
	for _, value := range []string{"", "   "} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Dispatch-User", value)
		if _, err := identity.Login(req); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("header %q error: got %v, want ErrNoIdentity", value, err)
		}
	}
	if len(people.people) != 0 {
		t.Errorf("an unnamed request recorded people: %#v", people.people)
	}
}

func TestCookieIdentityNamesThePersonOfAValidCookie(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{"a.b+c@d.example": 0}}
	identity := CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}
	if _, err := identity.Login(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("missing cookie error: got %v, want ErrNoIdentity", err)
	}

	request := cookieRequest(t, "a.b+c@d.example", 0)
	email, err := identity.Login(request)
	if err != nil {
		t.Fatalf("valid cookie: %v", err)
	}
	if email != "a.b+c@d.example" {
		t.Errorf("login: got %q, want a.b+c@d.example", email)
	}

	if _, err := (CookieIdentity{SigningKey: testSigningKey}).Login(request); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("cookie identity without session store error: got %v, want ErrNoIdentity", err)
	}
	if err := sessions.RevokeSessions(context.Background(), "a.b+c@d.example"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := identity.Login(request); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("cookie of a revoked generation error: got %v, want ErrNoIdentity", err)
	}
}

func TestCookieIdentityReadsGenerationWithoutWriting(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{"sami@example.com": 0}}
	email, err := (CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}).Login(cookieRequest(t, "sami@example.com", 0))
	if err != nil || email != "sami@example.com" {
		t.Fatalf("cookie login = %q, %v; want sami@example.com, nil", email, err)
	}
	if sessions.ensureSessionCalls != 0 {
		t.Fatalf("cookie identity created the session row %d time(s)", sessions.ensureSessionCalls)
	}
	if sessions.currentGenerationCalls != 1 {
		t.Fatalf("cookie identity generation reads = %d, want 1", sessions.currentGenerationCalls)
	}
}

func TestCookieIdentityRejectsCookieWithoutSessionRow(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{}}
	if _, err := (CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}).Login(cookieRequest(t, "sami@example.com", 0)); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("cookie without session row error = %v, want ErrNoIdentity", err)
	}
}

func TestWriteErrorMapsNoIdentityToUnauthorized(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, ErrNoIdentity)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Errorf("no identity: status %d body %s, want 401 NO_IDENTITY", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteError(w, fmt.Errorf("%w: %w", ErrSignInUnavailable, fmt.Errorf("oidc: refresh: %w: oauth2: 503", oidc.ErrIssuerUnavailable)))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"SIGNIN_UNAVAILABLE"`) || !strings.Contains(w.Body.String(), "try again in a minute") {
		t.Errorf("sign-in issuer unreachable: status %d body %s, want 503 SIGNIN_UNAVAILABLE", w.Code, w.Body.String())
	}
	// An issuer outage that is not the sign-in's (a service-account token's issuer) is not
	// SIGNIN_UNAVAILABLE: only ErrSignInUnavailable answers that, whatever else the error carries.
	w = httptest.NewRecorder()
	WriteError(w, fmt.Errorf("verify: %w", oidc.ErrIssuerUnavailable))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "SIGNIN_UNAVAILABLE") {
		t.Errorf("another issuer unreachable: status %d body %s, want 500 and not SIGNIN_UNAVAILABLE", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteError(w, errors.New("database down"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("other error: status %d, want 500", w.Code)
	}
}

func (s *testPeopleStore) EndSubject(ctx context.Context, email, subject string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	if !found || person.Subject != subject {
		return false, nil
	}
	if err := s.sessions.RevokeSessions(ctx, email); err != nil {
		return false, err
	}
	s.people[email] = testPerson{Subject: person.Subject}
	return true, nil
}
