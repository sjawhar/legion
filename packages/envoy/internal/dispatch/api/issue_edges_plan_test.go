package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func TestIssueEdgePlansOnIssueAndCitationVolume(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('VOL','Volume');
 insert into issues (key,project_key,number,title,created_by,rank,parent_key)
 select 'VOL-'||n,'VOL',n,'Topic '||n,'{"kind":"user","id":"alice"}',n::text,case when n>1 then 'VOL-1' end
 from generate_series(1,20000) n;
 insert into artifacts (id,issue_key,project_key,slug,name,kind,is_primary,created_by)
 select md5(n::text)::uuid,'VOL-'||n,'VOL','spec','spec.md','doc',true,'{"kind":"user","id":"alice"}' from generate_series(1,20000) n;
 insert into refs (from_kind,from_id,kind,to_kind,to_id)
 select 'artifact',md5(n::text)::uuid::text,'mentions','issue','VOL-'||(20000-x) from generate_series(1,20000) n cross join generate_series(1,5) x;
	update projects set next_number=20001 where key='VOL';
	update issues set title='Zymurgy ledger' where key='VOL-3';
 analyze issues; analyze artifacts; analyze refs;
 `); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass,true)`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"legacy-point", `select * from graph_edges where from_kind='issue' and from_id=$1`, []any{"VOL-2"}},
		{"named-links", `select * from ` + refs.AllEdgesSQL + ` g where from_kind='issue' and from_id=$1`, []any{"VOL-2"}},
		{"neighbourhood", relatedGraphSQL, []any{"VOL", []string{"issue"}, []string{"VOL-19999"}, "VOL-2"}},
		{"shadow", issueLinksShadowSQL, []any{"VOL-2"}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var raw []byte
			if err := database.Pool.QueryRow(ctx, `explain (analyze,buffers,format json) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct {
				Execution float64 `json:"Execution Time"`
				Planning  float64 `json:"Planning Time"`
				Plan      map[string]any
			}
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan) != 1 {
				t.Fatalf("plan=%s", raw)
			}
			t.Logf("20000 issues, 20000 specs, 100000 citations: planning=%.3fms execution=%.3fms rows=%v", plan[0].Planning, plan[0].Execution, plan[0].Plan["Actual Rows"])
			if query.name == "neighbourhood" && plan[0].Plan["Actual Rows"] != float64(3) {
				t.Fatalf("missing nonempty volume result: %s", raw)
			}
		})
	}
	var after string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass,true)`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != definition {
		t.Fatal("named graph reads changed the legacy view")
	}
	refreshTermStats(t, database, "VOL")
	const title = "Zymurgy ledger reconciliation"
	const text = "See dispatch://VOL-19999"
	started := time.Now()
	answer := directServer(deps).relatedCheck(t.Context(), relatedInput{
		project: "VOL", title: title, text: title + "\n" + text,
		source: suggestionSource{kind: "issue", issueKey: "VOL-2"},
	})
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("complete check at volume took %s, budget 2s", took)
	} else {
		t.Logf("complete related check at volume: %s", took)
	}
	if answer.Outcome == "not_made" || answer.GraphMissing != "" || len(answer.GraphRelated) != 3 {
		t.Fatalf("incomplete check at volume: %+v", answer)
	}
	started = time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "VOL", "title": title, "spec": text, "force": true,
	}, "alice")
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("issue creation at volume took %s, budget 2s", took)
	} else {
		t.Logf("complete issue creation at volume: %s", took)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("create at volume: %d %s", response.Code, response.Body.String())
	}
	created := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response)
	if got := created.Advice.Suggestions; got.Outcome == "" || got.Outcome == "not_made" || got.GraphMissing != "" || len(got.GraphRelated) != 3 {
		t.Fatalf("incomplete create check at volume: %+v", got)
	}
}
