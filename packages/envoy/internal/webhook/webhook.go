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

// CIRecorderFuncs adapts cistore functions required by CIRecorder.
type CIRecorderFuncs struct {
	RecordFunc      func(contracts.CIObservation) error
	RecordSuiteFunc func(contracts.CIObservation) error
	RecordHeadFunc  func(owner, repo, number, sha, updatedAt string) error
}

func (f CIRecorderFuncs) Record(observation contracts.CIObservation) error {
	return f.RecordFunc(observation)
}

func (f CIRecorderFuncs) RecordSuite(observation contracts.CIObservation) error {
	return f.RecordSuiteFunc(observation)
}

func (f CIRecorderFuncs) RecordHead(owner, repo, number, sha, updatedAt string) error {
	return f.RecordHeadFunc(owner, repo, number, sha, updatedAt)
}
