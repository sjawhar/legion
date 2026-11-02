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

// Source is one identity the exchange takes as a subject or an actor token: the token itself,
// read afresh every time it is asked for, and the RFC 8693 type it is presented under. A renewal
// is a fresh subject token traded again — the grant issues no refresh token — so a source is
// asked once per trade and must answer the token of that moment: a pod's projected token is
// rotated in place by its kubelet, and a machine's Google identity token is minted for an hour.
//
// Name is what a refusal calls the source, for a message a person reads. It names the file, host
// or account the token comes from and never the token.
type Source interface {
	Token(ctx context.Context) (string, error)
	TokenType() string
	Name() string
}

// FileSource is a token a file holds: a pod's projected Kubernetes service-account token, which
// its runtime mounts and its kubelet rewrites in place long before it expires. The file is read
// again for every trade, so each trade sends the token of that moment.
type FileSource struct {
	// Path is the file the token is read from.
	Path string
	// Type is the token's RFC 8693 type, and is the Kubernetes service-account type when empty:
	// a file is how a pod presents that one, and a caller that keeps another kind of token in a
	// file says which it is.
	Type string
}

func (s *FileSource) Token(_ context.Context) (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read the subject token at %s: %w", s.Path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the subject token file %s is empty", s.Path)
	}
	return token, nil
}

func (s *FileSource) TokenType() string {
	if s.Type == "" {
		return oauthwire.SubjectTokenTypeKubernetes
	}
	return s.Type
}

func (s *FileSource) Name() string { return "the token file " + s.Path }

// MetadataSource is this machine's own Google identity token, read from the Compute Engine
// metadata server for Audience (internal/googleidentity: format=full, no proxy, GCE_METADATA_HOST
// for a stand-in). It is the subject a Google Cloud VM and a pod on Google Kubernetes Engine
// present, and the actor token a person's sign-in on such a machine carries.
type MetadataSource struct {
	// Audience is the audience the token is minted for, which the broker checks against its own
	// setting: a token minted for another service is not spendable here.
	Audience string
}

func (s *MetadataSource) Token(ctx context.Context) (string, error) {
	return googleidentity.Token(ctx, s.Audience)
}

func (s *MetadataSource) TokenType() string { return oauthwire.SubjectTokenTypeGoogle }

func (s *MetadataSource) Name() string {
	return "this machine's Google identity token from the Compute Engine metadata server"
}

// StaticSource is a token the caller already holds, presented under the type it names: a session
// on a dev box trading the access token its machine login holds, and, in a test, a token minted
// for the exchange by hand.
type StaticSource struct {
	Value string
	Type  string
	What  string
}

func (s *StaticSource) Token(_ context.Context) (string, error) {
	if s.Value == "" {
		return "", fmt.Errorf("%s is empty", s.Name())
	}
	return s.Value, nil
}

func (s *StaticSource) TokenType() string {
	if s.Type == "" {
		return oauthwire.SubjectTokenTypeAccessToken
	}
	return s.Type
}

func (s *StaticSource) Name() string {
	if s.What == "" {
		return "the token the caller holds"
	}
	return s.What
}
