package api

import (
	"context"
	"net/http"
	"testing"
)

func TestUnifiedInboxShadowCountsDroppedAskAndNeverErasesBadDay(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, "alice", "bob")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('CORE','Core');
 insert into issues (key,project_key,number,title,created_by,assignee,rank) values
 ('CORE-1','CORE',1,'Graph','{"kind":"user","id":"alice"}','alice','U');
 insert into asks (id,issue_key,author,question) values
 ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"session","id":"agent"}','A delivered ask');
 `); err != nil {
		t.Fatal(err)
	}
	type report struct {
		Mismatched      int            `json:"mismatched"`
		CheckedUsers    int            `json:"checked_users"`
		ActiveUsers     int            `json:"active_users"`
		UncoveredUsers  int            `json:"uncovered_users"`
		Missing         map[string]int `json:"missing"`
		Ready           bool           `json:"ready"`
		DailyMismatched int            `json:"daily_mismatched"`
	}
	read := func() report {
		t.Helper()
		if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
			t.Fatal(err)
		}
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox/shadow", nil, "alice")
		if response.Code != 200 {
			t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
		}
		return decodeBody[report](t, response)
	}
	clean := read()
	if clean.Mismatched != 0 || clean.CheckedUsers != 2 || clean.ActiveUsers != 2 || clean.UncoveredUsers != 0 || clean.Ready {
		t.Fatalf("clean=%+v", clean)
	}
	if _, err := database.Pool.Exec(ctx, `alter view graph_inbox_edges rename to fixture_graph_inbox_edges;
 create view graph_inbox_edges as select * from fixture_graph_inbox_edges where from_id <> '00000000-0000-0000-0000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	broken := read()
	if broken.Mismatched != 1 || broken.Missing["ask"] != 1 || broken.DailyMismatched < 1 || broken.Ready {
		t.Fatalf("negative control=%+v", broken)
	}
	if _, err := database.Pool.Exec(ctx, `create or replace view graph_inbox_edges as select * from fixture_graph_inbox_edges`); err != nil {
		t.Fatal(err)
	}
	repaired := read()
	if repaired.Mismatched != 0 || repaired.DailyMismatched < 1 || repaired.Ready {
		t.Fatalf("bad day erased=%+v", repaired)
	}
	t.Logf("seeded shadow: active=%d checked=%d uncovered=%d mismatched=%d; dropped-edge control: missing.ask=%d mismatched=%d; repaired: mismatched=%d daily_mismatched=%d", clean.ActiveUsers, clean.CheckedUsers, clean.UncoveredUsers, clean.Mismatched, broken.Missing["ask"], broken.Mismatched, repaired.Mismatched, repaired.DailyMismatched)
}
