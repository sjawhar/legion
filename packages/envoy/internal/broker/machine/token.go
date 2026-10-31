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
var ErrServiceLogsInWithToken = errors.New("a service logs in with its own Kubernetes ServiceAccount token at POST /v1/launcher-credentials/token, never through a code a person approves")

var (
	// ErrTokenLoginsOff is VerifyServiceToken's refusal on a broker that trusts no Kubernetes token
	// issuer (BROKER_K8S_OIDC_ISSUER unset) and no Google VM audience (BROKER_GOOGLE_OIDC_AUDIENCE
	// unset).
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

// ServiceAccount is a verified token whose identity is listed to log in as a service: the token's
// claims (a VM's subject is its runtime id, enroll.VMClaims.RuntimeID), the service it logs in as,
// and the host its credential names.
type ServiceAccount struct {
	enroll.AccountClaims
	Service string
	// Host is a Kubernetes account's <namespace>/<name>, or a VM's <project>/<zone>/<instance name>.
	Host string
}

// TokenCredential is what a token login issued: the launcher credential's id and its expiry.
type TokenCredential struct {
	ID        string
	ExpiresAt time.Time
}

// VerifyServiceToken verifies a service's token and maps it to the service it logs in as. A token
// whose unverified iss claim names the VM verifier's issuer (s.Enroll.VM, Google's, for a Compute
// Engine VM's identity token) goes to that verifier alone, and its account must be bound by
// BROKER_VM_SERVICES from the right project and instance name (enroll.Service.VMService); every
// other token goes to the Kubernetes verifier alone (s.Enroll.Pod, the one pod enrollment uses),
// exactly as before there was a second, and its subject must be in BROKER_SERVICE_LOGINS. So no
// token makes the other issuer's verifier fetch its keys. On a broker with no Kubernetes issuer
// every token goes to the VM verifier. Either token is held to MaxTokenLoginLifetime from its
// issue. AuthenticateLauncher reads the same settings for every credential a login issues.
// Refusals: ErrTokenLoginsOff, ErrTokenExpired, ErrTokenInvalid, ErrTokenTooLongLived and
// ErrAccountNotListed; none repeats the token.
func (s *Service) VerifyServiceToken(ctx context.Context, token string) (ServiceAccount, error) {
	if vm := s.Enroll.VM; vm != nil && (s.Enroll.Pod == nil || vm.Issues(token)) {
		claims, err := vm.VerifyVM(ctx, token)
		if err != nil {
			return ServiceAccount{}, tokenRefusal(err)
		}
		return s.verifyVM(claims)
	}
	if s.Enroll.Pod == nil {
		return ServiceAccount{}, ErrTokenLoginsOff
	}
	claims, err := s.Enroll.Pod.VerifyAccount(ctx, token)
	if err != nil {
		return ServiceAccount{}, tokenRefusal(err)
	}
	return s.verifyKubernetes(claims)
}

// tokenRefusal is a token that did not verify: ErrTokenExpired for one past its expiry, else
// ErrTokenInvalid, each naming why.
func tokenRefusal(err error) error {
	if errors.Is(err, oidc.ErrExpired) {
		return fmt.Errorf("%w (%v)", ErrTokenExpired, err)
	}
	return fmt.Errorf("%w (%v)", ErrTokenInvalid, err)
}

// checkLifetime refuses a verified token that names no issue time, or lives longer than
// MaxTokenLoginLifetime from it.
func (s *Service) checkLifetime(claims enroll.AccountClaims) error {
	if claims.IssuedAt.IsZero() {
		return fmt.Errorf("%w: it names no issue time (iat)", ErrTokenTooLongLived)
	}
	if lives := claims.Expiry.Sub(claims.IssuedAt); lives > MaxTokenLoginLifetime+s.Skew {
		return fmt.Errorf("%w: it lives %s", ErrTokenTooLongLived, lives)
	}
	return nil
}

// verifyKubernetes maps a verified ServiceAccount token through BROKER_SERVICE_LOGINS.
func (s *Service) verifyKubernetes(claims enroll.AccountClaims) (ServiceAccount, error) {
	if err := s.checkLifetime(claims); err != nil {
		return ServiceAccount{}, err
	}
	service, listed := s.Enroll.ServiceLogins[claims.Subject]
	if !listed {
		return ServiceAccount{}, fmt.Errorf("%w: %s is not in BROKER_SERVICE_LOGINS", ErrAccountNotListed, claims.Subject)
	}
	namespace, name, _ := strings.Cut(strings.TrimPrefix(claims.Subject, "system:serviceaccount:"), ":")
	return ServiceAccount{AccountClaims: claims, Service: service, Host: namespace + "/" + name}, nil
}

// verifyVM maps a verified VM identity token to its service (enroll.Service.VMService: its
// account, project and instance name), refusing any other VM ErrAccountNotListed. The login's
// subject is the VM's runtime id, so each VM of the fleet holds a credential of its own.
func (s *Service) verifyVM(vm enroll.VMClaims) (ServiceAccount, error) {
	claims := enroll.AccountClaims{Subject: vm.RuntimeID(), Expiry: vm.Expiry, IssuedAt: vm.IssuedAt}
	if err := s.checkLifetime(claims); err != nil {
		return ServiceAccount{}, err
	}
	service, err := s.Enroll.VMService(vm)
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("%w: %v", ErrAccountNotListed, err)
	}
	return ServiceAccount{AccountClaims: claims, Service: service, Host: vm.Project + "/" + vm.Zone + "/" + vm.Instance}, nil
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
