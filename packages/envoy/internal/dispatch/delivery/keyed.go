package delivery

import "sync"

// keyedRunner runs handle on items one key at a time and on different keys at once: items of one
// key run in the order run was given them, each after the one before it has returned, and an item
// of another key does not wait for them. A key has a goroutine only while it has an item in its
// handler; the goroutine takes the items that queued behind it and exits once its queue is empty,
// so an idle key costs nothing.
//
// The intake runs a message per key this way (Intake.subscribe): two messages about one pull
// request or one run each fetch that thing's current state from GitHub and write it, and run at
// once an older fetch could be written after a newer one.
type keyedRunner[T any] struct {
	handle func(T)

	mu sync.Mutex
	// queued holds a key for each key with a goroutine, mapped to the items waiting behind the one
	// in its handler, oldest first.
	queued  map[string][]T
	stopped bool
	running sync.WaitGroup
}

func newKeyedRunner[T any](handle func(T)) *keyedRunner[T] {
	return &keyedRunner[T]{handle: handle, queued: map[string][]T{}}
}

// run hands item to key's goroutine, starting one when key has none, and reports whether it took
// item: once stop has run it takes nothing, and the caller still owns item.
func (r *keyedRunner[T]) run(key string, item T) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	if waiting, active := r.queued[key]; active {
		r.queued[key] = append(waiting, item)
		return true
	}
	r.queued[key] = nil
	r.running.Add(1)
	go r.drain(key, item)
	return true
}

// drain handles item, then each item queued behind it on key, and returns once key's queue is
// empty, removing key so the next item of it starts a goroutine of its own.
func (r *keyedRunner[T]) drain(key string, item T) {
	defer r.running.Done()
	for {
		r.handle(item)
		r.mu.Lock()
		waiting := r.queued[key]
		if len(waiting) == 0 {
			delete(r.queued, key)
			r.mu.Unlock()
			return
		}
		item = waiting[0]
		var zero T
		waiting[0] = zero
		r.queued[key] = waiting[1:]
		r.mu.Unlock()
	}
}

// stop makes run refuse every later item and returns the items still waiting behind their keys,
// which no handler will now take: the caller owns them. The items already in their handlers run
// on; wait waits for them.
func (r *keyedRunner[T]) stop() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	var waiting []T
	for key, items := range r.queued {
		waiting = append(waiting, items...)
		r.queued[key] = nil
	}
	return waiting
}

// wait returns once no handler is running.
func (r *keyedRunner[T]) wait() {
	r.running.Wait()
}
