package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The near-copy gate's tests. Every fixture scores at an extreme - an exact copy of an open
// issue's title and spec holds every picked term, a text sharing nothing holds none - so each
// case holds for any pair of bars with 0 < duplicateNudgeScore < duplicateRefuseScore < 1, and
// the replay that sets those two numbers cannot break them.

func createIssueRequest(t *testing.T, handler http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

func createDuplicateTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func requireCreatedIssue(t *testing.T, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// duplicateRefusal is the 409 body the gate answers, read as a client reads it.
type duplicateRefusal struct {
	Error      string                     `json:"error"`
	Code       string                     `json:"code"`
	Candidates []model.DuplicateCandidate `json:"candidates"`
}

func requireDuplicateRefusal(t *testing.T, response *httptest.ResponseRecorder) duplicateRefusal {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("near copy: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[duplicateRefusal](t, response)
	if body.Code != "POSSIBLE_DUPLICATE" {
		t.Fatalf("refusal code = %q, want POSSIBLE_DUPLICATE", body.Code)
	}
	return body
}

// The texts below are built from words that appear in one fixture and not the other, so a pair
// either shares its whole vocabulary or none of it.
const (
	duplicateSpecAstrolabe = "The astrolabe's calibration drifts whenever the gnomon is reseated, " +
		"so every altitude reading taken after a reseat is wrong by a constant offset. " +
		"Record the offset at each reseat and subtract it from the altitude the dial reports."
	duplicateSpecHarvest = "Orchard pickers cannot log a crate until the weighbridge answers, " +
		"and the weighbridge answers only while the loading dock has power. " +
		"Buffer each crate locally and reconcile the weights when the dock comes back."
)

func TestCreateIssueDuplicateRefusesANearCopyOfAnOpenIssue(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	second := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	})
	body := requireDuplicateRefusal(t, second)
	if len(body.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(body.Candidates), body.Candidates)
	}
	candidate := body.Candidates[0]
	if candidate.Key != first.Key || candidate.Title != "Astrolabe calibration drifts after a reseat" ||
		candidate.Status != "triage" || candidate.SharedTerms == 0 ||
		candidate.Href != "/issues/"+first.Key {
		t.Fatalf("candidate = %#v", candidate)
	}
	if !strings.Contains(candidate.Snippet, "<mark>astrolabe</mark>") &&
		!strings.Contains(candidate.Snippet, "<mark>Astrolabe</mark>") {
		t.Fatalf("candidate snippet marks no shared word: %q", candidate.Snippet)
	}
	if body.Error != "possible duplicate of "+first.Key+": "+first.Title {
		t.Fatalf("refusal error = %q", body.Error)
	}

	// Nothing was written, so the next create takes the next number.
	next := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	if next.Key != "TEST-2" {
		t.Fatalf("issue created after the refusal = %q, want TEST-2", next.Key)
	}
}

func TestCreateIssueDuplicateCreatesACopyOfADoneIssue(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	second := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("copy of a done issue = %q, want TEST-2", second.Key)
	}
}

func TestCreateIssueDuplicateCreatesACopyOfAnAncestor(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// The grandparent is the only near copy in the project, so a gate that leaves out the parent
	// alone refuses the grandchild.
	grandparent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	parent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": grandparent.Key,
		"title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	grandchild := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key,
		"title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	}))
	if grandchild.Key != "TEST-3" {
		t.Fatalf("copy of a grandparent = %q, want TEST-3", grandchild.Key)
	}

	// The parent itself is the other half of the clause.
	child := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key,
		"title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	if child.Key != "TEST-4" {
		t.Fatalf("copy of a parent = %q, want TEST-4", child.Key)
	}
}

func TestCreateIssueDuplicateCreatesATextSharingOnlyCommonWords(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	// Every word these two texts share is an English stop word, which the tokenizer removes, so
	// no term of the new text is one the open issue holds.
	second := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate after the dock is down",
		"spec": duplicateSpecHarvest,
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("text sharing only common words = %q, want TEST-2", second.Key)
	}
}

func TestCreateIssueDuplicateForcePassesTheGate(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	if forced.Key != "TEST-2" {
		t.Fatalf("forced issue key = %q, want TEST-2", forced.Key)
	}
}

func TestCreateIssueDuplicateIgnoresACitedIssueKey(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	cited := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	// The new text shares nothing with the cited issue but its key, which `skill://dispatch-first`
	// tells a filer to write down. The key is the rarest lexeme in the project, so a gate that
	// scored it would refuse this create.
	second := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate",
		"spec": duplicateSpecHarvest + "\n\nNearest issue ruled out: dispatch://" + cited.Key + ".",
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("create citing %s = %q, want TEST-2", cited.Key, second.Key)
	}
}

func TestCreateIssueDuplicateScopesTheGateToTheProjectAndSkipsExternal(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	createDuplicateTestProject(t, handler, "OTHER")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	other := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "OTHER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if other.Key != "OTHER-1" {
		t.Fatalf("cross-project issue key = %q, want OTHER-1", other.Key)
	}
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	external := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if external.Key != "TEST-2" {
		t.Fatalf("external issue key = %q, want TEST-2", external.Key)
	}
}

func TestCreateIssueDuplicateOrdersCandidatesByScore(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "ORDER")

	// Three open issues holding progressively more of the new text's words. Each is forced in,
	// because each is a near copy of the one before it.
	whole := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	half := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec":  "The astrolabe's calibration drifts whenever the gnomon is reseated.",
		"force": true,
	}))
	sliver := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration", "force": true,
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	})
	body := requireDuplicateRefusal(t, duplicate)
	keys := make([]string, 0, len(body.Candidates))
	for _, candidate := range body.Candidates {
		keys = append(keys, candidate.Key)
	}
	// Each named candidate is at or above the refusal bar; the order is by score, best first.
	if len(keys) == 0 || keys[0] != whole.Key {
		t.Fatalf("candidates = %v, want %q first", keys, whole.Key)
	}
	for index := 1; index < len(keys); index++ {
		if keys[index] == whole.Key {
			t.Fatalf("candidates = %v, want %q once and first", keys, whole.Key)
		}
	}
	rank := map[string]int{whole.Key: 0, half.Key: 1, sliver.Key: 2}
	for index := 1; index < len(keys); index++ {
		if rank[keys[index-1]] > rank[keys[index]] {
			t.Fatalf("candidates = %v, want them ordered %q, %q, %q", keys, whole.Key, half.Key, sliver.Key)
		}
	}
}

// pickRelatedTerms drops a lexeme more than a tenth of the project holds, and never fewer than
// relatedCommonFloor of its items (`commonAbove`). So in a project whose items have made the
// words of a title common, a title yields no picked term at all and there is no share to score.
//
// dispatch://INFRA-1160/artifact/verification-md's check k4 names INFRA-432 against INFRA-431 —
// two open issues with one title and no spec at all — as a pair the rule must refuse. Below
// duplicateMinTerms the gate scores nothing and asks the other question instead: does an open
// issue hold the same lexemes, title and spec alike (duplicateEqualLexemesQuery). A copy does,
// which is this case and that pair, and a title differing in one meaning word does not, which is
// TestCreateIssueDuplicateCreatesTwoTitlesThatDifferInTheirOneMeaningWord.
func TestCreateIssueDuplicateRefusesACopiedTitleWhoseWordsAreCommon(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDuplicateTestProject(t, handler, "COMMON")
	// Twelve issues holding the three words of the title below, so each word's document
	// frequency passes relatedCommonFloor and the picker drops it as a common word.
	for index := range 12 {
		requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
			"project": "COMMON",
			"title":   fmt.Sprintf("Astrolabe reseat note %s", string(rune('a'+index))),
			"spec":    "The astrolabe is reseated and its gimbal recalibrated.",
			"force":   true,
		}))
	}
	original := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "COMMON", "title": "Astrolabe gimbal reseated", "force": true,
	}))
	// The statistics as a create meets them in production: current, not ten creates behind.
	if _, err := refreshSearchTermStats(context.Background(), database.Pool, "COMMON"); err != nil {
		t.Fatalf("refresh the term statistics: %v", err)
	}

	copied := createIssueRequest(t, handler, map[string]any{
		"project": "COMMON", "title": "Astrolabe gimbal reseated",
	})

	body := requireDuplicateRefusal(t, copied)
	keys := make([]string, 0, len(body.Candidates))
	for _, candidate := range body.Candidates {
		keys = append(keys, candidate.Key)
	}
	if len(keys) == 0 || keys[0] != original.Key {
		t.Fatalf("candidates = %v, want %q first", keys, original.Key)
	}
}

// Two short titles that differ in the one word carrying their meaning are different work, and
// R2 says a text sharing only common words is created. pickRelatedTerms now fills its list back
// up to duplicateMinTerms from the words it dropped as common, so in a project of two issues the
// filled list is almost all shared words and the share the candidate holds reaches the refusal
// bar: the second issue is refused as a near copy of the first.
//
// These two titles are the seed of packages/dispatch/e2e/agent-name.e2e.ts, whose ten browser
// tests fail at this head with `POST /api/v1/issues failed: 409` on the second create.
func TestCreateIssueDuplicateCreatesTwoTitlesThatDifferInTheirOneMeaningWord(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "AGT")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "AGT", "title": "Held by a running agent",
		"spec": "The running agent comments here. The stopped agent comments there.",
	}))
	second := createIssueRequest(t, handler, map[string]any{
		"project": "AGT", "title": "Held by a stopped agent",
	})

	if second.Code != http.StatusCreated {
		t.Fatalf("second create: status=%d body=%s", second.Code, second.Body.String())
	}
}

// A spec is stored as Dispatch renders it, not as the request wrote it: the seed parses the
// markdown and stores its rendering (docs' SeedText), which renumbers an ordered list, and
// Postgres keeps a list's numbers as lexemes. Below duplicateMinTerms the gate compares the new
// spec's lexemes with the stored row's, so it renders the new spec the same way first
// (duplicateStoredSpec); built from the request's markdown instead, the copy of a spec written
// `1.` `1.` holds no `2` where the row it copies holds one, and the exact copy R2 refuses was
// created.
func TestCreateIssueDuplicateRefusesACopiedSpecWhoseListIsRenumbered(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "LIST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n1. Check\n",
	}))
	// The copy as the first create wrote it, the numbering the renderer changes.
	copied := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n1. Check\n",
	}))
	if len(copied.Candidates) != 1 || copied.Candidates[0].Key != first.Key {
		t.Fatalf("candidates for the copy = %#v, want %q alone", copied.Candidates, first.Key)
	}
	// The same copy written with the numbers the renderer gives it: the other ordering of the
	// one spec, and the same near copy.
	renumbered := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n2. Check\n",
	}))
	if len(renumbered.Candidates) != 1 || renumbered.Candidates[0].Key != first.Key {
		t.Fatalf("candidates for the renumbered copy = %#v, want %q alone", renumbered.Candidates, first.Key)
	}
}
