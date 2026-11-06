package provenanceaudit_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/provenanceaudit"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

func TestRequirementSourcesClassifiesEachConversationCitation(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
insert into projects (key, name) values ('CORE', 'Core');
insert into issues (key, project_key, number, title, rank, created_by) values
 ('CORE-1', 'CORE', 1, 'Requirements', 'a', '{"kind":"session","id":"agent"}'),
 ('CORE-2', 'CORE', 2, 'No requirements', 'b', '{"kind":"session","id":"agent"}');
insert into artifacts (id, project_key, issue_key, slug, name, kind, is_primary, created_by) values
 ('00000000-0000-0000-0000-000000000001', 'CORE', 'CORE-1', 'spec', 'spec.md', 'doc', true, '{"kind":"session","id":"agent"}'),
 ('00000000-0000-0000-0000-000000000002', 'CORE', 'CORE-2', 'spec', 'spec.md', 'doc', true, '{"kind":"session","id":"agent"}'),
 ('00000000-0000-0000-0000-000000000003', 'CORE', 'CORE-1', 'notes', 'notes.md', 'doc', false, '{"kind":"session","id":"agent"}');
insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
select '00000000-0000-0000-0000-000000000001', id, 'CORE-1/spec#' || id, type, 'sum', '', 2, now()
from (values ('r1', 'requirement'), ('r2', 'requirement'), ('r3', 'requirement'), ('r4', 'requirement'), ('a1', 'ask')) b(id, type);
insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
values ('00000000-0000-0000-0000-000000000002', 'p1', 'CORE-2/spec#p1', 'paragraph', 'sum', '', 1, now());
insert into document_block_texts (artifact_id, block_id, version, text) values
 ('00000000-0000-0000-0000-000000000001', 'r1', 1, 'Old dispatch://CORE-1/comment/00000000-0000-0000-0000-000000000011'),
 ('00000000-0000-0000-0000-000000000001', 'r1', 2, ':::requirement\nChild cites [ask](dispatch://CORE-1/spec#b-a1)\n:::'),
 ('00000000-0000-0000-0000-000000000001', 'r1', 3, 'Future dispatch://CORE-1/message/00000000-0000-0000-0000-000000000011'),
 ('00000000-0000-0000-0000-000000000001', 'r2', 2, 'Comment dispatch://CORE-1/comment/00000000-0000-0000-0000-000000000011'),
 ('00000000-0000-0000-0000-000000000001', 'r3', 2, 'dispatch://CORE-1/artifact/spec#b-a1 dispatch://CORE-1/ask/00000000-0000-0000-0000-000000000022 dispatch://CORE-1/message/00000000-0000-0000-0000-000000000031'),
 ('00000000-0000-0000-0000-000000000001', 'r4', 2, 'Only dispatch://CORE-2 and dispatch://CORE-1/spec#b-r1');
insert into document_block_versions (artifact_id, version) values
 ('00000000-0000-0000-0000-000000000001', 2),
 ('00000000-0000-0000-0000-000000000002', 1)
on conflict (artifact_id) do update set version = excluded.version;
insert into asks (id, issue_key, author, question, state, answer, block_artifact_id, block_id) values
 ('00000000-0000-0000-0000-000000000021', 'CORE-1', '{"kind":"session","id":"agent"}', 'Document?', 'answered', '{}', '00000000-0000-0000-0000-000000000001', 'a1'),
 ('00000000-0000-0000-0000-000000000022', 'CORE-1', '{"kind":"session","id":"agent"}', 'Item?', 'answered', '{}', null, null);
insert into comments (id, issue_key, artifact_id, author, body) values
 ('00000000-0000-0000-0000-000000000011', 'CORE-1', null, '{"kind":"user","id":"alice"}', 'Issue decision'),
 ('00000000-0000-0000-0000-000000000012', null, '00000000-0000-0000-0000-000000000003', '{"kind":"user","id":"alice"}', 'Document decision');
insert into messages (id, issue_key, author, body) values
 ('00000000-0000-0000-0000-000000000031', 'CORE-1', '{"kind":"user","id":"alice"}', 'Message decision');
insert into events (issue_key, seq, type, actor, payload, notify) values
 ('CORE-1', 1, 'ask.answered', '{"kind":"session","id":"agent"}', '{"id":"00000000-0000-0000-0000-000000000021"}', false),
 ('CORE-1', 2, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000021"}', false),
 ('CORE-1', 3, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000022"}', false);
`)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"1", "4", "3", "1", "1", "1", "1", "1", "2", "1", "0.333", "0.800"}
	assertMeasurement(t, database.Pool, want)

	// Neither an agent's participation nor a person's activity on a requirement-free
	// issue belongs in the volume. A former human answer must not survive an agent's
	// later answer; an unanswered ask's old event must not count either.
	_, err = database.Pool.Exec(ctx, `
insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
values ('00000000-0000-0000-0000-000000000003', 'a2', 'CORE-1/notes#a2', 'ask', 'sum', '', 1, now());
update document_block_texts set text = text || ' dispatch://CORE-1/notes#b-a2 dispatch://CORE-1/spec#b-missing'
where block_id = 'r4';
insert into asks (id, issue_key, author, question, state, answer) values
 ('00000000-0000-0000-0000-000000000023', 'CORE-1', '{"kind":"session","id":"agent"}', 'Agent answer?', 'answered', '{}'),
 ('00000000-0000-0000-0000-000000000024', 'CORE-2', '{"kind":"session","id":"agent"}', 'Outside scope?', 'answered', '{}'),
 ('00000000-0000-0000-0000-000000000025', 'CORE-1', '{"kind":"session","id":"agent"}', 'Unanswered?', 'open', null);
insert into events (issue_key, seq, type, actor, payload, notify) values
 ('CORE-1', 4, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000023"}', false),
 ('CORE-1', 5, 'ask.answered', '{"kind":"session","id":"agent"}', '{"id":"00000000-0000-0000-0000-000000000023"}', false),
 ('CORE-1', 6, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000025"}', false),
 ('CORE-2', 1, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000024"}', false);
insert into comments (issue_key, author, body) values
 ('CORE-1', '{"kind":"session","id":"agent"}', 'Not a person'),
 ('CORE-2', '{"kind":"user","id":"alice"}', 'No requirement');
insert into messages (issue_key, author, body) values
 ('CORE-1', '{"kind":"session","id":"agent"}', 'Not a person'),
 ('CORE-2', '{"kind":"user","id":"alice"}', 'No requirement');
`)
	if err != nil {
		t.Fatal(err)
	}
	assertMeasurement(t, database.Pool, want)
}

type beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func assertMeasurement(t *testing.T, database beginner, want []string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	results, err := tx.Conn().PgConn().Exec(ctx, provenanceaudit.RequirementSourcesSQL).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || len(results[0].Rows) != 1 {
		t.Fatalf("want one summary and one sample result, got %#v", results)
	}
	columns := make([]string, len(results[0].FieldDescriptions))
	for i, field := range results[0].FieldDescriptions {
		columns[i] = field.Name
	}
	wantColumns := []string{"scope_issues", "requirement_blocks", "cited_requirement_blocks", "cited_document_ask_only", "cited_other_only", "cited_both", "person_document_ask_answers", "person_item_ask_answers", "person_comments", "person_messages", "item24_share_cited", "item24_share_volume"}
	if !reflect.DeepEqual(columns, wantColumns) {
		t.Fatalf("columns = %v, want %v", columns, wantColumns)
	}
	got := make([]string, len(results[0].Rows[0]))
	for i, value := range results[0].Rows[0] {
		got[i] = string(value)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %v, want %v", got, want)
	}
	if len(results[1].Rows) != 3 {
		t.Fatalf("samples = %d, want 3", len(results[1].Rows))
	}
	wantSamples := map[string][]string{
		"CORE-1/spec#r1": {"document_ask_only", "dispatch://CORE-1/spec#b-a1"},
		"CORE-1/spec#r2": {"other_only", "dispatch://CORE-1/comment/00000000-0000-0000-0000-000000000011"},
		"CORE-1/spec#r3": {"both", "dispatch://CORE-1/artifact/spec#b-a1; dispatch://CORE-1/ask/00000000-0000-0000-0000-000000000022; dispatch://CORE-1/message/00000000-0000-0000-0000-000000000031"},
	}
	for _, row := range results[1].Rows {
		id := string(row[0])
		if got := []string{string(row[1]), string(row[2])}; !reflect.DeepEqual(got, wantSamples[id]) {
			t.Errorf("sample %s = %v, want %v", id, got, wantSamples[id])
		}
		delete(wantSamples, id)
	}
	if len(wantSamples) != 0 {
		t.Errorf("missing samples: %v", wantSamples)
	}
}
