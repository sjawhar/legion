package docs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
)

// This fixture supports nonconsecutive stored versions as well as ordinary histories.
type storedOrderHistoryTx struct {
	pgx.Tx
	updates  [][]byte
	versions []int64
}

func (tx *storedOrderHistoryTx) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	after, through := int64(-1), int64(1<<62)
	if len(args) > 1 {
		through = args[1].(int64)
	}
	if len(args) > 2 {
		after = args[2].(int64)
	}
	var indices []int
	for i, version := range tx.versions {
		if version > after && version <= through {
			indices = append(indices, i)
		}
	}
	return &storedOrderHistoryRows{tx: tx, indices: indices, full: strings.Contains(query, "content_changed"), pos: -1}, nil
}

type storedOrderHistoryRows struct {
	pgx.Rows
	tx      *storedOrderHistoryTx
	indices []int
	full    bool
	pos     int
}

func (r *storedOrderHistoryRows) Next() bool { r.pos++; return r.pos < len(r.indices) }
func (r *storedOrderHistoryRows) Close()     {}
func (r *storedOrderHistoryRows) Err() error { return nil }
func (r *storedOrderHistoryRows) Scan(dest ...any) error {
	i := r.indices[r.pos]
	*dest[0].(*int64) = r.tx.versions[i]
	*dest[1].(*[]byte) = r.tx.updates[i]
	if r.full {
		*dest[2].(*bool) = false
		*dest[3].(*time.Time) = time.Unix(r.tx.versions[i], 0)
	}
	return nil
}

func checkStoredOrderEquivalence(t *testing.T, updates [][]byte, gapped bool, since time.Time) {
	t.Helper()
	versions := make([]int64, len(updates))
	for i := range versions {
		versions[i] = int64(i + 1)
		if gapped {
			versions[i] = 101 + int64(i*3)
		}
	}
	tx := &storedOrderHistoryTx{updates: updates, versions: versions}
	var got, want StoredOrderReport
	wantErr := fullPrefixStoredOrder(context.Background(), tx, "replay", since, nil, &want)
	gotErr := foldStoredOrder(context.Background(), tx, "replay", since, nil, &got)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(got, want) {
		for i, update := range updates {
			t.Logf("row %d: %x", versions[i], update)
		}
		t.Fatalf("optimized replay = %+v, error %v; full-prefix replay = %+v, error %v", got, gotErr, want, wantErr)
	}
}

// Resending an overlapping parked update leaves emptied pending-queue slots. A later update
// must still apply when a comparison copies that prefix, including its unrelated parked content.
func storedOrderOverlappingHistory(t *testing.T) [][]byte {
	t.Helper()
	seed := newDocumentCopy(crdt.WithClientID(100))
	defer seed.Destroy()
	seed.Transact(typeParagraph(seed, "base"))
	return [][]byte{
		crdt.EncodeStateAsUpdateV1(seed, nil),
		storedOrderHex(t, "030109000401017401610108018408000162010701840700016200"),
		storedOrderHex(t, "01010802840801016300"),
		storedOrderHex(t, "030109000401017401610108018408000162010701840700016200"),
		storedOrderHex(t, "000107010101"),
		storedOrderHex(t, "01010801840800016200"),
		storedOrderHex(t, "010307000401017401618107000184070101630107010101"),
	}
}

func TestStoredOrderReplayWithOverlappingParkedUpdates(t *testing.T) {
	checkStoredOrderEquivalence(t, storedOrderOverlappingHistory(t), false, time.Time{})
}

func TestStoredOrderReplayMatchesFullPrefixReplay(t *testing.T) {
	seed := newDocumentCopy(crdt.WithClientID(100))
	defer seed.Destroy()
	seed.Transact(typeParagraph(seed, "base"))
	base := crdt.EncodeStateAsUpdateV1(seed, nil)
	var packets [][]byte
	for _, client := range []crdt.ClientID{7, 8, 9} {
		doc := newDocumentCopy(crdt.WithClientID(client))
		text := doc.GetText("t")
		for _, letter := range []string{"a", "b", "c"} {
			before := doc.StateVector()
			doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), letter, nil) })
			packets = append(packets, crdt.EncodeStateAsUpdateV1(doc, before))
		}
		doc.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 1, 1) })
		packets = append(packets, crdt.EncodeStateAsUpdateV1(doc, doc.StateVector()))
		packets = append(packets, crdt.EncodeStateAsUpdateV1(doc, nil))
		doc.Destroy()
	}
	packets = append(packets, mergeStoredOrder(t, packets[1], packets[6], packets[10]), []byte{0, 0})
	checks := 0
	check := func(name string, sequence [][]byte, gapped bool, since time.Time) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			checkStoredOrderEquivalence(t, append([][]byte{base}, sequence...), gapped, since)
		})
		checks++
	}
	// All orderings of two three-update chains include reverse resolutions, multiple parked
	// rows, and parked rows that depend on other parked rows. Repeat them with duplicates and gaps.
	perm := []int{0, 1, 2, 5, 6, 7}
	var visit func(int)
	visit = func(at int) {
		if at == len(perm) {
			sequence := make([][]byte, len(perm))
			for i, p := range perm {
				sequence[i] = packets[p]
			}
			check(fmt.Sprint(perm), sequence, false, time.Time{})
			sequence = append(sequence, sequence[1], []byte{0, 0}, sequence[0])
			check(fmt.Sprint(perm)+"-gapped", sequence, true, time.Time{})
			return
		}
		for i := at; i < len(perm); i++ {
			perm[at], perm[i] = perm[i], perm[at]
			visit(at + 1)
			perm[at], perm[i] = perm[i], perm[at]
		}
	}
	visit(0)
	// A fixed seed also covers mixed inserts/deletes, duplicate updates, re-inclusion snapshots,
	// no-ops, unresolved tails, and since-filtered prefixes.
	rng := rand.New(rand.NewPCG(2775, 20261104))
	for i := range 1000 {
		sequence := make([][]byte, 4+rng.IntN(15))
		for j := range sequence {
			sequence[j] = packets[rng.IntN(len(packets))]
		}
		since := time.Time{}
		if i%3 == 0 {
			since = time.Unix(int64(rng.IntN(10)), 0)
		}
		check(fmt.Sprintf("generated-%d", i), sequence, i%2 == 0, since)
	}
	t.Logf("%d histories matched the full-prefix replay", checks)
}

func TestBuildForkPreservesOverlappingParkedUpdates(t *testing.T) {
	updates := storedOrderOverlappingHistory(t)
	full := newDocumentCopy()
	defer full.Destroy()
	for _, update := range updates {
		if err := decodeStoredState(full, update); err != nil {
			t.Fatal(err)
		}
	}
	prefix := newDocumentCopy()
	defer prefix.Destroy()
	for i, update := range updates {
		if err := decodeStoredState(prefix, update); err != nil {
			t.Fatal(err)
		}
		t.Run(fmt.Sprintf("after-row-%d", i+1), func(t *testing.T) {
			fork, err := buildFork(200, crdt.EncodeStateAsUpdateV1(prefix, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer fork.Destroy()
			for _, later := range updates[i+1:] {
				if err := decodeStoredState(fork, later); err != nil {
					t.Fatal(err)
				}
			}
			if !sameStoredState(fork, full) || fork.GetText("t").ToString() != full.GetText("t").ToString() {
				t.Fatal("fork lost overlapping parked content")
			}
		})
	}
}
