// packages/envoy/internal/brokerclient/brokerclient.go
//
// Package brokerclient is the Go half of the secrets broker's token client: the one place in
// this module that trades an identity for a broker access token and keeps that token fresh.
// Its TypeScript half speaks the same wire, and `packages/broker-client/testdata/wire.json` is
// that wire written down — one entry per exchange, the form each posts, the proof's claims and
// the answers each must accept or refuse — which both suites replay, so "one contract" is
// something a test fails on rather than something a comment claims.
//
// What it holds:
//
//   - The RFC 9449 DPoP signer (dpop.go), which the agent-secrets helper also uses: one signer in
//     the repository. Each session makes its own key in its own process and the proof carries the
//     public half alone.
//   - Three subject-token sources (source.go, pool.go): a Kubernetes projected service-account
//     token read from a file, a Compute Engine metadata identity token, and a Google identity
//     token minted through a workload identity pool. A fourth, StaticSource, presents a token the
//     caller already holds.
//   - The RFC 8693 trade and the renewal loop (below).
//   - Google's device authorization grant, which signs a person in on a machine (device.go).
//
// The trade posts its own form rather than going through golang.org/x/oauth2's Config.Exchange:
// that method always posts a `code` parameter of its own beside the grant, and the wire contract
// has no such field, so the one client would be sending what the other does not. Every flow
// x/oauth2 implements exactly — the device grant, and the refresh that mints a signed-in person's
// next identity token — goes through it (device.go).
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
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// GrantTypeTokenExchange is RFC 8693's grant type, the one this client asks the broker's
	// token endpoint for.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// TokenPath is the broker's token endpoint, under its base URL.
	TokenPath = "/v1/oauth/token"
	// DefaultRenewFraction is how much of an access token's life the client spends before it
	// trades again: half of it, so a trade that fails has the other half to succeed in before
	// the token it holds lapses.
	DefaultRenewFraction = 2
	// DefaultRetryInterval is how long the renewal loop waits after a trade that failed while
	// the token it holds is still live. It matches the helper's refresh retry: a broker that is
	// down for minutes is met by a client that asks again every few seconds.
	DefaultRetryInterval = 15 * time.Second
)

// Token is one access token the exchange minted: the token itself, when it lapses, and the
// launcher credential the trade minted or re-used, for the caller that enrols sessions under it.
// A caller that only writes to Dispatch reads AccessToken and Expiry alone.
//
// There is no refresh token, and there is no field for one: this grant issues none, and a
// renewal is a fresh subject token traded again.
type Token struct {
	AccessToken         string
	Expiry              time.Time
	CredentialID        string
	CredentialExpiresAt time.Time
}

// Error is a refusal the broker answered the trade with: its OAuth error code and description,
// and the status it came under. Its message is one plain line, for a person reading a log.
type Error struct {
	Status      int
	Code        string
	Description string
}

func (e *Error) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("the broker refused the token exchange: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("the broker refused the token exchange: HTTP %d %s: %s", e.Status, e.Code, e.Description)
}

// Client trades one identity for the broker's access tokens and keeps that token fresh.
//
// Subject is the identity being traded. Actor, when set, is the machine a signed-in person is
// asking from, presented under the actor token type (RFC 8693 section 2.1); the person's own
// identity is then the subject. Key is the key this process made for itself: every request the
// client makes carries a DPoP proof of it, and the broker binds the token it mints to it.
type Client struct {
	// BrokerURL is the broker's base URL, with no trailing slash.
	BrokerURL string
	// Key is this process's own key. The broker is sent its public half alone.
	Key *ecdsa.PrivateKey
	// Subject and Actor are the identities the trade presents.
	Subject Source
	Actor   Source
	// Session is the name the caller asks to write under, which the broker binds to the
	// identity it decides. An entry that fixes a session refuses a request naming another;
	// an entry that fixes none requires it.
	Session string
	// HTTP is the client the trade goes through, and is http.DefaultClient when nil. Its
	// transport is wrapped in the DPoP signer for every call.
	HTTP *http.Client
	// Log is where the renewal loop reports; it reports nothing when nil.
	Log *slog.Logger
	// RetryInterval overrides DefaultRetryInterval, and Now and After are the clock, which a
	// test replaces.
	RetryInterval time.Duration
	Now           func() time.Time
	After         func(time.Duration) <-chan time.Time
}

// NewKey is a fresh P-256 key for one process's own use, the key its DPoP proofs are signed with
// and the broker binds its tokens to.
func NewKey() (*ecdsa.PrivateKey, error) { return proof.NewKey() }

// Thumbprint is the JWK thumbprint of a key's public half, the name the broker knows it by.
func Thumbprint(key *ecdsa.PrivateKey) (string, error) { return proof.Thumbprint(&key.PublicKey) }

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Client) after(d time.Duration) <-chan time.Time {
	if c.After == nil {
		return time.After(d)
	}
	return c.After(d)
}

func (c *Client) retryInterval() time.Duration {
	if c.RetryInterval <= 0 {
		return DefaultRetryInterval
	}
	return c.RetryInterval
}

func (c *Client) logger() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// tokenURL is the broker's token endpoint.
func (c *Client) tokenURL() string {
	return strings.TrimSuffix(c.BrokerURL, "/") + TokenPath
}

// httpClient is the client every call goes through, with a DPoP proof of c.Key signed onto each.
func (c *Client) httpClient() *http.Client {
	base := http.DefaultClient
	if c.HTTP != nil {
		base = c.HTTP
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{Transport: NewDPoPTransport(c.Key, transport), Timeout: base.Timeout}
}

// form is the exchange request's body: the fields packages/broker-client/testdata/wire.json
// records for this client's exchange and no others. It is the one place the wire is built, so
// the corpus has one thing to hold to.
func (c *Client) form(subject, actor string) url.Values {
	form := url.Values{
		"grant_type":         {GrantTypeTokenExchange},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subject},
		"subject_token_type": {c.Subject.TokenType()},
		"session":            {c.Session},
	}
	if actor != "" {
		form.Set("actor_token", actor)
		form.Set("actor_token_type", oauthwire.ActorTokenTypeMachine)
	}
	return form
}

// Trade asks the broker's token endpoint for an access token, presenting the subject token its
// source answers now, the actor token when there is one, and a DPoP proof of this process's key.
// It returns the broker's own refusal as an *Error, so a caller can log one plain line, and
// every other failure as the error it was, with the broker's URL redacted.
func (c *Client) Trade(ctx context.Context) (*Token, error) {
	switch {
	case c.BrokerURL == "":
		return nil, errors.New("the token client has no broker URL")
	case c.Key == nil:
		return nil, errors.New("the token client has no key of its own to prove")
	case c.Subject == nil:
		return nil, errors.New("the token client has no identity to trade")
	case c.Session == "":
		return nil, errors.New("the token client has no session name to ask under")
	}
	subject, err := c.Subject.Token(ctx)
	if err != nil {
		return nil, err
	}
	actor := ""
	if c.Actor != nil {
		if actor, err = c.Actor.Token(ctx); err != nil {
			return nil, err
		}
	}
	body := c.form(subject, actor).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(body))
	if err != nil {
		return nil, redact.Error(err, c.BrokerURL)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, redact.Error(err, c.BrokerURL)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		refusal := &Error{Status: resp.StatusCode}
		var answer struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(data, &answer) == nil {
			refusal.Code, refusal.Description = answer.Error, answer.Description
		}
		if refusal.Code == "" {
			refusal.Code = "no_oauth_error"
			refusal.Description = firstBytes(data, 512)
		}
		return nil, refusal
	}
	return c.read(data)
}

// read is the token answer as a Token, and refuses an answer that is not this grant's: one
// without an access token, and one carrying a refresh token, which this grant never issues and
// no client of it ever keeps.
func (c *Client) read(data []byte) (*Token, error) {
	var answer struct {
		AccessToken         string `json:"access_token"`
		TokenType           string `json:"token_type"`
		ExpiresIn           int64  `json:"expires_in"`
		RefreshToken        string `json:"refresh_token"`
		CredentialID        string `json:"credential_id"`
		CredentialExpiresAt string `json:"credential_expires_at"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return nil, fmt.Errorf("the broker's token answer is not the JSON object it must be: %w", err)
	}
	switch {
	case answer.AccessToken == "":
		return nil, errors.New("the broker's token answer names no access_token")
	case answer.RefreshToken != "":
		return nil, errors.New("the broker's token answer carries a refresh_token: this grant issues none, and the client keeps none")
	}
	token := &Token{AccessToken: answer.AccessToken}
	if answer.ExpiresIn > 0 {
		token.Expiry = c.now().Add(time.Duration(answer.ExpiresIn) * time.Second)
	}
	token.CredentialID = answer.CredentialID
	if answer.CredentialExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, answer.CredentialExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("the broker's token answer carries a %s that is no RFC 3339 time: %w", oauthwire.ExtraCredentialExpiresAt, err)
		}
		token.CredentialExpiresAt = expiresAt.UTC()
	}
	return token, nil
}

// Run keeps a token in hold's hands until ctx ends: it trades, hands the token over, and trades
// again when half the token's life has passed, so the caller is never restarted and never pauses
// for a renewal.
//
// A trade that fails while the token it holds is still live is retried every RetryInterval, so a
// broker that is down for minutes costs nothing: the token lives an hour. Once the held token has
// lapsed and the trade still fails, Run stops and returns that failure as it stands — the caller
// has no identity, and a caller with no identity must stop rather than write as nobody. hold's
// own error stops Run the same way, and no token is ever handed over that the trade did not
// answer.
func (c *Client) Run(ctx context.Context, hold func(*Token) error) error {
	var held *Token
	for {
		token, err := c.Trade(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if held == nil || !c.now().Before(held.Expiry) {
				return fmt.Errorf("the token client cannot renew its access token and holds none that is still live (%s): %w", c.Subject.Name(), err)
			}
			c.logger().Warn("the token exchange failed; the token in hand is still live, so it will be tried again",
				"broker", redact.URL(c.BrokerURL), "subject", c.Subject.Name(), "expires_at", held.Expiry.UTC().Format(time.RFC3339),
				"retry_in", c.retryInterval().String(), "error", err.Error())
			select {
			case <-ctx.Done():
				return nil
			case <-c.after(c.retryInterval()):
			}
			continue
		}
		held = token
		if err := hold(token); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.after(c.renewIn(token)):
		}
	}
}

// renewIn is how long the loop waits before trading again: half of what is left of the token's
// life, and the retry interval for a token the broker gave no expiry for or one already spent,
// so a loop never spins.
func (c *Client) renewIn(token *Token) time.Duration {
	left := token.Expiry.Sub(c.now())
	if left <= 0 {
		return c.retryInterval()
	}
	wait := left / DefaultRenewFraction
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}
