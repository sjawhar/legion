package architecture

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// writeStatement is a statement that writes rows.
var writeStatement = regexp.MustCompile(`(?is)^\s*(insert|update|delete)\b`)

// transactionStatement frames a transaction instead of writing anything itself. Every write the
// importer makes runs in a transaction of its own (Importer.inTransaction), so the freshness stamp
// carries one begin and one commit; what a frame carries is recorded too, so a write other than the
// stamp is still caught, and counting the frames still catches a second transaction (a row lock,
// say) the unchanged-head pass has no business opening.
var transactionStatement = regexp.MustCompile(`(?is)^\s*(begin|commit|rollback)\b`)

// touchStatement is the unchanged-head short-circuit's one write (Importer.touch).
var touchStatement = regexp.MustCompile(`(?is)^\s*update\s+architecture_sources\s+set\s+last_sync_at\s*=\s*now\(\),\s*last_commit\s*=\s*\$5\b`)

// The import timer stays, because GitHub has no feed of a source's branch head, so a pass over
// heads that have not moved must stay cheap: it fetches no tree and no blob, appends no event, and
// writes each source's freshness stamp once (touch) and nothing else - here over three sources of
// two projects, read through a pool of its own whose every statement is recorded.
func TestAnImportPassOverUnchangedHeadsFetchesNothingAndTouchesEachSourceOnce(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), repos: map[string]*fakeRepo{
		"acme/infra": {commit: commitInfraOne, files: map[string]string{"infra.md": "---\ntitle: Infrastructure\n---\nClusters.\n"}},
		"legion/ops": {commit: commitOps, files: map[string]string{"ops.md": "---\ntitle: Operations\n---\nRunbooks.\n"}},
	}}
	importer, database := newImporterFixture(t, fake)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('OPS', 'Operations');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'acme/infra', 'main', 11, '{"kind":"user","id":"alice"}'),
			('OPS', 'legion/ops', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	syncAll(ctx, importer)
	var imported int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_sources where last_commit is not null and last_error is null`).Scan(&imported); err != nil || imported != 3 {
		t.Fatalf("%d of 3 sources imported by the first pass (err %v); the pass under test needs every source current", imported, err)
	}
	trees, blobs := fake.reads()
	eventsBefore := projectEvents(t, database)
	var lastSync time.Time
	if err := database.Pool.QueryRow(ctx, `select max(last_sync_at) from architecture_sources`).Scan(&lastSync); err != nil {
		t.Fatalf("read last_sync_at: %v", err)
	}

	recorder := &storetest.StatementRecorder{}
	syncAll(ctx, NewImporter(&store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}, importer.github, events.NewBroker()))

	if gotTrees, gotBlobs := fake.reads(); gotTrees != trees || gotBlobs != blobs {
		t.Fatalf("the pass over unchanged heads read %d trees and %d blobs; want none", gotTrees-trees, gotBlobs-blobs)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, eventsBefore) {
		t.Fatalf("the pass over unchanged heads appended events: %v -> %v", eventsBefore, got)
	}
	var fresh int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_sources where last_sync_at > $1`, lastSync).Scan(&fresh); err != nil || fresh != 3 {
		t.Fatalf("%d of 3 sources carry a freshness stamp from the pass over unchanged heads (err %v); want every one", fresh, err)
	}
	touches, framing := 0, 0
	for _, sql := range recorder.Statements() {
		switch {
		case touchStatement.MatchString(sql):
			touches++
		case transactionStatement.MatchString(sql):
			framing++
		case writeStatement.MatchString(sql):
			t.Errorf("the pass over unchanged heads ran a write other than the freshness stamp: %s", strings.TrimSpace(sql))
		}
	}
	if touches != 3 {
		t.Fatalf("the pass over unchanged heads stamped freshness %d times over 3 sources; want one each", touches)
	}
	if framing != 2*touches {
		t.Fatalf("the pass over unchanged heads ran %d transaction statements around %d freshness stamps; want each stamp's own begin and commit and no other transaction", framing, touches)
	}
}
