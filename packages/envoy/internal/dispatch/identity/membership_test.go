package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	memberEmail    = "a.b+c@d.example"
	memberGroup    = "platform-managers"
	memberRedirect = "https://dispatch.example/auth/callback"
)

func memberClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "person-subject",
		"cognito:username": "GoogleWorkspace_" + memberEmail,
		"cognito:groups":   groups,
		"email":            "not-the-person@d.example",
		"identities":       []map[string]any{{"providerName": "GoogleWorkspace"}},
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
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	flow, err := oidc.NewCodeFlow(context.Background(), issuer.URL(), "dispatch-client", "dispatch-secret")
	if err != nil {
		t.Fatalf("NewCodeFlow: %v", err)
	}
	issuer.SignInAs(memberClaims(memberGroup))
	code, _ := issuer.Authorize(t, flow.AuthURL(memberRedirect, "state", "nonce"))
	session, err := flow.Exchange(context.Background(), memberRedirect, code, "nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	rig := &membershipRig{
		issuer:   issuer,
		people:   newTestPeopleStore(),
		sessions: &testSessionStore{generations: map[string]int64{memberEmail: 0}},
		now:      time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
	if err := rig.people.SignIn(context.Background(), memberEmail, session.RefreshToken, rig.now); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	rig.membership = &Membership{
		People:   rig.people,
		Sessions: rig.sessions,
		SignIn:   flow,
		Group:    memberGroup,
		Now:      func() time.Time { return rig.now },
	}
	return rig
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
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"hawk-users"} })
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm after leaving the group: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A refreshed ID token naming another person is not this person's membership, whatever its groups.
func TestMembershipEndsTheSessionWhenTheRefreshNamesSomeoneElse(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:username"] = "GoogleWorkspace_other@d.example" })
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
