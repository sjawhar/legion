package docs

import (
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// BenchmarkWriteFork measures a live write's fork of a resident room both ways on the documents
// BenchmarkLiveDocumentRead reads: state, the fork a write took before - the replica's state
// encoded under its lock (renderedReplica.readPoint) and decoded into the write's own document
// (buildFork) - and clone, the fork cloned from the replica under that same lock
// (renderedReplica.clonePoint). lock/state and lock/clone are what each holds the replica's lock
// for, which the room's update observer waits for (lockForUpdate): the encode against the clone.
// The whole fork is what the write pays; the lock pair is what the change costs the observer.
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
				state, _, _, _ := replica.readPoint("bench", live)
				if _, err := buildFork(forkWriteClient, state); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/clone", func(b *testing.B) {
			replica := benchmarkReplica(b, service, live)
			b.ReportAllocs()
			for b.Loop() {
				fork, _, _, _, _ := replica.clonePoint("bench", live, options...)
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
