package record

import (
	"crypto/ecdsa"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// Sign builds and signs a credential-request object (used by the CLI, the host helper, the
// Legion daemon, and tests). It mirrors proof/sign.go's go-jose usage: iss is the key's
// proof.Thumbprint, and exp is iat+600s.
func Sign(key *ecdsa.PrivateKey, audience string, details []AuthorizationDetail, reason, loginHint string, now time.Time) (string, error) {
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
	payload, err := json.Marshal(requestClaims{
		Issuer:               thumbprint,
		Audience:             audience,
		JTI:                  uuid.NewString(),
		IssuedAt:             iat,
		Expires:              iat + int64(maxRequestLifetime.Seconds()),
		AuthorizationDetails: details,
		Reason:               reason,
		LoginHint:            loginHint,
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
