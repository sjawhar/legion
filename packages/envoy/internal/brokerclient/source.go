// packages/envoy/internal/brokerclient/source.go

package brokerclient

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// Source is one platform identity this process can prove, read fresh each time it is asked: the
// subject token of the broker's RFC 8693 exchange, and the type the exchange reads it under. The
// grant issues no refresh token, so every renewal asks its Source again — a projected token the
// kubelet rewrote in place, a metadata token minted anew, a pool token traded again — and a
// Source that caches must cache for less than the token it answers lives.
//
// Three implementations are the three identities a machine of this deployment holds. Each is a
// Source so one client serves all three and a caller names which it has in one line.
type Source interface {
	// Token is a subject token to present and the subject_token_type it is presented under, or
	// an error that never reached the broker: an identity this machine could not read is never
	// an empty token for the broker to refuse.
	Token(ctx context.Context) (token string, tokenType string, err error)
	// Describe names the identity in a refusal, without naming the token: the file read, the
	// metadata server asked, or the pool traded at.
	Describe() string
}

// ProjectedToken is the identity of a pod: the projected Kubernetes service-account token its
// runtime mounts, minted for the broker's exchange audience. The kubelet rewrites that file in
// place well before the token expires, so it is read at call time and never once at start.
type ProjectedToken struct {
	// Path is the file the token is read from.
	Path string
}

// NewProjectedToken is the identity of a pod holding its projected service-account token at path.
func NewProjectedToken(path string) *ProjectedToken { return &ProjectedToken{Path: path} }

func (p *ProjectedToken) Token(context.Context) (string, string, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", "", fmt.Errorf("read the projected service-account token %s: %w", p.Path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", "", fmt.Errorf("the projected service-account token %s is empty", p.Path)
	}
	return token, oauthwire.SubjectTokenTypeKubernetes, nil
}

func (p *ProjectedToken) Describe() string {
	return "the projected service-account token " + p.Path
}

// MetadataIdentity is the identity of a machine on Google Cloud: the Google identity token its
// Compute Engine metadata server mints for Audience, asked for with format=full and fetched
// through no proxy (internal/googleidentity, the one reader of that server in this repository).
type MetadataIdentity struct {
	// Audience is what the minted token's `aud` carries, which the broker checks against
	// BROKER_GOOGLE_OIDC_AUDIENCE.
	Audience string
}

// NewMetadataIdentity is the identity of a machine whose metadata server mints its token for
// audience.
func NewMetadataIdentity(audience string) *MetadataIdentity {
	return &MetadataIdentity{Audience: audience}
}

func (m *MetadataIdentity) Token(ctx context.Context) (string, string, error) {
	token, err := googleidentity.Token(ctx, m.Audience)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

func (m *MetadataIdentity) Describe() string {
	return "this machine's Google identity token for " + m.Audience + " (" + googleidentity.MetadataHostVar + " names the metadata server)"
}
