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

// The identities a session name is claimed for here: a pod of the cluster and a dev box's machine
// credential, two of the three kinds the exchange binds a session name to.
const (
	podIdentity     = "11111111-2222-3333-4444-555555555555"
	machineIdentity = "66666666-7777-8888-9999-aaaaaaaaaaaa"
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

// TestASessionNameIsHeldByTheIdentityThatClaimedIt: the first identity to claim a session name
// holds it until its claim lapses, re-claiming extends the claim and never shortens it, and
// another identity is refused by name meanwhile and takes the name over once the claim has
// lapsed.
func TestASessionNameIsHeldByTheIdentityThatClaimedIt(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	hour := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name     string
		session  string
		identity string
		expires  time.Time
		lapse    bool
		want     error
	}{
		{name: "an unheld name is claimed", session: "implementer-g1", identity: podIdentity, expires: hour},
		{name: "the same identity re-claims it", session: "implementer-g1", identity: podIdentity, expires: hour.Add(time.Hour)},
		{name: "a shorter-lived token does not shorten the claim", session: "implementer-g1", identity: podIdentity, expires: time.Now().Add(time.Minute)},
		{name: "another identity is refused", session: "implementer-g1", identity: machineIdentity, expires: hour, want: oauthstore.ErrSessionHeld},
		{name: "another name is free", session: "ada-devbox", identity: machineIdentity, expires: hour},
		{name: "a lapsed claim is taken over", session: "implementer-g1", identity: machineIdentity, expires: hour, lapse: true},
		{name: "and then holds against the identity that let it lapse", session: "implementer-g1", identity: podIdentity, expires: hour, want: oauthstore.ErrSessionHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.lapse {
				if _, err := pool.Exec(ctx, `update oauth_session_bindings set expires_at = now() - interval '1 second' where session = $1`, c.session); err != nil {
					t.Fatalf("lapse the claim: %v", err)
				}
			}
			err := s.BindSession(ctx, c.session, c.identity, c.expires)
			if !errors.Is(err, c.want) {
				t.Fatalf("BindSession(%q, %q) = %v, want %v", c.session, c.identity, err, c.want)
			}
			if c.want != nil && !strings.Contains(err.Error(), c.session) {
				t.Errorf("BindSession refused with %q, which does not name the session %q", err, c.session)
			}
		})
	}
	var held time.Time
	if err := pool.QueryRow(ctx, `select expires_at from oauth_session_bindings where session = 'implementer-g1'`).Scan(&held); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if !held.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("the claim lapses at %s, want the moment the token it was made for expires", held)
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
