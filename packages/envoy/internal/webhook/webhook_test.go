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
	owner, repo, number, sha, checkName, checkRunID, url, status, conclusion string
}

// headCall records one CIRecorder.RecordHead invocation.
type headCall struct {
	owner, repo, number, sha string
}

// mockRecorder records CIRecorder calls for test assertions.
type mockRecorder struct {
	calls     []ciCall
	headCalls []headCall
	err       error
	headErr   error
}

func (m *mockRecorder) Record(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion string) error {
	m.calls = append(m.calls, ciCall{owner, repo, number, sha, checkName, checkRunID, url, status, conclusion})
	return m.err
}

func (m *mockRecorder) RecordHead(owner, repo, number, sha string) error {
	m.headCalls = append(m.headCalls, headCall{owner, repo, number, sha})
	return m.headErr
}

func TestCIRecorderFuncs(t *testing.T) {
	var record ciCall
	var head headCall
	recorder := CIRecorderFuncs{
		RecordFunc: func(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion string) error {
			record = ciCall{owner, repo, number, sha, checkName, checkRunID, url, status, conclusion}
			return nil
		},
		RecordHeadFunc: func(owner, repo, number, sha string) error {
			head = headCall{owner, repo, number, sha}
			return nil
		},
	}

	if err := recorder.Record("example-org", "example-repo", "42", "abcdef1234567", "unit-tests", "987654321", "https://example-host/checks/987654321", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recorder.RecordHead("example-org", "example-repo", "42", "abcdef1234567"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	if record.checkRunID != "987654321" || record.url != "https://example-host/checks/987654321" {
		t.Fatalf("record adapter lost check identity: %+v", record)
	}
	if head != (headCall{owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567"}) {
		t.Fatalf("head adapter call = %+v", head)
	}
}
