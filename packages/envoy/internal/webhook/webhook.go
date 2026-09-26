package webhook

import (
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher abstracts NATS envelope publishing for handler testability.
// Matches the envelopePublisher pattern from cmd/ghostwispr/main.go.
type Publisher interface {
	Publish(contracts.Envelope) error
}
