// Package texttrees is the stored display tree of a stored text: the text_trees table, written by
// every path that stores a text and read by every path that answers one (migration
// 20261105121325).
//
// It is a package of its own, depending on store, pmdoc and model and on nothing else of the
// server, because neither of the two packages that write stored texts can own it: api and docs
// both write them, and docs cannot import api. Everything either of them needs is here, so a
// third writer - the architecture importer is one - reaches the table the same way, and the one
// list of an ask's stored texts a reader and the background fill must agree on lives here too
// (AskShownTexts).
package texttrees

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Table is the text_trees table over one connection pool. It is a value, not a pointer: a caller
// holds a pool already and builds one of these where it needs it, so no server has a field for it
// and no test has one to wire.
type Table struct {
	pool *store.Pool
}

// New is the table over pool. A nil pool - a server built with no database, which every route
// refuses anyway - answers errNoPool from every method rather than panicking: storing a tree is
// never allowed to fail the write it belongs to.
func New(pool *store.Pool) Table { return Table{pool: pool} }

// For is the table over the pool of st, the store a server or a service was built with, and over
// no pool at all where that store is nil - a test builds one, and every route of it refuses
// anyway - so a caller unwraps no pool of its own and a nil store costs a refusal rather than a
// panic.
func For(st *store.Store) Table {
	if st == nil {
		return Table{}
	}
	return New(st.Pool)
}

// errNoPool is every method's answer on a table with no pool behind it.
var errNoPool = errors.New("texttrees: no database pool")

// Sum names the text one tree is of: pmdoc.TextSum, the key that package holds its own cache of
// read trees under, so a row of this table and an entry of that cache are found by the same
// bytes and the two agree on what one text is.
func Sum(text string) [32]byte { return pmdoc.TextSum(text) }

// StoreFor stores the display trees of texts through st's pool, under the one policy every path
// that writes a stored text keeps (api's storeDisplayTrees, docs' storeVersionTrees,
// architecture's storeComponentTrees): the store stands behind that write's own commit, never
// inside its transaction, and what it answers is logged by its caller under a subject of that
// caller's and changes no answer, because a tree that cannot be stored costs a later reader a
// markdown read and nothing more.
//
// It stores nothing and answers nil in the two cases that are no failure. There is no text to
// store; or the caller still holds a pooled connection, where taking a second one is the
// deadlock store.ErrNestedAcquire refuses, and work this optional asks rather than being
// refused, which would log the refusal's stack in a path that is working as intended.
//
// A nil store - one a test builds with no database - answers errNoPool, as a nil pool does.
func StoreFor(ctx context.Context, st *store.Store, mode pmdoc.DisplayMode, texts ...string) error {
	if len(texts) == 0 || store.HoldsPooledConnection(ctx) {
		return nil
	}
	return For(st).Store(ctx, mode, texts...)
}

// Store reads each of texts for display in mode and stores the trees, keyed by the text's Sum, the
// mode and the display parser version this process runs at (pmdoc.DisplayTreeVersionNow). A text
// whose tree is already stored keeps the stored one: the key is the reading, so two rows for one
// key would hold the same bytes.
//
// The whole batch is one statement. A request that writes several texts - an ask's question and
// every option's label and description - stores them in one round trip, and a text repeated among
// them is sent once.
//
// An empty text has no tree (pmdoc.DisplayJSON) and no row, and neither has one whose encoding
// failed: a reader shown no tree falls back to the markdown beside it, exactly as it does for a
// text no row holds yet.
func (t Table) Store(ctx context.Context, mode pmdoc.DisplayMode, texts ...string) error {
	if t.pool == nil {
		return errNoPool
	}
	sums := make([][]byte, 0, len(texts))
	trees := make([][]byte, 0, len(texts))
	seen := make(map[[32]byte]struct{}, len(texts))
	for _, text := range texts {
		tree := pmdoc.DisplayTreeToStore(text, mode)
		if tree == nil {
			continue
		}
		sum := Sum(text)
		if _, repeated := seen[sum]; repeated {
			continue
		}
		seen[sum] = struct{}{}
		sums = append(sums, sum[:])
		trees = append(trees, tree)
	}
	if len(sums) == 0 {
		return nil
	}
	if _, err := t.pool.Exec(ctx, `
		insert into text_trees (sum, mode, version, tree)
		select stored.sum, $1, $2, stored.tree
		from unnest($3::bytea[], $4::bytea[]) as stored (sum, tree)
		on conflict do nothing
	`, int16(mode), pmdoc.DisplayTreeVersionNow(), sums, trees); err != nil {
		return fmt.Errorf("store %d display trees: %w", len(sums), err)
	}
	return nil
}

// Read is the stored tree of each of sums read in mode, by the sum it is stored under, holding
// only the sums the table answered for: a sum with no row, or whose row was stored at another
// display parser version, is simply absent, and its caller reads that text for display itself.
func (t Table) Read(ctx context.Context, mode pmdoc.DisplayMode, sums [][32]byte) (map[[32]byte]json.RawMessage, error) {
	if t.pool == nil {
		return nil, errNoPool
	}
	if len(sums) == 0 {
		return nil, nil
	}
	keys := make([][]byte, len(sums))
	for index := range sums {
		keys[index] = sums[index][:]
	}
	rows, err := t.pool.Query(ctx, `
		select sum, tree from text_trees where version = $1 and mode = $2 and sum = any($3)
	`, pmdoc.DisplayTreeVersionNow(), int16(mode), keys)
	if err != nil {
		return nil, fmt.Errorf("read %d display trees: %w", len(sums), err)
	}
	defer rows.Close()
	stored := make(map[[32]byte]json.RawMessage, len(sums))
	for rows.Next() {
		var sum, tree []byte
		if err := rows.Scan(&sum, &tree); err != nil {
			return nil, fmt.Errorf("read a stored display tree: %w", err)
		}
		// A row's sum is a key's own 32 bytes, so a row of any other width is a row this package
		// did not write and no caller can look up.
		if len(sum) != sha256.Size {
			continue
		}
		stored[[32]byte(sum)] = tree
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %d display trees: %w", len(sums), err)
	}
	return stored, nil
}
