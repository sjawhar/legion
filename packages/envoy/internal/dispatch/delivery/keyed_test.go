package delivery

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyedItem is one item a keyedRunner test hands its runner: the key and a number naming it.
type keyedItem struct {
	key string
	n   int
}

// keyedRecorder is a keyedRunner handler that records when each item starts and ends, and holds
// any item whose number has a gate until the gate is closed.
type keyedRecorder struct {
	mu     sync.Mutex
	events []string
	gates  map[int]chan struct{}
	start  chan keyedItem
}

func (r *keyedRecorder) handle(item keyedItem) {
	r.record("start", item)
	r.start <- item
	if gate, ok := r.gates[item.n]; ok {
		<-gate
	}
	r.record("end", item)
}

func (r *keyedRecorder) record(what string, item keyedItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s %s%d", what, item.key, item.n))
}

// recorded is the events of items whose key is key, in the order they happened.
func (r *keyedRecorder) recorded(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []string
	for _, event := range r.events {
		if strings.Contains(event, " "+key) {
			events = append(events, event)
		}
	}
	return events
}

func awaitStart(t *testing.T, starts <-chan keyedItem) keyedItem {
	t.Helper()
	select {
	case item := <-starts:
		return item
	case <-time.After(5 * time.Second):
		t.Fatal("no item started within 5s")
		return keyedItem{}
	}
}

// Items of one key run in the order they were given, each once the one before it has ended, while
// an item of another key runs as soon as it is given; once every key's queue is empty no goroutine
// is left behind.
func TestAKeyedRunnerRunsOneKeyInOrderAndKeysAtOnce(t *testing.T) {
	recorder := &keyedRecorder{gates: map[int]chan struct{}{1: make(chan struct{})}, start: make(chan keyedItem, 8)}
	runner := newKeyedRunner(recorder.handle)

	runner.run("a", keyedItem{"a", 1})
	if item := awaitStart(t, recorder.start); item.n != 1 {
		t.Fatalf("first start = %+v, want a1", item)
	}
	runner.run("a", keyedItem{"a", 2})
	runner.run("b", keyedItem{"b", 3})
	if item := awaitStart(t, recorder.start); item.n != 3 {
		t.Fatalf("start while a1 is held = %+v, want b3 (a2 waits behind a1)", item)
	}
	select {
	case item := <-recorder.start:
		t.Fatalf("%+v started while a1 was still in its handler", item)
	case <-time.After(100 * time.Millisecond):
	}

	close(recorder.gates[1])
	if item := awaitStart(t, recorder.start); item.n != 2 {
		t.Fatalf("start after a1 ended = %+v, want a2", item)
	}
	runner.wait()
	if events, want := recorder.recorded("a"), []string{"start a1", "end a1", "start a2", "end a2"}; !slices.Equal(events, want) {
		t.Fatalf("key a's events = %q, want %q", events, want)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.queued) != 0 {
		t.Fatalf("keys still active after every item ended: %v", runner.queued)
	}
}

// stop hands back every item still waiting behind its key, in each key's order, and refuses every
// later item; the item in its handler runs on, and wait returns once it has ended.
func TestAStoppedKeyedRunnerHandsBackWhatWaits(t *testing.T) {
	recorder := &keyedRecorder{gates: map[int]chan struct{}{1: make(chan struct{})}, start: make(chan keyedItem, 8)}
	runner := newKeyedRunner(recorder.handle)
	runner.run("a", keyedItem{"a", 1})
	awaitStart(t, recorder.start)
	runner.run("a", keyedItem{"a", 2})
	runner.run("a", keyedItem{"a", 3})

	waiting := runner.stop()
	if want := []keyedItem{{"a", 2}, {"a", 3}}; !slices.Equal(waiting, want) {
		t.Fatalf("stop handed back %+v, want %+v", waiting, want)
	}
	if runner.run("b", keyedItem{"b", 4}) {
		t.Fatal("a stopped runner took an item")
	}
	close(recorder.gates[1])
	runner.wait()
	if events, want := recorder.recorded("a"), []string{"start a1", "end a1"}; !slices.Equal(events, want) {
		t.Fatalf("events = %q, want %q: only the item in its handler runs after stop", events, want)
	}
}
