package docs

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// reviewOrderIndex is the index migration 0120 adds in docs.ReviewOrder's own order.
const reviewOrderIndex = "artifact_reviews_by_artifact_event_seq"

// The newest-review reads take one row in ReviewOrder: the approval an approval request names, a
// carry candidate's latest review, read at every request and every version that holds an
// answered block, and the document's own newest review, read by every document write that stored
// a version (api.supersededApprovalAdvice). Each must read that row from the index in
// ReviewOrder's order, the first entry of the document's range, rather than read every review of
// the document and sort them, which artifact_reviews_by_artifact (created_at desc, id desc)
// alone would make it do. newestReviewQueries names them all, and
// TestEveryNewestReviewQueryIsPlanned fails when this file gains one this test does not explain.
var newestReviewQueries = []struct {
	name     string
	constant string
	sql      string
	args     []any
}{
	{"the latest approval", "latestApprovedReviewQuery", latestApprovedReviewQuery, []any{reviewPlanDocument}},
	{"a person's latest review", "latestReviewByQuery", latestReviewByQuery, []any{reviewPlanDocument, "alice@example.com"}},
	{"the newest review", "newestReviewQuery", newestReviewQuery, []any{reviewPlanDocument}},
}

// reviewPlanDocument stands for a document in every planned read.
const reviewPlanDocument = "00000000-0000-4000-8000-000000000000"

func TestNewestReviewReadsTakeTheReviewOrderIndex(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	// An empty table makes every plan cheap, so the planner is left the ordered reads alone: no
	// sequential scan and no bitmap scan, which reads its rows out of index order. What remains is
	// an index scan, and the test asks which index it reads and whether it sorts what it read.
	for _, setting := range []string{"set local enable_seqscan = off", "set local enable_bitmapscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	for _, query := range newestReviewQueries {
		t.Run(query.name, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
				t.Fatalf("explain query: %v", err)
			}
			var plans []struct {
				Plan json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode explain output (%v): %s", err, planJSON)
			}
			if !planUsesIndex(t, plans[0].Plan, reviewOrderIndex) {
				t.Fatalf("query plan does not read %s:\n%s", reviewOrderIndex, planJSON)
			}
			if planSorts(t, plans[0].Plan) {
				t.Fatalf("query plan sorts the document's reviews; want the first entry of %s:\n%s", reviewOrderIndex, planJSON)
			}
		})
	}
}

// The plan test above proves only the reads it names, so a fourth newest-review query added to
// approval_review.go and left out of newestReviewQueries would be unplanned and could read
// artifact_reviews_by_artifact, which sorts every review of the document. Every package-level
// query constant of that file that reads one row of artifact_reviews is therefore required to be
// in the table, by name.
func TestEveryNewestReviewQueryIsPlanned(t *testing.T) {
	planned := map[string]struct{}{}
	for _, query := range newestReviewQueries {
		planned[query.constant] = struct{}{}
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "approval_review.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse approval_review.go: %v", err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if index >= len(value.Values) {
					continue
				}
				text := strings.ToLower(literalsOf(value.Values[index]))
				if !strings.Contains(text, "from artifact_reviews") || !strings.Contains(text, "limit 1") {
					continue
				}
				if _, ok := planned[name.Name]; !ok {
					t.Errorf("%s reads one row of artifact_reviews and no test explains it; add it to newestReviewQueries so its plan is held to %s",
						name.Name, reviewOrderIndex)
				}
			}
		}
	}
}

// literalsOf is every string literal of an expression, joined: a query constant is written as a
// raw string spliced with ReviewColumns and ReviewOrder, and only its own literals say which
// table it reads.
func literalsOf(expression ast.Expr) string {
	var parts []string
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			if text, err := strconv.Unquote(literal.Value); err == nil {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.Join(parts, " ")
}

// planSorts reports whether a plan node, at any depth, is a sort.
func planSorts(t *testing.T, planJSON json.RawMessage) bool {
	t.Helper()
	var node struct {
		NodeType string            `json:"Node Type"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.NodeType == "Sort" || node.NodeType == "Incremental Sort" {
		return true
	}
	for _, child := range node.Plans {
		if planSorts(t, child) {
			return true
		}
	}
	return false
}
