// Package githubapp mints GitHub App installation tokens and proves the App
// can read a repository before Dispatch stores it as an architecture source.
//
// The RS256 App JWT is hand-rolled on the standard library (crypto/rsa,
// crypto/sha256, encoding/base64) — no JWT dependency for one fixed
// header/claims shape.
package githubapp

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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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

const defaultBase = "https://api.github.com"

// tokenExpirySlack retires a cached installation token this long before
// GitHub's expiry so a token handed to a caller never dies mid-request.
const tokenExpirySlack = 5 * time.Minute

// Installation is the slice of GitHub's repository-installation response
// dispatch inspects.
type Installation struct {
	ID          int64
	AppSlug     string
	Permissions auth.AppPerms
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
}

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
	return &Client{
		app:    *app,
		key:    key,
		base:   strings.TrimSuffix(base, "/"),
		http:   &http.Client{Timeout: 10 * time.Second},
		now:    time.Now,
		tokens: map[int64]cachedToken{},
	}, nil
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
// "none" Contents permission is ErrNoContentsRead.
func (c *Client) Installation(ctx context.Context, owner, repo string) (Installation, error) {
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
	var payload struct {
		ID          int64         `json:"id"`
		AppSlug     string        `json:"app_slug"`
		Permissions auth.AppPerms `json:"permissions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Installation{}, fmt.Errorf("decode installation: %w", err)
	}
	if payload.Permissions.Contents != "read" && payload.Permissions.Contents != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoContentsRead, owner, repo)
	}
	return Installation{ID: payload.ID, AppSlug: payload.AppSlug, Permissions: payload.Permissions}, nil
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
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s %s: %w", method, target, err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s %s response: %w", method, target, err)
	}
	return body, response.StatusCode, nil
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
