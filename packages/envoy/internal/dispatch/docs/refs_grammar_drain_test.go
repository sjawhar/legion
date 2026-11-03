package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// refsGrammarWritten is the time every source of these tests was written at, the time an edge a
// drain inserts for it takes.
var refsGrammarWritten = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// createRawSpec writes the primary document of CORE-1 as a build with no block index writes it:
// an artifacts row and one markdown version, with nothing indexed from either. Its markdown
// declares the question block q1, which is what a `#q1` written anywhere in its scope resolves
// against once that question has an ask.
func createRawSpec(t *testing.T, database *store.Store, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&id); err != nil {
		t.Fatalf("create the spec: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at, authors)
		values ($1, 1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, id, markdown, refsGrammarWritten); err != nil {
		t.Fatalf("write version 1 of the spec: %v", err)
	}
	return id
}

// settleQuestion writes the ask one question block of a document is settled by, as a build that
// knows nothing of this queue does: the asks row alone.
func settleQuestion(t *testing.T, database *store.Store, artifactID, blockID, question string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into asks (issue_key, author, question, block_id, block_artifact_id, created_at)
		values ('CORE-1', '{"kind":"session","id":"s1"}', $1, $2, $3, $4)
		returning id::text
	`, question, blockID, artifactID, refsGrammarWritten).Scan(&id); err != nil {
		t.Fatalf("settle question block %s: %v", blockID, err)
	}
	return id
}

func createRawComment(t *testing.T, database *store.Store, body string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', $1, $2)
		returning id::text
	`, body, refsGrammarWritten).Scan(&id); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	return id
}

func createRawMessage(t *testing.T, database *store.Store, body string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"session","id":"s1"}', $1, $2)
		returning id::text
	`, body, refsGrammarWritten).Scan(&id); err != nil {
		t.Fatalf("write the message: %v", err)
	}
	return id
}

// owedTexts is every row of the re-index queue as "kind id".
func owedTexts(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind || ' ' || id::text from refs_grammar_owed order by kind, id
	`)
	if err != nil {
		t.Fatalf("read refs_grammar_owed: %v", err)
	}
	defer rows.Close()
	owed := []string{}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan refs_grammar_owed: %v", err)
		}
		owed = append(owed, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs_grammar_owed: %v", err)
	}
	return owed
}

// assertRef fails unless the source's edges hold one to target, written at the time that source's
// text was written.
func assertRef(t *testing.T, database *store.Store, fromKind, fromID, toKind, toID string, writtenAt time.Time) {
	t.Helper()
	for _, ref := range refsFrom(t, database, fromKind, fromID) {
		if ref.ToKind != toKind || ref.ToID != toID {
			continue
		}
		if !ref.CreatedAt.Equal(writtenAt) {
			t.Fatalf("%s %s -> %s %s is dated %v, want the source's own write time %v",
				fromKind, fromID, toKind, toID, ref.CreatedAt, writtenAt)
		}
		return
	}
	t.Fatalf("%s %s has edges %#v, want one to %s %s", fromKind, fromID, refsFrom(t, database, fromKind, fromID), toKind, toID)
}

// Every text a build without the whole grammar stored is re-indexed by one tick of the block-index
// reconciliation, with no edit and no command run: the bare issue key a comment, an ask and a
// document write, the `#id` a message writes for a settled question block, and the short id a
// comment writes, each stored as an edge dated the time that source's text was written, and the
// document mention keeping the excerpt its block is cut to. The queue is empty after it.
func TestTheDrainIndexesTextStoredWithoutTheWholeGrammar(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createRawSpec(t, database, ":::ask{#q1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	ask := settleQuestion(t, database, spec, "q1", "Ship it after CORE-2?")
	comment := createRawComment(t, database, "Blocked on CORE-2, see dispatch://CORE-1/ask/"+ask[:8]+".")
	message := createRawMessage(t, database, "See #q1.")

	// A document whose live text the server serves, so its mention has a block to be cut to, with
	// its version written as a build without the block index writes one.
	var notes string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&notes); err != nil {
		t.Fatalf("create the notes: %v", err)
	}
	seedServiceText(t, service, notes, "Ship it after CORE-2.\n")
	live, _, err := service.TextWithBlocks(ctx, notes)
	if err != nil {
		t.Fatalf("read the notes: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at, authors)
		values ($1, 1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, notes, live, refsGrammarWritten); err != nil {
		t.Fatalf("write version 1 of the notes: %v", err)
	}
	settle(t, database, notes)
	settle(t, database, spec)
	// The block grammar's queue and the item blocks are settled, so the edges of the comment, the
	// ask and the message this tick writes are the whole grammar's own drain. The documents, which
	// an older build's version write leaves owed a block index, are indexed by the batch that owns
	// that index, which the drain owes them to.
	if _, err := database.Pool.Exec(ctx, `
		delete from block_grammar_owed;
		update comment_block_state set owed_at = null;
		update message_block_state set owed_at = null;
		update ask_block_state set owed_at = null;
	`); err != nil {
		t.Fatalf("settle the other queues: %v", err)
	}
	if owed := owedTexts(t, database); len(owed) == 0 {
		t.Fatalf("nothing is owed after writes an older build makes, want every one of them owed")
	}

	tick := reconcileBlockIndex(t, service, "")

	if tick.Texts == 0 || tick.Items != 0 || tick.ItemBlocks != 0 {
		t.Fatalf("tick = %#v, want the item edges of it written by the whole grammar's drain", tick)
	}
	if tick.OwedTexts != 0 {
		t.Fatalf("tick = %#v, want nothing owed an index with the whole grammar", tick)
	}
	if owed := owedTexts(t, database); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the tick, want it empty", owed)
	}
	assertRef(t, database, "comment", comment, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "comment", comment, "ask", ask, refsGrammarWritten)
	assertRef(t, database, "message", message, "ask", ask, refsGrammarWritten)
	assertRef(t, database, "ask", ask, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	holder := holderOf(t, service, notes, "CORE-2")
	var excerptVersion *int
	var excerptBlock *string
	if err := database.Pool.QueryRow(ctx, `
		select excerpt_version, excerpt_block_id from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'CORE-2'
	`, notes).Scan(&excerptVersion, &excerptBlock); err != nil {
		t.Fatalf("read the document mention's excerpt: %v", err)
	}
	if excerptVersion == nil || *excerptVersion != 1 || excerptBlock == nil || *excerptBlock != holder {
		t.Fatalf("the document mention's excerpt is block %v at version %v, want block %s at version 1",
			excerptBlock, excerptVersion, holder)
	}
}

// A `#id` written before its question settles names nothing at its own write: the question block
// has no ask yet, so the text is stored with no edge. The settlement is a write of the ask's row,
// which owes that document's scope, and the drain re-reads every source whose text belongs to the
// document, so the comment, the message and the document itself list under that question from then
// on, with no edit of their text.
func TestTheDrainResolvesAnIDWrittenBeforeItsQuestionSettles(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createRawSpec(t, database, ":::ask{#q1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nAnd see #q1.\n")
	comment := createRawComment(t, database, "Answered by #q1.")
	settle(t, database, spec)

	reconcileBlockIndex(t, service, "")

	for _, ref := range refsFrom(t, database, "comment", comment) {
		if ref.ToKind == "ask" {
			t.Fatalf("the comment names ask %s before the question settled, want no edge", ref.ToID)
		}
	}
	if owed := owedTexts(t, database); len(owed) != 0 {
		t.Fatalf("the queue holds %v before the settlement, want it empty", owed)
	}

	ask := settleQuestion(t, database, spec, "q1", "Ship it?")
	tick := reconcileBlockIndex(t, service, "")

	if tick.OwedTexts != 0 {
		t.Fatalf("tick = %#v, want the settlement's scope drained", tick)
	}
	assertRef(t, database, "comment", comment, "ask", ask, refsGrammarWritten)
	// The document's own `#q1` is re-indexed by the batch that owns a document's index, which the
	// scope drain owed it to.
	if next := reconcileBlockIndex(t, service, ""); next.Documents != 1 {
		t.Fatalf("the tick after the scope drain = %#v, want the document it owed an index indexed", next)
	}
	assertRef(t, database, "artifact", spec, "ask", ask, refsGrammarWritten)
}

// A document the release before this one indexed is stored at its latest version with that
// release's grammar, so its row of the queue is all it has: the drain owes it a block index, which
// is the re-index of its whole text, and the batch that owns a document's index writes it on the
// next tick, with the bare key its text writes and the excerpt the block holding it is cut to.
func TestTheDrainOwesADocumentIndexedByAnEarlierGrammar(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	var notes string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&notes); err != nil {
		t.Fatalf("create the notes: %v", err)
	}
	seedServiceText(t, service, notes, "Ship it after CORE-2.\n")
	live, _, err := service.TextWithBlocks(ctx, notes)
	if err != nil {
		t.Fatalf("read the notes: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at, authors)
		values ($1, 1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, notes, live, refsGrammarWritten); err != nil {
		t.Fatalf("write version 1 of the notes: %v", err)
	}
	settle(t, database, notes)
	// The earlier release indexed that version: its blocks are recorded, and the bare key its text
	// writes is no edge, since its grammar read none.
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = 1 where artifact_id = $1
	`, notes); err != nil {
		t.Fatalf("index the version as the earlier release does: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		delete from refs_grammar_owed where id = $1::uuid and kind = 'scope'
	`, notes); err != nil {
		t.Fatalf("settle the scope the earlier release's index leaves: %v", err)
	}
	if owed := owedTexts(t, database); len(owed) != 1 || owed[0] != "artifact "+notes {
		t.Fatalf("the queue holds %v, want the document alone", owed)
	}

	first := reconcileBlockIndex(t, service, "")

	if first.Texts != 1 || first.Documents != 0 || first.OwedTexts != 0 || first.OwedDocuments != 1 {
		t.Fatalf("first tick = %#v, want the row drained into the block index it owes", first)
	}
	second := reconcileBlockIndex(t, service, "")
	if second.Documents != 1 || second.OwedDocuments != 0 {
		t.Fatalf("second tick = %#v, want the document it owed an index indexed", second)
	}
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	holder := holderOf(t, service, notes, "CORE-2")
	var excerptVersion *int
	var excerptBlock *string
	if err := database.Pool.QueryRow(ctx, `
		select excerpt_version, excerpt_block_id from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'CORE-2'
	`, notes).Scan(&excerptVersion, &excerptBlock); err != nil {
		t.Fatalf("read the document mention's excerpt: %v", err)
	}
	if excerptVersion == nil || *excerptVersion != 1 || excerptBlock == nil || *excerptBlock != holder {
		t.Fatalf("the document mention's excerpt is block %v at version %v, want block %s at version 1",
			excerptBlock, excerptVersion, holder)
	}
}
