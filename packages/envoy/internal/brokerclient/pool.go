// packages/envoy/internal/brokerclient/pool.go

package brokerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

const (
	// DefaultSecurityTokenServiceURL is Google's security token service, which trades a
	// credential a workload identity pool trusts for a Google access token.
	DefaultSecurityTokenServiceURL = "https://sts.googleapis.com/v1/token"
	// DefaultIAMCredentialsURL is Google's IAM Credentials service, which mints an identity
	// token for a service account the caller may impersonate. The name of the account goes in
	// its path.
	DefaultIAMCredentialsURL = "https://iamcredentials.googleapis.com/v1"
	// cloudPlatformScope is the scope the security token service's access token is asked for;
	// IAM Credentials takes no narrower one.
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// jwtTokenType is the RFC 8693 name of a JWT, the type a pool credential is presented under.
	jwtTokenType = "urn:ietf:params:oauth:token-type:jwt"
	// accessTokenType is RFC 8693's name for an access token, which the security token service
	// is asked for.
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // a token type's name, not a token
)

// PoolSource is a Google identity token minted through a workload identity pool, for a machine
// that is not a Compute Engine instance and so has no metadata server to ask: a machine of
// another cloud, or a box whose own credential the pool is told to trust.
//
// It is two calls. The security token service trades Subject's token — the credential the pool's
// provider trusts — for a Google access token of the pool's own principal. IAM Credentials'
// generateIdToken then mints, for the managed identity Identity names, an identity token for
// Audience, with includeEmail set, which is what puts the `email` claim in it: the broker maps
// that claim to a fleet, so a token minted without it names no machine the broker can place and
// is refused.
//
// Nothing here is cached: a trade asks for a token of that moment, as every Source does.
type PoolSource struct {
	// Audience is the audience of the minted identity token, which the broker checks against
	// its own setting.
	Audience string
	// PoolAudience is the workload identity pool provider the credential is presented to, the
	// full resource name the security token service takes as its audience.
	PoolAudience string
	// Identity is the managed identity the token is minted for, as IAM Credentials names an
	// account in its path: the service account's client id, or its email.
	Identity string
	// Subject is the credential the pool's provider trusts, read afresh for every mint.
	Subject Source
	// SubjectTokenType is the RFC 8693 type that credential is presented to the security token
	// service under, and is a JWT when empty.
	SubjectTokenType string
	// SecurityTokenServiceURL and IAMCredentialsURL are the two Google endpoints, which default
	// to DefaultSecurityTokenServiceURL and DefaultIAMCredentialsURL. A test points them at its
	// own stubs; nothing else sets them.
	SecurityTokenServiceURL string
	IAMCredentialsURL       string
	// HTTP is the client both calls go through, and is http.DefaultClient when nil. Neither
	// call carries a DPoP proof: these are Google's endpoints, not the broker's.
	HTTP *http.Client
}

func (s *PoolSource) TokenType() string { return oauthwire.SubjectTokenTypeGoogle }

func (s *PoolSource) Name() string {
	return "a Google identity token minted for " + s.Identity + " through the workload identity pool"
}

// Token mints the identity token: the security token service, then generateIdToken. Every error
// names the call it came from and the identity it was for, never a token.
func (s *PoolSource) Token(ctx context.Context) (string, error) {
	subject, err := s.Subject.Token(ctx)
	if err != nil {
		return "", err
	}
	access, err := s.federate(ctx, subject)
	if err != nil {
		return "", err
	}
	return s.generateIDToken(ctx, access)
}

func (s *PoolSource) httpClient() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}

// federate is the security token service half: the pool credential for a Google access token of
// the pool's principal.
func (s *PoolSource) federate(ctx context.Context, subject string) (string, error) {
	subjectType := s.SubjectTokenType
	if subjectType == "" {
		subjectType = jwtTokenType
	}
	stsURL := s.SecurityTokenServiceURL
	if stsURL == "" {
		stsURL = DefaultSecurityTokenServiceURL
	}
	form := url.Values{
		"grant_type":           {GrantTypeTokenExchange},
		"audience":             {s.PoolAudience},
		"scope":                {cloudPlatformScope},
		"requested_token_type": {accessTokenType},
		"subject_token":        {subject},
		"subject_token_type":   {subjectType},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stsURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.call(req, "the security token service at "+stsURL, &answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the security token service at %s answered no access_token for the pool %s", stsURL, s.PoolAudience)
	}
	return answer.AccessToken, nil
}

// generateIDToken is the IAM Credentials half: an identity token for Identity, carrying its
// email, which is the claim the broker places the machine by.
func (s *PoolSource) generateIDToken(ctx context.Context, access string) (string, error) {
	base := s.IAMCredentialsURL
	if base == "" {
		base = DefaultIAMCredentialsURL
	}
	callURL := strings.TrimSuffix(base, "/") + "/projects/-/serviceAccounts/" + url.PathEscape(s.Identity) + ":generateIdToken"
	body, err := json.Marshal(map[string]any{"audience": s.Audience, "includeEmail": true})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	var answer struct {
		Token string `json:"token"`
	}
	if err := s.call(req, "generateIdToken at "+callURL, &answer); err != nil {
		return "", err
	}
	if answer.Token == "" {
		return "", fmt.Errorf("generateIdToken at %s answered no token for %s", callURL, s.Identity)
	}
	return answer.Token, nil
}

// call sends req and decodes its answer, naming what it asked in every refusal. A non-2xx answer
// carries its first 512 bytes, which is Google's own error object and names no credential of
// ours.
func (s *PoolSource) call(req *http.Request, what string, out any) error {
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s could not be reached: %w", what, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the answer of %s: %w", what, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s answered HTTP %d: %s", what, resp.StatusCode, firstBytes(data, 512))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the answer of %s is not the JSON object it must be: %w", what, err)
	}
	return nil
}

// firstBytes is at most n bytes of data, for a message that quotes a server's own error.
func firstBytes(data []byte, n int) string {
	if len(data) > n {
		return string(data[:n]) + "…"
	}
	return string(data)
}
