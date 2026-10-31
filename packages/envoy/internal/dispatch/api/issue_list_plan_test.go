package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// analyzedNode is what these tests read of one node of an `EXPLAIN (ANALYZE, FORMAT JSON)` plan:
// how many times the node ran, how many rows it answered on each of those runs, what it reads,
// and where it hangs. `loops` and `actual rows` are only in an ANALYZE plan, which is why these
// tests run the statements rather than only plan them.
type analyzedNode struct {
	NodeType     string         `json:"Node Type"`
	Relationship string         `json:"Parent Relationship,omitempty"`
	RelationName string         `json:"Relation Name,omitempty"`
	IndexName    string         `json:"Index Name,omitempty"`
	Rows         float64        `json:"Actual Rows"`
	Loops        float64        `json:"Actual Loops"`
	Plans        []analyzedNode `json:"Plans,omitempty"`
}

// name is the node as a failure cites it: the type, and the relation or index it reads.
func (node analyzedNode) name() string {
	described := node.NodeType
	if node.RelationName != "" {
		described += " on " + node.RelationName
	}
	if node.IndexName != "" {
		described += " using " + node.IndexName
	}
	return described
}

// attachSpine is every join that attaches a value to the rows the caller gets: the joins on the
// plan's outer edge, read from the top down. Each of them takes the rows below it and adds one
// relation's columns, so its own row count is the answer's. A join inside the relation being
// attached — the grouped read of a listing's blockers, say — is below an inner edge and is not
// one of these: it builds the thing attached, not the attachment.
func (node analyzedNode) attachSpine() []analyzedNode {
	spine := []analyzedNode{}
	for {
		if slices.Contains(joinTypes, node.NodeType) {
			spine = append(spine, node)
		}
		outer := -1
		for index, child := range node.Plans {
			if child.Relationship == "Outer" {
				outer = index
				break
			}
		}
		if outer < 0 {
			return spine
		}
		node = node.Plans[outer]
	}
}

// everyNode is every node of the plan, the CTE and subplan subtrees included.
func (node analyzedNode) everyNode() []analyzedNode {
	nodes := []analyzedNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, child.everyNode()...)
	}
	return nodes
}

// joinTypes is every node that attaches one relation's rows to another's. A listing's six
// resolved values each reach the page through one of these.
var joinTypes = []string{"Nested Loop", "Hash Join", "Merge Join"}

// analyzePlan runs statement under `EXPLAIN (ANALYZE, FORMAT JSON)` and answers its root node
// with the plan's text, which a failure prints.
func analyzePlan(t *testing.T, database *store.Store, statement string, arguments ...any) (analyzedNode, string) {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), "explain (analyze, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	var plans []struct {
		Plan analyzedNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the explain answer (%v): %s", err, raw)
	}
	return plans[0].Plan, string(raw)
}

// A paged listing resolves its page and nothing else. The node that attaches each of the six
// values to the page runs once and answers at most the page's rows, and each parent walk starts
// from at most the page's keys, which is what says the walk is no longer one per matching
// issue. The fixture's project holds several hundred issues, so a plan that resolved them all
// would show it.
func TestAPagedListingResolvesItsPageAlone(t *testing.T) {
	fixture := newIssueListFixture(t)
	for _, listing := range []struct {
		name      string
		statement string
		arguments []any
		limit     int
	}{
		{
			name:      "the paged listing",
			statement: listIssuesPageQuery,
			arguments: []any{"LIST", "", "", nil, []string{}, false, []int16{}, false, 5, 10},
			limit:     5,
		},
		{
			name:      "the paged pinned listing",
			statement: listPinnedIssuesPageQuery,
			arguments: []any{"LIST", "", "", nil, []string{}, false, []int16{}, false, "alice", 2, 1},
			limit:     2,
		},
	} {
		t.Run(listing.name, func(t *testing.T) {
			plan, text := analyzePlan(t, fixture.database, listing.statement, listing.arguments...)
			name, limit := listing.name, listing.limit
			joins := plan.attachSpine()
			for _, node := range joins {
				if node.Loops != 1 || node.Rows > float64(limit) {
					t.Errorf("%s attaches rows at %s with %.0f rows over %.0f loops, want at most %d rows over one loop:\n%s",
						name, node.name(), node.Rows, node.Loops, limit, text)
				}
			}
			if len(joins) < 6 {
				t.Errorf("%s attaches its values through %d joins, want one for each of the six:\n%s", name, len(joins), text)
			}
			walks := 0
			for _, node := range plan.everyNode() {
				if node.NodeType != "Recursive Union" {
					continue
				}
				walks++
				if len(node.Plans) == 0 {
					t.Fatalf("%s has a parent walk with no base term:\n%s", name, text)
				}
				if base := node.Plans[0]; base.Rows > float64(limit) || base.Loops != 1 {
					t.Errorf("%s starts a parent walk from %.0f keys over %.0f loops, want at most the page's %d over one loop:\n%s",
						name, base.Rows, base.Loops, limit, text)
				}
			}
			if walks != 2 {
				t.Errorf("%s runs %d parent walks, want the components walk and the repository walk:\n%s", name, walks, text)
			}
		})
	}
}

// An unpaged listing resolves every issue its filters match, and reads each of the three tables
// its joins reach once for all of them rather than once per issue.
func TestAnUnpagedListingReadsEachResolvedTableOnce(t *testing.T) {
	fixture := newIssueListFixture(t)
	plan, text := analyzePlan(t, fixture.database, listIssuesQuery, "LIST", "", "", nil, []string{}, false, []int16{}, false)
	for _, relation := range []string{"issue_links", "issue_components", "repo_projects"} {
		read := 0
		for _, node := range plan.everyNode() {
			if node.RelationName != relation {
				continue
			}
			read++
			if node.Loops != 1 {
				t.Errorf("the listing reads %s at %s over %.0f loops, want one:\n%s", relation, node.name(), node.Loops, text)
			}
		}
		if read == 0 {
			t.Errorf("the listing reads no %s at all:\n%s", relation, text)
		}
	}
}

// The listing reaches its open asks through the partial index on them, asks_open, rather than
// reading the table: an instance's ask volume is far larger than any one listing's. The index
// the scan reads is the assertion, not the absence of a sequential scan, because a scan can
// carry an index condition on a column its index does not lead with and still read the whole
// index (api/audited_reads_test.go). A condition is not required here: the index's own
// predicate, `state = 'open'`, is this read's filter, so the whole of it is exactly the rows
// the grouped read counts, and a condition it does carry is held to its first key column.
func TestTheListingReadsOpenAsksThroughTheirIndex(t *testing.T) {
	fixture := newIssueListFixture(t)
	firstKeys := explaintest.NewIndexKeys(fixture.database.Pool)
	for name, read := range map[string]struct {
		statement string
		arguments []any
	}{
		"the listing":       {listIssuesQuery, []any{"LIST", "", "", nil, []string{}, false, []int16{}, false}},
		"the paged listing": {listIssuesPageQuery, []any{"LIST", "", "", nil, []string{}, false, []int16{}, false, 5, 10}},
	} {
		plan := planWithoutSeqScans(t, fixture.database, read.statement, read.arguments...)
		if explaintest.SeqScansRelation(t, plan, "asks") {
			t.Fatalf("%s scans asks sequentially; want a search of asks_open:\n%s", name, plan)
		}
		searched := false
		for _, scan := range explaintest.IndexScans(t, plan) {
			if scan.Index != "asks_open" {
				continue
			}
			searched = true
			firstKey := firstKeys.FirstKey(t, scan.Index)
			if scan.Cond != "" && !explaintest.CondNamesFirstKey(scan.Cond, firstKey) {
				t.Errorf("%s reads asks_open on `%s`, which names nothing of its first key column `%s`:\n%s",
					name, scan.Cond, firstKey, plan)
			}
		}
		if !searched {
			t.Errorf("%s searches no asks_open:\n%s", name, plan)
		}
	}
}

// The project list counts every project's open asks in one grouped read: no node of its plan is
// a subquery run for each project row, and the asks it counts are read once.
func TestTheProjectListRunsNoSubqueryPerProject(t *testing.T) {
	fixture := newIssueListFixture(t)
	plan, text := analyzePlan(t, fixture.database, projectListQuery)
	for _, node := range plan.everyNode() {
		if node.Relationship == "SubPlan" {
			t.Errorf("the project list runs %s as a subplan, want one grouped read for every project:\n%s", node.name(), text)
		}
		if node.RelationName == "asks" && node.Loops != 1 {
			t.Errorf("the project list reads asks at %s over %.0f loops, want one:\n%s", node.name(), node.Loops, text)
		}
	}
	if !strings.Contains(text, "asks") {
		t.Errorf("the project list's plan reads no asks at all:\n%s", text)
	}
}
