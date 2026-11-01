// Package oidc verifies OIDC tokens against one issuer and one audience — a
// Kubernetes pod's projected service-account token, for the Dispatch server and
// the Envoy listener — and signs a person in to Dispatch with the authorization
// code flow against its sign-in issuer (CodeFlow).
package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// The class of every Verify failure. A caller logs the class and answers 401;
// no error carries the token.
var (
	ErrMalformed = errors.New("oidc: malformed token")
	ErrIssuer    = errors.New("oidc: token from another issuer")
	ErrAudience  = errors.New("oidc: token for another audience")
	ErrExpired   = errors.New("oidc: token expired")
	ErrCancelled = errors.New("oidc: verification context ended")
	ErrSignature = errors.New("oidc: token signature not verified")
)

// Reason names the class of a Verify failure in one word, for the log line and
// the error a caller returns. It never repeats any part of the token.
//
// An error carrying no sentinel is reported as a signature failure, because
// classify leaves that class as the residue. Two neighbours of that class are
// named rather than left in it. A request whose context has ended is
// ErrCancelled, so the alarming word is not spent on a caller that hung up;
// an issuer whose key set could not be read — the issuer down, its JWKS
// 500ing, TLS refused — is ErrIssuerUnavailable, reported as unavailable,
// because the key source's own ErrKeySet reaches classify (keyset.go) where
// go-oidc flattened it with %v. What stays in the signature class is a token
// the issuer never signed, a token naming a key id it publishes nothing under
// among them. The trade runs the other way too: classify reads the context
// before the error, so a genuinely bad token presented on an ended request
// reads cancelled. Both of the named classes mean "no verdict is
// trustworthy", not "the token was sound".
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrMalformed):
		return "malformed"
	case errors.Is(err, ErrIssuer):
		return "issuer"
	case errors.Is(err, ErrAudience):
		return "audience"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrCancelled):
		return "cancelled"
	case errors.Is(err, ErrIssuerUnavailable):
		return "unavailable"
	default:
		return "signature"
	}
}

// Claims are the verified claims of a token. For a projected service-account
// token Subject is the Kubernetes subject, "system:serviceaccount:<namespace>:<name>".
// For a Cognito ID token, Username is `cognito:username` (for a person federated
// from an identity provider, `<provider>_<id>`), Groups is `cognito:groups`,
// Providers names the providers of its `identities`, and Nonce is the sign-in's
// nonce; a token that carries none of them leaves them empty. For a Google ID
// token a domain-mode CodeFlow reads, Email is `email`, EmailVerified is
// `email_verified` (true only for the JSON true or the string "true"), and
// HostedDomain is `hd`, the account's Workspace domain; Verify never reads
// these three, so it leaves them empty.
type Claims struct {
	Subject       string
	Issuer        string
	Audience      []string
	Expiry        time.Time
	Username      string
	Groups        []string
	Providers     []string
	Nonce         string
	Email         string
	EmailVerified bool
	HostedDomain  string
}

// cognitoClaims are the claims beyond the standard ones that Claims carries.
type cognitoClaims struct {
	Username   string   `json:"cognito:username"`
	Groups     []string `json:"cognito:groups"`
	Nonce      string   `json:"nonce"`
	Identities []struct {
		ProviderName string `json:"providerName"`
	} `json:"identities"`
}

// Verifier verifies bearer tokens issued by one issuer to one audience.
type Verifier struct {
	verifier *gooidc.IDTokenVerifier
	issuer   string
	audience string
}

// New reads the issuer's OIDC discovery document now and returns a verifier for
// tokens issued to audience, over a key cache of its own (NewKeys). ctx bounds
// discovery only: no key is fetched here, the first Verify reads the key set,
// and that set refreshes on whatever context the verification runs on, so a
// boot context cancelling later never disables key rotation.
func New(ctx context.Context, issuer, audience string) (*Verifier, error) {
	keys, err := NewKeys(ctx, issuer)
	if err != nil {
		return nil, err
	}
	return NewWithKeys(issuer, audience, keys), nil
}

// NewWithKeys returns a verifier for tokens issuer issued to audience, checking
// every signature against keys. It exists so that one key cache serves a
// process's whole use of an issuer: the broker reads the cluster issuer's set
// both to verify account, pod and service-login tokens and, for the RFC 8693
// exchange, to hand the key itself to the OAuth provider (Keys), and building
// both from one *Keys makes one discovery read at boot, keeps one set in
// memory, and bounds the reads a token naming an unpublished key id prompts at
// one per MinRefresh across both of them.
func NewWithKeys(issuer, audience string, keys *Keys) *Verifier {
	return &Verifier{
		// SupportedSigningAlgs is set because gooidc.NewVerifier, unlike the
		// verifier a discovered provider builds, defaults to RS256 alone: the
		// pair named here is the pair keySet parses and the pair a cluster
		// signs its projected tokens with, so an ES256 cluster's tokens go on
		// verifying.
		verifier: gooidc.NewVerifier(issuer, keySet{keys: keys}, &gooidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: supportedSigningAlgs(),
		}),
		issuer:   issuer,
		audience: audience,
	}
}

// supportedSigningAlgs names keySetAlgorithms as go-oidc's configuration takes them.
func supportedSigningAlgs() []string {
	algs := make([]string, 0, len(keySetAlgorithms))
	for _, alg := range keySetAlgorithms {
		algs = append(algs, string(alg))
	}
	return algs
}

// newVerifier is the sign-in flow's verifier, built on a discovered provider
// and so on go-oidc's own RemoteKeySet: a CodeFlow verifies the ID tokens of
// the person signing in, which no other part of this package reads keys for,
// so there is no second reader to share a *Keys with (codeflow.go).
func newVerifier(provider *gooidc.Provider, issuer, audience string) *Verifier {
	return &Verifier{
		verifier: provider.Verifier(&gooidc.Config{ClientID: audience}),
		issuer:   issuer,
		audience: audience,
	}
}

// Verify checks raw against the issuer's published keys, the configured
// audience, and the token's expiry. Every error wraps exactly one of
// ErrMalformed, ErrIssuer, ErrAudience, ErrExpired, ErrCancelled,
// ErrIssuerUnavailable, and ErrSignature.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	// The slot the key source's own error comes back in: go-oidc flattens it
	// (keyset.go, keySetError).
	keyCtx, keySetErr := withKeySetError(ctx)
	token, err := v.verifier.Verify(keyCtx, raw)
	if err != nil {
		return Claims{}, v.classify(ctx, raw, err, keySetErr.err)
	}
	var person cognitoClaims
	if err := token.Claims(&person); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var providers []string
	for _, identity := range person.Identities {
		providers = append(providers, identity.ProviderName)
	}
	return Claims{
		Subject:   token.Subject,
		Issuer:    token.Issuer,
		Audience:  token.Audience,
		Expiry:    token.Expiry,
		Username:  person.Username,
		Groups:    person.Groups,
		Providers: providers,
		Nonce:     person.Nonce,
	}, nil
}

// classify decides the failure class from the token's own claims rather than
// from the verification library's error text, so the reason a caller logs is
// the same across library versions. The library checks the signature before any
// claim, so a token that is wrong in two ways is reported by its claims first
// and only an otherwise-sound token is left as a signature failure — or, where
// the issuer's key set could not be read at all, as ErrIssuerUnavailable:
// keySetErr is the key source's own error, which keyset.go carries past the %v
// go-oidc formats it with, and an ErrKeySet in it means no verdict was reached.
// ErrNoKey is not that case and stays in the signature class, a token signed by
// something the issuer never had.
//
// An ended context is decided first and on the context, never on the error:
// every fmt.Errorf in go-oidc v3.21.0's verify.go formats with %v, so no error
// leaving IDTokenVerifier.Verify preserves a wrapped context sentinel and
// errors.Is could not find one. The cost of reading the context instead is that
// a token that is genuinely bad, presented on a request whose context is
// already done, is reported cancelled rather than by its fault — every
// signature failure routes through the cancellable key-set fetch, so this is
// deterministic, not a race. It is the deliberate side to be wrong on:
// cancelled says "no verdict is trustworthy", where the alternative spends
// "signature" — the word an operator reads as a forgery attempt — on callers
// that merely hung up.
func (v *Verifier) classify(ctx context.Context, raw string, err, keySetErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %v", ErrCancelled, ctxErr)
	}
	var expired *gooidc.TokenExpiredError
	if errors.As(err, &expired) {
		return fmt.Errorf("%w at %s", ErrExpired, expired.Expiry.UTC().Format(time.RFC3339))
	}
	claims, decodeErr := decodeClaims(raw)
	switch {
	case decodeErr != nil:
		return fmt.Errorf("%w: %v", ErrMalformed, decodeErr)
	case claims.Issuer != v.issuer:
		return fmt.Errorf("%w: %q, want %q", ErrIssuer, claims.Issuer, v.issuer)
	case !slices.Contains(claims.Audience, v.audience):
		return fmt.Errorf("%w: %q, want %q", ErrAudience, []string(claims.Audience), v.audience)
	case claims.Expiry != 0 && time.Unix(int64(claims.Expiry), 0).Before(time.Now()):
		return fmt.Errorf("%w at %s", ErrExpired, time.Unix(int64(claims.Expiry), 0).UTC().Format(time.RFC3339))
	case errors.Is(keySetErr, ErrKeySet):
		return fmt.Errorf("%w: %w", ErrIssuerUnavailable, keySetErr)
	default:
		return ErrSignature
	}
}

// LooksLikeJWT reports whether s is three dot-separated non-empty segments. It
// is the callers' cheap discriminator between a JWT and the other bearer shapes
// they accept, none of which contain a dot.
func LooksLikeJWT(s string) bool {
	header := strings.IndexByte(s, '.')
	if header <= 0 {
		return false
	}
	payload := strings.IndexByte(s[header+1:], '.')
	if payload <= 0 {
		return false
	}
	signature := s[header+1+payload+1:]
	return signature != "" && !strings.Contains(signature, ".")
}

// tokenClaims are the claims a token carries about itself. They are read
// without verifying the signature and are used to classify a failure, never to
// authenticate anything.
type tokenClaims struct {
	Issuer   string   `json:"iss"`
	Audience audience `json:"aud"`
	Expiry   float64  `json:"exp"`
}

// audience is the "aud" claim, which OIDC allows to be a string or an array of
// strings.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func decodeClaims(raw string) (tokenClaims, error) {
	var claims tokenClaims
	payload, err := tokenPayload(raw)
	if err != nil {
		return claims, err
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("payload is not a claim set: %w", err)
	}
	return claims, nil
}

// tokenPayload is the JSON payload of raw, a compact JWT, decoded from base64url without verifying
// anything.
func tokenPayload(raw string) ([]byte, error) {
	if !LooksLikeJWT(raw) {
		return nil, errors.New("not three dot-separated segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw[strings.IndexByte(raw, '.')+1 : strings.LastIndexByte(raw, '.')])
	if err != nil {
		return nil, fmt.Errorf("payload is not base64url: %w", err)
	}
	return payload, nil
}
