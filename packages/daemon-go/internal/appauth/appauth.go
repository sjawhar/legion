// Package appauth mints GitHub installation tokens for Legion's two GitHub Apps.
package appauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const refreshWindow = 5 * time.Minute

// AppRole names the GitHub App a Legion role acts as.
type AppRole string

const (
	Implement AppRole = "implement"
	Review    AppRole = "review"
)

// GitIdentity is the bot identity that jj and git must use for work a role performs.
type GitIdentity = runtime.GitIdentity

// Lease is one installation-token lease, including its expiry and the App bot identity.
type Lease struct {
	Token     string
	ExpiresAt time.Time
	Identity  GitIdentity
}

// Tokens mints a GitHub installation token for an App and repository owner.
type Tokens interface {
	Token(ctx context.Context, role AppRole, owner string) (Lease, error)
}

// Options makes a token manager testable. The zero value uses api.github.com, http.DefaultClient,
// and the wall clock.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Manager caches installation tokens by App and owner, and bot identities by App id.
type Manager struct {
	apps    config.GitHubApps
	baseURL string
	client  *http.Client
	now     func() time.Time

	mu               sync.Mutex
	leases           map[string]Lease
	pending          map[string]*pendingLease
	installations    map[AppRole]map[string]string
	identities       map[string]GitIdentity
	pendingIdentities map[string]*pendingIdentity
}

type pendingLease struct {
	done  chan struct{}
	lease Lease
	err   error
}

type pendingIdentity struct {
	done     chan struct{}
	identity GitIdentity
	err      error
}

var _ Tokens = (*Manager)(nil)

// New makes an App token manager. Config.Load resolves command and secret key sources before
// constructing this manager; callers using LoadForValidation must resolve them themselves.
func New(apps config.GitHubApps, options Options) *Manager {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		apps:              apps,
		baseURL:           baseURL,
		client:            client,
		now:               now,
		leases:            map[string]Lease{},
		pending:           map[string]*pendingLease{},
		installations:     map[AppRole]map[string]string{},
		identities:        map[string]GitIdentity{},
		pendingIdentities: map[string]*pendingIdentity{},
	}
}

// Token returns a cached lease until fewer than five minutes remain, otherwise minting a fresh
// installation token. Concurrent requests for the same App and owner share that one exchange.
func (m *Manager) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Lease{}, fmt.Errorf("GitHub App owner must not be empty")
	}
	cacheKey := string(role) + ":" + strings.ToLower(owner)
	now := m.now()

	m.mu.Lock()
	if lease, ok := m.leases[cacheKey]; ok && lease.ExpiresAt.Sub(now) > refreshWindow {
		m.mu.Unlock()
		return lease, nil
	}
	if waiting, ok := m.pending[cacheKey]; ok {
		m.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.lease, waiting.err
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		}
	}
	waiting := &pendingLease{done: make(chan struct{})}
	m.pending[cacheKey] = waiting
	m.mu.Unlock()

	lease, err := m.mint(ctx, role, owner)
	m.mu.Lock()
	if err == nil {
		m.leases[cacheKey] = lease
	}
	waiting.lease, waiting.err = lease, err
	delete(m.pending, cacheKey)
	close(waiting.done)
	m.mu.Unlock()
	return lease, err
}

func (m *Manager) mint(ctx context.Context, role AppRole, owner string) (Lease, error) {
	app, err := m.app(role)
	if err != nil {
		return Lease{}, err
	}
	installation, err := m.installation(ctx, role, app, owner)
	if err != nil {
		return Lease{}, err
	}
	jwt, err := GenerateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return Lease{}, err
	}
	lease, err := m.exchange(ctx, jwt, installation)
	if err != nil {
		return Lease{}, err
	}
	identity, err := m.identity(ctx, app.AppID, jwt, lease.Token)
	if err != nil {
		return Lease{}, err
	}
	lease.Identity = identity
	return lease, nil
}

func (m *Manager) app(role AppRole) (config.GitHubApp, error) {
	switch role {
	case Implement:
		return m.apps.Implement, nil
	case Review:
		return m.apps.Review, nil
	default:
		return config.GitHubApp{}, fmt.Errorf("role_not_configured: %s", role)
	}
}

func (m *Manager) installation(ctx context.Context, role AppRole, app config.GitHubApp, owner string) (string, error) {
	ownerKey := strings.ToLower(owner)
	for configuredOwner, installation := range app.Installations {
		if strings.EqualFold(configuredOwner, owner) && installation != "" {
			return installation, nil
		}
	}
	m.mu.Lock()
	if installations := m.installations[role]; installations != nil {
		if installation := installations[ownerKey]; installation != "" {
			m.mu.Unlock()
			return installation, nil
		}
	}
	m.mu.Unlock()

	found, err := m.discoverInstallations(ctx, app)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.installations[role] == nil {
		m.installations[role] = map[string]string{}
	}
	for discoveredOwner, installation := range found {
		m.installations[role][strings.ToLower(discoveredOwner)] = installation
	}
	installation := m.installations[role][ownerKey]
	m.mu.Unlock()
	if installation == "" {
		return "", fmt.Errorf("github_app_not_installed: %s not installed on %s", role, owner)
	}
	return installation, nil
}

func (m *Manager) discoverInstallations(ctx context.Context, app config.GitHubApp) (map[string]string, error) {
	jwt, err := GenerateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return nil, err
	}
	installations := map[string]string{}
	for page := 1; ; page++ {
		endpoint, err := url.Parse(m.baseURL + "/app/installations")
		if err != nil {
			return nil, fmt.Errorf("GitHub App installation URL: %w", err)
		}
		query := endpoint.Query()
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		m.appHeaders(request, jwt)
		response, err := m.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("GitHub App installation discovery: %w", err)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			response.Body.Close()
			return nil, fmt.Errorf("GitHub App installation discovery failed (%d)", response.StatusCode)
		}
		var payload []struct {
			ID      json.RawMessage `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		err = json.NewDecoder(response.Body).Decode(&payload)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("GitHub App installation discovery returned an invalid response")
		}
		for _, installation := range payload {
			id, err := installationID(installation.ID)
			if err != nil || installation.Account.Login == "" {
				return nil, fmt.Errorf("GitHub App installation discovery returned an invalid installation account")
			}
			installations[installation.Account.Login] = id
		}
		if len(payload) < 100 {
			return installations, nil
		}
	}
}

func installationID(raw json.RawMessage) (string, error) {
	var stringID string
	if err := json.Unmarshal(raw, &stringID); err == nil && stringID != "" {
		return stringID, nil
	}
	var numberID int64
	if err := json.Unmarshal(raw, &numberID); err == nil && numberID > 0 {
		return strconv.FormatInt(numberID, 10), nil
	}
	return "", fmt.Errorf("invalid installation id")
}

func (m *Manager) exchange(ctx context.Context, jwt, installation string) (Lease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/app/installations/"+url.PathEscape(installation)+"/access_tokens", nil)
	if err != nil {
		return Lease{}, err
	}
	m.appHeaders(request, jwt)
	request.Header.Set("Accept", "application/vnd.github+v3+json")
	response, err := m.client.Do(request)
	if err != nil {
		return Lease{}, fmt.Errorf("GitHub App token exchange: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(response.Body)
		return Lease{}, fmt.Errorf("GitHub App token exchange failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil || payload.Token == "" {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	return Lease{Token: payload.Token, ExpiresAt: expiresAt}, nil
}

func (m *Manager) identity(ctx context.Context, appID, jwt, installationToken string) (GitIdentity, error) {
	m.mu.Lock()
	if identity, ok := m.identities[appID]; ok {
		m.mu.Unlock()
		return identity, nil
	}
	if waiting, ok := m.pendingIdentities[appID]; ok {
		m.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.identity, waiting.err
		case <-ctx.Done():
			return GitIdentity{}, ctx.Err()
		}
	}
	waiting := &pendingIdentity{done: make(chan struct{})}
	m.pendingIdentities[appID] = waiting
	m.mu.Unlock()

	identity, err := m.fetchIdentity(ctx, jwt, installationToken)
	m.mu.Lock()
	if err == nil {
		m.identities[appID] = identity
	}
	waiting.identity, waiting.err = identity, err
	delete(m.pendingIdentities, appID)
	close(waiting.done)
	m.mu.Unlock()
	return identity, err
}

func (m *Manager) fetchIdentity(ctx context.Context, jwt, installationToken string) (GitIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app", nil)
	if err != nil {
		return GitIdentity{}, err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App identity lookup: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return GitIdentity{}, fmt.Errorf("GitHub App identity lookup failed (%d)", response.StatusCode)
	}
	var app struct {
		Slug string `json:"slug"`
	}
	err = json.NewDecoder(response.Body).Decode(&app)
	response.Body.Close()
	if err != nil || app.Slug == "" {
		return GitIdentity{}, fmt.Errorf("GitHub App identity lookup returned an invalid app slug")
	}

	login := app.Slug + "[bot]"
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return GitIdentity{}, err
	}
	request.Header.Set("Authorization", "token "+installationToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
	response, err = m.client.Do(request)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup failed (%d)", response.StatusCode)
	}
	var bot struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&bot); err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	id, err := installationID(bot.ID)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	return GitIdentity{
		Name:  login,
		Email: id + "+" + login + "@users.noreply.github.com",
	}, nil
}

func (m *Manager) appHeaders(request *http.Request, jwt string) {
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
