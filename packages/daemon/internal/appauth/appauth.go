// Package appauth mints GitHub installation tokens for Legion's two GitHub Apps.
package appauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// refreshWindow is how long before a lease's expiry Token stops serving it from the cache and
// mints a fresh one. The daemon rewrites each role's gh token file from the cached lease, so a
// container sees a new token only after that rewrite reaches it, and a Kubernetes Secret volume
// reaches a running pod within about the kubelet's one-minute sync period. Twenty minutes leaves
// a container the new token at least some fifteen minutes before the old one expires, while GitHub
// keeps the old token valid until its own expiry, so a `gh` or `git` that started on the old one
// finishes on it.
const refreshWindow = 20 * time.Minute

// AppRole names the GitHub App a Legion role acts as.
type AppRole string

const (
	Implement AppRole = "implement"
	Review    AppRole = "review"
)

// Roles is every App role, the Apps a Legion deployment configures one of each of.
var Roles = []AppRole{Implement, Review}

// GitIdentity is the bot identity that jj and git must use for work a role performs.
type GitIdentity = runtime.GitIdentity

// Lease is one installation-token lease, including its expiry, when it was minted and the App bot
// identity.
type Lease struct {
	Token     string
	ExpiresAt time.Time
	// Minted is when GitHub issued the token: one moments old may not be accepted yet.
	Minted   time.Time
	Identity GitIdentity
}

// TransientError is a GitHub answer that says nothing about the App or its installation, so the
// same request may pass when it is made again: no answer at all (a transport failure, including a
// request or a body read cut off by its deadline), a 5xx, or a rate limit (a 429, or a 403 that
// says it is one). Every other failure is GitHub's definitive answer, or one about the
// configuration, and asking again changes nothing.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// transient marks a response, whose body is body, as GitHub's trouble rather than an answer: a
// 5xx, or a rate limit (githubrest.RateLimited, which reads the body for a secondary limit GitHub
// names there alone).
func transient(response *http.Response, body []byte, err error) error {
	if response.StatusCode >= http.StatusInternalServerError || githubrest.RateLimited(response, body) {
		return &TransientError{Err: err}
	}
	return err
}

// refusalBody is the body GitHub sent with a refusal, read whole, and the response closed. One
// that cannot be read is empty: the status and the headers still say what the answer is.
func refusalBody(response *http.Response) []byte {
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return body
}

// readBody reads a successful response's body whole: a read that fails is a transport failure,
// and transient, where what the body then fails to parse as is GitHub's answer.
func readBody(response *http.Response, what string) ([]byte, error) {
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s: read the response: %w", what, err)}
	}
	return body, nil
}

// Tokens mints a GitHub installation token for an App and repository owner.
type Tokens interface {
	Token(ctx context.Context, role AppRole, owner string) (Lease, error)
	// ReadToken is the App's installation token for owner narrowed to reading: contents, pull
	// requests and metadata read. A pane holds one for every owner of the project but its issue's.
	ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error)
}

// readPermissions narrows an installation token to reading a repository's contents, its pull
// requests and its metadata: what a role needs of every repository of the project but its own
// issue's. GitHub mints a token with the permissions a request names, each at most what the
// installation holds.
var readPermissions = map[string]string{"contents": "read", "pull_requests": "read", "metadata": "read"}

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

	mu                sync.Mutex
	leases            map[string]Lease
	pending           map[string]*pendingLease
	installations     map[AppRole]map[string]string
	identities        map[string]GitIdentity
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

// Token returns a cached lease until fewer than twenty minutes remain (refreshWindow), otherwise
// minting a fresh installation token. Concurrent requests for the same App and owner share that
// one exchange.
func (m *Manager) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	return m.lease(ctx, role, owner, "", nil)
}

// ReadToken is Token for a token narrowed to readPermissions, cached under a key of its own, so an
// owner's write and read tokens never stand in for each other.
func (m *Manager) ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error) {
	return m.lease(ctx, role, owner, "read:", readPermissions)
}

// Forgetter drops the leases a token source holds for an App role and owner, so the next one is
// minted: a token GitHub refused (401) is not served again from the cache.
type Forgetter interface {
	Forget(role AppRole, owner string)
}

// Forget drops every lease cached for role and owner, the write token's and the read token's.
func (m *Manager) Forget(role AppRole, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, leaseKey("", role, owner))
	delete(m.leases, leaseKey("read:", role, owner))
}

// leaseKey is the cache key of role's lease for owner under prefix: the write token's under "",
// the read token's under "read:".
func leaseKey(prefix string, role AppRole, owner string) string {
	return prefix + string(role) + ":" + strings.ToLower(strings.TrimSpace(owner))
}

// lease serves the cached lease under prefix+role+owner while more than refreshWindow remains, and
// otherwise mints one with permissions (nil: the installation's own), sharing one exchange among
// concurrent callers.
func (m *Manager) lease(ctx context.Context, role AppRole, owner, prefix string, permissions map[string]string) (Lease, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Lease{}, fmt.Errorf("GitHub App owner must not be empty")
	}
	cacheKey := leaseKey(prefix, role, owner)
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

	lease, err := m.mint(ctx, role, owner, permissions)
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

// mint mints role's installation token for owner. An installation the daemon found that answers
// the exchange 404 was removed since (errInstallationGone): it is forgotten and looked for once
// more, so a removed App answers ErrNotInstalled and an App installed there again mints on its new
// installation. A configured installation answering 404 is not installed either.
func (m *Manager) mint(ctx context.Context, role AppRole, owner string, permissions map[string]string) (Lease, error) {
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
	lease, err := m.exchange(ctx, jwt, installation, permissions)
	if errors.Is(err, errInstallationGone) && configuredInstallation(app, owner) == "" {
		m.forgetInstallation(role, owner)
		if installation, err = m.installation(ctx, role, app, owner); err != nil {
			return Lease{}, err
		}
		lease, err = m.exchange(ctx, jwt, installation, permissions)
	}
	if errors.Is(err, errInstallationGone) {
		m.forgetInstallation(role, owner)
		return Lease{}, fmt.Errorf("%w: %s's installation %s on %s answers %w", ErrNotInstalled, role, installation, owner, err)
	}
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
	if installation := configuredInstallation(app, owner); installation != "" {
		return installation, nil
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
		return "", fmt.Errorf("%w: %s not installed on %s", ErrNotInstalled, role, owner)
	}
	return installation, nil
}

// configuredInstallation is the installation app's configuration names for owner, if any.
func configuredInstallation(app config.GitHubApp, owner string) string {
	for configuredOwner, installation := range app.Installations {
		if strings.EqualFold(configuredOwner, owner) && installation != "" {
			return installation
		}
	}
	return ""
}

// forgetInstallation drops the installation found for role on owner, for the next mint to look
// for it again.
func (m *Manager) forgetInstallation(role AppRole, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.installations[role], strings.ToLower(owner))
}

// errInstallationGone is a token exchange GitHub answers 404: the installation is no longer there.
var errInstallationGone = errors.New("the installation is gone")

// ErrNotInstalled is a mint refused because the App has no installation on the owner: an answer
// about the owner, not GitHub's trouble, which a retry would only repeat.
var ErrNotInstalled = errors.New("github_app_not_installed")

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
			return nil, &TransientError{Err: fmt.Errorf("GitHub App installation discovery: %w", err)}
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return nil, transient(response, refusalBody(response), fmt.Errorf("GitHub App installation discovery failed (%d)", response.StatusCode))
		}
		body, err := readBody(response, "GitHub App installation discovery")
		if err != nil {
			return nil, err
		}
		var payload []struct {
			ID      json.RawMessage `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
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

func (m *Manager) exchange(ctx context.Context, jwt, installation string, permissions map[string]string) (Lease, error) {
	var requestBody io.Reader
	if permissions != nil {
		encoded, err := json.Marshal(map[string]map[string]string{"permissions": permissions})
		if err != nil {
			return Lease{}, err
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/app/installations/"+url.PathEscape(installation)+"/access_tokens", requestBody)
	if err != nil {
		return Lease{}, err
	}
	m.appHeaders(request, jwt)
	request.Header.Set("Accept", "application/vnd.github+v3+json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return Lease{}, &TransientError{Err: fmt.Errorf("GitHub App token exchange: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := refusalBody(response)
		failed := fmt.Errorf("GitHub App token exchange failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
		if response.StatusCode == http.StatusNotFound {
			return Lease{}, fmt.Errorf("%w: %w", errInstallationGone, failed)
		}
		return Lease{}, transient(response, body, failed)
	}
	body, err := readBody(response, "GitHub App token exchange")
	if err != nil {
		return Lease{}, err
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil || payload.Token == "" {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	return Lease{Token: payload.Token, ExpiresAt: expiresAt, Minted: m.now()}, nil
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
	slug, err := m.appSlug(ctx, jwt)
	if err != nil {
		return GitIdentity{}, err
	}
	login := slug + "[bot]"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return GitIdentity{}, err
	}
	request.Header.Set("Authorization", "token "+installationToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
	response, err := m.client.Do(request)
	if err != nil {
		return GitIdentity{}, &TransientError{Err: fmt.Errorf("GitHub App bot identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return GitIdentity{}, transient(response, refusalBody(response), fmt.Errorf("GitHub App bot identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App bot identity lookup")
	if err != nil {
		return GitIdentity{}, err
	}
	var bot struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &bot); err != nil {
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

// appSlug is the slug of the App jwt signs for, read from GET /app: its bot login is the slug's,
// "<slug>[bot]".
func (m *Manager) appSlug(ctx context.Context, jwt string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app", nil)
	if err != nil {
		return "", err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return "", &TransientError{Err: fmt.Errorf("GitHub App identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", transient(response, refusalBody(response), fmt.Errorf("GitHub App identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App identity lookup")
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &app); err != nil || app.Slug == "" {
		return "", fmt.Errorf("GitHub App identity lookup returned an invalid app slug")
	}
	return app.Slug, nil
}

func (m *Manager) appHeaders(request *http.Request, jwt string) {
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
