package docs

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// BenchmarkLiveDocumentRead measures what a read of a resident room costs by each way it can be
// taken: walking the live tree with no lock (walk, which races the room's peers), through a copy
// taken under the document's lock (copy), and through the update observer's replica brought up to
// date under that lock (replica: idle when the observer has already taken every update, keystroke
// when one typed character is still to catch up). lock is how long each holds the live document's
// lock, which every peer's update waits for.
func BenchmarkLiveDocumentRead(b *testing.B) {
	for _, size := range []int{51 << 10, 524 << 10} {
		live := benchmarkLiveDocument(b, size)
		name := fmt.Sprintf("%dKiB", size>>10)
		for _, read := range []struct {
			name string
			read func(*crdt.Doc) (*pmdoc.Node, error)
		}{
			{"tree", treeOf},
			{"render", func(doc *crdt.Doc) (*pmdoc.Node, error) {
				tree, err := treeOf(doc)
				if err != nil {
					return nil, err
				}
				_, err = documentMarkdown(tree)
				return tree, err
			}},
		} {
			b.Run(name+"/"+read.name+"/walk", func(b *testing.B) {
				for b.Loop() {
					if _, err := read.read(live); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"/"+read.name+"/copy", func(b *testing.B) {
				for b.Loop() {
					copied, err := snapshotDocument(live)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := read.read(copied); err != nil {
						b.Fatal(err)
					}
				}
			})
			for _, typed := range []bool{false, true} {
				variant := "idle"
				if typed {
					variant = "keystroke"
				}
				b.Run(name+"/"+read.name+"/replica-"+variant, func(b *testing.B) {
					replica := &renderedReplica{}
					replica.catchUp("bench", live)
					for b.Loop() {
						if typed {
							b.StopTimer()
							typeOneCharacter(b, live)
							b.StartTimer()
						}
						doc := replica.hold("bench", live)
						if doc == nil {
							b.Fatal("the replica has nothing to read")
						}
						_, readErr := read.read(doc)
						replica.mu.Unlock()
						if readErr != nil {
							b.Fatal(readErr)
						}
					}
				})
			}
		}
		b.Run(name+"/lock/copy", func(b *testing.B) {
			for b.Loop() {
				crdt.EncodeStateAsUpdateV1(live, nil)
			}
		})
		b.Run(name+"/lock/replica-keystroke", func(b *testing.B) {
			replica := &renderedReplica{}
			replica.catchUp("bench", live)
			for b.Loop() {
				b.StopTimer()
				typeOneCharacter(b, live)
				b.StartTimer()
				update := crdt.EncodeStateAsUpdateV1(live, replica.doc.StateVector())
				b.StopTimer()
				if err := crdt.ApplyUpdateV1(replica.doc, update, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkKeystrokeBesideReads measures how long a peer's keystroke holds up the room's other
// browsers while reads of the room run beside it: none, one every 250 ms, or one after another,
// each through a copy (snapshotDocument, as main read) or through readLive. ygo broadcasts a
// peer's update only once the room's update observer has returned, so a keystroke's latency here
// is its transaction on the live document and the observer's whole turn with the replica
// (renderedReplica.observe): the catch-up under the replica's lock and the render
// (renderDocumentForUpdate), which skips pmdoc.Read's copy since the tree it renders never
// outlives the call. 120 keystrokes 20 ms apart on a 524 KiB document; each run reports the
// latencies' percentiles in milliseconds.
func BenchmarkKeystrokeBesideReads(b *testing.B) {
	live := benchmarkLiveDocument(b, 524<<10)
	texts := paragraphTexts(live)
	if len(texts) == 0 {
		b.Fatal("the document holds no paragraph text")
	}
	walk := func(doc *crdt.Doc) {
		if _, err := treeOf(doc); err != nil {
			b.Error(err)
		}
	}
	readers := []struct {
		name string
		read func(service *Service)
	}{
		{"none", nil},
		{"copy", func(*Service) {
			copied, err := snapshotDocument(live)
			if err != nil {
				b.Error(err)
				return
			}
			walk(copied)
		}},
		{"readLive", func(service *Service) {
			if err := service.readLive("bench", live, walk); err != nil {
				b.Error(err)
			}
		}},
	}
	for _, reader := range readers {
		for _, every := range []time.Duration{250 * time.Millisecond, 0} {
			name := reader.name
			if reader.read != nil {
				name += "/back-to-back"
				if every > 0 {
					name = fmt.Sprintf("%s/every-%s", reader.name, every)
				}
			} else if every == 0 {
				continue
			}
			b.Run(name, func(b *testing.B) {
				service := &Service{}
				replica := service.keepReplica(live, nil)
				replica.observe("bench", live, nil)
				for b.Loop() {
					stop := make(chan struct{})
					var reading sync.WaitGroup
					if reader.read != nil {
						reading.Go(func() {
							for {
								select {
								case <-stop:
									return
								case <-time.After(every):
								}
								reader.read(service)
							}
						})
					}
					latencies := make([]time.Duration, 0, 120)
					for range 120 {
						time.Sleep(20 * time.Millisecond)
						start := time.Now()
						live.Transact(func(txn *crdt.Transaction) { texts[0].Insert(txn, 0, "y", nil) })
						replica.observe("bench", live, nil)
						latencies = append(latencies, time.Since(start))
					}
					close(stop)
					reading.Wait()
					slices.Sort(latencies)
					for _, percentile := range []int{50, 90, 99, 100} {
						at := min(len(latencies)*percentile/100, len(latencies)-1)
						b.ReportMetric(float64(latencies[at])/float64(time.Millisecond), fmt.Sprintf("p%d-ms", percentile))
					}
				}
			})
		}
	}
}

// BenchmarkPeerUpdateWeighing measures what the peer update hook costs the browser whose keystroke
// it weighs (Service.weighPeerUpdate), with an update of the size a keystroke sends against a 524
// KiB room, the size BenchmarkKeystrokeBesideReads types into. It is weighed twice: against a room
// whose published measurement is under the bound, which is the room budget's signal and the
// rendering's bytes, two atomic loads, and the addition every keystroke pays, and against a room
// whose measurement is at pmdoc.MaxDocumentBytes, where the hook decodes that one update and nets
// what it inserts against the room's state vector (insertedBeyond). The apply the hook is handed
// does nothing: what is timed is the weighing this change adds to the peer's read loop, where the
// room's own apply runs with the hook or without it. The percentiles are reported in
// milliseconds, as BenchmarkKeystrokeBesideReads reports a keystroke's, so the hook's cost can be
// read against the keystroke it rides on.
//
// The room is residentRoomDocument's rather than benchmarkLiveDocument's, which is the same shape
// and the same size but makes more elements than a document may hold at 524 KiB, so no room
// renders it and none would publish a measurement for the hook to weigh at all (room_heap_test.go,
// residentRoomMarkdown). What the hook costs turns on the measurement and the update, not on the
// document under them, and this one is a document a room can hold.
func BenchmarkPeerUpdateWeighing(b *testing.B) {
	const weighings = 1_000
	live := residentRoomDocument(b, 524<<10)
	rendering, err := renderDocument(live)
	if err != nil {
		b.Fatal(err)
	}
	service := &Service{}
	replica := service.keepReplica(live, &rendering)
	update := benchmarkKeystrokeUpdate(b, live)
	published, _, measured := replica.size()
	if !measured || published+int64(len(update)) >= pmdoc.MaxDocumentBytes {
		b.Fatalf("the room published %d bytes, want a measurement under the bound", published)
	}
	for _, room := range []struct {
		name  string
		bytes int64
	}{
		{"under-the-bound", published},
		{"at-the-bound", pmdoc.MaxDocumentBytes},
	} {
		b.Run(room.name, func(b *testing.B) {
			// A room at the bound is made by publishing a measurement at it, which is all the
			// hook reads of a room (renderedReplica.publishSize): a document whose own rendering
			// measured pmdoc.MaxDocumentBytes would be a document twice this one's size, and what
			// this weighs is the hook's branch rather than the document under it.
			replica.bytes.Store(room.bytes)
			info := websocket.PeerUpdateInfo{Room: "bench", Doc: live, Update: update}
			latencies := make([]time.Duration, weighings)
			for b.Loop() {
				for weighing := range latencies {
					start := time.Now()
					if err := service.weighPeerUpdate(info, func() error { return nil }); err != nil {
						b.Fatal(err)
					}
					latencies[weighing] = time.Since(start)
				}
				slices.Sort(latencies)
				for _, percentile := range []int{50, 90, 99, 100} {
					at := min(len(latencies)*percentile/100, len(latencies)-1)
					b.ReportMetric(float64(latencies[at])/float64(time.Millisecond), fmt.Sprintf("p%d-ms", percentile))
				}
			}
		})
	}
}

// benchmarkKeystrokeUpdate is the update a browser sends for one typed character into live's first
// paragraph, built on a copy of the room as the browser's own document is.
func benchmarkKeystrokeUpdate(b *testing.B, live *crdt.Doc) []byte {
	b.Helper()
	browser, err := snapshotDocument(live)
	if err != nil {
		b.Fatal(err)
	}
	texts := paragraphTexts(browser)
	if len(texts) == 0 {
		b.Fatal("the document holds no paragraph text")
	}
	browser.Transact(func(txn *crdt.Transaction) { texts[0].Insert(txn, 0, "y", nil) })
	return crdt.EncodeStateAsUpdateV1(browser, live.StateVector())
}

// benchmarkLiveDocument is a live document holding about size bytes of a spec's markdown - headings,
// marked paragraphs, lists, tables and code - after 2,000 keystrokes typed and deleted across it,
// so its delete set is a document's that people have edited.
//
// Each paragraph runs on in ordinary words past the sentence that names it, and that is what makes
// the document one a room can hold: the shape without those words makes 0.28 elements a byte, so
// 524 KiB of it measures 147,552 elements where a document may hold pmdoc.MaxDocumentElements,
// and pmdoc.Update refused to write it at all - the benchmark could not build its own document,
// at this commit or before it. With them the same 524 KiB measures 60,864 elements, which the
// write takes and the room renders, so a keystroke here costs what a keystroke on a document of
// this size costs a room.
func benchmarkLiveDocument(tb testing.TB, size int) *crdt.Doc {
	tb.Helper()
	running := " " + strings.Repeat("word ", 99) + "word"
	var markdown strings.Builder
	for section := 0; markdown.Len() < size; section++ {
		fmt.Fprintf(&markdown, "## Section %d\n\n", section)
		fmt.Fprintf(&markdown, "Paragraph %d holds **bold text**, `inline code`, *emphasis* and a "+
			"[link](https://example.com/%d) among the ordinary words a spec's paragraph runs to.%s\n\n",
			section, section, running)
		markdown.WriteString("- the first item\n- the second item with `code`\n- the third item\n\n")
		fmt.Fprintf(&markdown, "| Column | Value |\n| --- | --- |\n| row %d | cell |\n| other | cell |\n\n", section)
		markdown.WriteString("```go\nfunc example() int {\n\treturn 1\n}\n```\n\n")
	}
	tree, err := pmdoc.Parse(markdown.String())
	if err != nil {
		tb.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		tb.Fatal(err)
	}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
		tb.Fatal(err)
	}
	texts := paragraphTexts(live)
	for keystroke := range 2_000 {
		text := texts[keystroke*7%len(texts)]
		live.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
		live.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, 1) })
	}
	return live
}

// typeOneCharacter types one character into live's first paragraph, as a peer's keystroke does.
func typeOneCharacter(b *testing.B, live *crdt.Doc) {
	b.Helper()
	texts := paragraphTexts(live)
	if len(texts) == 0 {
		b.Fatal("the document holds no paragraph text")
	}
	live.Transact(func(txn *crdt.Transaction) { texts[0].Insert(txn, 0, "y", nil) })
}

func paragraphTexts(live *crdt.Doc) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	for _, block := range live.GetXmlFragment(fragmentName).Children() {
		paragraph, ok := block.(*crdt.YXmlElement)
		if !ok || paragraph.NodeName != "paragraph" {
			continue
		}
		for _, child := range paragraph.Children() {
			if text, ok := child.(*crdt.YXmlText); ok {
				texts = append(texts, text)
				break
			}
		}
	}
	return texts
}
