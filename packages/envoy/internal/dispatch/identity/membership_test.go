package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	memberEmail    = "a.b+c@d.example"
	memberGroup    = "dispatch-members"
	memberRedirect = "https://dispatch.example/auth/callback"
)

func memberClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "person-subject",
		"cognito:username": "ExampleIdP_" + memberEmail,
		"cognito:groups":   groups,
		"email":            "not-the-person@d.example",
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	}
}

type membershipRig struct {
	issuer     *oidctest.Issuer
	people     *testPeopleStore
	sessions   *testSessionStore
	membership *Membership
	now        time.Time
}

// newMembershipRig signs memberEmail in through a fake pool at rig.now, as the sign-in callback
// does, and returns the membership that renews it.
func newMembershipRig(t *testing.T) *membershipRig {
	t.Helper()
	return newMembershipRigFor(t, GroupRule{Group: memberGroup}, "", memberClaims(memberGroup), "")
}

// memberDomain is the Workspace domain of memberEmail, the domain rule's.
const memberDomain = "d.example"

// googleMemberClaims is a Google ID token's claims for memberEmail, a verified account of
// memberDomain.
func googleMemberClaims() map[string]any {
	return map[string]any{
		"sub":            "google-subject",
		"email":          "A.B+C@d.example",
		"email_verified": true,
		"hd":             memberDomain,
	}
}

// newDomainMembershipRig signs memberEmail in with Google for memberDomain at rig.now, as the
// sign-in callback does, recording the account's subject, and returns the membership that renews
// it.
func newDomainMembershipRig(t *testing.T) *membershipRig {
	t.Helper()
	return newMembershipRigFor(t, DomainRule{Domain: memberDomain}, memberDomain, googleMemberClaims(), "google-subject")
}

func newMembershipRigFor(t *testing.T, rule SignInRule, hostedDomain string, claims map[string]any, subject string) *membershipRig {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	flow, err := oidc.NewCodeFlow(context.Background(), issuer.URL(), "dispatch-client", "dispatch-secret", hostedDomain)
	if err != nil {
		t.Fatalf("NewCodeFlow: %v", err)
	}
	issuer.SignInAs(claims)
	code, _ := issuer.Authorize(t, flow.AuthURL(memberRedirect, "state", "nonce"))
	session, err := flow.Exchange(context.Background(), memberRedirect, code, "nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	sessions := &testSessionStore{generations: map[string]int64{memberEmail: 0}}
	rig := &membershipRig{
		issuer:   issuer,
		people:   newTestPeopleStore(sessions),
		sessions: sessions,
		now:      time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
	if err := rule.WriteSignIn(context.Background(), rig.people, memberEmail, subject, session.RefreshToken, rig.now); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	rig.membership = &Membership{
		People:   rig.people,
		Sessions: rig.sessions,
		SignIn:   flow,
		Rule:     rule,
		Now:      func() time.Time { return rig.now },
	}
	return rig
}

// Under the domain rule the hourly confirmation refreshes with Google and keeps the sessions of a
// verified account of the domain under the subject that signed in.
func TestDomainMembershipKeepsAMemberOfTheDomain(t *testing.T) {
	rig := newDomainMembershipRig(t)
	rig.now = rig.now.Add(time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm after an hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes after an hour = %d, want 1", refreshes)
	}
	if membership, _ := rig.people.get(memberEmail); !membership.ConfirmedAt.Equal(rig.now) || membership.Subject != "google-subject" {
		t.Fatalf("membership after the refresh = %#v, want confirmed at %s under google-subject", membership, rig.now)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("a successful refresh moved the session generation to %d", generation)
	}
}

// The domain rule runs again on the refreshed token, and the refreshed account must be the one
// that signed in: an account that left the domain, lost its verified email, changed its email or
// is another account (another subject), or a refresh Google refuses, ends every session.
func TestDomainMembershipEndsTheSessionWhenTheRefreshedAccountNoLongerQualifies(t *testing.T) {
	for name, change := range map[string]func(claims map[string]any){
		"another domain":         func(claims map[string]any) { claims["hd"] = "other.example" },
		"no domain":              func(claims map[string]any) { delete(claims, "hd") },
		"an unverified email":    func(claims map[string]any) { claims["email_verified"] = "false" },
		"another email":          func(claims map[string]any) { claims["email"] = "someone-else@d.example" },
		"another Google account": func(claims map[string]any) { claims["sub"] = "another-google-subject" },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDomainMembershipRig(t)
			rig.issuer.UpdateGrants(change)
			rig.now = rig.now.Add(2 * time.Hour)
			if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
				t.Fatalf("Confirm: err = %v, want ErrNoIdentity", err)
			}
			assertSessionEnded(t, rig)
		})
	}
	t.Run("a refresh Google refuses", func(t *testing.T) {
		rig := newDomainMembershipRig(t)
		rig.issuer.RevokeGrants()
		rig.now = rig.now.Add(2 * time.Hour)
		if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("Confirm: err = %v, want ErrNoIdentity", err)
		}
		assertSessionEnded(t, rig)
	})
}

// A row whose refresh token no domain sign-in recorded holds no subject to compare: the domain rule
// ends its sessions at the confirmation without asking Google, rather than refreshing with a token
// whose account it cannot check.
func TestDomainMembershipEndsTheSessionOfARowWithNoSubject(t *testing.T) {
	rig := newDomainMembershipRig(t)
	membership, _ := rig.people.get(memberEmail)
	if err := rig.people.End(context.Background(), memberEmail); err != nil {
		t.Fatal(err)
	}
	rig.people.mu.Lock()
	rig.people.people[memberEmail] = auth.PersonMembership{RefreshToken: membership.RefreshToken, ConfirmedAt: membership.ConfirmedAt}
	rig.people.mu.Unlock()
	rig.now = rig.now.Add(2 * time.Hour)
	refreshes := rig.issuer.Refreshes()
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm of a row with no subject: err = %v, want ErrNoIdentity", err)
	}
	if got := rig.issuer.Refreshes(); got != refreshes {
		t.Fatalf("Confirm of a row with no subject refreshed at the issuer (%d refreshes, want %d)", got, refreshes)
	}
	assertSessionEnded(t, rig)
}

func TestMembershipConfirmedWithinTheHourAsksTheIssuerNothing(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(59 * time.Minute)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm within the hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 0 {
		t.Fatalf("refreshes within the hour = %d, want 0", refreshes)
	}
}

// Membership is renewed at least hourly: once the last confirmation is an hour old, the next
// request refreshes with the stored refresh token and records the new confirmation.
func TestMembershipRefreshesOnceTheConfirmationIsAnHourOld(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm after an hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes after an hour = %d, want 1", refreshes)
	}
	membership, _ := rig.people.get(memberEmail)
	if !membership.ConfirmedAt.Equal(rig.now) || membership.RefreshToken == "" {
		t.Fatalf("membership after the refresh = %#v, want confirmed at %s with its refresh token", membership, rig.now)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("a successful refresh moved the session generation to %d", generation)
	}
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil || rig.issuer.Refreshes() != 1 {
		t.Fatalf("Confirm right after the refresh: err=%v refreshes=%d, want nil and still 1", err, rig.issuer.Refreshes())
	}
}

func TestMembershipEndsTheSessionWhenTheRefreshFails(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.RevokeGrants()
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm with a revoked refresh token: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

func TestMembershipEndsTheSessionWhenThePersonLeavesTheGroup(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm after leaving the group: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A refreshed ID token naming another person is not this person's membership, whatever its groups.
func TestMembershipEndsTheSessionWhenTheRefreshNamesSomeoneElse(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:username"] = "ExampleIdP_other@d.example" })
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm with a refresh naming someone else: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A person Dispatch knows but holds no refresh token for (recorded by the people migration, or
// whose session already ended) has no membership to confirm.
func TestMembershipWithoutARefreshTokenIsNoIdentity(t *testing.T) {
	rig := newMembershipRig(t)
	if err := rig.people.Record(context.Background(), "migrated@d.example"); err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, email := range []string{"migrated@d.example", "never-seen@d.example"} {
		if err := rig.membership.Confirm(context.Background(), email); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("Confirm %s: err = %v, want ErrNoIdentity", email, err)
		}
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 0 {
		t.Fatalf("refreshes with no refresh token = %d, want 0", refreshes)
	}
}

// The refresh belongs to the person, not to the request that noticed it was due: a browser that
// hangs up mid-refresh neither cancels it nor ends the session.
func TestMembershipRefreshOutlivesTheRequestThatStartedIt(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rig.membership.Confirm(ended, memberEmail); err != nil {
		t.Fatalf("Confirm on an ended request: %v", err)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("an ended request ended the session (generation %d)", generation)
	}
}

// A page load sends many requests at once; one refresh answers them all, so a pool that rotates
// refresh tokens is never handed the same one twice.
func TestMembershipRefreshesOnceForConcurrentRequests(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- rig.membership.Confirm(context.Background(), memberEmail) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Confirm: %v", err)
		}
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes for 16 concurrent requests = %d, want 1", refreshes)
	}
}

func assertSessionEnded(t *testing.T, rig *membershipRig) {
	t.Helper()
	if generation := rig.sessions.generation(memberEmail); generation != 1 {
		t.Fatalf("session generation after the membership ended = %d, want 1 (every cookie revoked)", generation)
	}
	membership, found := rig.people.get(memberEmail)
	if !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
		t.Fatalf("person after the membership ended = %#v (found %t), want kept with no refresh token or confirmation", membership, found)
	}
	refreshes := rig.issuer.Refreshes()
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm after the membership ended: err = %v, want ErrNoIdentity", err)
	}
	if rig.issuer.Refreshes() != refreshes {
		t.Fatal("an ended membership asked the issuer again")
	}
}

// refresherFunc is a Refresher a test writes inline.
type refresherFunc func(ctx context.Context, refreshToken string) (oidc.Session, error)

func (f refresherFunc) Refresh(ctx context.Context, refreshToken string) (oidc.Session, error) {
	return f(ctx, refreshToken)
}

// The old account's refresh is in flight when the address's new account signs in and is given its
// cookie, and then the refresh fails: the failure belongs to the old account, so it ends nothing of
// the new one's. The new account's cookie still signs in, and its row keeps its refresh token.
func TestDomainMembershipWhoseRefreshFailsAfterAnotherAccountSignedInLeavesThatAccount(t *testing.T) {
	rig := newDomainMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	var newGeneration int64
	rig.membership.SignIn = refresherFunc(func(ctx context.Context, _ string) (oidc.Session, error) {
		if err := rig.people.SignInSubject(ctx, memberEmail, "google-subject-B", "refresh-B", rig.now); err != nil {
			t.Errorf("the new account's sign-in: %v", err)
		}
		generation, err := rig.sessions.EnsureSession(ctx, memberEmail)
		if err != nil {
			t.Errorf("the new account's session: %v", err)
		}
		newGeneration = generation
		return oidc.Session{}, errors.New(`oauth2: "invalid_grant"`)
	})
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("the old account's failed confirmation: err = %v, want ErrNoIdentity", err)
	}
	login := CookieIdentity{SigningKey: "signing-key", Sessions: rig.sessions, Membership: rig.membership}
	if email, err := login.Login(cookieRequest(t, memberEmail, newGeneration)); err != nil || email != memberEmail {
		t.Fatalf("the new account's cookie: login %q, %v; want %s still signed in", email, err, memberEmail)
	}
	if membership, _ := rig.people.get(memberEmail); membership.Subject != "google-subject-B" || membership.RefreshToken != "refresh-B" {
		t.Fatalf("the row after the old account's failure: %#v, want google-subject-B with refresh-B", membership)
	}
}
