package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/persongraph"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestRecordedDeliveryCensusCommandIsReadOnlyAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	url := database.Pool.Config().ConnString()
	var out, errOut bytes.Buffer
	if code := runCensus(ctx, []string{"--deliveries"}, url, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no comments, messages or asks") {
		t.Fatalf("empty census: code=%d out=%s err=%s", code, &out, &errOut)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, author, body, target) values
		('00000000-0000-0000-0000-000000000001', '{"kind":"user","id":"alice"}', 'Direct', 'session:reader');
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ('00000000-0000-0000-0000-000000000001', 1, 'aside', 'reader', 'sent');
	`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--deliveries"}, url, &out, &errOut); code != 0 {
		t.Fatalf("equal census: code=%d out=%s err=%s", code, &out, &errOut)
	}
	var report persongraph.DeliveryCensus
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Checked != 1 || report.Mismatched != 0 || errOut.Len() != 0 {
		t.Fatalf("equal census report=%+v err=%s", report, &errOut)
	}
	if _, err := database.Pool.Exec(ctx, `alter view graph_person_edges rename to fixture_graph_person_edges; create view graph_person_edges as select * from fixture_graph_person_edges where kind <> 'delivered_to'`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--deliveries"}, url, &out, &errOut); code != 1 {
		t.Fatalf("mismatch census: code=%d out=%s err=%s", code, &out, &errOut)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mismatched != 1 || report.Missing["message_delivery"] != 1 {
		t.Fatalf("mismatch report = %+v", report)
	}
	if _, err := database.Pool.Exec(ctx, `drop view graph_person_edges`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--deliveries"}, url, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "graph_person_edges") {
		t.Fatalf("missing schema: code=%d out=%s err=%s", code, &out, &errOut)
	}
	var exists bool
	if err := database.Pool.QueryRow(ctx, `select to_regclass('graph_person_edges') is not null`).Scan(&exists); err != nil || exists {
		t.Fatalf("census must not migrate: graph_person_edges exists=%v err=%v", exists, err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--unknown"}, url, &out, &errOut); code != 2 {
		t.Fatalf("invalid flag: code=%d", code)
	}
}
