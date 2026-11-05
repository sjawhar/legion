package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestMyThreadsPlansKeepExistingReadAndUserThreadsIndex(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into projects(key,name) values('TVOL','Thread volume');
 insert into issues(key,project_key,number,title,created_by,rank) values('TVOL-1','TVOL',1,'Volume','{"kind":"user","id":"sender"}','U');
 insert into comments(id,issue_key,author,body,created_at)
 select md5(n::text)::uuid,'TVOL-1',jsonb_build_object('kind','user','id',case when n%1000=0 then 'alice' when n%10=0 then 'busy' else 'user-'||(n%1000)::text end),'Thread '||n::text,now()-interval '1 hour'
 from generate_series(1,20000) n;
 insert into comments(id,issue_key,author,body,reply_to)
 select md5(('reply-'||n::text))::uuid,'TVOL-1','{"kind":"session","id":"bot","owner":"alice"}','A newer reply',md5(n::text)::uuid
 from generate_series(1,20000) n;
 insert into comment_mentions(comment_id,target,delivery,resolved_session_id)
 select md5(n::text)::uuid,'session:bot','aside','bot' from generate_series(1,20000) n where n%1000=1;
 insert into events(issue_key,seq,type,actor,notify,payload,published_destinations)
 select 'TVOL-1',n,'comment.created','{"kind":"user","id":"sender"}',true,jsonb_build_object('id',md5((1+(n-1)%20000)::text)::uuid::text),
        array[case when (1+(n-1)%20000)%1000=2 then 'notifications.agent.bot' else 'notifications.agent.other' end]
 from generate_series(1,100000) n;
 analyze comments;analyze comment_mentions;analyze events;analyze user_threads;
 `)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"my-threads", myThreadsQuery, []any{"alice", 50, 0}},
		{"legacy-replies", myRepliesPageQuery, []any{"alice", "", nil, 50, 0}},
		{"existing-graph-point", `select * from graph_edges where from_kind='comment' and from_id=$1`, []any{"c4ca4238-a0b9-2382-0dcc-509a6f75849b"}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var raw []byte
			if err := database.Pool.QueryRow(ctx, `explain(analyze,buffers,format json) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct {
				Plan      map[string]any
				Execution float64         `json:"Execution Time"`
				Planning  float64         `json:"Planning Time"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			t.Logf("40000 comments, 20000 indexed threads, 100000 events: planning %.3fms execution %.3fms", plans[0].Planning, plans[0].Execution)
			hasIndex := false
			var walk func(map[string]any)
			walk = func(p map[string]any) {
				node, _ := p["Node Type"].(string)
				relation, _ := p["Relation Name"].(string)
				index, _ := p["Index Name"].(string)
				if relation != "" {
					t.Logf("%s relation=%s index=%s", node, relation, index)
				}
				if strings.HasPrefix(index, "user_threads") {
					hasIndex = true
				}
				if query.name == "existing-graph-point" && strings.Contains(node, "Seq Scan") && (relation == "comments" || relation == "messages" || relation == "events") {
					t.Errorf("existing point read scans %s", relation)
				}
				if relation == "user_threads" && strings.Contains(node, "Seq Scan") {
					t.Errorf("named read lost the per-person user_threads index")
				}
				children, _ := p["Plans"].([]any)
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
			walk(plans[0].Plan)
			if query.name != "existing-graph-point" && !hasIndex {
				t.Fatal("user_threads index absent from actual plan")
			}
		})
	}
	var total int
	var rows []byte
	if err := database.Pool.QueryRow(ctx, myThreadsQuery, "alice", 50, 0).Scan(&total, &rows); err != nil {
		t.Fatal(err)
	}
	if total != 60 {
		t.Fatalf("volume participation total=%d rows=%s", total, rows)
	}
	t.Logf("volume My threads: total=%d", total)
	// This is the release gate, independent of EXPLAIN's plan shape or JIT choice.
	// Time the authenticated route including query planning, execution and decoding;
	// cover both a typical caller and one participating in 1,980 candidate threads.
	const latencyBudget = 500 * time.Millisecond
	for _, person := range []struct {
		login string
		total int
	}{{"alice", 60}, {"busy", 1980}} {
		t.Run("http-p99-"+person.login, func(t *testing.T) {
			times := make([]time.Duration, 0, 100)
			for range 100 {
				start := time.Now()
				response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads?limit=50", nil, person.login)
				if response.Code != http.StatusOK {
					t.Fatalf("%s latency read: %d %s", person.login, response.Code, response.Body.String())
				}
				page := decodeBody[myThreadsPage](t, response)
				elapsed := time.Since(start)
				if page.Total != person.total || len(page.Rows) != 50 {
					t.Fatalf("%s volume membership: total=%d rows=%d", person.login, page.Total, len(page.Rows))
				}
				times = append(times, elapsed)
			}
			slices.Sort(times)
			p99 := times[98]
			t.Logf("HTTP %s: 100 samples, %d candidate threads, p99=%s, budget=%s", person.login, person.total, p99, latencyBudget)
			if p99 > latencyBudget {
				t.Errorf("My threads %s p99 %s exceeds release budget %s", person.login, p99, latencyBudget)
			}
		})
	}
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	shadow, err := readParticipationShadow(ctx, tx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Mismatched != 0 {
		t.Fatalf("volume shadow: %+v", shadow)
	}
	for cause, want := range map[string]int{"authored": 20, "mentioned": 20, "addressed": 40} {
		count := shadow.ByCause[cause]
		if count.Expected != want || count.Actual != want {
			t.Fatalf("volume %s: %+v want %d", cause, count, want)
		}
		t.Logf("volume shadow %s: %+v", cause, *count)
	}
}
