package enroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	googleTokenAudience = "broker-machines"
	googleTokenAccount  = "example-fleet@example-project.iam.gserviceaccount.com"
	googleTokenProject  = "example-project"
	googleTokenZone     = "example-zone-a"
	googleTokenInstance = "example-machine-1"
	googleTokenID       = "1234567890"
)

func newGoogleVerifier(t *testing.T) (GoogleVMVerifier, *oidctest.Issuer, *oidctest.Key) {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-signing-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), googleTokenAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return GoogleVMVerifier{Verifier: verifier}, issuer, key
}

func googleTokenClaims(issuer *oidctest.Issuer, audience string) map[string]any {
	claims := issuer.Claims(googleTokenAccount, audience)
	claims["email"] = googleTokenAccount
	claims["email_verified"] = true
	claims["google"] = map[string]any{
		"compute_engine": map[string]any{
			"project_id":    googleTokenProject,
			"zone":          googleTokenZone,
			"instance_name": googleTokenInstance,
			"instance_id":   googleTokenID,
		},
	}
	return claims
}

// TestVerifyGoogleAnswersTheClaimsAndRefusesNothingOfItsOwn: it verifies the issuer, the
// audience, the signature and the expiry, and answers what the token says. Which of the claims
// is a refusal is the caller's rule, so nothing here refuses one.
func TestVerifyGoogleAnswersTheClaimsAndRefusesNothingOfItsOwn(t *testing.T) {
	verifier, issuer, key := newGoogleVerifier(t)
	claims := googleTokenClaims(issuer, googleTokenAudience)

	got, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != googleTokenAccount || !got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email verified", got.Identity, googleTokenAccount)
	}
	if !got.Identity.Compute.Whole() || got.Identity.Compute.Project != googleTokenProject || got.Identity.Compute.Instance != googleTokenInstance {
		t.Errorf("Compute = %+v, want the instance %s of %s", got.Identity.Compute, googleTokenInstance, googleTokenProject)
	}
	wantExpiry := time.Unix(claims["exp"].(int64), 0)
	if !got.Expiry.Equal(wantExpiry) {
		t.Errorf("Expiry = %v, want %v", got.Expiry, wantExpiry)
	}
	wantIssuedAt := time.Unix(claims["iat"].(int64), 0)
	if !got.IssuedAt.Equal(wantIssuedAt) {
		t.Errorf("IssuedAt = %v, want %v", got.IssuedAt, wantIssuedAt)
	}
}

// TestVerifyGoogleAnswersATokenMintedOnNoInstance: a token minted through a workload identity
// pool, and the one a pod's metadata server on Google Kubernetes Engine answers, carry no
// google.compute_engine claims at all. Such a token comes back with no instance rather than
// refused, which is what lets a caller that pins nothing take it.
func TestVerifyGoogleAnswersATokenMintedOnNoInstance(t *testing.T) {
	verifier, issuer, key := newGoogleVerifier(t)
	claims := googleTokenClaims(issuer, googleTokenAudience)
	delete(claims, "google")

	got, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != googleTokenAccount || !got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email verified", got.Identity, googleTokenAccount)
	}
	if !got.Identity.Compute.Empty() || got.Identity.Compute.Whole() {
		t.Errorf("Compute = %+v, want no Compute Engine identity at all", got.Identity.Compute)
	}
}

// TestVerifyGoogleAnswersAnUnverifiedAddress: email_verified is reported rather than refused,
// because a caller decides what an unverified address means to it.
func TestVerifyGoogleAnswersAnUnverifiedAddress(t *testing.T) {
	verifier, issuer, key := newGoogleVerifier(t)
	claims := googleTokenClaims(issuer, googleTokenAudience)
	claims["email_verified"] = false

	got, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != googleTokenAccount || got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email not verified", got.Identity, googleTokenAccount)
	}
}

// TestVerifyGoogleRefusesATokenOfAnotherAudience: the verification it does keep is the token's
// own — the issuer, the audience, the signature and the expiry — so a token minted for another
// service is refused before any claim of it is read.
func TestVerifyGoogleRefusesATokenOfAnotherAudience(t *testing.T) {
	verifier, issuer, key := newGoogleVerifier(t)

	_, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, googleTokenClaims(issuer, "other-audience")))
	if !errors.Is(err, oidc.ErrAudience) {
		t.Fatalf("VerifyGoogle error = %v, want %v", err, oidc.ErrAudience)
	}
}
