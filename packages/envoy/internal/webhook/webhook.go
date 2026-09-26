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

// publishFailed answers a delivery whose envelope the bus did not publish. One NATS cannot take
// whole (bus.ErrTooLarge) is refused the same way on every redelivery, so it is a 422, which a
// redelivery sweep takes as terminal, logged `<source> publish refused`. Any other failure may pass
// on a redelivery, so it is a 503, logged `<source> publish failed`, the line an alert pages on.
func publishFailed(w http.ResponseWriter, source string, err error) {
	if errors.Is(err, bus.ErrTooLarge) {
		log.Printf("%s publish refused: %v", source, err)
		http.Error(w, "envelope too large to publish", http.StatusUnprocessableEntity)
		return
	}
	log.Printf("%s publish failed: %v", source, err)
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}
