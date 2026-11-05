package docs

import (
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// BenchmarkWriteFork measures a live write's fork of a resident room both ways on the documents
// BenchmarkLiveDocumentRead reads: state, the fork a write took before - the replica's state
// encoded under its lock and decoded into the write's own document (buildFork) - and clone, the
// fork cloned from the replica under that same lock (Service.forkPoint). lock/state and
// lock/clone are what each holds the replica's lock for, which the room's update observer waits
// for (lockForUpdate): the encode against the clone. The whole fork is what the write pays; the
// lock pair is what the change costs the observer.
//
// What it measures on a two-core pod: on the 200 KiB document the fork costs about 90 ms, 27 MB
// and 193,000 allocations cloned against about 185 ms, 50 MB and 585,000 encoded and decoded,
// while the hold rises from the encode's 31 ms to the clone's whole 90 ms; on the 51 KiB
// document it costs about 22 ms, 7 MB and 51,000 against about 50 ms, 13.6 MB and 157,000, with
// the hold rising from 8 ms to 22 ms. So the room's update observer waits around three times as
// long for a fork that costs half as much and allocates a third.
//
// A document is a spec's markdown with 2,000 keystrokes typed and deleted across it
// (benchmarkLiveDocument), at the two sizes BenchmarkSettlementTreeRead uses.
func BenchmarkWriteFork(b *testing.B) {
	for _, size := range []int{51 << 10, 200 << 10} {
		live := benchmarkLiveDocument(b, size)
		name := fmt.Sprintf("%dKiB", size>>10)
		service := &Service{}
		options := forkOptions(forkWriteClient)
		b.Run(name+"/state", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				var state []byte
				replica.atPoint("bench", live, func(_ uint64, doc *crdt.Doc) {
					state = crdt.EncodeStateAsUpdateV1(doc, nil)
				})
				if _, err := buildFork(forkWriteClient, state); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/clone", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				var fork *crdt.Doc
				replica.atPoint("bench", live, func(_ uint64, doc *crdt.Doc) { fork = doc.Clone(options...) })
				if fork == nil {
					b.Fatal("the replica has nothing to clone")
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
		b.Run(name+"/lock/clone", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				doc := replica.hold()
				if doc == nil {
					b.Fatal("the replica has nothing to read")
				}
				doc.Clone(options...)
				replica.mu.Unlock()
			}
		})
	}
}
