package oidc

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newVerifierFor discovers issuer and returns a verifier for testAudience,
// along with the published signing key.
func newVerifierFor(t *testing.T, issuer *testIssuer) (*Verifier, *testKey) {
	t.Helper()
	signing := newTestKey(t, "signing-key")
	issuer.publish(signing)
	verifier, err := New(context.Background(), issuer.url(), testAudience)
	if err != nil {
		t.Fatalf("New(%s, %s): %v", issuer.url(), testAudience, err)
	}
	return verifier, signing
}

func TestVerifyAcceptsProjectedServiceAccountToken(t *testing.T) {
	issuer := newTestIssuer(t)
	verifier, signing := newVerifierFor(t, issuer)

	arrayClaims := issuer.claims()
	stringClaims := issuer.claims()
	stringClaims["aud"] = testAudience

	tests := []struct {
		name   string
		claims map[string]any
	}{
		{name: "audience as a one-element array", claims: arrayClaims},
		{name: "audience as a bare string", claims: stringClaims},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), issuer.mint(t, signing, tt.claims))
			if err != nil {
				t.Fatalf("Verify: unexpected error: %v", err)
			}
			if claims.Subject != testSubject {
				t.Errorf("Subject = %q, want %q", claims.Subject, testSubject)
			}
			if claims.Issuer != issuer.url() {
				t.Errorf("Issuer = %q, want %q", claims.Issuer, issuer.url())
			}
			if len(claims.Audience) != 1 || claims.Audience[0] != testAudience {
				t.Errorf("Audience = %q, want [%q]", claims.Audience, testAudience)
			}
			wantExpiry := time.Unix(tt.claims["exp"].(int64), 0)
			if !claims.Expiry.Equal(wantExpiry) {
				t.Errorf("Expiry = %v, want %v", claims.Expiry, wantExpiry)
			}
		})
	}
}

func TestVerifyClassifiesRejection(t *testing.T) {
	issuer := newTestIssuer(t)
	verifier, signing := newVerifierFor(t, issuer)
	// Same key id as the published key, different key material: the key set
	// resolves, only the signature fails.
	foreign := newTestKey(t, signing.kid)

	wrongAudience := issuer.claims()
	wrongAudience["aud"] = []string{"envoy"}

	expired := issuer.claims()
	expired["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	otherIssuer := issuer.claims()
	otherIssuer["iss"] = "https://oidc.eks.example.invalid/id/OTHER"

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{name: "audience the verifier was not configured for", token: issuer.mint(t, signing, wrongAudience), want: ErrAudience},
		{name: "token past its expiry", token: issuer.mint(t, signing, expired), want: ErrExpired},
		{name: "issuer the verifier was not configured for", token: issuer.mint(t, signing, otherIssuer), want: ErrIssuer},
		{name: "signed by a key the issuer does not publish", token: issuer.mint(t, foreign, issuer.claims()), want: ErrSignature},
		{name: "three segments that are not a token", token: "not.a.jwt", want: ErrMalformed},
		{name: "no segments at all", token: "abc", want: ErrMalformed},
		{name: "three segments that are not base64url", token: "@@@.@@@.@@@", want: ErrMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), tt.token)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify error = %v, want %v", err, tt.want)
			}
			if !reflect.DeepEqual(claims, Claims{}) {
				t.Errorf("Verify returned claims %+v on a rejected token, want the zero value", claims)
			}
			if strings.Contains(err.Error(), tt.token) {
				t.Errorf("Verify error %q repeats the token", err)
			}
		})
	}
}

func TestVerifyRefreshesKeySetForRotatedKey(t *testing.T) {
	issuer := newTestIssuer(t)
	verifier, signing := newVerifierFor(t, issuer)

	if _, err := verifier.Verify(context.Background(), issuer.mint(t, signing, issuer.claims())); err != nil {
		t.Fatalf("Verify with the published key: %v", err)
	}
	fetchesBefore := issuer.jwksFetches()

	rotated := newTestKey(t, "rotated-key")
	token := issuer.mint(t, rotated, issuer.claims())
	issuer.publish(rotated)

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify with the rotated key: %v", err)
	}
	if claims.Subject != testSubject {
		t.Errorf("Subject = %q, want %q", claims.Subject, testSubject)
	}
	if got := issuer.jwksFetches() - fetchesBefore; got != 1 {
		t.Errorf("key set fetched %d more times, want 1 refresh", got)
	}
}

func TestNewReportsDiscoveryFailure(t *testing.T) {
	issuer := newTestIssuer(t)
	issuer.failDiscovery(http.StatusInternalServerError)

	_, err := New(context.Background(), issuer.url(), testAudience)
	if err == nil {
		t.Fatal("New: expected an error when discovery fails")
	}
	if !strings.Contains(err.Error(), issuer.url()) {
		t.Errorf("New error %q does not name the issuer URL %s", err, issuer.url())
	}
}

func TestNewReportsIssuerMismatch(t *testing.T) {
	issuer := newTestIssuer(t)
	issuer.advertiseIssuer("https://oidc.eks.example.invalid/id/OTHER")

	_, err := New(context.Background(), issuer.url(), testAudience)
	if err == nil {
		t.Fatal("New: expected an error when the document names another issuer")
	}
	if !strings.Contains(err.Error(), issuer.url()) {
		t.Errorf("New error %q does not name the issuer URL %s", err, issuer.url())
	}
}

func TestLooksLikeJWT(t *testing.T) {
	issuer := newTestIssuer(t)
	signing := newTestKey(t, "signing-key")
	issuer.publish(signing)
	minted := issuer.mint(t, signing, issuer.claims())

	secret := []byte("0123456789abcdef0123456789abcdef")

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "a minted token", token: minted, want: true},
		{name: "a Dispatch personal token", token: "dsp_" + base64.RawURLEncoding.EncodeToString(secret), want: false},
		{name: "a shared token", token: hex.EncodeToString(secret), want: false},
		{name: "an empty bearer", token: "", want: false},
		{name: "an empty segment", token: "aaa..ccc", want: false},
		{name: "four segments", token: "aaa.bbb.ccc.ddd", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeJWT(tt.token); got != tt.want {
				t.Errorf("LooksLikeJWT(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}
