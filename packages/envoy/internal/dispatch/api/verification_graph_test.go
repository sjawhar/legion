package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type verificationBody struct {
	Issue  string                    `json:"issue"`
	Checks []model.CheckBlock        `json:"checks"`
	Serves map[string][]servesTarget `json:"serves"`
}

func readVerification(t *testing.T, handler http.Handler, key string) verificationBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/verification", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("verification: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[verificationBody](t, response)
}

func TestVerificationGraphReadIsScopedToTheNamedIssuesDocument(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	own := createInteractionIssue(t, handler, "VGR", "Own", twoRequirements)
	other := createInteractionIssue(t, handler, "VGO", "Other project", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", checkServing("own", "dispatch://"+own.Key+"/spec#b-r1"))
	uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "verification.md", checkServing("other", "dispatch://"+other.Key+"/spec#b-r1"))
	answer := readVerification(t, handler, own.Key)
	if answer.Issue != own.Key || len(answer.Checks) != 1 || answer.Checks[0].BlockID != "own" {
		t.Fatalf("cross-issue data in verification: %+v", answer)
	}
	want := []servesTarget{{Reference: "dispatch://" + own.Key + "/spec#b-r1"}}
	if got := answer.Serves["own"]; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("serves = %v", answer.Serves)
	}
	census := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if census.Code != http.StatusOK {
		t.Fatal(census.Body.String())
	}
	report := decodeBody[verificationShadowReport](t, census)
	if report.Issues != 2 || report.Mismatches != 0 || report.NextIssue != "" {
		t.Fatalf("census scope: %s", census.Body.String())
	}
	for _, path := range []string{"/api/v1/issues/" + own.Key + "/verification", "/api/v1/verification-shadow"} {
		if response := dispatchRequest(t, handler, http.MethodGet, path, nil, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", path, response.Code)
		}
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/NOPE-1/verification", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("missing issue: %d", response.Code)
	}
}

func assertVerificationShadow(t *testing.T, handler http.Handler) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("verification shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[struct {
		Mismatches int `json:"mismatches"`
	}](t, response)
	if report.Mismatches != 0 {
		t.Fatalf("verification shadow mismatch: %s", response.Body.String())
	}
}

// The census's candidate read must agree with what the route serves today, field for field, or
// the census reports a cause rather than claiming equality.
func TestCensusCandidateJSONMatchesTodaysResults(t *testing.T) {
	handler, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VJR", "Results", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	if response := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)); response.Code != http.StatusCreated {
		t.Fatal(response.Body.String())
	}
	old, err := s.liveCheckResults(t.Context(), database.Pool, issue.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.graphCheckResults(t.Context(), database.Pool, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(old)
	nextJSON, _ := json.Marshal(next)
	if !bytes.Equal(oldJSON, nextJSON) {
		t.Fatalf("old: %s\nnew: %s", oldJSON, nextJSON)
	}
	if causes := resultMismatchCauses(old, next); len(causes) != 0 {
		t.Fatalf("equal JSON has causes %v; old result %#v new result %#v", causes, old.Checks[0].Results[0], next.Checks[0].Results[0])
	}
}

// Every state in which the block index does not hold a check the live document holds. In each of
// them /results and /verification list the check, in document order, with a zero version and
// digest - the promise results.go makes - and the check serves nothing rather than vanishing.
func TestChecksTheBlockIndexDoesNotHoldAreStillListed(t *testing.T) {
	for _, state := range []struct {
		name string
		// production is the production state this index state stands for.
		production string
		owe        func(t *testing.T, database *store.Store, artifactID string)
	}{
		{
			name:       "no block row at all",
			production: "an upload that wrote no tree, so the document has no indexed block",
			owe: func(t *testing.T, database *store.Store, artifactID string) {
				if _, err := database.Pool.Exec(context.Background(),
					`delete from document_blocks where artifact_id = $1::uuid`, artifactID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "one block row owed",
			production: "a document over 2,000 blocks, whose index docs/reference_excerpts.go defers to the 30-second reconciliation, and a resident room ahead of the store",
			owe: func(t *testing.T, database *store.Store, artifactID string) {
				if _, err := database.Pool.Exec(context.Background(),
					`delete from document_blocks where artifact_id = $1::uuid and block_id = 'k1'`, artifactID); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(state.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{})
			issue := createInteractionIssue(t, handler, "VOW", "Owed index", twoRequirements)
			document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			state.owe(t, database, document.Artifact.ID)
			t.Logf("index state: %s", state.production)

			results := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/results", nil, "alice")
			if results.Code != http.StatusOK {
				t.Fatalf("results: %d %s", results.Code, results.Body.String())
			}
			listed := decodeBody[checkResultsRead](t, results)
			if len(listed.Checks) != 2 || listed.Checks[0].BlockID != "k1" || listed.Checks[1].BlockID != "k2" {
				t.Fatalf("/results dropped a check the document holds: %s", results.Body.String())
			}
			if listed.Checks[0].Ref == "" {
				t.Fatalf("/results answered no reference for an unindexed check: %s", results.Body.String())
			}
			answer := readVerification(t, handler, issue.Key)
			if len(answer.Checks) != 2 || answer.Checks[0].BlockID != "k1" || answer.Checks[1].BlockID != "k2" {
				t.Fatalf("/verification dropped a check the document holds: %+v", answer.Checks)
			}
			if listed.Checks[0].Version != answer.Checks[0].Version || listed.Checks[0].SHA256 != answer.Checks[0].SHA256 {
				t.Fatalf("the two reads disagree about k1: %+v %+v", listed.Checks[0], answer.Checks[0])
			}
			if answer.Checks[0].Version != 0 || answer.Checks[0].SHA256 != "" {
				t.Fatalf("an owed check answered a version: %+v", answer.Checks[0])
			}
			if serves, named := answer.Serves["k1"]; !named || len(serves) != 0 {
				t.Fatalf("an owed check must have an empty serves entry: %+v", answer.Serves)
			}
		})
	}
}

// A check id longer than a B-tree index entry never becomes a node, so the index can never hold
// it. The document still holds the check, and both reads still list it.
func TestACheckIdTooLongToIndexIsStillListed(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VLG", "Long id", twoRequirements)
	long := strings.Repeat("c", 300)
	document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#"+long+"}\nA check whose id is longer than an index entry.\n:::\n")
	// Whatever the writer managed, the index must not decide what the reads list.
	if _, err := database.Pool.Exec(context.Background(),
		`delete from document_blocks where artifact_id = $1::uuid and length(block_id) > 256`, document.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	results := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/results", nil, "alice")
	listed := decodeBody[checkResultsRead](t, results)
	if len(listed.Checks) != 1 || listed.Checks[0].BlockID != long {
		t.Fatalf("/results hid the long check: %s", results.Body.String())
	}
	answer := readVerification(t, handler, issue.Key)
	if len(answer.Checks) != 1 || answer.Checks[0].BlockID != long {
		t.Fatalf("/verification hid the long check: %+v", answer.Checks)
	}
}

// Deleted and retyped targets remain citations in the check's document even when the typed
// graph no longer lists them as active serves edges. Both stay visible, marked removed.
func TestARemovedServesTargetIsListedAsRemoved(t *testing.T) {
	for _, state := range []struct {
		name, statement string
	}{
		{"deleted", `delete from document_blocks where artifact_id = $1::uuid and block_id = 'r1'`},
		{"retyped", `update document_blocks set type = 'paragraph' where artifact_id = $1::uuid and block_id = 'r1'`},
	} {
		t.Run(state.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{})
			issue := createInteractionIssue(t, handler, "VRM", "Removed target", twoRequirements)
			reference := "dispatch://" + issue.Key + "/spec#b-r1"
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k1", reference))
			if answer := readVerification(t, handler, issue.Key); len(answer.Serves["k1"]) != 1 || answer.Serves["k1"][0].Removed {
				t.Fatalf("the target is present, not removed: %+v", answer.Serves)
			}
			if _, err := database.Pool.Exec(context.Background(), state.statement, issue.PrimaryArtifactID); err != nil {
				t.Fatal(err)
			}
			got := readVerification(t, handler, issue.Key).Serves["k1"]
			if len(got) != 1 || got[0].Reference != reference || !got[0].Removed {
				t.Fatalf("a %s target is not reported as removed: %+v", state.name, got)
			}
		})
	}
}

func TestServesReferencesEncodeTheTargetBlockID(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VEN", "Encoded target", ":::requirement{#r_1}\nEncoded requirement.\n:::\n")
	reference := "dispatch://" + issue.Key + "/spec#b-r%5F1"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k1", reference))
	for _, removed := range []bool{false, true} {
		if removed {
			if _, err := database.Pool.Exec(t.Context(), `delete from document_blocks where artifact_id = $1::uuid and block_id = 'r_1'`, issue.PrimaryArtifactID); err != nil {
				t.Fatal(err)
			}
		}
		got := readVerification(t, handler, issue.Key).Serves["k1"]
		if len(got) != 1 || got[0].Reference != reference || got[0].Removed != removed {
			t.Fatalf("removed=%t: serves = %+v, want %s", removed, got, reference)
		}
	}
}

func TestVerificationShadowCountsEveryDocumentAndDetectsMissingMembership(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VSH", "Shadow", twoRequirements)
	document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	// Simulate a document whose index is owed. The census must not silently claim equality.
	if _, err := database.Pool.Exec(context.Background(), `delete from document_blocks where artifact_id = $1::uuid and block_id = 'k1'`, document.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[verificationShadowReport](t, response)
	if report.Issues != 1 || report.Mismatches != 1 || report.ByCause["check_membership"] != 1 || len(report.Details) != 1 || report.Details[0].Issue != issue.Key {
		t.Fatalf("census hid missing membership: %s", response.Body.String())
	}
}

func TestCoverageKeepsTheLegacyReadWhileTheShadowReportsADifference(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VCP", "Coverage candidate", twoRequirements)
	document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k1", "dispatch://"+issue.Key+"/spec#b-r1"))
	// A pointer whose type constraint disagrees with its target is hidden by graph_edges,
	// but the existing coverage read still counts the reference to the requirement.
	if _, err := database.Pool.Exec(t.Context(), `update block_refs set to_types = array['paragraph'] where artifact_id = $1::uuid and kind = 'serves'`, document.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	coverage := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/coverage", nil, "alice")
	if coverage.Code != http.StatusOK {
		t.Fatalf("coverage: %d %s", coverage.Code, coverage.Body.String())
	}
	answer := decodeBody[model.IssueCoverage](t, coverage)
	if len(answer.Requirements) != 2 || answer.Requirements[0].Checks != 1 {
		t.Fatalf("coverage switched to the candidate: %s", coverage.Body.String())
	}
	shadow := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if shadow.Code != http.StatusOK {
		t.Fatalf("shadow: %d %s", shadow.Code, shadow.Body.String())
	}
	report := decodeBody[verificationShadowReport](t, shadow)
	if report.Issues != 1 || report.Mismatches != 1 || report.ByCause["coverage"] != 1 {
		t.Fatalf("shadow hid the coverage difference: %s", shadow.Body.String())
	}
}

// The census is a page, not a fleet sweep: it reads at most limit issues and names where the
// next page starts.
func TestVerificationShadowReadsOnePageAtATime(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	keys := []string{}
	for _, project := range []string{"VPA", "VPB", "VPC"} {
		issue := createInteractionIssue(t, handler, project, "Paged", twoRequirements)
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
		keys = append(keys, issue.Key)
	}
	page := func(query string) verificationShadowReport {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("shadow%s: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[verificationShadowReport](t, response)
	}
	first := page("?limit=2")
	if first.Issues != 2 || first.NextIssue != keys[2] {
		t.Fatalf("first page: %+v", first)
	}
	second := page("?issue=" + first.NextIssue)
	if second.Issues != 1 || second.NextIssue != "" {
		t.Fatalf("second page: %+v", second)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=many"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow"+query, nil, "alice"); response.Code != http.StatusBadRequest {
			t.Fatalf("shadow%s: %d", query, response.Code)
		}
	}
}
