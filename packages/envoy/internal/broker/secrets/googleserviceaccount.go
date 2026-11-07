package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// googleIAMCredentialsURL is the IAM Service Account Credentials API, which mints an access token
// for a service account the caller may impersonate.
const googleIAMCredentialsURL = "https://iamcredentials.googleapis.com"

// googleCloudPlatformScope is the scope the broker holds its own credential at (ownSource) and
// the one a secret that names no scopes mints with: the kind exists so a person's own machine
// acts as a service account of their fleet, and that account's IAM roles, not a narrower scope,
// are what bound it. A secret whose scopes tag names scopes asks Google for those instead
// (MintRequest.GoogleScopes), which is a token narrower than the account's own roles, never a
// wider one.
const googleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// googleServiceAccountLifetime is every generateAccessToken request's lifetime, fixed at Google's
// own maximum for a default-configured project, one hour: MintRequest carries no lifetime
// (see GoogleServiceAccount).
const googleServiceAccountLifetime = "3600s"

// googleServiceAccountEmail is one service account of a project, the only value this kind mints
// from: 6 to 30 of lowercase letters, digits and hyphens, then the project and the fixed
// .iam.gserviceaccount.com domain, the rule Google's own account ids follow. It is anchored and
// admits no comma, space or slash, so a person's address (aditya@theorem.dev), a list of accounts
// and a value that would reach past the request's path all fail it.
var googleServiceAccountEmail = regexp.MustCompile(`^[a-z0-9-]{6,30}@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

// ValidServiceAccountEmail answers whether account is one service account email this minter will
// mint for, the googleServiceAccountEmail rule above. It is exported so the broker's
// configuration can hold every account in BROKER_SERVICE_ACCOUNT_SECRETS to the same rule the
// minter applies, and so that setting can never bind a secret to an address the minter would
// refuse: a binding the minter can never match would refuse every mint of that secret with
// ErrMintNotBound, at mint time, rather than when the deployment was configured.
func ValidServiceAccountEmail(account string) bool {
	return googleServiceAccountEmail.MatchString(account)
}

// GoogleServiceAccount mints a Google access token for the one service account the deployment
// binds to a google-service-account secret (Accounts), by impersonating that account with the
// broker's own Google credential. The secret's stored value names an account as well, and a
// mint happens only when the two are the same account: the broker holds
// roles/iam.serviceAccountTokenCreator on every account it is bound to, so a stored value alone
// choosing the account would let whoever may create a secret store someone else's account in it
// and mint that person's token. The token carries the scopes the secret's scopes tag names
// (MintRequest.GoogleScopes), and the cloud-platform scope alone when it names none. BaseURL and
// HTTP let tests replace the IAM Service Account Credentials endpoint and its transport, as
// GoogleLogin's TokenURL and HTTP do.
//
// The minted token lives one hour, whatever lifetime the grant that released it carries:
// MintRequest holds no lifetime, and Google's generateAccessToken bounds the token itself, so a
// shorter grant stops the broker releasing the token again rather than cutting short the token
// already released. The policy makes that bearable by serving this kind only to the requesters
// the secret's tag names — its owner, and beside them the registered services whose pods a
// person approves one by one — for grants of an hour at most (policy.TagRules).
type GoogleServiceAccount struct {
	BaseURL string
	HTTP    *http.Client
	// Source is the broker's OWN Google credential, the identity that must hold
	// roles/iam.serviceAccountTokenCreator on the account it mints for. Nil means Google's
	// application default credentials, which in production are the pod's Workload Identity,
	// found on the first mint that needs them and kept for every mint after it (ownSource):
	// nothing is resolved when the broker is built, so a test that sets a fixed source runs on a
	// machine with no Google credential at all.
	Source oauth2.TokenSource

	// Accounts is the deployment's binding of each google-service-account secret to the one
	// account it may mint for, keyed by the secret's policy name as a session asks for it and
	// MintRequest.Name carries it (DEV_ADITYA, GCP_ADMIN), each value that secret's one account
	// email. The broker fills it from BROKER_SERVICE_ACCOUNT_SECRETS. A nil or empty map binds
	// nothing, so every mint of this kind is refused with ErrMintNotBound, which is what a
	// deployment that has not set the variable gets; that is deliberate, since the stored value
	// alone may not choose the account a broker with Token Creator on it impersonates.
	Accounts map[string]string

	// mu guards resolved: two releases can mint at once, and only one of them resolves.
	mu sync.Mutex
	// resolved is the application default credentials as the first mint that needed them found
	// them. It is kept so no later mint re-reads the credentials and takes a token round trip of
	// its own, and it is written only once a resolution has succeeded, so a transient failure to
	// find them never becomes permanent.
	resolved oauth2.TokenSource
}

// ownSource is the broker's own credential for one mint: Source whenever a caller set one, and
// otherwise the application default credentials, resolved by the first mint that needs them and
// kept for the rest. Its refusal is ErrMintUpstream, word for word what resolving inline
// answered.
func (m *GoogleServiceAccount) ownSource(ctx context.Context) (oauth2.TokenSource, error) {
	if m.Source != nil {
		return m.Source, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolved != nil {
		return m.resolved, nil
	}
	// The default credentials are found with a context detached from the request's cancellation
	// (its values are kept): x/oauth2's refresher holds on to the context its source was built
	// with, so a source built from one request's context would fail every refresh once that
	// request ended. Their error is dropped: it names the file or metadata server it tried.
	source, err := google.DefaultTokenSource(context.WithoutCancel(ctx), googleCloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("%w: the broker found no Google credentials of its own", ErrMintUpstream)
	}
	m.resolved = source
	return source, nil
}

type googleServiceAccountSecret struct {
	ServiceAccount string `json:"service_account"`
}

type googleServiceAccountToken struct {
	AccessToken string `json:"accessToken"`
}

// googleServiceAccountRequest is one generateAccessToken body: the scopes the token carries and
// its lifetime. It is marshalled per mint rather than concatenated into a string, because Scope
// comes from the secret's scopes tag and a value that held a quote or a backslash would
// otherwise break the JSON instead of being refused by Google.
type googleServiceAccountRequest struct {
	Scope    []string `json:"scope"`
	Lifetime string   `json:"lifetime"`
}

// Mint returns the bare Google access token for the account the deployment binds to the secret
// req names, as GoogleLogin.Mint returns a login's, so a wrapper that exports it as a gcloud
// access token reads it unchanged. A stored value that is not one service account email, and a
// secret whose binding is missing or names a different account than the stored value, are both
// refused before any credential is resolved and before any call is made. It never puts the
// stored value, the broker's own token, the minted token or Google's answer in an error, and
// every error wraps one of ErrMintStoredValue, ErrMintNotBound, ErrMintRefused or
// ErrMintUpstream.
//
// The receiver is a pointer because a mint that resolves the broker's own credentials keeps
// them on it (ownSource), which a copy would throw away.
func (m *GoogleServiceAccount) Mint(ctx context.Context, req MintRequest) (string, error) {
	const what = "mint Google service account token"
	var secret googleServiceAccountSecret
	// encoding/json's syntax errors quote the offending byte, so the decode's own error is
	// dropped: the stored value is a credential.
	if json.Unmarshal([]byte(req.Value), &secret) != nil {
		return "", fmt.Errorf("%w: decode Google service account secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if !googleServiceAccountEmail.MatchString(secret.ServiceAccount) {
		return "", fmt.Errorf("%w: Google service account secret must name one <id>@<project>.iam.gserviceaccount.com account in service_account", ErrMintStoredValue)
	}
	// The binding is checked before the broker's own credential is resolved and before any call
	// is made: whoever may create a secret may store any account in it, and the broker holds
	// Token Creator on every account it is bound to, so the deployment's binding, not the stored
	// value, is what chooses the account. The bound account is safe to name in an error, since
	// an operator reads it to fix BROKER_SERVICE_ACCOUNT_SECRETS; the stored value is not.
	bound, ok := m.Accounts[req.Name]
	if !ok {
		return "", fmt.Errorf("%w: secret %s: the deployment binds no Google service account to it", ErrMintNotBound, req.Name)
	}
	if bound != secret.ServiceAccount {
		return "", fmt.Errorf("%w: secret %s: the deployment binds it to %s, which its stored value does not name", ErrMintNotBound, req.Name, bound)
	}
	source, err := m.ownSource(ctx)
	if err != nil {
		return "", err
	}
	// A token source's error can quote the credential it read, so only its class is reported.
	own, err := source.Token()
	if err != nil {
		return "", fmt.Errorf("%w: the broker's own Google credentials gave no token", ErrMintUpstream)
	}
	// Only the email is escaped, and the verb is appended after: url.PathEscape would be free to
	// escape the colon of ":generateAccessToken", and Google reads a path whose verb arrived as
	// %3AgenerateAccessToken as a resource name of its own and answers 404, never a token. The
	// escaping is what keeps a stored value from reaching a different resource; the pattern above
	// already admits no slash, so the two agree.
	endpoint := cmp.Or(m.BaseURL, googleIAMCredentialsURL) +
		"/v1/projects/-/serviceAccounts/" + url.PathEscape(secret.ServiceAccount) + ":generateAccessToken"
	// A secret that names no scopes mints at cloud-platform, as every token of this kind did
	// before the scopes tag existed: an empty scope list is not a token with no scope, it is a
	// request Google refuses. Marshalling a struct of a string slice and a string cannot fail,
	// so its error is dropped.
	scopes := req.GoogleScopes
	if len(scopes) == 0 {
		scopes = []string{googleCloudPlatformScope}
	}
	body, _ := json.Marshal(googleServiceAccountRequest{Scope: scopes, Lifetime: googleServiceAccountLifetime})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: create Google service account token request: %w", ErrMintUpstream, err)
	}
	own.SetAuthHeader(request)
	request.Header.Set("Content-Type", "application/json")
	payload, err := mintCall(m.HTTP, request, what, "Google", http.StatusOK, mintResponseLimit)
	if err != nil {
		return "", err
	}
	var token googleServiceAccountToken
	if json.Unmarshal(payload, &token) != nil {
		return "", fmt.Errorf("%w: decode Google service account token response: not a JSON object of its fields", ErrMintUpstream)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("%w: Google returned no access token", ErrMintUpstream)
	}
	return token.AccessToken, nil
}
