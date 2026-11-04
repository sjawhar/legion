package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func seedConversionCommandDocument(t *testing.T, database *store.Store, slug, markdown string, primary bool) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(t.Context(), `insert into artifacts(issue_key,project_key,slug,name,kind,is_primary,created_by) values('DOC-1','DOC',$1,$2,'doc',$3,'{"kind":"user","id":"alice"}'::jsonb) returning id::text`, slug, strings.ReplaceAll(slug, "-md", "")+".md", primary).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	document := crdt.New()
	fragment := document.GetXmlFragment("prosemirror")
	document.Transact(func(tx *crdt.Transaction) { err = pmdoc.Update(tx, fragment, tree) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(t.Context(), `insert into doc_updates(artifact_id,version,update,content_changed) values($1,1,$2,true)`, id, crdt.EncodeStateAsUpdateV1(document, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(t.Context(), `insert into artifact_versions(artifact_id,number,markdown,doc_update_version,authors) values($1,1,$2,1,'[{"kind":"user","id":"alice"}]'::jsonb)`, id, canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(t.Context(), `insert into doc_settlements_pending(artifact_id) values($1) on conflict do nothing`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConvertDecisionsSubcommandDryRunApplyAndRetry(t *testing.T) {
	database := storetest.Open(t)
	for _, statement := range []string{
		`insert into projects(key,name) values('DOC','Documents')`,
		`insert into issues(key,project_key,number,title,created_by,rank) values('DOC-1','DOC',1,'Conversion command','{"kind":"user","id":"alice"}'::jsonb,'U')`,
	} {
		if _, err := database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	spec := seedConversionCommandDocument(t, database, "spec", "The spec.\n", true)
	legacy := seedConversionCommandDocument(t, database, "decisions-md", "## Keep the source\n\nOne original choice.\n", false)
	counts := func() [4]int {
		t.Helper()
		var n [4]int
		if err := database.Pool.QueryRow(t.Context(), `select (select count(*) from artifact_versions),(select count(*) from doc_updates),(select count(*) from doc_settlements_pending),(select count(*) from events)`).Scan(&n[0], &n[1], &n[2], &n[3]); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run := func(args ...string) (int, string) {
		t.Helper()
		var out, errors bytes.Buffer
		code := runSubcommand(context.Background(), append([]string{"convert-decisions"}, args...), envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()}), &out, &errors)
		return code, out.String() + errors.String()
	}
	before := counts()
	if code, out := run(); code != 0 || !strings.Contains(out, "documents=1 moved=1") || !strings.Contains(out, "apply=false") {
		t.Fatalf("dry run: exit=%d\n%s", code, out)
	}
	if after := counts(); after != before {
		t.Fatalf("dry-run process cleanup wrote data: %v -> %v", before, after)
	}
	if code, out := run("--apply"); code != 0 || !strings.Contains(out, "documents=1 moved=1") || !strings.Contains(out, "apply=true") {
		t.Fatalf("apply: exit=%d\n%s", code, out)
	}
	var nodes int
	var name string
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from document_blocks where artifact_id=$1 and type='decision'`, spec).Scan(&nodes); err != nil || nodes != 1 {
		t.Fatalf("actual decision nodes=%d err=%v", nodes, err)
	}
	if err := database.Pool.QueryRow(t.Context(), `select name from artifacts where id=$1`, legacy).Scan(&name); err != nil || !strings.HasPrefix(name, "decision-history-") {
		t.Fatalf("source not retired: %q %v", name, err)
	}
	applied := counts()
	if code, out := run("--apply"); code != 0 || !strings.Contains(out, "documents=0 moved=0") {
		t.Fatalf("retry: exit=%d\n%s", code, out)
	}
	if after := counts(); after != applied {
		t.Fatalf("retry wrote again: %v -> %v", applied, after)
	}
	if code, _ := run("--not-a-flag"); code != 2 {
		t.Fatalf("invalid flag exit=%d, want2", code)
	}
}
