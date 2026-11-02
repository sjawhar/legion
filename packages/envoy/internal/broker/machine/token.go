package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// MaxTokenLoginLifetime bounds a token login twice: the token it presents lives at most this long
// from its issue (exp - iat, beside the request object's skew), so a copied token logs in as its
// service for at most an hour after it was minted, and the launcher credential the login issues
// expires with that token, never more than this long after the login.
const MaxTokenLoginLifetime = time.Hour

// ErrServiceLogsInWithToken refuses a device-code machine login that names a service, and a
// decision on one opened before token logins: a service logs in with its own token.
var ErrServiceLogsInWithToken = errors.New("a service logs in with its own projected Kubernetes ServiceAccount token at POST /v1/launcher-credentials/token, never through a code a person approves")

var (
	// ErrTokenLoginsOff is VerifyServiceToken's refusal on a broker that trusts no Kubernetes
	// token issuer (BROKER_K8S_OIDC_ISSUER unset).
	ErrTokenLoginsOff = errors.New("this broker trusts no Kubernetes token issuer, so no service logs in with a token")
	// ErrTokenExpired is VerifyServiceToken's refusal of a token past its expiry: the caller logs in
	// again with its current token.
	ErrTokenExpired = errors.New("the ServiceAccount token has expired")
	// ErrTokenInvalid is VerifyServiceToken's refusal of a token that does not verify: another
	// issuer or audience, a bad signature, or a malformed token.
	ErrTokenInvalid = errors.New("the ServiceAccount token does not verify")
	// ErrTokenTooLongLived is VerifyServiceToken's refusal of a verified token that names no issue
	// time, or lives longer than MaxTokenLoginLifetime from it: a projected token's
	// expirationSeconds, or `kubectl create token --duration`, must be at most an hour.
	ErrTokenTooLongLived = errors.New("the ServiceAccount token lives longer than an hour from its issue")
	// ErrAccountNotListed is the refusal of a verified token whose ServiceAccount
	// BROKER_SERVICE_LOGINS does not name, or names for another service than the request asks.
	ErrAccountNotListed = errors.New("this ServiceAccount does not log in as this service")
)

// ServiceAccount is a verified token whose identity is listed to log in as a service: the
// token's claims, the service it logs in as, and the host its credential names.
type ServiceAccount struct {
	enroll.AccountClaims
	Service string
	// Host is the account's <namespace>/<name>.
	Host string
}

// TokenCredential is what a token login issued: the launcher credential's id and its expiry.
type TokenCredential struct {
	ID        string
	ExpiresAt time.Time
}

// VerifyServiceToken verifies a service's projected Kubernetes ServiceAccount token and maps it
// to the service it logs in as: the token goes to the cluster's verifier (s.Enroll.Pod, the one
// pod enrollment uses) and its subject must be in BROKER_SERVICE_LOGINS. It is held to
// MaxTokenLoginLifetime from its issue, and AuthenticateLauncher reads the same setting for
// every credential a login issues. It is both the check a token login makes and the check a
// route whose callers are a deployment's daemons makes (api's serviceAuth class,
// POST /v1/oauth/exchange/pod-sessions), so the two refuse the same accounts for the same
// reasons. With no Kubernetes issuer it is ErrTokenLoginsOff. The other refusals are
// ErrTokenExpired, ErrTokenInvalid, ErrTokenTooLongLived and ErrAccountNotListed; none repeats
// the token.
func (s *Service) VerifyServiceToken(ctx context.Context, token string) (ServiceAccount, error) {
	if s.Enroll.Pod == nil {
		return ServiceAccount{}, ErrTokenLoginsOff
	}
	claims, err := s.Enroll.Pod.VerifyAccount(ctx, token)
	if err != nil {
		return ServiceAccount{}, tokenRefusal(err)
	}
	return s.verifyKubernetes(claims)
}

// tokenRefusal is a ServiceAccount token that did not verify: ErrTokenExpired for one past its
// expiry, else ErrTokenInvalid, each naming why.
func tokenRefusal(err error) error {
	if errors.Is(err, oidc.ErrExpired) {
		return fmt.Errorf("%w (%v)", ErrTokenExpired, err)
	}
	return fmt.Errorf("%w (%v)", ErrTokenInvalid, err)
}

// lifetimeProblem says why a verified token is ErrTokenTooLongLived: it names no issue time, or
// lives longer than MaxTokenLoginLifetime from it. "" is a token that is not.
func (s *Service) lifetimeProblem(claims enroll.AccountClaims) string {
	if claims.IssuedAt.IsZero() {
		return "names no issue time (iat)"
	}
	if lives := claims.Expiry.Sub(claims.IssuedAt); lives > MaxTokenLoginLifetime+s.Skew {
		return fmt.Sprintf("lives %s", lives)
	}
	return ""
}

// verifyKubernetes maps a verified ServiceAccount token through BROKER_SERVICE_LOGINS.
func (s *Service) verifyKubernetes(claims enroll.AccountClaims) (ServiceAccount, error) {
	if problem := s.lifetimeProblem(claims); problem != "" {
		return ServiceAccount{}, fmt.Errorf("%w: it %s", ErrTokenTooLongLived, problem)
	}
	service, listed := s.Enroll.ServiceLogins[claims.Subject]
	if !listed {
		return ServiceAccount{}, fmt.Errorf("%w: %s is not in BROKER_SERVICE_LOGINS", ErrAccountNotListed, claims.Subject)
	}
	namespace, name, _ := strings.Cut(strings.TrimPrefix(claims.Subject, "system:serviceaccount:"), ":")
	return ServiceAccount{AccountClaims: claims, Service: service, Host: namespace + "/" + name}, nil
}

// TokenLogin logs account (VerifyServiceToken's answer) in, and no person approves it.
// compactRequest is a machine request object signed by the key the credential is bound to, with
// exactly one launcher_credential detail naming account's service, verified and replay-checked as
// LoginDevice's is (verifyLauncherRequest). The account's one credential (enroll.Service.IssueTokenLogin)
// moves to that key and expires with the token, at most MaxTokenLoginLifetime after the login.
//
// Refusals: record.ErrRequestInvalid (a request object that does not verify, names no single
// launcher_credential detail, or repeats a jti), ErrAccountNotListed (it names another service),
// and ErrKeyHoldsLiveCredential (a key another live credential holds).
func (s *Service) TokenLogin(ctx context.Context, account ServiceAccount, compactRequest string) (TokenCredential, error) {
	now := time.Now()
	obj, err := s.verifyLauncherRequest(ctx, compactRequest, now, func(obj record.RequestObject) error {
		if obj.Service() != account.Service {
			return fmt.Errorf("%w: %s logs in as %q, not %q", ErrAccountNotListed, account.Subject, account.Service, obj.Service())
		}
		return nil
	})
	if err != nil {
		return TokenCredential{}, err
	}
	jwk, err := embeddedJWK(compactRequest)
	if err != nil {
		return TokenCredential{}, err
	}
	expires := now.Add(MaxTokenLoginLifetime)
	if account.Expiry.Before(expires) {
		expires = account.Expiry
	}
	expires = expires.UTC().Truncate(time.Microsecond)
	id, err := s.Enroll.IssueTokenLogin(ctx, enroll.TokenLogin{
		Subject: account.Subject, Service: account.Service, Host: account.Host, PodUID: account.PodUID, TokenExpires: account.Expiry,
		Thumbprint: obj.Thumbprint, JWK: jwk, Expires: expires,
	})
	if store.IsUniqueViolation(err) {
		return TokenCredential{}, ErrKeyHoldsLiveCredential
	}
	if err != nil {
		return TokenCredential{}, err
	}
	return TokenCredential{ID: id.String(), ExpiresAt: expires}, nil
}
