package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// loopCheckFound is the whole seed found: a loopFound that matches a plan exactly, which each
// verdict test then moves one number of.
func loopCheckFound(p *loopPlan) loopFound {
	return loopFound{
		Issues:          len(p.Proposals),
		CommentRows:     p.Totals.CommentRows,
		ThreadBytes:     p.Totals.ThreadBytes,
		Parts:           p.Totals.Parts,
		ReviewPages:     p.Totals.ReviewPages,
		ReviewPageBytes: p.Totals.ReviewPageBytes,
		ReviewIndexes:   p.Totals.ReviewIndexes,
	}
}

func loopCheckNamed(t *testing.T, lines []loopCheckLine, name string) loopCheckLine {
	t.Helper()
	for _, l := range lines {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no check named %q", name)
	return loopCheckLine{}
}

// TestLoopSeedCheckExpectedFromPlan: every expected number is the plan's own, and over the
// embedded source file's plan they are the numbers acceptance criterion A2 of
// dispatch://INFRA-988 fixes. Nothing here is typed twice: the check reads the plan.
func TestLoopSeedCheckExpectedFromPlan(t *testing.T) {
	p := loopPlanned(t)
	lines := loopCheckLines(p, loopFound{})
	want := []struct {
		name      string
		expected  int64
		tolerance float64
	}{
		{"proposal issues", 891, 0},
		{"comment rows", 464_602, 0.01},
		{"thread text bytes", 564_113_205, 0.01},
		{"review pages", 6_057, 0},
		{"review-page bytes", 60_291_378, 0.01},
		{"review indexes", 536, 0},
		{"part documents", 1_370, 0},
		{"documents past a cap", 0, 0},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d checks, want %d", len(lines), len(want))
	}
	for i, c := range want {
		if lines[i].Name != c.name {
			t.Errorf("check %d is %q, want %q", i, lines[i].Name, c.name)
		}
		if lines[i].Expected != c.expected {
			t.Errorf("%s expects %d, want the plan's %d", c.name, lines[i].Expected, c.expected)
		}
		if lines[i].Tolerance != c.tolerance {
			t.Errorf("%s is judged at %v, want %v", c.name, lines[i].Tolerance, c.tolerance)
		}
	}
	// A plan of other counts expects those: the numbers are read, not held.
	small := loopTestPlanOf(t, 1, loopTestRows(), map[int]int{0: 2})
	if got := loopCheckNamed(t, loopCheckLines(small, loopFound{}), "proposal issues").Expected; got != 3 {
		t.Errorf("a three-proposal plan expects %d issues, want 3", got)
	}
	if got := loopCheckNamed(t, loopCheckLines(small, loopFound{}), "comment rows").Expected; got != int64(small.Totals.CommentRows) {
		t.Errorf("comment rows expects %d, want the plan's %d", got, small.Totals.CommentRows)
	}
}

// TestLoopSeedCheckVerdicts: each line's tolerance. An exact line fails on one object; a line at
// 1% holds a difference of 1% and fails just past it; an expected zero is met by zero alone.
func TestLoopSeedCheckVerdicts(t *testing.T) {
	p := loopPlanned(t)
	full := loopCheckFound(p)
	for _, c := range []struct {
		what  string
		found loopFound
		name  string
		ok    bool
	}{
		{"every number as planned", full, "comment rows", true},
		{"one issue short", loopFound{Issues: 890}, "proposal issues", false},
		{"one issue over", loopFound{Issues: 892}, "proposal issues", false},
		{"one part document short", loopFound{Parts: p.Totals.Parts - 1}, "part documents", false},
		{"one review page short", loopFound{ReviewPages: p.Totals.ReviewPages - 1}, "review pages", false},
		{"one review index short", loopFound{ReviewIndexes: p.Totals.ReviewIndexes - 1}, "review indexes", false},
		{"comment rows 1% light", loopFound{CommentRows: p.Totals.CommentRows - p.Totals.CommentRows/100}, "comment rows", true},
		{"comment rows 1.1% light", loopFound{CommentRows: p.Totals.CommentRows - p.Totals.CommentRows*11/1000}, "comment rows", false},
		{"comment rows 1% heavy", loopFound{CommentRows: p.Totals.CommentRows + p.Totals.CommentRows/100}, "comment rows", true},
		{"comment rows 2% heavy", loopFound{CommentRows: p.Totals.CommentRows + p.Totals.CommentRows/50}, "comment rows", false},
		{"thread bytes 1% light", loopFound{ThreadBytes: p.Totals.ThreadBytes - p.Totals.ThreadBytes/100}, "thread text bytes", true},
		{"thread bytes 5% light", loopFound{ThreadBytes: p.Totals.ThreadBytes - p.Totals.ThreadBytes/20}, "thread text bytes", false},
		{"review-page bytes 1% heavy", loopFound{ReviewPageBytes: p.Totals.ReviewPageBytes + p.Totals.ReviewPageBytes/100}, "review-page bytes", true},
		{"review-page bytes 3% heavy", loopFound{ReviewPageBytes: p.Totals.ReviewPageBytes + p.Totals.ReviewPageBytes*3/100}, "review-page bytes", false},
		{"no document past a cap", full, "documents past a cap", true},
		{"one document past a cap", loopFound{OverCap: 1}, "documents past a cap", false},
	} {
		line := loopCheckNamed(t, loopCheckLines(p, c.found), c.name)
		if line.ok() != c.ok {
			t.Errorf("%s: %s expected %d, found %d is ok=%v, want %v (delta %.4f)",
				c.what, line.Name, line.Expected, line.Found, line.ok(), c.ok, line.delta())
		}
	}
	// A found number beside an expected zero is infinitely far off, not a division by zero.
	over := loopCheckNamed(t, loopCheckLines(p, loopFound{OverCap: 3}), "documents past a cap")
	if d := over.delta(); d <= 0 || !strings.Contains(fmt.Sprintf("%+.2f", d), "Inf") {
		t.Errorf("3 documents past a cap against an expected 0 is delta %v, want +Inf", d)
	}
}

// TestLoopSeedCheckPrints: the check's table and its verdict line, as a reader sees them.
func TestLoopSeedCheckPrints(t *testing.T) {
	p := loopPlanned(t)
	var pass strings.Builder
	if failed := printLoopCheck(&pass, loopCheckLines(p, loopCheckFound(p))); failed != 0 {
		t.Fatalf("a seed that matches its plan failed %d checks", failed)
	}
	want := strings.Join([]string{
		"proposal issues: expected 891, found 891 — ok (+0.00%)",
		"comment rows: expected 464602, found 464602 — ok (+0.00%)",
		"thread text bytes: expected 564113205, found 564113205 — ok (+0.00%)",
		"review pages: expected 6057, found 6057 — ok (+0.00%)",
		"review-page bytes: expected 60291378, found 60291378 — ok (+0.00%)",
		"review indexes: expected 536, found 536 — ok (+0.00%)",
		"part documents: expected 1370, found 1370 — ok (+0.00%)",
		"documents past a cap: expected 0, found 0 — ok (+0.00%)",
		"loop-seed-check: all 8 checks passed",
		"",
	}, "\n")
	if pass.String() != want {
		t.Errorf("the passing table is\n%s\nwant\n%s", pass.String(), want)
	}

	short := loopCheckFound(p)
	short.Issues = 890
	short.ThreadBytes = p.Totals.ThreadBytes / 2
	short.OverCap = 2
	var fail strings.Builder
	if failed := printLoopCheck(&fail, loopCheckLines(p, short)); failed != 3 {
		t.Errorf("%d checks failed, want 3", failed)
	}
	for _, line := range []string{
		"proposal issues: expected 891, found 890 — FAIL (-0.11%)",
		"thread text bytes: expected 564113205, found 282056602 — FAIL (-50.00%)",
		"documents past a cap: expected 0, found 2 — FAIL (+Inf%)",
		"loop-seed-check: 3 of 8 checks failed",
	} {
		if !strings.Contains(fail.String(), line+"\n") {
			t.Errorf("the failing table does not hold %q:\n%s", line, fail.String())
		}
	}
	if strings.Contains(fail.String(), "all 8 checks passed") {
		t.Error("a failing run printed the passing verdict")
	}
}

// --- the check against a live Postgres -------------------------------------------------------

// loopCheckSchemaDDL is the part of Dispatch's own schema the check reads
// (internal/dispatch/store/migrations/0001_init.up.sql: projects, issues, artifacts,
// artifact_versions and comments), with the columns readLoopFound names and no other. A full
// Dispatch is not this package's to start, so the live check runs over these tables, filled from
// what the stub answered.
const loopCheckSchemaDDL = `
create table projects (key text primary key, name text not null);
create table issues (
  key text primary key, project_key text not null references projects(key), title text not null);
create table artifacts (
  id uuid primary key, issue_key text not null references issues(key),
  slug text not null, name text not null, kind text not null);
create table artifact_versions (
  id uuid primary key default gen_random_uuid(), artifact_id uuid not null references artifacts(id),
  number integer not null, markdown text, unique (artifact_id, number));
create table comments (
  id uuid primary key, issue_key text not null references issues(key), body text not null,
  anchor jsonb, reply_to uuid references comments(id), resolved boolean not null default false)`

// TestLoopSeedCheckLive: the check's own queries, over a database holding a seed. A plan is
// written through the stub, every object it created is stored as Dispatch stores it, and the check
// finds exactly what the plan expected; then a document is taken past each cap and the check sees
// both.
func TestLoopSeedCheckLive(t *testing.T) {
	pool := loopTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, loopCheckSchemaDDL); err != nil {
		t.Fatalf("create the dispatch tables the check reads: %v", err)
	}
	p := loopTestPlanOf(t, 17, loopTestRows(), map[int]int{0: 3})
	planID := loopPlanID(p)
	ids, err := loadLoopIDStore(ctx, pool, planID)
	if err != nil {
		t.Fatal(err)
	}
	stub := newLoopStub(t)
	w := newLoopWriter(p, newAPI(stub.srv.URL, "token", 8), ids, "LOOP", log.New(io.Discard, "", 0))
	if err := w.run(ctx, 3); err != nil {
		t.Fatal(err)
	}
	loopStoreSeed(ctx, t, pool, planID, stub)

	found, err := readLoopFound(ctx, pool, planID)
	if err != nil {
		t.Fatalf("read what the database holds: %v", err)
	}
	if want := loopCheckFound(p); found != want {
		t.Fatalf("the database holds %+v, want the plan's %+v", found, want)
	}
	var out strings.Builder
	if failed := printLoopCheck(&out, loopCheckLines(p, found)); failed != 0 {
		t.Fatalf("%d checks failed over a seed that matches its plan:\n%s", failed, out.String())
	}
	t.Logf("over a live database:\n%s", out.String())

	// A seed of another plan in the same database counts for nothing: the check reads the rows
	// its own id map names.
	other := loopTestPlanOf(t, 18, loopTestRows(), map[int]int{0: 3})
	empty, err := readLoopFound(ctx, pool, loopPlanID(other))
	if err != nil {
		t.Fatal(err)
	}
	if empty != (loopFound{}) {
		t.Errorf("a plan nothing wrote finds %+v, want nothing", empty)
	}

	// One document past the markdown cap and another past the margin cap: both are counted, and
	// each is counted once. The margin is every comment on the document, the seed's own and any
	// other, which is what Dispatch weighs (internal/dispatch/docs/growth.go:149-153).
	var part, page string
	if err := pool.QueryRow(ctx, `select local_id from loadtest.loop_ids where plan = $1 and kind = 'part' order by path limit 1`, planID).Scan(&part); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select local_id from loadtest.loop_ids where plan = $1 and kind = 'page' order by path limit 1`, planID).Scan(&page); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update artifact_versions set markdown = repeat('x', $2) where artifact_id = $1`, page, loopMarkdownCap+1); err != nil {
		t.Fatal(err)
	}
	var issue string
	if err := pool.QueryRow(ctx, `select issue_key from artifacts where id = $1`, part).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into comments (id, issue_key, body, anchor)
		values (gen_random_uuid(), $1, repeat('y', $2), jsonb_build_object('artifact_id', $3::text, 'quote', 'q'))
	`, issue, loopMarginCap+1, part); err != nil {
		t.Fatal(err)
	}
	past, err := readLoopFound(ctx, pool, planID)
	if err != nil {
		t.Fatal(err)
	}
	if past.OverCap != 2 {
		t.Errorf("%d documents past a cap, want the one over the markdown cap and the one over the margin cap", past.OverCap)
	}
	line := loopCheckNamed(t, loopCheckLines(p, past), "documents past a cap")
	if line.ok() {
		t.Error("two documents past a cap passed the check")
	}
}

// loopStoreSeed stores everything the stub was sent as Dispatch stores it: the project, the
// issues, each document with its version 1 markdown, each thread root with its anchor and each
// reply under it. It walks loadtest.loop_ids, so what it stores is exactly what the run recorded.
func loopStoreSeed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, planID string, stub *loopStub) {
	t.Helper()
	// In the order the seed writes them, so an issue is there before its documents and a thread
	// root before the replies that name it.
	rows, err := pool.Query(ctx, `
		select kind, path, local_id from loadtest.loop_ids where plan = $1
		order by array_position($2::text[], kind), path
	`, planID, loopKinds)
	if err != nil {
		t.Fatal(err)
	}
	type object struct{ kind, path, local string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.kind, &o.path, &o.local); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The issue each object belongs to, by the path's proposal: the id map names the object, and
	// the writer sent it to that proposal's issue.
	issueOf := map[string]string{}
	for _, o := range objects {
		if o.kind == loopKindIssue {
			issueOf[strings.TrimPrefix(o.path, loopKindIssue+".")] = o.local
		}
	}
	proposalOf := func(path string) string {
		parts := strings.Split(path, ".")
		return parts[1]
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store the seed (%s): %v", strings.TrimSpace(strings.SplitN(sql, "(", 2)[0]), err)
		}
	}
	for _, o := range objects {
		write := stub.made[o.local]
		switch o.kind {
		case loopKindProject:
			exec(`insert into projects (key, name) values ($1, $1)`, o.local)
		case loopKindIssue:
			exec(`insert into issues (key, project_key, title) values ($1, $2, $3)`,
				o.local, "LOOP", write.body["title"])
		case loopKindPart, loopKindPage, loopKindIndex:
			name, _ := write.body["name"].(string)
			content, _ := write.body["content"].(string)
			exec(`insert into artifacts (id, issue_key, slug, name, kind) values ($1, $2, $3, $3, 'doc')`,
				o.local, issueOf[proposalOf(o.path)], name)
			exec(`insert into artifact_versions (artifact_id, number, markdown) values ($1, 1, $2)`,
				o.local, content)
		case loopKindThread, loopKindReply:
			body, _ := write.body["body"].(string)
			var anchor []byte
			if a, ok := write.body["anchor"].(map[string]any); ok {
				anchor, _ = json.Marshal(map[string]any{"artifact_id": a["artifact"], "quote": a["quote"]})
			}
			var replyTo any
			if to, ok := write.body["reply_to"].(string); ok {
				replyTo = to
			}
			exec(`insert into comments (id, issue_key, body, anchor, reply_to) values ($1, $2, $3, $4, $5)`,
				o.local, issueOf[proposalOf(o.path)], body, anchor, replyTo)
		case loopKindResolve:
			exec(`update comments set resolved = true where id = $1`, o.local)
		}
	}
}
