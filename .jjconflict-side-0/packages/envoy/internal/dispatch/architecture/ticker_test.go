package architecture

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// One import pass imports every enabled source, two of one project included, and a source whose
// import fails - here the second of CORE's, before OPS's - records its failure and leaves the
// sources after it to import.
func TestAnImportPassSyncsEverySourceAndGoesOnPastAFailure(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), repos: map[string]*fakeRepo{
		"acme/broken": {commit: commitAcmeBroken, branchMissing: true},
		"legion/ops":  {commit: commitOps, files: map[string]string{"ops.md": "---\ntitle: Operations\n---\nRunbooks.\n"}},
	}}
	importer, database := newImporterFixture(t, fake)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('OPS', 'Operations');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'acme/broken', 'main', 11, '{"kind":"user","id":"alice"}'),
			('OPS', 'legion/ops', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}

	syncAll(ctx, importer)

	rows, err := database.Pool.Query(ctx, `
		select concat_ws(' ', project_key, repo, coalesce(last_commit, '-'), case when last_error is null then 'ok' else 'failed' end,
		                 (select count(*) from components c where c.project_key = s.project_key and c.source_id = s.id))
		from architecture_sources s order by project_key, id
	`)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	want := []string{
		"CORE legion/arch " + commitOne + " ok 3",
		"CORE acme/broken - failed 0",
		"OPS legion/ops " + commitOps + " ok 1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources after one pass = %#v\nwant %#v", got, want)
	}
}
