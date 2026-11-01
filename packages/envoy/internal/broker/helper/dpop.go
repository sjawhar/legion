// packages/envoy/internal/broker/helper/dpop.go

package helper

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
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
// retried once with that nonce. The helper carries no proof on the device authorization request.
//
// A request that presents an access token under the DPoP scheme (Authorization: DPoP <token>) is a
// request to a resource server, and its proof also carries ath, the base64url-encoded SHA-256 of
// that token: RFC 9449 Section 4.3 step 12 requires a resource server to reject a proof whose ath
// does not hash the token it was presented with, so a proof is good only for the one token it was
// signed beside. A request that presents no such token is a token endpoint request, and its proof
// carries no ath — there is no access token for it to name, the token endpoint being where one is
// asked for — so those proofs are what they have always been.
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
	compact, err := d.proof(r.Method, htu.String(), nonce, accessTokenHash(r.Header))
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

// proof is one DPoP proof for method on htu, carrying nonce and ath when they are not empty.
func (d *dpopTransport) proof(method, htu, nonce, ath string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: d.key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		return "", err
	}
	claims := map[string]any{"jti": uuid.NewString(), "htm": strings.ToUpper(method), "htu": htu, "iat": time.Now().Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if ath != "" {
		claims["ath"] = ath
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

// accessTokenHash is the ath claim for a request presenting an access token under the DPoP scheme,
// the base64url-encoded SHA-256 of that token (RFC 9449 Section 4.3 step 12), and is empty for a
// request presenting none.
func accessTokenHash(h http.Header) string {
	scheme, token, found := strings.Cut(h.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "DPoP") || token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
