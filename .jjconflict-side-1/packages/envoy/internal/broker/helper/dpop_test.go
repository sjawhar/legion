// packages/envoy/internal/broker/helper/dpop_test.go

package helper

import (
	"crypto"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// dpopClaims is the payload of a DPoP proof the test server read.
type dpopClaims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	Nonce string `json:"nonce"`
}

// TestTheDPoPTransportSignsEachTokenRequest: every request the transport carries has an RFC 9449
// proof that verifies against the machine key embedded in it, names the request's method and its
// URL without the query, and has a jti of its own. A 400 use_dpop_nonce with a DPoP-Nonce header
// is retried once, with the nonce, a fresh jti and the same body.
func TestTheDPoPTransportSignsEachTokenRequest(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, _ := proof.Thumbprint(&key.PublicKey)
	var mu sync.Mutex
	var seen []dpopClaims
	var bodies []string
	var withoutProof int
	demandNonce := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		header := r.Header.Get("DPoP")
		if header == "" {
			withoutProof++
			w.WriteHeader(http.StatusOK)
			return
		}
		sig, err := jose.ParseSigned(header, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Errorf("proof does not parse: %v", err)
			return
		}
		h := sig.Signatures[0].Protected
		if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != "dpop+jwt" || h.JSONWebKey == nil {
			t.Errorf("proof header typ %q jwk %v; want dpop+jwt with the key embedded", typ, h.JSONWebKey)
			return
		}
		if tp, err := h.JSONWebKey.Thumbprint(crypto.SHA256); err != nil || base64.RawURLEncoding.EncodeToString(tp) != thumbprint {
			t.Errorf("proof's embedded key is not the machine key (%v)", err)
			return
		}
		payload, err := sig.Verify(h.JSONWebKey)
		if err != nil {
			t.Errorf("proof does not verify against its embedded key: %v", err)
			return
		}
		var c dpopClaims
		_ = json.Unmarshal(payload, &c)
		seen = append(seen, c)
		bodies = append(bodies, r.PostForm.Encode())
		if demandNonce && c.Nonce == "" {
			w.Header().Set("DPoP-Nonce", "nonce-1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := &http.Client{Transport: NewDPoPTransport(key, srv.Client().Transport)}
	tokenURL := srv.URL + "/v1/oauth/token"
	post := func(u string) *http.Response {
		t.Helper()
		resp, err := client.Post(u, "application/x-www-form-urlencoded", strings.NewReader(url.Values{"grant_type": {"refresh_token"}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	post(tokenURL + "?ignored=1")
	post(tokenURL)
	mu.Lock()
	if len(seen) != 2 || withoutProof != 0 {
		t.Fatalf("proofs on %d requests, none on %d; want one on each of the 2 requests", len(seen), withoutProof)
	}
	for _, c := range seen {
		if c.HTM != http.MethodPost || c.HTU != tokenURL || c.IAT == 0 || c.JTI == "" {
			t.Fatalf("proof claims %+v; want htm POST, htu %s, an iat and a jti", c, tokenURL)
		}
	}
	if seen[0].JTI == seen[1].JTI {
		t.Fatalf("two requests carried the same jti %q", seen[0].JTI)
	}
	seen, bodies, demandNonce = nil, nil, true
	mu.Unlock()

	if resp := post(tokenURL); resp.StatusCode != http.StatusOK {
		t.Fatalf("a request the server answers use_dpop_nonce ended %d; want the retry's 200", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0].Nonce != "" || seen[1].Nonce != "nonce-1" || seen[0].JTI == seen[1].JTI {
		t.Fatalf("proofs %+v; want one without a nonce, then one retry with nonce-1 and a fresh jti", seen)
	}
	if bodies[0] != bodies[1] || bodies[1] != "grant_type=refresh_token" {
		t.Fatalf("retry body %q after %q; want the same form", bodies[1], bodies[0])
	}
}
