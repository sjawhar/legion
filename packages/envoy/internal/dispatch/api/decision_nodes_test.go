package api

import (
  "bytes"
  "context"
  "github.com/sjawhar/envoy/internal/dispatch/docs"
  "github.com/sjawhar/envoy/internal/dispatch/model"
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

func TestDecisionShadowEqualityAndIssueScope(t *testing.T) {
 fixture:=newDecisionsFixture(t)
 created:=dispatchRequest(t,fixture.handler,http.MethodPost,"/api/v1/issues",map[string]any{"project":"CORE","title":"Unrelated choice","spec":":::decision{#unrelated title=\"Other issue only\"}\nUnrelated choice.\n:::\n","force":true},"alice")
 if created.Code!=http.StatusCreated{t.Fatalf("unrelated issue: %d %s",created.Code,created.Body.String())}
 other:=decodeBody[model.Issue](t,created)
 response:=dispatchRequest(t,fixture.handler,http.MethodGet,"/api/v1/issues/"+fixture.issue.Key+"/decisions-shadow",nil,"alice")
 if response.Code!=http.StatusOK{t.Fatalf("shadow: %d %s",response.Code,response.Body.String())}
 var census struct{ Source int `json:"source_count"`;Nodes int `json:"node_count"`;Differences int `json:"differences"` }
 if err:=json.Unmarshal(response.Body.Bytes(),&census);err!=nil{t.Fatal(err)}
 if census.Source!=4||census.Nodes!=4||census.Differences!=0{t.Fatalf("census: %s",response.Body.String())}
 t.Logf("decision shadow equality: source=%d nodes=%d differences=%d",census.Source,census.Nodes,census.Differences)
 response=dispatchRequest(t,fixture.handler,http.MethodGet,"/api/v1/issues/"+fixture.issue.Key+"/decisions?artifact="+other.PrimaryArtifactID+"&version=1",nil,"alice")
 if response.Code!=http.StatusNotFound{t.Fatalf("cross-issue document: %d %s",response.Code,response.Body.String())}
 anonymous:=dispatchRequest(t,fixture.handler,http.MethodGet,"/api/v1/issues/"+fixture.issue.Key+"/decisions-shadow",nil,"")
 if anonymous.Code!=http.StatusUnauthorized{t.Fatalf("anonymous census: %d",anonymous.Code)}
}

func TestDecisionConversionDryRunPreservesHistoryAndIsIdempotent(t *testing.T) {
 handler,database,deps:=newTestServer(t,testServerOptions{settle:time.Hour})
 createReferenceAPIProject(t,handler,"CORE")
 issue:=createSpecIssue(t,handler,"CORE","The spec.\n")
 legacy:=uploadIssueDocument(t,handler,issue.Key,"decisions.md","## Keep the table\n\nChoose a table.\n")
 service:=deps.Docs.(*docs.Service)
 var out bytes.Buffer
 before:=specVersionCount(t,database,issue.PrimaryArtifactID)
 dry,err:=service.ConvertDecisionDocuments(context.Background(),false,&out)
 if err!=nil||dry.Moved!=1||dry.Documents!=1{t.Fatalf("dry run: %#v %v\n%s",dry,err,out.String())}
 if got:=specVersionCount(t,database,issue.PrimaryArtifactID);got!=before{t.Fatalf("dry run wrote a spec version: %d -> %d",before,got)}
 if got,err:=deps.Docs.Text(context.Background(),legacy.ID);err!=nil||!strings.Contains(got,"Choose a table."){t.Fatalf("dry run modified source: %q %v",got,err)}
 applied,err:=service.ConvertDecisionDocuments(context.Background(),true,&out)
 if err!=nil||applied.Moved!=1{t.Fatalf("apply: %#v %v\n%s",applied,err,out.String())}
 var name string;var versions int
 if err:=database.Pool.QueryRow(t.Context(),`select name,(select count(*) from artifact_versions where artifact_id=a.id) from artifacts a where id=$1`,legacy.ID).Scan(&name,&versions);err!=nil{t.Fatal(err)}
 if name=="decisions.md"||versions!=2{t.Fatalf("history not retired intact: name=%q versions=%d",name,versions)}
 var original string
 if err:=database.Pool.QueryRow(t.Context(),`select markdown from artifact_versions where artifact_id=$1 and number=1`,legacy.ID).Scan(&original);err!=nil||!strings.Contains(original,"Choose a table."){t.Fatalf("original version: %q %v",original,err)}
 after:=specVersionCount(t,database,issue.PrimaryArtifactID)
 again,err:=service.ConvertDecisionDocuments(context.Background(),true,&out)
 if err!=nil||again.Documents!=0||again.Moved!=0||specVersionCount(t,database,issue.PrimaryArtifactID)!=after{t.Fatalf("retry wrote twice: %#v %v\n%s",again,err,out.String())}
}

func TestDecisionConversionRestoresTheOriginalAnsweredAskRow(t *testing.T) {
 handler,database,deps:=newTestServer(t,testServerOptions{settle:20*time.Millisecond})
 createReferenceAPIProject(t,handler,"CORE")
 issue:=createSpecIssue(t,handler,"CORE","Context.\n\n:::ask{#answer-here}\nWhich name?\n:::\n")
 view,_:=waitForOpenQuestions(t,handler,issue.Key,1)
 ask:=view.OpenQuestions[0].Ask
 answered:=dispatchRequest(t,handler,http.MethodPost,"/api/v1/asks/"+ask.ID+"/answer",map[string]any{"selected":[]string{},"text":"My original words.","expected_edited_at":ask.EditedAt},"alice")
 if answered.Code!=http.StatusOK{t.Fatalf("answer: %d %s",answered.Code,answered.Body.String())}
 editSpec(t,handler,issue.Key,[]map[string]any{{"op":"delete","block":"answer-here"}})
 var out bytes.Buffer
 report,err:=deps.Docs.(*docs.Service).ConvertDecisionDocuments(context.Background(),true,&out)
 if err!=nil||report.Restored!=1{t.Fatalf("restore: %#v %v\n%s",report,err,out.String())}
 decision:=decisionNamed(t,readIssueDecisions(t,handler,issue.Key),"Which name?")
 if len(decision.Asks)!=1||decision.Asks[0]!=ask.ID||!strings.Contains(decision.Text,"My original words."){t.Fatalf("restored a copy, not the original: %#v",decision)}
 var count int
 if err:=database.Pool.QueryRow(t.Context(),`select count(*) from asks where issue_key=$1`,issue.Key).Scan(&count);err!=nil||count!=1{t.Fatalf("ask rows=%d err=%v",count,err)}
}
