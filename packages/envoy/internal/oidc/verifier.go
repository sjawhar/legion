// Package oidc verifies OIDC bearer tokens — a Kubernetes pod's projected
// service-account token — against one issuer and one audience, for the Dispatch
// server and the Envoy listener.
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
// classify leaves that class as the residue. One real failure still lands
// there and is not a forgery: a key-set retrieval that fails for its own
// reasons — the issuer down, its JWKS 500ing, TLS refused — because go-oidc
// flattens that error with %v and leaves nothing to match on. The cleanly
// detectable half of that family, a request whose context has ended, is
// ErrCancelled, so the alarming word is not spent on a caller that hung up.
// The trade runs the other way too: classify reads the context before the
// error, so a genuinely bad token presented on an ended request reads
// cancelled. The class means "no verdict is trustworthy", not "the token was
// sound".
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
	default:
		return "signature"
	}
}

// Claims are the verified claims of a service-account token. Subject is the
// Kubernetes subject, "system:serviceaccount:<namespace>:<name>".
type Claims struct {
	Subject  string
	Issuer   string
	Audience []string
	Expiry   time.Time
}

// Verifier verifies bearer tokens issued by one issuer to one audience.
type Verifier struct {
	verifier *gooidc.IDTokenVerifier
	issuer   string
	audience string
}

// New reads the issuer's OIDC discovery document now and returns a verifier for
// tokens issued to audience. ctx bounds discovery only: the key set refreshes on
// its own background context, so a boot context cancelling later never disables
// key rotation.
func New(ctx context.Context, issuer, audience string) (*Verifier, error) {
	provider, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering issuer %s: %w", issuer, err)
	}
	return &Verifier{
		verifier: provider.Verifier(&gooidc.Config{ClientID: audience}),
		issuer:   issuer,
		audience: audience,
	}, nil
}

// Verify checks raw against the issuer's published keys, the configured
// audience, and the token's expiry. Every error wraps exactly one of
// ErrMalformed, ErrIssuer, ErrAudience, ErrExpired, ErrCancelled, and
// ErrSignature.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, v.classify(ctx, raw, err)
	}
	return Claims{
		Subject:  token.Subject,
		Issuer:   token.Issuer,
		Audience: token.Audience,
		Expiry:   token.Expiry,
	}, nil
}

// classify decides the failure class from the token's own claims rather than
// from the verification library's error text, so the reason a caller logs is
// the same across library versions. The library checks the signature before any
// claim, so a token that is wrong in two ways is reported by its claims first
// and only an otherwise-sound token is left as a signature failure.
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
func (v *Verifier) classify(ctx context.Context, raw string, err error) error {
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
	if !LooksLikeJWT(raw) {
		return claims, errors.New("not three dot-separated segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw[strings.IndexByte(raw, '.')+1 : strings.LastIndexByte(raw, '.')])
	if err != nil {
		return claims, fmt.Errorf("payload is not base64url: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("payload is not a claim set: %w", err)
	}
	return claims, nil
}
