package webhook

import (
	"errors"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher abstracts NATS envelope publishing for handler testability.
// Matches the envelopePublisher pattern from cmd/ghostwispr/main.go.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// deliveryFailed answers a delivery whose envelope, or CI observation, the listener did not publish,
// and logs `<action> refused` or `<action> failed`. A refusal (bus.ErrRefused: too large to publish
// whole, or a subject NATS does not accept) is refused the same way on every redelivery, so it is a
// 422, which a redelivery sweep takes as terminal, and its error says why. Any other failure may
// pass on a redelivery, so it is a 503; `github publish failed` is the line an alert pages on.
func deliveryFailed(w http.ResponseWriter, action string, err error) {
	if errors.Is(err, bus.ErrRefused) {
		log.Printf("%s refused: %v", action, err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("%s failed: %v", action, err)
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}
