package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// beforeBlockIDKey is the last migration before 0126_asks_block_id_key and
// 0127_comments_anchor_block_id_key rebuild the block id indexes over block_id_key.
const beforeBlockIDKey = 125

// incompressibleID is an id of length random hex characters, so no index holds it by compressing
// it.
func incompressibleID(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, (length+1)/2)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)[:length]
}

// writeAskBlock writes the row of an ask block blockID on document, as settlement opens one.
func writeAskBlock(store *Store, document, blockID string) error {
	_, err := store.Pool.Exec(context.Background(), `
		insert into asks (issue_key, block_id, block_artifact_id, author, question)
		values ('CORE-1', $2, $1, '{"kind":"user","id":"alice"}', 'Ship it?')
	`, document, blockID)
	return err
}

// writeAnchored writes a comment (table comments) or an ask (asks) anchored inside block blockID
// of document.
func writeAnchored(store *Store, table, document, blockID string) error {
	text := "body"
	if table == "asks" {
		text = "question"
	}
	_, err := store.Pool.Exec(context.Background(), `
		insert into `+table+` (issue_key, author, `+text+`, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Anchored.',
			jsonb_build_object('artifact_id', $1::text, 'block_id', $2::text, 'mark_id', 'm', 'version', 1, 'quote', 'q'))
	`, document, blockID)
	return err
}

// sqlState is err's SQLSTATE and constraint, empty for no Postgres error.
func sqlState(err error) (string, string) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", ""
	}
	return pgErr.Code, pgErr.ConstraintName
}

// A block id is a string a browser's edit can make any length. Before 0126 and 0127 an
// incompressible id past a B-tree entry's 2704 bytes fails the write of an ask block's row and of
// an anchor (SQLSTATE 54000); a long id that compresses, which main stores, does not. The
// migrations apply over the rows main wrote, those long ids included; afterwards an incompressible
// 3200-character id is written in all three places, every id is found by what reads it through
// the indexes, (block_artifact_id, block_id) stays unique, a row main wrote takes an edit, and ids
// differing only in a backslash or a non-ASCII character are different keys.
func TestTheBlockIDKeyIndexesHoldIDsPastTheKeyLimit(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeBlockIDKey)
	seedBlockIndexIssue(t, store)
	document := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text`)
	other := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`)
	compressible := strings.Repeat("q", 6000)
	incompressible := incompressibleID(t, 3200)

	for _, id := range []string{"decision", compressible} {
		if err := writeAskBlock(store, document, id); err != nil {
			t.Fatalf("main writes the ask block %.20q… (%d bytes): %v", id, len(id), err)
		}
		for _, table := range []string{"comments", "asks"} {
			if err := writeAnchored(store, table, document, id); err != nil {
				t.Fatalf("main writes a %s anchor in block %.20q… (%d bytes): %v", table, id, len(id), err)
			}
		}
	}
	if code, _ := sqlState(writeAskBlock(store, document, incompressible)); code != "54000" {
		t.Fatalf("before 0126 an ask block of 3200 random hex characters answers SQLSTATE %q, want 54000", code)
	}
	for _, table := range []string{"comments", "asks"} {
		if code, _ := sqlState(writeAnchored(store, table, document, incompressible)); code != "54000" {
			t.Fatalf("before 0126 a %s anchor in a block of 3200 random hex characters answers SQLSTATE %q, want 54000", table, code)
		}
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate over the rows main wrote: %v", err)
	}

	if err := writeAskBlock(store, document, incompressible); err != nil {
		t.Fatalf("write the ask block of 3200 random hex characters: %v", err)
	}
	for _, table := range []string{"comments", "asks"} {
		if err := writeAnchored(store, table, document, incompressible); err != nil {
			t.Fatalf("write a %s anchor in a block of 3200 random hex characters: %v", table, err)
		}
	}
	for _, id := range []string{"decision", compressible, incompressible} {
		var found int
		if err := store.Pool.QueryRow(ctx, `
			select count(*) from asks a
			where a.block_artifact_id = $1 and block_id_key(a.block_id) = block_id_key($2) and a.block_id = $2
		`, document, id).Scan(&found); err != nil || found != 1 {
			t.Fatalf("the ask block %.20q… (%d bytes) is found %d times (%v), want once", id, len(id), found, err)
		}
		for _, table := range []string{"comments", "asks"} {
			if err := store.Pool.QueryRow(ctx, `
				select count(*) from `+table+`
				where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' = $2
			`, document, id).Scan(&found); err != nil || found != 1 {
				t.Fatalf("the %s anchor in block %.20q… (%d bytes) is found %d times (%v), want once", table, id, len(id), found, err)
			}
		}
		if code, constraint := sqlState(writeAskBlock(store, document, id)); code != "23505" || constraint != "asks_block_id_unique" {
			t.Fatalf("a second ask block %.20q… (%d bytes) on the document answers %q on %q, want 23505 on asks_block_id_unique", id, len(id), code, constraint)
		}
	}
	if err := writeAskBlock(store, other, incompressible); err != nil {
		t.Fatalf("another document's ask block under the same long id: %v", err)
	}
	if tag, err := store.Pool.Exec(ctx, `
		update asks set question = 'Ship it now?' where block_artifact_id = $1 and block_id = $2
	`, document, compressible); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("edit the ask block main wrote under a long id: %v rows (%v), want 1", tag.RowsAffected(), err)
	}

	// block_id_key is the SHA-256 of the id's UTF-8 bytes, so ids differing only in a backslash or
	// an accent are different keys, and each is an ask block of its own on one document.
	for _, id := range []string{`a\b`, `a\\b`, `a\x41`, `\101`, "décision", "decision\\"} {
		var key []byte
		if err := store.Pool.QueryRow(ctx, `select block_id_key($1)`, id).Scan(&key); err != nil {
			t.Fatalf("block_id_key(%q): %v", id, err)
		}
		if want := sha256.Sum256([]byte(id)); string(key) != string(want[:]) {
			t.Fatalf("block_id_key(%q) = %x, want the SHA-256 of its UTF-8 bytes %x", id, key, want)
		}
		if err := writeAskBlock(store, document, id); err != nil {
			t.Fatalf("write the ask block %q beside its look-alikes: %v", id, err)
		}
	}

	for name, read := range map[string]struct{ query, index string }{
		"an ask block by id": {`select a.id from asks a where a.block_artifact_id = '` + document +
			`' and block_id_key(a.block_id) = block_id_key('decision') and a.block_id = 'decision'`, "asks_block_id_unique"},
		"a document's ask blocks": {`select a.id from asks a where a.block_artifact_id = '` + document +
			`' and a.block_id is not null`, "asks_block_id_unique"},
		"a document's anchored comments": {`select anchor->>'block_id' from comments where anchor->>'artifact_id' = '` + document +
			`' and anchor->>'block_id' is not null and anchor->>'block_id' <> ''`, "comments_anchor_block_id_idx"},
		"a document's anchored asks": {`select anchor->>'block_id' from asks where anchor->>'artifact_id' = '` + document +
			`' and anchor->>'block_id' is not null and anchor->>'block_id' <> ''`, "asks_anchor_block_id_idx"},
	} {
		if plan := explainWithoutSeqScan(t, store, read.query); !strings.Contains(plan, read.index) {
			t.Errorf("%s: plan does not read %s:\n%s", name, read.index, plan)
		}
	}
}
