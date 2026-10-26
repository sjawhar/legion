package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/githubappjwt"
)

const githubAPIBase = "https://api.github.com"

// GitHubApp mints installation tokens from the App id and private key in a github-app secret.
// Base, HTTP and Now let tests replace GitHub, its transport and the clock.
type GitHubApp struct {
	Base string
	HTTP *http.Client
	Now  func() time.Time
}

type githubAppSecret struct {
	AppID      string `json:"app_id"`
	PrivateKey string `json:"private_key"`
}

type githubInstallation struct {
	ID int64 `json:"id"`
	// Account is the user or organization the App is installed on, named by Login, or the
	// enterprise, which GitHub names by Slug and gives no login.
	Account struct {
		Login string `json:"login"`
		Slug  string `json:"slug"`
	} `json:"account"`
	// SuspendedAt is set while the App is suspended on the installation's account, where GitHub
	// refuses its token, so Mint leaves the installation out rather than failing every owner.
	SuspendedAt *time.Time `json:"suspended_at"`
}

// githubInstallationToken is GitHub's answer to a token request: the token, its expiry, and what
// GitHub says the token holds, which Mint checks against a narrowing (GitHubNarrowing.heldBy).
type githubInstallationToken struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		Name string `json:"name"`
	} `json:"repositories"`
}

type githubMintedInstallations struct {
	Installations map[string]githubMintedInstallation `json:"installations"`
}

type githubMintedInstallation struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// githubAccessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body for a narrowed
// token: the repositories it reaches and the permissions it holds, each left out when the
// narrowing does not name it.
type githubAccessTokensBody struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// readOnlyPermissions are the permissions a read-only narrowing asks GitHub for, and the most a
// token minted for one may hold.
var readOnlyPermissions = map[string]string{"contents": "read", "metadata": "read"}

// narrowsToken reports whether n narrows the token GitHub mints, not only which installation it is
// minted for: whether it sends GitHub an access_tokens body.
func (n *GitHubNarrowing) narrowsToken() bool {
	return n != nil && (len(n.Repositories) > 0 || n.ReadOnly)
}

// accessTokensBody is narrowing as GitHub's access_tokens body, nil for a narrowing that names
// neither repositories nor read-only (it narrows to its owner alone) and for none, whose tokens
// are asked for with no body, as before narrowing existed.
func (n *GitHubNarrowing) accessTokensBody() ([]byte, error) {
	if !n.narrowsToken() {
		return nil, nil
	}
	body := githubAccessTokensBody{Repositories: n.Repositories}
	if n.ReadOnly {
		body.Permissions = readOnlyPermissions
	}
	return json.Marshal(body)
}

// refusal is err, GitHub's answer to the token request for owner, unless it is the 422 GitHub
// gives a narrowed token it cannot mint: that is the caller's narrowing (ErrMintNarrowingRefused),
// named by what it likely got wrong, since GitHub's own text is never quoted. A 422 to a request
// that sent no body narrowed nothing, and stays the App's; a 422 that is a rate limit is no verdict
// on the narrowing, and stays the upstream's.
func (n *GitHubNarrowing) refusal(err error, appID, owner string) error {
	var status *mintStatusError
	if !n.narrowsToken() || !errors.As(err, &status) || status.code != http.StatusUnprocessableEntity || status.rateLimited {
		return err
	}
	var causes []string
	if len(n.Repositories) > 0 {
		causes = append(causes, fmt.Sprintf("a repository it names (%s) that the installation does not reach, such as a misspelled name", strings.Join(n.Repositories, ", ")))
	}
	if n.ReadOnly {
		causes = append(causes, "contents: read, which the App does not hold")
	}
	return fmt.Errorf("%w: github app %s: GitHub answered HTTP 422 to the narrowed token for %s: likely %s; the caller fixes the narrowing", ErrMintNarrowingRefused, appID, owner, strings.Join(causes, ", or "))
}

// heldBy checks that token, as GitHub describes it, holds no more than n asked: for repositories, a
// selection of named repositories, each one n names (GitHub names them in its own case); for
// read-only, named permissions, each at most readOnlyPermissions'. GitHub applies the
// access_tokens body and refuses what it cannot grant, so a token past it means something between
// the broker and GitHub dropped or widened the body; refusing it keeps the narrowing the release
// answers with true by construction. Its errors name the rule the answer failed and never a value
// the answer holds, which an upstream that is not GitHub can fill with what the request carried. A
// token not narrowed is not checked.
func (n *GitHubNarrowing) heldBy(token githubInstallationToken) error {
	if !n.narrowsToken() {
		return nil
	}
	if len(n.Repositories) > 0 {
		if token.RepositorySelection != "selected" {
			return errors.New(`GitHub's token has a repository selection other than "selected", so it may reach more than the narrowing names`)
		}
		if len(token.Repositories) == 0 {
			return errors.New("GitHub's token names no repositories, so it may reach more than the narrowing names")
		}
		for _, repository := range token.Repositories {
			if !slices.ContainsFunc(n.Repositories, func(name string) bool { return strings.EqualFold(name, repository.Name) }) {
				return errors.New("GitHub's token reaches a repository the narrowing does not name")
			}
		}
	}
	if n.ReadOnly {
		if len(token.Permissions) == 0 {
			return errors.New("GitHub's token names no permissions, so it may hold more than read access")
		}
		for _, permission := range slices.Sorted(maps.Keys(token.Permissions)) {
			if _, asked := readOnlyPermissions[permission]; !asked {
				return errors.New("GitHub's token holds a permission past contents and metadata")
			}
			if token.Permissions[permission] != readOnlyPermissions[permission] {
				return errors.New("GitHub's token holds more than read access to contents or metadata")
			}
		}
	}
	return nil
}

// revokeTimeout bounds the revoke of a token Mint minted and will not release.
const revokeTimeout = 10 * time.Second

// revokedWiderMessage is logged, at WARN, when GitHub does not confirm the revoke of a narrowed
// token it minted wider than asked, followed by app=, installation=, expires_at= (when the token
// lapses at GitHub without the revoke) and error=, which never holds the token.
const revokedWiderMessage = "broker: GitHub did not revoke a narrowed token it minted wider than asked; it lapses at its expiry"

// revokeInstallationToken asks GitHub to revoke token, as the token itself
// (DELETE /installation/token), for a token Mint minted and will not release. It runs to its own
// bound even once ctx ends, since GitHub honours an unreleased token for an hour otherwise.
func (m GitHubApp) revokeInstallationToken(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	const what = "revoke GitHub App installation token"
	request, err := githubRequest(ctx, http.MethodDelete, m.base()+"/installation/token", token, nil)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
	}
	_, err = mintCall(m.HTTP, request, what, "GitHub", http.StatusNoContent)
	return err
}

// Mint returns a token and expiry for every installation of the App in req.Value that is not
// suspended and whose account req.GitHubOwners lists, compared without regard to case. It leaves
// out an installation on any other account without asking GitHub for its token: a public App can
// be installed by anyone, and a release must not carry a token for an outsider's account. It also
// leaves out a record with no positive installation id, which names no installation a token can be
// asked for. With req.GitHubNarrowing it mints for the narrowing's owner alone, which
// req.GitHubOwners must list (it refuses, asking GitHub nothing, otherwise), sending the narrowing
// as GitHub's access_tokens body, and releases the token only when GitHub's answer holds no more
// than the narrowing asked (heldBy); a token it refuses so it revokes at GitHub first. The value is
// the JSON object stored in the secret, never part of an error. Every error wraps one of
// ErrMintStoredValue, ErrMintRefused, ErrMintNarrowingRefused or ErrMintUpstream.
func (m GitHubApp) Mint(ctx context.Context, req MintRequest) (string, error) {
	var secret githubAppSecret
	// encoding/json's syntax errors quote the offending byte, so this decode and the token's do
	// not wrap their errors: the stored key and GitHub's token are both secrets.
	if json.Unmarshal([]byte(req.Value), &secret) != nil {
		return "", fmt.Errorf("%w: decode GitHub App secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if !validGitHubAppID(secret.AppID) || secret.PrivateKey == "" {
		return "", fmt.Errorf("%w: GitHub App secret needs an app_id and private_key", ErrMintStoredValue)
	}
	owners := req.GitHubOwners
	narrowing := req.GitHubNarrowing
	if narrowing != nil {
		// requests.Machine.Values refuses an unlisted owner before any minter runs; this is the
		// second barrier.
		owner := strings.ToLower(narrowing.Owner)
		if !slices.Contains(owners, owner) {
			return "", fmt.Errorf("%w: github app %s: %s is not an account its github-owners tag lists", ErrMintNarrowingRefused, secret.AppID, narrowing.Owner)
		}
		owners = []string{owner}
	}
	body, err := narrowing.accessTokensBody()
	if err != nil {
		return "", fmt.Errorf("%w: encode the narrowing as GitHub's access_tokens body: %w", ErrMintUpstream, err)
	}
	key, err := githubappjwt.ParseKey(secret.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("%w: parse GitHub App private key: %w", ErrMintStoredValue, err)
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	jwt, err := githubappjwt.Sign(secret.AppID, key, now())
	if err != nil {
		return "", fmt.Errorf("%w: sign GitHub App JWT: %w", ErrMintStoredValue, err)
	}
	installations, err := m.installations(ctx, jwt)
	if err != nil {
		return "", err
	}
	minted := githubMintedInstallations{Installations: make(map[string]githubMintedInstallation, len(owners))}
	for _, installation := range installations {
		// JSON decoding leaves a missing or null id 0: no installation GitHub mints for, so the
		// record is left out rather than failing the release before a valid one is reached.
		if installation.SuspendedAt != nil || installation.ID <= 0 {
			continue
		}
		// An account with neither a login nor a slug names nothing github-owners can list, so the
		// owners filter leaves it out like any unlisted account.
		owner := cmp.Or(installation.Account.Login, installation.Account.Slug)
		if !slices.Contains(owners, strings.ToLower(owner)) {
			continue
		}
		token, err := m.installationToken(ctx, jwt, installation.ID, body)
		if err != nil {
			return "", narrowing.refusal(err, secret.AppID, owner)
		}
		if err := narrowing.heldBy(token); err != nil {
			if revokeErr := m.revokeInstallationToken(ctx, token.Token); revokeErr != nil {
				slog.Warn(revokedWiderMessage, "app", secret.AppID, "installation", installation.ID, "expires_at", token.ExpiresAt, "error", revokeErr)
			}
			return "", fmt.Errorf("%w: github app %s: installation %d: %w", ErrMintUpstream, secret.AppID, installation.ID, err)
		}
		minted.Installations[owner] = githubMintedInstallation{
			InstallationID: installation.ID,
			Token:          token.Token,
			ExpiresAt:      token.ExpiresAt,
		}
	}
	if len(minted.Installations) == 0 {
		if narrowing != nil {
			return "", fmt.Errorf("%w: github app %s has no installation on %s", ErrMintRefused, secret.AppID, narrowing.Owner)
		}
		return "", fmt.Errorf("%w: github app %s has no installation on an account its github-owners tag lists", ErrMintRefused, secret.AppID)
	}
	encoded, err := json.Marshal(minted)
	if err != nil {
		return "", fmt.Errorf("%w: encode GitHub App installations: %w", ErrMintUpstream, err)
	}
	return string(encoded), nil
}

// validGitHubAppID reports whether id is a non-empty run of ASCII digits.
func validGitHubAppID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool { return r < '0' || r > '9' })
}

func (m GitHubApp) base() string {
	return cmp.Or(strings.TrimSuffix(m.Base, "/"), githubAPIBase)
}

// githubRequest is a call to GitHub's API with bearer as its credential (the App's JWT, or an
// installation token revoking itself), carrying body as JSON when it is not nil.
func githubRequest(ctx context.Context, method, target, bearer string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (m GitHubApp) installations(ctx context.Context, jwt string) ([]githubInstallation, error) {
	const what = "list GitHub App installations"
	var installations []githubInstallation
	for page := 1; ; page++ {
		request, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations?per_page=100&page=%d", m.base(), page), jwt, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
		}
		payload, err := mintCall(m.HTTP, request, what, "GitHub", http.StatusOK)
		if err != nil {
			return nil, err
		}
		var entries []githubInstallation
		if json.Unmarshal(payload, &entries) != nil {
			return nil, fmt.Errorf("%w: decode GitHub App installations: not a JSON array of installations", ErrMintUpstream)
		}
		installations = append(installations, entries...)
		if len(entries) < 100 {
			return installations, nil
		}
	}
}

// installationToken asks GitHub for installationID's token, sending body, GitHub's access_tokens
// body, when it is not nil.
func (m GitHubApp) installationToken(ctx context.Context, jwt string, installationID int64, body []byte) (githubInstallationToken, error) {
	const what = "mint GitHub App installation token"
	request, err := githubRequest(ctx, http.MethodPost, m.base()+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt, body)
	if err != nil {
		return githubInstallationToken{}, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
	}
	payload, err := mintCall(m.HTTP, request, what, "GitHub", http.StatusCreated)
	if err != nil {
		return githubInstallationToken{}, err
	}
	var token githubInstallationToken
	if json.Unmarshal(payload, &token) != nil {
		return githubInstallationToken{}, fmt.Errorf("%w: decode GitHub App installation token: not a JSON object of its fields", ErrMintUpstream)
	}
	if token.Token == "" || token.ExpiresAt.IsZero() {
		return githubInstallationToken{}, fmt.Errorf("%w: GitHub returned an incomplete installation token", ErrMintUpstream)
	}
	return token, nil
}
