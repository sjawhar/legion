// Package explaintest reads an `EXPLAIN (FORMAT JSON)` plan tree for the one thing every
// index-pinning test in this module asks it: whether a named relation is sequentially scanned
// anywhere in the plan. dispatch/api and broker/requests each pin an index's reach with this
// check, so it lives here once rather than as a second (or third) hand copy of the same recursive
// walk; it is an ordinary package rather than a _test.go file because Go cannot share test-only
// code across packages (see internal/stacktest's identical reasoning).
package explaintest

import (
	"encoding/json"
	"testing"
)

// SeqScansRelation reports whether planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s
// top-level array — contains a "Seq Scan" node naming relation, at any depth.
func SeqScansRelation(t testing.TB, planJSON json.RawMessage, relation string) bool {
	t.Helper()
	var node struct {
		NodeType     string            `json:"Node Type"`
		RelationName string            `json:"Relation Name"`
		Plans        []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.NodeType == "Seq Scan" && node.RelationName == relation {
		return true
	}
	for _, child := range node.Plans {
		if SeqScansRelation(t, child, relation) {
			return true
		}
	}
	return false
}
