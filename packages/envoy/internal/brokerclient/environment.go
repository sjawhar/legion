// packages/envoy/internal/brokerclient/environment.go

package brokerclient

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// The variables every program that holds one of these identities reads, so `agent-secrets`, its
// helper and `legion controller start` are told the same things the same way, and a deployment
// writes one set of values.
const (
	// BrokerURLVar names the secrets broker, as it does for every other form of agent-secrets.
	BrokerURLVar = "AGENT_SECRETS_URL"
	// IdentityVar says which identity this machine proves: kubernetes, metadata or pool. Unset,
	// this machine has none and nothing trades — which is every deployment that has not turned
	// the exchange on, and is why no program here fails for want of it.
	IdentityVar = "AGENT_SECRETS_MACHINE_IDENTITY"
	// MachineTokenFileVar is the projected Kubernetes service-account token file, which
	// IdentityKubernetes reads and IdentityPool federates.
	MachineTokenFileVar = "AGENT_SECRETS_MACHINE_TOKEN_FILE"
	// MachineAudienceVar is the audience of this machine's Google identity token: the metadata
	// server mints it for that audience, and a pool mint names it as the minted token's own.
	MachineAudienceVar = "AGENT_SECRETS_MACHINE_AUDIENCE"
	// PoolAudienceVar is the workload identity pool provider an external credential is traded
	// at, the full //iam.googleapis.com/projects/… resource name.
	PoolAudienceVar = "AGENT_SECRETS_POOL_AUDIENCE"
	// PoolAccountVar is the managed identity generateIdToken is called on.
	PoolAccountVar = "AGENT_SECRETS_POOL_ACCOUNT"
	// SessionVar is the session name a trade writes under, where the deployment's entry fixes
	// none. Unset, the caller names its own or the entry's stands.
	SessionVar = "AGENT_SECRETS_SESSION"
	// SignInIssuerVar is the OpenID provider a person signs in at, Google's unless a dev stack
	// says otherwise.
	SignInIssuerVar = "AGENT_SECRETS_SIGNIN_ISSUER"
	// SignInClientIDVar and SignInClientSecretFileVar are the device-flow client
	// `agent-secrets login` runs the sign-in as. The secret is in a file, never in a variable:
	// a variable is read by every child of the shell that set it.
	SignInClientIDVar         = "AGENT_SECRETS_SIGNIN_CLIENT_ID"
	SignInClientSecretFileVar = "AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"

	// DefaultSignInIssuer is where every real person signs in.
	DefaultSignInIssuer = "https://accounts.google.com"

	// The values IdentityVar takes.
	IdentityKubernetes = "kubernetes"
	IdentityMetadata   = "metadata"
	IdentityPool       = "pool"
)

// callTimeout bounds one call a client of this package makes to the broker, including the dial.
const callTimeout = 30 * time.Second

// HTTPClient is the client every caller of this package uses unless it has one of its own: a
// timeout on each call, and the default transport.
func HTTPClient() *http.Client { return &http.Client{Timeout: callTimeout} }

// MachineIdentity is the identity IdentityVar names, or nil when it names none, which is a
// machine that trades nothing and is no error. A named identity whose own variables are missing
// is an error naming each of them, since a deployment that asked for one and gave it nothing has
// a mistake in it.
func MachineIdentity(getenv func(string) string) (Source, error) {
	kind := strings.TrimSpace(getenv(IdentityVar))
	switch kind {
	case "":
		return nil, nil
	case IdentityKubernetes:
		path, err := required(getenv, MachineTokenFileVar, kind)
		if err != nil {
			return nil, err
		}
		return NewProjectedToken(path), nil
	case IdentityMetadata:
		audience, err := required(getenv, MachineAudienceVar, kind)
		if err != nil {
			return nil, err
		}
		return NewMetadataIdentity(audience), nil
	case IdentityPool:
		path, err := required(getenv, MachineTokenFileVar, kind)
		if err != nil {
			return nil, err
		}
		audience, err := required(getenv, MachineAudienceVar, kind)
		if err != nil {
			return nil, err
		}
		pool, err := required(getenv, PoolAudienceVar, kind)
		if err != nil {
			return nil, err
		}
		account, err := required(getenv, PoolAccountVar, kind)
		if err != nil {
			return nil, err
		}
		return &PoolIdentity{
			Subject: NewProjectedToken(path), Audience: pool, ServiceAccount: account,
			TokenAudience: audience, HTTP: HTTPClient(),
		}, nil
	default:
		return nil, fmt.Errorf("%s=%q names no identity this machine can prove; it takes %s, %s or %s",
			IdentityVar, kind, IdentityKubernetes, IdentityMetadata, IdentityPool)
	}
}

// SignInFlow is the device-flow client a person signs in with, or nil when the deployment
// configures none. The client secret is read from its file here, so a refusal names the file
// rather than failing at the first sign-in.
func SignInFlow(getenv func(string) string) (*DeviceFlow, error) {
	clientID := strings.TrimSpace(getenv(SignInClientIDVar))
	secretFile := strings.TrimSpace(getenv(SignInClientSecretFileVar))
	if clientID == "" && secretFile == "" {
		return nil, nil
	}
	if clientID == "" || secretFile == "" {
		return nil, fmt.Errorf("a person's sign-in needs both %s and %s; this machine has one of them",
			SignInClientIDVar, SignInClientSecretFileVar)
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, fmt.Errorf("read the sign-in client's secret (%s): %w", SignInClientSecretFileVar, err)
	}
	issuer := strings.TrimSpace(getenv(SignInIssuerVar))
	if issuer == "" {
		issuer = DefaultSignInIssuer
	}
	return &DeviceFlow{
		Issuer: strings.TrimSuffix(issuer, "/"), ClientID: clientID,
		ClientSecret: strings.TrimSpace(string(secret)), HTTP: HTTPClient(),
	}, nil
}

// required is a variable a named identity cannot do without.
func required(getenv func(string) string, name, kind string) (string, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required by %s=%s", name, IdentityVar, kind)
	}
	return value, nil
}
