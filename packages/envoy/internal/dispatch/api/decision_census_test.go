package api

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type decisionCensus struct {
	Differences int      `json:"differences"`
	Missing     []string `json:"missing"`
	Extra       []string `json:"extra"`
}

func readDecisionCensus(t *testing.T, handler http.Handler, key string) decisionCensus {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/decisions-shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("decision census: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[decisionCensus](t, response)
}

func TestDecisionConversionKeepsLegacyUploadIdentity(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createConversionIssue(t, handler, "The legacy writer remains active")
	legacy := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## Original choice\n\nOriginal reasoning.\n")
	var out bytes.Buffer
	if _, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out); err != nil {
		t.Fatal(err)
	}
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "decisions.md", "content": "## New choice\n\nNew reasoning.\n", "actor": map[string]any{"kind": "session", "id": "legacy-writer"},
	}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("legacy upload: %d %s", response.Code, response.Body.String())
	}
	updated := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response).Artifact
	if updated.ID != legacy.ID || updated.Slug != "decisions-md" || updated.Name != "decisions.md" {
		t.Fatalf("legacy writer forked its source: before=%#v after=%#v", legacy, updated)
	}
	markdown, err := deps.Docs.Text(t.Context(), legacy.ID)
	if err != nil || !strings.Contains(markdown, "New reasoning.") {
		t.Fatalf("the reader's original artifact is stale: %q (%v)", markdown, err)
	}
	if census := readDecisionCensus(t, handler, issue.Key); census.Differences == 0 {
		t.Fatalf("a later legacy write must stop the cutover until migrated: %#v", census)
	}
}

func TestDecisionCensusFlagsAndRestoresHistoricallyRetypedAnswer(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Context.\n\n:::ask{#answered-here}\nWhich name?\n:::\n")
	view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
	ask := view.OpenQuestions[0].Ask
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}, "text": "The original answer.", "expected_edited_at": ask.EditedAt}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", response.Code, response.Body.String())
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "answered-here", "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Keep this later note"}}})
	census := readDecisionCensus(t, handler, issue.Key)
	if census.Differences == 0 || !slices.Contains(census.Missing, "dispatch://"+issue.Key+"/ask/"+ask.ID) {
		t.Fatalf("a retained answered row disappeared from the census: %#v", census)
	}
	var out bytes.Buffer
	report, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err != nil || report.Restored != 1 {
		t.Fatalf("restore retyped answer: %#v %v\n%s", report, err, out.String())
	}
	nodes, err := (&server{deps: deps}).issueDecisionNodes(t.Context(), issue.Key)
	if err != nil || len(nodes.Decisions) != 1 || nodes.Decisions[0].Kind != "person" || len(nodes.Decisions[0].Asks) != 1 || nodes.Decisions[0].Asks[0] != ask.ID || !strings.Contains(nodes.Decisions[0].Text, "The original answer.") {
		t.Fatalf("restored a copy or lost the answer: %#v (%v)", nodes, err)
	}
	if nodes.Decisions[0].BlockID == "answered-here" {
		t.Fatal("restoration overwrote the later callout instead of choosing a free block id")
	}
	markdown, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
	if err != nil || !strings.Contains(markdown, "Keep this later note") {
		t.Fatalf("restoration destroyed the later note: %q (%v)", markdown, err)
	}
	var count int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from asks where issue_key=$1`, issue.Key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restoration duplicated the person's ask row: %d (%v)", count, err)
	}
	if census := readDecisionCensus(t, handler, issue.Key); census.Differences != 0 {
		t.Fatalf("restored answer still differs: %#v", census)
	}
}

func TestDecisionCensusDetectsLostShapedRelation(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", shipIt("The passage this choice shaped.")+"\n")
	target := "dispatch://" + issue.Key + "/spec#b-ship%5Fit"
	uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## Shaped choice\n\nUse a table. It shapes "+target+".\n")
	var out bytes.Buffer
	if _, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out); err != nil {
		t.Fatal(err)
	}
	before, err := (&server{deps: deps}).issueDecisionNodes(t.Context(), issue.Key)
	if err != nil || len(before.Decisions) != 1 || len(before.Decisions[0].Links) != 1 {
		t.Fatalf("conversion did not shape the passage: %#v (%v)", before, err)
	}
	if census := readDecisionCensus(t, handler, issue.Key); census.Differences != 0 {
		t.Fatalf("converted shaped choice differs: %#v", census)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": before.Decisions[0].BlockID, "type": "decision", "attributes": map[string]any{"title": "Shaped choice"}}})
	after, err := (&server{deps: deps}).issueDecisionNodes(t.Context(), issue.Key)
	if err != nil || len(after.Decisions) != 1 || len(after.Decisions[0].Links) != 0 || after.Decisions[0].TextSHA256 != before.Decisions[0].TextSHA256 {
		t.Fatalf("counterexample must lose only the relation: before=%#v after=%#v (%v)", before, after, err)
	}
	census := readDecisionCensus(t, handler, issue.Key)
	if census.Differences == 0 || len(census.Missing) == 0 || !strings.Contains(strings.Join(census.Missing, "\n"), target) {
		t.Fatalf("lost shaped relation passed the census: %#v", census)
	}
}
