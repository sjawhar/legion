// Package brokerclient is this repository's one Go client of the secrets broker's RFC 8693 token
// exchange: a machine or a person trades the platform identity it already holds for one of the
// broker's access tokens, which Dispatch and the Envoy listener read back at the broker's
// introspection route. `agent-secrets`, `agent-secrets-helper` and `legion controller start` use
// it; the TypeScript client of packages/broker-client is its other half, and the two are held to
// one wire contract by the corpus in testdata/wire.json, which both suites replay.
//
// What the exchange is: the caller presents the subject token its platform already gives it (a
// Source), proves a key of its own with a DPoP proof (RFC 9449), and is answered an access token
// bound to that key. The answer carries no refresh token at all — the grant issues none — so a
// renewal is a fresh subject token traded again, which is why a Source is read at every trade
// rather than once. A person's sign-in trades their own Google identity token with the machine's
// as the actor token (RFC 8693 Section 2.1), which is the one request that carries two tokens.
//
// Each process makes its own key and sends only its public half: the proof embeds the public JWK,
// and no code here marshals a private member onto the wire.
//
// What it refuses to do: hand out a token it could not renew. Token answers the held token while
// that token is live and the broker is merely unreachable, and answers the error once the broker
// has refused the identity or the held token has run out, so a caller that writes only with what
// Token gave it never writes with no identity.
package brokerclient

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

const (
	// TokenPath is the broker's token endpoint, where every trade is posted.
	TokenPath = "/v1/oauth/token"
	// GrantTypeTokenExchange is RFC 8693's own grant name: what every trade at the broker's
	// token endpoint asks for, and what Google's security token service takes for the same
	// exchange (pool.go). The library the broker is built on spells it in a package its callers
	// cannot import.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"

	// DefaultRenewBefore is how long before an access token's expiry the client trades again, so
	// a renewal lands with the held token still good and nothing of the caller's restarts. The
	// broker mints a machine's token for an hour or less.
	DefaultRenewBefore = 5 * time.Minute
	// defaultRetryWait is the wait after a trade the broker did not answer or answered 5xx,
	// doubling to maxRetryWait. The grant has no rotation window to miss, so an outage longer
	// than one token's life is the same event as a short one: the loop keeps trading until the
	// broker answers again.
	defaultRetryWait = 15 * time.Second
	maxRetryWait     = 5 * time.Minute
	// tradeTimeout bounds one trade, the token endpoint being one request.
	tradeTimeout = 30 * time.Second
)

// Token is one of the broker's access tokens as this client holds it.
type Token struct {
	// AccessToken is the token itself, presented as `Authorization: DPoP <token>` beside a
	// proof of the key it is bound to.
	AccessToken string
	// TokenType is the answer's token_type, DPoP for every token this grant mints.
	TokenType string
	// Expiry is when the token lapses, from the answer's expires_in.
	Expiry time.Time
	// CredentialID is the launcher credential the trade minted for this machine or sign-in
	// (oauthwire.ExtraCredentialID), under which sessions of this machine enrol. It is empty
	// where the answer carries none.
	CredentialID string
	// CredentialExpiresAt is when that credential lapses (oauthwire.ExtraCredentialExpiresAt),
	// zero where the answer carries none.
	CredentialExpiresAt time.Time
}

// RefusedError is a trade the broker refused: a plain error naming what it said, which no retry
// can cure — an identity no entry lists, a sign-in a person revoked, a token of the wrong
// audience. The renewal loop stops on it rather than asking again every quarter minute.
type RefusedError struct {
	// Status is the HTTP status of the refusal.
	Status int
	// Code and Description are the OAuth error the broker answered.
	Code, Description string
}

func (e *RefusedError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("the secrets broker refused this machine's identity: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("the secrets broker refused this machine's identity: HTTP %d %s: %s", e.Status, e.Code, e.Description)
}

// Config is one client: the broker it trades at, the identity it trades, and the key it proves.
type Config struct {
	// BrokerURL is the broker's base URL, with or without a trailing slash.
	BrokerURL string
	// Key is this process's own DPoP key, whose public half alone reaches the broker. A person's
	// sign-in proves the box key `agent-secrets login` made; every other caller proves a key it
	// made at start.
	Key *ecdsa.PrivateKey
	// Subject is the identity being traded.
	Subject Source
	// Actor is the machine a person signed in on, presented as the actor token beside their own
	// Google identity token. Nil for every trade a machine makes for itself.
	Actor Source
	// Session is the session name the minted token writes under, which an entry that fixes one
	// refuses to be told and an entry that fixes none requires.
	Session string
	// HTTP is the client every call goes through; nil is http.DefaultClient. The DPoP proof is
	// signed by a transport this client wraps around it, so a caller passing its own client
	// keeps its timeout and its dialler.
	HTTP *http.Client
	// Log records each trade and each failed renewal: the identity, the session and the
	// broker's refusal code, never a token. Nil logs nothing.
	Log *slog.Logger
	// RenewBefore overrides DefaultRenewBefore. Only a test shortens it.
	RenewBefore time.Duration
	// Now and After are time.Now and time.After unless a test replaces them.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// Client trades Config's identity for the broker's access tokens and keeps one live.
type Client struct {
	brokerURL   string
	key         *ecdsa.PrivateKey
	subject     Source
	actor       Source
	session     string
	http        *http.Client
	log         *slog.Logger
	renewBefore time.Duration
	now         func() time.Time
	after       func(time.Duration) <-chan time.Time

	mu   sync.Mutex
	held *Token
}

// New is the client Config describes. It refuses a configuration that could never trade — no
// broker, no key, no identity — rather than failing at the first call.
func New(cfg Config) (*Client, error) {
	switch {
	case strings.TrimSpace(cfg.BrokerURL) == "":
		return nil, errors.New("brokerclient: no broker URL")
	case cfg.Key == nil:
		return nil, errors.New("brokerclient: no DPoP key: a trade is judged on a proof of the caller's own key")
	case cfg.Subject == nil:
		return nil, errors.New("brokerclient: no subject token source: nothing names the identity to trade")
	}
	client := &Client{
		brokerURL:   strings.TrimSuffix(strings.TrimSpace(cfg.BrokerURL), "/"),
		key:         cfg.Key,
		subject:     cfg.Subject,
		actor:       cfg.Actor,
		session:     cfg.Session,
		http:        cfg.HTTP,
		log:         cfg.Log,
		renewBefore: cfg.RenewBefore,
		now:         cfg.Now,
		after:       cfg.After,
	}
	if client.http == nil {
		client.http = http.DefaultClient
	}
	if client.renewBefore <= 0 {
		client.renewBefore = DefaultRenewBefore
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.after == nil {
		client.after = time.After
	}
	return client, nil
}

// Key is the key this client proves, whose public half the broker records: a caller that enrols
// under the minted credential signs with it.
func (c *Client) Key() *ecdsa.PrivateKey { return c.key }

// Token is a live access token: the held one while it is good for longer than RenewBefore, and
// otherwise a fresh trade. A trade the broker did not answer while the held token is still live
// answers that token, since a client that already holds an identity need not stop for a broker
// that is merely down; a refusal, and a failure with nothing live left, answer the error, so no
// caller writes with no identity.
func (c *Client) Token(ctx context.Context) (*Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held != nil && c.now().Before(c.held.Expiry.Add(-c.renewBefore)) {
		return c.held, nil
	}
	token, err := c.trade(ctx)
	if err == nil {
		c.held = token
		return token, nil
	}
	var refused *RefusedError
	if !errors.As(err, &refused) && c.held != nil && c.now().Before(c.held.Expiry) {
		c.logf(ctx, "the secrets broker did not answer a renewal; the token in hand is still live", "error", err.Error())
		return c.held, nil
	}
	c.held = nil
	return nil, err
}

// Run keeps one token live until ctx ends, handing each one to onToken as it lands: the first
// trade, and every renewal RenewBefore ahead of the token in hand. It is the shape a caller with
// something to keep fresh takes — a credential to install, a token file to rewrite — and
// nothing of that caller restarts across a renewal.
//
// It stops with a plain error when the broker refuses the identity, which no retry can cure, and
// when onToken fails. A broker that does not answer is retried, from defaultRetryWait doubling
// to maxRetryWait, for as long as ctx lives: an outage past the token's own life is the same
// event as a short one, since this grant has no rotation window to miss. A cancelled ctx is no
// error.
func (c *Client) Run(ctx context.Context, onToken func(context.Context, *Token) error) error {
	wait := defaultRetryWait
	for {
		c.mu.Lock()
		token, err := c.trade(ctx)
		if err == nil {
			c.held = token
		}
		c.mu.Unlock()
		switch {
		case err == nil:
			wait = defaultRetryWait
			if err := onToken(ctx, token); err != nil {
				return err
			}
			if !c.sleep(ctx, c.untilRenewal(token)) {
				return nil
			}
			continue
		case ctx.Err() != nil:
			return nil
		}
		var refused *RefusedError
		if errors.As(err, &refused) {
			return err
		}
		c.logf(ctx, "the secrets broker did not answer a trade; asking again", "error", err.Error(), "in", wait.String())
		if !c.sleep(ctx, wait) {
			return nil
		}
		if wait *= 2; wait > maxRetryWait {
			wait = maxRetryWait
		}
	}
}

// untilRenewal is how long to wait before trading again for the token in hand: RenewBefore ahead
// of its expiry, and at once for one that is already inside that window.
func (c *Client) untilRenewal(token *Token) time.Duration {
	wait := token.Expiry.Add(-c.renewBefore).Sub(c.now())
	if wait < 0 {
		return 0
	}
	return wait
}

// sleep waits d, reporting false when ctx ended first.
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-c.after(d):
		return true
	}
}

func (c *Client) logf(ctx context.Context, message string, args ...any) {
	if c.log == nil {
		return
	}
	c.log.InfoContext(ctx, message, append([]any{"broker", c.brokerURL, "identity", c.subject.Describe()}, args...)...)
}

// trade is one RFC 8693 exchange: the subject token this moment's Source answers, the actor
// token where a person is signing in, the session name, and a DPoP proof of this client's key.
func (c *Client) trade(ctx context.Context) (*Token, error) {
	subject, subjectType, err := c.subject.Token(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":         {GrantTypeTokenExchange},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subject},
		"subject_token_type": {subjectType},
	}
	if c.session != "" {
		form.Set(oauthwire.FormSession, c.session)
	}
	if c.actor != nil {
		actor, actorType, err := c.actor.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the machine this person signed in on: %w", err)
		}
		if actorType == oauthwire.SubjectTokenTypeGoogle {
			// The same token under the type that says what it proves here: not who is
			// asking, but which box the person is asking from.
			actorType = oauthwire.ActorTokenTypeMachine
		}
		form.Set("actor_token", actor)
		form.Set("actor_token_type", actorType)
	}
	answer, err := c.post(ctx, form)
	if err != nil {
		return nil, err
	}
	c.logf(ctx, "traded this machine's identity for one of the broker's access tokens",
		"session", c.session, "credential", answer.CredentialID, "expires", answer.Expiry.UTC().Format(time.RFC3339))
	return answer, nil
}

// tokenAnswer is the broker's token answer as this client reads it. Every member the contract
// names is here and nothing else is read: the corpus holds the client to exactly this set.
type tokenAnswer struct {
	AccessToken         string      `json:"access_token"`
	TokenType           string      `json:"token_type"`
	IssuedTokenType     string      `json:"issued_token_type"`
	ExpiresIn           json.Number `json:"expires_in"`
	RefreshToken        string      `json:"refresh_token"`
	CredentialID        string      `json:"credential_id"`
	CredentialExpiresAt string      `json:"credential_expires_at"`
	Error               string      `json:"error"`
	ErrorDescription    string      `json:"error_description"`
}

// post sends one trade and reads the answer. A refusal the broker answered with an OAuth error
// is a RefusedError; everything else — a broker that did not answer, a 5xx, an answer that is
// not this grant's — is an error a renewal may ask about again.
func (c *Client) post(ctx context.Context, form url.Values) (*Token, error) {
	ctx, cancel := context.WithTimeout(ctx, tradeTimeout)
	defer cancel()
	endpoint := c.brokerURL + TokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	proofed := &http.Client{Transport: NewDPoPTransport(c.key, base), Timeout: c.http.Timeout}
	resp, err := proofed.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trade %s at %s: %w", c.subject.Describe(), endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read the broker's answer from %s: %w", endpoint, err)
	}
	var answer tokenAnswer
	if err := json.Unmarshal(body, &answer); err != nil && resp.StatusCode/100 == 2 {
		return nil, fmt.Errorf("the broker's answer from %s is not JSON: %w", endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode/100 == 4 && answer.Error != "" {
			return nil, &RefusedError{Status: resp.StatusCode, Code: answer.Error, Description: answer.ErrorDescription}
		}
		return nil, fmt.Errorf("the broker at %s answered HTTP %d %s", endpoint, resp.StatusCode, answer.Error)
	}
	return c.read(endpoint, answer)
}

// read is the answer as a Token, refusing one that is not this grant's: no access token, a token
// type that is not DPoP, no expiry, or a refresh token — the exchange issues none, and a client
// that kept one would hold a credential this contract says is never minted.
func (c *Client) read(endpoint string, answer tokenAnswer) (*Token, error) {
	switch {
	case answer.AccessToken == "":
		return nil, fmt.Errorf("the broker at %s answered no access token", endpoint)
	case !strings.EqualFold(answer.TokenType, "DPoP"):
		return nil, fmt.Errorf("the broker at %s answered token_type %q, and this grant mints DPoP-bound tokens alone", endpoint, answer.TokenType)
	case answer.RefreshToken != "":
		return nil, fmt.Errorf("the broker at %s answered a refresh token, which this grant never issues; a renewal trades a fresh identity token instead", endpoint)
	}
	seconds, err := answer.ExpiresIn.Int64()
	if err != nil || seconds <= 0 {
		return nil, fmt.Errorf("the broker at %s answered no usable expires_in (%q)", endpoint, answer.ExpiresIn.String())
	}
	token := &Token{
		AccessToken:  answer.AccessToken,
		TokenType:    answer.TokenType,
		Expiry:       c.now().Add(time.Duration(seconds) * time.Second),
		CredentialID: answer.CredentialID,
	}
	if answer.CredentialExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339, answer.CredentialExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("the broker at %s answered a %s that is no RFC 3339 time (%q)", endpoint, oauthwire.ExtraCredentialExpiresAt, answer.CredentialExpiresAt)
		}
		token.CredentialExpiresAt = expires.UTC()
	}
	return token, nil
}
