package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/mentiongraph"
)

func TestMentionPlansAndCensusOnRealisticRecordedVolume(t *testing.T) {
	_, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `insert into projects(key,name) values ('VOL','Volume');
 insert into issues(key,project_key,number,title,created_by,rank) values ('VOL-1','VOL',1,'Mentions','{"kind":"user","id":"alice"}','U');
 insert into comments(id,issue_key,author,body) select md5(n::text)::uuid,'VOL-1','{"kind":"user","id":"bob"}','Volume mention' from generate_series(1,20000) n;
 insert into comments(issue_key,author,body) values ('VOL-1','{"kind":"session","id":"reader","owner":"alice"}','Verified session');
 insert into comment_mentions(comment_id,target,delivery,resolved_session_id) select md5(n::text)::uuid,'session:reader','steer','reader' from generate_series(1,10000) n;
 insert into refs(from_kind,from_id,to_kind,to_id) select 'comment',md5(n::text)::uuid::text,'person_mention','alice' from generate_series(10001,20000) n;
 insert into events(issue_key,seq,type,actor,notify,payload) select 'VOL-1',n,'issue.updated','{"kind":"session","id":"volume"}',false,'{}' from generate_series(1,100000) n;
 analyze comments;analyze comment_mentions;analyze refs;analyze events;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"existing point", `select * from graph_edges where from_kind='comment' and from_id=$1`, []any{"c4ca4238-a0b9-2382-0dcc-509a6f75849b"}},
		{"person including mentions", personNodesQuery, []any{"alice", 50, 0}},
	} {
		var raw []byte
		if err := database.Pool.QueryRow(ctx, `explain(analyze,buffers,format json) `+query.sql, query.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan []struct {
			Plan          map[string]any
			ExecutionTime float64 `json:"Execution Time"`
			PlanningTime  float64 `json:"Planning Time"`
		}
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: 20001 comments / 20000 recorded mentions / 100000 events, planning %.3f ms execution %.3f ms", query.name, plan[0].PlanningTime, plan[0].ExecutionTime)
		logMentionScans(t, plan[0].Plan)
		if query.name == "existing point" && strings.Contains(string(raw), `"Relation Name": "events"`) {
			t.Fatal("existing point read reached events")
		}
	}
	items, err := readInboxMentions(ctx, mentionExplainQuery{queryer: database.Pool, t: t}, "alice", 50, 0)
	if err != nil || len(items) != 50 {
		t.Fatalf("bounded inbox mentions = %d, %v", len(items), err)
	}
	report, err := mentiongraph.Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil || report.Checked != 20001 || report.Recorded != 30000 || report.Mismatched != 0 {
		t.Fatalf("volume census = %+v %v", report, err)
	}
	t.Logf("volume census: checked=%d recorded=%d mismatched=%d", report.Checked, report.Recorded, report.Mismatched)
}

type mentionExplainQuery struct {
	queryer
	t *testing.T
}

func (q mentionExplainQuery) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.t.Helper()
	var raw []byte
	if err := q.QueryRow(ctx, "explain(analyze,buffers,format json) "+sql, args...).Scan(&raw); err != nil {
		return nil, err
	}
	var plan []struct {
		Plan          map[string]any
		ExecutionTime float64 `json:"Execution Time"`
		PlanningTime  float64 `json:"Planning Time"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, err
	}
	q.t.Logf("bounded unified mention expansion: planning %.3f ms execution %.3f ms", plan[0].PlanningTime, plan[0].ExecutionTime)
	logMentionScans(q.t, plan[0].Plan)
	return q.queryer.Query(ctx, sql, args...)
}

func logMentionScans(t *testing.T, node map[string]any) {
	t.Helper()
	if relation, ok := node["Relation Name"]; ok {
		t.Logf("%v %v index=%v rows=%v loops=%v time=%vms hits=%v reads=%v", node["Node Type"], relation, node["Index Name"], node["Actual Rows"], node["Actual Loops"], node["Actual Total Time"], node["Shared Hit Blocks"], node["Shared Read Blocks"])
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		logMentionScans(t, child.(map[string]any))
	}
}
