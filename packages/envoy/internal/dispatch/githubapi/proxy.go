package githubapi

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

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

const (
	githubAPIBase       = "https://api.github.com"
	proactiveRefreshMS  = 5 * 60 * 1000
	proxyRequestTimeout = 60 * time.Second
)

// ProxyConfig is the per-request state needed to forward a dashboard request
// to GitHub. One config is built per request from the user's record + the
// loaded Envoy App credentials.
type ProxyConfig struct {
	Tokens       *auth.Tokens
	Users        auth.UserStore
	Login        string
	ClientID     string
	ClientSecret string
	// HTTPClient lets tests inject a fake transport. nil → default http.Client.
	HTTPClient auth.HTTPClient
	// RefreshFn lets tests override the refresh call. nil → real OAuth refresh.
	RefreshFn func(ctx context.Context, tokens *auth.Tokens) (*auth.Tokens, error)
}

func (c *ProxyConfig) httpClient() auth.HTTPClient {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: proxyRequestTimeout}
}

func (c *ProxyConfig) refresh(ctx context.Context, tokens *auth.Tokens) (*auth.Tokens, error) {
	if c.RefreshFn != nil {
		return c.RefreshFn(ctx, tokens)
	}
	return auth.RefreshTokens(ctx, c.ClientID, c.ClientSecret, tokens.RefreshToken, c.HTTPClient)
}

// ProxyREST forwards /api/github/rest/<rest_path> to GitHub.
func ProxyREST(w http.ResponseWriter, r *http.Request, cfg *ProxyConfig) {
	const prefix = "/api/github/rest/"
	restPath := strings.TrimPrefix(r.URL.Path, prefix)
	target := fmt.Sprintf("%s/%s", githubAPIBase, restPath)
	if r.URL.RawQuery != "" {
		target = target + "?" + r.URL.RawQuery
	}
	proxy(w, r, cfg, target)
}

// ProxyGraphQL forwards /api/github/graphql to GitHub.
func ProxyGraphQL(w http.ResponseWriter, r *http.Request, cfg *ProxyConfig) {
	proxy(w, r, cfg, githubAPIBase+"/graphql")
}

func proxy(w http.ResponseWriter, r *http.Request, cfg *ProxyConfig, target string) {
	// Buffer body up front so we can retry after a token refresh.
	var bodyBytes []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
			return
		}
		bodyBytes = data
	}

	ctx := r.Context()
	tokens, err := proactivelyRefresh(ctx, cfg)
	if err != nil {
		writeNeedsReauth(w)
		return
	}
	resp, err := forward(ctx, cfg, target, r, tokens, bodyBytes)
	if err != nil {
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusUnauthorized {
		writeResponse(w, resp)
		return
	}
	resp.Body.Close()

	// Retry once after a forced refresh.
	refreshed, err := refreshAndStore(ctx, cfg, tokens)
	if err != nil {
		writeNeedsReauth(w)
		return
	}
	retry, err := forward(ctx, cfg, target, r, refreshed, bodyBytes)
	if err != nil {
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	if retry.StatusCode == http.StatusUnauthorized {
		retry.Body.Close()
		writeNeedsReauth(w)
		return
	}
	writeResponse(w, retry)
}

func proactivelyRefresh(ctx context.Context, cfg *ProxyConfig) (*auth.Tokens, error) {
	if cfg.Tokens.AccessExpiresAt-time.Now().UnixMilli() >= proactiveRefreshMS {
		return cfg.Tokens, nil
	}
	return refreshAndStore(ctx, cfg, cfg.Tokens)
}

func refreshAndStore(ctx context.Context, cfg *ProxyConfig, tokens *auth.Tokens) (*auth.Tokens, error) {
	refreshed, err := cfg.refresh(ctx, tokens)
	if err != nil {
		return nil, err
	}
	// Persist the refreshed pair onto the user's existing record so the rest
	// of it (the addressed map) survives the refresh. A missing record means
	// the user logged out under a live cookie; resurrecting them here would
	// undo that, so refuse instead.
	user, err := cfg.Users.Read(cfg.Login)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, fmt.Errorf("refresh tokens: no user record for %q (logged out?)", cfg.Login)
	}
	user.Tokens = *refreshed
	if err := cfg.Users.Write(user); err != nil {
		return nil, err
	}
	cfg.Tokens = refreshed
	return refreshed, nil
}

func forward(ctx context.Context, cfg *ProxyConfig, target string, r *http.Request, tokens *auth.Tokens, body []byte) (*http.Response, error) {
	if _, err := url.Parse(target); err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bodyReader)
	if err != nil {
		return nil, err
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	return cfg.httpClient().Do(req)
}

func writeResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	for name, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-ratelimit-") {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func writeNeedsReauth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]bool{"needs_reauth": true})
}

// ForwardRequest is a small wrapper around the internal proxy() helper that
// targets an explicit URL (used for /api/installations/* where we don't want
// to expose the entire GitHub host).
func ForwardRequest(w http.ResponseWriter, r *http.Request, cfg *ProxyConfig, target string) {
	proxy(w, r, cfg, target)
}

// InstallationsPath lists the Envoy App installations visible to the user
// behind a user-to-server token.
const InstallationsPath = "/user/installations"

// InstallationOwners returns the GitHub account logins (users/orgs) covered
// by every Envoy App installation visible to the user behind cfg. It
// proactively refreshes an expiring token and retries once on a 401.
func InstallationOwners(ctx context.Context, cfg *ProxyConfig) ([]string, error) {
	tokens, err := proactivelyRefresh(ctx, cfg)
	if err != nil {
		return nil, err
	}
	resp, err := getInstallations(ctx, cfg, tokens)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		refreshed, err := refreshAndStore(ctx, cfg, tokens)
		if err != nil {
			return nil, err
		}
		if resp, err = getInstallations(ctx, cfg, refreshed); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("installations: unexpected status %d", resp.StatusCode)
	}
	var payload struct {
		Installations []struct {
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		} `json:"installations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse installations: %w", err)
	}
	owners := make([]string, 0, len(payload.Installations))
	for _, inst := range payload.Installations {
		if inst.Account.Login != "" {
			owners = append(owners, inst.Account.Login)
		}
	}
	return owners, nil
}

// getInstallations fetches one page of installations; per_page=100 is GitHub's
// maximum, well above any real installation count for one user.
func getInstallations(ctx context.Context, cfg *ProxyConfig, tokens *auth.Tokens) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+InstallationsPath+"?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	return cfg.httpClient().Do(req)
}
