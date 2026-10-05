package docs

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

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
				replica.observe("bench", live)
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
						replica.observe("bench", live)
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

// benchmarkLiveDocument is a live document holding about size bytes of a spec's markdown - headings,
// marked paragraphs, lists, tables and code - after 2,000 keystrokes typed and deleted across it,
// so its delete set is a document's that people have edited.
func benchmarkLiveDocument(b *testing.B, size int) *crdt.Doc {
	b.Helper()
	var markdown strings.Builder
	for section := 0; markdown.Len() < size; section++ {
		fmt.Fprintf(&markdown, "## Section %d\n\n", section)
		fmt.Fprintf(&markdown, "Paragraph %d holds **bold text**, `inline code`, *emphasis* and a "+
			"[link](https://example.com/%d) among the ordinary words a spec's paragraph runs to.\n\n",
			section, section)
		markdown.WriteString("- the first item\n- the second item with `code`\n- the third item\n\n")
		fmt.Fprintf(&markdown, "| Column | Value |\n| --- | --- |\n| row %d | cell |\n| other | cell |\n\n", section)
		markdown.WriteString("```go\nfunc example() int {\n\treturn 1\n}\n```\n\n")
	}
	tree, err := pmdoc.Parse(markdown.String())
	if err != nil {
		b.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		b.Fatal(err)
	}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
		b.Fatal(err)
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
