// Package explaintest reads an `EXPLAIN (FORMAT JSON)` plan tree for the things every
// index-pinning test in this module asks it: whether a named relation is sequentially scanned
// anywhere in the plan, whether any scan in it reads a relation without an index condition, and
// which index each index scan reads through, under which condition. It also answers, from the
// catalog, the first key column of a named index and whether a scan's condition names it, which
// is what tells a read served by an index from one that reads the whole index and filters it,
// and it holds a whole plan to that (RequireIndexKeyed) and builds the generic plan a prepared
// statement settles on (GenericPlan), which is the plan an index is lost in.
// dispatch/api, dispatch/refs and broker/requests each pin an index's reach with these checks,
// so they live here once rather than as a second (or third) hand copy of the same recursive
// walk; it is an ordinary package rather than a _test.go file because Go cannot share test-only
// code across packages (see internal/stacktest's identical reasoning).
package explaintest

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// OnePlan is the one "Plan" node of raw, an `EXPLAIN (FORMAT JSON)` answer, which is what every
// check below is given. A statement explains to a top-level array of one plan, so an answer
// holding any other number is the explain the caller did not ask for, and the whole answer is
// printed with the refusal.
func OnePlan(t testing.TB, raw []byte) json.RawMessage {
	t.Helper()
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode the explain answer: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain answered %d plans; want one:\n%s", len(plans), raw)
	}
	return plans[0].Plan
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

// Queryer is the one call the catalog reads below need, shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// IndexKeys reads the first key column of each index a plan names, which is the one column an
// index scan narrows on: a scan whose condition names a column its index does not lead with
// reads the whole index and filters it, and carries an "Index Cond" all the same. Each index is
// read from the catalog once and kept, since one test reads the same indexes over and over.
type IndexKeys struct {
	queryer Queryer
	keys    map[string]string
}

// NewIndexKeys reads its indexes through queryer.
func NewIndexKeys(queryer Queryer) *IndexKeys {
	return &IndexKeys{queryer: queryer, keys: map[string]string{}}
}

// FirstKey is the first key column of the index named index, as the index itself declares it: a
// plain column reads `session_id`, and an expression index reads its expression,
// `((ask_id)::text)`.
func (k *IndexKeys) FirstKey(t testing.TB, index string) string {
	t.Helper()
	if definition, ok := k.keys[index]; ok {
		return definition
	}
	var definition string
	statement := `select pg_get_indexdef(c.oid, 1, true) from pg_class c where c.relname = $1`
	if err := k.queryer.QueryRow(context.Background(), statement, index).Scan(&definition); err != nil {
		t.Fatalf("read the first key column of %s: %v", index, err)
	}
	k.keys[index] = definition
	return definition
}

// indexKeyNoise matches what a first key column's definition writes that names no column: a
// string literal, and the type a cast names.
var indexKeyNoise = regexp.MustCompile(`'[^']*'|::"?[a-z_][a-z0-9_]*"?`)

// indexKeyWord matches one lower-case identifier of a key definition or of a plan's Index Cond.
// A keyword Postgres deparses is upper case and so matches nothing, and a word of a longer
// identifier is not one of these: `parent` does not match inside `parent_key`.
var indexKeyWord = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// CondNamesFirstKey reports whether cond, a scan's Index Cond, reads firstKey, its index's first
// key column as FirstKey reads it. The two are compared by the identifiers of the key rather
// than as strings, because the plan writes the same expression its own way: it qualifies each
// column with the query's alias and spaces and casts as it likes, so the names are all the two
// spellings share. Dropping the string literals and cast types of the key definition leaves its
// columns and its lower-case function names, and one of those appearing as a whole word of cond
// is the scan reading the key.
func CondNamesFirstKey(cond, firstKey string) bool {
	words := indexKeyWord.FindAllString(cond, -1)
	for _, identifier := range indexKeyWord.FindAllString(indexKeyNoise.ReplaceAllString(firstKey, " "), -1) {
		if slices.Contains(words, identifier) {
			return true
		}
	}
	return false
}

// AnyIndex is RequireIndexKeyed's index for a plan held to no index in particular: every index
// scan of it is held to the first key column of whatever index it reads, and a plan holding no
// index scan at all is no failure.
const AnyIndex = ""

// CondRule is what RequireIndexKeyed makes of a scan carrying no index condition at all.
type CondRule int

const (
	// CondRequired fails such a scan: it reads the whole of its index.
	CondRequired CondRule = iota
	// CondOptional leaves it to the caller: a read whose index is partial on the read's own
	// filter wants the whole of that index, and a caller that has already named every unkeyed
	// read through ScansWithoutIndexCond would otherwise name it twice.
	CondOptional
)

// RequireIndexKeyed holds every index scan of plan that reads through index to that index's own
// first key column, the one column an index scan narrows on: a scan whose condition names a
// column its index does not lead with reads the whole index and filters it, and carries an
// "Index Cond" all the same. A plan that searches index nowhere is a failure too, since a read
// pinned to an index that no longer reaches it would otherwise pass this check in silence; pass
// AnyIndex to hold each scan to its own index instead and require none. Every failure names
// subject, the read under test, and prints the plan.
func (k *IndexKeys) RequireIndexKeyed(t testing.TB, plan json.RawMessage, index, subject string, rule CondRule) {
	t.Helper()
	searched := false
	for _, scan := range IndexScans(t, plan) {
		if index != AnyIndex && scan.Index != index {
			continue
		}
		searched = true
		if scan.Cond == "" {
			if rule == CondRequired {
				t.Errorf("%s reads all of %s, under no index condition:\n%s", subject, scan.Index, plan)
			}
			continue
		}
		if firstKey := k.FirstKey(t, scan.Index); !CondNamesFirstKey(scan.Cond, firstKey) {
			t.Errorf("%s reads %s on `%s`, which names nothing of that index's first key column `%s`:\n%s",
				subject, scan, scan.Cond, firstKey, plan)
		}
	}
	if index != AnyIndex && !searched {
		t.Errorf("%s searches no %s:\n%s", subject, index, plan)
	}
}

// GenericPlan is the plan Postgres builds for statement without knowing the values it will be
// executed with, the plan a prepared statement settles on. It is the plan at risk: an index
// whose eligibility depends on the bound value is lost there, and a fixture small enough for a
// test never reaches a generic plan on its own, so the mode is forced.
//
// The statement is prepared under name with the parameter types parameters, and EXECUTE takes
// no bind parameter, so literals are written into it. settings are the `set local` statements
// the plan is read under, the caller's own: sequential scans off is every caller's, and a plan
// the planner can hash or merge a whole relation into needs those two off as well. The prepared
// statement outlives the transaction that explains it and is deallocated by name on connection,
// the one connection that holds it.
func GenericPlan(
	t testing.TB,
	ctx context.Context,
	connection *pgxpool.Conn,
	name, statement string,
	parameters, literals []string,
	settings ...string,
) json.RawMessage {
	t.Helper()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s's transaction: %v", name, err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range append(slices.Clone(settings), "set local plan_cache_mode = force_generic_plan") {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	if _, err := tx.Exec(ctx, "prepare "+name+" ("+strings.Join(parameters, ", ")+") as "+statement); err != nil {
		t.Fatalf("prepare %s: %v", name, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) execute "+name+"("+strings.Join(literals, ", ")+")").Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s's transaction: %v", name, err)
	}
	if _, err := connection.Exec(ctx, "deallocate "+name); err != nil {
		t.Fatalf("deallocate %s: %v", name, err)
	}
	return OnePlan(t, raw)
}
