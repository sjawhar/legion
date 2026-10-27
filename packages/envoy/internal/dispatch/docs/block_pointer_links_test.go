package docs

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func claimPointing(id, serves, body string) string {
	return ":::claim{#" + id + " serves=\"" + serves + "\"}\n" + body + "\n:::\n"
}

// writeIndexedDocument creates document slug of CORE-1 holding markdown as its version 1, written
// and indexed in one transaction as an upload of a new document is, and returns its uuid.
func writeIndexedDocument(t *testing.T, database *store.Store, service *Service, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		written, err := service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, err = service.IndexDocumentReferences(ctx, id, 1, written)
		return err
	}); err != nil {
		t.Fatalf("write document %s: %v", slug, err)
	}
	return id
}

// editIndexedDocument applies ops to the document and writes the version they leave, as an agent's
// edit is.
func editIndexedDocument(t *testing.T, service *Service, artifactID string, ops ...model.EditOp) {
	t.Helper()
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ApplyOps(ctx, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
			return err
		}
		result, err := service.SnapshotVersion(ctx, artifactID, model.Actor{Kind: "user", ID: "alice"})
		if err == nil && !result.Wrote {
			t.Errorf("the edit of %s wrote no version", artifactID)
		}
		return err
	}); err != nil {
		t.Fatalf("edit %s: %v", artifactID, err)
	}
}

// requireLinksTo fails unless the graph lists exactly want ("<kind> <source node id>") pointing at
// the block node nodeID.
func requireLinksTo(t *testing.T, database *store.Store, stage, nodeID string, want ...string) {
	t.Helper()
	edges, err := refs.Edges(context.Background(), database.Pool, refs.Query{Direction: "in", Kind: "block", ID: nodeID})
	if err != nil {
		t.Fatalf("%s: read the links to %s: %v", stage, nodeID, err)
	}
	got := make([]string, len(edges))
	for index, edge := range edges {
		got[index] = edge.Kind + " " + edge.Node.ID
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: links to %s = %q, want %q", stage, nodeID, got, want)
	}
}

// pointerRow is one block_refs row, every column a test compares.
type pointerRow struct {
	BlockID, Kind, ToKind, ToID string
	ToTypes                     []string
	CreatedVersion              int
	CreatedAt                   time.Time
	SourceSeq                   *int64
}

func pointerRowsOf(t *testing.T, database *store.Store, artifactID string) []pointerRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, kind, to_kind, to_id, to_types, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, kind, to_kind, to_id
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pointerRow, error) {
		var pointer pointerRow
		err := row.Scan(&pointer.BlockID, &pointer.Kind, &pointer.ToKind, &pointer.ToID, &pointer.ToTypes, &pointer.CreatedVersion, &pointer.CreatedAt, &pointer.SourceSeq)
		return pointer, err
	})
	if err != nil {
		t.Fatalf("scan block_refs: %v", err)
	}
	return found
}

// A change of an attribute's targets reaches the pointers a document already stores at its next
// indexing: a version of the citing document that changes nothing about the pointer lists the edge
// the new targets allow.
func TestATargetsChangeReachesStoredPointersAtTheNextIndexing(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	target := writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA requirement.\n:::\n")
	checks := writeIndexedDocument(t, database, service, "checks", claimPointing("c1", "dispatch://CORE-1/artifact/target#b-r1", "It holds."))
	requireLinksTo(t, database, "pointing at a requirement", "CORE-1/target#r1", "serves CORE-1/checks#c1")

	editIndexedDocument(t, service, target, model.EditOp{Op: "retype", Block: "r1", Type: "callout"})
	requireLinksTo(t, database, "r1 retyped to a callout", "CORE-1/target#r1")

	// No room reads the schema across the swap.
	closeDocumentRoom(t, service, target)
	closeDocumentRoom(t, service, checks)
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(map[string]pmdoc.BlockAttributeSchema{
		"serves": {Kind: "block[]", Targets: []string{"requirement", "callout"}},
	}))
	requireLinksTo(t, database, "serves allowing callouts, checks.md not yet indexed", "CORE-1/target#r1")

	editIndexedDocument(t, service, checks, model.EditOp{Op: "insert", After: "end", Markdown: "Unrelated.\n"})
	requireLinksTo(t, database, "checks.md indexed under the new targets", "CORE-1/target#r1", "serves CORE-1/checks#c1")
	closeDocumentRoom(t, service, checks)
}

// A browser's write is never refused: a settled browser tree holding a pointer to a block of a type
// the attribute does not allow writes its version and stores the pointer, which the graph does not
// list until its target has an allowed type.
func TestABrowserWrittenPointerToADisallowedTypeIsStoredAndNotListed(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	target := writeIndexedDocument(t, database, service, "target", ":::callout{#k1 kind=\"note\" title=\"\"}\nA callout.\n:::\n")
	checks := writeIndexedDocument(t, database, service, "checks", "First.\n")

	browserReplaceText(t, service, checks, claimPointing("c1", "dispatch://CORE-1/artifact/target#b-k1", "It holds."), model.Actor{Kind: "user", ID: "bob"})
	waitForLandedAppends(t, service, checks)
	state := service.room(checks)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(checks, generation)

	var versions int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, checks).Scan(&versions); err != nil {
		t.Fatalf("count the versions of checks.md: %v", err)
	}
	state.mu.Lock()
	failed, failures := state.failed, state.settleFailures
	state.mu.Unlock()
	if versions != 2 || failed != nil || failures != 0 {
		t.Fatalf("settlement wrote %d versions (failed %v, %d failures), want version 2 and no failure", versions, failed, failures)
	}
	var stored []pointerRow
	for _, row := range pointerRowsOf(t, database, checks) {
		stored = append(stored, pointerRow{BlockID: row.BlockID, Kind: row.Kind, ToKind: row.ToKind, ToID: row.ToID, ToTypes: row.ToTypes, CreatedVersion: row.CreatedVersion})
	}
	if want := []pointerRow{{BlockID: "c1", Kind: "serves", ToKind: "block", ToID: "CORE-1/target#k1", ToTypes: []string{"requirement"}, CreatedVersion: 2}}; !reflect.DeepEqual(stored, want) {
		t.Fatalf("block_refs of checks.md = %#v, want %#v", stored, want)
	}
	requireLinksTo(t, database, "pointing at a callout", "CORE-1/target#k1")

	editIndexedDocument(t, service, target, model.EditOp{Op: "retype", Block: "k1", Type: "requirement"})
	requireLinksTo(t, database, "k1 retyped to a requirement", "CORE-1/target#k1", "serves CORE-1/checks#c1")
}

// The reconciliation indexes a document's pointers as its version write did: re-indexing the same
// version leaves every block_refs row as it was, the pointer's targets included.
func TestTheReconciliationIndexesTheSamePointersAsTheVersionWrite(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA requirement.\n:::\n")
	const r1 = "dispatch://CORE-1/artifact/target#b-r1"
	checks := writeIndexedDocument(t, database, service, "checks", claimPointing("c1", r1, "It holds, per "+r1+".")+"\nSee dispatch://CORE-2.\n")
	paragraph := holderOf(t, service, checks, "It holds, per")
	other := holderOf(t, service, checks, "See dispatch://CORE-2")

	written := pointerRowsOf(t, database, checks)
	kinds := make([]string, len(written))
	for index, row := range written {
		toTypes := "null"
		if row.ToTypes != nil {
			toTypes = fmt.Sprint(row.ToTypes)
		}
		kinds[index] = row.Kind + " " + row.BlockID + " " + row.ToID + " " + toTypes
	}
	want := []string{
		"serves c1 CORE-1/target#r1 [requirement]",
		"mentions " + paragraph + " CORE-1/target#r1 null",
		"mentions " + other + " CORE-2 null",
	}
	sort.Strings(kinds)
	sort.Strings(want)
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("block_refs the version write stored = %q, want %q", kinds, want)
	}

	if _, err := database.Pool.Exec(context.Background(), `update document_block_versions set version = null where artifact_id = $1`, checks); err != nil {
		t.Fatalf("mark checks.md owed: %v", err)
	}
	settle(t, database, checks)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("block index tick = %#v, want checks.md indexed", tick)
	}
	if at := blockIndexAt(t, database, checks); at == nil || *at != 1 {
		t.Fatalf("checks.md indexed at %v, want version 1", at)
	}
	if again := pointerRowsOf(t, database, checks); !reflect.DeepEqual(again, written) {
		t.Fatalf("block_refs after the reconciliation = %#v, want %#v", again, written)
	}
}
