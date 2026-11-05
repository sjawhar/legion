package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMaskTextOrdinals(t *testing.T) {
	o := ordinals{}
	s := maskText("a 0bd2f0d8-1c2e-4c47-9a39-0c1a2b3c4d5e b 2026-10-26T22:55:00.123Z c 0BD2F0D8-1C2E-4C47-9A39-0C1A2B3C4D5E", o)
	if s != "a <id1> b <ts> c <id1>" {
		t.Fatalf("masked %q", s)
	}
}

func TestMD5FieldsKeepsBoundaries(t *testing.T) {
	a, b := md5.New(), md5.New()
	md5Fields(a, "ab", "c")
	md5Fields(b, "a", "bc")
	if string(a.Sum(nil)) == string(b.Sum(nil)) {
		t.Fatal("moving a field boundary must change the digest")
	}
}

func TestDumpAnchorOrdinals(t *testing.T) {
	docOrd := map[string]int{"doc-uuid": 2}
	scopes := map[string]ordinals{}
	scope := func(k string) ordinals {
		if scopes[k] == nil {
			scopes[k] = ordinals{}
		}
		return scopes[k]
	}
	first := dumpAnchorOf([]byte(`{"artifact_id":"doc-uuid","block_id":"B1","mark_id":"m-uuid-1","version":4,"quote":"q","orphaned":false}`), docOrd, scope)
	second := dumpAnchorOf([]byte(`{"artifact_id":"doc-uuid","block_id":"b1","mark_id":"m-uuid-2","version":5,"quote":"q","orphaned":true}`), docOrd, scope)
	if first.Doc != 2 || first.Block != second.Block || first.Mark == second.Mark || second.Version != 5 || !second.Orphaned {
		t.Fatalf("anchors %+v %+v", *first, *second)
	}
	if dumpAnchorOf(nil, docOrd, scope) != nil {
		t.Fatal("no anchor, no dump")
	}
}

// --- a live Postgres -------------------------------------------------------------------------

// replayShadowSchemaDDL is an empty copy, in the schema named after the database's login, of the
// replay's own two tables. Its columns are the ones the seed writes and the check reads, so a
// statement that resolved here would answer nothing rather than fail - which is what the
// deployment met of the loop seed's map (dispatch://INFRA-988).
const replayShadowSchemaDDL = `
create table %[1]s.loadtest_replay_ids (
  kind text not null, logged_id text not null, local_id text not null,
  primary key (kind, logged_id));
create table %[1]s.loadtest_replay_anchors (
  kind text not null, logged_id text not null, local_id text not null, logged_doc text not null,
  branch text not null, block_id text not null, span_start int not null, span_end int not null,
  quote_at int not null default -1,
  primary key (kind, logged_id))`

// TestReplaySeedReadsPublicUnderAShadowingUserSchema: the case the local stack cannot produce by
// itself - its login is postgres, which names no schema - built here on purpose, as
// TestLoopSeedReadsPublicUnderAShadowingUserSchema builds it for the loop seed's map. The
// database's own login is given a schema of its name holding an empty table of the name of each
// table the replay owns; Postgres's default search_path is "$user", public, so from that session
// every unqualified name resolves to the empty copy. A seed whose DDL made such a schema is what
// made a deployment serve a second, empty copy of Dispatch's whole schema
// (dispatch://INFRA-988), so the DDL must create no schema at all and every statement of the
// seed, the run and the check must name public.
func TestReplaySeedReadsPublicUnderAShadowingUserSchema(t *testing.T) {
	pool, dsn := loopTestDBAt(t)
	ctx := context.Background()
	var login, database string
	if err := pool.QueryRow(ctx, `select current_user, current_database()`).Scan(&login, &database); err != nil {
		t.Fatal(err)
	}
	user := pgx.Identifier{login}.Sanitize()
	if _, err := pool.Exec(ctx, "create schema "+user); err != nil {
		t.Fatalf("create the schema named as the login (%s): %v", login, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(replayShadowSchemaDDL, user)); err != nil {
		t.Fatalf("create the empty copies in %s: %v", user, err)
	}
	// The default search_path, set on the database so that every session opened after this one
	// resolves "$user" before public, whatever the test server was started with.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`alter database %s set search_path = "$user", public`,
		pgx.Identifier{database}.Sanitize())); err != nil {
		t.Fatalf("set the database's search_path: %v", err)
	}
	shadowed, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer shadowed.Close()
	var path string
	if err := shadowed.QueryRow(ctx, "show search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, `"$user"`) {
		t.Fatalf("the shadowed session's search_path is %q, want one that resolves \"$user\" first", path)
	}

	// The seed's own DDL, over that session: it creates no schema, so it can shadow none.
	if _, err := shadowed.Exec(ctx, replayIDsDDL); err != nil {
		t.Fatalf("create the replay's tables under the shadow: %v", err)
	}
	var schemas int
	if err := shadowed.QueryRow(ctx, `select count(*) from pg_namespace where nspname = 'loadtest'`).Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 0 {
		t.Fatalf("replayIDsDDL created %d schema named loadtest; the seed must create none", schemas)
	}

	// The seed writes its id map and its anchors over the same session: an empty map first, as a
	// resumed seed reads it, then one row per created object and one per anchor it placed.
	ids, err := loadIDStore(ctx, shadowed)
	if err != nil {
		t.Fatalf("read the id map under the shadow: %v", err)
	}
	if ids.len() != 0 {
		t.Fatalf("the id map of a fresh database holds %d rows", ids.len())
	}
	for _, m := range [][3]string{{idArtifact, "d1", "local-d1"}, {idComment, "c1", "local-c1"}, {idAsk, "a1", "local-a1"}} {
		if err := ids.put(ctx, m[0], m[1], m[2]); err != nil {
			t.Fatalf("record %s %s under the shadow: %v", m[0], m[1], err)
		}
	}
	s := &replaySeeder{pool: shadowed, rules: map[string]int{}}
	d := &issueDoc{a: &replayArtifact{ID: "d1", Kind: "doc"}, local: "local-d1"}
	occurrence := 1
	if err := s.recordAnchor(ctx, idComment, "c1", "local-c1", d,
		anchorChoice{Branch: ruleInBlock, BlockID: "b1", Start: 11, End: 42}); err != nil {
		t.Fatalf("record the comment's anchor under the shadow: %v", err)
	}
	if err := s.recordAnchor(ctx, idAsk, "a1", "local-a1", d,
		anchorChoice{Branch: ruleLoggedQuote, BlockID: "b2", Start: 50, End: 90, Occurrence: &occurrence, QuoteAt: 61}); err != nil {
		t.Fatalf("record the ask's anchor under the shadow: %v", err)
	}

	// The shadow is in force: an unqualified name reads the empty copies, and the seed's own rows
	// are in public. Without this the test would pass against unqualified SQL too.
	var shadowIDs, publicIDs, shadowAnchors, publicAnchors int
	if err := shadowed.QueryRow(ctx, `select (select count(*) from loadtest_replay_ids), (select count(*) from public.loadtest_replay_ids),
		(select count(*) from loadtest_replay_anchors), (select count(*) from public.loadtest_replay_anchors)`).
		Scan(&shadowIDs, &publicIDs, &shadowAnchors, &publicAnchors); err != nil {
		t.Fatal(err)
	}
	if shadowIDs != 0 || shadowAnchors != 0 {
		t.Fatalf("the %s schema holds %d id-map rows and %d anchors; the seed wrote into the shadow",
			login, shadowIDs, shadowAnchors)
	}
	if publicIDs != 3 || publicAnchors != 2 {
		t.Fatalf("public holds %d id-map rows and %d anchors, want the seed's 3 and 2", publicIDs, publicAnchors)
	}

	// What the run and the check read is the seed's own rows, not the empty copies.
	got, err := readReplayIDs(ctx, shadowed)
	if err != nil {
		t.Fatalf("read the id map under the shadow: %v", err)
	}
	want := map[[2]string]string{
		{idArtifact, "d1"}: "local-d1", {idComment, "c1"}: "local-c1", {idAsk, "a1"}: "local-a1",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("under a schema named as the login the id map reads %v, want the seed's %v", got, want)
	}
	anchors, err := readReplayAnchors(ctx, shadowed)
	if err != nil {
		t.Fatalf("read the anchors under the shadow: %v", err)
	}
	wantAnchors := map[[2]string]anchorRow{
		{idComment, "c1"}: {ruleInBlock, 11, 42, -1},
		{idAsk, "a1"}:     {ruleLoggedQuote, 50, 90, 61},
	}
	if !maps.Equal(anchors, wantAnchors) {
		t.Fatalf("under a schema named as the login the anchors read %v, want the seed's %v", anchors, wantAnchors)
	}

	// And a resumed seed reads the map it wrote, so it skips what is already there.
	resumed, err := loadIDStore(ctx, shadowed)
	if err != nil {
		t.Fatalf("resume under the shadow: %v", err)
	}
	if local, ok := resumed.get(idComment, "c1"); !ok || local != "local-c1" {
		t.Fatalf("a resumed seed reads comment c1 as %q (%v), want local-c1", local, ok)
	}
}
