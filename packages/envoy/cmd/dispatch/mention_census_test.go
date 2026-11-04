package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMentionCensusCommandRefusesEmptyAndDetectsCorruptionWithoutMigrating(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	url := database.Pool.Config().ConnString()
	var out, errOut bytes.Buffer
	if code := runCensus(ctx, []string{"--mentions"}, url, &out, &errOut); code != 1 {
		t.Fatalf("empty = %d %s %s", code, &out, &errOut)
	}
	if _, err := database.Pool.Exec(ctx, `insert into messages(author,body,target) values ('{"kind":"user","id":"alice"}','Mention','session:reader')`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--mentions"}, url, &out, &errOut); code != 0 || !strings.Contains(out.String(), `"mismatched":0`) {
		t.Fatalf("equal = %d %s %s", code, &out, &errOut)
	}
	if _, err := database.Pool.Exec(ctx, `alter view graph_mention_edges rename to fixture_mentions;create view graph_mention_edges as select * from fixture_mentions where false`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--mentions"}, url, &out, &errOut); code != 1 || !strings.Contains(out.String(), `"mismatched":1`) {
		t.Fatalf("corruption = %d %s %s", code, &out, &errOut)
	}
	if _, err := database.Pool.Exec(ctx, `drop view graph_mention_edges`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runCensus(ctx, []string{"--mentions"}, url, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "graph_mention_edges") {
		t.Fatalf("schema = %d %s %s", code, &out, &errOut)
	}
}
