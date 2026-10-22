// Package oauth is the broker's OAuth 2.0 provider (authelia.com/provider/oauth2) for a machine
// login: a device authorization grant (RFC 8628) whose tokens are DPoP-bound (RFC 9449) to the
// machine's key, with rotating refresh tokens. A machine asks for a device code and the user code
// a person types into Dispatch, Dispatch's approval calls ApproveDevice, and the machine polls the
// token endpoint for its token pair, then refreshes it. Its storage is oauthstore.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/compose"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
)

// Config is what the provider is built with.
type Config struct {
	// Secret is the HMAC secret (oauthstore.Store.Secret) every code and token is signed with.
	Secret []byte
	// DeviceLifetime is how long a device code and its user code wait for a person's decision.
	DeviceLifetime time.Duration
	// RefreshLifetime is how long a refresh token lasts; each rotation issues one with a fresh
	// lifetime, so a login lasts as long as it keeps refreshing within it.
	RefreshLifetime time.Duration
	// VerificationURL is the page a person types the user code into.
	VerificationURL string
}

// Provider runs the machine login's device authorization and token endpoints.
type Provider struct {
	oauth    oauth2.Provider
	store    *oauthstore.Store
	strategy *codeStrategy
}

// TokenHooks are the broker's own steps around a token request the library has validated.
type TokenHooks struct {
	// BeforeRefresh runs before a refresh issues anything, with the login's session extra; an
	// error refuses the refresh invalid_grant.
	BeforeRefresh func(ctx context.Context, extra map[string]any) error
	// After runs once tokens are issued, with the grant type and the session extra; its map is
	// merged into the token response.
	After func(ctx context.Context, grant string, extra map[string]any) (map[string]any, error)
}

// Refusal is an onRequest error the device authorization endpoint answers as
// {"error": "invalid_request", "error_description": "<Code>: <Message>"} with Status, and with
// Retry-After when RetryAfter (seconds) is set. Any other onRequest error is a 500 server_error.
type Refusal struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// New builds the provider on store: the device grant, the refresh grant, token revocation and DPoP,
// with every token bound to the key the device request names.
func New(c Config, store *oauthstore.Store) (*Provider, error) {
	if len(c.Secret) < 32 {
		return nil, fmt.Errorf("oauth: the secret is %d bytes, want at least 32", len(c.Secret))
	}
	if _, err := url.ParseRequestURI(c.VerificationURL); err != nil {
		return nil, fmt.Errorf("oauth: verification URL: %w", err)
	}
	cfg := &oauth2.Config{
		GlobalSecret:                c.Secret,
		RFC8628CodeLifespan:         c.DeviceLifetime,
		RFC8628TokenPollingInterval: 5 * time.Second,
		RFC8628UserVerificationURL:  c.VerificationURL,
		AccessTokenLifespan:         15 * time.Minute,
		RefreshTokenLifespan:        c.RefreshLifetime,
		RefreshTokenScopes:          []string{},
		DPoPEnabled:                 true,
		DPoPEnforce:                 true,
	}
	strategy := &codeStrategy{HMACCoreStrategy: compose.NewOAuth2HMACStrategy(cfg)}
	provider := compose.Compose(cfg, store, strategy,
		compose.RFC8628DeviceAuthorizeFactory, compose.RFC8628UserAuthorizeFactory,
		compose.RFC8628DeviceAuthorizeTokenFactory, compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2TokenRevocationFactory, compose.DPoPDeviceAuthorizeFactory, compose.DPoPTokenFactory)
	return &Provider{oauth: provider, store: store, strategy: strategy}, nil
}

// DeviceAuthorize is the device authorization endpoint. It validates the request, mints and stores
// the device code and user code, and hands onRequest the request's form and user code, all in one
// transaction (onRequest's ctx carries it: oauthstore.TxFrom), which commits only when onRequest
// succeeds. The answer omits verification_uri_complete: a person always types the code.
func (p *Provider) DeviceAuthorize(w http.ResponseWriter, r *http.Request, onRequest func(ctx context.Context, form url.Values, userCode string) error) {
	ctx := r.Context()
	request, err := p.oauth.NewRFC862DeviceAuthorizeRequest(ctx, r)
	if err != nil {
		p.writeError(ctx, w, nil, err)
		return
	}
	txCtx, err := p.store.BeginTX(ctx)
	if err != nil {
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = p.store.Rollback(txCtx)
		}
	}()
	resp, err := p.oauth.NewRFC862DeviceAuthorizeResponse(txCtx, request, &oauth2.DefaultSession{})
	if err != nil {
		p.writeError(ctx, w, nil, err)
		return
	}
	if err := onRequest(txCtx, request.GetRequestForm(), resp.GetUserCode()); err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			WriteRefusal(w, refusal)
			return
		}
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	if err := p.store.Commit(txCtx); err != nil {
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed = true
	body := resp.ToMap()
	delete(body, "verification_uri_complete")
	writeOAuthJSON(w, http.StatusOK, body)
}

// ApproveDevice records a person's decision on the device login userCode names (as typed:
// NormalizeUserCode applies), in the transaction ctx carries when it carries one. An approval
// grants the machine scope and gives the login's session extra, whose "operator" is its subject
// and which every token the login is issued carries. A code no device login waits on (none was
// issued, or its machine already holds its tokens) is oauth2.ErrNotFound; one already decided or
// expired is oauth2.ErrInvalidGrant.
func (p *Provider) ApproveDevice(ctx context.Context, userCode string, approved bool, extra map[string]any) error {
	code := NormalizeUserCode(userCode)
	signature, err := p.strategy.RFC8628UserCodeSignature(ctx, code)
	if err != nil {
		return err
	}
	// The library answers an unknown code as it answers a decided one (invalid_grant); this
	// lookup tells the two apart.
	if _, err := p.store.GetDeviceCodeSessionByUserCode(ctx, signature, nil); err != nil {
		return err
	}
	form := url.Values{"user_code": {code}}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request, err := p.oauth.NewRFC8628UserAuthorizeRequest(ctx, r)
	if err != nil {
		return err
	}
	status := oauth2.DeviceAuthorizeStatusDenied
	if approved {
		status = oauth2.DeviceAuthorizeStatusApproved
		request.GrantScope("machine")
	}
	request.SetStatus(status)
	operator, _ := extra["operator"].(string)
	session := &oauth2.DefaultSession{Subject: operator, Extra: extra}
	// The decision's session replaces the stored one, so it keeps the stored codes' expiry.
	if stored, ok := request.GetSession().(*oauth2.DefaultSession); ok {
		session.ExpiresAt = maps.Clone(stored.ExpiresAt)
	}
	_, err = p.oauth.NewRFC8628UserAuthorizeResponse(ctx, request, session)
	return err
}

// Token is the token endpoint: the device code grant and the refresh grant, each with a DPoP proof
// from the key the device request named. A refresh runs hooks.BeforeRefresh first; every issued
// pair runs hooks.After, whose fields the answer carries.
func (p *Provider) Token(w http.ResponseWriter, r *http.Request, hooks TokenHooks) {
	ctx := r.Context()
	request, err := p.oauth.NewAccessRequest(ctx, r, &oauth2.DefaultSession{})
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	grant := request.GetGrantTypes()[0]
	var extra map[string]any
	if session, ok := request.GetSession().(*oauth2.DefaultSession); ok {
		extra = session.Extra
	}
	if grant == "refresh_token" {
		if err := hooks.BeforeRefresh(ctx, extra); err != nil {
			p.writeError(ctx, w, request, oauth2.ErrInvalidGrant.WithHint("The machine login has ended.").WithWrap(err).WithDebugError(err))
			return
		}
	}
	resp, err := p.oauth.NewAccessResponse(ctx, request)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	fields, err := hooks.After(ctx, grant, extra)
	if err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	for k, v := range fields {
		resp.SetExtra(k, v)
	}
	p.oauth.WriteAccessResponse(ctx, w, request, resp)
}

// writeError answers err as the library's OAuth error JSON, logging a server error's cause.
func (p *Provider) writeError(ctx context.Context, w http.ResponseWriter, request oauth2.AccessRequester, err error) {
	if oauth2.ErrorToRFC6749Error(err).CodeField >= http.StatusInternalServerError {
		slog.Error("broker: OAuth request failed", "error", oauth2.ErrorToDebugRFC6749Error(err))
	}
	p.oauth.WriteAccessError(ctx, w, request, err)
}

// WriteRefusal answers refusal as an OAuth error a client's OAuth library parses: invalid_request,
// its description naming the broker's code.
func WriteRefusal(w http.ResponseWriter, refusal *Refusal) {
	if refusal.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(refusal.RetryAfter))
	}
	writeOAuthJSON(w, refusal.Status, map[string]string{"error": "invalid_request", "error_description": refusal.Error()})
}

// writeOAuthJSON answers status with body and the no-store headers RFC 6749 and RFC 8628 require.
func writeOAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
