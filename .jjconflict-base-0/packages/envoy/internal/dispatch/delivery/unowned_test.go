package delivery

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedUnownedIssueProject replaces project key's issues with issues, written directly: the P0
// rule reads issues.priority, status, claimed_by and route and nothing else. deliveryTestPool's
// database is the shared one the package's tests migrate, not a clone per test, so the test owns
// only the projects it names and clears their issues first.
func seedUnownedIssueProject(t *testing.T, ctx context.Context, pool *store.Pool, key string, issues []unownedSeed) {
	t.Helper()
	if _, err := pool.Exec(ctx, `delete from issues where project_key = $1`, key); err != nil {
		t.Fatalf("clear %s issues: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ($1, $1) on conflict do nothing`, key); err != nil {
		t.Fatalf("seed project %s: %v", key, err)
	}
	for _, issue := range issues {
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank)
			values ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, case when $8::jsonb is null then null else now() end, '{"kind":"system","id":"delivery-test"}', 'U')
		`, issue.key, key, issue.number, issue.title, issue.status, issue.priority, issue.route, issue.claimedBy); err != nil {
			t.Fatalf("seed issue %s: %v", issue.key, err)
		}
	}
}

type unownedSeed struct {
	key       string
	number    int
	title     string
	status    string
	priority  *int
	route     *string
	claimedBy *string
}

func TestListUnownedP0(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	p0, p1 := 0, 1
	route := "role:dora"
	claim := `{"kind":"session","id":"01a1-test-session"}`
	seedUnownedIssueProject(t, ctx, pool, "ACME", []unownedSeed{
		{key: "ACME-103", number: 103, title: "Production deploy gate flakes", status: "triage", priority: &p0},
		{key: "ACME-104", number: 104, title: "claimed", status: "in_progress", priority: &p0, claimedBy: &claim},
		{key: "ACME-105", number: 105, title: "routed", status: "todo", priority: &p0, route: &route},
		{key: "ACME-106", number: 106, title: "closed", status: "done", priority: &p0},
		{key: "ACME-107", number: 107, title: "not a P0", status: "todo", priority: &p1},
		{key: "ACME-2", number: 2, title: "an earlier unowned P0", status: "backlog", priority: &p0},
	})
	seedUnownedIssueProject(t, ctx, pool, "ACMEB", []unownedSeed{
		{key: "ACMEB-1", number: 1, title: "another project's unowned P0", status: "todo", priority: &p0},
	})

	unowned, err := ListUnownedP0(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnownedP0: %v", err)
	}
	// Other packages' tests can leave issues in the shared database; the rule is checked on the
	// projects this test owns, which the query returns in project-then-number order like any other.
	ours := slices.DeleteFunc(slices.Clone(unowned), func(issue UnownedIssue) bool {
		return !slices.Contains([]string{"ACME-2", "ACME-103", "ACME-104", "ACME-105", "ACME-106", "ACME-107", "ACMEB-1"}, issue.Key)
	})
	want := []UnownedIssue{
		{Key: "ACME-2", Title: "an earlier unowned P0"},
		{Key: "ACME-103", Title: "Production deploy gate flakes"},
		{Key: "ACMEB-1", Title: "another project's unowned P0"},
	}
	if !slices.Equal(ours, want) {
		t.Fatalf("ListUnownedP0 = %+v, want %+v (open, P0, neither claimed nor routed; project then number)", ours, want)
	}
}
