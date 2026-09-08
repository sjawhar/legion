package webhook

import (
	"testing"

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

func TestCIRecorderFuncs(t *testing.T) {
	check := contracts.CIObservation{
		Owner:      "example-org",
		Repo:       "example-repo",
		Number:     "42",
		SHA:        "abcdef1234567",
		CheckName:  "unit-tests",
		SuiteID:    "900",
		CheckRunID: 987654321,
		URL:        "https://example-host/checks/987654321",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:00:00Z",
	}
	suiteObservation := contracts.CIObservation{
		Owner:      "example-org",
		Repo:       "example-repo",
		Number:     "42",
		SHA:        "abcdef1234567",
		SuiteID:    "900",
		AppID:      "77",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:00:00Z",
	}
	var record, suite contracts.CIObservation
	var head headCall
	recorder := CIRecorderFuncs{
		RecordFunc: func(observation contracts.CIObservation) error {
			record = observation
			return nil
		},
		RecordSuiteFunc: func(observation contracts.CIObservation) error {
			suite = observation
			return nil
		},
		RecordHeadFunc: func(owner, repo, number, sha, updatedAt string) error {
			head = headCall{owner, repo, number, sha, updatedAt}
			return nil
		},
	}
	if err := recorder.Record(check); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recorder.RecordSuite(suiteObservation); err != nil {
		t.Fatalf("record suite: %v", err)
	}
	if err := recorder.RecordHead("example-org", "example-repo", "42", "abcdef1234567", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	if record != check {
		t.Fatalf("record adapter observation = %+v, want %+v", record, check)
	}
	if suite != suiteObservation {
		t.Fatalf("suite adapter observation = %+v, want %+v", suite, suiteObservation)
	}
	if head != (headCall{owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567", updatedAt: "2026-09-07T03:00:00Z"}) {
		t.Fatalf("head adapter call = %+v", head)
	}
}
