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
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

const (
	// DefaultSecurityTokenServiceURL is Google's own security token service, where an external
	// credential is traded for a federated access token (RFC 8693 again, Google's spelling of
	// it: a JSON body with camelCase members rather than a form).
	DefaultSecurityTokenServiceURL = "https://sts.googleapis.com/v1/token"
	// DefaultIAMCredentialsURL is Google's own IAM Credentials service, whose generateIdToken
	// mints the identity token the broker reads.
	DefaultIAMCredentialsURL = "https://iamcredentials.googleapis.com"
	// poolGrantType is the grant the security token service takes, RFC 8693's own name.
	poolGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	// poolRequestedTokenType is what the security token service answers: a federated access
	// token, spendable at IAM Credentials and nowhere else this client asks.
	poolRequestedTokenType = oauthwire.SubjectTokenTypeAccessToken
	// poolScope is the one scope a federated access token is asked for; IAM Credentials takes
	// no narrower one.
	poolScope = "https://www.googleapis.com/auth/cloud-platform"
	// poolTimeout bounds each of the two calls a pool mint makes.
	poolTimeout = 20 * time.Second
)

// PoolIdentity is the identity of a machine with no Google service account of its own: an
// external credential — the subject this deployment already holds, a projected Kubernetes token
// among them — traded at Google's security token service for a federated access token, which
// IAM Credentials' generateIdToken then spends to mint the Google identity token the broker
// reads. includeEmail is set on that mint, because the broker looks a machine up by the token's
// `email` claim and a token minted without it names no account at all.
//
// A token minted this way carries no google.compute_engine claims: it was minted off an
// instance. An exchange entry that pins a project or an instance name refuses it, and one that
// pins neither takes it, which is what the broker's own settings say.
type PoolIdentity struct {
	// Subject is the external credential the pool federates: the identity this machine already
	// holds, such as a projected Kubernetes service-account token minted for Audience.
	Subject Source
	// Audience is the workload identity pool provider the external credential is presented to,
	// the full resource name Google spells
	// //iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>.
	Audience string
	// ServiceAccount is the managed identity generateIdToken is called on: the service account
	// the pool lets this credential act as, by its email address or its unique id.
	ServiceAccount string
	// TokenAudience is the `aud` of the identity token generateIdToken mints — the client id
	// the broker checks against BROKER_GOOGLE_OIDC_AUDIENCE.
	TokenAudience string
	// SecurityTokenServiceURL and IAMCredentialsURL are Google's own services unless a dev
	// stack or a test names its own stand-ins. IAMCredentialsURL is an origin; the resource
	// path is this client's to build, as Google's own libraries build it.
	SecurityTokenServiceURL string
	IAMCredentialsURL       string
	// HTTP is the client both calls go through; nil is http.DefaultClient.
	HTTP *http.Client
}

func (p *PoolIdentity) Describe() string {
	return "a Google identity token for " + p.TokenAudience + " minted through the workload identity pool " + p.Audience + " as " + p.ServiceAccount
}

// Token trades the external credential for a federated access token and spends it on one
// generateIdToken, answering the identity token that mint produced. Every error names the call
// that failed and never a token.
func (p *PoolIdentity) Token(ctx context.Context) (string, string, error) {
	subject, subjectType, err := p.Subject.Token(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read the credential the workload identity pool federates: %w", err)
	}
	federated, err := p.federate(ctx, subject, subjectType)
	if err != nil {
		return "", "", err
	}
	token, err := p.generateIDToken(ctx, federated)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

// federate is the security token service's half: the external credential for a federated access
// token that acts as the pool's own principal.
func (p *PoolIdentity) federate(ctx context.Context, subject, subjectType string) (string, error) {
	endpoint := p.SecurityTokenServiceURL
	if endpoint == "" {
		endpoint = DefaultSecurityTokenServiceURL
	}
	body, err := json.Marshal(map[string]string{
		"audience":           p.Audience,
		"grantType":          poolGrantType,
		"requestedTokenType": poolRequestedTokenType,
		"scope":              poolScope,
		"subjectTokenType":   poolSubjectTokenType(subjectType),
		"subjectToken":       subject,
	})
	if err != nil {
		return "", fmt.Errorf("build the security token service request: %w", err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := p.call(ctx, endpoint, "", body, &answer); err != nil {
		return "", fmt.Errorf("trade this machine's credential at the security token service %s: %w", endpoint, err)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the security token service %s answered no access_token", endpoint)
	}
	return answer.AccessToken, nil
}

// generateIDToken is IAM Credentials' half: the federated access token spent on one
// generateIdToken for the managed identity, with includeEmail set.
func (p *PoolIdentity) generateIDToken(ctx context.Context, federated string) (string, error) {
	origin := p.IAMCredentialsURL
	if origin == "" {
		origin = DefaultIAMCredentialsURL
	}
	endpoint := strings.TrimSuffix(origin, "/") + "/v1/projects/-/serviceAccounts/" +
		url.PathEscape(p.ServiceAccount) + ":generateIdToken"
	body, err := json.Marshal(map[string]any{"audience": p.TokenAudience, "includeEmail": true})
	if err != nil {
		return "", fmt.Errorf("build the generateIdToken request: %w", err)
	}
	var answer struct {
		Token string `json:"token"`
	}
	if err := p.call(ctx, endpoint, federated, body, &answer); err != nil {
		return "", fmt.Errorf("mint this machine's identity token at %s: %w", endpoint, err)
	}
	if answer.Token == "" {
		return "", fmt.Errorf("generateIdToken at %s answered no token", endpoint)
	}
	return answer.Token, nil
}

// call posts body to endpoint, with bearer on the Authorization header when it is not empty, and
// decodes the answer into out. A non-2xx answer is an error naming the status and the body
// Google's services put their reason in, which carries no credential of this machine's.
func (p *PoolIdentity) call(ctx context.Context, endpoint, bearer string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, poolTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, out)
}

// poolSubjectTokenType is the subjectTokenType the security token service reads an external
// credential under. A Kubernetes projected token is a JWT there, whatever name the broker's own
// exchange knows it by, since the pool provider that verifies it is an OIDC provider and not
// this broker.
func poolSubjectTokenType(brokerType string) string {
	if brokerType == oauthwire.SubjectTokenTypeKubernetes {
		return "urn:ietf:params:oauth:token-type:jwt"
	}
	return brokerType
}
