package delivery

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueSeed is one issue a test writes directly: the reads below look only at priority, status
// and parent_key.
type issueSeed struct {
	key      string
	priority *int
	status   string
	parent   string
}

// seedIssues replaces project's issues with issues, in order, each parent written before its child.
// deliveryTestPool's database is shared by the package's tests, so a test owns only the projects it
// names. One delete statement clears them, so a parent chain or a cycle a failed run left is
// removed whole.
func seedIssues(t *testing.T, ctx context.Context, pool *store.Pool, project string, issues []issueSeed) {
	t.Helper()
	if _, err := pool.Exec(ctx, `delete from issues where project_key = $1`, project); err != nil {
		t.Fatalf("clear %s issues: %v", project, err)
	}
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ($1, $1) on conflict do nothing`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	for number, issue := range issues {
		status := issue.status
		if status == "" {
			status = "todo"
		}
		var parent *string
		if issue.parent != "" {
			parent = &issue.parent
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, priority, parent_key, created_by, rank)
			values ($1, $2, $3, $1, $4, $5, $6, '{"kind":"system","id":"delivery-test"}', 'U')
		`, issue.key, project, number+1, status, issue.priority, parent); err != nil {
			t.Fatalf("seed issue %s: %v", issue.key, err)
		}
	}
}

func setParent(t *testing.T, ctx context.Context, pool *store.Pool, key, parent string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `update issues set parent_key = $2 where key = $1`, key, parent); err != nil {
		t.Fatalf("set %s's parent to %s: %v", key, parent, err)
	}
}

func keysOf(set map[string]struct{}) []string { return slices.Sorted(maps.Keys(set)) }

// OpenP0Keys is every open P0 across every project: a P0 in any status but done, and no P1, no
// unset priority and no done P0.
func TestOpenP0KeysListsOpenP0sOnly(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	p0, p1 := 0, 1
	seedIssues(t, ctx, pool, "LIVEP", []issueSeed{
		{key: "LIVEP-1", priority: &p0, status: "todo"},
		{key: "LIVEP-2", priority: &p0, status: "done"},
		{key: "LIVEP-3", priority: &p1, status: "todo"},
		{key: "LIVEP-4", priority: &p0, status: "in_progress"},
		{key: "LIVEP-5", status: "todo"},
		{key: "LIVEP-6", priority: &p0, status: "backlog"},
		{key: "LIVEP-7", priority: &p1, status: "done"},
	})
	seedIssues(t, ctx, pool, "LIVEQ", []issueSeed{{key: "LIVEQ-1", priority: &p0, status: "triage"}})

	open, err := OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys: %v", err)
	}
	var ours []string
	for _, key := range keysOf(open) {
		if strings.HasPrefix(key, "LIVEP-") || strings.HasPrefix(key, "LIVEQ-") {
			ours = append(ours, key)
		}
	}
	if want := []string{"LIVEP-1", "LIVEP-4", "LIVEP-6", "LIVEQ-1"}; !slices.Equal(ours, want) {
		t.Fatalf("OpenP0Keys over the seeded projects = %v, want %v", ours, want)
	}

	seedIssues(t, ctx, pool, "LIVEP", []issueSeed{{key: "LIVEP-1", priority: &p0, status: "done"}})
	seedIssues(t, ctx, pool, "LIVEQ", nil)
	open, err = OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys again: %v", err)
	}
	for key := range open {
		if strings.HasPrefix(key, "LIVEP-") || strings.HasPrefix(key, "LIVEQ-") {
			t.Fatalf("OpenP0Keys lists %s, which is done or gone", key)
		}
	}
	if open == nil {
		t.Fatal("OpenP0Keys returned a nil set")
	}
}

// IssueAncestors walks every key's parent chain to its root: a three-deep chain returns each
// ancestor, keys that share a chain return it once, and a root or an unknown key adds nothing.
func TestIssueAncestorsReturnsEveryAncestor(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedIssues(t, ctx, pool, "LIVEA", []issueSeed{
		{key: "LIVEA-1"},
		{key: "LIVEA-2", parent: "LIVEA-1"},
		{key: "LIVEA-3", parent: "LIVEA-2"},
		{key: "LIVEA-4", parent: "LIVEA-3"},
		{key: "LIVEA-5"},
		{key: "LIVEA-6", parent: "LIVEA-5"},
	})

	for _, tc := range []struct {
		keys []string
		want []string
	}{
		{keys: []string{"LIVEA-4"}, want: []string{"LIVEA-1", "LIVEA-2", "LIVEA-3"}},
		{keys: []string{"LIVEA-4", "LIVEA-3"}, want: []string{"LIVEA-1", "LIVEA-2", "LIVEA-3"}},
		{keys: []string{"LIVEA-4", "LIVEA-6"}, want: []string{"LIVEA-1", "LIVEA-2", "LIVEA-3", "LIVEA-5"}},
		{keys: []string{"LIVEA-2"}, want: []string{"LIVEA-1"}},
		{keys: []string{"LIVEA-1", "LIVEA-5"}, want: nil},
		{keys: []string{"LIVEA-404"}, want: nil},
		{keys: nil, want: nil},
	} {
		ancestors, err := IssueAncestors(ctx, pool, tc.keys, 32)
		if err != nil {
			t.Fatalf("IssueAncestors(%v): %v", tc.keys, err)
		}
		if ancestors == nil {
			t.Fatalf("IssueAncestors(%v) returned a nil set", tc.keys)
		}
		if got := keysOf(ancestors); !slices.Equal(got, tc.want) {
			t.Fatalf("IssueAncestors(%v) = %v, want %v", tc.keys, got, tc.want)
		}
	}
}

// The walk stops at maxDepth links up: a chain longer than the cap returns the ancestors within
// it, and a cycle, which the API refuses to write but a raced reparent could commit, ends at the
// cap too. In a cycle a key is its own ancestor, so it is in the answer.
func TestIssueAncestorsStopsAtTheDepthCap(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedIssues(t, ctx, pool, "LIVEC", []issueSeed{
		{key: "LIVEC-0"},
		{key: "LIVEC-1", parent: "LIVEC-0"},
		{key: "LIVEC-2", parent: "LIVEC-1"},
		{key: "LIVEC-3", parent: "LIVEC-2"},
		{key: "LIVEC-4", parent: "LIVEC-3"},
		{key: "LIVEC-5", parent: "LIVEC-4"},
		{key: "LIVEC-6", parent: "LIVEC-5"},
	})
	capped, err := IssueAncestors(ctx, pool, []string{"LIVEC-6"}, 3)
	if err != nil {
		t.Fatalf("IssueAncestors past the cap: %v", err)
	}
	if got, want := keysOf(capped), []string{"LIVEC-3", "LIVEC-4", "LIVEC-5"}; !slices.Equal(got, want) {
		t.Fatalf("IssueAncestors(LIVEC-6, maxDepth 3) = %v, want %v", got, want)
	}

	seedIssues(t, ctx, pool, "LIVED", []issueSeed{
		{key: "LIVED-1"},
		{key: "LIVED-2", parent: "LIVED-1"},
		{key: "LIVED-3", parent: "LIVED-2"},
	})
	setParent(t, ctx, pool, "LIVED-1", "LIVED-3")
	for _, maxDepth := range []int{1, 2, 32} {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		ancestors, err := IssueAncestors(bounded, pool, []string{"LIVED-2"}, maxDepth)
		cancel()
		if err != nil {
			t.Fatalf("IssueAncestors over a cycle, maxDepth %d: %v", maxDepth, err)
		}
		want := map[int][]string{1: {"LIVED-1"}, 2: {"LIVED-1", "LIVED-3"}, 32: {"LIVED-1", "LIVED-2", "LIVED-3"}}[maxDepth]
		if got := keysOf(ancestors); !slices.Equal(got, want) {
			t.Fatalf("IssueAncestors(LIVED-2) over a cycle, maxDepth %d = %v, want %v", maxDepth, got, want)
		}
	}
}
