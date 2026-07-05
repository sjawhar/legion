package webhook

import "github.com/sjawhar/envoy/internal/contracts"

// mockPublisher records published envelopes for test assertions.
type mockPublisher struct {
	published []contracts.Envelope
	err       error
}

func (m *mockPublisher) Publish(item contracts.Envelope) error {
	m.published = append(m.published, item)
	return m.err
}

// ciCall records one CIRecorder.Record invocation.
type ciCall struct {
	owner, repo, number, sha, checkName, status, conclusion string
}

// mockRecorder records CIRecorder calls for test assertions.
type mockRecorder struct {
	calls []ciCall
	err   error
}

func (m *mockRecorder) Record(owner, repo, number, sha, checkName, status, conclusion string) error {
	m.calls = append(m.calls, ciCall{owner, repo, number, sha, checkName, status, conclusion})
	return m.err
}
