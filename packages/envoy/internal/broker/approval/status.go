package approval

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oidc"
)

// What GET /signin/status answers about this page is written from here: a sign-in that
// completes, a sign-in that fails, and a decision a person makes each write their own fact into
// the broker's one signin_status row (store.SignInStatus).
//
// No write here can fail what it reports on. A sign-in that worked has already made its session,
// and a decision has already been applied and is already in the record's own event chain; the
// status is a report about them, so failing the person's sign-in or losing their decision
// because a report could not be written would trade the thing itself for the account of it. Each
// failure is logged at WARN instead, which is what a report nobody could write is worth.

// signInFailureCodes are the only reasons a failed sign-in is ever written with. Every one of
// them is a word this code chose, never a word a provider sent: a provider's own message can
// name the person who tried to sign in, and GET /signin/status is answered to a caller who
// showed no credential. The set is closed again by the signin_status check constraint, which
// refuses anything else before it can be stored.
//
// A failure none of these names is not written at all (recordFailedSignIn): a twelfth code
// meaning "something else" would say nothing a reader could act on, would be the code every new
// failure silently landed in, and would leave the rollback trigger this vocabulary exists for -
// a check watching for one exact word - matching a word whose meaning drifts. The log line
// carries the whole error for whoever is looking.
const (
	// From the token endpoint, as it named them (RFC 6749 section 5.2).
	codeInvalidClient       = "invalid_client"
	codeUnauthorizedClient  = "unauthorized_client"
	codeRedirectURIMismatch = "redirect_uri_mismatch"
	codeInvalidGrant        = "invalid_grant"
	// The issuer could not be asked at all: its discovery document could not be read.
	codeIssuerUnreachable = "issuer_unreachable"
	// From the ID token's own verification (oidc.Reason's classes) and its nonce.
	codeBadIssuer    = "bad_issuer"
	codeBadAudience  = "bad_audience"
	codeBadSignature = "bad_signature"
	codeBadNonce     = "bad_nonce"
	// From the Workspace rule the verified token is then held to (oidc.WorkspaceLogin).
	codeNotInDomain      = "not_in_domain"
	codeEmailNotVerified = "email_not_verified"
)

// signInFailureCode is the code for err, and "" for a failure none of the codes names.
func signInFailureCode(err error) string {
	var retrieve *oauth2.RetrieveError
	switch {
	case errors.Is(err, oidc.ErrIssuerUnavailable):
		return codeIssuerUnreachable
	case errors.As(err, &retrieve):
		// The token endpoint's own error code, kept only when it is one of the four named here:
		// an issuer's word is not this page's vocabulary just because it arrived in the error
		// field.
		switch retrieve.ErrorCode {
		case codeInvalidClient, codeUnauthorizedClient, codeRedirectURIMismatch, codeInvalidGrant:
			return retrieve.ErrorCode
		}
	case errors.Is(err, oidc.ErrIssuer):
		return codeBadIssuer
	case errors.Is(err, oidc.ErrAudience):
		return codeBadAudience
	case errors.Is(err, oidc.ErrSignature):
		return codeBadSignature
	case errors.Is(err, oidc.ErrNonce):
		return codeBadNonce
	case errors.Is(err, oidc.ErrNotInDomain):
		return codeNotInDomain
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return codeEmailNotVerified
	}
	return ""
}

// recordSignIn writes the time of a sign-in that completed.
func (p *Page) recordSignIn(ctx context.Context) {
	if err := p.cfg.Store.RecordSignIn(ctx, time.Now()); err != nil {
		slog.Warn("approval: the sign-in status was not written", "fact", "the last sign-in", "error", err)
	}
}

// recordFailedSignIn writes the time of a sign-in that failed at the token exchange or at the
// ID-token check, with the code for err. A failure signInFailureCode does not name leaves the
// row as it was, so the status goes on reporting the last failure it could name rather than a
// newer one it could not: the line logged here is where that failure is read.
func (p *Page) recordFailedSignIn(ctx context.Context, err error) {
	code := signInFailureCode(err)
	if code == "" {
		slog.Warn("approval: a failed sign-in that no status code names, so the sign-in status still reports the last one that was named", "error", err)
		return
	}
	if err := p.cfg.Store.RecordFailedSignIn(ctx, code, time.Now()); err != nil {
		slog.Warn("approval: the sign-in status was not written", "fact", "the last failed sign-in", "error", err)
	}
}

// recordDecision writes the time of a decision made on the page.
func (p *Page) recordDecision(ctx context.Context) {
	if err := p.cfg.Store.RecordDecision(ctx, time.Now()); err != nil {
		slog.Warn("approval: the sign-in status was not written", "fact", "the last decision", "error", err)
	}
}
