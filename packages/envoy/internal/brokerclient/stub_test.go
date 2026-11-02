// packages/envoy/internal/brokerclient/stub_test.go

package brokerclient

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// stubBroker is a broker's token endpoint as these tests drive one: it records the form and the
// DPoP proof of every exchange it is asked for and answers what the test told it to, in turn.
// Nothing here verifies a proof — that is the broker's own suite — but the proof is parsed, so a
// test can hold the client to the claim set the wire contract records.
type stubBroker struct {
	server *httptest.Server

	mu       sync.Mutex
	forms    []url.Values
	proofs   []stubProof
	answers  []stubAnswer
	answered int
}

// stubAnswer is one answer the stub gives, in the order the test set them.
type stubAnswer struct {
	status int
	body   string
}

// stubProof is one DPoP proof as the stub read it: its header, its claims, and the JWK it
// embedded, each as a plain map so a test can compare key sets with the corpus.
type stubProof struct {
	header map[string]any
	claims map[string]any
	jwk    map[string]any
}

func newStubBroker(t *testing.T, answers ...stubAnswer) *stubBroker {
	t.Helper()
	stub := &stubBroker{answers: answers}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *stubBroker) URL() string { return s.server.URL }

func (s *stubBroker) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != TokenPath {
		http.Error(w, "the stub broker serves only "+TokenPath, http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proof, err := readProof(r.Header.Get("DPoP"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.forms = append(s.forms, r.PostForm)
	s.proofs = append(s.proofs, proof)
	answer := stubAnswer{status: http.StatusOK, body: `{"access_token":"stub-access-token","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`}
	if s.answered < len(s.answers) {
		answer = s.answers[s.answered]
	}
	s.answered++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	_, _ = w.Write([]byte(answer.body))
}

// calls is how many exchanges the stub has been asked for.
func (s *stubBroker) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forms)
}

// call is the nth exchange (0-based) as the stub read it.
func (s *stubBroker) call(t *testing.T, n int) (url.Values, stubProof) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.forms) {
		t.Fatalf("the stub broker was asked for %d exchanges; call %d was never made", len(s.forms), n)
	}
	return s.forms[n], s.proofs[n]
}

// readProof parses a compact DPoP proof into its header, claims and embedded JWK, without
// checking its signature: what these tests hold the client to is what it put in the proof.
func readProof(compact string) (stubProof, error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return stubProof{}, errNoProof
	}
	var proof stubProof
	for i, into := range []*map[string]any{&proof.header, &proof.claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			return stubProof{}, err
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return stubProof{}, err
		}
	}
	proof.jwk, _ = proof.header["jwk"].(map[string]any)
	return proof, nil
}

// errNoProof is what the stub answers a request that carried no DPoP header, which is itself a
// failure of the contract.
var errNoProof = &stubError{"the request carried no DPoP proof"}

type stubError struct{ message string }

func (e *stubError) Error() string { return e.message }
