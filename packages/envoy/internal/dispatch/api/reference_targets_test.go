package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// referenceTargetsRequest reads what references point at, as alice, with etag, when set, as
// If-None-Match. Each reference rides as its own ?ref=, in the order given: url.Values keeps the
// order of one key's values.
func referenceTargetsRequest(t *testing.T, handler http.Handler, references []string, etag string) *httptest.ResponseRecorder {
	t.Helper()
	query := url.Values{}
	for _, reference := range references {
		query.Add("ref", reference)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/reference-targets?"+query.Encode(), nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// referenceTargets is the answer of a read that must succeed.
func referenceTargets(t *testing.T, handler http.Handler, references []string) ([]model.ReferenceTarget, string) {
	t.Helper()
	response := referenceTargetsRequest(t, handler, references, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read %d references: status=%d body=%s", len(references), response.Code, response.Body.String())
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("the read carries no ETag: %s", response.Body.String())
	}
	return decodeBody[model.ReferenceTargets](t, response).Targets, etag
}

// One request resolves a whole set: a block that exists, a block a later version deleted, and an
// issue of its own. Each member carries the kind its reference names, the words a reader is shown
// for its target, and whether that target is gone — the deleted block answered, not refused,
// since a chip whose target is gone still has to draw.
func TestReferenceTargetsResolveABatchInOneRequest(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "First paragraph.\n\nSecond paragraph.\n")
	other := createReferenceAPIIssue(t, handler, "CORE")
	ids := blockIDsByText(t, handler, issue.PrimaryArtifactID)
	specRef := "dispatch://" + issue.Key + "/spec"
	held := specRef + "#b-" + text.EncodeBlockID(ids["First paragraph."])
	gone := specRef + "#b-" + text.EncodeBlockID(ids["Second paragraph."])
	if ids["First paragraph."] == "" || ids["Second paragraph."] == "" {
		t.Fatalf("the spec's blocks are %v, want both paragraphs", ids)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "find": "Second paragraph."}})

	targets, _ := referenceTargets(t, handler, []string{held, gone, "dispatch://" + other.Key})
	if len(targets) != 3 {
		t.Fatalf("the read answered %#v, want one member per reference", targets)
	}
	if want := (model.ReferenceTarget{Ref: held, Kind: "block", Name: "First paragraph."}); targets[0] != want {
		t.Errorf("the block that exists answered %#v, want %#v", targets[0], want)
	}
	if want := (model.ReferenceTarget{Ref: gone, Kind: "block", Gone: true}); targets[1] != want {
		t.Errorf("the deleted block answered %#v, want it gone and unnamed", targets[1])
	}
	if want := (model.ReferenceTarget{Ref: "dispatch://" + other.Key, Kind: "issue", Name: other.Title}); targets[2] != want {
		t.Errorf("the other issue answered %#v, want %#v", targets[2], want)
	}
}

// The ETag covers the whole answer: the same set, unchanged, is answered 304 under the same tag,
// and a set one of whose members was renamed is answered the new name under a new tag. Nothing
// else in the set moved, so a tag over the references alone would tell a page holding it 304 for
// an answer that now names the issue differently.
func TestTheReferenceTargetsETagAnswers304UntilAMemberChanges(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "First paragraph.\n")
	other := createReferenceAPIIssue(t, handler, "CORE")
	ids := blockIDsByText(t, handler, issue.PrimaryArtifactID)
	set := []string{
		"dispatch://" + issue.Key + "/spec#b-" + text.EncodeBlockID(ids["First paragraph."]),
		"dispatch://" + other.Key,
	}

	_, etag := referenceTargets(t, handler, set)
	unchanged := referenceTargetsRequest(t, handler, set, etag)
	if unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 || unchanged.Header().Get("ETag") != etag {
		t.Fatalf("the unchanged set answered status=%d etag=%q body=%q, want 304 under the same tag",
			unchanged.Code, unchanged.Header().Get("ETag"), unchanged.Body.String())
	}

	renamed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+other.Key, map[string]any{
		"title": "Reference issue, renamed",
	}, "alice")
	if renamed.Code != http.StatusOK {
		t.Fatalf("retitle the other issue: status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	changed := referenceTargetsRequest(t, handler, set, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("the changed set answered status=%d body=%s, want the new answer", changed.Code, changed.Body.String())
	}
	if tag := changed.Header().Get("ETag"); tag == etag {
		t.Errorf("the changed set kept the ETag %s", tag)
	}
	targets := decodeBody[model.ReferenceTargets](t, changed).Targets
	if len(targets) != 2 || targets[1].Name != "Reference issue, renamed" {
		t.Errorf("the changed set answered %#v, want the issue's new title", targets)
	}
}

// A set at the cap is read, one past it is refused naming the cap, and a reference the grammar
// cannot read is refused naming it. The cap is counted before anything is parsed, so a set past
// it costs no reference's parse.
func TestReferenceTargetsRefuseASetPastTheCapAndAReferenceItCannotRead(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	reference := "dispatch://" + issue.Key

	atCap := make([]string, referenceTargetsMax)
	for index := range atCap {
		atCap[index] = reference
	}
	targets, _ := referenceTargets(t, handler, atCap)
	if len(targets) != referenceTargetsMax || targets[referenceTargetsMax-1].Name != issue.Title {
		t.Fatalf("a set of %d references answered %d members, want one per reference, each the issue",
			referenceTargetsMax, len(targets))
	}

	past := referenceTargetsRequest(t, handler, append(atCap, reference), "")
	if past.Code != http.StatusBadRequest ||
		!strings.Contains(past.Body.String(), `"code":"CAP_EXCEEDED"`) ||
		!strings.Contains(past.Body.String(), "ref is 1 over the 256-item limit (257/256)") {
		t.Fatalf("a set one past the cap answered status=%d body=%s, want 400 CAP_EXCEEDED naming the cap",
			past.Code, past.Body.String())
	}

	unreadable := referenceTargetsRequest(t, handler, []string{reference, "https://example.com/notes"}, "")
	if unreadable.Code != http.StatusBadRequest ||
		!strings.Contains(unreadable.Body.String(), `"code":"INVALID_REFERENCE"`) ||
		!strings.Contains(unreadable.Body.String(), "https://example.com/notes") {
		t.Fatalf("a reference the grammar cannot read answered status=%d body=%s, want 400 INVALID_REFERENCE naming it",
			unreadable.Code, unreadable.Body.String())
	}

	none := referenceTargetsRequest(t, handler, nil, "")
	if none.Code != http.StatusBadRequest || !strings.Contains(none.Body.String(), `"code":"INVALID_REFERENCE"`) {
		t.Fatalf("a read naming no reference answered status=%d body=%s, want 400 INVALID_REFERENCE",
			none.Code, none.Body.String())
	}
}
