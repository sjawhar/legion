// Package explaintest reads an `EXPLAIN (FORMAT JSON)` plan tree for the two things every
// index-pinning test in this module asks it: whether a named relation is sequentially scanned
// anywhere in the plan, and whether any scan in it reads a relation without an index condition.
// dispatch/api, dispatch/refs and broker/requests each pin an index's reach with one of these
// checks, so they live here once rather than as a second (or third) hand copy of the same
// recursive walk; it is an ordinary package rather than a _test.go file because Go cannot share
// test-only code across packages (see internal/stacktest's identical reasoning).
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

// ScansWithoutIndexCond names every scan in planJSON — one "Plan" node of `EXPLAIN (FORMAT
// JSON)`'s top-level array — that reads a relation no index condition keys, at any depth: a
// "Seq Scan", and an "Index Scan", "Index Only Scan" or "Bitmap Index Scan" carrying no "Index
// Cond". A statement written to reach every table through an index is held to that by its plan,
// and such a scan is a read no index serves. Each name carries the node type, the relation and
// the index the node reports, so a failure says which read is unkeyed rather than only that one
// is.
func ScansWithoutIndexCond(t testing.TB, planJSON json.RawMessage) []string {
	t.Helper()
	var node struct {
		NodeType     string            `json:"Node Type"`
		RelationName string            `json:"Relation Name"`
		IndexName    string            `json:"Index Name"`
		IndexCond    *string           `json:"Index Cond"`
		Plans        []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	var unkeyed []string
	switch node.NodeType {
	case "Seq Scan":
		unkeyed = append(unkeyed, describeScan(node.NodeType, node.RelationName, node.IndexName))
	case "Index Scan", "Index Only Scan", "Bitmap Index Scan":
		if node.IndexCond == nil {
			unkeyed = append(unkeyed, describeScan(node.NodeType, node.RelationName, node.IndexName))
		}
	}
	for _, child := range node.Plans {
		unkeyed = append(unkeyed, ScansWithoutIndexCond(t, child)...)
	}
	return unkeyed
}

// describeScan names one scan node as a failure message cites it. A Bitmap Index Scan reports no
// relation and a Seq Scan no index, so each part is left out when the plan does not carry it.
func describeScan(nodeType, relation, index string) string {
	description := nodeType
	if relation != "" {
		description += " on " + relation
	}
	if index != "" {
		description += " using " + index
	}
	return description
}
