// packages/envoy/internal/broker/helper/dpop_resource_test.go

package helper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// resourceConfig is the rfc9449.StrategyConfig a resource server checks a proof with: the
// algorithm the transport signs with, and a window a proof signed moments ago is inside.
type resourceConfig struct{}

func (resourceConfig) GetDPoPAllowedJWSAlgorithms(context.Context) []string {
	return []string{string(jose.ES256)}
}
func (resourceConfig) GetDPoPClockSkew(context.Context) time.Duration     { return time.Second }
func (resourceConfig) GetDPoPProofLifespan(context.Context) time.Duration { return time.Minute }
func (resourceConfig) GetDPoPNonceLifespan(context.Context) time.Duration { return time.Minute }

// resourceStore is that check's rfc9449.Storage in memory: each accepted proof recorded as used
// until it expires, keyed on jti and htu as DPoPReplayStorage requires, and the nonces minted.
type resourceStore struct {
	mu     sync.Mutex
	used   map[string]time.Time
	nonces map[string]time.Time
}

func newResourceStore() *resourceStore {
	return &resourceStore{used: map[string]time.Time{}, nonces: map[string]time.Time{}}
}

func (s *resourceStore) CheckAndSetDPoPProofUsed(_ context.Context, jti, _, _, _, htu string, exp time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jti + "\x00" + htu
	if until, ok := s.used[key]; ok && until.After(time.Now()) {
		return true, nil
	}
	s.used[key] = exp
	return false, nil
}

func (s *resourceStore) CreateDPoPNonce(_ context.Context, nonce string, exp time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nonces[nonce] = exp
	return nil
}

func (s *resourceStore) IsDPoPNonceValid(_ context.Context, nonce string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.nonces[nonce]
	return ok && exp.After(time.Now()), nil
}

// capturedRequests keeps the request the transport signed, so the proof on it is judged by the
// server-side validator itself rather than by a second reading of RFC 9449 written here. Its
// RoundTrip runs on the goroutine calling the client, so the field needs no lock.
type capturedRequests struct {
	last *http.Request
}

func (c *capturedRequests) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

// TestTheDPoPTransportSignsAResourceRequestWithTheAccessTokenHash: a request presenting an access
// token under the DPoP scheme is a resource request, and the proof the transport signs for it
// passes the library's own RFC 9449 Section 4.3 resource check, 'ath' included; one whose 'ath'
// hashes another token does not; and a request presenting no token is still a token endpoint
// request, whose proof carries no 'ath' and verifies as it did before.
func TestTheDPoPTransportSignsAResourceRequestWithTheAccessTokenHash(t *testing.T) {
	const (
		resourceURL = "http://dispatch.example/api/v1/sessions?limit=1"
		resourceHTU = "http://dispatch.example/api/v1/sessions"
		tokenURL    = "http://broker.example/v1/oauth/token"
		accessToken = "the-access-token-the-broker-minted"
		otherToken  = "a-token-this-request-does-not-present"
	)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	captured := &capturedRequests{}
	client := &http.Client{Transport: NewDPoPTransport(key, captured)}
	strategy := rfc9449.NewDefaultStrategy(resourceConfig{}, newResourceStore())
	ctx := t.Context()

	// send carries one request through the transport and returns the request it signed.
	send := func(method, u, authorization string) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return captured.last
	}

	t.Run("the resource server accepts a proof signed for the token it is presented with", func(t *testing.T) {
		signed := send(http.MethodGet, resourceURL, "DPoP "+accessToken)
		parsed, err := strategy.ValidateResourceAccess(ctx, signed, accessToken, jkt, false)
		if err != nil {
			t.Fatalf("the resource check refused the transport's proof: %v", err)
		}
		if parsed.Thumbprint != jkt {
			t.Fatalf("the accepted proof names key %q; want the transport's own %q", parsed.Thumbprint, jkt)
		}
		if parsed.AccessTokenHash == "" || parsed.URL != resourceHTU {
			t.Fatalf("accepted proof ath %q htu %q; want an ath and %q", parsed.AccessTokenHash, parsed.URL, resourceHTU)
		}
	})

	t.Run("the resource server refuses a proof whose ath names another token", func(t *testing.T) {
		// Signed against otherToken, then presented with the token the resource server knows, so
		// nothing but the 'ath' claim differs from the request accepted above.
		signed := send(http.MethodGet, resourceURL, "DPoP "+otherToken)
		signed.Header.Set("Authorization", "DPoP "+accessToken)
		_, err := strategy.ValidateResourceAccess(ctx, signed, accessToken, jkt, false)
		if !errors.Is(err, oauth2.ErrInvalidDPoPProof) {
			t.Fatalf("the resource check answered %v; want invalid_dpop_proof", err)
		}
		if hint := oauth2.ErrorToRFC6749Error(err).HintField; !strings.Contains(hint, "'ath'") {
			t.Fatalf("the refusal reads %q; want one naming the 'ath' claim", hint)
		}
	})

	t.Run("a token endpoint request's proof carries no ath", func(t *testing.T) {
		signed := send(http.MethodPost, tokenURL, "")
		parsed, err := strategy.ValidateDPoPProof(ctx, signed.Method, tokenURL, signed.Header.Get("DPoP"), false)
		if err != nil {
			t.Fatalf("the token endpoint refused the transport's proof: %v", err)
		}
		if parsed.AccessTokenHash != "" {
			t.Fatalf("a token endpoint proof carries ath %q; want none, no access token being presented", parsed.AccessTokenHash)
		}
		if parsed.Thumbprint != jkt || parsed.Method != http.MethodPost || parsed.URL != tokenURL {
			t.Fatalf("token endpoint proof key %q htm %q htu %q; want %q, POST and %q", parsed.Thumbprint, parsed.Method, parsed.URL, jkt, tokenURL)
		}
	})
}
