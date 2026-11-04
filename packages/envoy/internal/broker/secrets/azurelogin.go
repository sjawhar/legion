package secrets

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
)

// DefaultAzureTokenFile is the broker pod's projected ServiceAccount token for Azure federation.
const DefaultAzureTokenFile = "/var/run/secrets/azure/tokens/azure-identity-token"

// AzureLogin releases the broker's projected token with the identity in an azure-login secret.
// TokenFile names the projected volume, read anew on each release so Kubernetes rotation is used.
// Now lets tests replace the clock. Entra verifies the token when the caller exchanges it;
// the broker trusts its own mounted file and reads the JWT claims only to refuse an expired token.
type AzureLogin struct {
	TokenFile string
	Now       func() time.Time
}

type azureLoginSecret struct {
	TenantID string `json:"tenant_id"`
	ClientID string `json:"client_id"`
}

type azureLoginRelease struct {
	TenantID       string    `json:"tenant_id"`
	ClientID       string    `json:"client_id"`
	FederatedToken string    `json:"federated_token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Mint returns a JSON document for an Azure federated sign-in. A malformed identity is a
// stored-value failure; an unreadable, malformed or expired projected token is an upstream
// failure for the broker's operator. Errors never quote stored fields, token bytes or file paths.
func (m AzureLogin) Mint(_ context.Context, req MintRequest) (string, error) {
	var identity azureLoginSecret
	if json.Unmarshal([]byte(req.Value), &identity) != nil || strings.TrimSpace(identity.TenantID) == "" || strings.TrimSpace(identity.ClientID) == "" {
		return "", fmt.Errorf("%w: Azure login secret must name tenant_id and client_id", ErrMintStoredValue)
	}
	data, err := os.ReadFile(cmp.Or(m.TokenFile, DefaultAzureTokenFile))
	if err != nil {
		return "", fmt.Errorf("%w: read Azure projected token file", ErrMintUpstream)
	}
	token := strings.TrimSpace(string(data))
	if !oidc.LooksLikeJWT(token) {
		return "", fmt.Errorf("%w: Azure projected token is not a JWT", ErrMintUpstream)
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[strings.IndexByte(token, '.')+1 : strings.LastIndexByte(token, '.')])
	if err != nil {
		return "", fmt.Errorf("%w: Azure projected token payload is not base64url", ErrMintUpstream)
	}
	var claims struct {
		Expiry int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Expiry <= 0 {
		return "", fmt.Errorf("%w: Azure projected token must carry an exp claim in seconds", ErrMintUpstream)
	}
	expires := time.Unix(claims.Expiry, 0).UTC()
	now := m.Now
	if now == nil {
		now = time.Now
	}
	if !now().Before(expires) {
		return "", fmt.Errorf("%w: Azure projected token has expired", ErrMintUpstream)
	}
	encoded, err := json.Marshal(azureLoginRelease{
		TenantID: identity.TenantID, ClientID: identity.ClientID,
		FederatedToken: token, ExpiresAt: expires,
	})
	if err != nil {
		return "", fmt.Errorf("%w: Azure projected token expiry cannot be encoded", ErrMintUpstream)
	}
	return string(encoded), nil
}
