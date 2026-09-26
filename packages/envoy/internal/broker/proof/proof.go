package proof

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
)

var ErrInvalid = errors.New("proof invalid")

type Verifier struct {
	Skew   time.Duration
	Lookup func(ctx context.Context, enrollmentID string) (thumbprint string, live bool, err error)
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
}

// Verify returns the enrollment id a proof authenticates. Lookup and Replay errors are returned
// unwrapped (never ErrInvalid) so callers can distinguish an internal failure from an invalid
// proof; every other failure is ErrInvalid wrapped with a reason that never quotes the proof or
// the key.
func (v *Verifier) Verify(ctx context.Context, compact, method, url string, now time.Time) (string, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 {
		return "", fmt.Errorf("%w: not a single ES256 JWS", ErrInvalid)
	}
	header := sig.Signatures[0].Protected
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != typeHeader {
		return "", fmt.Errorf("%w: typ is not %s", ErrInvalid, typeHeader)
	}
	if header.JSONWebKey == nil {
		return "", fmt.Errorf("%w: no embedded key", ErrInvalid)
	}
	pub, ok := header.JSONWebKey.Key.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().Name != "P-256" {
		return "", fmt.Errorf("%w: key is not EC P-256", ErrInvalid)
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		return "", fmt.Errorf("%w: signature", ErrInvalid)
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || c.JTI == "" || c.EnrollmentID == "" {
		return "", fmt.Errorf("%w: claims", ErrInvalid)
	}
	issued := time.Unix(c.IssuedAt, 0)
	if issued.After(now.Add(v.Skew)) || issued.Before(now.Add(-v.Skew)) {
		return "", fmt.Errorf("%w: iat outside %s", ErrInvalid, v.Skew)
	}
	if c.Method != method || c.URL != url {
		return "", fmt.Errorf("%w: htm/htu do not match the request", ErrInvalid)
	}
	thumbprint, live, err := v.Lookup(ctx, c.EnrollmentID)
	if err != nil {
		return "", err
	}
	if !live {
		return "", fmt.Errorf("%w: enrollment is not live", ErrInvalid)
	}
	presented, err := thumbprintOf(*header.JSONWebKey)
	if err != nil {
		return "", fmt.Errorf("%w: thumbprint", ErrInvalid)
	}
	if presented != thumbprint {
		return "", fmt.Errorf("%w: key is not the enrolled key", ErrInvalid)
	}
	fresh, err := v.Replay(ctx, c.JTI, issued.Add(5*time.Minute))
	if err != nil {
		return "", err
	}
	if !fresh {
		return "", fmt.Errorf("%w: replayed jti", ErrInvalid)
	}
	return c.EnrollmentID, nil
}
