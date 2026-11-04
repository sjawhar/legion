package api

import (
 "encoding/json"
 "net/http"
 "strings"
 "testing"
 "time"
)

func TestDecisionsJSONShape(t *testing.T) {
 handler, _, _ := newTestServer(t, testServerOptions{})
 createReferenceAPIProject(t, handler, "CORE")
 issue := createSpecIssue(t, handler, "CORE", "No decisions yet.\n")
 response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/decisions", nil, "alice")
 if response.Code != http.StatusOK { t.Fatalf("decisions: %d %s", response.Code, response.Body.String()) }
 var shape map[string]json.RawMessage
 if err := json.Unmarshal(response.Body.Bytes(), &shape); err != nil { t.Fatal(err) }
 if len(shape) != 3 || string(shape["document"]) != "null" || string(shape["open_questions"]) != "[]" || string(shape["decisions"]) != "[]" { t.Fatalf("wire shape: %s", response.Body.String()) }
}

func TestDecisionNodesReadTheAskItselfAndIgnoreUntypedHeadings(t *testing.T) {
 handler, _, _ := newTestServer(t, testServerOptions{settle: 20*time.Millisecond})
 createReferenceAPIProject(t, handler, "CORE")
 issue := createSpecIssue(t, handler, "CORE", "## Not a decision\n\n:::ask{#name}\nWhich name?\n:::\n")
 view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
 ask := view.OpenQuestions[0].Ask
 const answer = "The name I gave.\n\nKeep my words exactly."
 response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}, "text": answer, "expected_edited_at": ask.EditedAt}, "alice")
 if response.Code != http.StatusOK { t.Fatalf("answer: %d %s", response.Code, response.Body.String()) }
 view = readIssueDecisions(t, handler, issue.Key)
 if len(view.Decisions) != 1 { t.Fatalf("decisions: %s", mustJSON(t, view)) }
 decision := view.Decisions[0]
 if decision.Kind != "person" || decision.Heading != "Which name?" || !strings.Contains(decision.Text, answer) || decision.WaitingReview || len(decision.Asks) != 1 || decision.Asks[0] != ask.ID { t.Fatalf("ask decision: %s", mustJSON(t, decision)) }
}

func TestTypedDecisionNodesOwnTheirShapedEdgesAndKeep(t *testing.T) {
 handler, database, _ := newTestServer(t, testServerOptions{})
 createReferenceAPIProject(t, handler, "CORE")
 issue := createSpecIssue(t, handler, "CORE", shipIt("Ship it.")+"\n")
 reference := "dispatch://"+issue.Key+"/spec"
 editSpec(t, handler, issue.Key, []map[string]any{{"op":"insert", "after":"end", "markdown":":::decision{#store title=\"Use a table\" shaped=\""+reference+"#b-ship%5Fit\" decided_by=\"alice\"}\nChoose the table.\n:::"}})
 view := readIssueDecisions(t, handler, issue.Key)
 decision := decisionNamed(t, view, "Use a table")
 if decision.Kind != "agent" || !decision.WaitingReview || len(decision.Links) != 1 || decision.Links[0].To.BlockID != "ship_it" { t.Fatalf("node: %s", mustJSON(t, decision)) }
 var shaped, person int
 err := database.Pool.QueryRow(t.Context(), `select count(*) filter (where kind='shaped' and to_kind='block'), count(*) filter (where kind='decided_by' and to_kind='person' and to_id='alice') from graph_edges where from_kind='block' and from_id=$1`, issue.Key+"/spec#store").Scan(&shaped,&person)
 if err != nil || shaped != 1 || person != 1 { t.Fatalf("typed relations: shaped=%d person=%d err=%v",shaped,person,err) }
 keepBlock(t, handler, issue.PrimaryArtifactID, decision.BlockID, decision.TextSHA256)
 editSpec(t, handler, issue.Key, []map[string]any{{"op":"replace", "find":"Choose the table.", "with":"Choose the table after measuring."}})
 response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/store/keep", map[string]any{"text_sha256": decision.TextSHA256}, "alice")
 if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "DECISION_CHANGED") { t.Fatalf("stale Keep: %d %s",response.Code,response.Body.String()) }
}
