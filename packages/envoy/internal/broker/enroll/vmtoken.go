package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/oidc"
)

// ErrVMEmailNotVerified indicates that a VM identity token has no verified email.
var ErrVMEmailNotVerified = errors.New("vm token email is not verified")

// ErrVMComputeEngineClaims indicates that a VM identity token lacks a Compute Engine identity.
var ErrVMComputeEngineClaims = errors.New("vm token has no Compute Engine claims")

// VMClaims are the verified identity fields from a Google Compute Engine identity token. Project,
// Zone and InstanceID together identify one Compute Engine instance. IssuedAt is zero when the
// token has no iat claim.
type VMClaims struct {
	Account    string
	Project    string
	Zone       string
	Instance   string
	InstanceID string
	Expiry     time.Time
	IssuedAt   time.Time
}

// VMVerifier verifies a VM identity token and returns its verified identity fields.
type VMVerifier interface {
	VerifyVM(ctx context.Context, raw string) (VMClaims, error)
}

// GoogleVMVerifier verifies Google Compute Engine identity tokens against one configured OIDC
// verifier and reads the email and Compute Engine claims from the verified token.
type GoogleVMVerifier struct{ Verifier *oidc.Verifier }

func isVMEmailVerified(value json.RawMessage) bool {
	var text string
	return string(value) == "true" || json.Unmarshal(value, &text) == nil && text == "true"
}

// VerifyVM verifies raw's issuer, audience, signature and expiry before reading its email and
// Compute Engine claims. Every error is an OIDC verifier error, one of this file's sentinels, a
// malformed token, or a malformed private claim. The private claims are read without a second
// verification only after the OIDC verifier has verified this exact token.
func (v GoogleVMVerifier) VerifyVM(ctx context.Context, raw string) (VMClaims, error) {
	claims, err := v.Verifier.Verify(ctx, raw)
	if err != nil {
		return VMClaims{}, err
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return VMClaims{}, err
	}
	var registered jwt.Claims
	var private struct {
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		Google        struct {
			ComputeEngine struct {
				ProjectID    string `json:"project_id"`
				Zone         string `json:"zone"`
				InstanceName string `json:"instance_name"`
				InstanceID   string `json:"instance_id"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if err := parsed.UnsafeClaimsWithoutVerification(&registered, &private); err != nil {
		return VMClaims{}, err
	}
	if private.Email == "" || !isVMEmailVerified(private.EmailVerified) {
		return VMClaims{}, ErrVMEmailNotVerified
	}
	compute := private.Google.ComputeEngine
	if compute.ProjectID == "" || compute.Zone == "" || compute.InstanceName == "" || compute.InstanceID == "" {
		return VMClaims{}, ErrVMComputeEngineClaims
	}
	var issuedAt time.Time
	if registered.IssuedAt != nil {
		issuedAt = registered.IssuedAt.Time()
	}
	return VMClaims{
		Account:    private.Email,
		Project:    compute.ProjectID,
		Zone:       compute.Zone,
		Instance:   compute.InstanceName,
		InstanceID: compute.InstanceID,
		Expiry:     claims.Expiry,
		IssuedAt:   issuedAt,
	}, nil
}
