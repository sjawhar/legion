package agentsecrets

import (
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

// The daemon's own JWS signers: byte-compatible with the broker's real verifier code
// (packages/envoy/internal/broker/record/sign.go and internal/broker/proof/sign.go) — same
// go-jose construction (EmbedJWK, the typ header, ES256), same claim names, same iat/exp
// relationship. This package holds no import of packages/envoy: the daemon's production binary
// shares no code path with the broker, only the wire contract.

const (
	requestTyp = "agent-secrets-request+jwt"
	proofTyp   = "agent-secrets-proof+jwt"

	// requestLifetimeSeconds bounds how far past iat a request object's exp sits: the broker's
	// record.VerifyRequestObject refuses an exp later than iat+600.
	requestLifetimeSeconds = 600
)

// authorizationDetail is one entry of a request object's RFC 9396 authorization_details, mirrored
// from envoy's record.AuthorizationDetail wire shape. A machine login always sends exactly one
// "launcher_credential" entry, so Actions is never needed here.
type authorizationDetail struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
	Service    string `json:"service,omitempty"`
}

// requestClaims is a credential-request object's JSON payload, mirrored from envoy's
// record.requestClaims. Reason is always empty for a machine login (envoy's own sign.go leaves
// it empty too) and so never appears on the wire.
type requestClaims struct {
	Issuer               string                `json:"iss"`
	Audience             string                `json:"aud"`
	JTI                  string                `json:"jti"`
	IssuedAt             int64                 `json:"iat"`
	Expires              int64                 `json:"exp"`
	AuthorizationDetails []authorizationDetail `json:"authorization_details"`
	Reason               string                `json:"reason,omitempty"`
	LoginHint            string                `json:"login_hint,omitempty"`
}

// proofClaims is a launcher proof's JSON payload, mirrored from envoy's proof.claims. The daemon
// signs only launcher proofs (lid): eid — a session proof's own subject — is never used here, so
// it is omitted from this struct entirely rather than carried as an always-empty field.
type proofClaims struct {
	JTI        string `json:"jti"`
	IssuedAt   int64  `json:"iat"`
	Method     string `json:"htm"`
	URL        string `json:"htu"`
	LauncherID string `json:"lid"`
}

// thumbprint is RFC 7638 over the public JWK, base64url without padding — the value a request
func thumbprint(pub *ecdsa.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// signRequestObject signs a machine-login credential-request object naming host (and, for a
// service credential, service) as the launcher_credential the daemon asks to hold, approved by
// loginHint. Byte-compatible with envoy's record.Sign.
func signRequestObject(key *ecdsa.PrivateKey, audience, host, service, loginHint string, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: requestTyp},
	})
	if err != nil {
		return "", err
	}
	iss, err := thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	iat := now.Unix()
	payload, err := json.Marshal(requestClaims{
		Issuer:   iss,
		Audience: audience,
		JTI:      uuid.NewString(),
		IssuedAt: iat,
		Expires:  iat + requestLifetimeSeconds,
		AuthorizationDetails: []authorizationDetail{
			{Type: "launcher_credential", Identifier: host, Service: service},
		},
		LoginHint: loginHint,
	})
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

// signLauncherProof signs a launcher proof identifying lid (the launcher credential's own id) as
// the key's subject. Byte-compatible with envoy's proof.SignLauncher.
func signLauncherProof(key *ecdsa.PrivateKey, lid, method, url string, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: proofTyp},
	})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(proofClaims{
		JTI:        uuid.NewString(),
		IssuedAt:   now.Unix(),
		Method:     method,
		URL:        url,
		LauncherID: lid,
	})
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}
