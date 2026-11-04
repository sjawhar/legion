package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A missing delivered edge, another person's row, or a mark/Clear keyed with the
// wrong identity must change these literal sets.
func TestUnifiedInboxKeepsLegacyMembershipAndOwnScope(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('CORE','Core');
 insert into issues (key,project_key,number,title,created_by,assignee,rank) values
 ('CORE-1','CORE',1,'Alice work','{"kind":"user","id":"alice"}','alice','U'),
 ('CORE-2','CORE',2,'Bob work','{"kind":"user","id":"bob"}','bob','V');
 insert into asks (id,issue_key,author,question) values
 ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"session","id":"agent"}','Alice question'),
 ('00000000-0000-0000-0000-000000000002','CORE-2','{"kind":"session","id":"agent"}','Bob question');
 insert into comments (id,issue_key,author,body,created_at) values
 ('00000000-0000-0000-0000-000000000010','CORE-1','{"kind":"user","id":"alice"}','Root',now()-interval '10 minutes');
 insert into comments (id,issue_key,author,body,reply_to,created_at) values
 ('00000000-0000-0000-0000-000000000011','CORE-1','{"kind":"session","id":"agent"}','Thread reply','00000000-0000-0000-0000-000000000010',now()-interval '9 minutes');
 insert into messages (id,author,body,target,created_at) values
 ('00000000-0000-0000-0000-000000000020','{"kind":"user","id":"ALICE"}','Direct root','session:agent',now()-interval '10 minutes');
 insert into messages (id,author,body,in_reply_to,target,created_at) values
 ('00000000-0000-0000-0000-000000000021','{"kind":"session","id":"agent"}','Direct reply','00000000-0000-0000-0000-000000000020','session:agent',now()-interval '8 minutes'),
 ('00000000-0000-0000-0000-000000000022','{"kind":"session","id":"agent"}','Already read reply','00000000-0000-0000-0000-000000000020','session:agent',now()-interval '7 minutes');
 insert into user_agent_reply_read (login,session_id,reply_id) values ('alice','agent','00000000-0000-0000-0000-000000000022');
 `)
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		ID    string `json:"id"`
		Cause string `json:"cause"`
	}
	type page struct {
		Rows   []item `json:"rows"`
		Total  int    `json:"total"`
		Person string `json:"person"`
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "ALICE")
	if response.Code != http.StatusOK {
		t.Fatalf("inbox: %d %s", response.Code, response.Body.String())
	}
	got := decodeBody[page](t, response)
	if got.Person != "alice" || got.Total != 3 || len(got.Rows) != 3 {
		t.Fatalf("page=%+v", got)
	}
	want := map[string]string{"00000000-0000-0000-0000-000000000001": "ask", "00000000-0000-0000-0000-000000000011": "thread_reply", "00000000-0000-0000-0000-000000000021": "direct_reply"}
	for _, row := range got.Rows {
		if want[row.ID] != row.Cause {
			t.Fatalf("unexpected row=%+v", row)
		}
		delete(want, row.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing=%v", want)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "bob")
	bob := decodeBody[page](t, response)
	if response.Code != 200 || bob.Total != 1 || bob.Rows[0].ID != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("bob=%+v code=%d", bob, response.Code)
	}
	for _, path := range []string{"/api/v1/me/inbox?person=bob", "/api/v1/me/inbox?login=bob", "/api/v1/me/inbox?assignee=bob"} {
		if response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"); response.Code != http.StatusBadRequest {
			t.Fatalf("override accepted: %s %d", path, response.Code)
		}
	}
	for _, path := range []string{"/api/v1/me/inbox", "/api/v1/me/inbox/shadow"} {
		if response := sessionRequest(t, handler, http.MethodGet, path, nil); response.Code != http.StatusForbidden {
			t.Fatalf("bearer %s: %d", path, response.Code)
		}
		anonymous := httptest.NewRecorder()
		handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, path, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous=%d", anonymous.Code)
		}
	}
	// Clear remains keyed on the raw actor id, unlike the canonical read rows.
	if _, err := database.Pool.Exec(ctx, `insert into user_agent_state (login,session_id,cleared_before) values ('ALICE','agent',now())`); err != nil {
		t.Fatal(err)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "ALICE")
	cleared := decodeBody[page](t, response)
	if cleared.Total != 2 {
		t.Fatalf("Clear did not subtract direct reply: %+v", cleared)
	}
	if _, err := database.Pool.Exec(ctx, `update user_threads set read_through = now() where login='alice'`); err != nil {
		t.Fatal(err)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "ALICE")
	read := decodeBody[page](t, response)
	if read.Total != 1 || read.Rows[0].Cause != "ask" {
		t.Fatalf("read marks=%+v", read)
	}
}
