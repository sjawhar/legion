package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The named queries see 50,000 unrelated blocks and pointers, 50,000 project events and 1,000
// events on this issue. The old point read is measured both before and after the new write.
func TestInterpretationQueriesAtVolume(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#choice}\nUse a table.\n:::\n")
	_, err := database.Pool.Exec(t.Context(), `
 insert into artifacts (project_key,slug,name,kind,created_by)
 select 'CORE','load-'||i,'load-'||i||'.md','doc','{"kind":"session","id":"seed"}'::jsonb from generate_series(1,100) i;
 insert into document_blocks (artifact_id,block_id,node_id,type,text_sha256,excerpt,changed_version,changed_at)
 select a.id,'load-'||i,a.ref_key||'#load-'||i,'paragraph',repeat('a',64),'Unrelated block.',1,now()
 from artifacts a cross join generate_series(1,500) i where a.slug like 'load-%';
 insert into block_refs (artifact_id,block_id,from_node,to_kind,to_id,kind,created_version)
 select artifact_id,block_id,node_id,'issue','CORE-1','mentions',1 from document_blocks where block_id like 'load-%';
 insert into events (project_key,seq,type,actor,payload,notify)
 select 'CORE',1000+i,'artifact.version','{"kind":"session","id":"seed"}'::jsonb,'{}'::jsonb,false from generate_series(1,50000) i;
 insert into events (issue_key,seq,type,actor,payload,notify)
 select 'CORE-1',1000+i,'artifact.version','{"kind":"session","id":"seed"}'::jsonb,'{}'::jsonb,false from generate_series(1,1000) i;
 analyze artifacts; analyze document_blocks; analyze events; analyze block_refs;
 `)
	if err != nil {
		t.Fatal(err)
	}
	var blocks, pointers, events int
	if err := database.Pool.QueryRow(t.Context(), `select (select count(*) from document_blocks where block_id like 'load-%'),(select count(*) from block_refs where block_id like 'load-%'),(select count(*) from events where seq>=1000)`).Scan(&blocks, &pointers, &events); err != nil {
		t.Fatal(err)
	}
	if blocks != 50000 || pointers != 50000 || events != 51000 {
		t.Fatalf("seed volume: blocks=%d pointers=%d events=%d", blocks, pointers, events)
	}
	t.Logf("seed volume: blocks=%d pointers=%d events=%d", blocks, pointers, events)
	const point = `select from_kind,from_id,kind,to_kind,to_id from graph_edges where from_kind='block' and from_id=$1`
	explainInterpretationQuery(t, database, "baseline existing point", point, issue.Key+"/spec#choice")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/interpretations", map[string]any{"decision": "choice", "body": "The measured reading.", "actor": model.Actor{Kind: "session", ID: "measuring-agent"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("interpretation: %d %s", response.Code, response.Body.String())
	}
	for _, query := range []struct{ name, sql, arg string }{
		{"new interpretation graph", interpretationEdgesQuery, issue.Key},
		{"new trusted authors", interpretationAuthorsQuery, issue.Key},
		{"head existing point", point, issue.Key + "/spec#choice"},
	} {
		explainInterpretationQuery(t, database, query.name, query.sql, query.arg)
	}
	if items := readInterpretations(t, handler, issue.Key); len(items) != 1 || items[0].Author == nil || items[0].Author.ID != "measuring-agent" {
		t.Fatalf("volume changed projection: %#v", items)
	}
}

func explainInterpretationQuery(t *testing.T, database *store.Store, name, query, arg string) {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(t.Context(), "explain (analyze,buffers,format json) "+query, arg).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Planning  float64 `json:"Planning Time"`
		Execution float64 `json:"Execution Time"`
		Plan      struct {
			Kind string `json:"Node Type"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("EXPLAIN: %s %v", raw, err)
	}
	t.Logf("%s: planning=%.3f ms execution=%.3f ms root=%s", name, plans[0].Planning, plans[0].Execution, plans[0].Plan.Kind)
}
