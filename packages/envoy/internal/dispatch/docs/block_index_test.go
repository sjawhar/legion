package docs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
		if !indexed || blockText != live[block.From:block.To] {
			t.Fatalf("block %s is indexed %v with text %q, want it indexed with its live text %q", block.ID, indexed, blockText, live[block.From:block.To])
		}
	}
	if !strings.Contains(texts["region"], `state="resolved"`) {
		t.Fatalf("the ask block is indexed at version 1 without the sweep's state: %q", texts["region"])
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

// An edit of a comment that lands while the tick indexes it is not lost: the tick waits on the row
// the edit holds, indexes the edited text once the edit commits, and leaves the comment owed, since
// the edit moved its owed row; the next tick settles it. Neither waits on the other for good.
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
	waitForLockWait(t, ctx, database, "%from comments where id%", ticked)
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
