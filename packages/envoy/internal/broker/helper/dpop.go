// packages/envoy/internal/broker/helper/dpop.go

package helper

import (
	"crypto/ecdsa"
	"net/http"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

// NewDPoPTransport is internal/brokerclient's RFC 9449 signer under the name the helper has
// always called it by. The signer itself lives in that package because the token client is the
// other caller of it, and one repository holds one DPoP signer: a second copy would be a second
// set of claims to keep in step with the broker's checks. Its behaviour, the nonce retry and the
// ath claim included, is documented there.
func NewDPoPTransport(key *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper {
	return brokerclient.NewDPoPTransport(key, base)
}
