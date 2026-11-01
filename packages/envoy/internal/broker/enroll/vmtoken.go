package enroll

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/broker/exchange"
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

// VMVerifier verifies a VM identity token and returns its verified identity fields. VerifyGoogle
// is the same verification without the Compute Engine rules, for the callers that have rules of
// their own (the token exchange's machine entries, which may pin an instance or take a token
// minted on none). Issues reports, without verifying anything, whether a token names the
// verifier's issuer, so a token login sends a VM's token to this verifier alone and every other
// token to the cluster's.
type VMVerifier interface {
	VerifyVM(ctx context.Context, raw string) (VMClaims, error)
	VerifyGoogle(ctx context.Context, raw string) (GoogleToken, error)
	Issues(raw string) bool
}

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

// Issues is the OIDC verifier's Issues: whether raw's unverified iss claim names its issuer.
func (v GoogleVMVerifier) Issues(raw string) bool {
	return v.Verifier.Issues(raw)
}

// VerifyGoogle verifies raw's issuer, audience, signature and expiry and reads the identity
// claims of the verified token: the account, whether the address is verified, and the Compute
// Engine instance it was minted on when it carries one. It refuses nothing of its own — a token
// naming no account, an unverified address and a token minted on no instance are each a value it
// answers, since what each means is its caller's rule (VerifyVM's, and the token exchange's
// machine entries') — so every error it returns is an OIDC verifier error or a malformed token.
// The private claims are read without a second verification only after the OIDC verifier has
// verified this exact token.
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

// VerifyVM is VerifyGoogle held to what a VM's token login needs: a verified email and the whole
// Compute Engine identity of the instance the token was minted on, which is what names the VM the
// login enrolls (VMClaims.RuntimeID). A token carrying none of those claims, or part of them, is
// ErrVMComputeEngineClaims: this caller has an instance to name and cannot name one from a token
// that was not minted on one. Every other error is VerifyGoogle's.
func (v GoogleVMVerifier) VerifyVM(ctx context.Context, raw string) (VMClaims, error) {
	token, err := v.VerifyGoogle(ctx, raw)
	if err != nil {
		return VMClaims{}, err
	}
	identity := token.Identity
	if identity.Account == "" {
		return VMClaims{}, ErrVMNoEmail
	}
	if !identity.EmailVerified {
		return VMClaims{}, ErrVMEmailNotVerified
	}
	if !identity.Compute.Whole() {
		return VMClaims{}, ErrVMComputeEngineClaims
	}
	return VMClaims{
		Account:    identity.Account,
		Project:    identity.Compute.Project,
		Zone:       identity.Compute.Zone,
		Instance:   identity.Compute.Instance,
		InstanceID: identity.Compute.InstanceID,
		Expiry:     token.Expiry,
		IssuedAt:   token.IssuedAt,
	}, nil
}
