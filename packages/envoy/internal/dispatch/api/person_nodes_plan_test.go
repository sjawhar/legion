package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/persongraph"
)

func TestPersonGraphPlansOnRecordedDeliveryVolume(t *testing.T) {
	_, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('VOL', 'Volume');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('VOL-1', 'VOL', 1, 'Volume', '{"kind":"user","id":"alice"}', 'U');
		insert into comments (id, issue_key, author, body)
		select md5(n::text)::uuid, 'VOL-1', jsonb_build_object('kind', 'user', 'id', case when n % 1000 = 0 then 'alice' else 'user-' || (n % 1000)::text end), 'Volume comment'
		from generate_series(1, 20000) n;
		insert into comments (issue_key, author, body)
		values ('VOL-1', '{"kind":"session","id":"s-owned","owner":"alice"}', 'An owned session');
		insert into events (issue_key, seq, type, actor, notify, payload, published_destinations)
		select 'VOL-1', n, 'comment.created', '{"kind":"user","id":"sender"}', true,
		       jsonb_build_object('id', md5((1 + (n - 1) % 20000)::text)::uuid::text),
		       array[case when n % 1000 = 1 then 'notifications.agent.s-owned' else 'notifications.agent.other' end]
		from generate_series(1, 100000) n;
		analyze comments; analyze events;
	`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"person", personNodesQuery, []any{"alice", 50, 0}},
		{"existing-point-read", `select * from graph_edges where from_kind = 'comment' and from_id = $1`, []any{"c4ca4238-a0b9-2382-0dcc-509a6f75849b"}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var raw []byte
			if err := database.Pool.QueryRow(ctx, `explain (analyze, buffers, format json) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct {
				Plan          map[string]any
				ExecutionTime float64 `json:"Execution Time"`
				PlanningTime  float64 `json:"Planning Time"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 {
				t.Fatalf("unexpected EXPLAIN: %s", raw)
			}
			t.Logf("20001 comments, 100000 recorded events: planning %.3f ms, execution %.3f ms", plans[0].PlanningTime, plans[0].ExecutionTime)
			var walk func(map[string]any)
			walk = func(plan map[string]any) {
				kind, _ := plan["Node Type"].(string)
				relation, _ := plan["Relation Name"].(string)
				index, _ := plan["Index Name"].(string)
				if relation != "" {
					t.Logf("%s %s %s", kind, relation, index)
				}
				if query.name == "existing-point-read" && strings.Contains(kind, "Seq Scan") && (relation == "events" || relation == "comments" || relation == "messages") {
					t.Errorf("point graph read must not scan %s", relation)
				}
				children, _ := plan["Plans"].([]any)
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
			walk(plans[0].Plan)
			if query.name == "person" && plans[0].Plan["Actual Rows"] != float64(40) {
				t.Fatalf("person plan returned %v rows, want 40 authored/received nodes", plans[0].Plan["Actual Rows"])
			}
		})
	}
	var appendedRows, busyRows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_edges where from_kind = 'comment' and from_id = 'c4ca4238-a0b9-2382-0dcc-509a6f75849b' and to_kind <> 'person'`).Scan(&appendedRows); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_edges where to_kind = 'session' and to_id = 'other'`).Scan(&busyRows); err != nil {
		t.Fatal(err)
	}
	t.Logf("5390d0e9 and dc656255 unchanged graph read: %d outgoing session rows for one comment; old view's busiest session has %d incoming delivery rows", appendedRows, busyRows)
	if appendedRows != 0 || busyRows != 0 {
		t.Fatal("identity or delivery escaped into an old reader's view")
	}
	previous, err := os.ReadFile("../store/migrations/20261101183525_graph_edges_ask_parts_and_block_pointers.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, string(previous)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := database.Pool.QueryRow(ctx, `explain (analyze, buffers, format json) select * from graph_edges where from_kind = 'comment' and from_id = 'c4ca4238-a0b9-2382-0dcc-509a6f75849b'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var baseline []struct {
		ExecutionTime float64 `json:"Execution Time"`
		PlanningTime  float64 `json:"Planning Time"`
	}
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	var mainRows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_edges where from_kind = 'comment' and from_id = 'c4ca4238-a0b9-2382-0dcc-509a6f75849b' and to_kind <> 'person'`).Scan(&mainRows); err != nil {
		t.Fatal(err)
	}
	t.Logf("main baseline on identical data: planning %.3f ms, execution %.3f ms, outgoing rows %d", baseline[0].PlanningTime, baseline[0].ExecutionTime, mainRows)
	report, err := persongraph.Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 20001 || report.Mismatched != 0 {
		t.Fatalf("volume census = %+v", report)
	}
	t.Logf("volume census: checked=%d by_kind=%v mismatched=%d missing=%v unexpected=%v", report.Checked, report.ByKind, report.Mismatched, report.Missing, report.Unexpected)
}
