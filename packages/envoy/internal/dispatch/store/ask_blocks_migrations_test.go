package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// beforeAskBlocks is the last migration before an ask's parts are blocks, the highest numbered
// one the set holds (0161_delivery_run_warnings): 20261101143211_ask_blocks and
// 20261101143212_asks_block_parts_trigger are timestamped, so they sort after every number and
// apply after it. askBlockTables is 20261101143211, which creates the tables 20261101143212
// attaches its trigger to and seeds. A numbered migration that merges above this one moves it:
// it must name the version this pair applies directly after.
const (
	beforeAskBlocks = 161
	askBlockTables  = 20261101143211
)

// askBlockState is an ask's row of ask_block_state.
type askBlockState struct {
	found               bool
	owedAt              *time.Time
	revision, nextBlock int
}

// readAskBlockState reads an ask's state row: its owed_at, revision and next block id.
func readAskBlockState(t *testing.T, store *Store, id string) askBlockState {
	t.Helper()
	var state askBlockState
	err := store.Pool.QueryRow(context.Background(),
		`select owed_at, revision, next_block from ask_block_state where ask_id = $1`, id).
		Scan(&state.owedAt, &state.revision, &state.nextBlock)
	if errors.Is(err, pgx.ErrNoRows) {
		return state
	}
	if err != nil {
		t.Fatalf("read ask block state(%s): %v", id, err)
	}
	state.found = true
	return state
}

func assertAskOwed(t *testing.T, store *Store, what, id string) askBlockState {
	t.Helper()
	state := readAskBlockState(t, store, id)
	if !state.found || state.owedAt == nil {
		t.Fatalf("%s: ask %s state row found %v, owed_at %v; want it owed", what, id, state.found, state.owedAt)
	}
	return state
}

// An ask a build without the ask block index writes, during the roll or after a rollback, is owed
// an index that build cannot give it: on insert and on a write of any of the three columns its
// parts are read from, question, options and answer. A write of another column owes nothing, and a
// write to an ask already indexed moves only owed_at, keeping its revision and next block id. A
// block-aware build's own index, in the write's transaction, leaves the ask indexed, and the
// reconciliation's owed read is served by the partial index.
func TestAnOlderBuildsWriteLeavesTheAskBlocksOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedBlockIndexIssue(t, store)

	ask := insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which transport?')
		returning id::text`)
	if state := assertAskOwed(t, store, "an inserted ask", ask); state.revision != 0 || state.nextBlock != 1 {
		t.Fatalf("an inserted ask: revision %d, next_block %d; want 0 and 1", state.revision, state.nextBlock)
	}

	// The index of a block-aware build settles it at revision 1 with the question and two option
	// blocks minted, so next_block is 4.
	indexed := func() {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			update ask_block_state set owed_at = null, revision = 1, next_block = 4 where ask_id = $1
		`, ask); err != nil {
			t.Fatalf("index ask: %v", err)
		}
	}
	// The trigger names three columns, so each is written on its own: the question, the options
	// an answerer picks from, and the answer, whose part the answerer made.
	for _, write := range []struct{ what, statement string }{
		{"an edited question", `update asks set question = 'Which transport ships first?' where id = $1`},
		{"edited options", `update asks set options = '[{"label":"REST"},{"label":"gRPC"}]'::jsonb where id = $1`},
		{"an answer", `update asks set answer = '{"text":"REST"}'::jsonb, state = 'answered' where id = $1`},
	} {
		indexed()
		if _, err := store.Pool.Exec(ctx, write.statement, ask); err != nil {
			t.Fatalf("%s: %v", write.what, err)
		}
		if state := assertAskOwed(t, store, write.what, ask); state.revision != 1 || state.nextBlock != 4 {
			t.Fatalf("%s: revision %d, next_block %d; want 1 and 4 kept", write.what, state.revision, state.nextBlock)
		}
	}

	indexed()
	if _, err := store.Pool.Exec(ctx, `update asks set urgency = 'high' where id = $1`, ask); err != nil {
		t.Fatalf("raise the ask's urgency: %v", err)
	}
	if state := readAskBlockState(t, store, ask); state.owedAt != nil {
		t.Fatalf("raising an ask's urgency owed it again at %v, want only its question, options or answer to", *state.owedAt)
	}

	// A write to an ask already owed moves its owed_at, so a drain that read the earlier time
	// leaves the row owed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `update ask_block_state set owed_at = $2 where ask_id = $1`, ask, past); err != nil {
		t.Fatalf("age the owed ask: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update asks set question = 'Which transport ships at all?' where id = $1`, ask); err != nil {
		t.Fatalf("edit owed ask: %v", err)
	}
	if state := assertAskOwed(t, store, "an owed ask edited again", ask); !state.owedAt.After(past) {
		t.Fatalf("an owed ask edited again kept owed_at %v, want it moved past %v", *state.owedAt, past)
	}

	// A block-aware build opens the ask and indexes it in one transaction.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var settled string
	if err := tx.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which region?')
		returning id::text`).Scan(&settled); err != nil {
		t.Fatalf("insert indexed ask: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		update ask_block_state set owed_at = null, revision = 1, next_block = 2 where ask_id = $1
	`, settled); err != nil {
		t.Fatalf("index ask in its transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit indexed ask: %v", err)
	}
	if state := readAskBlockState(t, store, settled); !state.found || state.owedAt != nil || state.revision != 1 {
		t.Fatalf("a block-aware build's ask: %+v, want indexed at revision 1", state)
	}

	const owedAsks = `select ask_id, owed_at from ask_block_state where owed_at is not null order by owed_at limit 200`
	if plan := explainWithoutSeqScan(t, store, owedAsks); !strings.Contains(plan, "ask_block_state_owed") {
		t.Errorf("owed asks: plan does not read ask_block_state_owed:\n%s", plan)
	}
}

// Every ask stored before the ask block index is owed one by the migrations: an ask written
// before 20261101143211, and one written after 20261101143211 and before the trigger that owes
// it (an older build's write during the migrations), each gets an owed state row.
func TestTheAskBlockMigrationsOweEveryStoredAsk(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeAskBlocks)
	seedBlockIndexIssue(t, store)
	insertAsk := func(question string) string {
		return insertReturningID(t, store, `
			insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text`, question)
	}
	olderAsk := insertAsk("Older?")

	migrateThrough(t, store, askBlockTables)
	racedAsk := insertAsk("Raced?")

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, ask := range []struct{ what, id string }{
		{"an ask stored before 20261101143211", olderAsk},
		{"an ask written between 20261101143211 and its trigger", racedAsk},
	} {
		if state := assertAskOwed(t, store, ask.what, ask.id); state.revision != 0 || state.nextBlock != 1 {
			t.Errorf("%s: revision %d, next_block %d; want 0 and 1", ask.what, state.revision, state.nextBlock)
		}
	}
}
