// packages/envoy/internal/broker/helper/dpop.go

package helper

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

// dpopTransport signs an RFC 9449 DPoP proof of its key for every request it carries.
type dpopTransport struct {
	key  *ecdsa.PrivateKey
	base http.RoundTripper

	mu    sync.Mutex
	nonce string // the most recent DPoP-Nonce the server sent, carried in every later proof
}

// NewDPoPTransport is a RoundTripper that sends each request through base with a DPoP header: a
// proof of key (typ dpop+jwt, ES256, the public key embedded) naming a fresh jti, the request's
// method (htm), its URL without query or fragment (htu) and the time (iat), plus the server's most
// recent nonce once it has sent one. A 400 use_dpop_nonce answer that carries a DPoP-Nonce is
// retried once with that nonce. The helper carries only its token endpoint requests through it:
// the device authorization request needs no proof.
func NewDPoPTransport(key *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper {
	return &dpopTransport{key: key, base: base}
}

func (d *dpopTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.send(r)
	if err != nil || resp.StatusCode != http.StatusBadRequest || resp.Header.Get("DPoP-Nonce") == "" {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Error != "use_dpop_nonce" || (r.Body != nil && r.GetBody == nil) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	retry := r.Clone(r.Context())
	if r.Body != nil {
		if retry.Body, err = r.GetBody(); err != nil {
			return nil, err
		}
	}
	return d.send(retry)
}

// send signs r and sends it, remembering any nonce the answer carries.
func (d *dpopTransport) send(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	nonce := d.nonce
	d.mu.Unlock()
	htu := *r.URL
	htu.RawQuery, htu.Fragment = "", ""
	compact, err := d.proof(r.Method, htu.String(), nonce)
	if err != nil {
		return nil, err
	}
	signed := r.Clone(r.Context())
	signed.Header.Set("DPoP", compact)
	resp, err := d.base.RoundTrip(signed)
	if err != nil {
		return nil, err
	}
	if next := resp.Header.Get("DPoP-Nonce"); next != "" {
		d.mu.Lock()
		d.nonce = next
		d.mu.Unlock()
	}
	return resp, nil
}

// proof is one DPoP proof for method on htu, carrying nonce when it is not empty.
func (d *dpopTransport) proof(method, htu, nonce string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: d.key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		return "", err
	}
	claims := map[string]any{"jti": uuid.NewString(), "htm": strings.ToUpper(method), "htu": htu, "iat": time.Now().Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}
