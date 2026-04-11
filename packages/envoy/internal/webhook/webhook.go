package webhook

import (
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher abstracts NATS envelope publishing for handler testability.
// Matches the envelopePublisher pattern from cmd/ghostwispr/main.go.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// PublisherFunc adapts a plain function to the Publisher interface.
type PublisherFunc func(contracts.Envelope) error

func (f PublisherFunc) Publish(item contracts.Envelope) error {
	return f(item)
}
