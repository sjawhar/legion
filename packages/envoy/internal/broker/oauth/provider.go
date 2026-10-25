// Package oauth is the broker's OAuth 2.0 provider (authelia.com/provider/oauth2) for a machine
// login: a device authorization grant (RFC 8628) whose tokens are DPoP-bound (RFC 9449) to the
// machine's key, with rotating refresh tokens. A machine asks for a device code and the user code
// a person types into Dispatch, Dispatch's approval calls ApproveDevice, and the machine polls the
// token endpoint for its token pair, then refreshes it. Its storage is oauthstore.
//
// The same token endpoint runs the RFC 8693 token exchange: a pod, a continuous-integration
// workflow run or a dev box session presents the identity its platform already gave it and is
// answered a short-lived Dispatch access token, bound to its own key, saying which session is
// writing, which person it acts for and which project it may write. Those tokens stay opaque, so
// Dispatch reads one back through Introspect, the RFC 7662 answer.
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
	"sync"
	"sync/atomic"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/compose"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
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
	// AccessLifetime is how long an issued access token lasts, a machine login's and an
	// exchanged one's alike. A caller renews within it rather than holding one token for a whole
	// run, so the setting is what lets a deployment trade how often a caller renews against how
	// long a revoked credential's last token stays spendable.
	AccessLifetime time.Duration
	// VerificationURL is the page a person types the user code into.
	VerificationURL string
	// ProofSkew is BROKER_PROOF_SKEW_SECONDS: a DPoP proof is accepted while the broker's clock is
	// within this of its iat, as every other broker proof is (proof.Verifier.Skew), give or take
	// the one second the library's proof lifespan adds after it.
	ProofSkew time.Duration
	// Exchange is what the RFC 8693 token exchange decides and verifies with: the listed
	// identities, and the issuers whose tokens it accepts as a subject token.
	Exchange ExchangeConfig
}

// Provider runs the machine login's device authorization and token endpoints, and the token
// exchange that mints a Dispatch access token from a caller's platform identity.
type Provider struct {
	oauth    oauth2.Provider
	store    *oauthstore.Store
	strategy *codeStrategy
	// exchange is Config.Exchange, which the exchange's own step decides by.
	exchange ExchangeConfig
	// reservations are the session names the exchange's entries fix, each mapped to the
	// principal it belongs to, which reserveSessions holds before this provider's first exchange.
	reservations map[string]string
	// reserved says reserveSessions has held them; reserving serializes the first exchanges
	// while it does.
	reserved  atomic.Bool
	reserving sync.Mutex
}

// ErrLoginEnded is the TokenHooks.Before error (wrapped or not) that refuses a token request
// invalid_grant: the machine login has ended, so its client stops using it. Any other Before error
// is a 500 server_error, which a client retries.
var ErrLoginEnded = errors.New("the machine login has ended")

// TokenHooks are the broker's own steps around a token request the library has validated.
type TokenHooks struct {
	// Before runs before either grant issues anything, with the grant type and the login's session
	// extra; ErrLoginEnded refuses the request invalid_grant, and any other error answers
	// server_error.
	Before func(ctx context.Context, grant oauth2.GrantType, extra map[string]any) error
	// After runs once tokens are issued, with the grant type and the session extra; its map is
	// merged into the token response.
	After func(ctx context.Context, grant oauth2.GrantType, extra map[string]any) (map[string]any, error)
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

// New builds the provider on store: the device grant, the refresh grant, the RFC 8693 token
// exchange, token revocation, introspection and DPoP, with every token bound to the key the
// request's proof names. It is the exchange's wiring too, so it sets the decision the store's
// GetSubjectForTokenExchange calls (Store.ExchangeSubject): the store holds no policy of its
// own, and the grant is composed here whether or not any issuer is.
func New(c Config, store *oauthstore.Store) (*Provider, error) {
	if len(c.Secret) < 32 {
		return nil, fmt.Errorf("oauth: the secret is %d bytes, want at least 32", len(c.Secret))
	}
	if _, err := url.ParseRequestURI(c.VerificationURL); err != nil {
		return nil, fmt.Errorf("oauth: verification URL: %w", err)
	}
	if c.DeviceLifetime <= 0 || c.RefreshLifetime <= 0 || c.AccessLifetime <= 0 {
		return nil, fmt.Errorf("oauth: the device lifetime (%v), refresh lifetime (%v) and access lifetime (%v) must all be positive", c.DeviceLifetime, c.RefreshLifetime, c.AccessLifetime)
	}
	if c.ProofSkew <= 0 {
		return nil, fmt.Errorf("oauth: the proof skew (%v) must be positive", c.ProofSkew)
	}
	if err := c.Exchange.validate(); err != nil {
		return nil, err
	}
	// A deployment that has listed no identity gets the empty set rather than a nil one, so the
	// exchange composes and refuses every caller by name instead of faulting on a map nobody
	// built.
	if c.Exchange.Policy == nil {
		policy, err := exchange.NewSet(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("oauth: the empty exchange policy: %w", err)
		}
		c.Exchange.Policy = policy
	}
	cfg := &oauth2.Config{
		GlobalSecret:                c.Secret,
		RFC8628CodeLifespan:         c.DeviceLifetime,
		RFC8628TokenPollingInterval: 5 * time.Second,
		RFC8628UserVerificationURL:  c.VerificationURL,
		AccessTokenLifespan:         c.AccessLifetime,
		RefreshTokenLifespan:        c.RefreshLifetime,
		RefreshTokenScopes:          []string{},
		DPoPEnabled:                 true,
		DPoPEnforce:                 true,
		// The library accepts a proof from DPoPClockSkew before its iat until DPoPProofLifespan
		// plus DPoPClockSkew after it; a lifespan of one second (zero is its 10 s default) keeps
		// that window the broker's own.
		DPoPClockSkew:             c.ProofSkew,
		DPoPProofLifespan:         time.Second,
		RFC8693TokenTypes:         c.Exchange.tokenTypes(),
		DefaultRequestedTokenType: tokenTypeAccessToken,
	}
	cfg.ClientAuthenticationStrategy = newExchangeClientAuth(store, cfg)
	strategy := &codeStrategy{HMACCoreStrategy: compose.NewOAuth2HMACStrategy(cfg)}
	// The RFC 8693 factories run in the order compose/compose_rfc8693.go states: the grant
	// handler first, so the 'act' claim is on the session before a token-type handler issues
	// anything, and the actor-token validation last, so the token-type handlers have put an actor
	// token on the session for it to read. The ID token type is left out: this provider issues no
	// ID tokens, and that factory asserts the composed strategy to an issuing interface
	// codeStrategy has no reason to answer.
	provider := compose.Compose(cfg, store, strategy,
		compose.RFC8628DeviceAuthorizeFactory, compose.RFC8628UserAuthorizeFactory,
		compose.RFC8628DeviceAuthorizeTokenFactory, compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2TokenRevocationFactory, compose.OAuth2TokenIntrospectionFactory,
		compose.DPoPDeviceAuthorizeFactory, compose.DPoPTokenFactory,
		compose.RFC8693TokenExchangeGrantFactory, compose.RFC8693AccessTokenTypeFactory,
		compose.RFC8693RefreshTokenTypeFactory, compose.RFC8693CustomJWTTypeFactory,
		compose.RFC8693ActorTokenValidationFactory)
	reservations := map[string]string{}
	for _, reservation := range c.Exchange.Policy.Reservations() {
		reservations[reservation.Session] = reservation.Principal
	}
	p := &Provider{oauth: provider, store: store, strategy: strategy, exchange: c.Exchange, reservations: reservations}
	// A provider built with no store serves no request at all and is only ever inspected, as the
	// broker's route wiring test inspects it, so there is nothing there to give the decision to.
	if store != nil {
		store.ExchangeSubject = p.decide
	}
	return p, nil
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
		request.GrantScope(oauthwire.Scope)
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

// Token is the token endpoint: the device code grant, the refresh grant and the RFC 8693 token
// exchange, each with a DPoP proof from the key the grant is bound to. An exchange is answered by
// exchange, which runs none of the hooks: they are the machine login's own steps, and an
// exchanged token follows no login. Either login grant runs hooks.Before first (ErrLoginEnded
// refuses it invalid_grant, any other error is server_error); every issued pair
// runs hooks.After, whose fields the answer carries. The hooks and the issued tokens share one
// transaction (their ctx carries it: oauthstore.TxFrom), committed only once the answer is built,
// so a row Before locks stays locked until the new tokens are stored. Validating the request stays
// outside it: a reused refresh token's family revocation commits on its own though the request is
// refused.
func (p *Provider) Token(w http.ResponseWriter, r *http.Request, hooks TokenHooks) {
	ctx := r.Context()
	// Read before the library does, because the two kinds of request need different sessions and
	// the exchange needs its whole request in one transaction; a body the library cannot parse
	// is left for it to refuse in its own words.
	if err := r.ParseForm(); err == nil && r.PostForm.Get("grant_type") == string(oauth2.GrantTypeTokenExchange) {
		p.exchangeGrant(w, r)
		return
	}
	request, err := p.oauth.NewAccessRequest(ctx, r, &oauth2.DefaultSession{})
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	grant := oauth2.GrantType(request.GetGrantTypes()[0])
	var extra map[string]any
	if session, ok := request.GetSession().(*oauth2.DefaultSession); ok {
		extra = session.Extra
	}
	txCtx, err := p.store.BeginTX(ctx)
	if err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = p.store.Rollback(txCtx)
		}
	}()
	if err := hooks.Before(txCtx, grant, extra); err != nil {
		refusal := oauth2.ErrServerError
		if errors.Is(err, ErrLoginEnded) {
			refusal = oauth2.ErrInvalidGrant.WithHint("The machine login has ended.")
		}
		p.writeError(ctx, w, request, refusal.WithWrap(err).WithDebugError(err))
		return
	}
	resp, err := p.oauth.NewAccessResponse(txCtx, request)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	fields, err := hooks.After(txCtx, grant, extra)
	if err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	if err := p.store.Commit(txCtx); err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed = true
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
