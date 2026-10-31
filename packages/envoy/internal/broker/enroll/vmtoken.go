package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/oidc"
)

// ErrVMEmailNotVerified indicates that a VM identity token's email is not verified.
var ErrVMEmailNotVerified = errors.New("vm token email is not verified")

// ErrVMNoEmail indicates that a VM identity token names no email at all: one minted without
// format=full, or for no service account.
var ErrVMNoEmail = errors.New("vm token names no email (request it with format=full)")

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

// RuntimeID names one VM as the broker keys it: <account>@<project>/<zone>/<instance id>, the
// service-account email with the instance's project, zone and id. An instance id is unique only
// within a project and a zone, so the id alone does not name one VM. A VM's token login holds its
// launcher credential under this subject, and its vm enrollment carries it as its runtime id, so
// each VM of a fleet that shares one account has a credential and an enrollment of its own.
func (c VMClaims) RuntimeID() string {
	return c.Account + "@" + c.Project + "/" + c.Zone + "/" + c.InstanceID
}

// vmRuntimeAccount reads the account and project back out of a VMClaims.RuntimeID; ok is false
// for any other subject, a Kubernetes ServiceAccount's among them, which holds no "@".
func vmRuntimeAccount(id string) (account, project string, ok bool) {
	at := strings.LastIndex(id, "@")
	if at <= 0 {
		return "", "", false
	}
	project, _, ok = strings.Cut(id[at+1:], "/")
	return id[:at], project, ok
}

// VMVerifier verifies a VM identity token and returns its verified identity fields. Issues reports,
// without verifying anything, whether a token names the verifier's issuer, so a token login sends
// a VM's token to this verifier alone and every other token to the cluster's.
type VMVerifier interface {
	VerifyVM(ctx context.Context, raw string) (VMClaims, error)
	Issues(raw string) bool
}

// GoogleVMVerifier verifies Google Compute Engine identity tokens against one configured OIDC
// verifier and reads the email and Compute Engine claims from the verified token.
type GoogleVMVerifier struct{ Verifier *oidc.Verifier }

// Issues is the OIDC verifier's Issues: whether raw's unverified iss claim names its issuer.
func (v GoogleVMVerifier) Issues(raw string) bool {
	return v.Verifier.Issues(raw)
}

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
	if private.Email == "" {
		return VMClaims{}, ErrVMNoEmail
	}
	if !isVMEmailVerified(private.EmailVerified) {
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
