package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

type githubInstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type githubMintedInstallations struct {
	Installations map[string]githubMintedInstallation `json:"installations"`
}

type githubMintedInstallation struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Mint returns a token and expiry for every installation of the App in value that is not
// suspended. The value is the JSON object stored in the secret, never part of an error. Every
// error wraps one of ErrMintStoredValue, ErrMintRefused or ErrMintUpstream.
func (m GitHubApp) Mint(ctx context.Context, value string) (string, error) {
	var secret githubAppSecret
	// encoding/json's syntax errors quote the offending byte, so this decode and the token's do
	// not wrap their errors: the stored key and GitHub's token are both secrets.
	if json.Unmarshal([]byte(value), &secret) != nil {
		return "", fmt.Errorf("%w: decode GitHub App secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if !validGitHubAppID(secret.AppID) || secret.PrivateKey == "" {
		return "", fmt.Errorf("%w: GitHub App secret needs an app_id and private_key", ErrMintStoredValue)
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
	minted := githubMintedInstallations{Installations: make(map[string]githubMintedInstallation, len(installations))}
	for _, installation := range installations {
		if installation.SuspendedAt != nil {
			continue
		}
		owner := installation.Account.Login
		if owner == "" {
			owner = installation.Account.Slug
		}
		if owner == "" {
			return "", fmt.Errorf("%w: github app %s installation %d names no account", ErrMintUpstream, secret.AppID, installation.ID)
		}
		token, err := m.installationToken(ctx, jwt, installation.ID)
		if err != nil {
			return "", err
		}
		minted.Installations[owner] = githubMintedInstallation{
			InstallationID: installation.ID,
			Token:          token.Token,
			ExpiresAt:      token.ExpiresAt,
		}
	}
	if len(minted.Installations) == 0 {
		return "", fmt.Errorf("%w: github app %s has no installation", ErrMintRefused, secret.AppID)
	}
	encoded, err := json.Marshal(minted)
	if err != nil {
		return "", fmt.Errorf("%w: encode GitHub App installations: %w", ErrMintUpstream, err)
	}
	return string(encoded), nil
}

func validGitHubAppID(id string) bool {
	if id == "" {
		return false
	}
	for _, character := range id {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (m GitHubApp) base() string {
	if base := strings.TrimSuffix(m.Base, "/"); base != "" {
		return base
	}
	return githubAPIBase
}

// githubRequest is a call to GitHub's App API, authenticated as the App.
func githubRequest(ctx context.Context, method, target, jwt string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	return request, nil
}

func (m GitHubApp) installations(ctx context.Context, jwt string) ([]githubInstallation, error) {
	const what = "list GitHub App installations"
	var installations []githubInstallation
	for page := 1; ; page++ {
		request, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations?per_page=100&page=%d", m.base(), page), jwt)
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

func (m GitHubApp) installationToken(ctx context.Context, jwt string, installationID int64) (githubInstallationToken, error) {
	const what = "mint GitHub App installation token"
	request, err := githubRequest(ctx, http.MethodPost, m.base()+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt)
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
