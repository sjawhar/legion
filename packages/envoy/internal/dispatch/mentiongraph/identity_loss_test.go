package mentiongraph

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func TestCensusDetectsDeletedIdentityRowsWhileAnotherMentionRemains(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const comment = "00000000-0000-0000-0000-000000000061"
	const message = "00000000-0000-0000-0000-000000000062"
	const document = "00000000-0000-0000-0000-000000000063"
	const body = "See dispatch://people/alice and dispatch://people/bob"
	if _, err := database.Pool.Exec(ctx, `insert into projects(key,name) values ('CORE','Core');
 insert into issues(key,project_key,number,title,rank,created_by) values ('CORE-1','CORE',1,'Mentions','U','{"kind":"user","id":"carol"}');
 insert into comments(id,issue_key,author,body) values ('00000000-0000-0000-0000-000000000061','CORE-1','{"kind":"user","id":"carol"}','See dispatch://people/alice and dispatch://people/bob');
 insert into messages(id,issue_key,author,body) values ('00000000-0000-0000-0000-000000000062','CORE-1','{"kind":"user","id":"carol"}','See dispatch://people/alice and dispatch://people/bob');
 insert into artifacts(id,issue_key,project_key,slug,name,kind,created_by) values ('00000000-0000-0000-0000-000000000063','CORE-1','CORE','spec','Spec','doc','{"kind":"user","id":"carol"}');`); err != nil {
		t.Fatal(err)
	}
	tree, err := pmdoc.ParseRendering(":::callout{#mention-block}\n" + body + "\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for i, b := range offsets {
		blocks[i] = model.ArtifactBlock{ID: b.ID, Type: b.Type, From: b.From, To: b.To}
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, source := range []struct{ kind, id string }{{"comment", comment}, {"message", message}} {
		if _, err := refs.ReplaceCounted(ctx, tx, source.kind, source.id, body, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `insert into artifact_versions(artifact_id,number,markdown) values ($1,1,$2)`, document, markdown); err != nil {
		t.Fatal(err)
	}
	if _, err := refs.IndexBlocks(ctx, tx, document, 1, time.Now(), markdown, blocks, text.ExtractAt(markdown, ""), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	report, err := Census(ctx, database.Pool, since)
	if err != nil || report.Mismatched != 0 || report.Recorded < 6 {
		t.Fatalf("positive=%+v %v", report, err)
	}
	if _, err := database.Pool.Exec(ctx, `delete from identity_mentions where to_id='alice'`); err != nil {
		t.Fatal(err)
	}
	report, err = Census(ctx, database.Pool, since)
	if err != nil || report.Mismatched < 3 || report.Missing["comment:person"] != 1 || report.Missing["message:person"] != 1 || report.Missing["block:person"] < 1 {
		t.Fatalf("deleted indexed rows=%+v %v", report, err)
	}
	t.Logf("deleted indexed rows: mismatched=%d missing=%v", report.Mismatched, report.Missing)
}
