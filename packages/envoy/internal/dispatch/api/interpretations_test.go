package api

import (
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInterpretationsStayUnderTheirDecisionWithRecordedAuthor(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#choice}\nWhich name?\n:::\n\n:::decision{#own title=\"Agent choice\"}\nUse a table.\n:::\n")
	view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
	ask := view.OpenQuestions[0].Ask
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}, "text": "My exact answer.", "expected_edited_at": ask.EditedAt}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", response.Code, response.Body.String())
	}
	route := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/interpretations"
	response = sessionRequest(t, handler, http.MethodPost, route, map[string]any{"decision": "choice", "body": "The agent understands the answer this way.", "actor": map[string]string{"kind": "session", "id": "reader-agent"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("interpret: %d %s", response.Code, response.Body.String())
	}
	view = readIssueDecisions(t, handler, issue.Key)
	if len(view.Decisions) != 2 {
		t.Fatalf("interpretation counted as a decision: %s", mustJSON(t, view))
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/interpretations", nil, "alice")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "reader-agent") || !strings.Contains(response.Body.String(), "The agent understands") {
		t.Fatalf("projection lost provenance: %d %s", response.Code, response.Body.String())
	}
	items := decodeBody[[]model.Interpretation](t, response)
	if len(items) != 1 || items[0].Text != "The agent understands the answer this way." || items[0].Author == nil || items[0].Author.ID != "reader-agent" || items[0].DecisionRef != "dispatch://"+issue.Key+"/spec#b-choice" {
		t.Fatalf("a person's answer became an interpretation: %#v", items)
	}
	if !strings.Contains(mustJSON(t, view), "My exact answer.") {
		t.Fatalf("person's answer lost: %s", mustJSON(t, view))
	}
	var count int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_edges where kind='interprets' and to_id=$1`, issue.Key+"/spec#choice").Scan(&count); err != nil || count != 1 {
		t.Fatalf("interprets edges=%d err=%v", count, err)
	}
	// Human-authenticated writes cannot masquerade as an agent interpretation.
	response = dispatchRequest(t, handler, http.MethodPost, route, map[string]any{"decision": "choice", "body": "Person text."}, "alice")
	if response.Code != http.StatusForbidden {
		t.Fatalf("human interpretation: %d %s", response.Code, response.Body.String())
	}
	response = sessionRequest(t, handler, http.MethodPost, route, map[string]any{"decision": "missing", "body": "Not a decision.", "actor": map[string]string{"kind": "session", "id": "reader-agent"}})
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing decision: %d %s", response.Code, response.Body.String())
	}
}

func readInterpretations(t *testing.T, handler http.Handler, key string) []model.Interpretation {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/interpretations", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("interpretations: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[[]model.Interpretation](t, response)
}

func TestHandmadeInterpretationsAndReusedIDsDoNotClaimAnotherAuthor(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#choice title=\"A choice\"}\nUse a table.\n:::\n")
	before := readIssueDecisions(t, handler, issue.Key).Decisions[0]
	route := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/interpretations"
	response := sessionRequest(t, handler, http.MethodPost, route, map[string]any{"decision": "choice", "body": "Original reading.", "actor": model.Actor{Kind: "session", ID: "original-agent"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	original := decodeBody[model.Interpretation](t, response)
	after := readIssueDecisions(t, handler, issue.Key).Decisions[0]
	if before.TextSHA256 != after.TextSHA256 || before.ChangedVersion != after.ChangedVersion {
		t.Fatalf("interpretation changed parent: %#v -> %#v", before, after)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Original reading.", "with": "Edited reading."}, {"op": "move", "block": original.BlockID, "after": "end"}})
	items := readInterpretations(t, handler, issue.Key)
	if len(items) != 1 || items[0].Author == nil || items[0].Author.ID != "original-agent" || items[0].Text != "Edited reading." {
		t.Fatalf("edit lost trusted author: %#v", items)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": original.BlockID}})
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::interpretation{#" + original.BlockID + " interprets=\"dispatch://" + issue.Key + "/spec#b-choice\"}\nHand-made reading.\n:::\n"}})
	items = readInterpretations(t, handler, issue.Key)
	if len(items) != 1 || items[0].Author != nil || items[0].Text != "Hand-made reading." {
		t.Fatalf("hand-made node claimed historical author: %#v", items)
	}
	if len(readIssueDecisions(t, handler, issue.Key).Decisions) != 1 {
		t.Fatal("hand-made interpretation counted as a decision")
	}
	for _, input := range []map[string]any{
		{"decision": "choice", "body": "Forged.", "actor": model.Actor{Kind: "user", ID: "alice"}},
		{"decision": "choice", "body": ":::decision{#nested}\nA person's words are not an interpretation.\n:::\n", "actor": model.Actor{Kind: "session", ID: "agent"}},
	} {
		refused := sessionRequest(t, handler, http.MethodPost, route, input)
		if refused.Code != http.StatusBadRequest {
			t.Fatalf("unsafe input: %d %s", refused.Code, refused.Body.String())
		}
	}
}

func TestInterpretationGraphEqualityAndScope(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#choice}\nOriginal decision.\n:::\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Other isolated interpretation", "spec": ":::decision{#other}\nOther decision.\n:::\n", "force": true}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("other issue: %d %s", created.Code, created.Body.String())
	}
	other := decodeBody[model.Issue](t, created)
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::interpretation{#reading interprets=\"dispatch://" + issue.Key + "/spec#b-choice\"}\nLocal reading.\n:::\n"}})
	editSpec(t, handler, other.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::interpretation{#other-reading interprets=\"dispatch://" + other.Key + "/spec#b-other\"}\nOther reading.\n:::\n"}})
	_, blocks, err := deps.Docs.TextWithBlocks(t.Context(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	source := 0
	for _, block := range blocks {
		if block.Type == "interpretation" {
			source++
		}
	}
	count := len(readInterpretations(t, handler, issue.Key))
	if source != 1 || count != source {
		t.Fatalf("source=%d graph=%d", source, count)
	}
	t.Logf("interpretation equality: source=%d graph=%d differences=0", source, count)
	if _, err := database.Pool.Exec(t.Context(), `delete from block_refs where artifact_id=$1 and kind='interprets'`, issue.PrimaryArtifactID); err != nil {
		t.Fatal(err)
	}
	count = len(readInterpretations(t, handler, issue.Key))
	if count != 0 {
		t.Fatalf("negative control: expected one missing edge, got graph=%d", count)
	}
	t.Logf("negative control: source=%d graph=%d differences=%d", source, count, source-count)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/interpretations", nil, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", response.Code)
	}
}

func TestNestedInterpretationIsNeverDecisionText(t *testing.T) {
	text := decisionNodeText("::::decision{#own title=\"Own choice\"}\nKeep a table.\n\n:::interpretation{#reading interprets=\"dispatch://CORE-1/spec#b-own\"}\nAn interpretation is not the decision.\n:::\n::::\n")
	if strings.Contains(text, "interpretation") || !strings.Contains(text, "Keep a table.") {
		t.Fatalf("decision includes interpretation prose: %q", text)
	}
}

func TestInterpretationAdditionsAndEditsCarrySpecApproval(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Approved prose.\n\n:::decision{#choice title=\"A choice\"}\nUse a table.\n:::\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/interpretations", map[string]any{
		"decision": "choice", "body": "Original reading.", "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("interpret: %d %s", response.Code, response.Body.String())
	}
	item := decodeBody[model.Interpretation](t, response)
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 2, 1, "Carried forward from the approval of version 1: only interpretation blocks were added or edited.", nil)
	advice := adviceFromResponse(t, response.Code, response.Body.String())
	if advice.SupersededApproval != nil {
		t.Fatalf("interpretation superseded approval: %#v", advice.SupersededApproval)
	}
	edited := agentEdit(t, handler, issue.PrimaryArtifactID, 3, replaceOp("Original reading.", "Edited reading."))
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 2, "Carried forward from the approval of version 2: only interpretation blocks were added or edited.", nil)
	if edited.SupersededApproval != nil {
		t.Fatalf("interpretation edit superseded approval: %#v", edited.SupersededApproval)
	}
	// Removing an interpretation is not an addition or edit covered by this rule.
	agentEdit(t, handler, issue.PrimaryArtifactID, 4, map[string]string{"op": "delete", "block": item.BlockID})
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 3, "stale")
}

func TestInterpretationCannotCarryApprovalAcrossChangedSpecText(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Approved prose.\n\n:::decision{#choice}\nUse a table.\n:::\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("Approved prose.", "Unapproved prose."))
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/interpretations", map[string]any{
		"decision": "choice", "body": "A reading of the changed spec.", "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("interpret: %d %s", response.Code, response.Body.String())
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

func TestInterpretationsFollowDocumentPosition(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#choice}\nUse a table.\n:::\n")
	for _, id := range []string{"z-first", "a-second"} {
		editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::interpretation{#" + id + " interprets=\"dispatch://" + issue.Key + "/spec#b-choice\"}\n" + id + " reading.\n:::\n"}})
	}
	items := readInterpretations(t, handler, issue.Key)
	if len(items) != 2 || items[0].BlockID != "z-first" || items[1].BlockID != "a-second" {
		t.Fatalf("document order: %#v", items)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "move", "block": "a-second", "after": "block:choice"}})
	items = readInterpretations(t, handler, issue.Key)
	if len(items) != 2 || items[0].BlockID != "a-second" || items[1].BlockID != "z-first" {
		t.Fatalf("moved document order: %#v", items)
	}
}

func TestInterpretationReportsLiveInsertSurvival(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#choice}\nUse a table.\n:::\n")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/interpretations", map[string]any{
		"decision": "choice", "body": "An uncontended reading.", "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("interpret: %d %s", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		LostOps *[]int `json:"lost_ops"`
	}](t, response)
	if body.LostOps == nil || len(*body.LostOps) != 0 {
		t.Fatalf("insert survival verdict: %s", response.Body.String())
	}
}

func TestInterpretationReportsApprovalSupersededByUnsettledSpecEdit(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Approved prose.\n\n:::decision{#choice}\nUse a table.\n:::\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	// A committed live edit has not been captured by a version yet. The interpretation's
	// snapshot will capture both changes, so it must report the approval it supersedes.
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	ctx, ledger := deps.Docs.Join(t.Context(), tx)
	defer ledger.Discard()
	if _, err := deps.Docs.ApplyOps(ctx, issue.PrimaryArtifactID, []model.EditOp{{Op: "replace", Find: "Approved prose.", With: "Unapproved prose."}}, model.Actor{Kind: "session", ID: "live-editor"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/interpretations", map[string]any{
		"decision": "choice", "body": "A reading alongside a live edit.", "actor": sessionActor(),
	})
	advice := adviceFromResponse(t, response.Code, response.Body.String())
	if advice.SupersededApproval == nil || advice.SupersededApproval.ApprovedVersion != 1 || advice.SupersededApproval.Version != 2 || advice.SupersededApproval.ApprovedBy != "alice" {
		t.Fatalf("superseded approval advice: %s", response.Body.String())
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}
