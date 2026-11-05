package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestDecisionAnsweredAskRefusesAgentRemovalButAllowsPerson(t *testing.T) {
	for _, write := range []string{"delete", "replace document", "retype"} {
		t.Run(write, func(t *testing.T) {
			handler, _, deps := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
			createReferenceAPIProject(t, handler, "CORE")
			issue := createSpecIssue(t, handler, "CORE", "Context.\n\n:::ask{#chosen}\nWhich name?\n:::\n")
			view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
			ask := view.OpenQuestions[0].Ask
			answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}, "text": "Keep my choice.", "expected_edited_at": ask.EditedAt}, "alice")
			if answered.Code != http.StatusOK {
				t.Fatalf("answer: %d %s", answered.Code, answered.Body.String())
			}
			before, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits"
			body := map[string]any{"ops": []map[string]any{{"op": "delete", "block": "chosen"}}, "summary": "Remove the block"}
			success := http.StatusOK
			switch write {
			case "replace document":
				path = "/api/v1/issues/" + issue.Key + "/artifacts"
				body = map[string]any{"name": "spec.md", "content": "Context.\n"}
				success = http.StatusCreated
			case "retype":
				body["ops"] = []map[string]any{{"op": "retype", "block": "chosen", "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Old choice"}}}
			}
			body["actor"] = map[string]any{"kind": "session", "id": "decision-editor"}
			refused := agentRequest(t, handler, http.MethodPost, path, body, "agent-token")
			if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), ask.ID) {
				t.Fatalf("agent %s: %d %s, want 409 naming %s", write, refused.Code, refused.Body.String(), ask.ID)
			}
			after, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
			if err != nil || after != before {
				t.Fatalf("refused write changed document: %q -> %q (%v)", before, after, err)
			}
			delete(body, "actor")
			allowed := dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
			if allowed.Code != success {
				t.Fatalf("person %s: %d %s", write, allowed.Code, allowed.Body.String())
			}
		})
	}
}

func TestDecisionKeepRequiresPersonScope(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Context.\n")
	legacy := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## The choice\n\nUse a table.\n")
	decision := decisionNamed(t, readIssueDecisions(t, handler, issue.Key), "The choice")
	path := "/api/v1/artifacts/" + legacy.ID + "/blocks/" + decision.BlockID + "/keep"
	body := map[string]any{"text_sha256": decision.TextSHA256}
	refused := agentRequest(t, handler, http.MethodPost, path, body, "agent-token")
	if refused.Code != http.StatusForbidden {
		t.Fatalf("unscoped Keep: %d %s", refused.Code, refused.Body.String())
	}
	allowed := agentRequest(t, handler, http.MethodPost, path, body, personalTokenHolding(t, database, model.ScopePerson))
	if allowed.Code != http.StatusOK {
		t.Fatalf("person-scoped Keep: %d %s", allowed.Code, allowed.Body.String())
	}
	if keep := decodeBody[model.DecisionKeep](t, allowed); keep.KeptBy != "alice" {
		t.Fatalf("Keep attributed to %q, want alice", keep.KeptBy)
	}
}

func TestDecisionConversionPreservesLegacyLinkAndReplacesSpecPointer(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createConversionIssue(t, handler, "Stable legacy link")
	pointer := "The decisions are in dispatch://" + issue.Key + "/artifact/decisions-md."
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": pointer}})
	legacy := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## Use a table\n\nKeep this reasoning.\n")
	var out bytes.Buffer
	report, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err != nil || report.Moved != 1 {
		t.Fatalf("convert: %#v %v\n%s", report, err, out.String())
	}
	var slug string
	if err := database.Pool.QueryRow(t.Context(), `select slug from artifacts where id=$1`, legacy.ID).Scan(&slug); err != nil || slug != "decisions-md" {
		t.Fatalf("legacy slug = %q (%v)", slug, err)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/decisions-md", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("old link: %d %s", read.Code, read.Body.String())
	}
	markdown, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
	if err != nil || strings.Contains(markdown, pointer) || !strings.Contains(markdown, "The decisions are the decision and answered ask nodes in this spec.") {
		t.Fatalf("spec pointer not converted: %q (%v)", markdown, err)
	}
	again, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err != nil || again.Documents != 0 {
		t.Fatalf("retry: %#v %v\n%s", again, err, out.String())
	}
}

func TestDecisionShadowFindsUnconvertedChangedAndUnkeptSections(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createConversionIssue(t, handler, "Independent shadow source")
	legacy := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## Use a table\n\nKeep this reasoning.\n")
	path := "/api/v1/issues/" + issue.Key + "/decisions-shadow"
	read := func(want int) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		census := decodeBody[struct {
			Differences int      `json:"differences"`
			Missing     []string `json:"missing"`
			Extra       []string `json:"extra"`
		}](t, response)
		if response.Code != http.StatusOK || census.Differences != want {
			t.Fatalf("shadow want %d differences: %d %s", want, response.Code, response.Body.String())
		}
		if want > 0 && len(census.Missing) == 0 {
			t.Fatalf("shadow did not report missing sections or Keeps: %s", response.Body.String())
		}
	}
	read(1)
	markdown, blocks, err := deps.Docs.TextWithBlocks(t.Context(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range blocks {
		if block.Type == "heading" && strings.Contains(markdown[block.From:block.To], "Use a table") {
			if _, err := database.Pool.Exec(t.Context(), `insert into decision_keeps(artifact_id,block_id,text_sha256,changed_version,kept_by) values($1,$2,$3,1,'alice')`, legacy.ID, block.ID, decisionTextSum("## Use a table\n\nKeep this reasoning.")); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out bytes.Buffer
	if _, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out); err != nil {
		t.Fatalf("convert: %v\n%s", err, out.String())
	}
	read(0)
	if _, err := database.Pool.Exec(t.Context(), `delete from decision_keeps where artifact_id=$1`, issue.PrimaryArtifactID); err != nil {
		t.Fatal(err)
	}
	read(1)
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Keep this reasoning.", "with": "Different reasoning."}})
	read(3)
}

func TestDecisionAgentCannotClaimAnotherDecider(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Context.\n")
	for _, session := range []string{"decision-editor", "alice"} {
		t.Run(session, func(t *testing.T) {
			response := agentRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"actor": map[string]any{"kind": "session", "id": session},
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": ":::decision{#claim title=\"A person's choice\" decided_by=\"alice\"}\nWords the person never said.\n:::"}},
			}, "agent-token")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "alice") || !strings.Contains(response.Body.String(), "claim") {
				t.Fatalf("agent attribution: %d %s", response.Code, response.Body.String())
			}
			markdown, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
			if err != nil || markdown != "Context.\n" {
				t.Fatalf("refused claim changed the document: %q (%v)", markdown, err)
			}
		})
	}
}
