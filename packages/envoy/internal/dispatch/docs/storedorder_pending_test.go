package docs

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
)

type storedOrderReplayTx struct {
	pgx.Tx
	updates  [][]byte
	classes  []bool
	prefixes []int64
}

func (tx *storedOrderReplayTx) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	through := len(tx.updates)
	if len(args) > 1 {
		tx.prefixes = append(tx.prefixes, args[1].(int64))
		through = int(args[1].(int64))
	}
	return &storedOrderReplayRows{tx: tx, full: strings.Contains(query, "content_changed"), through: through}, nil
}

type storedOrderReplayRows struct {
	pgx.Rows
	tx           *storedOrderReplayTx
	full         bool
	pos, through int
}

func (r *storedOrderReplayRows) Next() bool { r.pos++; return r.pos <= r.through }
func (r *storedOrderReplayRows) Close()     {}
func (r *storedOrderReplayRows) Err() error { return nil }
func (r *storedOrderReplayRows) Scan(dest ...any) error {
	*dest[0].(*int64) = int64(r.pos)
	*dest[1].(*[]byte) = r.tx.updates[r.pos-1]
	if r.full {
		*dest[2].(*bool) = r.tx.classes[r.pos-1]
		*dest[3].(*time.Time) = time.Unix(int64(r.pos), 0)
	}
	return nil
}

func replayStoredOrder(t *testing.T, since time.Time, updates ...[]byte) StoredOrderReport {
	t.Helper()
	tx := &storedOrderReplayTx{updates: updates}
	doc := newDocumentCopy()
	before := renderStoredFold(doc)
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatal(err)
		}
		after := renderStoredFold(doc)
		tx.classes = append(tx.classes, !before.renders || !after.renders || before.markdown != after.markdown)
		before = after
	}
	var report StoredOrderReport
	if err := foldStoredOrder(context.Background(), tx, "replay", since, nil, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func storedOrderHex(t *testing.T, value string) []byte {
	t.Helper()
	update, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func mergeStoredOrder(t *testing.T, updates ...[]byte) []byte {
	t.Helper()
	merged, err := crdt.MergeUpdatesV1(updates...)
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

func TestStoredOrderPendingRows(t *testing.T) {
	seed := newDocumentCopy(crdt.WithClientID(100))
	seed.Transact(typeParagraph(seed, "base"))
	base := crdt.EncodeStateAsUpdateV1(seed, nil)
	pair := func(client crdt.ClientID) (predecessor, dependent, combined []byte) {
		doc := newDocumentCopy(crdt.WithClientID(client))
		if err := crdt.ApplyUpdateV1(doc, base, nil); err != nil {
			t.Fatal(err)
		}
		before := doc.StateVector()
		text := doc.GetXmlFragment(fragmentName).Children()[0].(*crdt.YXmlElement).Children()[0].(*crdt.YXmlText)
		doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), " predecessor", nil) })
		predecessor = crdt.EncodeStateAsUpdateV1(doc, before)
		next := doc.StateVector()
		doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), " dependent", nil) })
		return predecessor, crdt.EncodeStateAsUpdateV1(doc, next), crdt.EncodeStateAsUpdateV1(doc, before)
	}
	pred, dep, combined := pair(7)
	otherPred, otherDep, otherCombined := pair(8)
	integrated, _, _ := pair(9)
	mixedPred, mixedDep, mixedCombined := pair(11)
	delete1 := storedOrderHex(t, "000114010101")                   // client 20, [1,2)
	delete2 := storedOrderHex(t, "000114010201")                   // client 20, [2,3)
	filler := storedOrderHex(t, "01011400040101710361626300")      // q = abc
	otherDelete := storedOrderHex(t, "000115010101")               // client 21, [1,2)
	otherFiller := storedOrderHex(t, "01011500040101720361626300") // r = abc
	p9 := storedOrderHex(t, "01010901840900016200")
	p11 := storedOrderHex(t, "01010b01840b00016400")
	f9 := storedOrderHex(t, "010109000401017402616200")
	f11 := storedOrderHex(t, "01010b0004010174016300")
	for _, test := range []struct {
		name       string
		updates    [][]byte
		mismatches []int64
		awaiting   int
		since      time.Time
	}{
		{name: "reincluded", updates: [][]byte{base, dep, combined}},
		{name: "real mismatch", updates: [][]byte{base, dep, pred}, mismatches: []int64{2}},
		{name: "unresolved tail", updates: [][]byte{base, dep}, awaiting: 1},
		{name: "unrelated earlier pending", updates: [][]byte{base, otherDep, dep, pred}, mismatches: []int64{3}, awaiting: 1},
		{name: "unrelated later pending", updates: [][]byte{base, dep, otherDep, pred}, mismatches: []int64{2}, awaiting: 1},
		{name: "reincluded beside unresolved", updates: [][]byte{base, dep, otherDep, combined}, awaiting: 1},
		{name: "reverse resolution", updates: [][]byte{base, dep, otherDep, otherPred, pred}, mismatches: []int64{3, 2}},
		{name: "separate legitimate rows", updates: [][]byte{base, dep, otherDep, combined, otherCombined}},
		{name: "mixed insert reincluded", updates: [][]byte{base, mergeStoredOrder(t, integrated, mixedDep), mixedCombined}},
		{name: "mixed insert whole row reincluded", updates: [][]byte{base, mergeStoredOrder(t, integrated, mixedDep), mergeStoredOrder(t, integrated, mixedCombined)}},
		{name: "mixed insert not reincluded", updates: [][]byte{base, mergeStoredOrder(t, integrated, mixedDep), mixedPred}, mismatches: []int64{2}},
		{name: "mixed deletions reincluded", updates: [][]byte{mergeStoredOrder(t, base, filler), mergeStoredOrder(t, delete1, otherDelete), mergeStoredOrder(t, otherFiller, otherDelete)}},
		{name: "mixed deletions not reincluded", updates: [][]byte{mergeStoredOrder(t, base, filler), mergeStoredOrder(t, delete1, otherDelete), otherFiller}, mismatches: []int64{2}},
		{name: "only bad row blamed", updates: [][]byte{base, p9, p11, f9, f11}, mismatches: []int64{3}},
		{name: "different pending deletions", updates: [][]byte{mergeStoredOrder(t, base, delete1), mergeStoredOrder(t, p9, delete2), f9, filler}, mismatches: []int64{2}},
		{name: "delete range grows without count growth", updates: [][]byte{mergeStoredOrder(t, base, delete1), delete2, filler}, mismatches: []int64{2}},
		{name: "duplicate pending row", updates: [][]byte{base, dep, dep}, awaiting: 1},
		{name: "unchecked pending row", updates: [][]byte{base, dep, pred}, since: time.Unix(3, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := replayStoredOrder(t, test.since, test.updates...)
			var mismatches []int64
			for _, mismatch := range report.Mismatches {
				if mismatch.Kind != StoredOrderPending {
					t.Fatalf("unexpected mismatch: %+v", mismatch)
				}
				mismatches = append(mismatches, mismatch.Update)
			}
			if report.Refused != "" || !slices.Equal(mismatches, test.mismatches) || report.UpdatesAwaitingPredecessor != test.awaiting {
				t.Fatalf("report = %+v, want pending rows %v awaiting=%d", report, test.mismatches, test.awaiting)
			}
		})
	}
}

func TestStoredOrderStreamsEachOmittedPrefix(t *testing.T) {
	seed := newDocumentCopy(crdt.WithClientID(100))
	seed.Transact(typeParagraph(seed, "base"))
	updates := [][]byte{
		crdt.EncodeStateAsUpdateV1(seed, nil),
		storedOrderHex(t, "01010901840900016200"),
		storedOrderHex(t, "01010b01840b00016400"),
		storedOrderHex(t, "01010d01840d00016600"),
		storedOrderHex(t, "010109000401017402616200"),
		storedOrderHex(t, "01010b0004010174016300"),
		storedOrderHex(t, "01010d0004010174016500"),
	}
	tx := &storedOrderReplayTx{updates: updates, classes: make([]bool, len(updates))}
	var report StoredOrderReport
	if err := foldStoredOrder(context.Background(), tx, "replay", time.Time{}, nil, &report); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tx.prefixes, []int64{5, 6, 7}) {
		t.Fatalf("streamed prefixes = %v, want a separate prefix through each row's resolution: [5 6 7]", tx.prefixes)
	}
	if report.Refused != "" || report.UpdatesAwaitingPredecessor != 0 || len(report.Mismatches) != 2 || report.Mismatches[0].Update != 3 || report.Mismatches[1].Update != 4 {
		t.Fatalf("report = %+v, want only rows 3 and 4 blamed", report)
	}
}

func TestStoredOrderComparesParkedContent(t *testing.T) {
	full, without := newDocumentCopy(), newDocumentCopy()
	delete1 := storedOrderHex(t, "000114010101")
	delete2 := storedOrderHex(t, "000114010201")
	for _, doc := range []*crdt.Doc{full, without} {
		if err := crdt.ApplyUpdateV1(doc, delete1, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !sameStoredState(full, without) {
		t.Fatal("identical parked deletions compare unequal")
	}
	if err := crdt.ApplyUpdateV1(full, delete2, nil); err != nil {
		t.Fatal(err)
	}
	if full.PendingStats().DeleteRanges != 1 || without.PendingStats().DeleteRanges != 1 {
		t.Fatal("fixture must keep equal pending range counts")
	}
	if sameStoredState(full, without) {
		t.Error("different parked deletions compare equal")
	}
	filler := storedOrderHex(t, "01011400040101710361626300")
	for _, doc := range []*crdt.Doc{full, without} {
		if err := crdt.ApplyUpdateV1(doc, filler, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, omitted := full.GetText("q").ToString(), without.GetText("q").ToString(); got != "a" || omitted != "ac" {
		t.Fatalf("resolved text = %q without row = %q, want a and ac", got, omitted)
	}

	t.Run("parked structs", func(t *testing.T) {
		one, two := newDocumentCopy(), newDocumentCopy()
		if err := crdt.ApplyUpdateV1(one, storedOrderHex(t, "01010901840900016200"), nil); err != nil {
			t.Fatal(err)
		}
		if err := crdt.ApplyUpdateV1(two, storedOrderHex(t, "01010901840900016300"), nil); err != nil {
			t.Fatal(err)
		}
		if one.PendingStats().Items != 1 || two.PendingStats().Items != 1 {
			t.Fatal("fixture must keep equal parked item counts")
		}
		if sameStoredState(one, two) {
			t.Fatal("parked structs with different content compare equal")
		}
		copy := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(copy, crdt.EncodeStateAsUpdateV1(one, nil), nil); err != nil {
			t.Fatal(err)
		}
		if !sameStoredState(one, copy) {
			t.Fatal("identical parked structs compare unequal")
		}
	})
}
