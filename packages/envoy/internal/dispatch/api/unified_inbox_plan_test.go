package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

func TestUnifiedInboxPlansAndP99OnRealisticVolume(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into people(email) values('alice'),('bob') on conflict(email) do nothing;
 insert into projects(key,name) values('VOL','Volume');
 insert into issues(key,project_key,number,title,created_by,assignee,rank) values
 ('VOL-1','VOL',1,'Volume','{"kind":"user","id":"alice"}','alice','U');
 insert into comments(id,issue_key,author,body,created_at)
 select md5('comment-'||n)::uuid,'VOL-1',jsonb_build_object('kind','user','id',case when n%1000=0 then 'alice' else 'user-'||(n%1000)::text end),'Volume comment',now()-interval '1 hour'
 from generate_series(1,20000)n;
 insert into comments(issue_key,author,body,reply_to)
 select 'VOL-1','{"kind":"session","id":"agent"}','Volume reply',md5('comment-'||n)::uuid from generate_series(1000,20000,1000)n;
 insert into messages(id,issue_key,author,body)
 select md5('message-'||n)::uuid,'VOL-1','{"kind":"session","id":"other"}','Unrelated message' from generate_series(1,20000)n;
 insert into messages(id,author,body,target,created_at)
 select md5('direct-'||n)::uuid,jsonb_build_object('kind','user','id',case when n%1000=0 then 'alice' else 'user-'||(n%1000)::text end),'Direct root','session:agent',now()-interval '1 hour' from generate_series(1,2000)n;
 insert into messages(author,body,target,in_reply_to)
 select '{"kind":"session","id":"agent"}','Direct reply','session:agent',md5('direct-'||n)::uuid from generate_series(1000,2000,1000)n;
 insert into asks(issue_key,author,question) select 'VOL-1','{"kind":"session","id":"agent"}','Volume ask '||n from generate_series(1,20)n;
 insert into events(issue_key,seq,type,actor,notify,payload,published_destinations)
 select 'VOL-1',n,'comment.created','{"kind":"user","id":"sender"}',true,
 jsonb_build_object('id',md5('comment-'||(1+(n-1)%20000))::uuid::text),array['notifications.agent.other'] from generate_series(1,100000)n;
 insert into events(issue_key,seq,type,actor,notify,payload)
 select 'VOL-1',100000+row_number()over(order by id),'ask.opened','{"kind":"session","id":"agent"}',true,jsonb_build_object('id',id::text,'question',question)
 from asks where issue_key='VOL-1';
 analyze comments;analyze messages;analyze events;analyze asks;analyze issues;analyze user_threads;
 `); err != nil {
		t.Fatal(err)
	}
	var comments, messages, eventCount int
	if err := database.Pool.QueryRow(ctx, `select (select count(*) from comments),(select count(*) from messages),(select count(*) from events)`).Scan(&comments, &messages, &eventCount); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	args := append(unreadDirectRepliesArgs("alice", nil), 50, 0)
	if err := database.Pool.QueryRow(ctx, `explain(analyze,buffers,format json) `+unifiedInboxQuery, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan          map[string]any
		PlanningTime  float64 `json:"Planning Time"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	var walk func(map[string]any)
	walk = func(p map[string]any) {
		relation, _ := p["Relation Name"].(string)
		kind, _ := p["Node Type"].(string)
		index, _ := p["Index Name"].(string)
		if relation != "" {
			t.Logf("%s %s %s", kind, relation, index)
		}
		if strings.Contains(kind, "Seq Scan") && (relation == "comments" || relation == "messages" || relation == "events") {
			t.Errorf("new inbox must not sequentially scan %s", relation)
		}
		children, _ := p["Plans"].([]any)
		for _, child := range children {
			walk(child.(map[string]any))
		}
	}
	walk(plans[0].Plan)
	t.Logf("EXPLAIN %d comments / %d messages / %d events: planning %.3f ms execution %.3f ms", comments, messages, eventCount, plans[0].PlanningTime, plans[0].ExecutionTime)
	// Exercise the actual authenticated HTTP reads, not only planner cost. Alternate
	// which implementation runs first so the new path gets no warm-cache advantage.
	legacy := func() {
		for _, path := range []string{"/api/v1/inbox?assignee=me", "/api/v1/me/replies?limit=50"} {
			r := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
			if r.Code != 200 {
				t.Fatalf("legacy %s: %d %s", path, r.Code, r.Body.String())
			}
		}
	}
	unified := func() {
		r := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox?limit=50", nil, "alice")
		if r.Code != 200 {
			t.Fatalf("unified: %d %s", r.Code, r.Body.String())
		}
		var page unifiedInboxPage
		if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 42 {
			t.Fatalf("volume membership=%d want 42", page.Total)
		}
	}
	legacy()
	unified()
	oldTimes, newTimes := make([]time.Duration, 0, 200), make([]time.Duration, 0, 200)
	measure := func(f func(), samples *[]time.Duration) {
		start := time.Now()
		f()
		*samples = append(*samples, time.Since(start))
	}
	for n := range 200 {
		if n%2 == 0 {
			measure(legacy, &oldTimes)
			measure(unified, &newTimes)
		} else {
			measure(unified, &newTimes)
			measure(legacy, &oldTimes)
		}
	}
	slices.Sort(oldTimes)
	slices.Sort(newTimes)
	t.Logf("HTTP interleaved 200 samples: today's combined inbox p99=%s; unified p99=%s; median old=%s new=%s", oldTimes[197], newTimes[197], oldTimes[99], newTimes[99])
	if newTimes[197] > oldTimes[197] {
		t.Errorf("unified p99 %s exceeds today's %s", newTimes[197], oldTimes[197])
	}
	safe, err := events.NewHorizon(database.Pool).Safe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := observeInboxShadow(ctx, database.Pool, safe); err != nil {
		t.Fatal(err)
	}
	report, err := readInboxShadowReport(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if report.CheckedUsers != 2 || report.Mismatched != 0 || report.Errors != 0 {
		t.Fatalf("volume shadow=%+v", report)
	}
	t.Logf("volume shadow: checked_users=%d mismatched=%d errors=%d missing=%v unexpected=%v", report.CheckedUsers, report.Mismatched, report.Errors, report.Missing, report.Unexpected)
}
