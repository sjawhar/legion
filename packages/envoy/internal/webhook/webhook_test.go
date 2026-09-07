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

// ciCall records one CIRecorder.Record invocation.
type ciCall struct {
	owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt string
}

// headCall records one CIRecorder.RecordHead invocation.
type headCall struct {
	owner, repo, number, sha, updatedAt string
}

type suiteCall struct {
	owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string
}

// mockRecorder records CIRecorder calls for test assertions.
type mockRecorder struct {
	calls      []ciCall
	suiteCalls []suiteCall
	headCalls  []headCall
	err        error
	suiteErr   error
	headErr    error
}

func (m *mockRecorder) Record(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt string) error {
	m.calls = append(m.calls, ciCall{owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt})
	return m.err
}

func (m *mockRecorder) RecordSuite(owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string) error {
	m.suiteCalls = append(m.suiteCalls, suiteCall{owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt})
	return m.suiteErr
}

func (m *mockRecorder) RecordHead(owner, repo, number, sha, updatedAt string) error {
	m.headCalls = append(m.headCalls, headCall{owner, repo, number, sha, updatedAt})
	return m.headErr
}

func TestCIRecorderFuncs(t *testing.T) {
	var record ciCall
	var suite suiteCall
	var head headCall
	recorder := CIRecorderFuncs{
		RecordFunc: func(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt string) error {
			record = ciCall{owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt}
			return nil
		},
		RecordSuiteFunc: func(owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string) error {
			suite = suiteCall{owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt}
			return nil
		},
		RecordHeadFunc: func(owner, repo, number, sha, updatedAt string) error {
			head = headCall{owner, repo, number, sha, updatedAt}
			return nil
		},
	}

	if err := recorder.Record("example-org", "example-repo", "42", "abcdef1234567", "unit-tests", "987654321", "https://example-host/checks/987654321", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recorder.RecordSuite("example-org", "example-repo", "42", "abcdef1234567", "900", "completed", "success", "77", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record suite: %v", err)
	}
	if err := recorder.RecordHead("example-org", "example-repo", "42", "abcdef1234567", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	if record.checkRunID != "987654321" || record.url != "https://example-host/checks/987654321" ||
		record.observedAt != "2026-09-07T03:00:00Z" {
		t.Fatalf("record adapter lost check identity: %+v", record)
	}
	if suite.suiteID != "900" || suite.appID != "77" || suite.observedAt != "2026-09-07T03:00:00Z" {
		t.Fatalf("suite adapter call = %+v", suite)
	}
	if head != (headCall{owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567", updatedAt: "2026-09-07T03:00:00Z"}) {
		t.Fatalf("head adapter call = %+v", head)
	}
}
