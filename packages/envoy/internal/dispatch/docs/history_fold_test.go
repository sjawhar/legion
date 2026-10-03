package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A stored history folds into a state that reads exactly as the whole history merged does, at its
// head, at every version before it, and after compactions that fold part or all of it: the same
// markdown, the same tree with every block id and mark, the same margin records. Each document is
// a pmdoc fixture's browser encoding followed by what deletes under it - every block replaced by
// another fixture's and back, a margin record projected over itself, and a second client's insert
// written against the first state, which the replacement already deleted around.
func TestAFoldedHistoryReadsAsTheMergedHistory(t *testing.T) {
	fixtures := readHistoryFixtures(t)
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	for index, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			history := documentHistory(t, fixture.update, fixtures[(index+1)%len(fixtures)].update)
			artifactID := createDocument(t, database, "history")
			for _, update := range history {
				if _, err := versioned.AppendUpdate(ctx, artifactID, update); err != nil {
					t.Fatalf("append update: %v", err)
				}
			}
			want := readMerged(t, history)
			for version := 1; version <= len(history); version++ {
				state, err := versioned.MaterializeAt(ctx, artifactID, persistence.Version(version))
				if err != nil {
					t.Fatalf("materialize version %d: %v", version, err)
				}
				requireReadsAs(t, fmt.Sprintf("version %d", version), state, readMerged(t, history[:version]))
			}
			requireLoadReadsAs(t, versioned, artifactID, "the head", want)

			if _, err := versioned.Compact(ctx, artifactID, 2); err != nil {
				t.Fatalf("compact to two updates: %v", err)
			}
			for version := len(history) - 1; version <= len(history); version++ {
				state, err := versioned.MaterializeAt(ctx, artifactID, persistence.Version(version))
				if err != nil {
					t.Fatalf("materialize version %d after compaction: %v", version, err)
				}
				requireReadsAs(t, fmt.Sprintf("version %d after compaction to two", version), state, readMerged(t, history[:version]))
			}
			if _, err := versioned.Compact(ctx, artifactID, compactKeep); err != nil {
				t.Fatalf("compact: %v", err)
			}
			if rows := storedUpdateCount(t, database, artifactID); rows != 1 {
				t.Fatalf("compaction left %d stored updates, want 1", rows)
			}
			requireLoadReadsAs(t, versioned, artifactID, "the compacted head", want)
		})
	}
}

// Compaction leaves the state and none of the content a later update deleted: a margin record
// projected forty times over itself is stored as the one record it is, and so is the load of the
// history before compaction.
func TestCompactionLeavesTheStateNotTheContentItDeleted(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	artifactID := createDocument(t, database, "history")
	const recordBytes = 64 << 10
	writer := crdt.New()
	marks := writer.GetMap(marksMapName)
	var history [][]byte
	for projection := range 40 {
		before := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) {
			marks.Set(txn, "c1", map[string]any{"text": strings.Repeat(fmt.Sprint(projection%10), recordBytes)})
		})
		history = append(history, crdt.EncodeStateAsUpdateV1(writer, before))
	}
	for _, update := range history {
		if _, err := versioned.AppendUpdate(ctx, artifactID, update); err != nil {
			t.Fatalf("append update: %v", err)
		}
	}
	const bound = recordBytes + 16<<10
	if stored := storedUpdateBytes(t, database, artifactID); stored < 40*recordBytes {
		t.Fatalf("history stored %d bytes, want the forty projections it wrote", stored)
	}
	loaded, err := versioned.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Update) > bound {
		t.Fatalf("load returned %d bytes, want the one record's state, at most %d", len(loaded.Update), bound)
	}
	want := readMerged(t, history)
	requireReadsAs(t, "the load", loaded.Update, want)
	if _, err := versioned.Compact(ctx, artifactID, compactKeep); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stored := storedUpdateBytes(t, database, artifactID); stored > bound {
		t.Fatalf("compaction stored %d bytes, want the one record's state, at most %d", stored, bound)
	}
	requireLoadReadsAs(t, versioned, artifactID, "the compacted load", want)
}

// A room parks an update whose dependency has not arrived, and a delete of an item it does not
// hold, until a peer sends what they wait on. A stored log can hold both - an update its writer
// persisted before one it depends on that never was - so a load and a compaction keep them, and
// once the missing updates arrive the parked insert shows and the parked delete holds.
func TestALoadAndACompactionKeepWhatTheDocumentParked(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()

	writer := crdt.New(crdt.WithClientID(7))
	text := writer.GetText("t")
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "base", nil) })
	base := crdt.EncodeStateAsUpdateV1(writer, nil)
	afterBase := writer.StateVector()
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 4, " lost", nil) })
	lost := crdt.EncodeStateAsUpdateV1(writer, afterBase)
	afterLost := writer.StateVector()
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 9, " kept", nil) })
	parkedInsert := crdt.EncodeStateAsUpdateV1(writer, afterLost)

	other := crdt.New(crdt.WithClientID(8))
	if err := crdt.ApplyUpdateV1(other, base, nil); err != nil {
		t.Fatalf("open the second writer: %v", err)
	}
	otherText := other.GetText("t")
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "gone ", nil) })
	gone := crdt.EncodeStateAsUpdateV1(other, afterBase)
	afterGone := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherText.Delete(txn, 0, 5) })
	parkedDelete := crdt.EncodeStateAsUpdateV1(other, afterGone)

	stored := [][]byte{base, parkedInsert, parkedDelete}
	artifactID := createDocument(t, database, "parked")
	for _, update := range stored {
		if _, err := versioned.AppendUpdate(ctx, artifactID, update); err != nil {
			t.Fatalf("append update: %v", err)
		}
	}
	arrive := func(t *testing.T, what string, state []byte) {
		t.Helper()
		doc := crdt.New()
		for _, update := range [][]byte{state, lost, gone} {
			if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatalf("%s: apply: %v", what, err)
			}
		}
		if got := doc.GetText("t").ToString(); got != "base lost kept" {
			t.Fatalf("%s, once the missing updates arrive, reads %q, want %q", what, got, "base lost kept")
		}
	}
	merged, err := crdt.MergeUpdatesV1(stored...)
	if err != nil {
		t.Fatalf("merge the stored updates: %v", err)
	}
	arrive(t, "the merged log", merged)
	loaded, err := versioned.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	arrive(t, "the load", loaded.Update)
	if _, err := versioned.Compact(ctx, artifactID, compactKeep); err != nil {
		t.Fatalf("compact: %v", err)
	}
	compacted, err := versioned.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load the compacted log: %v", err)
	}
	arrive(t, "the compacted load", compacted.Update)
}

type historyFixture struct {
	Name   string `json:"name"`
	B64    string `json:"yjs_update_v1_b64"`
	update []byte
}

// readHistoryFixtures reads every pmdoc fixture's browser encoding (pmdoc/testdata/fixtures.json).
func readHistoryFixtures(t *testing.T) []historyFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "pmdoc", "testdata", "fixtures.json"))
	if err != nil {
		t.Fatalf("read pmdoc fixtures: %v", err)
	}
	var fixtures []historyFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode pmdoc fixtures: %v", err)
	}
	if len(fixtures) < 2 {
		t.Fatalf("pmdoc fixtures hold %d documents, want several", len(fixtures))
	}
	for index := range fixtures {
		fixtures[index].update, err = base64.StdEncoding.DecodeString(fixtures[index].B64)
		if err != nil {
			t.Fatalf("decode fixture %s: %v", fixtures[index].Name, err)
		}
	}
	return fixtures
}

// documentHistory is the stored log of a document that starts as initial and is rewritten into
// other and back, as a room stores one: each update is what one transaction added since the last.
func documentHistory(t *testing.T, initial, other []byte) [][]byte {
	t.Helper()
	server := crdt.New(crdt.WithClientID(1001))
	if err := crdt.ApplyUpdateV1(server, initial, nil); err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	fragment := server.GetXmlFragment(fragmentName)
	marks := server.GetMap(marksMapName)
	initialTree, err := pmdoc.Read(fragment)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	otherDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(otherDoc, other, nil); err != nil {
		t.Fatalf("open the other fixture: %v", err)
	}
	otherTree, err := pmdoc.Read(otherDoc.GetXmlFragment(fragmentName))
	if err != nil {
		t.Fatalf("read the other fixture: %v", err)
	}
	// The second client starts where the stored log does and writes into its first text.
	browser := crdt.New(crdt.WithClientID(2002))
	if err := crdt.ApplyUpdateV1(browser, initial, nil); err != nil {
		t.Fatalf("open the second client: %v", err)
	}
	browserText := firstXMLText(browser.GetXmlFragment(fragmentName))

	history := [][]byte{initial}
	step := func(what string, change func(*crdt.Transaction) error) {
		t.Helper()
		before := server.StateVector()
		if err := server.TransactE(change); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		history = append(history, crdt.EncodeStateAsUpdateV1(server, before))
	}
	step("rewrite into the other fixture", func(txn *crdt.Transaction) error {
		return pmdoc.Update(txn, fragment, otherTree)
	})
	for replies := 1; replies <= 3; replies++ {
		step("project the margin record", func(txn *crdt.Transaction) error {
			record := map[string]any{"kind": "comment", "text": "note", "replies": strings.Repeat("reply ", replies)}
			marks.Set(txn, "c1", record)
			return nil
		})
	}
	if browserText != nil {
		before := browser.StateVector()
		browser.Transact(func(txn *crdt.Transaction) { browserText.Insert(txn, 0, "concurrent ", nil) })
		concurrent := crdt.EncodeStateAsUpdateV1(browser, before)
		if err := crdt.ApplyUpdateV1(server, concurrent, nil); err != nil {
			t.Fatalf("receive the second client's write: %v", err)
		}
		history = append(history, concurrent)
	}
	step("rewrite back into the fixture", func(txn *crdt.Transaction) error {
		return pmdoc.Update(txn, fragment, initialTree)
	})
	return history
}

// firstXMLText is the first text node in document order, or nil for a document without one.
func firstXMLText(fragment *crdt.YXmlFragment) *crdt.YXmlText {
	for _, child := range fragment.Children() {
		switch node := child.(type) {
		case *crdt.YXmlText:
			return node
		case *crdt.YXmlElement:
			if text := firstXMLText(&node.YXmlFragment); text != nil {
				return text
			}
		}
	}
	return nil
}

// documentReading is everything a reader of a document state sees: its markdown (or why it has
// none), its whole tree with block ids and marks, and its margin records.
type documentReading struct {
	markdown string
	tree     string
	marks    string
}

func readState(t *testing.T, what string, state []byte) documentReading {
	t.Helper()
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("%s: apply the state: %v", what, err)
	}
	var reading documentReading
	tree, err := treeOf(doc)
	if err != nil {
		reading.tree = "unreadable: " + err.Error()
	} else {
		if encoded, err := tree.JSON(); err != nil {
			reading.tree = "invalid: " + err.Error()
		} else {
			reading.tree = string(encoded)
		}
		if markdown, err := renderTree(tree); err != nil {
			reading.markdown = "unrenderable: " + err.Error()
		} else {
			reading.markdown = markdown
		}
	}
	marks, err := doc.GetMap(marksMapName).ToJSON()
	if err != nil {
		t.Fatalf("%s: read the margin records: %v", what, err)
	}
	reading.marks = string(marks)
	return reading
}

// readMerged reads the state the whole of history makes when merged, every byte it inserted kept.
func readMerged(t *testing.T, history [][]byte) documentReading {
	t.Helper()
	merged, err := crdt.MergeUpdatesV1(history...)
	if err != nil {
		t.Fatalf("merge the history: %v", err)
	}
	return readState(t, "the merged history", merged)
}

func requireReadsAs(t *testing.T, what string, state []byte, want documentReading) {
	t.Helper()
	got := readState(t, what, state)
	if got.markdown != want.markdown {
		t.Fatalf("%s renders\n%q\nwant\n%q", what, got.markdown, want.markdown)
	}
	if got.tree != want.tree {
		t.Fatalf("%s holds the tree\n%s\nwant\n%s", what, got.tree, want.tree)
	}
	if got.marks != want.marks {
		t.Fatalf("%s holds the margin records %s, want %s", what, got.marks, want.marks)
	}
}

func requireLoadReadsAs(t *testing.T, versioned *PgVersioned, artifactID, what string, want documentReading) {
	t.Helper()
	loaded, err := versioned.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load %s: %v", what, err)
	}
	requireReadsAs(t, what, loaded.Update, want)
}

func storedUpdateCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&rows); err != nil {
		t.Fatalf("count stored updates: %v", err)
	}
	return rows
}

func storedUpdateBytes(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var bytes int
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(sum(octet_length(update)), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&bytes); err != nil {
		t.Fatalf("sum stored updates: %v", err)
	}
	return bytes
}
