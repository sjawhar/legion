package record

import (
	"crypto/ecdsa"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// SignOption adds a claim to a request object that most signers do not make. It is an option
// rather than a parameter because only the host helper, which pins the asking process, can make
// the one there is: every other signer (the CLI's own key, a pod, a box, the Legion daemon, the
// tests) signs the same seven arguments it always did.
type SignOption func(*requestClaims)

// WithPeerFacts signs what the kernel answered about the process that asked, as the helper read
// it through the pidfd it had pinned (PeerFacts). The signature proves the enrolled session's
// key made the claim, no more: on a machine session the helper runs, that key is the helper's
// alone and the asking process cannot have written it; PeerFacts' doc comment says where that
// reasoning stops.
func WithPeerFacts(facts PeerFacts) SignOption {
	return func(c *requestClaims) { c.Peer = &facts }
}

// Sign builds and signs a credential-request object (used by the CLI, the host helper, the
// Legion daemon, and tests). It mirrors proof/sign.go's go-jose usage: iss is the key's
// proof.Thumbprint, exp is iat+600s, and lifetimeSeconds is nil when an agent-secret grant lasts
// until its session ends.
func Sign(key *ecdsa.PrivateKey, audience string, details []AuthorizationDetail, reason, loginHint string, lifetimeSeconds *int, now time.Time, opts ...SignOption) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: RequestTyp},
	})
	if err != nil {
		return "", err
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	iat := now.Unix()
	claims := requestClaims{
		Issuer:               thumbprint,
		Audience:             audience,
		JTI:                  uuid.NewString(),
		IssuedAt:             iat,
		Expires:              iat + int64(maxRequestLifetime.Seconds()),
		AuthorizationDetails: details,
		Reason:               reason,
		LoginHint:            loginHint,
		LifetimeSeconds:      lifetimeSeconds,
	}
	for _, opt := range opts {
		opt(&claims)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}
