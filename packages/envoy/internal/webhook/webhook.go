package webhook

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher publishes a webhook's envelopes, each within the context its handler gives it, which
// carries the request's own end and the route's budget for its caller (*bus.Client.PublishWithin,
// which also keeps the bus's own five-second bound).
type Publisher interface {
	PublishWithin(context.Context, contracts.Envelope) error
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
