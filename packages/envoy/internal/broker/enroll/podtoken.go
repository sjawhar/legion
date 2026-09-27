// K8sPodVerifier verifies a projected service-account token with the shared internal/oidc
// verifier (issuer, audience, signature, expiry, subject) and then reads the pod UID claim from
// the same, already-verified token — the check Hawk's token broker makes (_is_pod_bound).
package enroll

import (
	"context"
	"fmt"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/oidc"
)

type K8sPodVerifier struct{ Verifier *oidc.Verifier }

func (k K8sPodVerifier) Verify(ctx context.Context, raw string) (PodClaims, error) {
	claims, err := k.Verifier.Verify(ctx, raw)
	if err != nil {
		return PodClaims{}, err
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return PodClaims{}, err
	}
	var private struct {
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	// The signature was verified by oidc.Verifier on this exact token; reading the claims
	// without a second verification is safe here and only here.
	if err := parsed.UnsafeClaimsWithoutVerification(&private); err != nil {
		return PodClaims{}, err
	}
	if private.Kubernetes.Pod.UID == "" {
		return PodClaims{}, fmt.Errorf("projected token is not bound to a pod")
	}
	return PodClaims{Subject: claims.Subject, PodUID: private.Kubernetes.Pod.UID}, nil
}
