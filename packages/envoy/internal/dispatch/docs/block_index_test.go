package docs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockIndexService is a document service whose documents never settle during the test, on a
// database holding project CORE and its issues CORE-1, where the documents live, and CORE-2, which
// they cite.
func blockIndexService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	return database, service
}

// createOwedDocument creates document slug of CORE-1, id its uuid (empty: one the database draws), whose
// live text is markdown and whose version 1 stores stored (empty: the live text), as a build
// without the block index writes it: no block index, so the document is owed one. Its seed leaves
// it owing a settlement, as an unsettled document does.
func createOwedDocument(t *testing.T, database *store.Store, service *Service, id, slug, markdown, stored string) string {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values (coalesce(nullif($1, '')::uuid, gen_random_uuid()), 'CORE-1', 'CORE', $2, $2 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, id, slug).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	seedServiceText(t, service, id, markdown)
	if stored == "" {
		live, _, err := service.TextWithBlocks(ctx, id)
		if err != nil {
			t.Fatalf("read document %s: %v", slug, err)
		}
		stored = live
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, id, stored); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	return id
}

// settle clears the document's pending settlement, as the settlement that wrote its latest
// version does.
func settle(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("settle %s: %v", artifactID, err)
	}
}

// blockIndexAt is the version the document's blocks are indexed at; nil while owed.
func blockIndexAt(t *testing.T, database *store.Store, artifactID string) *int {
	t.Helper()
	var version *int
	if err := database.Pool.QueryRow(context.Background(),
		`select version from document_block_versions where artifact_id = $1`, artifactID).Scan(&version); err != nil {
		t.Fatalf("read the block index of %s: %v", artifactID, err)
	}
	return version
}

// reconcileBlockIndex runs one tick of a loop that has found no document live ahead before.
func reconcileBlockIndex(t *testing.T, service *Service, cursor string) BlockIndexTick {
	t.Helper()
	tick, err := service.ReconcileBlockIndex(context.Background(), cursor, liveAheadDocuments{})
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	return tick
}

// holderOf is the id of the innermost block of the document's live text holding reference.
func holderOf(t *testing.T, service *Service, artifactID, reference string) string {
	t.Helper()
	markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read %s: %v", artifactID, err)
	}
	offset := strings.Index(markdown, reference)
	holder := -1
	for index, block := range blocks {
		if block.From <= offset && offset < block.To && (holder < 0 || block.To-block.From <= blocks[holder].To-blocks[holder].From) {
			holder = index
		}
	}
	if offset < 0 || holder < 0 {
		t.Fatalf("no block of %s holds %s: %q %#v", artifactID, reference, markdown, blocks)
	}
	return blocks[holder].ID
}

type blockPair struct {
	BlockID, ToKind, ToID string
	CreatedVersion        int
	CreatedAt             time.Time
	SourceSeq             *int64
}

func blockPairsOf(t *testing.T, database *store.Store, artifactID string) []blockPair {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, to_kind, to_id, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, to_kind, to_id
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (blockPair, error) {
		var pair blockPair
		err := row.Scan(&pair.BlockID, &pair.ToKind, &pair.ToID, &pair.CreatedVersion, &pair.CreatedAt, &pair.SourceSeq)
		return pair, err
	})
	if err != nil {
		t.Fatalf("scan block_refs: %v", err)
	}
	return pairs
}

type refRow struct {
	ToKind, ToID string
	CreatedAt    time.Time
	SourceSeq    *int64
}

func refsFrom(t *testing.T, database *store.Store, fromKind, fromID string) []refRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_kind, to_id, created_at, source_seq from refs
		where from_kind = $1 and from_id = $2 order by to_kind, to_id
	`, fromKind, fromID)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (refRow, error) {
		var ref refRow
		err := row.Scan(&ref.ToKind, &ref.ToID, &ref.CreatedAt, &ref.SourceSeq)
		return ref, err
	})
	if err != nil {
		t.Fatalf("scan refs: %v", err)
	}
	return found
}

// shipItSpec is a spec whose callout ship_it holds a paragraph citing CORE-2, beside another
// paragraph.
const shipItSpec = ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it after dispatch://CORE-2.\n:::\n\nOther paragraph.\n"

// An owed document is indexed whole by one tick: its blocks with their texts at its latest
// version, the mention a block holds, dated from the document's own reference to that target on
// this first indexing, the mention's excerpt, and the version recorded indexed.
func TestBlockIndexIndexesAnOwedDocument(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", shipItSpec, "")
	settle(t, database, spec)
	// The document's own reference, as the build that wrote the version indexed it.
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq) values ('artifact', $1, 'issue', 'CORE-2', $2, 7)
	`, spec, written); err != nil {
		t.Fatalf("write the document's reference: %v", err)
	}
	markdown, blocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 || tick.Full || tick.Cursor != "" {
		t.Fatalf("tick = %#v, want the one document indexed and nothing owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	var recorded bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_indexed_versions where artifact_id = $1 and version = 1)
	`, spec).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("version 1 recorded indexed = %v (%v)", recorded, err)
	}
	if len(blocks) < 2 {
		t.Fatalf("the spec has blocks %#v, want the callout and its paragraphs", blocks)
	}
	for _, block := range blocks {
		var changed int
		var text string
		if err := database.Pool.QueryRow(ctx, `
			select d.changed_version, t.text from document_blocks d
			join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 1
			where d.artifact_id = $1 and d.block_id = $2
		`, spec, block.ID).Scan(&changed, &text); err != nil {
			t.Fatalf("block %s is not indexed: %v", block.ID, err)
		}
		if changed != 1 || text != markdown[block.From:block.To] {
			t.Fatalf("block %s changed at %d with text %q, want version 1 and %q", block.ID, changed, text, markdown[block.From:block.To])
		}
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	seq := int64(7)
	want := []blockPair{{BlockID: holder, ToKind: "issue", ToID: "CORE-2", CreatedVersion: 1, CreatedAt: written, SourceSeq: &seq}}
	if pairs := blockPairsOf(t, database, spec); len(pairs) != 1 || pairs[0].BlockID != holder || pairs[0].ToID != "CORE-2" ||
		!pairs[0].CreatedAt.Equal(written) || pairs[0].SourceSeq == nil || *pairs[0].SourceSeq != seq {
		t.Fatalf("block_refs = %#v, want %#v", pairs, want)
	}
	var excerptVersion *int
	var excerptBlock *string
	if err := database.Pool.QueryRow(ctx, `
		select excerpt_version, excerpt_block_id from refs where from_kind = 'artifact' and from_id = $1
	`, spec).Scan(&excerptVersion, &excerptBlock); err != nil {
		t.Fatalf("read the mention's excerpt: %v", err)
	}
	if excerptVersion == nil || *excerptVersion != 1 || excerptBlock == nil || *excerptBlock != holder {
		t.Fatalf("the mention's excerpt is block %v at version %v, want block %s at version 1", excerptBlock, excerptVersion, holder)
	}
}

// A document owed a settlement whose live text differs from its latest version is ahead of it:
// indexing the live text at that version would date the later edit's blocks to it. It is skipped,
// counted, and left owed.
func TestBlockIndexSkipsADocumentWhoseLiveTextIsAheadOfItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "", "spec", shipItSpec, "Ship it.\n")

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 0 || tick.LiveAhead != 1 || tick.OwedDocuments != 1 {
		t.Fatalf("tick = %#v, want the document skipped as live ahead and still owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version != nil {
		t.Fatalf("blocks indexed at %d, want the document still owed", *version)
	}
	var blocks, indexed int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_blocks where artifact_id = $1),
		       (select count(*) from document_indexed_versions where artifact_id = $1)
	`, spec).Scan(&blocks, &indexed); err != nil {
		t.Fatal(err)
	}
	if blocks != 0 || indexed != 0 {
		t.Fatalf("a skipped document has %d blocks and %d indexed versions, want none", blocks, indexed)
	}
}

// A closed issue's settlement never resumes, so its document can owe one for good. When its live
// text is its latest version's, byte for byte, the index is exact and the document is indexed.
func TestBlockIndexIndexesAClosedDocumentWhoseLiveTextEqualsItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "", "spec", shipItSpec, "")
	var pending bool
	if err := database.Pool.QueryRow(context.Background(), `
		update issues set closed_at = now() where key = 'CORE-1'
		returning exists (select 1 from doc_settlements_pending where artifact_id = $1)
	`, spec).Scan(&pending); err != nil || !pending {
		t.Fatalf("close CORE-1 with its document owing a settlement: pending %v (%v)", pending, err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	if pairs := blockPairsOf(t, database, spec); len(pairs) != 1 || pairs[0].BlockID != holder {
		t.Fatalf("block_refs = %#v, want the mention from block %s", pairs, holder)
	}
}

// closedSpec is a spec whose paragraph cites CORE-2, beside an ask block no settlement has indexed.
const closedSpec = "Ship it after dispatch://CORE-2.\n\n:::ask{#region urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"

// An issue's close, its final sweep and the repair write ask blocks' states into a closed issue's
// documents and write no version, and no settlement runs while the issue is closed, so such a
// document stays live ahead of its latest version until the issue reopens. A deploy that marks
// every document owed (migration 0121), or a rebuild-refs, must still index it: it is indexed at
// its latest version from its live text, which every reader of the closed issue sees, with each
// block under the id the live document gives it. Every block of it, an untyped one included, then
// resolves by its #b- reference.
func TestBlockIndexIndexesAClosedDocumentWhoseAskStatesAreAheadOfItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", closedSpec, "")
	var stored string
	if err := database.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = 1`, spec).Scan(&stored); err != nil {
		t.Fatalf("read version 1: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	// The close's final sweep, as the repair runs it, under the issue's row: it writes resolved
	// the ask block no settlement has indexed.
	var marked []BlockRef
	if err := joinedWrite(service, func(ctx context.Context) (err error) {
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update`); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		marked, err = service.WriteIssueAskStates(ctx, "CORE-1", nil, model.Actor{Kind: "user", ID: "carol"}, true)
		return err
	}); err != nil {
		t.Fatalf("write the closed issue's ask states: %v", err)
	}
	if want := []BlockRef{{ArtifactID: spec, BlockID: "region"}}; !reflect.DeepEqual(marked, want) {
		t.Fatalf("the sweep marked %+v, want %+v", marked, want)
	}
	closeDocumentRoom(t, service, spec)
	live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var pending bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, spec).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if live == stored || !strings.Contains(live, `state="resolved"`) || !pending {
		t.Fatalf("the sweep left live text %q (settlement pending %v), want it ahead of version 1 %q", live, pending, stored)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	rows, err := database.Pool.Query(ctx, `
		select d.block_id, t.text from document_blocks d
		join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 1
		where d.artifact_id = $1 order by d.block_id
	`, spec)
	if err != nil {
		t.Fatalf("read the indexed blocks: %v", err)
	}
	texts := map[string]string{}
	for rows.Next() {
		var id, blockText string
		if err := rows.Scan(&id, &blockText); err != nil {
			t.Fatalf("scan an indexed block: %v", err)
		}
		texts[id] = blockText
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the indexed blocks: %v", err)
	}
	if len(texts) != len(liveBlocks) {
		t.Fatalf("indexed blocks %v, want the live document's %#v", texts, liveBlocks)
	}
	for _, block := range liveBlocks {
		blockText, indexed := texts[block.ID]
		if !indexed || !strings.Contains(stored, blockText) {
			t.Fatalf("block %s is indexed %v with text %q, want it indexed with its text in version 1 %q", block.ID, indexed, blockText, stored)
		}
	}
	if strings.Contains(texts["region"], `state="resolved"`) {
		t.Fatalf("the ask block is indexed at version 1 with the sweep's state: %q", texts["region"])
	}
	document, found, err := refs.Node(ctx, database.Pool, "artifact", spec, false)
	if err != nil || !found {
		t.Fatalf("resolve the spec: found %v (%v)", found, err)
	}
	for _, id := range []string{holderOf(t, service, spec, "dispatch://CORE-2"), "region"} {
		reference := refs.BlockRef(document.Ref, id)
		ref, ok := text.ParseRef(reference, "https://dispatch.example")
		if !ok {
			t.Fatalf("%s is no reference", reference)
		}
		node, found, err := refs.Node(ctx, database.Pool, "block", text.TargetID(ref), true)
		if err != nil || !found || node.BlockID != id {
			t.Fatalf("%s resolves to %#v, found %v (%v), want block %s", reference, node, found, err, id)
		}
	}
}

// A closed issue's document can also be live ahead of its latest version by a real edit: one the
// close could not settle (the close goes on when its settlement fails), or one a build older than
// the pre-close settlement left unsettled when it closed the issue. No settlement runs while the
// issue is closed, so the block index must not wait for one: the document is indexed at its latest
// version, nothing stays owed, and a block whose text that version holds resolves by its #b-
// reference under the id the live document gives it.
func TestBlockIndexIndexesAClosedDocumentWithAnUnsettledEdit(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	stored := "Ship it after dispatch://CORE-2.\n"
	spec := createOwedDocument(t, database, service, "", "spec", stored+"\nUnsettled typed line.\n", stored)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	live, _, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	if !strings.Contains(live, "Unsettled typed line.") {
		t.Fatalf("live text %q, want the unsettled edit ahead of version 1 %q", live, stored)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	document, found, err := refs.Node(ctx, database.Pool, "artifact", spec, false)
	if err != nil || !found {
		t.Fatalf("resolve the spec: found %v (%v)", found, err)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	reference := refs.BlockRef(document.Ref, holder)
	ref, ok := text.ParseRef(reference, "https://dispatch.example")
	if !ok {
		t.Fatalf("%s is no reference", reference)
	}
	node, found, err := refs.Node(ctx, database.Pool, "block", text.TargetID(ref), true)
	if err != nil || !found || node.BlockID != holder {
		t.Fatalf("%s resolves to %#v, found %v (%v), want block %s", reference, node, found, err, holder)
	}
}

// A closed issue's document indexed while its live text was ahead of its latest version must not
// keep a history that no version holds once the issue reopens. The reopen's settlement writes the
// edit the close could not settle into version 2. The block that edit made is then dated to
// version 2, the first version that holds it. Every text recorded at version 1 is text version 1
// holds, so a reference pinned to version 1 never shows a block or a text that version 1 does not
// have.
func TestTheReopensSettlementDatesAClosedDocumentsUnsettledEditToItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	stored := "Ship it after dispatch://CORE-2.\n"
	spec := createOwedDocument(t, database, service, "", "spec", stored+"\nUnsettled typed line.\n", stored)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	reconcileBlockIndex(t, service, "")
	live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var typed string
	for _, block := range liveBlocks {
		if strings.Contains(live[block.From:block.To], "Unsettled typed line.") {
			typed = block.ID
		}
	}
	if typed == "" {
		t.Fatalf("no live block holds the unsettled edit: %q %#v", live, liveBlocks)
	}

	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = null where key = 'CORE-1'`); err != nil {
		t.Fatalf("reopen CORE-1: %v", err)
	}
	if err := service.SettleIssueDocuments(ctx, "CORE-1"); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	var latest int
	var second string
	if err := database.Pool.QueryRow(ctx, `
		select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, spec).Scan(&latest, &second); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	if latest != 2 || !strings.Contains(second, "Unsettled typed line.") {
		t.Fatalf("latest version %d %q, want version 2 holding the edit", latest, second)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 2 {
		t.Fatalf("blocks indexed at %v, want version 2", version)
	}

	var changed int
	if err := database.Pool.QueryRow(ctx, `
		select changed_version from document_blocks where artifact_id = $1 and block_id = $2
	`, spec, typed).Scan(&changed); err != nil {
		t.Fatalf("read the typed block: %v", err)
	}
	if changed != 2 {
		t.Fatalf("the block the unsettled edit made is dated version %d, want 2, the first version that holds it", changed)
	}
	rows, err := database.Pool.Query(ctx, `
		select block_id, text from document_block_texts where artifact_id = $1 and version = 1
	`, spec)
	if err != nil {
		t.Fatalf("read the texts recorded at version 1: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, blockText string
		if err := rows.Scan(&id, &blockText); err != nil {
			t.Fatalf("scan a text recorded at version 1: %v", err)
		}
		if !strings.Contains(stored, blockText) {
			t.Fatalf("block %s is recorded at version 1 with text %q, which version 1 %q does not hold", id, blockText, stored)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the texts recorded at version 1: %v", err)
	}
}

// A block of a closed issue's document that an unsettled edit changed keeps the history version 1
// gives it. Version 1 holds two untyped paragraphs, indexed while the issue was open and its live
// text was version 1's. The live text then edits the plain paragraph, the same block with new
// text, the issue closes with the edit unsettled, and a rebuild-refs marks the document owed. The
// closed tick indexes version 1's own text: the unedited paragraph keeps its row under its live
// id, and the edited one, whose live text version 1 does not hold, keeps its row under the id of
// the stored row that holds version 1's text of it, so it stays dated version 1 with version 1's
// text and is recorded deleted at no version that holds it, and no text at version 1 holds the
// edit. The reopen's settlement writes the edit into version 2, which dates the edited block and
// records its text, and its text at version 1 stays version 1's.
func TestAClosedDocumentsUnsettledEditOfAnUntypedBlockIsDatedToTheVersionThatHoldsIt(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", "Ship it after dispatch://CORE-2.\n\nPlain paragraph.\n", "")
	var stored string
	if err := database.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = 1`, spec).Scan(&stored); err != nil {
		t.Fatalf("read version 1: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("indexing version 1 while CORE-1 is open: tick = %#v", tick)
	}
	cited := holderOf(t, service, spec, "dispatch://CORE-2")
	edited := holderOf(t, service, spec, "Plain paragraph.")
	var firstChanged int
	var firstText string
	if err := database.Pool.QueryRow(ctx, `
		select d.changed_version, t.text from document_blocks d
		join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 1
		where d.artifact_id = $1 and d.block_id = $2
	`, spec, edited).Scan(&firstChanged, &firstText); err != nil || firstChanged != 1 || !strings.Contains(firstText, "Plain paragraph.") {
		t.Fatalf("the plain paragraph %s is indexed at %d with text %q (%v), want version 1's text", edited, firstChanged, firstText, err)
	}

	replaced := false
	var replace func(node *pmdoc.Node)
	replace = func(node *pmdoc.Node) {
		if node.Type == "text" && strings.Contains(node.Text, "Plain paragraph.") {
			node.Text, replaced = strings.Replace(node.Text, "Plain paragraph.", "Edited paragraph.", 1), true
		}
		for _, child := range node.Children {
			replace(child)
		}
	}
	editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
		replace(tree)
		return tree
	})
	if !replaced {
		t.Fatalf("no text of %s holds the plain paragraph", spec)
	}
	waitForLandedAppends(t, service, spec)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	// A rebuild-refs marks the document owed.
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, spec); err != nil {
		t.Fatalf("mark the document owed: %v", err)
	}
	live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var editedText string
	for _, block := range liveBlocks {
		if block.ID == edited {
			editedText = live[block.From:block.To]
		}
	}
	var pending bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, spec).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(editedText, "Edited paragraph.") || strings.Contains(stored, "Edited paragraph.") || !pending {
		t.Fatalf("live text %q gives block %s the text %q (settlement pending %v), want the same block edited ahead of version 1 %q", live, edited, editedText, pending, stored)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	document, found, err := refs.Node(ctx, database.Pool, "artifact", spec, false)
	if err != nil || !found {
		t.Fatalf("resolve the spec: found %v (%v)", found, err)
	}
	resolve := func(id string) {
		t.Helper()
		reference := refs.BlockRef(document.Ref, id)
		ref, ok := text.ParseRef(reference, "https://dispatch.example")
		if !ok {
			t.Fatalf("%s is no reference", reference)
		}
		node, found, err := refs.Node(ctx, database.Pool, "block", text.TargetID(ref), true)
		if err != nil || !found || node.BlockID != id {
			t.Fatalf("%s resolves to %#v, found %v (%v), want block %s", reference, node, found, err, id)
		}
	}
	resolve(cited)
	var keptChanged int
	var keptText string
	var keptDeleted bool
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select d.changed_version from document_blocks d where d.artifact_id = t.artifact_id and d.block_id = t.block_id), 0),
		       coalesce(t.text, ''), t.text is null
		from document_block_texts t
		where t.artifact_id = $1 and t.block_id = $2 and t.version = 1
	`, spec, edited).Scan(&keptChanged, &keptText, &keptDeleted); err != nil {
		t.Fatalf("read the edited block %s at version 1 after the closed tick: %v", edited, err)
	}
	if keptChanged != 1 || keptDeleted || keptText != firstText {
		t.Fatalf("the edited block %s is dated version %d with text %q (deleted %v) at version 1, want version 1 with version 1's text %q", edited, keptChanged, keptText, keptDeleted, firstText)
	}
	resolve(edited)
	rows, err := database.Pool.Query(ctx, `
		select block_id, text from document_block_texts where artifact_id = $1 and version = 1 and text is not null
	`, spec)
	if err != nil {
		t.Fatalf("read the texts recorded at version 1: %v", err)
	}
	versionTexts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var recorded [2]string
		err := row.Scan(&recorded[0], &recorded[1])
		return recorded, err
	})
	if err != nil {
		t.Fatalf("scan the texts recorded at version 1: %v", err)
	}
	for _, recorded := range versionTexts {
		if !strings.Contains(stored, recorded[1]) {
			t.Fatalf("block %s is recorded at version 1 with text %q, which version 1 %q does not hold", recorded[0], recorded[1], stored)
		}
	}

	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = null where key = 'CORE-1'`); err != nil {
		t.Fatalf("reopen CORE-1: %v", err)
	}
	if err := service.SettleIssueDocuments(ctx, "CORE-1"); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	var latest int
	var second string
	if err := database.Pool.QueryRow(ctx, `
		select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, spec).Scan(&latest, &second); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	if latest != 2 || !strings.Contains(second, "Edited paragraph.") {
		t.Fatalf("latest version %d %q, want version 2 holding the edit", latest, second)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 2 {
		t.Fatalf("blocks indexed at %v, want version 2", version)
	}
	resolve(edited)
	var changed int
	var secondText string
	if err := database.Pool.QueryRow(ctx, `
		select d.changed_version, t.text from document_blocks d
		join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 2
		where d.artifact_id = $1 and d.block_id = $2
	`, spec, edited).Scan(&changed, &secondText); err != nil {
		t.Fatalf("read the edited block at version 2: %v", err)
	}
	if changed != 2 || secondText != editedText {
		t.Fatalf("the edited block %s is dated version %d with text %q, want version 2 with the edited text %q", edited, changed, secondText, editedText)
	}
	var reopenedText string
	var reopenedDeleted bool
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(text, ''), text is null from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1
	`, spec, edited).Scan(&reopenedText, &reopenedDeleted); err != nil || reopenedDeleted || reopenedText != firstText {
		t.Fatalf("the edited block %s has text %q (deleted %v) at version 1 after the reopen (%v), want version 1's text %q", edited, reopenedText, reopenedDeleted, err, firstText)
	}
}

// Identical paragraphs keep their own ids and dates on a closed issue. Version 1 holds three
// "Same." paragraphs and an "Other." one, indexed while the issue was open. An unsettled edit then
// inserts a new "Same." paragraph after "Other." (before the third "Same."), the issue closes, and
// a rebuild-refs marks the document owed. The closed tick indexes version 1's own text: each block
// version 1 holds keeps its own id, date and text, none is recorded deleted at version 1, and the
// inserted paragraph, which no version holds, gets no text at version 1. The reopen's settlement
// writes version 2, which dates the inserted paragraph, and the third "Same." stays dated version 1.
func TestAClosedDocumentsIdenticalParagraphsKeepTheirOwnHistory(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", "Same.\n\nSame.\n\nOther.\n\nSame.\n", "")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("indexing version 1 while CORE-1 is open: tick = %#v", tick)
	}
	_, firstBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var held []string
	for _, block := range firstBlocks {
		if block.Type == "paragraph" {
			held = append(held, block.ID)
		}
	}
	if len(held) != 4 {
		t.Fatalf("version 1's paragraphs %#v, want 4", firstBlocks)
	}
	const inserted = "5a3e0c1d-7b2f-4e6a-9c8d-1f2e3d4c5b6a"
	editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
		for index, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id == held[2] {
				paragraph := &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: inserted}, Children: []*pmdoc.Node{{Type: "text", Text: "Same."}}}
				tree.Children = slices.Insert(tree.Children, index+1, paragraph)
				return tree
			}
		}
		t.Fatalf("no paragraph %s in the live tree", held[2])
		return tree
	})
	waitForLandedAppends(t, service, spec)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, spec); err != nil {
		t.Fatalf("mark the document owed: %v", err)
	}
	_, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var order []string
	for _, block := range liveBlocks {
		if block.Type == "paragraph" {
			order = append(order, block.ID)
		}
	}
	if want := []string{held[0], held[1], held[2], inserted, held[3]}; !slices.Equal(order, want) {
		t.Fatalf("live paragraphs %v, want %v", order, want)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
	}
	dated := func(id string, version int) (int, bool, bool) {
		t.Helper()
		var changed int
		var recorded, deleted bool
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select changed_version from document_blocks where artifact_id = $1 and block_id = $2), 0),
			       exists (select 1 from document_block_texts where artifact_id = $1 and block_id = $2 and version = $3 and text is not null),
			       exists (select 1 from document_block_texts where artifact_id = $1 and block_id = $2 and version = $3 and text is null)
		`, spec, id, version).Scan(&changed, &recorded, &deleted); err != nil {
			t.Fatalf("read block %s at version %d: %v", id, version, err)
		}
		return changed, recorded, deleted
	}
	for _, id := range held {
		if changed, recorded, deleted := dated(id, 1); changed != 1 || !recorded || deleted {
			t.Fatalf("after the closed tick, block %s that version 1 holds is dated %d (text at version 1 %v, deleted at version 1 %v), want dated 1 with its text", id, changed, recorded, deleted)
		}
	}
	if _, recorded, _ := dated(inserted, 1); recorded {
		t.Fatalf("after the closed tick, the inserted paragraph %s, which no version holds, has a text at version 1", inserted)
	}

	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = null where key = 'CORE-1'`); err != nil {
		t.Fatalf("reopen CORE-1: %v", err)
	}
	if err := service.SettleIssueDocuments(ctx, "CORE-1"); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 2 {
		t.Fatalf("blocks indexed at %v, want version 2", version)
	}
	if changed, _, deleted := dated(held[3], 1); changed != 1 || deleted {
		t.Fatalf("after the reopen, the third \"Same.\" %s is dated %d (deleted at version 1 %v), want version 1, the first that holds it", held[3], changed, deleted)
	}
	if changed, recorded, _ := dated(inserted, 1); changed != 2 || recorded {
		t.Fatalf("after the reopen, the inserted paragraph %s is dated %d (text at version 1 %v), want version 2, the first that holds it", inserted, changed, recorded)
	}
}

// With no row to name a block (a document none of whose blocks the index holds yet, as in the
// deploy order), a text held more than once is matched in order only when the version and the
// live document hold the same number of copies of it. Otherwise no copy of it is matched until a
// settlement. In the spec, version 1 holds "Same." three times; the live document holds it four
// times, a copy typed after the close sitting before the version's third. No copy of "Same." is
// indexed at version 1 while the issue is closed, and the typed copy is never dated version 1,
// also after the reopen. (An original copy left unmatched may then be dated to the reopen's
// version, as #2210 dates any block it first indexes after the deploy: that is the accepted limit.)
// The notes are the control: version 1 holds "Same." three times, unedited, and a paragraph typed
// after the close is all that sets the live text apart, so every copy resolves at version 1.
func TestAClosedDocumentsRepeatedTextWithNoRowIsMatchedOnlyWhenItsCountsAgree(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	stored := "Same.\n\nSame.\n\nOther.\n\nSame.\n"
	spec := createOwedDocument(t, database, service, "", "spec", "Same.\n\nSame.\n\nOther.\n\nSame.\n\nSame.\n", stored)
	notes := createOwedDocument(t, database, service, "", "notes", stored+"\nTyped after the close.\n", stored)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	closeDocumentRoom(t, service, notes)
	type paragraph struct{ ID, Text string }
	paragraphs := func(artifactID string) []paragraph {
		t.Helper()
		live, blocks, err := service.TextWithBlocks(ctx, artifactID)
		if err != nil {
			t.Fatalf("read the live document %s: %v", artifactID, err)
		}
		var held []paragraph
		for _, block := range blocks {
			if block.Type == "paragraph" {
				held = append(held, paragraph{ID: block.ID, Text: strings.TrimSpace(live[block.From:block.To])})
			}
		}
		return held
	}
	specParagraphs := paragraphs(spec)
	if len(specParagraphs) != 5 || specParagraphs[3].Text != "Same." {
		t.Fatalf("live spec paragraphs %#v, want Same., Same., Other., the typed Same., Same.", specParagraphs)
	}
	typed := specParagraphs[3].ID
	dated := func(artifactID, id string) (int, bool) {
		t.Helper()
		var changed int
		var recorded bool
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select changed_version from document_blocks where artifact_id = $1 and block_id = $2), 0),
			       exists (select 1 from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1 and text is not null)
		`, artifactID, id).Scan(&changed, &recorded); err != nil {
			t.Fatalf("read block %s: %v", id, err)
		}
		return changed, recorded
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 2 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want both closed documents indexed and nothing owed", tick)
	}
	for _, block := range specParagraphs {
		changed, recorded := dated(spec, block.ID)
		if block.Text == "Same." && (changed == 1 || recorded) {
			t.Fatalf("spec paragraph %s %q is dated %d (text at version 1 %v) while closed; the version holds \"Same.\" 3 times and the live document 4, so no copy may be matched", block.ID, block.Text, changed, recorded)
		}
		if block.Text == "Other." && (changed != 1 || !recorded) {
			t.Fatalf("spec paragraph %s %q is dated %d (text at version 1 %v), want version 1", block.ID, block.Text, changed, recorded)
		}
	}
	for _, block := range paragraphs(notes) {
		changed, recorded := dated(notes, block.ID)
		if block.Text == "Typed after the close." {
			if changed == 1 || recorded {
				t.Fatalf("the notes' typed paragraph %s is dated %d (text at version 1 %v), want no history at version 1", block.ID, changed, recorded)
			}
			continue
		}
		if changed != 1 || !recorded {
			t.Fatalf("notes paragraph %s %q is dated %d (text at version 1 %v); the counts agree, so it must resolve at version 1", block.ID, block.Text, changed, recorded)
		}
	}

	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = null where key = 'CORE-1'`); err != nil {
		t.Fatalf("reopen CORE-1: %v", err)
	}
	if err := service.SettleIssueDocuments(ctx, "CORE-1"); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 2 {
		t.Fatalf("spec blocks indexed at %v, want version 2", version)
	}
	if changed, recorded := dated(spec, typed); changed != 2 || recorded {
		t.Fatalf("after the reopen, the typed copy %s is dated %d (text at version 1 %v), want version 2", typed, changed, recorded)
	}
}

// A closed issue's document keeps the history its rows give it when unsettled edits cross. Version
// 1 holds the paragraphs "Intro.", "Moved.", "Same.", "Same.", "Retyped.", "Middle.", "Same." and
// "End.". Unsettled edits then move "Moved." to the end, delete "Retyped." and type in its place a
// paragraph of the same text under a new id, and type "Typed after the close." under a new id
// after "Middle."; the issue closes with them unsettled. In mode rows, version 1 was indexed while
// the issue was open and a rebuild-refs marks the document owed. In mode deploy, the document has
// no index at all, as migration 0121 leaves a database, and is owed.
//
// The closed tick indexes version 1's own text: nothing is left owed, every text recorded at
// version 1 is text version 1 holds, no block is recorded deleted at version 1, and the paragraph
// typed after the close has no row and no text at version 1. In mode rows, every block indexed at
// version 1 while the issue was open keeps its row under its own id - the moved paragraph, each
// "Same." and the deleted "Retyped." - dated version 1 with version 1's text, and its reference
// resolves, while the retyped paragraph, which no version holds under its id, has no row. In mode
// deploy, text identity is all the index has: each live block whose text version 1 holds resolves,
// the retyped paragraph among them, and the moved paragraph, whose text is held once on each side,
// is matched by that text across the blocks it moved past and stays dated version 1 after the
// reopen.
//
// The reopen's settlement writes version 2. The paragraph typed after the close, and in mode rows
// the retyped one, are dated version 2, in mode rows the blocks unchanged since version 1 stay
// dated version 1, and no row is dated to, or records a text at, a version whose markdown lacks
// that text.
func TestAClosedDocumentsCrossedEditsKeepTheirRowsHistory(t *testing.T) {
	for _, mode := range []string{"rows", "deploy"} {
		t.Run(mode, func(t *testing.T) {
			database, service := blockIndexService(t)
			ctx := context.Background()
			spec := createOwedDocument(t, database, service, "", "spec",
				"Intro.\n\nMoved.\n\nSame.\n\nSame.\n\nRetyped.\n\nMiddle.\n\nSame.\n\nEnd.\n", "")
			var stored string
			if err := database.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = 1`, spec).Scan(&stored); err != nil {
				t.Fatalf("read version 1: %v", err)
			}
			if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
				t.Fatalf("indexing version 1 while CORE-1 is open: tick = %#v", tick)
			}
			_, firstBlocks, err := service.TextWithBlocks(ctx, spec)
			if err != nil {
				t.Fatalf("read the live document: %v", err)
			}
			var held []string
			for _, block := range firstBlocks {
				if block.Type == "paragraph" {
					held = append(held, block.ID)
				}
			}
			if len(held) != 8 {
				t.Fatalf("version 1's paragraphs %#v, want 8", firstBlocks)
			}
			firstTexts := map[string]string{}
			for _, id := range held {
				var firstText string
				if err := database.Pool.QueryRow(ctx, `
					select text from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1
				`, spec, id).Scan(&firstText); err != nil {
					t.Fatalf("read block %s's text at version 1 while CORE-1 is open: %v", id, err)
				}
				firstTexts[id] = firstText
			}
			moved, retyped, middle := held[1], held[4], held[5]
			const retypedAgain = "0b6f2c4e-8a1d-4f3b-9e7c-2d5a6b8c9e01"
			const typedAfter = "7c9d1e2f-3a4b-4c5d-8e6f-9a0b1c2d3e4f"
			paragraph := func(id, body string) *pmdoc.Node {
				return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id}, Children: []*pmdoc.Node{{Type: "text", Text: body}}}
			}
			editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
				var movedNode *pmdoc.Node
				children := make([]*pmdoc.Node, 0, len(tree.Children)+1)
				for _, child := range tree.Children {
					id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
					switch id {
					case moved:
						movedNode = child
						continue
					case retyped:
						child = paragraph(retypedAgain, "Retyped.")
					}
					children = append(children, child)
					if id == middle {
						children = append(children, paragraph(typedAfter, "Typed after the close."))
					}
				}
				if movedNode == nil {
					t.Fatalf("no paragraph %s in the live tree", moved)
				}
				tree.Children = append(children, movedNode)
				return tree
			})
			waitForLandedAppends(t, service, spec)
			if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
				t.Fatalf("close CORE-1: %v", err)
			}
			closeDocumentRoom(t, service, spec)
			if mode == "rows" {
				// A rebuild-refs marks the document owed.
				if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, spec); err != nil {
					t.Fatalf("mark the document owed: %v", err)
				}
			} else {
				// The document has no index at all, as migration 0121 leaves a database, and is owed.
				for _, statement := range []string{
					`delete from block_refs where artifact_id = $1`,
					`delete from document_block_texts where artifact_id = $1`,
					`delete from document_blocks where artifact_id = $1`,
					`delete from document_indexed_versions where artifact_id = $1`,
					`update document_block_versions set version = null where artifact_id = $1`,
				} {
					if _, err := database.Pool.Exec(ctx, statement, spec); err != nil {
						t.Fatalf("clear the document's index (%s): %v", statement, err)
					}
				}
			}
			live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
			if err != nil {
				t.Fatalf("read the live document: %v", err)
			}
			var order []string
			for _, block := range liveBlocks {
				if block.Type == "paragraph" {
					order = append(order, block.ID)
				}
			}
			if want := []string{held[0], held[2], held[3], retypedAgain, middle, typedAfter, held[6], held[7], moved}; !slices.Equal(order, want) {
				t.Fatalf("live paragraphs %v, want %v", order, want)
			}

			if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
				t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
			}
			if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
				t.Fatalf("blocks indexed at %v, want version 1", version)
			}
			document, found, err := refs.Node(ctx, database.Pool, "artifact", spec, false)
			if err != nil || !found {
				t.Fatalf("resolve the spec: found %v (%v)", found, err)
			}
			resolve := func(id string) {
				t.Helper()
				reference := refs.BlockRef(document.Ref, id)
				ref, ok := text.ParseRef(reference, "https://dispatch.example")
				if !ok {
					t.Fatalf("%s is no reference", reference)
				}
				node, found, err := refs.Node(ctx, database.Pool, "block", text.TargetID(ref), true)
				if err != nil || !found || node.BlockID != id {
					t.Fatalf("%s resolves to %#v, found %v (%v), want block %s", reference, node, found, err, id)
				}
			}
			dated := func(id string, version int) (int, bool, bool) {
				t.Helper()
				var changed int
				var recorded, deleted bool
				if err := database.Pool.QueryRow(ctx, `
					select coalesce((select changed_version from document_blocks where artifact_id = $1 and block_id = $2), 0),
					       exists (select 1 from document_block_texts where artifact_id = $1 and block_id = $2 and version = $3 and text is not null),
					       exists (select 1 from document_block_texts where artifact_id = $1 and block_id = $2 and version = $3 and text is null)
				`, spec, id, version).Scan(&changed, &recorded, &deleted); err != nil {
					t.Fatalf("read block %s at version %d: %v", id, version, err)
				}
				return changed, recorded, deleted
			}
			rows, err := database.Pool.Query(ctx, `
				select block_id, text from document_block_texts where artifact_id = $1 and version = 1
			`, spec)
			if err != nil {
				t.Fatalf("read the texts recorded at version 1: %v", err)
			}
			for rows.Next() {
				var id string
				var body *string
				if err := rows.Scan(&id, &body); err != nil {
					t.Fatalf("scan a text recorded at version 1: %v", err)
				}
				if body == nil {
					t.Errorf("after the closed tick, block %s is recorded deleted at version 1, which holds every block indexed before the close", id)
				} else if !strings.Contains(stored, *body) {
					t.Errorf("after the closed tick, block %s is recorded at version 1 with text %q, which version 1 %q does not hold", id, *body, stored)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the texts recorded at version 1: %v", err)
			}
			if changed, recorded, deleted := dated(typedAfter, 1); changed != 0 || recorded || deleted {
				t.Errorf("after the closed tick, the paragraph typed after the close %s is dated %d (text at version 1 %v, deleted at version 1 %v), want no row and no text at version 1", typedAfter, changed, recorded, deleted)
			}
			if mode == "rows" {
				for _, id := range held {
					var keptText string
					if err := database.Pool.QueryRow(ctx, `
						select coalesce(text, '') from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1
					`, spec, id).Scan(&keptText); err != nil && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("read block %s's text at version 1: %v", id, err)
					}
					if changed, _, _ := dated(id, 1); changed != 1 || keptText != firstTexts[id] {
						t.Errorf("after the closed tick, block %s that version 1 holds is dated %d with text %q at version 1, want dated 1 with %q", id, changed, keptText, firstTexts[id])
						continue
					}
					resolve(id)
				}
				if changed, recorded, deleted := dated(retypedAgain, 1); changed != 0 || recorded || deleted {
					t.Errorf("after the closed tick, the retyped paragraph %s is dated %d (text at version 1 %v, deleted at version 1 %v), want no row", retypedAgain, changed, recorded, deleted)
				}
			} else {
				for _, block := range liveBlocks {
					if strings.Contains(stored, live[block.From:block.To]) {
						resolve(block.ID)
					}
				}
			}
			if t.Failed() {
				t.FailNow()
			}

			if _, err := database.Pool.Exec(ctx, `update issues set closed_at = null where key = 'CORE-1'`); err != nil {
				t.Fatalf("reopen CORE-1: %v", err)
			}
			if err := service.SettleIssueDocuments(ctx, "CORE-1"); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			var latest int
			if err := database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, spec).Scan(&latest); err != nil {
				t.Fatalf("read the latest version: %v", err)
			}
			if version := blockIndexAt(t, database, spec); latest != 2 || version == nil || *version != 2 {
				t.Fatalf("latest version %d, blocks indexed at %v, want version 2 written and indexed", latest, version)
			}
			newlyTyped := []string{typedAfter}
			if mode == "rows" {
				newlyTyped = append(newlyTyped, retypedAgain)
			}
			for _, id := range newlyTyped {
				if changed, recorded, _ := dated(id, 1); changed != 2 || recorded {
					t.Errorf("after the reopen, paragraph %s is dated %d (text at version 1 %v), want version 2, the first that holds it", id, changed, recorded)
				}
			}
			if mode == "deploy" {
				if changed, _, deleted := dated(moved, 1); changed != 1 || deleted {
					t.Errorf("after the reopen, the moved paragraph %s is dated %d (deleted at version 1 %v), want version 1, the first that holds it", moved, changed, deleted)
				}
			}
			if mode == "rows" {
				for _, id := range held {
					if id == retyped {
						continue
					}
					if changed, _, deleted := dated(id, 1); changed != 1 || deleted {
						t.Errorf("after the reopen, block %s unchanged since version 1 is dated %d (deleted at version 1 %v), want version 1", id, changed, deleted)
					}
				}
			}
			rows, err = database.Pool.Query(ctx, `
				select d.block_id, d.changed_version, coalesce(t.text, ''), t.text is not null, coalesce(v.markdown, '')
				from document_blocks d
				left join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = d.changed_version
				left join artifact_versions v on v.artifact_id = d.artifact_id and v.number = d.changed_version
				where d.artifact_id = $1
				union all
				select t.block_id, t.version, t.text, true, coalesce(v.markdown, '')
				from document_block_texts t
				left join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
				where t.artifact_id = $1 and t.text is not null
			`, spec)
			if err != nil {
				t.Fatalf("read the dated blocks and texts: %v", err)
			}
			for rows.Next() {
				var id, body, markdown string
				var version int
				var recorded bool
				if err := rows.Scan(&id, &version, &body, &recorded, &markdown); err != nil {
					t.Fatalf("scan a dated block or text: %v", err)
				}
				if !recorded || !strings.Contains(markdown, body) {
					t.Errorf("after the reopen, block %s is dated to or records a text at version %d (%q, recorded %v), whose markdown %q lacks it", id, version, body, recorded, markdown)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the dated blocks and texts: %v", err)
			}
		})
	}
}

// A block pasted in the browser keeps its source's id until a settlement stamps a new one. Version
// 1 holds "Foo." twice, indexed while the issue was open as two rows. An unsettled paste puts a
// copy of the first, under the first's id, between them; the issue closes and a rebuild-refs marks
// the document owed. The closed tick gives the version's two "Foo." blocks the two rows' ids, each
// once: the second stays dated version 1 with its version 1 text, is not recorded deleted at the
// version that holds it, and resolves by its #b- reference.
func TestAClosedDocumentsPasteKeepingItsSourcesIDTakesNoRow(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", "Foo.\n\nFoo.\n", "")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("indexing version 1 while CORE-1 is open: tick = %#v", tick)
	}
	paragraphs := func() []string {
		t.Helper()
		_, blocks, err := service.TextWithBlocks(ctx, spec)
		if err != nil {
			t.Fatalf("read the live document: %v", err)
		}
		var ids []string
		for _, block := range blocks {
			if block.Type == "paragraph" {
				ids = append(ids, block.ID)
			}
		}
		return ids
	}
	held := paragraphs()
	if len(held) != 2 {
		t.Fatalf("version 1's paragraphs %v, want 2", held)
	}
	source, second := held[0], held[1]
	var secondText string
	if err := database.Pool.QueryRow(ctx, `
		select text from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1
	`, spec, second).Scan(&secondText); err != nil {
		t.Fatalf("read block %s's text at version 1 while CORE-1 is open: %v", second, err)
	}
	editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
		for index, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id == source {
				paste := &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: source}, Children: []*pmdoc.Node{{Type: "text", Text: "Foo."}}}
				tree.Children = slices.Insert(tree.Children, index+1, paste)
				return tree
			}
		}
		t.Fatalf("no paragraph %s in the live tree", source)
		return tree
	})
	waitForLandedAppends(t, service, spec)
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	closeDocumentRoom(t, service, spec)
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, spec); err != nil {
		t.Fatalf("mark the document owed: %v", err)
	}
	if order, want := paragraphs(), []string{source, source, second}; !slices.Equal(order, want) {
		t.Fatalf("live paragraphs %v, want %v", order, want)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed and nothing owed", tick)
	}
	var changed int
	var kept string
	var deleted bool
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select changed_version from document_blocks where artifact_id = t.artifact_id and block_id = t.block_id), 0),
		       coalesce(t.text, ''), t.text is null
		from document_block_texts t
		where t.artifact_id = $1 and t.block_id = $2 and t.version = 1
	`, spec, second).Scan(&changed, &kept, &deleted); err != nil {
		t.Fatalf("read block %s at version 1 after the closed tick: %v", second, err)
	}
	if changed != 1 || deleted || kept != secondText {
		t.Fatalf("after the closed tick, block %s is dated %d with text %q (deleted %v) at version 1, want version 1 with version 1's text %q", second, changed, kept, deleted, secondText)
	}
	document, found, err := refs.Node(ctx, database.Pool, "artifact", spec, false)
	if err != nil || !found {
		t.Fatalf("resolve the spec: found %v (%v)", found, err)
	}
	reference := refs.BlockRef(document.Ref, second)
	ref, ok := text.ParseRef(reference, "https://dispatch.example")
	if !ok {
		t.Fatalf("%s is no reference", reference)
	}
	if node, found, err := refs.Node(ctx, database.Pool, "block", text.TargetID(ref), true); err != nil || !found || node.BlockID != second {
		t.Fatalf("%s resolves to %#v, found %v (%v), want block %s", reference, node, found, err, second)
	}
}

// Twenty-five documents of an open issue whose ids sort first are skipped as live ahead, owed a
// settlement that has not run. The keyset moves past them, so the document that sorts last is
// indexed in the second batch rather than waiting behind them; they stay owed, and the next tick
// starts over.
func TestBlockIndexIsNotHeldByDocumentsItSkips(t *testing.T) {
	database, service := blockIndexService(t)
	skipped := make([]string, 25)
	for index := range skipped {
		id := fmt.Sprintf("00000000-0000-0000-0000-0000000000%02d", index+1)
		skipped[index] = createOwedDocument(t, database, service, id, fmt.Sprintf("draft-%02d", index+1),
			fmt.Sprintf("Live text %d.\n", index+1), fmt.Sprintf("Earlier text %d.\n", index+1))
	}
	last := createOwedDocument(t, database, service, "ffffffff-ffff-ffff-ffff-ffffffffffff", "spec", shipItSpec, "")
	settle(t, database, last)

	first := reconcileBlockIndex(t, service, "")
	if first.Documents != 0 || first.LiveAhead != blockIndexDocumentBatch || !first.Full || first.Cursor != skipped[blockIndexDocumentBatch-1] {
		t.Fatalf("first tick = %#v, want a full batch of skipped documents and the cursor past them", first)
	}
	second := reconcileBlockIndex(t, service, first.Cursor)
	if second.Documents != 1 || second.LiveAhead != len(skipped)-blockIndexDocumentBatch || second.Full || second.Cursor != "" || second.OwedDocuments != len(skipped) {
		t.Fatalf("second tick = %#v, want the last document indexed past the skipped ones, which stay owed", second)
	}
	if version := blockIndexAt(t, database, last); version == nil || *version != 1 {
		t.Fatalf("the last document's blocks are indexed at %v, want version 1", version)
	}
	for _, id := range skipped {
		if version := blockIndexAt(t, database, id); version != nil {
			t.Fatalf("skipped document %s indexed at %d, want it owed", id, *version)
		}
	}
	if again := reconcileBlockIndex(t, service, second.Cursor); again.LiveAhead != blockIndexDocumentBatch || again.Documents != 0 {
		t.Fatalf("third tick = %#v, want the batch to start over at the skipped documents", again)
	}
}

// A loop remembers the documents it found live ahead: one listed again at the same version, with
// the same pending-settlement mark and no room resident, is counted live ahead without being read,
// so an open issue's document whose settlement waits is not re-read every tick. A live update, a
// new version or a resident room has the next tick read it again, and a document indexed elsewhere
// is forgotten.
func TestBlockIndexDoesNotRereadALiveAheadDocumentUntilItMoves(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	edited := createOwedDocument(t, database, service, "", "edited", "Live text one.\n", "Earlier text one.\n")
	versioned := createOwedDocument(t, database, service, "", "versioned", "Live text two.\n", "Earlier text two.\n")
	opened := createOwedDocument(t, database, service, "", "opened", "Live text three.\n", "Earlier text three.\n")
	indexedElsewhere := createOwedDocument(t, database, service, "", "indexed", "Live text four.\n", "Earlier text four.\n")
	documents := []string{edited, versioned, opened, indexedElsewhere}
	for _, id := range documents {
		closeDocumentRoom(t, service, id)
	}
	liveAhead := liveAheadDocuments{}
	tick := func() BlockIndexTick {
		t.Helper()
		tick, err := service.ReconcileBlockIndex(ctx, "", liveAhead)
		if err != nil {
			t.Fatalf("block index tick: %v", err)
		}
		return tick
	}
	live := func(id string) string {
		t.Helper()
		markdown, _, err := service.TextWithBlocks(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return markdown
	}
	storeAsVersion1 := func(id string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, id, live(id)); err != nil {
			t.Fatalf("store the live text of %s as version 1: %v", id, err)
		}
	}
	settlementMark := func(id string) time.Time {
		t.Helper()
		var markedAt time.Time
		if err := database.Pool.QueryRow(ctx, `select marked_at from doc_settlements_pending where artifact_id = $1`, id).Scan(&markedAt); err != nil {
			t.Fatalf("read the pending settlement of %s: %v", id, err)
		}
		return markedAt
	}

	if first := tick(); first.LiveAhead != 4 || first.Documents != 0 {
		t.Fatalf("first tick = %#v, want the four documents live ahead", first)
	}
	// Each version 1 now holds its document's live text, which a read would index.
	for _, id := range documents {
		storeAsVersion1(id)
	}
	if second := tick(); second.LiveAhead != 4 || second.Documents != 0 || second.OwedDocuments != 4 {
		t.Fatalf("second tick = %#v, want the four documents counted live ahead unread", second)
	}

	// A live update moves the settlement mark; version 1 then holds the edited text.
	later, err := pmdoc.Parse("Later text one.\n")
	if err != nil {
		t.Fatal(err)
	}
	editLiveTree(t, service, edited, func(*pmdoc.Node) *pmdoc.Node { return later })
	waitForLandedAppends(t, service, edited)
	closeDocumentRoom(t, service, edited)
	storeAsVersion1(edited)
	// A version 2 holding the live text, with the settlement mark as it was.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')
	`, versioned, live(versioned)); err != nil {
		t.Fatalf("write version 2 of %s: %v", versioned, err)
	}
	// A resident room, which moves neither.
	mark := settlementMark(opened)
	if err := service.warmLiveDocument(ctx, opened); err != nil {
		t.Fatalf("open the room of %s: %v", opened, err)
	}
	if service.srv.GetDoc(opened) == nil || !settlementMark(opened).Equal(mark) {
		t.Fatalf("opening the room of %s left none resident or moved its settlement mark", opened)
	}
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = 1 where artifact_id = $1`, indexedElsewhere); err != nil {
		t.Fatalf("index %s elsewhere: %v", indexedElsewhere, err)
	}

	if third := tick(); third.Documents != 3 || third.LiveAhead != 0 || third.OwedDocuments != 0 {
		t.Fatalf("third tick = %#v, want the three documents read again and indexed", third)
	}
	if len(liveAhead) != 0 {
		t.Fatalf("the loop still remembers %v, want nothing", liveAhead)
	}
}

// The loop's memory of a live-ahead document holds whether its issue was open: a close can leave
// the document's version, settlement mark and room as they were. An open issue's document is
// found live ahead and remembered; its issue then closes with no new mark, and the next tick of
// the same loop reads it again and indexes it as a closed issue's document.
func TestBlockIndexRereadsALiveAheadDocumentWhoseIssueCloses(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", "Live text.\n", "Earlier text.\n")
	closeDocumentRoom(t, service, spec)
	liveAhead := liveAheadDocuments{}
	first, err := service.ReconcileBlockIndex(ctx, "", liveAhead)
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	if _, remembered := liveAhead[spec]; first.LiveAhead != 1 || first.Documents != 0 || !remembered {
		t.Fatalf("first tick = %#v (remembered %v), want the open issue's document live ahead and remembered", first, remembered)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	second, err := service.ReconcileBlockIndex(ctx, "", liveAhead)
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	if second.Documents != 1 || second.LiveAhead != 0 || second.OwedDocuments != 0 {
		t.Fatalf("second tick = %#v, want the closed issue's document read again and indexed", second)
	}
}

// The listing reads whether a document's issue is open with no lock, and live ahead is decided
// under the owner row's lock, so the two can disagree. A tick lists a closed issue's live-ahead
// document; the issue reopens before the tick locks it, so the lock finds it live ahead and the
// tick skips it, remembering nothing it listed closed. The issue then closes again with no new
// mark, and the next tick of the same loop reads it again and indexes it as a closed issue's
// document.
func TestBlockIndexRemembersNoLiveAheadDocumentItListedClosed(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "", "spec", "Live text.\n", "Earlier text.\n")
	closeDocumentRoom(t, service, spec)
	setClosed := func(closed bool) {
		t.Helper()
		statement := `update issues set closed_at = null where key = 'CORE-1'`
		if closed {
			statement = `update issues set closed_at = now() where key = 'CORE-1'`
		}
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set CORE-1 closed %v: %v", closed, err)
		}
	}
	setClosed(true)
	service.afterBlockIndexListed = func() { setClosed(false) }
	liveAhead := liveAheadDocuments{}
	first, err := service.ReconcileBlockIndex(ctx, "", liveAhead)
	service.afterBlockIndexListed = nil
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	if first.LiveAhead != 1 || first.Documents != 0 {
		t.Fatalf("first tick = %#v, want the document listed closed, locked open and skipped as live ahead", first)
	}
	if found, remembered := liveAhead[spec]; remembered {
		t.Errorf("the loop remembers %#v, which it listed with its issue closed, want nothing", found)
	}
	setClosed(true)
	second, err := service.ReconcileBlockIndex(ctx, "", liveAhead)
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	if second.Documents != 1 || second.LiveAhead != 0 || second.OwedDocuments != 0 {
		t.Fatalf("second tick = %#v, want the closed issue's document read again and indexed", second)
	}
}

// writeAsAnOlderBuild writes the document's next version as a build without the block index does:
// the edit (from replaced by to in a text of the live document) reaches the store through the room,
// and its settlement writes the version, the document's reference to CORE-2 with the old grammar,
// and the version's artifact.version event, and clears the pending settlement. It indexes no block,
// and nothing here marks the document owed. It returns the version and its event's id.
func writeAsAnOlderBuild(t *testing.T, database *store.Store, service *Service, artifactID, from, to string) (model.Version, int64) {
	t.Helper()
	ctx := context.Background()
	replaced := false
	var replace func(node *pmdoc.Node)
	replace = func(node *pmdoc.Node) {
		if node.Type == "text" && strings.Contains(node.Text, from) {
			node.Text, replaced = strings.Replace(node.Text, from, to, 1), true
		}
		for _, child := range node.Children {
			replace(child)
		}
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		replace(tree)
		return tree
	})
	if !replaced {
		t.Fatalf("no text of %s holds %q", artifactID, from)
	}
	waitForLandedAppends(t, service, artifactID)
	live, _, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil || !strings.Contains(live, to) {
		t.Fatalf("live text after the edit = %q (%v), want it to hold %q", live, err, to)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var version model.Version
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number, created_at
	`, artifactID, live).Scan(&version.Number, &version.CreatedAt); err != nil {
		t.Fatalf("write the version: %v", err)
	}
	var eventID int64
	if err := tx.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', $1::text, 'name', 'spec.md', 'version', jsonb_build_object('number', $2::int)), false
		from events where issue_key = 'CORE-1'
		returning id
	`, artifactID, version.Number).Scan(&eventID); err != nil {
		t.Fatalf("append the version's event: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'CORE-2', $2)
		on conflict do nothing
	`, artifactID, eventID); err != nil {
		t.Fatalf("index the version's references: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return version, eventID
}

// citedSpec is a spec whose callout cites nothing yet; writeAsAnOlderBuild adds the citation.
const citedSpec = ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it.\n:::\n\nOther paragraph.\n"

// indexedSpec is CORE-1's spec, indexed at version 1 by a tick.
func indexedSpec(t *testing.T, database *store.Store, service *Service) string {
	t.Helper()
	spec := createOwedDocument(t, database, service, "", "spec", citedSpec, "")
	settle(t, database, spec)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("indexing version 1: tick = %#v", tick)
	}
	return spec
}

// A version a build without the block index writes during a roll is indexed by the next tick, with
// no command run: the trigger on artifact_versions marked the document owed. The mention the version
// added is dated by the version and the event that recorded it.
func TestAnOlderBuildsVersionWriteIsIndexedWithNoCommand(t *testing.T) {
	database, service := blockIndexService(t)
	spec := indexedSpec(t, database, service)
	version, eventID := writeAsAnOlderBuild(t, database, service, spec, "Ship it.", "Ship it after dispatch://CORE-2.")
	if indexed := blockIndexAt(t, database, spec); indexed != nil {
		t.Fatalf("after the older build's version the blocks stand at %d, want the document owed", *indexed)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the older build's version indexed", tick)
	}
	if indexed := blockIndexAt(t, database, spec); indexed == nil || *indexed != version.Number {
		t.Fatalf("blocks indexed at %v, want version %d", indexed, version.Number)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	pairs := blockPairsOf(t, database, spec)
	if len(pairs) != 1 || pairs[0].BlockID != holder || pairs[0].ToID != "CORE-2" || pairs[0].CreatedVersion != version.Number ||
		!pairs[0].CreatedAt.Equal(version.CreatedAt) || pairs[0].SourceSeq == nil || *pairs[0].SourceSeq != eventID {
		t.Fatalf("block_refs = %#v, want the mention from %s created by version %d at %s, event %d", pairs, holder, version.Number, version.CreatedAt, eventID)
	}
	var changed int
	if err := database.Pool.QueryRow(context.Background(), `
		select changed_version from document_blocks where artifact_id = $1 and block_id = $2
	`, spec, holder).Scan(&changed); err != nil || changed != version.Number {
		t.Fatalf("the edited block changed at %d (%v), want version %d", changed, err, version.Number)
	}
}

// The link a tick moves from the document onto its block keeps its place in an incremental read:
// GET /api/v1/references?since= lists it past the event before its version's, and not past its own.
func TestAReconciledLinkKeepsItsPlaceInSince(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := indexedSpec(t, database, service)
	_, eventID := writeAsAnOlderBuild(t, database, service, spec, "Ship it.", "Ship it after dispatch://CORE-2.")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("tick = %#v, want the version indexed", tick)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")

	before := eventID - 1
	edges, err := refs.Edges(ctx, database.Pool, refs.Query{Direction: "in", Kind: "issue", ID: "CORE-2", Since: &before})
	if err != nil {
		t.Fatalf("read the references to CORE-2 since %d: %v", before, err)
	}
	if len(edges) != 1 || edges[0].Node.Kind != "block" || edges[0].Node.ID != "CORE-1/spec#"+holder ||
		edges[0].SourceSeq == nil || *edges[0].SourceSeq != eventID {
		t.Fatalf("references to CORE-2 since %d = %#v, want the block %s's mention from event %d", before, edges, holder, eventID)
	}
	edges, err = refs.Edges(ctx, database.Pool, refs.Query{Direction: "in", Kind: "issue", ID: "CORE-2", Since: &eventID})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("references to CORE-2 since its own event %d = %#v, want none", eventID, edges)
	}
}

// A comment and an issue message an older build wrote cite a block: the comment by its dashboard
// URL, which that build indexed as the whole spec, the message by a pinned dispatch:// reference,
// which it dropped. One tick indexes each with the block grammar: one edge each, to the block,
// dated when the text was written and by no event. Nothing is left owed, and a second tick changes
// nothing.
func TestOlderItemTextCitingABlockIsReindexed(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	messageAt := time.Date(2026, 3, 5, 6, 7, 8, 0, time.UTC)
	var comment, message string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'See https://dispatch.example/issues/CORE-1/spec#b-ship%5Fit please.', $1)
		returning id::text
	`, commentAt).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Read dispatch://CORE-1/spec#b-ship%5Fit@v1 first.', $1)
		returning id::text
	`, messageAt).Scan(&message); err != nil {
		t.Fatalf("write the message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq) values ('comment', $1, 'artifact', 'CORE-1/spec', $2, 3)
	`, comment, commentAt); err != nil {
		t.Fatalf("index the comment with the old grammar: %v", err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Items != 2 || tick.OwedItems != 0 {
		t.Fatalf("tick = %#v, want both items indexed and none owed", tick)
	}
	for _, item := range []struct {
		kind, id string
		at       time.Time
	}{{"comment", comment, commentAt}, {"message", message, messageAt}} {
		got := refsFrom(t, database, item.kind, item.id)
		if len(got) != 1 || got[0].ToKind != "block" || got[0].ToID != "CORE-1/spec#ship_it" || !got[0].CreatedAt.Equal(item.at) || got[0].SourceSeq != nil {
			t.Fatalf("refs from the %s = %#v, want one edge to block CORE-1/spec#ship_it created at %s by no event", item.kind, got, item.at)
		}
	}
	commentRefs, messageRefs := refsFrom(t, database, "comment", comment), refsFrom(t, database, "message", message)

	if again := reconcileBlockIndex(t, service, ""); again.Items != 0 || again.OwedItems != 0 {
		t.Fatalf("second tick = %#v, want nothing owed", again)
	}
	if got := refsFrom(t, database, "comment", comment); !reflect.DeepEqual(got, commentRefs) {
		t.Fatalf("the second tick changed the comment's refs: %#v -> %#v", commentRefs, got)
	}
	if got := refsFrom(t, database, "message", message); !reflect.DeepEqual(got, messageRefs) {
		t.Fatalf("the second tick changed the message's refs: %#v -> %#v", messageRefs, got)
	}
}

// An edit of a comment that lands while the tick indexes it is not lost: the tick waits on the
// comment's owner lock, which an edit holds from before its first read to its commit, indexes the
// edited text once the edit commits, and leaves the comment owed, since the edit moved its owed
// row; the next tick settles it. Neither waits on the other for good.
func TestADrainedItemKeepsAConcurrentEdit(t *testing.T) {
	database, service := blockIndexService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'See https://dispatch.example/issues/CORE-1/spec#b-ship%5Fit.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	// The edit as a route makes it: the owner's lock first, its write after.
	if err := store.LockOwner(ctx, writer, store.IssueOwner("CORE-1")); err != nil {
		t.Fatalf("take the comment's owner lock: %v", err)
	}
	if _, err := writer.Exec(ctx, `
		update comments set body = 'Read dispatch://CORE-1/spec#b-other first.', edited_at = now() where id = $1
	`, comment); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}

	ticked := make(chan error, 1)
	var tick BlockIndexTick
	go func() {
		var err error
		tick, err = service.ReconcileBlockIndex(ctx, "", liveAheadDocuments{})
		ticked <- err
	}()
	waitForLockWait(t, ctx, database, "%owner_lock%", ticked)
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatalf("tick beside the edit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the tick never finished after the edit committed")
	}
	if tick.Items != 1 || tick.OwedItems != 1 {
		t.Fatalf("tick beside the edit = %#v, want the comment indexed and still owed for the edit", tick)
	}
	next, err := service.ReconcileBlockIndex(ctx, "", liveAheadDocuments{})
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if next.Items != 1 || next.OwedItems != 0 {
		t.Fatalf("second tick = %#v, want the comment indexed and nothing owed", next)
	}
	if got := refsFrom(t, database, "comment", comment); len(got) != 1 || got[0].ToKind != "block" || got[0].ToID != "CORE-1/spec#other" {
		t.Fatalf("refs from the comment = %#v, want the edited text's one edge to block CORE-1/spec#other", got)
	}
	var stillOwed bool
	err = database.Pool.QueryRow(ctx, `select true from block_grammar_owed where id = $1::uuid`, comment).Scan(&stillOwed)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the comment is still owed (%v), want its owed row gone", err)
	}
}

// itemBlockRow is one block of a comment or message as stored: a message's has changed revision 0.
type itemBlockRow struct {
	ID              string
	Position        *int
	ChangedRevision int
	ChangedAt       time.Time
	MadeAt          *time.Time
	RemovedAt       *time.Time
}

// itemBlocksOf is every block comment or message id ever held, by block id.
func itemBlocksOf(t *testing.T, database *store.Store, kind, id string) map[string]itemBlockRow {
	t.Helper()
	query := `select block_id, position, changed_revision, changed_at, made_at, removed_at
		from comment_blocks where comment_id = $1::uuid`
	if kind == "message" {
		query = `select block_id, position, 0, changed_at, made_at, removed_at
			from message_blocks where message_id = $1::uuid`
	}
	rows, err := database.Pool.Query(context.Background(), query, id)
	if err != nil {
		t.Fatalf("read the blocks of %s %s: %v", kind, id, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (itemBlockRow, error) {
		var block itemBlockRow
		err := row.Scan(&block.ID, &block.Position, &block.ChangedRevision, &block.ChangedAt, &block.MadeAt, &block.RemovedAt)
		return block, err
	})
	if err != nil {
		t.Fatalf("scan the blocks of %s %s: %v", kind, id, err)
	}
	blocks := make(map[string]itemBlockRow, len(found))
	for _, block := range found {
		blocks[block.ID] = block
	}
	return blocks
}

// itemBlockRefsOf is each link a block of comment or message id holds, as "<block id> <to kind>
// <to id>", with its created_at.
func itemBlockRefsOf(t *testing.T, database *store.Store, kind, id string) map[string]time.Time {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id || ' ' || to_kind || ' ' || to_id, created_at from `+kind+`_block_refs where `+kind+`_id = $1::uuid
	`, id)
	if err != nil {
		t.Fatalf("read the block links of %s %s: %v", kind, id, err)
	}
	defer rows.Close()
	links := map[string]time.Time{}
	for rows.Next() {
		var link string
		var at time.Time
		if err := rows.Scan(&link, &at); err != nil {
			t.Fatalf("scan a block link of %s %s: %v", kind, id, err)
		}
		links[link] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block links of %s %s: %v", kind, id, err)
	}
	return links
}

// checkItemBlock fails unless the block is current at position, last changed at revision and at,
// and made at madeAt.
func checkItemBlock(t *testing.T, label string, blocks map[string]itemBlockRow, id string, position, revision int, at, madeAt time.Time) {
	t.Helper()
	block, ok := blocks[id]
	if !ok || block.Position == nil || *block.Position != position || block.ChangedRevision != revision ||
		!block.ChangedAt.Equal(at) || block.MadeAt == nil || !block.MadeAt.Equal(madeAt) || block.RemovedAt != nil {
		t.Fatalf("%s block %s = %#v (found %v), want current at position %d, changed at revision %d at %s, made at %s",
			label, id, block, ok, position, revision, at, madeAt)
	}
}

// A comment, an issue message and a direct message a build without item blocks inserted, and a
// comment it edited after this build indexed it, are owed their blocks by the triggers, which no
// write of theirs cleared. One tick indexes each, at the time its body was written (a comment's
// coalesce(edited_at, created_at), a message's created_at): its blocks, the links each holds, and
// for the comment and the issue message their own references; the direct message holds no link.
// The edit is indexed at the comment's next revision under ids aligned with the ones it held: the
// moved blocks keep theirs and their changed revision, the reworded block keeps its id at the new
// revision, and the new block takes the next id. Nothing is left owed.
func TestTheBlockIndexIndexesItemsAnOlderBuildWrote(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	messageAt := time.Date(2026, 3, 5, 6, 7, 8, 0, time.UTC)
	directAt := time.Date(2026, 3, 6, 7, 8, 9, 0, time.UTC)
	editedFirstAt := time.Date(2026, 3, 7, 8, 9, 10, 0, time.UTC)
	editedAt := time.Date(2026, 3, 8, 9, 10, 11, 0, time.UTC)
	insert := func(label, query string, args ...any) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatalf("write the %s: %v", label, err)
		}
		return id
	}
	comment := insert("comment", `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'First paragraph.\n\nSecond cites dispatch://CORE-2.', $1)
		returning id::text`, commentAt)
	message := insert("issue message", `
		insert into messages (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Read dispatch://CORE-2 now.', $1)
		returning id::text`, messageAt)
	direct := insert("direct message", `
		insert into messages (author, body, target, created_at)
		values ('{"kind":"user","id":"alice"}', E'Hello there.\n\nSee dispatch://CORE-2.', 'session:s1', $1)
		returning id::text`, directAt)
	edited := insert("comment to edit", `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'Alpha.\n\nBeta.\n\nGamma.', $1)
		returning id::text`, editedFirstAt)

	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 4 || tick.OwedItemBlocks != 0 || tick.Items != 0 {
		t.Fatalf("first tick = %#v, want the four items' blocks indexed and none owed", tick)
	}
	blocks := itemBlocksOf(t, database, "comment", comment)
	if len(blocks) != 2 {
		t.Fatalf("comment blocks = %#v, want two", blocks)
	}
	checkItemBlock(t, "comment", blocks, "b1", 0, 1, commentAt, commentAt)
	checkItemBlock(t, "comment", blocks, "b2", 1, 1, commentAt, commentAt)
	if links := itemBlockRefsOf(t, database, "comment", comment); len(links) != 1 || !links["b2 issue CORE-2"].Equal(commentAt) {
		t.Fatalf("comment block links = %#v, want b2's link to CORE-2 at %s", links, commentAt)
	}
	if got := refsFrom(t, database, "comment", comment); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(commentAt) || got[0].SourceSeq != nil {
		t.Fatalf("refs from the comment = %#v, want one edge to CORE-2 at %s by no event", got, commentAt)
	}
	blocks = itemBlocksOf(t, database, "message", message)
	if len(blocks) != 1 {
		t.Fatalf("message blocks = %#v, want one", blocks)
	}
	checkItemBlock(t, "message", blocks, "b1", 0, 0, messageAt, messageAt)
	if links := itemBlockRefsOf(t, database, "message", message); len(links) != 1 || !links["b1 issue CORE-2"].Equal(messageAt) {
		t.Fatalf("message block links = %#v, want b1's link to CORE-2 at %s", links, messageAt)
	}
	if got := refsFrom(t, database, "message", message); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(messageAt) {
		t.Fatalf("refs from the message = %#v, want one edge to CORE-2 at %s", got, messageAt)
	}
	blocks = itemBlocksOf(t, database, "message", direct)
	if len(blocks) != 2 {
		t.Fatalf("direct message blocks = %#v, want two", blocks)
	}
	checkItemBlock(t, "direct message", blocks, "b1", 0, 0, directAt, directAt)
	checkItemBlock(t, "direct message", blocks, "b2", 1, 0, directAt, directAt)
	if links := itemBlockRefsOf(t, database, "message", direct); len(links) != 0 {
		t.Fatalf("direct message block links = %#v, want none", links)
	}
	if got := refsFrom(t, database, "message", direct); len(got) != 0 {
		t.Fatalf("refs from the direct message = %#v, want none", got)
	}
	blocks = itemBlocksOf(t, database, "comment", edited)
	if len(blocks) != 3 {
		t.Fatalf("blocks of the comment to edit = %#v, want three", blocks)
	}
	for position, id := range []string{"b1", "b2", "b3"} {
		checkItemBlock(t, "comment to edit", blocks, id, position, 1, editedFirstAt, editedFirstAt)
	}

	// The older build's edit moves Gamma first, rewords Beta and adds Delta.
	if _, err := database.Pool.Exec(ctx, `
		update comments set body = E'Gamma.\n\nAlpha.\n\nBeta, reworded, cites dispatch://CORE-2.\n\nDelta.', edited_at = $2
		where id = $1::uuid
	`, edited, editedAt); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 || tick.OwedItemBlocks != 0 {
		t.Fatalf("tick after the edit = %#v, want the edited comment's blocks indexed and none owed", tick)
	}
	blocks = itemBlocksOf(t, database, "comment", edited)
	if len(blocks) != 4 {
		t.Fatalf("blocks of the edited comment = %#v, want four", blocks)
	}
	checkItemBlock(t, "edited comment", blocks, "b3", 0, 1, editedFirstAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b1", 1, 1, editedFirstAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b2", 2, 2, editedAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b4", 3, 2, editedAt, editedAt)
	var revision, next int
	var owedAt *time.Time
	if err := database.Pool.QueryRow(ctx, `
		select revision, next_block, owed_at from comment_block_state where comment_id = $1::uuid
	`, edited).Scan(&revision, &next, &owedAt); err != nil {
		t.Fatalf("read the edited comment's block state: %v", err)
	}
	if revision != 2 || next != 5 || owedAt != nil {
		t.Fatalf("edited comment's state = revision %d, next block %d, owed at %v; want revision 2, next block 5, not owed", revision, next, owedAt)
	}
	if links := itemBlockRefsOf(t, database, "comment", edited); len(links) != 1 || !links["b2 issue CORE-2"].Equal(editedAt) {
		t.Fatalf("edited comment block links = %#v, want b2's link to CORE-2 at %s", links, editedAt)
	}
	if got := refsFrom(t, database, "comment", edited); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(editedAt) {
		t.Fatalf("refs from the edited comment = %#v, want one edge to CORE-2 at %s", got, editedAt)
	}

	if again := reconcileBlockIndex(t, service, ""); again.ItemBlocks != 0 || again.OwedItemBlocks != 0 {
		t.Fatalf("third tick = %#v, want nothing owed", again)
	}
}

// A comment or message rebuild-refs marks owed, already indexed, is re-indexed by the next tick
// with nothing about its blocks changed: the same ids at the same revision.
func TestTheBlockIndexReindexesItemsRebuildRefsOwed(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'One.\n\nTwo cites dispatch://CORE-2.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 {
		t.Fatalf("first tick = %#v, want the comment indexed", tick)
	}
	before := itemBlocksOf(t, database, "comment", comment)
	report, err := refs.RebuildAll(ctx, database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild refs: %v", err)
	}
	if report.ItemsOwed != 1 {
		t.Fatalf("rebuild report = %#v, want the one comment owed", report)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 || tick.OwedItemBlocks != 0 {
		t.Fatalf("tick after rebuild-refs = %#v, want the comment indexed and none owed", tick)
	}
	if after := itemBlocksOf(t, database, "comment", comment); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-indexing changed the comment's blocks: %#v -> %#v", before, after)
	}
	var revision int
	if err := database.Pool.QueryRow(ctx, `select revision from comment_block_state where comment_id = $1::uuid`, comment).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("revision after the re-index = %d (%v), want 1", revision, err)
	}
}
