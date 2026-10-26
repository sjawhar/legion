package oauthstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The principals a session name is claimed for here, spelled as the exchange spells them: a listed
// service account, a listed workflow and a person, the three kinds the exchange decides, and a
// second account standing for any other caller.
const (
	accountPrincipal  = "account:system:serviceaccount:example-ns:example-worker"
	daemonPrincipal   = "account:system:serviceaccount:example-ns:example-daemon"
	otherPrincipal    = "account:system:serviceaccount:other-ns:example-worker"
	workflowPrincipal = "workflow:example-org/example-repo/.github/workflows/release.yaml@refs/heads/main"
	personPrincipal   = "person:ada@example.com"
)

// exchangeRequest is an exchanged token's request: the exchange client, an ExchangeSession
// carrying the claims an exchange decided and the tokens it decided from, and no launcher
// credential, because an exchanged token follows no machine login.
func exchangeRequest(t *testing.T, s *oauthstore.Store, requestID string) *oauth2.Request {
	t.Helper()
	client, err := s.GetClient(context.Background(), oauthwire.ExchangeClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	session := oauthstore.NewExchangeSession()
	session.Subject = "ada@example.com"
	session.Username = "ada@example.com"
	session.JWKThumbprint = "thumbprint-of-the-callers-key"
	session.Extra["session"] = "implementer-g1"
	session.Extra["person"] = "ada@example.com"
	session.Extra["project"] = "ACME"
	session.SetExpiresAt(oauth2.AccessToken, time.Now().Add(time.Hour))
	session.SetSubjectToken(map[string]any{"sub": "system:serviceaccount:example-ns:example-worker"})
	session.SetActorToken(map[string]any{"sub": "example-actor"})
	request := oauth2.NewRequest()
	request.SetID(requestID)
	request.Client = client
	request.GrantScope(oauthwire.Scope)
	request.SetSession(session)
	return request
}

// TestASessionNameIsHeldByThePrincipalThatClaimedIt: the first principal to claim a session name
// holds it until SessionRetention past the expiry of the last token it claimed it for: re-claiming
// extends the claim and never shortens it, and another principal is refused by name meanwhile,
// even once that token has expired. Only a claim past its retention is taken over.
func TestASessionNameIsHeldByThePrincipalThatClaimedIt(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	hour := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name      string
		session   string
		principal string
		expires   time.Time
		runOut    bool
		want      error
	}{
		{name: "an unheld name is claimed", session: "implementer-g1", principal: accountPrincipal, expires: hour},
		{name: "the same principal re-claims it", session: "implementer-g1", principal: accountPrincipal, expires: hour.Add(time.Hour)},
		{name: "a shorter-lived token does not shorten the claim", session: "implementer-g1", principal: accountPrincipal, expires: time.Now().Add(time.Minute)},
		{name: "another principal is refused", session: "implementer-g1", principal: personPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
		{name: "another name is free", session: "ada-devbox", principal: personPrincipal, expires: hour},
		{name: "a name whose token has expired is claimed", session: "reviewer-g1", principal: accountPrincipal, expires: time.Now().Add(-time.Hour)},
		{name: "and is still refused to another principal", session: "reviewer-g1", principal: otherPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
		{name: "a claim past its retention is taken over", session: "implementer-g1", principal: personPrincipal, expires: hour, runOut: true},
		{name: "and then holds against the principal that let it run out", session: "implementer-g1", principal: accountPrincipal, expires: hour, want: oauthstore.ErrSessionHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.runOut {
				if _, err := pool.Exec(ctx, `update oauth_session_bindings set expires_at = now() - interval '1 second' where session = $1`, c.session); err != nil {
					t.Fatalf("run the claim out: %v", err)
				}
			}
			taken, err := s.BindSession(ctx, c.session, c.principal, false, c.expires)
			if !errors.Is(err, c.want) {
				t.Fatalf("BindSession(%q, %q) = %v, want %v", c.session, c.principal, err, c.want)
			}
			if c.want != nil && !strings.Contains(err.Error(), c.session) {
				t.Errorf("BindSession refused with %q, which does not name the session %q", err, c.session)
			}
			if taken != "" {
				t.Errorf("BindSession answered it took %q from %q, want nothing taken by a claim no configuration fixes", c.session, taken)
			}
		})
	}
	var held time.Time
	if err := pool.QueryRow(ctx, `select expires_at from oauth_session_bindings where session = 'implementer-g1'`).Scan(&held); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if want := hour.Add(oauthstore.SessionRetention); held.Before(want.Add(-time.Second)) {
		t.Errorf("the claim runs out at %s, want SessionRetention past the expiry of the token it was made for, %s", held, want)
	}
}

// TestTheNamesTheConfigurationFixesAreHeldForTheirEntries: ReserveSessions holds every name it is
// handed for its principal unless another principal holds it, and says who does; the entry's own
// claim, which its configuration fixes the name for, takes it from that holder and says whom.
// From then on any other principal is refused it, its own principal claims it as often as it
// likes, and no claim or sweep lets it run out. A name a later configuration no longer fixes goes
// back to the ordinary rule: its principal keeps it for SessionRetention from then, and nobody
// else takes it meanwhile. A configuration re-pointing a name takes it at its new entry's claim,
// and an older configuration reserving again afterwards takes nothing back.
func TestTheNamesTheConfigurationFixesAreHeldForTheirEntries(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	hour := time.Now().Add(time.Hour)
	if _, err := s.BindSession(ctx, "legion-daemon:ACME", personPrincipal, false, hour); err != nil {
		t.Fatalf("a dev box claims the name before the configuration fixes it: %v", err)
	}
	first := map[string]string{"legion-daemon:ACME": daemonPrincipal, "ci-release": workflowPrincipal}
	left, err := s.ReserveSessions(ctx, first)
	if err != nil {
		t.Fatalf("ReserveSessions: %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": personPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions left %v with their holders, want %v: the dev box keeps the name until the entry claims it, and an unheld one is reserved", left, want)
	}
	for _, c := range []struct {
		name      string
		session   string
		principal string
		fixed     bool
		want      error
		taken     string
	}{
		{name: "another account is refused the name the dev box holds", session: "legion-daemon:ACME", principal: otherPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "the entry's account takes it from the dev box", session: "legion-daemon:ACME", principal: daemonPrincipal, fixed: true, taken: personPrincipal},
		{name: "the dev box that held it is refused", session: "legion-daemon:ACME", principal: personPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "another account is refused", session: "legion-daemon:ACME", principal: otherPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "and the entry's account claims it again", session: "legion-daemon:ACME", principal: daemonPrincipal, fixed: true},
		{name: "a dev box is refused the workflow's name before the workflow has claimed it", session: "ci-release", principal: personPrincipal, want: oauthstore.ErrSessionHeld},
		{name: "the workflow claims its name", session: "ci-release", principal: workflowPrincipal, fixed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			taken, err := s.BindSession(ctx, c.session, c.principal, c.fixed, hour)
			if !errors.Is(err, c.want) || taken != c.taken {
				t.Fatalf("BindSession(%q, %q, fixed %t) = %q, %v, want %q, %v", c.session, c.principal, c.fixed, taken, err, c.taken, c.want)
			}
		})
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	held := func(session string) (principal string, forever bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select principal, expires_at = 'infinity' from oauth_session_bindings where session = $1`, session).Scan(&principal, &forever); err != nil {
			t.Fatalf("read the claim on %s: %v", session, err)
		}
		return principal, forever
	}
	for session, want := range first {
		if principal, forever := held(session); principal != want || !forever {
			t.Errorf("after its principal's claims and a sweep, %s is held by %q for ever: %t; want %q with no expiry", session, principal, forever, want)
		}
	}

	// The next configuration fixes the daemon's name for another account and no longer fixes the
	// workflow's. Reserving takes nothing; the other account's own claim does.
	next := map[string]string{"legion-daemon:ACME": otherPrincipal}
	left, err = s.ReserveSessions(ctx, next)
	if err != nil {
		t.Fatalf("ReserveSessions(the next configuration): %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": daemonPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions(the next configuration) left %v with their holders, want %v", left, want)
	}
	if principal, forever := held("ci-release"); principal != workflowPrincipal || forever {
		t.Errorf("the name the configuration stopped fixing is held by %q for ever: %t; want %q under the ordinary retention", principal, forever, workflowPrincipal)
	}
	var runsOut time.Time
	if err := pool.QueryRow(ctx, `select expires_at from oauth_session_bindings where session = 'ci-release'`).Scan(&runsOut); err != nil {
		t.Fatalf("read the released claim: %v", err)
	}
	if want := time.Now().Add(oauthstore.SessionRetention); runsOut.Before(want.Add(-time.Minute)) || runsOut.After(want.Add(time.Minute)) {
		t.Errorf("the released claim runs out at %s, want SessionRetention from now, about %s", runsOut, want)
	}
	if _, err := s.BindSession(ctx, "ci-release", personPrincipal, false, hour); !errors.Is(err, oauthstore.ErrSessionHeld) {
		t.Errorf("BindSession(the released name, a dev box) = %v, want ErrSessionHeld: the workflow used it within the retention", err)
	}
	if taken, err := s.BindSession(ctx, "legion-daemon:ACME", otherPrincipal, true, hour); err != nil || taken != daemonPrincipal {
		t.Fatalf("BindSession(the re-pointed name, its new entry) = %q, %v, want it taken from %q", taken, err, daemonPrincipal)
	}

	// A replica still on the first configuration reserves after the next one has: it takes the
	// re-pointed name back from nobody, and a name both hold for one principal is its again.
	left, err = s.ReserveSessions(ctx, first)
	if err != nil {
		t.Fatalf("ReserveSessions(the first configuration, again): %v", err)
	}
	if want := map[string]string{"legion-daemon:ACME": otherPrincipal}; !reflect.DeepEqual(left, want) {
		t.Fatalf("ReserveSessions(the first configuration, again) left %v with their holders, want %v", left, want)
	}
	if principal, forever := held("legion-daemon:ACME"); principal != otherPrincipal || !forever {
		t.Errorf("after an older configuration reserved again, the re-pointed name is held by %q for ever: %t; want %q with no expiry", principal, forever, otherPrincipal)
	}
	if principal, forever := held("ci-release"); principal != workflowPrincipal || !forever {
		t.Errorf("the workflow's name, which the older configuration fixes for it, is held by %q for ever: %t; want %q with no expiry", principal, forever, workflowPrincipal)
	}
	if _, err := s.BindSession(ctx, "legion-daemon:ACME", daemonPrincipal, false, hour); !errors.Is(err, oauthstore.ErrSessionHeld) {
		t.Errorf("BindSession(the re-pointed name, its old entry under the next configuration) = %v, want ErrSessionHeld", err)
	}
}

// TestAJWTIdIsAcceptedOnce: the exchange's replay store accepts a jti once and refuses it while
// the token it came from is live. A record whose token has expired is taken over, so the sweep's
// deletions refuse nothing.
func TestAJWTIdIsAcceptedOnce(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	exp := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name string
		mark func() error
		want bool
	}{
		{name: "an exchanged subject token is accepted", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp) }},
		{name: "and refused the second time", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp) }, want: true},
		{name: "another jti is its own", mark: func() error { return s.SetTokenExchangeCustomJWT(ctx, "jti-2", exp) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.mark(); (err != nil) != c.want {
				t.Fatalf("= %v, want an error: %v", err, c.want)
			}
		})
	}
	if _, err := pool.Exec(ctx, `update oauth_exchange_jtis set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("expire the records: %v", err)
	}
	if err := s.SetTokenExchangeCustomJWT(ctx, "jti-1", exp); err != nil {
		t.Errorf("SetTokenExchangeCustomJWT(an expired record) = %v, want it accepted", err)
	}
}

// TestAnExchangeSessionRoundTrips: an exchanged token's session is stored and read back whole —
// the claims the exchange decided, the subject and actor tokens it decided from, the DPoP key it
// is bound to and its expiry — because an introspection of that token answers from the row.
func TestAnExchangeSessionRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	request := exchangeRequest(t, s, "request-1")
	stored := request.Session.(*oauthstore.ExchangeSession)
	if err := s.CreateAccessTokenSession(ctx, "access-1", request); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	read := oauthstore.NewExchangeSession()
	back, err := s.GetAccessTokenSession(ctx, "access-1", read)
	if err != nil {
		t.Fatalf("GetAccessTokenSession: %v", err)
	}
	if back.GetClient().GetID() != oauthwire.ExchangeClientID {
		t.Errorf("the stored request's client is %q, want the exchange client", back.GetClient().GetID())
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{name: "subject", got: read.GetSubject(), want: stored.GetSubject()},
		{name: "username", got: read.GetUsername(), want: stored.GetUsername()},
		{name: "DPoP thumbprint", got: read.GetDPoPJWKThumbprint(), want: stored.GetDPoPJWKThumbprint()},
		{name: "extra claims", got: read.Extra, want: stored.Extra},
		{name: "subject token", got: read.GetSubjectToken(), want: stored.GetSubjectToken()},
		{name: "actor token", got: read.GetActorToken(), want: stored.GetActorToken()},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !reflect.DeepEqual(c.got, c.want) {
				t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
			}
		})
	}
	if got, want := read.GetExpiresAt(oauth2.AccessToken), stored.GetExpiresAt(oauth2.AccessToken); !got.Equal(want) {
		t.Errorf("access token expiry = %s, want %s", got, want)
	}
	if clone, ok := read.Clone().(*oauthstore.ExchangeSession); !ok || !reflect.DeepEqual(clone.SubjectToken, read.SubjectToken) {
		t.Errorf("Clone = %#v, want a copy of the exchange session", read.Clone())
	}
}

// TestAnExchangedTokenNamesNoLauncherCredential: an exchanged token follows no machine login, so
// its row holds no credential and no revoke of a credential can end it; a machine login's token
// that has lost its credential is still refused, which is the fault the check was written for.
func TestAnExchangedTokenNamesNoLauncherCredential(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.CreateAccessTokenSession(ctx, "access-1", exchangeRequest(t, s, "request-1")); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	if err := s.CreateRefreshTokenSession(ctx, "refresh-1", "access-1", exchangeRequest(t, s, "request-1")); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	for _, c := range []struct{ name, query string }{
		{name: "access token", query: `select credential_id is null from oauth_access_tokens where signature = 'access-1'`},
		{name: "refresh token", query: `select credential_id is null from oauth_refresh_tokens where signature = 'refresh-1'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var none bool
			if err := pool.QueryRow(ctx, c.query).Scan(&none); err != nil {
				t.Fatalf("read the row: %v", err)
			}
			if !none {
				t.Errorf("the %s names a launcher credential, want none", c.name)
			}
		})
	}
	if err := s.CreateAccessTokenSession(ctx, "access-2", tokenRequest(t, s, "request-2", "")); err == nil {
		t.Error("a machine login's token session with no credential_id = nil, want an error")
	}
}

// TestATokenSessionIsReadIntoEitherKind: the library reads the subject access token of an
// exchange into the exchange request's own session, so a row written from a machine login's
// *oauth2.DefaultSession must read back into an *ExchangeSession, and a row written from an
// exchange session must read back into a plain one. A third kind is still refused.
func TestATokenSessionIsReadIntoEitherKind(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	credentialID := newCredential(t, pool)
	if err := s.CreateAccessTokenSession(ctx, "access-1", tokenRequest(t, s, "request-1", credentialID)); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	if err := s.CreateAccessTokenSession(ctx, "access-2", exchangeRequest(t, s, "request-2")); err != nil {
		t.Fatalf("CreateAccessTokenSession: %v", err)
	}
	intoExchange := oauthstore.NewExchangeSession()
	if _, err := s.GetAccessTokenSession(ctx, "access-1", intoExchange); err != nil {
		t.Fatalf("a machine login's token read into an exchange session: %v", err)
	}
	if intoExchange.Extra[oauthwire.ExtraCredentialID] != credentialID {
		t.Errorf("the machine login's session lost its %s: %#v", oauthwire.ExtraCredentialID, intoExchange.Extra)
	}
	intoDefault := &oauth2.DefaultSession{}
	if _, err := s.GetAccessTokenSession(ctx, "access-2", intoDefault); err != nil {
		t.Fatalf("an exchanged token read into a default session: %v", err)
	}
	if intoDefault.Extra["session"] != "implementer-g1" {
		t.Errorf("the exchanged token's claims read back as %#v, want the session it was minted for", intoDefault.Extra)
	}
	if _, err := s.GetAccessTokenSession(ctx, "access-2", &otherSession{}); err == nil {
		t.Error("a read into a third session type = nil, want an error")
	}
}

// TestTheExchangeStorageNeedsItsWiring: the store decides no policy of its own, so each seam the
// provider wiring fills is refused rather than guessed at while it is empty, and answers through
// whatever the wiring set once it is.
func TestTheExchangeStorageNeedsItsWiring(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if _, err := s.GetSubjectForTokenExchange(ctx, exchangeRequest(t, s, "request-1"), map[string]any{}); err == nil {
		t.Error("GetSubjectForTokenExchange with no decision wired = nil, want an error")
	}
	s.ExchangeSubject = func(context.Context, oauth2.Requester, map[string]any) (string, error) {
		return "ada@example.com", nil
	}
	subject, err := s.GetSubjectForTokenExchange(ctx, exchangeRequest(t, s, "request-1"), map[string]any{})
	if err != nil || subject != "ada@example.com" {
		t.Fatalf("GetSubjectForTokenExchange = (%q, %v), want the decision it was wired to", subject, err)
	}
}
