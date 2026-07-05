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

// CIRecorderFunc adapts a plain function to the CIRecorder interface.
type CIRecorderFunc func(owner, repo, number, sha, checkName, status, conclusion string) error

func (f CIRecorderFunc) Record(owner, repo, number, sha, checkName, status, conclusion string) error {
	return f(owner, repo, number, sha, checkName, status, conclusion)
}
