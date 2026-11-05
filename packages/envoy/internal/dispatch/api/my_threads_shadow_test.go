package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestParticipationIncludesAskQuestionsCurrentPartsAndHistoricalAnswers(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "HIST", "History", "Participation sources")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 update issues set assignee=null where key=$1;
 insert into asks(id,issue_key,author,question,answer,state) values
 ('60000000-0000-0000-0000-000000000001',$1,'{"kind":"session","id":"bot"}','Question mention',null,'open'),
 ('60000000-0000-0000-0000-000000000002',$1,'{"kind":"session","id":"bot"}','Option mention',null,'open'),
 ('60000000-0000-0000-0000-000000000003',$1,'{"kind":"session","id":"bot"}','Removed option',null,'open'),
 ('60000000-0000-0000-0000-000000000004',$1,'{"kind":"session","id":"bot"}','Historical answer','{"user":"bob","at":"2026-01-02T00:00:00Z","selected":[]}','answered');
 insert into refs(from_kind,from_id,to_kind,to_id,kind) values
 ('ask','60000000-0000-0000-0000-000000000001','person_mention','alice','mentions');
 insert into ask_blocks(ask_id,block_id,node_id,position,type,text_sha256,excerpt,changed_revision,changed_at,removed_at) values
 ('60000000-0000-0000-0000-000000000002','b1','ask/60000000-0000-0000-0000-000000000002#b1',0,'option','sha','Mention',1,now(),null),
 ('60000000-0000-0000-0000-000000000003','b1','ask/60000000-0000-0000-0000-000000000003#b1',null,'option','sha','Removed',1,now(),now());
 insert into ask_block_refs(ask_id,block_id,from_node,to_kind,to_id,kind) values
 ('60000000-0000-0000-0000-000000000002','b1','ask/60000000-0000-0000-0000-000000000002#b1','person_mention','alice','mentions'),
 ('60000000-0000-0000-0000-000000000003','b1','ask/60000000-0000-0000-0000-000000000003#b1','person_mention','alice','mentions');
 insert into events(issue_key,seq,type,actor,notify,payload) values
 ($1,200,'ask.answered','{"kind":"user","id":"alice"}',true,'{"id":"60000000-0000-0000-0000-000000000004","answer":{"user":"alice","at":"2026-01-01T00:00:00Z","selected":[]}}');
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("threads: %d %s", response.Code, response.Body.String())
	}
	page := decodeBody[myThreadsPage](t, response)
	if page.Total != 3 {
		t.Fatalf("question, current option, and historical answer must participate: %s", response.Body.String())
	}
	for _, row := range page.Rows {
		if row.Subject == "Removed option" {
			t.Fatal("a removed ask part still grants participation")
		}
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads/shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[participationShadow](t, response)
	if report.Mismatched != 0 || report.ByCause["mentioned"].Expected != 2 || report.ByCause["authored"].Expected != 1 {
		t.Fatalf("independent ask/historical-answer comparison: %+v", report)
	}
}

func TestParticipationShadowEndsALockedSnapshot(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	locked, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.Background())
	if _, err := locked.Exec(ctx, `lock table refs in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/me/threads/shadow", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(response, request)
	}()
	select {
	case <-finished:
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("blocked shadow must fail, not report zero mismatches: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(7 * time.Second):
		cancel()
		<-finished
		t.Fatal("shadow held a locked snapshot beyond its five-second deadline")
	}
}
