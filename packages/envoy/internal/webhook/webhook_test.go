package webhook

import (
	"github.com/sjawhar/envoy/internal/contracts"
)

// mockPublisher records published envelopes for test assertions.
type mockPublisher struct {
	published []contracts.Envelope
	err       error
}

func (m *mockPublisher) Publish(item contracts.Envelope) error {
	m.published = append(m.published, item)
	return m.err
}

// headCall records one CIRecorder.RecordHead invocation.
type headCall struct {
	owner, repo, number, sha, updatedAt string
}

// mockRecorder records CIRecorder calls for test assertions.
type mockRecorder struct {
	calls      []contracts.CIObservation
	suiteCalls []contracts.CIObservation
	headCalls  []headCall
	err        error
	suiteErr   error
	headErr    error
}

func (m *mockRecorder) Record(observation contracts.CIObservation) error {
	m.calls = append(m.calls, observation)
	return m.err
}

func (m *mockRecorder) RecordSuite(observation contracts.CIObservation) error {
	m.suiteCalls = append(m.suiteCalls, observation)
	return m.suiteErr
}

func (m *mockRecorder) RecordHead(owner, repo, number, sha, updatedAt string) error {
	m.headCalls = append(m.headCalls, headCall{owner, repo, number, sha, updatedAt})
	return m.headErr
}
