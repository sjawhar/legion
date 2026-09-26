package proof

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

const typeHeader = "agent-secrets-proof+jwt"

type claims struct {
	JTI          string `json:"jti"`
	IssuedAt     int64  `json:"iat"`
	Method       string `json:"htm"`
	URL          string `json:"htu"`
	EnrollmentID string `json:"eid"`
}

func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Thumbprint is RFC 7638 over the public JWK, base64url without padding — the value the
// launcher enrols and the value every proof is compared against.
func Thumbprint(pub *ecdsa.PublicKey) (string, error) {
	return thumbprintOf(jose.JSONWebKey{Key: pub})
}

func thumbprintOf(jwk jose.JSONWebKey) (string, error) {
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

func Sign(key *ecdsa.PrivateKey, enrollmentID, method, url string, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: typeHeader},
	})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims{JTI: uuid.NewString(), IssuedAt: now.Unix(), Method: method, URL: url, EnrollmentID: enrollmentID})
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}
