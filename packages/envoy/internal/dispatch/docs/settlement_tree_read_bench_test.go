package docs

import (
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// BenchmarkSettlementTreeRead measures a settlement's read of a resident room both ways on the
// documents BenchmarkLiveDocumentRead reads: state, the read a settlement took before - the
// replica's state encoded under its lock and decoded into a document of its own to read one tree
// from (stateTree) - and tree, the read that walks the replica itself under that same lock
// (Service.treePoint). lock/state and lock/tree are what each holds the replica's lock for,
// which the room's update observer waits for (lockForUpdate): the encode against the walk. The
// whole read is where the allocation went; the lock pair is what the change costs the observer.
//
// What it measures on a two-core pod: on the 200 KiB document the walk holds the replica's lock
// for about 54 ms against the encode's 33 ms, while the whole read costs about 54 ms, 11 MB and
// 135,000 allocations against about 260 ms, 64 MB and 731,000 for the state decoded into a
// document of its own; on the 51 KiB document the read costs about 14 ms and 2.8 MB against
// about 66 ms and 17 MB, with a hold of 13 to 26 ms, run to run, against the encode's 8 ms - two
// cores make the shorter runs noisy. So the observer waits around half as long again for a read
// that costs a quarter as much and allocates a fifth.
//
// A document is a spec's markdown with 2,000 keystrokes typed and deleted across it
// (benchmarkLiveDocument). 200 KiB is about as large as that generator goes: markdown making
// more than 65,536 elements is refused (pmdoc.ErrTooManyElements), which that generator passes
// around 237 KiB - and which is why the 524 KiB document BenchmarkLiveDocumentRead still asks
// for fails to build.
func BenchmarkSettlementTreeRead(b *testing.B) {
	for _, size := range []int{51 << 10, 200 << 10} {
		live := benchmarkLiveDocument(b, size)
		name := fmt.Sprintf("%dKiB", size>>10)
		service := &Service{}
		b.Run(name+"/state", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				var state []byte
				replica.atPoint("bench", live, func(_ uint64, doc *crdt.Doc) {
					state = crdt.EncodeStateAsUpdateV1(doc, nil)
				})
				if _, err := stateTree(state); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/tree", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				var err error
				replica.atPoint("bench", live, func(_ uint64, doc *crdt.Doc) { _, err = treeOfUnshared(doc) })
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/lock/state", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				doc := replica.hold()
				if doc == nil {
					b.Fatal("the replica has nothing to read")
				}
				crdt.EncodeStateAsUpdateV1(doc, nil)
				replica.mu.Unlock()
			}
		})
		b.Run(name+"/lock/tree", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				doc := replica.hold()
				if doc == nil {
					b.Fatal("the replica has nothing to read")
				}
				_, err := treeOfUnshared(doc)
				replica.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
