package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// MaxTokenLoginLifetime bounds a token-issued launcher credential: it expires with the token that
// proved it, and never more than an hour after the login, however long that token lives.
const MaxTokenLoginLifetime = time.Hour

// errServiceLogsInWithToken is why a device-code login naming a service is refused.
var errServiceLogsInWithToken = errors.New("a service logs in with its own Kubernetes ServiceAccount token (POST /v1/launcher-credentials/token), never through a code a person approves")

var (
	// ErrTokenLoginsOff is TokenLogin's refusal on a broker that trusts no Kubernetes token issuer
	// (BROKER_K8S_OIDC_ISSUER unset).
	ErrTokenLoginsOff = errors.New("this broker trusts no Kubernetes token issuer, so no service logs in with a token")
	// ErrTokenExpired is TokenLogin's refusal of a token past its expiry: the caller logs in again
	// with its current token.
	ErrTokenExpired = errors.New("the ServiceAccount token has expired")
	// ErrTokenInvalid is TokenLogin's refusal of a token that does not verify: another issuer or
	// audience, a bad signature, or a malformed token.
	ErrTokenInvalid = errors.New("the ServiceAccount token does not verify")
	// ErrAccountNotListed is TokenLogin's refusal of a verified token whose ServiceAccount
	// BROKER_SERVICE_LOGINS does not name, or names for another service than the request asks.
	ErrAccountNotListed = errors.New("this ServiceAccount does not log in as this service")
)

// TokenVerifier verifies a projected ServiceAccount token: enroll.K8sPodVerifier, the verifier pod
// enrollment uses, against BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE.
type TokenVerifier interface {
	VerifyAccount(ctx context.Context, raw string) (enroll.AccountClaims, error)
}

// TokenCredential is what a token login issued: the launcher credential's id and its expiry.
type TokenCredential struct {
	ID        string
	ExpiresAt time.Time
}

// TokenLogin logs a service in with its own projected ServiceAccount token, and no person approves
// it. token must verify against the broker's Kubernetes issuer and audience (s.Tokens), and its
// subject must be an account s.ServiceLogins names; compactRequest is a machine request object
// signed by the key the credential is bound to, with exactly one launcher_credential detail naming
// that account's service, and is replay-checked as Login's is. The account holds one launcher
// credential, created at its first login: each login moves it to the request's key and to the
// token's expiry, at most MaxTokenLoginLifetime away, so the previous key stops authenticating at
// once while every pod the account enrolled stays under the same credential. Its host is the
// account's <namespace>/<name>. Each login writes a launcher_credential.token_login audit row with
// the subject, the pod the token is bound to (none for an unbound token) and the new key.
//
// Refusals: ErrTokenLoginsOff, ErrTokenExpired, ErrTokenInvalid, ErrAccountNotListed,
// record.ErrRequestInvalid (a request object that does not verify, names no single
// launcher_credential detail or another service, or repeats a jti), and ErrKeyHoldsLiveCredential
// (a key another live credential holds).
func (s *Service) TokenLogin(ctx context.Context, token, compactRequest string) (TokenCredential, error) {
	if s.Tokens == nil {
		return TokenCredential{}, ErrTokenLoginsOff
	}
	claims, err := s.Tokens.VerifyAccount(ctx, token)
	switch {
	case errors.Is(err, oidc.ErrExpired):
		return TokenCredential{}, fmt.Errorf("%w (%v)", ErrTokenExpired, err)
	case err != nil:
		return TokenCredential{}, fmt.Errorf("%w (%v)", ErrTokenInvalid, err)
	}
	service, listed := s.ServiceLogins[claims.Subject]
	if !listed {
		return TokenCredential{}, fmt.Errorf("%w: %s is not in BROKER_SERVICE_LOGINS", ErrAccountNotListed, claims.Subject)
	}
	now := time.Now()
	obj, err := record.VerifyRequestObject(compactRequest, s.Audience, s.Skew, now)
	if err != nil {
		return TokenCredential{}, err
	}
	if len(obj.Details) != 1 || obj.Details[0].Type != record.KindLauncherCredential {
		return TokenCredential{}, fmt.Errorf("%w: exactly one launcher_credential detail is required", record.ErrRequestInvalid)
	}
	if obj.Service() != service {
		return TokenCredential{}, fmt.Errorf("%w: %s logs in as %q, not %q", ErrAccountNotListed, claims.Subject, service, obj.Service())
	}
	jwk, err := embeddedJWK(compactRequest)
	if err != nil {
		return TokenCredential{}, err
	}
	fresh, err := s.Replay(ctx, obj.JTI, obj.Expires.Add(s.Skew+jtiRetentionMargin))
	if err != nil {
		return TokenCredential{}, err
	}
	if !fresh {
		return TokenCredential{}, fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}

	expires := now.Add(MaxTokenLoginLifetime)
	if claims.Expiry.Before(expires) {
		expires = claims.Expiry
	}
	expires = expires.UTC().Truncate(time.Microsecond)
	namespace, name := accountName(claims.Subject)
	host := namespace + "/" + name
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return TokenCredential{}, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	err = tx.QueryRow(ctx, `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, expires_at)
		values ($1, null, $2, $3, $4, $5, null, $6, $7)
		on conflict (subject) where subject is not null and revoked_at is null
		do update set service=excluded.service, host=excluded.host, key_thumbprint=excluded.key_thumbprint,
			public_jwk=excluded.public_jwk, expires_at=excluded.expires_at
		returning id`,
		uuid.New(), service, host, obj.Thumbprint, []byte(jwk), claims.Subject, expires).Scan(&id)
	if store.IsUniqueViolation(err) {
		return TokenCredential{}, ErrKeyHoldsLiveCredential
	}
	if err != nil {
		return TokenCredential{}, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.token_login',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'service',$3::text,'subject',$4::text,'pod_uid',nullif($5::text,''),
			'key_thumbprint',$6::text,'expires_at',$7::timestamptz)))`,
		record.LauncherActor(id.String()), id.String(), service, claims.Subject, claims.PodUID, obj.Thumbprint, expires); err != nil {
		return TokenCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenCredential{}, err
	}
	return TokenCredential{ID: id.String(), ExpiresAt: expires}, nil
}

// accountName splits a ServiceAccount subject, system:serviceaccount:<namespace>:<name>, which
// BROKER_SERVICE_LOGINS already holds to that form.
func accountName(subject string) (namespace, name string) {
	rest := strings.TrimPrefix(subject, "system:serviceaccount:")
	namespace, name, _ = strings.Cut(rest, ":")
	return namespace, name
}
