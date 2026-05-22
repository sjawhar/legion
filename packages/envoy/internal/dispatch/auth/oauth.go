// Shared OAuth helpers used by both the web flow (auth/webflow.go) and the
// token-refresh path. GitHub Apps' /login/oauth/access_token endpoint accepts
// both code-exchange and refresh-token grant types; both go through
// postForm + tokensFromOAuthBody.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPClient lets tests substitute a fake transport. nil means use the
// package default.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

func resolveClient(c HTTPClient) HTTPClient {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// RefreshTokens exchanges a refresh token for a fresh access+refresh pair.
// GitHub App user-to-server tokens issue refresh tokens by default (8h
// access / 6mo refresh); the proxy calls this proactively before the access
// token expires.
func RefreshTokens(ctx context.Context, clientID, clientSecret, refreshToken string, client HTTPClient) (*Tokens, error) {
	c := resolveClient(client)
	body, err := postForm(ctx, c, "https://github.com/login/oauth/access_token", url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return nil, err
	}
	if errVal, _ := body["error"].(string); errVal != "" {
		return nil, fmt.Errorf("refresh failed: %s", errVal)
	}
	return tokensFromOAuthBody(ctx, c, body)
}

// tokensFromOAuthBody parses the standard OAuth access_token response and
// enriches it with the user's login (fetched via GET /user). The /user call
// is what makes ExchangeCode and RefreshTokens single-RTT-from-caller's
// perspective.
func tokensFromOAuthBody(ctx context.Context, c HTTPClient, body map[string]any) (*Tokens, error) {
	now := time.Now().UnixMilli()
	access, err := stringField(body, "access_token")
	if err != nil {
		return nil, err
	}
	refresh, err := stringField(body, "refresh_token")
	if err != nil {
		return nil, err
	}
	expiresIn, err := intField(body, "expires_in")
	if err != nil {
		return nil, err
	}
	refreshExpiresIn, err := intField(body, "refresh_token_expires_in")
	if err != nil {
		return nil, err
	}
	login, err := fetchLogin(ctx, c, access)
	if err != nil {
		return nil, err
	}
	return &Tokens{
		AccessToken:      access,
		RefreshToken:     refresh,
		AccessExpiresAt:  now + int64(expiresIn)*1000,
		RefreshExpiresAt: now + int64(refreshExpiresIn)*1000,
		GithubLogin:      login,
	}, nil
}

func fetchLogin(ctx context.Context, c HTTPClient, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode /user: %w", err)
	}
	return stringField(body, "login")
}

func postForm(ctx context.Context, c HTTPClient, endpoint string, values url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("decode oauth response: %w", err)
	}
	return body, nil
}

func stringField(body map[string]any, field string) (string, error) {
	v, ok := body[field].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("GitHub OAuth response missing %s", field)
	}
	return v, nil
}

func intField(body map[string]any, field string) (int, error) {
	v, ok := body[field].(float64)
	if !ok {
		return 0, fmt.Errorf("GitHub OAuth response missing %s", field)
	}
	return int(v), nil
}
