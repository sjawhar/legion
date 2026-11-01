// packages/envoy/internal/broker/enroll/googletoken.go
//
// The broker's own verification of a Google identity token, against one configured OIDC
// verifier. The RFC 8693 exchange does not use it: the library resolves a subject token's key
// itself and checks its signature, issuer and expiry before the exchange's policy reads a claim.
// What needs this is every other caller that is handed such a token whole — a named caller the
// broker authenticates by its Google account, and a resource server reading a machine's token
// back — each of which has rules of its own and so is given the claims rather than a refusal.
package enroll

import (
	"context"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/oidc"
)

// GoogleToken is one verified Google identity token: the identity claims it carries
// (exchange.GoogleIdentity, the one reading of Google's claim shapes) and the times its
// registered claims give it. IssuedAt is zero when the token has no iat claim.
type GoogleToken struct {
	Identity exchange.GoogleIdentity
	Expiry   time.Time
	IssuedAt time.Time
}

// GoogleVMVerifier verifies Google identity tokens against one configured OIDC verifier and reads
// the email and Compute Engine claims from the verified token.
type GoogleVMVerifier struct{ Verifier *oidc.Verifier }

// VerifyGoogle verifies raw's issuer, audience, signature and expiry and reads the identity
// claims of the verified token: the account, whether the address is verified, and the Compute
// Engine instance it was minted on when it carries one. It refuses nothing of its own — a token
// naming no account, an unverified address and a token minted on no instance are each a value it
// answers, since what each means is its caller's rule — so every error it returns is an OIDC
// verifier error or a malformed token. The private claims are read without a second verification
// only after the OIDC verifier has verified this exact token.
func (v GoogleVMVerifier) VerifyGoogle(ctx context.Context, raw string) (GoogleToken, error) {
	claims, err := v.Verifier.Verify(ctx, raw)
	if err != nil {
		return GoogleToken{}, err
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return GoogleToken{}, err
	}
	var registered jwt.Claims
	var private map[string]any
	if err := parsed.UnsafeClaimsWithoutVerification(&registered, &private); err != nil {
		return GoogleToken{}, err
	}
	token := GoogleToken{Identity: exchange.ReadGoogleIdentity(private), Expiry: claims.Expiry}
	if registered.IssuedAt != nil {
		token.IssuedAt = registered.IssuedAt.Time()
	}
	return token, nil
}
