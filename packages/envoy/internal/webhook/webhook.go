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
	RecordFunc      func(owner, repo, number, sha, checkName, suiteID, checkRunID, url, status, conclusion, observedAt string) error
	RecordSuiteFunc func(owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string) error
	RecordHeadFunc  func(owner, repo, number, sha, updatedAt string) error
}

func (f CIRecorderFuncs) Record(owner, repo, number, sha, checkName, suiteID, checkRunID, url, status, conclusion, observedAt string) error {
	return f.RecordFunc(owner, repo, number, sha, checkName, suiteID, checkRunID, url, status, conclusion, observedAt)
}

func (f CIRecorderFuncs) RecordSuite(owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string) error {
	return f.RecordSuiteFunc(owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt)
}

func (f CIRecorderFuncs) RecordHead(owner, repo, number, sha, updatedAt string) error {
	return f.RecordHeadFunc(owner, repo, number, sha, updatedAt)
}
