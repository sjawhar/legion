package secrets

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
// suspended. The value is the JSON object stored in the secret, never part of an error.
func (m GitHubApp) Mint(ctx context.Context, value string) (string, error) {
	var secret githubAppSecret
	// encoding/json's syntax errors quote the offending byte, so this decode and the token's do
	// not wrap their errors: the stored key and GitHub's token are both secrets.
	if json.Unmarshal([]byte(value), &secret) != nil {
		return "", errors.New("decode GitHub App secret: not a JSON object of its fields")
	}
	if !validGitHubAppID(secret.AppID) || secret.PrivateKey == "" {
		return "", errors.New("GitHub App secret needs an app_id and private_key")
	}
	key, err := parseGitHubAppKey(secret.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("parse GitHub App private key: %w", err)
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	jwt, err := githubAppJWT(secret.AppID, key, now())
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
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
			return "", fmt.Errorf("github app %s installation %d names no account", secret.AppID, installation.ID)
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
		return "", fmt.Errorf("github app %s has no installation", secret.AppID)
	}
	encoded, err := json.Marshal(minted)
	if err != nil {
		return "", fmt.Errorf("encode GitHub App installations: %w", err)
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

func parseGitHubAppKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not a PKCS1 or PKCS8 private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}

func githubAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (m GitHubApp) base() string {
	if base := strings.TrimSuffix(m.Base, "/"); base != "" {
		return base
	}
	return githubAPIBase
}

func (m GitHubApp) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return http.DefaultClient
}

func (m GitHubApp) installations(ctx context.Context, jwt string) ([]githubInstallation, error) {
	base, client := m.base(), m.client()
	var installations []githubInstallation
	for page := 1; ; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations?per_page=100&page=%d", base, page), nil)
		if err != nil {
			return nil, fmt.Errorf("list GitHub App installations: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+jwt)
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("list GitHub App installations: %w", err)
		}
		payload, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read GitHub App installations: %w", readErr)
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list GitHub App installations: GitHub returned %s", response.Status)
		}
		var entries []githubInstallation
		if err := json.Unmarshal(payload, &entries); err != nil {
			return nil, fmt.Errorf("decode GitHub App installations: %w", err)
		}
		installations = append(installations, entries...)
		if len(entries) < 100 {
			return installations, nil
		}
	}
}

func (m GitHubApp) installationToken(ctx context.Context, jwt string, installationID int64) (githubInstallationToken, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base()+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", nil)
	if err != nil {
		return githubInstallationToken{}, fmt.Errorf("mint GitHub App installation token: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+jwt)
	response, err := m.client().Do(request)
	if err != nil {
		return githubInstallationToken{}, fmt.Errorf("mint GitHub App installation token: %w", err)
	}
	payload, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		return githubInstallationToken{}, fmt.Errorf("read GitHub App installation token: %w", readErr)
	}
	if response.StatusCode != http.StatusCreated {
		return githubInstallationToken{}, fmt.Errorf("mint GitHub App installation token: GitHub returned %s", response.Status)
	}
	var token githubInstallationToken
	if json.Unmarshal(payload, &token) != nil {
		return githubInstallationToken{}, errors.New("decode GitHub App installation token: not a JSON object of its fields")
	}
	if token.Token == "" || token.ExpiresAt.IsZero() {
		return githubInstallationToken{}, errors.New("GitHub returned an incomplete installation token")
	}
	return token, nil
}
