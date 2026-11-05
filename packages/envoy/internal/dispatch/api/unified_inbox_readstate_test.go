package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestUnifiedInboxPreservesRawClearKeyAndCanonicalReadMarks(t *testing.T) {
	handler, database, _, reply, _ := directConversationFrom(t, "Alice")
	message := decodeBody[model.Message](t, reply("On it."))
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into user_agent_state(login,session_id,cleared_before) values('Alice','s1',$1)`, message.CreatedAt); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		login string
		want  int
	}{{"Alice", 0}, {"alice", 1}} {
		page, err := readUnifiedInbox(ctx, database.Pool, test.login, 50, 0)
		if err != nil || page.Total != test.want {
			t.Fatalf("raw Clear key %q: total=%d want=%d err=%v", test.login, page.Total, test.want, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `insert into user_agent_read(login,session_id,read_through) values('alice','s1',$1)`, message.CreatedAt); err != nil {
		t.Fatal(err)
	}
	page, err := readUnifiedInbox(ctx, database.Pool, "ALICE", 50, 0)
	if err != nil || page.Total != 0 {
		t.Fatalf("canonical read mark: total=%d err=%v", page.Total, err)
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "Alice"); response.Code != 200 {
		t.Fatalf("canonical header actor: %d %s", response.Code, response.Body.String())
	}
}

func TestUnifiedInboxDirectRootUsesPersonLoginNormalization(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	message := decodeBody[model.Message](t, reply("On it."))
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update messages set author=jsonb_build_object('kind','user','id',E'  ALICE\t') where id=$1`, root.ID); err != nil {
		t.Fatal(err)
	}
	page, err := readUnifiedInbox(ctx, database.Pool, "alice", 50, 0)
	if err != nil || page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].ID != message.ID {
		t.Fatalf("normalized graph membership: %+v err=%v", page, err)
	}
	want, err := readLegacyInboxMembership(ctx, database.Pool, "alice")
	if err != nil || len(want) != 1 {
		t.Fatalf("normalized route membership: %v err=%v", want, err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "alice")
	if response.Code != 200 || decodeBody[unifiedInboxPage](t, response).Total != 1 {
		t.Fatalf("normalized inbox route: %d %s", response.Code, response.Body.String())
	}
}
