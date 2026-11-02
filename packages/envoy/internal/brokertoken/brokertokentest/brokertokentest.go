// Package brokertokentest is the two stands-in a test of the broker-token check needs: the
// secrets broker's introspection route, answering what the test says each token is, and a caller
// holding one key, whose proofs come from the producer a real caller uses (internal/broker/helper's
// DPoP transport) rather than from a second signer written beside the test. It is an ordinary
// package rather than a _test.go file so both suites that drive the check — internal/brokertoken's
// own and internal/dispatch/api's — read one copy; Go cannot share test-only code across packages.
package brokertokentest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokertoken"
)

// StubBroker is the secrets broker's introspection route: it answers what the test has said each
// token is, counts the introspections so a test can prove how many reached it, and keeps every
// Authorization it was presented so a test can prove Dispatch authenticated as itself.
type StubBroker struct {
	// Server is the stub's own server. Its URL is the broker URL a verifier is built on, and a
	// test closes it to prove what a broker that answers nothing is answered with.
	Server *httptest.Server

	mu      sync.Mutex
	answers map[string]brokertoken.Introspection
	bearers []string

	introspections atomic.Int64
}

// NewStubBroker serves the stub until the test ends.
func NewStubBroker(t testing.TB) *StubBroker {
	t.Helper()
	broker := &StubBroker{answers: map[string]brokertoken.Introspection{}}
	broker.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != brokertoken.IntrospectPath {
			http.Error(w, "not the introspection route", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "unreadable form", http.StatusBadRequest)
			return
		}
		broker.introspections.Add(1)
		broker.mu.Lock()
		broker.bearers = append(broker.bearers, r.Header.Get("Authorization"))
		answer := broker.answers[r.PostForm.Get("token")]
		broker.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(broker.Server.Close)
	return broker
}

// Says makes the broker answer answer for token. A token nothing says anything about is answered
// not live, as a token the broker never issued is.
func (b *StubBroker) Says(token string, answer brokertoken.Introspection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers[token] = answer
}

// Count is how many introspections have reached the broker.
func (b *StubBroker) Count() int64 { return b.introspections.Load() }

// Bearers is every Authorization the broker was presented, in the order it was, as a copy: what
// a test reads to prove the caller authenticated as itself.
func (b *StubBroker) Bearers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.bearers)
}

// capturedRequest keeps the request the helper's DPoP transport signed, instead of sending it.
type capturedRequest struct {
	mu   sync.Mutex
	last *http.Request
}

func (c *capturedRequest) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

// Caller is one caller holding one key, producing the proofs its real client produces.
type Caller struct {
	client   *http.Client
	captured *capturedRequest
	jkt      string
}

// NewCaller mints a caller's key and the transport that proves it.
func NewCaller(t testing.TB) *Caller {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("mint the caller's key: %v", err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint the caller's key: %v", err)
	}
	captured := &capturedRequest{}
	return &Caller{client: &http.Client{Transport: helper.NewDPoPTransport(key, captured)}, captured: captured, jkt: jkt}
}

// JKT is the thumbprint of the caller's key: what the broker binds a token to, and what a proof
// of this key is checked against.
func (c *Caller) JKT() string { return c.jkt }

// Signed is the request this caller's client signed for method and url, presenting token under
// the DPoP scheme: a test sends it, or reads its Authorization and DPoP headers onto a request of
// its own. Each call signs a fresh proof, as a real client does, and nothing leaves the process —
// the transport keeps the request rather than making it.
func (c *Caller) Signed(t testing.TB, method, url, token string) *http.Request {
	t.Helper()
	signing, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build the request to sign: %v", err)
	}
	signing.Header.Set("Authorization", "DPoP "+token)
	response, err := c.client.Do(signing)
	if err != nil {
		t.Fatalf("sign the request: %v", err)
	}
	response.Body.Close()
	c.captured.mu.Lock()
	defer c.captured.mu.Unlock()
	return c.captured.last
}
