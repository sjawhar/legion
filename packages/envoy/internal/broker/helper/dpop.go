// packages/envoy/internal/broker/helper/dpop.go

package helper

import (
	"crypto/ecdsa"
	"net/http"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

// NewDPoPTransport is brokerclient.NewDPoPTransport, the repository's one RFC 9449 signer, which
// moved to internal/brokerclient so the Go token client and the helper sign one way rather than
// two. It stays here because every caller of the helper's broker — the helper itself, the broker's
// own tests and the test rigs — reaches the signer through this package, and because a signer
// named twice is a signer that can drift.
func NewDPoPTransport(key *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper {
	return brokerclient.NewDPoPTransport(key, base)
}
