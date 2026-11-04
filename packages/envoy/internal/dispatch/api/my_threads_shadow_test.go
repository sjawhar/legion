package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestParticipationShadowChecksEachCauseAndDetectsProjectionCorruption(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PART", "Participation", "Mixed sources")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 update issues set assignee='alice' where key=$1;
 insert into comments(id,issue_key,author,body) values
 ('30000000-0000-0000-0000-000000000001',$1,'{"kind":"user","id":"alice"}','Authored root'),
 ('30000000-0000-0000-0000-000000000002',$1,'{"kind":"session","id":"alice-session","owner":"alice"}','A verified session'),
 ('30000000-0000-0000-0000-000000000003',$1,'{"kind":"user","id":"bob"}','Mentioned-only root'),
 ('30000000-0000-0000-0000-000000000004',$1,'{"kind":"user","id":"bob"}','Addressed-only root');
 insert into comment_mentions(comment_id,target,delivery,resolved_session_id) values
 ('30000000-0000-0000-0000-000000000003','session:alice-session','aside','alice-session');
 insert into events(issue_key,seq,type,actor,payload,notify,published_destinations) values
 ($1,200,'comment.created','{"kind":"user","id":"bob"}','{"id":"30000000-0000-0000-0000-000000000004"}',true,array['notifications.agent.alice-session']);
 insert into asks(id,issue_key,author,question,answer,state) values
 ('30000000-0000-0000-0000-000000000005',$1,'{"kind":"session","id":"agent"}','Assigned and answered ask',jsonb_build_object('user','alice','at',now()::text,'selected','[]'::jsonb),'answered');
 insert into messages(id,author,body,target) values
 ('30000000-0000-0000-0000-000000000006','{"kind":"user","id":"alice"}','Alice direct thread','session:alice-direct-agent'),
 ('30000000-0000-0000-0000-000000000007','{"kind":"user","id":"bob"}','Bob unrelated direct thread','session:another-session');
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	read := func() participationShadow {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads/shadow", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
		}
		return decodeBody[participationShadow](t, response)
	}
	report := read()
	for cause, count := range report.ByCause {
		if count.Expected == 0 || count.Actual != count.Expected || count.Missing != 0 || count.Unexpected != 0 {
			t.Fatalf("empty or unequal %s: %+v", cause, count)
		}
	}
	if report.Mismatched != 0 || report.ByCause["mentioned"].Expanded != 1 || report.ByCause["addressed"].Expanded < 1 || report.AuthoredDirect != 1 {
		t.Fatalf("census %+v", report)
	}
	t.Logf("seeded shadow: %+v; authored=%+v mentioned=%+v addressed=%+v", report, *report.ByCause["authored"], *report.ByCause["mentioned"], *report.ByCause["addressed"])
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("threads: %d %s", response.Code, response.Body.String())
	}
	page := decodeBody[myThreadsPage](t, response)
	if page.Total != 5 {
		t.Fatalf("got %d threads: %s", page.Total, response.Body.String())
	}
	for _, row := range page.Rows {
		if strings.Contains(row.Subject, "Bob unrelated") {
			t.Fatal("another person's direct conversation leaked")
		}
	}

	// Keep the source records and graph edges untouched; corrupt only the final
	// participation relation. All three independent comparisons must notice it.
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_thread_participation'::regclass,true)`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	definition = strings.TrimSuffix(strings.TrimSpace(definition), ";")
	if _, err := database.Pool.Exec(ctx, `create or replace view graph_thread_participation as select * from (`+definition+`) source where cause='authored' and thread_id<>'30000000-0000-0000-0000-000000000001'::uuid`); err != nil {
		t.Fatal(err)
	}
	broken := read()
	for cause, count := range broken.ByCause {
		if count.Missing == 0 {
			t.Fatalf("negative control missed %s: %+v", cause, broken)
		}
	}
	if broken.Mismatched == 0 {
		t.Fatal("negative control accepted corrupted participation")
	}
	t.Logf("negative control: mismatched=%d authored=%+v mentioned=%+v addressed=%+v", broken.Mismatched, *broken.ByCause["authored"], *broken.ByCause["mentioned"], *broken.ByCause["addressed"])
}
