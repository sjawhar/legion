package brokertoken

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/googleidentity"
)

// GoogleAccountCacheTTL is how long a GoogleAccount reuses the token it last read. The metadata
// server mints an identity token that lives an hour and the broker refuses a caller's token that
// lives longer than an hour from its issue (machine.MaxTokenLoginLifetime), so a token this fresh
// is never near expiry; the window is AccountTokenCacheTTL's for the reason that one is what it
// is, that a credential read once per call would be read once per request.
//
// It is a window rather than an expiry read from the token because nothing here parses a claim:
// internal/googleidentity reads the bytes and leaves every claim to the broker, and a reader that
// started decoding the token to find its 'exp' would be deciding how long to trust a credential
// from the credential itself.
const GoogleAccountCacheTTL = 30 * time.Second

// GoogleAccount is the Account of a caller holding no Kubernetes token at all: a listener on a
// Compute Engine VM, whose own platform identity is the identity token its metadata server mints
// for the instance's service account (internal/googleidentity). The broker resolves the calling
// service from the token's 'email' claim (BROKER_API_ACCOUNTS' Google half), so what a deployment
// names here is the audience alone.
//
// A machine's identity token rotates as a projected one does, so the metadata server is asked at
// call time and never once at boot.
type GoogleAccount struct {
	// Audience is the 'aud' the token is minted for: the broker's own audience, as the broker's
	// verifier of its callers' Google tokens requires it (BROKER_GOOGLE_OIDC_AUDIENCE).
	Audience string
	// TTL is how long Read reuses what it read, GoogleAccountCacheTTL for every reader
	// NewGoogleAccount builds. A test that proves a second read reaches the metadata server
	// lapses the cache by zeroing it.
	TTL time.Duration

	mu     sync.Mutex
	cached string
	readAt time.Time
}

// NewGoogleAccount is a reader of this machine's Google identity token for audience, which Read
// asks the metadata server for at call time, caching what it reads for GoogleAccountCacheTTL.
func NewGoogleAccount(audience string) *GoogleAccount {
	return &GoogleAccount{Audience: audience, TTL: GoogleAccountCacheTTL}
}

// Read is the bearer a call presents: this machine's identity token for Audience, asked for again
// once the copy it cached is older than TTL. A metadata server that cannot be reached, or that
// answers no token, is an error naming the audience and no request at all, never an empty bearer
// for the broker to refuse: a machine identity this program cannot read is this deployment's own
// misconfiguration, as an unreadable projected token is.
//
// The read runs on a context of its own, bounded by internal/googleidentity's own timeout, rather
// than on the request's: one request going away must not leave the next one without the machine's
// identity, and the answer is cached for every caller.
func (g *GoogleAccount) Read() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached != "" && time.Since(g.readAt) < g.TTL {
		return g.cached, nil
	}
	token, err := googleidentity.Token(context.Background(), g.Audience)
	if err != nil {
		return "", fmt.Errorf("brokertoken: read this machine's Google identity token for %s: %w", g.Audience, err)
	}
	g.cached, g.readAt = token, time.Now()
	return token, nil
}
