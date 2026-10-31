// Package explaintest reads an `EXPLAIN (FORMAT JSON)` plan tree for the three things every
// index-pinning test in this module asks it: whether a named relation is sequentially scanned
// anywhere in the plan, whether any scan in it reads a relation without an index condition, and
// which index each index scan reads through, under which condition. dispatch/api, dispatch/refs
// and broker/requests each pin an index's reach with one of these checks, so they live here once
// rather than as a second (or third) hand copy of the same recursive walk; it is an ordinary
// package rather than a _test.go file because Go cannot share test-only code across packages
// (see internal/stacktest's identical reasoning).
package explaintest

import (
	"encoding/json"
	"slices"
	"testing"
)

// indexScanTypes is every plan node that reads a relation through an index, and so every node
// that can carry an "Index Cond".
var indexScanTypes = []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"}

// planNode is what the checks below read of one "Plan" node of `EXPLAIN (FORMAT JSON)`: its
// type, the relation and index it names, the index condition it carries - nil when it carries
// none, which is not the same as an empty one - and its children.
type planNode struct {
	NodeType     string            `json:"Node Type"`
	RelationName string            `json:"Relation Name"`
	IndexName    string            `json:"Index Name"`
	IndexCond    *string           `json:"Index Cond"`
	Plans        []json.RawMessage `json:"Plans"`
}

// planNodes flattens planJSON - one "Plan" node of `EXPLAIN (FORMAT JSON)`'s top-level array -
// into every node of the tree under it, each node before its children. Every check this package
// exports is a filter over this one walk, so the tree is descended in one place rather than once
// per check.
func planNodes(t testing.TB, planJSON json.RawMessage) []planNode {
	t.Helper()
	var node planNode
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	nodes := []planNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, planNodes(t, child)...)
	}
	return nodes
}

// SeqScansRelation reports whether planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s
// top-level array — contains a "Seq Scan" node naming relation, at any depth.
func SeqScansRelation(t testing.TB, planJSON json.RawMessage, relation string) bool {
	t.Helper()
	for _, node := range planNodes(t, planJSON) {
		if node.NodeType == "Seq Scan" && node.RelationName == relation {
			return true
		}
	}
	return false
}

// IndexScan is one index-reading node of a plan, with what a caller needs to judge the read: the
// node type, the relation (empty on a "Bitmap Index Scan", which reports only its index), the
// index the node reads through, and the index condition the plan wrote, empty when the node
// carries none.
type IndexScan struct {
	NodeType string
	Relation string
	Index    string
	Cond     string
}

// String names the scan as a failure message cites it, the way ScansWithoutIndexCond names one.
func (s IndexScan) String() string {
	return describeScan(s.NodeType, s.Relation, s.Index)
}

// IndexScans names every index scan in planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s
// top-level array — at any depth, in the order the walk meets them. ScansWithoutIndexCond holds
// a scan to having some index condition; whether that condition keys the index's own first
// column, the only column an index scan actually narrows on, is a question the catalog answers,
// so a caller that asks it reads each scan's index and condition from here and judges the pair
// itself.
func IndexScans(t testing.TB, planJSON json.RawMessage) []IndexScan {
	t.Helper()
	var scans []IndexScan
	for _, node := range planNodes(t, planJSON) {
		if !slices.Contains(indexScanTypes, node.NodeType) {
			continue
		}
		scan := IndexScan{NodeType: node.NodeType, Relation: node.RelationName, Index: node.IndexName}
		if node.IndexCond != nil {
			scan.Cond = *node.IndexCond
		}
		scans = append(scans, scan)
	}
	return scans
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
	var unkeyed []string
	for _, node := range planNodes(t, planJSON) {
		unkeyedScan := node.NodeType == "Seq Scan" ||
			(slices.Contains(indexScanTypes, node.NodeType) && node.IndexCond == nil)
		if unkeyedScan {
			unkeyed = append(unkeyed, describeScan(node.NodeType, node.RelationName, node.IndexName))
		}
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
