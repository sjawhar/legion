package webhook

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"sync"
)

// bodyReadStep is the first buffer a body is read into; the buffer doubles each time it fills.
const bodyReadStep = 16 << 10

// bodyBudget is a byte budget that concurrent requests read their bodies against. A request
// charges its read buffer as the buffer grows, which is as its body arrives: a sender that sends
// only headers, or trickles its body, holds a buffer for what it has actually sent and nothing for
// what it declared. A request whose next piece does not fit waits for room, except the one that has
// held its place longest, which always proceeds: it is either still reading, and nothing stops it,
// or done and about to give everything back, so the requests in flight always drain and no set of
// half-read bodies waits on each other. That one request can take the total past the limit by at
// most its own body (twice its buffer for the moment the buffer grows).
type bodyBudget struct {
	mu      sync.Mutex
	limit   int64
	held    int64
	holders *list.List    // the requests holding a place, longest first
	room    chan struct{} // closed and replaced whenever held falls or the longest holder leaves
}

func newBodyBudget(limit int64) *bodyBudget {
	return &bodyBudget{limit: limit, holders: list.New(), room: make(chan struct{})}
}

// bodyRead is one request's place in a budget. release must be called once the request no longer
// holds its body.
type bodyRead struct {
	budget  *bodyBudget
	place   *list.Element
	charged int64
}

// begin takes a place in the budget.
func (b *bodyBudget) begin() *bodyRead {
	b.mu.Lock()
	defer b.mu.Unlock()
	read := &bodyRead{budget: b}
	read.place = b.holders.PushBack(read)
	return read
}

// charge waits until n more bytes fit the budget or this is the longest holder, then holds them.
func (r *bodyRead) charge(ctx context.Context, n int64) error {
	b := r.budget
	for {
		b.mu.Lock()
		if b.held+n <= b.limit || b.holders.Front() == r.place {
			b.held += n
			r.charged += n
			b.mu.Unlock()
			return nil
		}
		room := b.room
		b.mu.Unlock()
		select {
		case <-room:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// refund gives back n of the bytes this read holds.
func (r *bodyRead) refund(n int64) {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= n
	r.charged -= n
	b.signalLocked()
}

// release gives back everything the read holds and its place.
func (r *bodyRead) release() {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= r.charged
	r.charged = 0
	b.holders.Remove(r.place)
	b.signalLocked()
}

func (b *bodyBudget) signalLocked() {
	close(b.room)
	b.room = make(chan struct{})
}

// readAll reads body, charging the budget for its buffer before each time it grows. With a
// declared length (declared >= 0) the buffer never grows past it and a shorter body is
// io.ErrUnexpectedEOF; otherwise a body longer than limit is an *http.MaxBytesError.
func (r *bodyRead) readAll(ctx context.Context, body io.Reader, declared, limit int64) ([]byte, error) {
	target := declared
	if target < 0 {
		target = limit + 1
	}
	var buf []byte
	for {
		if declared >= 0 && int64(len(buf)) == declared {
			return buf, nil
		}
		if len(buf) == cap(buf) {
			if int64(cap(buf)) == target {
				return nil, &http.MaxBytesError{Limit: limit}
			}
			size := min(2*int64(cap(buf)), target)
			size = max(size, min(bodyReadStep, target))
			if err := r.charge(ctx, size); err != nil {
				return nil, err
			}
			grown := make([]byte, len(buf), size)
			copy(grown, buf)
			r.refund(int64(cap(buf)))
			buf = grown
		}
		n, err := body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			if declared >= 0 && int64(len(buf)) != declared {
				return nil, io.ErrUnexpectedEOF
			}
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
