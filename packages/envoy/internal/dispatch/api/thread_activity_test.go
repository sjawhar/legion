package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestThreadActivityKeepsHistoricalAnswerWriteAfterRestoration(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ACT", "Activity", "Historical answers")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into asks(id,issue_key,author,question,created_at,answer,state) values
 ('50000000-0000-0000-0000-000000000001',$1,'{"kind":"session","id":"bot"}','Question','2026-01-01T00:00:00Z','{"user":"alice","at":"2026-01-02T00:00:00Z","selected":[]}','answered');
 insert into events(issue_key,seq,type,actor,notify,payload,created_at) values
 ($1,200,'ask.answered','{"kind":"user","id":"bob"}',true,'{"id":"50000000-0000-0000-0000-000000000001","answer":{"user":"bob","at":"2026-01-03T00:00:00Z","selected":[]}}','2026-01-03T00:00:00Z');
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := database.Pool.QueryRow(ctx, `select dispatch_thread_activity('ask','50000000-0000-0000-0000-000000000001')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("activity=%s want latest recorded answer %s, not restored answer time", got, want)
	}
}
