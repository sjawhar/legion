package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestEditingABlockDoesNotMoveAnOldMentionUpThePersonPage(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "AGE")
	first := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	later := first.Add(time.Hour)
	if _, err := database.Pool.Exec(context.Background(), `insert into artifacts(id,project_key,slug,name,kind,created_by) values ('00000000-0000-0000-0000-000000000071','AGE','spec','Spec','doc','{"kind":"user","id":"bob"}');
 insert into document_blocks(artifact_id,block_id,node_id,type,text_sha256,excerpt,changed_version,changed_at) values
 ('00000000-0000-0000-0000-000000000071','old','AGE/artifact/spec#old','paragraph',repeat('a',64),'Old mention',2,now()),
 ('00000000-0000-0000-0000-000000000071','new','AGE/artifact/spec#new','paragraph',repeat('b',64),'Later mention',1,now()-interval '1 day');`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into identity_mentions(owner_kind,owner_id,from_kind,from_id,to_kind,to_id,created_at) values
 ('artifact','00000000-0000-0000-0000-000000000071','block','AGE/artifact/spec#old','person','alice',$1),
 ('artifact','00000000-0000-0000-0000-000000000071','block','AGE/artifact/spec#new','person','alice',$2)`, first, later); err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("person page=%d %s", response.Code, response.Body.String())
	}
	page := decodeBody[personNodes](t, response)
	if len(page.Rows) != 2 || page.Rows[0].ID != "AGE/artifact/spec#new" || page.Rows[1].ID != "AGE/artifact/spec#old" || !page.Rows[0].CreatedAt.Equal(later) || !page.Rows[1].CreatedAt.Equal(first) {
		t.Fatalf("old mention jumped after its block edit: %+v", page.Rows)
	}
}
