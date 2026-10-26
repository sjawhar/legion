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

// noInterval is the least interval a test can give the measurer (elementMeasurer.interval): a
// rendering published after its room's last count joins the queue at once, so a test of what the
// measurer counts does not wait out roomCountInterval for it.
const noInterval = time.Nanosecond

// waitIdle blocks until the measurer has no worker running and no room waiting out the interval, so
// a test reads what the measurer stored once it has stored it, rather than sleeping past a count it
// cannot see.
func (m *elementMeasurer) waitIdle() {
	for {
		m.mu.Lock()
		idle, delayed := m.idle, m.delayed
		m.mu.Unlock()
		switch {
		case idle != nil:
			<-idle
		case delayed > 0:
			time.Sleep(time.Millisecond)
		default:
			return
		}
	}
}

// headings is a rendering of a few thousand headings, a different one for each n, each making a
// different count.
func headings(n int) string {
	return strings.Repeat("# a\n", 4_096+n)
}

// countRecorder records the renderings a measurer parses and when, and holds the first parse until
// release is closed, once started has been closed, so a test can publish renderings while a count
// runs.
type countRecorder struct {
	mu       sync.Mutex
	counted  []string
	at       []time.Time
	started  chan struct{}
	release  chan struct{}
	holdOnce sync.Once
}

func recordCounts(measurer *elementMeasurer) *countRecorder {
	recorder := &countRecorder{started: make(chan struct{}), release: make(chan struct{})}
	measurer.beforeCount = func(markdown string) {
		recorder.mu.Lock()
		recorder.counted = append(recorder.counted, markdown)
		recorder.at = append(recorder.at, time.Now())
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

func (c *countRecorder) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.at)
}

// A rendering an update publishes is counted off the update observer, and the count reaches the
// atomic the room budget reads, while the bytes the peer update hook weighs are there at once.
func TestTheMeasurerCountsARenderingAnUpdatePublishes(t *testing.T) {
	service := &Service{}
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	rendering := headings(1)
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
	rendering := headings(1)
	replica := service.keepReplica(newDocumentCopy(), nil, &rendering)
	if _, elements, measured := replica.size(); !measured || elements != countElements(rendering) {
		t.Fatalf("the loaded room published %d elements (measured %t), want %d", elements, measured, countElements(rendering))
	}
	service.measurer.waitIdle()
	if parses := recorder.parses(); len(parses) != 0 {
		t.Fatalf("the measurer parsed %d renderings for a load, want none", len(parses))
	}
}

// A small rendering is counted as a large one is: the room budget charges every room by its
// elements, and a small document can make tens of thousands of them, so none is published as zero.
func TestASmallRenderingIsCountedToo(t *testing.T) {
	service := &Service{}
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	if _, elements, _ := replica.size(); elements != countElements(loaded) || elements == 0 {
		t.Fatalf("the loaded room published %d elements, want its count %d", elements, countElements(loaded))
	}
	small := "# Title\n\nOne paragraph.\n"
	replica.publishSize(&small)
	service.measurer.waitIdle()
	if bytes, elements, _ := replica.size(); bytes != int64(len(small)) || elements != countElements(small) {
		t.Fatalf("the room published %d bytes and %d elements, want %d and %d", bytes, elements, len(small), countElements(small))
	}
	if parses := recorder.parses(); !slices.Equal(parses, []string{small}) {
		t.Fatalf("the measurer parsed %d renderings, want the small one", len(parses))
	}
}

// A room typed into faster than it is counted is counted at its latest rendering: renderings
// published while a count runs replace each other, and only the last is parsed after it.
func TestTheMeasurerCountsARoomsLatestRenderingRatherThanEach(t *testing.T) {
	service := &Service{}
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	first, second, third := headings(1), headings(2), headings(3)
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

// After its load's count, a room is counted at most once each interval: a rendering published
// sooner waits out the rest of it, renderings published meanwhile replace it, and the bytes the
// hook weighs are published at once all the same.
func TestTheMeasurerCountsARoomAtMostOncePerInterval(t *testing.T) {
	const interval = 300 * time.Millisecond
	service := &Service{}
	service.measurer.interval = interval
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	loaded := "# Title\n"
	start := time.Now()
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	first, second, third := headings(1), headings(2), headings(3)
	replica.publishSize(&first)
	if bytes, _, _ := replica.size(); bytes != int64(len(first)) {
		t.Fatalf("the room published %d bytes, want the rendering's %d at once", bytes, len(first))
	}
	waitFor(t, 10*time.Second, "the first rendering's count", func() bool { return len(recorder.parses()) == 1 })
	replica.publishSize(&second)
	replica.publishSize(&third)
	service.measurer.waitIdle()
	if parses := recorder.parses(); !slices.Equal(parses, []string{first, third}) {
		t.Fatalf("the measurer parsed %d renderings, want the first and the latest", len(parses))
	}
	times := recorder.times()
	if waited := times[0].Sub(start); waited < interval {
		t.Fatalf("the first count came %s after the room's load, want at least %s", waited, interval)
	}
	// The interval runs from when the count took its rendering, a moment before the recorder's
	// own clock reads.
	if waited := times[1].Sub(times[0]); waited < interval-20*time.Millisecond {
		t.Fatalf("the second count came %s after the first, want about %s at least", waited, interval)
	}
	if _, elements, _ := replica.size(); elements != countElements(third) {
		t.Fatalf("the room published %d elements, want the latest rendering's %d", elements, countElements(third))
	}
}

// A rendering the room could not read publishes no measurement, neither at once nor once a count
// of an earlier rendering lands, and a rendering it withdrew from the queue is never parsed.
func TestAnUnreadableRenderingPublishesNoMeasurement(t *testing.T) {
	service := &Service{}
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	counting := service.keepReplica(newDocumentCopy(), nil, &loaded)
	waiting := service.keepReplica(newDocumentCopy(), nil, &loaded)
	first, second := headings(1), headings(2)
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
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	counting := service.keepReplica(newDocumentCopy(), nil, &loaded)
	first := headings(1)
	counting.publishSize(&first)
	<-recorder.started
	retired := queueRetiredRoom(service, headings(2))
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

// Nor is it kept alive while it waits out the interval since its last count: the timer it waits on
// holds it weakly too, and the count it was waiting for never runs.
func TestTheMeasurerKeepsNoRetiredRoomsReplicaWaitingOutTheInterval(t *testing.T) {
	service := &Service{}
	service.measurer.interval = 2 * time.Second
	recorder := recordCounts(&service.measurer)
	close(recorder.release)
	retired := queueRetiredRoom(service, headings(2))
	waitFor(t, 30*time.Second, "the retired room's replica to be collected", func() bool {
		runtime.GC()
		return retired.Value() == nil
	})
	service.measurer.waitIdle()
	if parses := recorder.parses(); len(parses) != 0 {
		t.Fatalf("the measurer parsed %d renderings, want none of the retired room's", len(parses))
	}
}

// queueRetiredRoom loads a room, publishes rendering so the room waits for the measurer, and drops
// every reference to the room but a weak one to its replica.
func queueRetiredRoom(service *Service, rendering string) weak.Pointer[renderedReplica] {
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	replica.publishSize(&rendering)
	return weak.Make(replica)
}

// Shutdown stops the measurer: it waits for the count in flight, which still lands, and no
// rendering published after it is counted.
func TestTheMeasurerStopsWithTheService(t *testing.T) {
	service := &Service{}
	service.measurer.interval = noInterval
	recorder := recordCounts(&service.measurer)
	loaded := "# Title\n"
	replica := service.keepReplica(newDocumentCopy(), nil, &loaded)
	first := headings(1)
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
	later := headings(2)
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
// the shape a room holds up to a mebibyte of (residentRoomDocument). The count's cost is what the
// measurer's interval is set against (roomCountInterval), and what keeps it out of the hook
// (peer_update.go:37-50).
func BenchmarkElementCountAgainstRender(b *testing.B) {
	for _, shape := range []struct {
		name  string
		build func(testing.TB, int) *crdt.Doc
		sizes []int
	}{
		{"live", benchmarkLiveDocument, []int{51 << 10, 524 << 10}},
		{"resident", residentRoomDocument, []int{16 << 10, 51 << 10, 524 << 10, pmdoc.MaxDocumentBytes}},
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
