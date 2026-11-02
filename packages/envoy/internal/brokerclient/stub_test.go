// packages/envoy/internal/brokerclient/stub_test.go
//
// The stub broker every test in this package drives its client against: it records each trade
// whole — the form, the DPoP proof's header and its claims — and answers whatever the test told
// it to, so a test asserts what went on the wire rather than what the client meant to send.

package brokerclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// stubRequest is one trade as the stub broker received it.
type stubRequest struct {
	Method      string
	Path        string
	ContentType string
	Form        url.Values
	// ProofHeader and ProofClaims are the DPoP proof's two JSON objects, decoded from the
	// compact serialisation without verifying the signature: what the proof carries is what
	// this package's tests judge, and the broker's own tests judge whether it verifies.
	ProofHeader, ProofClaims map[string]any
	// Proof is the compact proof itself, for a test that compares two requests' proofs.
	Proof string
}

// stubBroker is a broker's token endpoint and nothing else.
type stubBroker struct {
	server *httptest.Server
	// answer gives the status and body for the nth trade, counting from zero.
	answer func(n int, form url.Values) (int, map[string]any)

	mu       sync.Mutex
	requests []stubRequest
}

// newStubBroker serves POST /v1/oauth/token, answering what answer gives for each trade in turn.
func newStubBroker(t *testing.T, answer func(n int, form url.Values) (int, map[string]any)) *stubBroker {
	t.Helper()
	broker := &stubBroker{answer: answer}
	mux := http.NewServeMux()
	mux.HandleFunc(TokenPath, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "unparsable form", http.StatusBadRequest)
			return
		}
		header, claims, err := decodeProof(r.Header.Get("DPoP"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		broker.mu.Lock()
		n := len(broker.requests)
		broker.requests = append(broker.requests, stubRequest{
			Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"),
			Form: r.PostForm, ProofHeader: header, ProofClaims: claims, Proof: r.Header.Get("DPoP"),
		})
		broker.mu.Unlock()
		status, body := broker.answer(n, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	broker.server = httptest.NewServer(mux)
	t.Cleanup(broker.server.Close)
	return broker
}

// URL is the broker's base URL, what a Config's BrokerURL takes.
func (b *stubBroker) URL() string { return b.server.URL }

// Requests is every trade the stub has received, in order.
func (b *stubBroker) Requests() []stubRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]stubRequest(nil), b.requests...)
}

// decodeProof reads a DPoP proof's header and claims out of its compact serialisation.
func decodeProof(compact string) (header, claims map[string]any, err error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, nil, errNoProof
	}
	if header, err = decodeSegment(parts[0]); err != nil {
		return nil, nil, err
	}
	if claims, err = decodeSegment(parts[1]); err != nil {
		return nil, nil, err
	}
	return header, claims, nil
}

func decodeSegment(segment string) (map[string]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

// errNoProof is the stub's refusal of a request carrying no DPoP proof at all, which is what a
// client that forgot the proof looks like from the broker's side.
var errNoProof = &proofError{"the request carries no DPoP proof"}

type proofError struct{ message string }

func (e *proofError) Error() string { return e.message }

// testKey is one ECDSA P-256 key, what every client in these tests proves.
func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return key
}

// scriptedSource is a Source answering one token of one type: the stand-in for an identity whose
// own reader has a test of its own.
type scriptedSource struct {
	token, tokenType, describe string
}

func (s *scriptedSource) Token(context.Context) (string, string, error) {
	return s.token, s.tokenType, nil
}

func (s *scriptedSource) Describe() string { return s.describe }

// newRawServer is one httptest server on handler, answering its base URL; the stubs that are not
// the broker's token endpoint are built on it.
func newRawServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}
