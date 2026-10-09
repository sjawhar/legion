// Package githubapp mints GitHub App installation tokens and proves the App
// can read a repository before Dispatch stores it as an architecture source.
//
// The RS256 App JWT is hand-rolled on the standard library (crypto/rsa,
// crypto/sha256, encoding/base64) — no JWT dependency for one fixed
// header/claims shape.
package githubapp

import (
	"bytes"
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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// Typed access-check failures. Handlers surface these verbatim to the human
// configuring a source; anything else is a genuine upstream failure.
var (
	// ErrNoAppKey is the "no app credentials yet" boot state: the server has no
	// GitHub App private key, so no installation call can be signed.
	ErrNoAppKey = errors.New("the app has no private key configured")
	// ErrNoInstallation is GitHub's 404 for a repository's App installation.
	ErrNoInstallation = errors.New("the GitHub App is not installed")
	// ErrNoContentsRead is an installation whose Contents permission is missing
	// or "none". Read and write both satisfy read.
	ErrNoContentsRead = errors.New("the installation lacks Contents: read")
	// ErrNoActionsRead is an installation whose Actions permission is missing or
	// "none" (LEGION-567's reconcile needs it to list workflow runs and jobs).
	ErrNoActionsRead = errors.New("the installation lacks Actions: read")
	// ErrNoPullRequestsRead is an installation whose Pull requests permission is
	// missing or "none" (LEGION-567's reconcile needs it to read merged PRs).
	ErrNoPullRequestsRead = errors.New("the installation lacks Pull requests: read")
	// ErrNoBranch is GitHub's 404/422 for the configured branch: the branch does
	// not exist (or the repository vanished under the installation token).
	ErrNoBranch = errors.New("the branch does not exist")
	// ErrTreeTruncated is GitHub's truncated=true on a tree read: the
	// directory exceeds the tree API caps (100k entries / 7 MB), so the
	// listing is not trustworthy and the sync must fail whole.
	ErrTreeTruncated = errors.New("the directory tree is too large for the GitHub tree API")
	// ErrFileTooLarge is a directory entry whose recorded size exceeds
	// MaxFileSize: it is refused before its blob is fetched.
	ErrFileTooLarge = errors.New("file too large")
)

// MaxFileSize bounds one architecture file. The tree entry's size is checked
// against it before the blob is read.
const MaxFileSize = 1 << 20

// responseLimit caps a GitHub API response body; a blob response is read
// through blobResponseLimit instead, since base64 with GitHub's line wrapping
// inflates MaxFileSize bytes past responseLimit.
const (
	responseLimit     = 1 << 20
	blobResponseLimit = 2 << 20
)

// RequestTimeout bounds one API call -- the request and reading its body -- unless the caller's
// own context ends sooner, which always wins. GitHub terminates a request it has spent about
// ten seconds processing and answers its own error (documented for both the REST and the
// GraphQL API), so a deadline at ten seconds races GitHub's own: an answer that was about to
// arrive, success or GitHub's own timeout, is cut off and reported as a bare client timeout
// instead. This is that bound plus room for the connection and for transferring a response at
// responseLimit.
// A var rather than a const so a test can shrink it; nothing outside a test writes it.
var RequestTimeout = 20 * time.Second

// ResponseTooLargeError reports a response that did not fit inside a caller's configured limit.
type ResponseTooLargeError struct {
	Limit int64
}

func (err *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("GitHub response body exceeds the %d-byte limit", err.Limit)
}

const defaultBase = "https://api.github.com"

// tokenExpirySlack retires a cached installation token this long before
// GitHub's expiry so a token handed to a caller never dies mid-request.
const tokenExpirySlack = 5 * time.Minute

// Installation is the slice of GitHub's repository-installation response
// dispatch inspects. AccountLogin (the installation's own account -- a user or an org) is set
// only by ListInstallations (GET /app/installations), which is account-less in its own answer
// shape; RepositoryToken's own installation()/DeliveryPermissions() lookups never set it, since
// they already know the account from the owner/repo they resolved.
type Installation struct {
	ID           int64
	AppSlug      string
	Permissions  auth.AppPerms
	AccountLogin string
}

// Source is a successful access check: the installation that covers the
// repository and the slug of the App it belongs to.
type Source struct {
	InstallationID int64
	AppSlug        string
}

type cachedToken struct {
	token   string
	expires time.Time
}

// ttlCache is a small time-bounded cache keyed by K, shared by ListInstallations (one fixed key)
// and ListInstallationRepositoriesByID (one key per installation id) -- both independently
// hand-rolled the identical lock/check-age/unlock-on-hit, fetch-then-lock/store/unlock shape
// before this helper existed. A singleflight.Group collapses concurrent cold-cache fetches for
// the same key into one network call: two goroutines racing a cold cache for the same
// installation (plausible under the cross-installation errgroup's own concurrency) now share
// one fetch's answer instead of each minting its own.
type ttlCache[K comparable, V any] struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	items map[K]ttlCacheEntry[V]
	group singleflight.Group
}

type ttlCacheEntry[V any] struct {
	value V
	at    time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration, now func() time.Time) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, now: now, items: map[K]ttlCacheEntry[V]{}}
}

// fresh returns key's cached value when younger than ttl.
func (c *ttlCache[K, V]) fresh(key K) (V, bool) {
	c.mu.Lock()
	entry, ok := c.items[key]
	c.mu.Unlock()
	if ok && c.now().Sub(entry.at) < c.ttl {
		return entry.value, true
	}
	var zero V
	return zero, false
}

// get returns key's cached value when fresh; otherwise it calls fetch exactly once across every
// concurrent caller for this key (singleflight, keyed by key's string form) and caches the
// result for the rest.
func (c *ttlCache[K, V]) get(key K, fetch func() (V, error)) (V, error) {
	if value, ok := c.fresh(key); ok {
		return value, nil
	}
	value, err, _ := c.group.Do(fmt.Sprint(key), func() (any, error) {
		// Re-check: a sibling caller may have already refreshed this key while this one waited
		// to acquire the singleflight call.
		if value, ok := c.fresh(key); ok {
			return value, nil
		}
		fetched, err := fetch()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.items[key] = ttlCacheEntry[V]{value: fetched, at: c.now()}
		c.mu.Unlock()
		return fetched, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return value.(V), nil
}

// Client calls the GitHub App API as the configured App. A nil *Client is the
// valid "no app credentials yet" state: every method returns ErrNoAppKey.
type Client struct {
	app  auth.AppConfig
	key  *rsa.PrivateKey
	base string
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
	// repositories maps "owner/repo" to the installation that covers it, for RepositoryToken.
	repositories map[string]int64
	// installations caches ListInstallations' own answer under one fixed key
	// (installationsCacheKey): the installation list changes only when a human
	// installs/uninstalls the App, far slower than every reconcile pass needs to re-fetch it.
	installations *ttlCache[string, []Installation]
	// installationRepos caches ListInstallationRepositoriesByID's own answer per installation id,
	// for the same reason.
	installationRepos *ttlCache[int64, []string]
}

// installationsCacheKey is the one key installations (a single answer, not per-installation)
// caches under.
const installationsCacheKey = "installations"

// installationsCacheTTL bounds how long ListInstallations trusts its own cached answer before
// asking GitHub again.
const installationsCacheTTL = 10 * time.Minute

// New builds a client from the loaded App credentials. It returns (nil, nil)
// when app is nil or carries no private key — the caller keeps the nil client
// and methods answer ErrNoAppKey — and an error only for a malformed key, so
// a misconfigured deployment fails at boot. base overrides the GitHub API
// origin (DISPATCH_GITHUB_API_BASE); empty means api.github.com.
func New(app *auth.AppConfig, base string) (*Client, error) {
	if app == nil || app.PEM == "" {
		return nil, nil
	}
	key, err := parsePrivateKey(app.PEM)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	if base == "" {
		base = defaultBase
	}
	c := &Client{
		app:  *app,
		key:  key,
		base: strings.TrimSuffix(base, "/"),
		// No Timeout here: request applies RequestTimeout to each call's own context instead,
		// so a caller that needs a shorter one sets it on the context it passes.
		http:         &http.Client{},
		now:          time.Now,
		tokens:       map[int64]cachedToken{},
		repositories: map[string]int64{},
	}
	nowFunc := func() time.Time { return c.now() }
	c.installations = newTTLCache[string, []Installation](installationsCacheTTL, nowFunc)
	c.installationRepos = newTTLCache[int64, []string](installationsCacheTTL, nowFunc)
	return c, nil
}

func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unsupported key type %T (need RSA)", parsed)
	}
	return key, nil
}

// appJWT signs the App-authentication JWT. GitHub accepts the App's client ID
// as iss (it is the one identifier LoadAppFromEnv requires; the numeric App ID
// is optional and zero in e2e) and caps exp at ten minutes; iat is backdated
// a minute against clock skew.
func (c *Client) appJWT() (string, error) {
	now := c.now()
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.app.ClientID,
	})
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Installation resolves the App installation covering owner/repo and checks
// its Contents permission. A GitHub 404 is ErrNoInstallation; a missing or
// "none" Contents permission is ErrNoContentsRead. Does not consult
// RepositoryToken's installationID cache (c.repositories): that cache exists
// so RepositoryToken can skip resolving owner/repo again before minting a
// token by ID, but GitHub's installation-lookup endpoint already takes
// owner/repo directly in one GET -- there is no separate "resolve the ID"
// round trip here to skip, and a permissions check must read GitHub's
// current answer every call regardless (that is this function's whole job;
// a cached ID doesn't carry cached permissions with it, and an org admin can
// revoke a permission between calls).
func (c *Client) Installation(ctx context.Context, owner, repo string) (Installation, error) {
	installation, err := c.installation(ctx, owner, repo)
	if err != nil {
		return Installation{}, err
	}
	if installation.Permissions.Contents != "read" && installation.Permissions.Contents != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoContentsRead, owner, repo)
	}
	return installation, nil
}

// DeliveryPermissions resolves the App installation covering owner/repo and checks its Actions
// and Pull-requests permissions (LEGION-567's delivery timeline: the reconcile lists workflow
// runs/jobs and merged pull requests, never Contents). A GitHub 404 is ErrNoInstallation; a
// missing or "none" Actions permission is ErrNoActionsRead; a missing or "none" Pull-requests
// permission is ErrNoPullRequestsRead -- checked in that order, so a caller showing one error at
// a time names Actions first.
func (c *Client) DeliveryPermissions(ctx context.Context, owner, repo string) (Installation, error) {
	installation, err := c.installation(ctx, owner, repo)
	if err != nil {
		return Installation{}, err
	}
	if installation.Permissions.Actions != "read" && installation.Permissions.Actions != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoActionsRead, owner, repo)
	}
	if installation.Permissions.PullRequests != "read" && installation.Permissions.PullRequests != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoPullRequestsRead, owner, repo)
	}
	return installation, nil
}

func (c *Client) installation(ctx context.Context, owner, repo string) (Installation, error) {
	if c == nil {
		return Installation{}, ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return Installation{}, err
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	body, status, err := c.do(ctx, http.MethodGet, target, "Bearer "+jwt)
	if err != nil {
		return Installation{}, err
	}
	if status == http.StatusNotFound {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoInstallation, owner, repo)
	}
	if status != http.StatusOK {
		return Installation{}, fmt.Errorf("GET %s: status %d: %s", target, status, body)
	}
	var payload installationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Installation{}, fmt.Errorf("decode installation: %w", err)
	}
	return Installation{ID: payload.ID, AppSlug: payload.AppSlug, Permissions: payload.Permissions}, nil
}

// installationPayload is GitHub's one object shape for an installation, shared by
// GET /repos/{owner}/{repo}/installation (installation()) and GET /app/installations
// (ListInstallations): identical except that only the list form's answer needs Account, since a
// repo-scoped lookup already knows the account from its own owner/repo argument.
type installationPayload struct {
	ID      int64  `json:"id"`
	AppSlug string `json:"app_slug"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	Permissions auth.AppPerms `json:"permissions"`
}

// maxInstallations bounds ListInstallations' pagination the same way maxInstallationRepositories
// bounds a single installation's own repository list: a real-world App installed on far more
// accounts than this should fail loudly rather than grow an unbounded page loop forever.
const maxInstallations = 2000

// ListInstallations lists every installation of this App (GET /app/installations,
// JWT-authenticated, paginated): LEGION-567's merged-PR search must cover every installation the
// App has, not only the one covering the configured deploy repository, since the population rule
// (an author allowlist) names no installation or org boundary. Cached for installationsCacheTTL:
// unlike RepositoryToken's owner/repo cache (invalidated only when a mint actually fails), this is
// a plain time-based cache, since there is no per-call signal analogous to a failed token mint to
// invalidate it on -- the install list changes only when a human installs/uninstalls the App, far
// slower than any reconcile pass needs a fresh answer. Unlike this cache's own pre-ttlCache form,
// a zero-installations answer is now cached too (ttlCache.fresh tests presence, not a non-nil
// slice): an App installed nowhere re-fetches at most once per TTL instead of on every call.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	return c.installations.get(installationsCacheKey, func() ([]Installation, error) {
		jwt, err := c.appJWT()
		if err != nil {
			return nil, err
		}
		var installations []Installation
		for page := 1; ; page++ {
			target := fmt.Sprintf("%s/app/installations?per_page=100&page=%d", c.base, page)
			body, status, header, err := c.request(ctx, http.MethodGet, target, "Bearer "+jwt, nil, responseLimit)
			if err != nil {
				return nil, fmt.Errorf("list installations (page %d): %w", page, err)
			}
			if err := CheckResponse(status, header, body); err != nil {
				return nil, fmt.Errorf("list installations (page %d): %w", page, err)
			}
			var payload []installationPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, fmt.Errorf("decode installations (page %d): %w", page, err)
			}
			if len(payload) == 0 {
				break
			}
			for _, entry := range payload {
				installations = append(installations, Installation{
					ID: entry.ID, AppSlug: entry.AppSlug, Permissions: entry.Permissions, AccountLogin: entry.Account.Login,
				})
			}
			if len(installations) > maxInstallations {
				return nil, fmt.Errorf("list installations: the App has more than %d installations", maxInstallations)
			}
			if len(payload) < 100 {
				break
			}
		}
		return installations, nil
	})
}

// RepositoryToken returns an installation token of the installation covering owner/repo. The
// installation a repository resolves to is remembered; a token that cannot be minted for it
// forgets it, so an App moved between installations is looked up again.
func (c *Client) RepositoryToken(ctx context.Context, owner, repo string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	name := owner + "/" + repo
	c.mu.Lock()
	installationID, known := c.repositories[name]
	c.mu.Unlock()
	if !known {
		installation, err := c.installation(ctx, owner, repo)
		if err != nil {
			return "", err
		}
		installationID = installation.ID
		c.mu.Lock()
		c.repositories[name] = installationID
		c.mu.Unlock()
	}
	token, err := c.Token(ctx, installationID)
	if err != nil {
		c.mu.Lock()
		delete(c.repositories, name)
		c.mu.Unlock()
		return "", err
	}
	return token, nil
}

// installationRepositoriesPerPage is GitHub's own page size for GET /installation/repositories.
const installationRepositoriesPerPage = 100

// maxInstallationRepositories bounds ListInstallationRepositoriesByID the same way
// delivery.maxDeliveryWindow/maxRunsPerWindow/maxPullRequestsPerWindow bound their own queries:
// an installation covering a very large org should fail loudly past this rather than build an
// ever-growing, unbounded `repo:` qualifier list on every 5-minute reconcile pass.
const maxInstallationRepositories = 2000

// ListInstallationRepositoriesByID lists every repository ("owner/name") installation installs
// -- GET /installation/repositories (an installation-token endpoint, paginated), under a token
// minted directly by id (ListInstallations already resolved the id; there is no representative
// owner/repo to mint one through RepositoryToken's own owner/repo-keyed cache). LEGION-567's
// merged-PR search needs this to scope its query to exactly each installation's own repositories:
// an unqualified GitHub search query is NOT scoped by the authenticating token for
// public-repository content -- it searches all of public GitHub (confirmed live against a real
// installation token) -- so the caller must supply an explicit repo: qualifier per repository
// instead of relying on the token alone. Cached for installationsCacheTTL per installation id,
// for the same reason ListInstallations caches its own answer: round 4's reconcile pass refetched
// every installation's full repository list on every pass, multiplying the across-installation
// search's own cost by the installation count for data that changes on human timescales, not
// every five minutes.
func (c *Client) ListInstallationRepositoriesByID(ctx context.Context, installationID int64) ([]string, error) {
	return c.installationRepos.get(installationID, func() ([]string, error) {
		token, err := c.Token(ctx, installationID)
		if err != nil {
			return nil, fmt.Errorf("mint token for installation %d: %w", installationID, err)
		}
		return listInstallationRepositories(ctx, c, token)
	})
}

func listInstallationRepositories(ctx context.Context, c *Client, token string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", installationRepositoriesPerPage, page)
		body, status, header, err := c.Read(ctx, token, path)
		if err != nil {
			return nil, fmt.Errorf("list installation repositories (page %d): %w", page, err)
		}
		if err := CheckResponse(status, header, body); err != nil {
			return nil, fmt.Errorf("list installation repositories (page %d): %w", page, err)
		}
		var payload struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode installation repositories (page %d): %w", page, err)
		}
		if len(payload.Repositories) == 0 {
			break
		}
		for _, repository := range payload.Repositories {
			names = append(names, repository.FullName)
		}
		if len(names) > maxInstallationRepositories {
			return nil, fmt.Errorf("list installation repositories: installation covers more than %d repositories", maxInstallationRepositories)
		}
		if len(payload.Repositories) < installationRepositoriesPerPage {
			break
		}
	}
	return names, nil
}

// Read performs GET path (an API path with its query, under the API origin) with an
// installation token, returning GitHub's complete answer when it fits within the response limit.
func (c *Client) Read(ctx context.Context, token, path string) ([]byte, int, http.Header, error) {
	if c == nil {
		return nil, 0, nil, ErrNoAppKey
	}
	return c.request(ctx, http.MethodGet, c.base+path, "Bearer "+token, nil, responseLimit)
}

// GraphQL posts query/variables to the API origin's /graphql endpoint with an installation token,
// returning GitHub's complete JSON response body (an `{data, errors}` envelope the caller
// decodes) when it fits within the response limit. Some GitHub Apps' installation tokens can read
// a repository's REST endpoints but are refused by the REST `/search/issues` endpoint for a
// private repository ("cannot be searched... do not have permission") -- GraphQL's `search`
// connection does not share that restriction, which is why delivery/github_prs.go's merged-PR
// search uses this instead of REST search.
func (c *Client) GraphQL(ctx context.Context, token string, query string, variables map[string]any) ([]byte, int, http.Header, error) {
	if c == nil {
		return nil, 0, nil, ErrNoAppKey
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode GraphQL request: %w", err)
	}
	return c.request(ctx, http.MethodPost, c.base+"/graphql", "Bearer "+token, bytes.NewReader(payload), responseLimit)
}

// Token returns an installation access token, minting one only when the
// cached token is within tokenExpirySlack of GitHub's expiry.
func (c *Client) Token(ctx context.Context, installationID int64) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	c.mu.Lock()
	cached, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && c.now().Before(cached.expires.Add(-tokenExpirySlack)) {
		return cached.token, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	target := c.base + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	body, status, err := c.do(ctx, http.MethodPost, target, "Bearer "+jwt)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("POST %s: status %d: %s", target, status, body)
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode installation token: %w", err)
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: payload.Token, expires: payload.ExpiresAt}
	c.mu.Unlock()
	return payload.Token, nil
}

// CheckSource proves end to end that the App can read owner/repo: the
// installation exists with Contents: read, a token mints, and the repository
// resolves under that token. Failures a human can fix are the typed errors
// above; anything else is an upstream failure.
func (c *Client) CheckSource(ctx context.Context, owner, repo string) (Source, error) {
	if c == nil {
		return Source{}, ErrNoAppKey
	}
	installation, err := c.Installation(ctx, owner, repo)
	if err != nil {
		return Source{}, err
	}
	token, err := c.Token(ctx, installation.ID)
	if err != nil {
		return Source{}, err
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	body, status, err := c.do(ctx, http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return Source{}, err
	}
	if status == http.StatusNotFound {
		return Source{}, fmt.Errorf("%w: its token cannot read %s/%s", ErrNoInstallation, owner, repo)
	}
	if status != http.StatusOK {
		return Source{}, fmt.Errorf("GET %s: status %d: %s", target, status, body)
	}
	return Source{InstallationID: installation.ID, AppSlug: installation.AppSlug}, nil
}

func (c *Client) do(ctx context.Context, method, target, authorization string) ([]byte, int, error) {
	return c.doLimited(ctx, method, target, authorization, responseLimit)
}

func (c *Client) doLimited(ctx context.Context, method, target, authorization string, limit int64) ([]byte, int, error) {
	body, status, _, err := c.request(ctx, method, target, authorization, nil, limit)
	return body, status, err
}

// request performs one API call and returns its complete body when it fits within limit, status
// and headers. body is nil for every GET call; GraphQL is this package's only POST with a body.
func (c *Client) request(ctx context.Context, method, target, authorization string, body io.Reader, limit int64) ([]byte, int, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build %s %s: %w", method, target, err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read %s %s response: %w", method, target, err)
	}
	if int64(len(responseBody)) > limit {
		return nil, 0, nil, fmt.Errorf("%s %s: %w", method, target, &ResponseTooLargeError{Limit: limit})
	}
	return responseBody, response.StatusCode, response.Header, nil
}

// Ref resolves branch to its commit SHA under an installation token
// (`GET /repos/{owner}/{repo}/commits/{branch}`) — one call that also proves
// the branch exists: GitHub's 404 (and its 422 for an empty repository or a
// malformed ref) is ErrNoBranch.
func (c *Client) Ref(ctx context.Context, token, owner, repo, branch string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/commits/" + url.PathEscape(branch)
	body, status, err := c.do(ctx, http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return "", fmt.Errorf("%w: %s on %s/%s", ErrNoBranch, branch, owner, repo)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d: %s", target, status, body)
	}
	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode commit for %s/%s@%s: %w", owner, repo, branch, err)
	}
	if payload.SHA == "" {
		return "", fmt.Errorf("commit for %s/%s@%s carries no sha", owner, repo, branch)
	}
	return payload.SHA, nil
}

// Dir is one directory listing at a commit: the directory's own tree object
// id (identical files give an identical SHA, whatever else the commit
// changed) and its regular markdown files.
type Dir struct {
	// SHA is the subtree object id; empty when the directory does not exist
	// at the commit.
	SHA   string
	Files []DirFile
}

// DirFile is one `*.md` regular file directly inside the directory.
type DirFile struct {
	Name string // file name inside the directory
	Path string // repo-relative path, for error messages
	SHA  string // blob id
	Size int64
}

// Dir lists the markdown files directly inside dir at commit sha with one
// subtree read (`GET /repos/{owner}/{repo}/git/trees/{sha}:{dir}`): only the
// directory's own entries come back, so a large repository elsewhere cannot
// push the response past the read cap. GitHub's 404 for a commit without dir
// is an empty Dir (a valid, empty model). Only regular blobs (mode 100644 /
// 100755) count: symlinks (120000), submodules, and nested directories are
// skipped. A listing GitHub answers truncated is ErrTreeTruncated — it cannot
// be trusted, so the sync fails whole. dir is a plain repo-relative path, no
// leading slash.
func (c *Client) Dir(ctx context.Context, token, owner, repo, sha, dir string) (Dir, error) {
	if c == nil {
		return Dir{}, ErrNoAppKey
	}
	dir = strings.Trim(dir, "/")
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/git/trees/" + url.PathEscape(sha+":"+dir)
	body, status, err := c.do(ctx, http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return Dir{}, err
	}
	if status == http.StatusNotFound {
		return Dir{}, nil
	}
	if status != http.StatusOK {
		return Dir{}, fmt.Errorf("GET %s: status %d: %s", target, status, body)
	}
	var tree struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return Dir{}, fmt.Errorf("decode tree %s:%s: %w", sha, dir, err)
	}
	if tree.Truncated {
		return Dir{}, fmt.Errorf("%w: %s/%s@%s:%s", ErrTreeTruncated, owner, repo, sha, dir)
	}
	if tree.SHA == "" {
		return Dir{}, fmt.Errorf("tree %s:%s carries no sha", sha, dir)
	}
	listing := Dir{SHA: tree.SHA}
	for _, entry := range tree.Tree {
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") ||
			strings.Contains(entry.Path, "/") || !strings.HasSuffix(entry.Path, ".md") {
			continue
		}
		listing.Files = append(listing.Files, DirFile{
			Name: entry.Path, Path: dir + "/" + entry.Path, SHA: entry.SHA, Size: entry.Size,
		})
	}
	return listing, nil
}

// DirFiles reads every file of a Dir listing (one blob read each, base64),
// keyed by file name. A file whose listed size exceeds MaxFileSize is
// ErrFileTooLarge before any blob is fetched; a blob that vanished between
// the listing and the read fails the whole set.
func (c *Client) DirFiles(ctx context.Context, token, owner, repo string, listing Dir) (map[string][]byte, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	for _, file := range listing.Files {
		if file.Size > MaxFileSize {
			return nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrFileTooLarge, file.Path, file.Size, MaxFileSize)
		}
	}
	repoBase := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	files := make(map[string][]byte, len(listing.Files))
	for _, file := range listing.Files {
		content, err := c.blob(ctx, token, repoBase, file.Path, file.SHA)
		if err != nil {
			return nil, err
		}
		files[file.Name] = content
	}
	return files, nil
}

func (c *Client) blob(ctx context.Context, token, repoBase, path, sha string) ([]byte, error) {
	target := repoBase + "/git/blobs/" + url.PathEscape(sha)
	body, status, err := c.doLimited(ctx, http.MethodGet, target, "Bearer "+token, blobResponseLimit)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s (%s): status %d: %s", target, path, status, body)
	}
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode blob %s (%s): %w", sha, path, err)
	}
	if payload.Encoding != "base64" {
		return nil, fmt.Errorf("blob %s (%s) has encoding %q, want base64", sha, path, payload.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode blob %s (%s) content: %w", sha, path, err)
	}
	if len(content) > MaxFileSize {
		return nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrFileTooLarge, path, len(content), MaxFileSize)
	}
	return content, nil
}
