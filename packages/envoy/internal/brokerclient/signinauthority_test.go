// packages/envoy/internal/brokerclient/signinauthority_test.go
//
// Which launcher credential a machine's sessions enrol under, and when the answer changes.
//
// These lock `agent-secrets-helper`'s rule — a session enrols under the person signed in on the
// box as the helper's own user, and under the machine otherwise — in the package whose tests
// run anywhere, because the helper's own suite needs a unix socket whose peer the kernel can
// pin (SO_PEERPIDFD, Linux 6.5), which a pod under gVisor has not got. The helper's side of it
// (the enrolment body, and the retry when the broker refuses a person's credential) is locked
// in internal/broker/helper, for CI to run at the head.

package brokerclient

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// signInRig is a box: an issuer a person signs in at, a broker that mints their launcher
// credential, and the store this machine keeps the sign-in in.
type signInRig struct {
	issuer    *oidctest.Issuer
	flow      *DeviceFlow
	broker    *stubBroker
	store     SignInStore
	authority *SignInAuthority
	signIns   int
}

// newSignInRig is that box, with the broker answering credential ids in turn
// (credential-1, credential-2, …) unless answer says otherwise.
func newSignInRig(t *testing.T, answer func(n int, form url.Values) (int, map[string]any)) *signInRig {
	t.Helper()
	const clientID, clientSecret = "example-device-client", "example-device-secret"
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret}
	if answer == nil {
		answer = func(n int, _ url.Values) (int, map[string]any) { return 200, mintedToken(n + 1) }
	}
	broker := newStubBroker(t, answer)
	store := storeFor(t)
	return &signInRig{
		issuer: issuer, flow: flow, broker: broker, store: store,
		authority: &SignInAuthority{
			Store:     store,
			Machine:   &scriptedSource{token: "this-machine's-identity-token", tokenType: oauthwire.SubjectTokenTypeGoogle, describe: "this machine"},
			Flow:      flow,
			BrokerURL: broker.URL(),
		},
	}
}

// mintedToken is the broker's answer to a trade: an access token bound to the caller's key and
// the launcher credential that trade minted.
func mintedToken(n int) map[string]any {
	return map[string]any{
		"access_token": fmt.Sprintf("access-token-%d", n), "token_type": "DPoP", "expires_in": 3600,
		"credential_id": fmt.Sprintf("credential-%d", n), "credential_expires_at": "2099-01-01T00:00:00Z",
	}
}

// signIn is a person signing in on this box as `agent-secrets login` does: the device grant at
// the rig's issuer, and the record it leaves, with a box key and a thumbprint of its own —
// signing in again makes a key of its own, which is what makes it another sign-in.
func (r *signInRig) signIn(t *testing.T, person string) SignInRecord {
	t.Helper()
	r.issuer.SignInByDefault(map[string]any{"sub": person, "email": person, "email_verified": true})
	authorization, err := r.flow.Start(context.Background())
	if err != nil {
		t.Fatalf("start %s's sign-in: %v", person, err)
	}
	if err := r.issuer.ApproveDeviceCode(authorization.UserCode); err != nil {
		t.Fatalf("%s types the code in: %v", person, err)
	}
	authorization.Interval = 1
	granted, err := r.flow.Wait(context.Background(), authorization)
	if err != nil {
		t.Fatalf("%s's sign-in: %v", person, err)
	}
	r.signIns++
	record := signedIn(t, person)
	record.Issuer, record.ClientID = r.flow.Issuer, r.flow.ClientID
	record.RefreshToken = granted.RefreshToken
	record.Thumbprint = fmt.Sprintf("thumbprint-%d-of-%s", r.signIns, person)
	if err := r.store.Write(record); err != nil {
		t.Fatalf("keep %s's sign-in: %v", person, err)
	}
	return record
}

// TestNobodySignedInLeavesTheMachinesOwnAuthority: with no sign-in on the box, the authority
// answers nothing at all — no error and no trade — and the caller enrols under its own machine
// credential.
func TestNobodySignedInLeavesTheMachinesOwnAuthority(t *testing.T) {
	rig := newSignInRig(t, nil)
	authority, err := rig.authority.Current(context.Background())
	if authority != nil || err != nil {
		t.Fatalf("with nobody signed in the authority answered %+v, %v; want nil and no error", authority, err)
	}
	if n := len(rig.broker.Requests()); n != 0 {
		t.Fatalf("with nobody signed in the authority made %d trades; want none", n)
	}
}

// TestASignInMadeWhileTheProcessRunsIsTakenAtTheNextEnrolment: the box's state decides, not the
// moment the process started, so the helper needs no restart after `agent-secrets login`. The
// credential is the person's, and the trade carried their identity token with this machine's as
// the actor.
func TestASignInMadeWhileTheProcessRunsIsTakenAtTheNextEnrolment(t *testing.T) {
	const person = "ada@example.com"
	rig := newSignInRig(t, nil)
	if authority, err := rig.authority.Current(context.Background()); authority != nil || err != nil {
		t.Fatalf("before the sign-in: %+v, %v; want nil and no error", authority, err)
	}
	record := rig.signIn(t, person)

	authority, err := rig.authority.Current(context.Background())
	if err != nil {
		t.Fatalf("after the sign-in: %v", err)
	}
	if authority == nil {
		t.Fatal("a sign-in made while the process runs was not taken: the authority still answers nothing")
	}
	if authority.Operator != person {
		t.Errorf("the authority acts for %q, want %q", authority.Operator, person)
	}
	if authority.CredentialID != "credential-1" {
		t.Errorf("the authority names credential %q, want the one the trade minted", authority.CredentialID)
	}
	boxKey, err := record.Key()
	if err != nil {
		t.Fatal(err)
	}
	if !authority.Key.Equal(boxKey) {
		t.Error("the authority signs with a key that is not the sign-in's box key, which is the key the broker bound the credential to")
	}
	trades := rig.broker.Requests()
	if len(trades) != 1 {
		t.Fatalf("the authority made %d trades, want one", len(trades))
	}
	form := trades[0].Form
	if got := form.Get("subject_token_type"); got != oauthwire.SubjectTokenTypePerson {
		t.Errorf("the trade's subject is %q, want the person's own identity token", got)
	}
	if got := form.Get("actor_token_type"); got != oauthwire.ActorTokenTypeMachine {
		t.Errorf("the trade's actor is %q, want this machine's identity token", got)
	}
	// The exchange fixes no session for a sign-in and refuses a name belonging to another
	// identity, so a person's trade asks under a name of their own rather than the one this
	// machine trades its own identity under.
	session := form.Get("session")
	if !strings.Contains(session, person) || !strings.Contains(session, record.Thumbprint[:12]) {
		t.Errorf("the trade asked under the session %q, want one naming the person and their box key", session)
	}
}

// TestTheTokenInHandServesEveryEnrolmentUntilItLapses: asking at every enrolment costs a read of
// the store, never a trade of its own, so a box registering sessions all day signs one person in
// once.
func TestTheTokenInHandServesEveryEnrolmentUntilItLapses(t *testing.T) {
	rig := newSignInRig(t, nil)
	rig.signIn(t, "ada@example.com")
	for i := range 3 {
		if _, err := rig.authority.Current(context.Background()); err != nil {
			t.Fatalf("enrolment %d: %v", i, err)
		}
	}
	if n := len(rig.broker.Requests()); n != 1 {
		t.Fatalf("three enrolments made %d trades; want one, the token in hand serving the rest", n)
	}
}

// TestARevokedSignInFallsBackToTheMachine: the broker refuses the trade once the person has
// revoked the sign-in, which is a definite answer — that sign-in is over — so the authority
// answers nothing, the caller enrols under the machine, and nothing asks again until somebody
// signs in afresh.
func TestARevokedSignInFallsBackToTheMachine(t *testing.T) {
	rig := newSignInRig(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "this sign-in was revoked"}
	})
	rig.signIn(t, "ada@example.com")
	for i := range 3 {
		authority, err := rig.authority.Current(context.Background())
		if authority != nil || err != nil {
			t.Fatalf("enrolment %d under a revoked sign-in answered %+v, %v; want nil and no error, so the session enrols under this machine", i, authority, err)
		}
	}
	if n := len(rig.broker.Requests()); n != 1 {
		t.Fatalf("a revoked sign-in was traded %d times; want once, the refusal remembered", n)
	}
}

// TestSigningInAgainAfterARevokeIsANewSignIn: the refusal is remembered against the record it
// refused, and `agent-secrets login` writes a record with a box key of its own, so the box works
// again with no restart.
func TestSigningInAgainAfterARevokeIsANewSignIn(t *testing.T) {
	const person = "ada@example.com"
	refuse := true
	rig := newSignInRig(t, func(n int, _ url.Values) (int, map[string]any) {
		if refuse {
			return 400, map[string]any{"error": "invalid_grant", "error_description": "this sign-in was revoked"}
		}
		return 200, mintedToken(n + 1)
	})
	rig.signIn(t, person)
	if authority, _ := rig.authority.Current(context.Background()); authority != nil {
		t.Fatalf("a revoked sign-in answered %+v; want nil", authority)
	}

	refuse = false
	rig.signIn(t, person)
	authority, err := rig.authority.Current(context.Background())
	if err != nil {
		t.Fatalf("after signing in again: %v", err)
	}
	if authority == nil || authority.Operator != person {
		t.Fatalf("after signing in again the authority answered %+v; want %s's credential", authority, person)
	}
}

// TestARefusedCredentialIsTradedAgain: a launcher proof the broker rejects (the credential
// lapsed, or the sign-in was revoked with it) tells the authority, and the next call trades
// rather than presenting the same credential again — which is how one enrolment settles in one
// call instead of failing for the caller to retry.
func TestARefusedCredentialIsTradedAgain(t *testing.T) {
	rig := newSignInRig(t, nil)
	rig.signIn(t, "ada@example.com")
	first, err := rig.authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rig.authority.Refused(first.CredentialID)
	second, err := rig.authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.CredentialID == first.CredentialID {
		t.Fatalf("after the broker refused %s the authority answered %+v; want a credential a fresh trade minted", first.CredentialID, second)
	}
	if n := len(rig.broker.Requests()); n != 2 {
		t.Fatalf("%d trades; want two: the first and the one the refusal asked for", n)
	}

	// A credential the authority no longer holds says nothing: the trade above replaced it.
	rig.authority.Refused(first.CredentialID)
	if _, err := rig.authority.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(rig.broker.Requests()); n != 2 {
		t.Fatalf("%d trades; want two: a stale credential id must throw nothing away", n)
	}
}

// TestABrokerThatDoesNotAnswerIsNoAnswerAboutWhoIsSignedIn: an undecided trade is an error, never
// a quiet fall back to the machine, so a session is never given less authority than its person
// has because one call timed out. (The enrolment it would have signed goes to the same broker,
// so nothing is lost by failing here.)
func TestABrokerThatDoesNotAnswerIsNoAnswerAboutWhoIsSignedIn(t *testing.T) {
	rig := newSignInRig(t, func(int, url.Values) (int, map[string]any) {
		return 503, map[string]any{"error": "unavailable"}
	})
	rig.signIn(t, "ada@example.com")
	authority, err := rig.authority.Current(context.Background())
	if authority != nil {
		t.Fatalf("a broker that did not answer gave the authority %+v; want none", authority)
	}
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") {
		t.Fatalf("a broker that did not answer must be an error naming whose sign-in it was: %v", err)
	}
}

// TestTwoPeopleOnOneBoxAreRefusedUntilToldWhich: the rule that stops a session acting as the
// wrong colleague. The box has two sign-ins of this user and nothing says which, so the
// authority refuses by name rather than picking one or quietly dropping to the machine, and
// AGENT_SECRETS_PERSON settles it.
func TestTwoPeopleOnOneBoxAreRefusedUntilToldWhich(t *testing.T) {
	rig := newSignInRig(t, nil)
	rig.signIn(t, "ada@example.com")
	rig.signIn(t, "grace@example.com")

	authority, err := rig.authority.Current(context.Background())
	if authority != nil {
		t.Fatalf("with two people signed in the authority chose %+v; want a refusal", authority)
	}
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "grace@example.com") ||
		!strings.Contains(err.Error(), PersonVar) {
		t.Fatalf("the refusal must name both people and the variable that settles it: %v", err)
	}

	rig.authority.Person = "grace@example.com"
	authority, err = rig.authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authority == nil || authority.Operator != "grace@example.com" {
		t.Fatalf("%s named grace@example.com and the authority answered %+v", PersonVar, authority)
	}
}

// TestOneUsersSignInIsNeverAnothersAuthority: the store an authority reads is the one its own
// process was given — this user's key directory — so a sign-in in another user's directory, who
// has a helper of their own, is no authority of this one's.
func TestOneUsersSignInIsNeverAnothersAuthority(t *testing.T) {
	ada := newSignInRig(t, nil)
	ada.signIn(t, "ada@example.com")

	grace := newSignInRig(t, nil)
	grace.signIn(t, "grace@example.com")

	authority, err := ada.authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authority == nil || authority.Operator != "ada@example.com" {
		t.Fatalf("this user's authority is %+v; want ada@example.com's own sign-in", authority)
	}
	if ada.store.Dir == grace.store.Dir {
		t.Fatal("the two users' stores must be two directories for this test to mean anything")
	}
	records, err := ada.store.List()
	if err != nil || len(records) != 1 || records[0].Person != "ada@example.com" {
		t.Fatalf("this user's store holds %+v (%v); want ada@example.com's record alone", records, err)
	}
}

// TestAnAuthorityWithNoMachineIdentityTradesNothing: a person's trade presents the machine as
// its actor, so a box that proves no identity of its own can trade no sign-in at all. The
// helper builds no authority there; this is the guard against one built anyway.
func TestAnAuthorityWithNoMachineIdentityTradesNothing(t *testing.T) {
	rig := newSignInRig(t, nil)
	rig.signIn(t, "ada@example.com")
	rig.authority.Machine = nil
	if _, err := rig.authority.Current(context.Background()); err == nil {
		t.Fatal("an authority with no machine identity must refuse rather than trade")
	}
}
