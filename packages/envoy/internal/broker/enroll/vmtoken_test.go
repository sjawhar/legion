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
	vmTokenAudience = "broker-vm"
	vmTokenAccount  = "example-fleet@example-project.iam.gserviceaccount.com"
	vmTokenProject  = "example-project"
	vmTokenZone     = "example-zone-a"
	vmTokenInstance = "example-vm-1"
	vmTokenID       = "1234567890"
)

var _ VMVerifier = GoogleVMVerifier{}

func newGoogleVMVerifier(t *testing.T) (GoogleVMVerifier, *oidctest.Issuer, *oidctest.Key) {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "vm-signing-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), vmTokenAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return GoogleVMVerifier{Verifier: verifier}, issuer, key
}

func vmTokenClaims(issuer *oidctest.Issuer, audience string) map[string]any {
	claims := issuer.Claims(vmTokenAccount, audience)
	claims["email"] = vmTokenAccount
	claims["email_verified"] = true
	claims["google"] = map[string]any{
		"compute_engine": map[string]any{
			"project_id":    vmTokenProject,
			"zone":          vmTokenZone,
			"instance_name": vmTokenInstance,
			"instance_id":   vmTokenID,
		},
	}
	return claims
}

func TestAVMTokenGivesItsAccountProjectAndInstance(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)

	got, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyVM: %v", err)
	}
	if got.Account != vmTokenAccount {
		t.Errorf("Account = %q, want %q", got.Account, vmTokenAccount)
	}
	if got.Project != vmTokenProject {
		t.Errorf("Project = %q, want %q", got.Project, vmTokenProject)
	}
	if got.Zone != vmTokenZone {
		t.Errorf("Zone = %q, want %q", got.Zone, vmTokenZone)
	}
	if got.Instance != vmTokenInstance {
		t.Errorf("Instance = %q, want %q", got.Instance, vmTokenInstance)
	}
	if got.InstanceID != vmTokenID {
		t.Errorf("InstanceID = %q, want %q", got.InstanceID, vmTokenID)
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

func TestAVMTokenWithStringEmailVerifiedGivesItsAccount(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	claims["email_verified"] = "true"

	got, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyVM: %v", err)
	}
	if got.Account != vmTokenAccount {
		t.Errorf("Account = %q, want %q", got.Account, vmTokenAccount)
	}
}

func TestATokenForAnotherAudienceIsRefused(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)

	_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, vmTokenClaims(issuer, "other-audience")))
	if !errors.Is(err, oidc.ErrAudience) {
		t.Fatalf("VerifyVM error = %v, want %v", err, oidc.ErrAudience)
	}
}

func TestATokenWithEmailVerifiedFalseIsRefused(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	for _, emailVerified := range []any{false, "false"} {
		claims := vmTokenClaims(issuer, vmTokenAudience)
		claims["email_verified"] = emailVerified

		_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
		if !errors.Is(err, ErrVMEmailNotVerified) {
			t.Fatalf("VerifyVM error = %v, want %v", err, ErrVMEmailNotVerified)
		}
	}
}

func TestATokenWithoutAnEmailIsRefused(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	delete(claims, "email")

	_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
	if !errors.Is(err, ErrVMNoEmail) {
		t.Fatalf("VerifyVM error = %v, want %v", err, ErrVMNoEmail)
	}
}

func TestATokenWithoutTheComputeEngineClaimsIsRefused(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	delete(claims, "google")

	_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
	if !errors.Is(err, ErrVMComputeEngineClaims) {
		t.Fatalf("VerifyVM error = %v, want %v", err, ErrVMComputeEngineClaims)
	}
}

func TestATokenWithAPartialComputeEngineIdentityIsRefused(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	for _, missing := range []string{"project_id", "zone", "instance_name", "instance_id"} {
		t.Run(missing, func(t *testing.T) {
			claims := vmTokenClaims(issuer, vmTokenAudience)
			compute := claims["google"].(map[string]any)["compute_engine"].(map[string]any)
			delete(compute, missing)

			_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
			if !errors.Is(err, ErrVMComputeEngineClaims) {
				t.Fatalf("VerifyVM error = %v, want %v", err, ErrVMComputeEngineClaims)
			}
		})
	}
}

func TestAnExpiredTokenIsRefusedAsExpired(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()

	_, err := verifier.VerifyVM(context.Background(), issuer.Mint(t, key, claims))
	if !errors.Is(err, oidc.ErrExpired) {
		t.Fatalf("VerifyVM error = %v, want %v", err, oidc.ErrExpired)
	}
}

// TestVerifyGoogleAnswersTheClaimsAndRefusesNothingOfItsOwn: the half of the verification a
// caller with Compute Engine rules of its own takes (the token exchange's machine entries). It
// verifies the issuer, the audience, the signature and the expiry, and answers what the token
// says: a token minted on no instance comes back with no instance rather than refused, and an
// unverified address comes back unverified rather than refused, because which of those is a
// refusal is the caller's rule.
func TestVerifyGoogleAnswersTheClaimsAndRefusesNothingOfItsOwn(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)

	got, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, claims))
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != vmTokenAccount || !got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email verified", got.Identity, vmTokenAccount)
	}
	if !got.Identity.Compute.Whole() || got.Identity.Compute.Project != vmTokenProject || got.Identity.Compute.Instance != vmTokenInstance {
		t.Errorf("Compute = %+v, want the instance %s of %s", got.Identity.Compute, vmTokenInstance, vmTokenProject)
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
// google.compute_engine claims at all. VerifyGoogle answers such a token with an empty instance,
// which is what lets an exchange entry that pins nothing take it; VerifyVM, which has an instance
// to name, still refuses it.
func TestVerifyGoogleAnswersATokenMintedOnNoInstance(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	delete(claims, "google")
	token := issuer.Mint(t, key, claims)

	got, err := verifier.VerifyGoogle(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != vmTokenAccount || !got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email verified", got.Identity, vmTokenAccount)
	}
	if !got.Identity.Compute.Empty() || got.Identity.Compute.Whole() {
		t.Errorf("Compute = %+v, want no Compute Engine identity at all", got.Identity.Compute)
	}
	if _, err := verifier.VerifyVM(context.Background(), token); !errors.Is(err, ErrVMComputeEngineClaims) {
		t.Fatalf("VerifyVM of the same token = %v, want %v", err, ErrVMComputeEngineClaims)
	}
}

// TestVerifyGoogleAnswersAnUnverifiedAddress: VerifyGoogle reports email_verified rather than
// refusing it, and VerifyVM refuses it, so the one verification serves both rules.
func TestVerifyGoogleAnswersAnUnverifiedAddress(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)
	claims := vmTokenClaims(issuer, vmTokenAudience)
	claims["email_verified"] = false
	token := issuer.Mint(t, key, claims)

	got, err := verifier.VerifyGoogle(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyGoogle: %v", err)
	}
	if got.Identity.Account != vmTokenAccount || got.Identity.EmailVerified {
		t.Errorf("Identity = %+v, want the account %s with its email not verified", got.Identity, vmTokenAccount)
	}
	if _, err := verifier.VerifyVM(context.Background(), token); !errors.Is(err, ErrVMEmailNotVerified) {
		t.Fatalf("VerifyVM of the same token = %v, want %v", err, ErrVMEmailNotVerified)
	}
}

// TestVerifyGoogleRefusesATokenOfAnotherAudience: the verification VerifyGoogle does keep is the
// token's own — the issuer, the audience, the signature and the expiry — so a token minted for
// another service is refused before any claim of it is read.
func TestVerifyGoogleRefusesATokenOfAnotherAudience(t *testing.T) {
	verifier, issuer, key := newGoogleVMVerifier(t)

	_, err := verifier.VerifyGoogle(context.Background(), issuer.Mint(t, key, vmTokenClaims(issuer, "other-audience")))
	if !errors.Is(err, oidc.ErrAudience) {
		t.Fatalf("VerifyGoogle error = %v, want %v", err, oidc.ErrAudience)
	}
}
