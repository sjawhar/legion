package embed

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// entraDefaultAuthority is Microsoft Entra's public-cloud sign-in host.
	entraDefaultAuthority = "https://login.microsoftonline.com"
	// entraScope asks for a token Azure AI services accept.
	entraScope = "https://cognitiveservices.azure.com/.default"
	// entraAssertionType says client_assertion is a JWT: the federated token from the token file.
	entraAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// entraRefreshMargin is how long before its expiry a cached token is replaced, so a token
	// handed to a call never expires while that call is still in flight.
	entraRefreshMargin = 5 * time.Minute
	// entraExchangeTimeout bounds one exchange, so a hung sign-in never holds the exchange slot
	// every other caller waits on, whatever deadline the caller's own context carries.
	entraExchangeTimeout = 30 * time.Second
	// entraResponseLimit bounds how much of Entra's answer is read.
	entraResponseLimit = 64 << 10
)

// EntraConfig configures an EntraTokenSource: Microsoft Entra's client-credentials grant with a
// federated assertion, the keyless form of sign-in, so no Azure key is ever stored.
type EntraConfig struct {
	// TenantID is the Entra tenant the identity lives in.
	TenantID string
	// ClientID is the identity's client (application) id.
	ClientID string
	// TokenFile holds the federated token Entra trusts for ClientID (a Kubernetes projected
	// service-account token). It is re-read on every exchange, because the kubelet rotates it.
	TokenFile string
	// Authority replaces entraDefaultAuthority; a test points it at a local server.
	Authority string
	// HTTPClient replaces http.DefaultClient; a test injects one.
	HTTPClient *http.Client
}

// EntraTokenSource exchanges the federated token for an Azure access token and caches it until
// entraRefreshMargin before its expiry. At most one exchange runs at a time: callers that arrive
// during one wait for it and take its token, rather than each signing in again.
type EntraTokenSource struct {
	tokenURL  string
	clientID  string
	tokenFile string
	http      *http.Client
	now       func() time.Time

	// slot is held by whoever is reading or replacing the cache; a channel rather than a mutex so
	// a waiter whose context ends stops waiting.
	slot    chan struct{}
	token   string
	expires time.Time
}

// NewEntraTokenSource checks cfg's shape and returns an EntraTokenSource; it makes no call and
// does not read the token file yet, so a missing file fails the first Embed, not boot.
func NewEntraTokenSource(cfg EntraConfig) (*EntraTokenSource, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.TokenFile == "" {
		return nil, errors.New("embed: entra sign-in needs a tenant id, a client id and a token file")
	}
	authority := cmp.Or(cfg.Authority, entraDefaultAuthority)
	base, err := url.Parse(authority)
	if err != nil {
		return nil, fmt.Errorf("embed: entra authority: %w", err)
	}
	if !base.IsAbs() || base.Host == "" {
		return nil, errors.New("embed: entra authority must be an absolute URL with a host")
	}
	client := cmp.Or(cfg.HTTPClient, http.DefaultClient)
	return &EntraTokenSource{
		tokenURL:  strings.TrimRight(authority, "/") + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token",
		clientID:  cfg.ClientID,
		tokenFile: cfg.TokenFile,
		http:      client,
		now:       time.Now,
		slot:      make(chan struct{}, 1),
	}, nil
}

// Token returns the cached access token while it has more than entraRefreshMargin left, and
// otherwise exchanges the federated token for a new one. A failure is never a throttle: the
// caller's batch is retried as any other infrastructure failure is.
func (s *EntraTokenSource) Token(ctx context.Context) (string, error) {
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.slot }()
	if s.token != "" && s.now().Before(s.expires.Add(-entraRefreshMargin)) {
		return s.token, nil
	}
	token, lifetime, err := s.exchange(ctx)
	if err != nil {
		return "", err
	}
	s.token, s.expires = token, s.now().Add(lifetime)
	return token, nil
}

type entraTokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
}

// exchange makes one client-credentials request. Its errors name the token file's path, the HTTP
// status and Entra's error field, never the assertion or a token.
func (s *EntraTokenSource) exchange(ctx context.Context) (string, time.Duration, error) {
	raw, err := os.ReadFile(s.tokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("embed: entra read token file: %w", err)
	}
	assertion := strings.TrimSpace(string(raw))
	if assertion == "" {
		return "", 0, fmt.Errorf("embed: entra token file %s is empty", s.tokenFile)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {s.clientID},
		"client_assertion_type": {entraAssertionType},
		"client_assertion":      {assertion},
		"scope":                 {entraScope},
	}
	ctx, cancel := context.WithTimeout(ctx, entraExchangeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("embed: entra build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := s.http.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("embed: entra token exchange: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, entraResponseLimit))
	if err != nil {
		return "", 0, fmt.Errorf("embed: entra read response: %w", err)
	}
	var decoded entraTokenResponse
	decodeErr := json.Unmarshal(body, &decoded)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if decoded.Error != "" {
			return "", 0, fmt.Errorf("embed: entra token exchange: HTTP %s, error %q", response.Status, decoded.Error)
		}
		return "", 0, fmt.Errorf("embed: entra token exchange: HTTP %s", response.Status)
	}
	if decodeErr != nil {
		return "", 0, fmt.Errorf("embed: entra decode response: %w", decodeErr)
	}
	if decoded.AccessToken == "" {
		return "", 0, errors.New("embed: entra answered no access_token")
	}
	lifetime, err := entraLifetime(decoded.ExpiresIn)
	if err != nil {
		return "", 0, err
	}
	return decoded.AccessToken, lifetime, nil
}

// entraLifetime reads expires_in, the token's lifetime in seconds, which may arrive as a JSON
// number or as a string; both are accepted.
func entraLifetime(raw json.RawMessage) (time.Duration, error) {
	value := strings.Trim(string(raw), `"`)
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("embed: entra answered expires_in %q, want a positive number of seconds", value)
	}
	return time.Duration(seconds) * time.Second, nil
}
