package docs

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// waitIdle blocks until the measurer has no worker running, so a test reads what the measurer
// stored once it has stored it, rather than sleeping past a count it cannot see.
func (m *elementMeasurer) waitIdle() {
	for {
		m.mu.Lock()
		idle := m.idle
		m.mu.Unlock()
		if idle == nil {
			return
		}
		<-idle
	}
}

// headingsPastTheGate is a rendering past the element gate, a different one for each n, each making
// a different count.
func headingsPastTheGate(n int) string {
	return strings.Repeat("# a\n", elementGateBytes/4+n)
}

// countRecorder records the renderings a measurer parses, and holds the first parse until release
// is closed, once started has been closed, so a test can publish renderings while a count runs.
type countRecorder struct {
	mu       sync.Mutex
	counted  []string
	started  chan struct{}
	release  chan struct{}
	holdOnce sync.Once
}

func recordCounts(measurer *elementMeasurer) *countRecorder {
	recorder := &countRecorder{started: make(chan struct{}), release: make(chan struct{})}
	measurer.beforeCount = func(markdown string) {
		recorder.mu.Lock()
		recorder.counted = append(recorder.counted, markdown)
		recorder.mu.Unlock()
		recorder.holdOnce.Do(func() {
			close(recorder.started)
			<-recorder.release
		})
	}
	return recorder
}

func (c *countRecorder) parses() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.counted)
}

// A rendering an update publishes past the element gate is counted off the update observer, and
// the count reaches the atomic the peer update hook loads, while the bytes are there at once.
func TestTheMeasurerCountsARenderingPublishedPastTheGate(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), &loaded)
	rendering := headingsPastTheGate(1)
	replica.publishSize(&rendering)
	if bytes, _, measured := replica.size(); !measured || bytes != int64(len(rendering)) {
		t.Fatalf("the room published %d bytes (measured %t) at once, want %d", bytes, measured, len(rendering))
	}
	service.measurer.waitIdle()
	want := countElements(rendering)
	if want <= 0 {
		t.Fatalf("the rendering counts %d elements, want some", want)
	}
	if _, elements, _ := replica.size(); elements != want {
		t.Fatalf("the room published %d elements, want the rendering's count %d", elements, want)
	}
	if parses := recorder.parses(); len(parses) != 1 || parses[0] != rendering {
		t.Fatalf("the measurer parsed %d renderings, want the one published", len(parses))
	}
}

// A room's load counts its rendering on the loading goroutine, so the room never starts with no
// element count: the count is there when keepReplica returns, and the measurer parsed nothing.
func TestARoomsLoadMeasuresItsRenderingAtOnce(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	rendering := headingsPastTheGate(1)
	replica := service.keepReplica(newDocumentCopy(), &rendering)
	if _, elements, measured := replica.size(); !measured || elements != countElements(rendering) {
		t.Fatalf("the loaded room published %d elements (measured %t), want %d", elements, measured, countElements(rendering))
	}
	service.measurer.waitIdle()
	if parses := recorder.parses(); len(parses) != 0 {
		t.Fatalf("the measurer parsed %d renderings for a load, want none", len(parses))
	}
}

// A room typed into faster than it is counted is counted at its latest rendering: renderings
// published while a count runs replace each other, and only the last is parsed after it.
func TestTheMeasurerCountsARoomsLatestRenderingRatherThanEach(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), &loaded)
	first, second, third := headingsPastTheGate(1), headingsPastTheGate(2), headingsPastTheGate(3)
	replica.publishSize(&first)
	<-recorder.started
	replica.publishSize(&second)
	replica.publishSize(&third)
	if bytes, _, _ := replica.size(); bytes != int64(len(third)) {
		t.Fatalf("the room published %d bytes, want the latest rendering's %d", bytes, len(third))
	}
	close(recorder.release)
	service.measurer.waitIdle()
	if parses := recorder.parses(); !slices.Equal(parses, []string{first, third}) {
		t.Fatalf("the measurer parsed %d renderings, want the one it was counting and the latest", len(parses))
	}
	if _, elements, _ := replica.size(); elements != countElements(third) {
		t.Fatalf("the room published %d elements, want the latest rendering's %d", elements, countElements(third))
	}
}

// A rendering under the gate publishes a count of zero at once and parses nothing, and a count of
// an older rendering still running when it lands does not put that rendering's weight back.
func TestARenderingUnderTheGateMeasuresZeroWithoutAParse(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	loaded := headingsPastTheGate(1)
	replica := service.keepReplica(newDocumentCopy(), &loaded)
	small := "# Title\n\nOne paragraph.\n"
	replica.publishSize(&small)
	if bytes, elements, _ := replica.size(); bytes != int64(len(small)) || elements != 0 {
		t.Fatalf("the room published %d bytes and %d elements, want %d and zero", bytes, elements, len(small))
	}
	service.measurer.waitIdle()
	if parses := recorder.parses(); len(parses) != 0 {
		t.Fatalf("the measurer parsed %d renderings under the gate, want none", len(parses))
	}

	big := headingsPastTheGate(2)
	replica.publishSize(&big)
	<-recorder.started
	replica.publishSize(&small)
	close(recorder.release)
	service.measurer.waitIdle()
	if bytes, elements, _ := replica.size(); bytes != int64(len(small)) || elements != 0 {
		t.Fatalf("the room published %d bytes and %d elements once the older count landed, want %d and zero", bytes, elements, len(small))
	}
}

// A rendering the room could not read publishes no measurement, neither at once nor once a count
// of an earlier rendering lands, and a rendering it withdrew from the queue is never parsed.
func TestAnUnreadableRenderingPublishesNoMeasurement(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	counting := service.keepReplica(newDocumentCopy(), &loaded)
	waiting := service.keepReplica(newDocumentCopy(), &loaded)
	first, second := headingsPastTheGate(1), headingsPastTheGate(2)
	counting.publishSize(&first)
	<-recorder.started
	waiting.publishSize(&second)
	counting.publishSize(nil)
	waiting.publishSize(nil)
	for _, replica := range []*renderedReplica{counting, waiting} {
		if bytes, elements, measured := replica.size(); measured || bytes != sizeUnmeasured || elements != sizeUnmeasured {
			t.Fatalf("an unreadable room published %d bytes and %d elements (measured %t), want no measurement", bytes, elements, measured)
		}
	}
	close(recorder.release)
	service.measurer.waitIdle()
	for _, replica := range []*renderedReplica{counting, waiting} {
		if bytes, elements, measured := replica.size(); measured || bytes != sizeUnmeasured || elements != sizeUnmeasured {
			t.Fatalf("an unreadable room published %d bytes and %d elements (measured %t) once the count landed, want no measurement", bytes, elements, measured)
		}
	}
	if parses := recorder.parses(); !slices.Equal(parses, []string{first}) {
		t.Fatalf("the measurer parsed %d renderings, want only the one it was counting", len(parses))
	}
}

// A retired room's replica, a whole copy of its document, is not kept alive by the measurer's queue
// while the room waits its turn behind another room's count.
func TestTheMeasurerKeepsNoRetiredRoomsReplica(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	counting := service.keepReplica(newDocumentCopy(), &loaded)
	first := headingsPastTheGate(1)
	counting.publishSize(&first)
	<-recorder.started
	retired := queueRetiredRoom(service, headingsPastTheGate(2))
	waitFor(t, 30*time.Second, "the retired room's replica to be collected", func() bool {
		runtime.GC()
		return retired.Value() == nil
	})
	close(recorder.release)
	service.measurer.waitIdle()
	if parses := recorder.parses(); !slices.Equal(parses, []string{first}) {
		t.Fatalf("the measurer parsed %d renderings, want only the live room's", len(parses))
	}
	runtime.KeepAlive(counting)
}

// queueRetiredRoom loads a room, publishes rendering past the gate so the room waits in the
// measurer's queue, and drops every reference to the room but a weak one to its replica.
func queueRetiredRoom(service *Service, rendering string) weak.Pointer[renderedReplica] {
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), &loaded)
	replica.publishSize(&rendering)
	return weak.Make(replica)
}

// Shutdown stops the measurer: it waits for the count in flight, which still lands, and no
// rendering published after it is counted.
func TestTheMeasurerStopsWithTheService(t *testing.T) {
	service := &Service{}
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), &loaded)
	first := headingsPastTheGate(1)
	replica.publishSize(&first)
	<-recorder.started
	stopped := make(chan struct{})
	go func() {
		service.measurer.stop(context.Background())
		close(stopped)
	}()
	close(recorder.release)
	<-stopped
	if _, elements, _ := replica.size(); elements != countElements(first) {
		t.Fatalf("the room published %d elements, want the count in flight at the stop, %d", elements, countElements(first))
	}
	later := headingsPastTheGate(2)
	replica.publishSize(&later)
	service.measurer.waitIdle()
	if parses := recorder.parses(); !slices.Equal(parses, []string{first}) {
		t.Fatalf("the measurer parsed %d renderings, want none after it stopped", len(parses))
	}
	if bytes, elements, _ := replica.size(); bytes != int64(len(later)) || elements != countElements(first) {
		t.Fatalf("the room published %d bytes and %d elements after the stop, want %d and the last count %d", bytes, elements, len(later), countElements(first))
	}
}

// BenchmarkElementCountAgainstRender weighs the measurer's count of a rendering
// (pmdoc.MeasureDocument) against the update observer's render that made it
// (renderDocumentForUpdate), on the live benchmarks' own document (benchmarkLiveDocument) and on
// the shape a room holds up to a mebibyte of (residentRoomDocument). The count costing less than
// the render at every size is what keeps a room typed into alone at most one update behind
// (renderedReplica.size): the count of one rendering ends before the next rendering is published.
func BenchmarkElementCountAgainstRender(b *testing.B) {
	for _, shape := range []struct {
		name  string
		build func(testing.TB, int) *crdt.Doc
		sizes []int
	}{
		{"live", benchmarkLiveDocument, []int{51 << 10, 524 << 10}},
		{"resident", residentRoomDocument, []int{elementGateBytes, 51 << 10, 524 << 10, pmdoc.MaxDocumentBytes}},
	} {
		for _, size := range shape.sizes {
			live := shape.build(b, size)
			markdown, _, err := renderDocumentForUpdate(live)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/%dKiB/render", shape.name, size>>10), func(b *testing.B) {
				for b.Loop() {
					if _, _, err := renderDocumentForUpdate(live); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/%dKiB/count", shape.name, size>>10), func(b *testing.B) {
				for b.Loop() {
					countElements(markdown)
				}
			})
		}
	}
}
