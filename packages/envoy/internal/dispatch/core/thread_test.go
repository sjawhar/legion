package core

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// TestRequestIDQueryMatchesMarker locks the idempotency invariant: the token
// SearchByRequestID looks for in:body must actually appear in the body that
// BuildMetaMarker writes. If these drift, dedupe silently breaks and retries
// create duplicate threads — this is the regression guard for the original
// requestId (marker) vs request_id (search) mismatch.
func TestRequestIDQueryMatchesMarker(t *testing.T) {
	id := ComputeRequestID("641", "Subject", "Body", UrgencyMed, nil)
	marker := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: id})
	query := githubapi.BuildRequestIDQuery("sjawhar", "legion", id, dispatchLabel)

	if !strings.Contains(query, "\""+id+"\"") {
		t.Fatalf("query %q does not search for quoted request id %q", query, id)
	}
	if !strings.Contains(marker, id) {
		t.Fatalf("marker %q does not contain request id %q the search looks for", marker, id)
	}
	if !strings.Contains(query, "label:"+dispatchLabel) {
		t.Errorf("query %q not scoped to dispatch label %q", query, dispatchLabel)
	}
}

// TestComputeRequestIDIncludesAsk ensures two dispatches that differ only in
// their structured ask produce different request ids, so they do not collapse
// onto a single thread.
func TestComputeRequestIDIncludesAsk(t *testing.T) {
	ask := []QuestionInfo{{Question: "Color?", Options: []QuestionOption{{Label: "red"}}}}
	base := ComputeRequestID("641", "S", "B", UrgencyMed, nil)
	withAsk := ComputeRequestID("641", "S", "B", UrgencyMed, ask)
	if base == withAsk {
		t.Fatalf("request id ignored ask: both %q", base)
	}
	again := ComputeRequestID("641", "S", "B", UrgencyMed, ask)
	if withAsk != again {
		t.Fatalf("request id not deterministic for same ask: %q vs %q", withAsk, again)
	}
}

// TestComputeRequestIDStableForSameInputs guards the retry short-circuit: the
// same logical dispatch must hash to the same id across invocations.
func TestComputeRequestIDStableForSameInputs(t *testing.T) {
	a := ComputeRequestID("641", "S", "B", UrgencyHigh, nil)
	b := ComputeRequestID("641", "S", "B", UrgencyHigh, nil)
	if a != b {
		t.Fatalf("request id not stable: %q vs %q", a, b)
	}
}
