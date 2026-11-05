package mentiongraph

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestRecentDirectMessageDeliveryIsNotAMention(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into messages(id,author,body,target,created_at) values ('00000000-0000-0000-0000-000000000010','{"kind":"user","id":"alice"}','Old message retried today','session:reader',now()-interval '31 days');insert into message_deliveries(message_id,attempt,delivery,session_id,state) values ('00000000-0000-0000-0000-000000000010',1,'aside','reader','sent');`); err != nil {
		t.Fatal(err)
	}
	report, err := Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil || report.Checked != 1 || report.Recorded != 0 || report.Mismatched != 0 {
		t.Fatalf("recent resolution census = %+v %v", report, err)
	}
}
