package webhook

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"sync"
)

// bodyFirstRead is the most a body's first read takes into a buffer the budget does not charge.
// Nothing is charged until that read delivers bytes, and the charged buffer then starts at what it
// delivered, so a connection that has sent only headers holds no room at all. It is small because
// every such connection holds one.
const bodyFirstRead = 512

// bodyBudget is a byte budget that concurrent requests read their bodies against. A request
// charges its read buffer as the buffer grows, which is as its body arrives, starting at the bytes
// its first read delivered and doubling as it fills: a sender that sends only headers holds no
// room, and one that trickles its body holds at most twice what it has sent, never what it
// declared. A request whose next piece does not fit waits for room, except the one that has held
// its place longest, which always proceeds: it is either still reading, and nothing stops it, or
// done and about to give everything back, so the requests in flight always drain and no set of
// half-read bodies waits on each other. That one request can take the total past the limit by at
// most its own body (twice its buffer for the moment the buffer grows).
type bodyBudget struct {
	mu      sync.Mutex
	limit   int64
	held    int64
	most    int64         // the most held at once, which the bound above caps
	holders *list.List    // the requests holding a place, longest first
	room    chan struct{} // closed and replaced whenever held falls or the longest holder leaves
}

func newBodyBudget(limit int64) *bodyBudget {
	return &bodyBudget{limit: limit, holders: list.New(), room: make(chan struct{})}
}

// mostHeld is the most the budget has held at once.
func (b *bodyBudget) mostHeld() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.most
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
			b.most = max(b.most, b.held)
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

// readAll reads body, charging the budget for its buffer before each time it grows. A body longer
// than limit is an *http.MaxBytesError: at once, before a byte is read, when its declared length
// says so, and otherwise once limit+1 bytes have arrived. With a declared length (declared >= 0)
// the buffer never grows past it and a shorter body is io.ErrUnexpectedEOF.
func (r *bodyRead) readAll(ctx context.Context, body io.Reader, declared, limit int64) ([]byte, error) {
	if declared > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	if declared == 0 {
		return nil, nil
	}
	target := declared
	if target < 0 {
		target = limit + 1
	}
	head := make([]byte, min(bodyFirstRead, target))
	var buf []byte
	for {
		var n int
		var err error
		if buf == nil {
			n, err = body.Read(head)
			if n > 0 {
				charged, growErr := r.grow(ctx, nil, int64(n))
				if growErr != nil {
					return nil, growErr
				}
				buf = append(charged, head[:n]...)
			}
		} else {
			if len(buf) == cap(buf) {
				grown, growErr := r.grow(ctx, buf, min(2*int64(cap(buf)), target))
				if growErr != nil {
					return nil, growErr
				}
				buf = grown
			}
			n, err = body.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
		}
		// Only an undeclared body can get here past limit: its buffer grows to one byte more than
		// limit so that the byte that says so can arrive, with or without the end of the body.
		if int64(len(buf)) > limit {
			return nil, &http.MaxBytesError{Limit: limit}
		}
		if err == io.EOF {
			if declared >= 0 && int64(len(buf)) != declared {
				return nil, io.ErrUnexpectedEOF
			}
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
		if declared >= 0 && int64(len(buf)) == declared {
			return buf, nil
		}
	}
}

// grow charges size bytes and only then allocates a buffer of that size holding buf, and gives
// back buf's capacity: a request waiting for room holds no buffer the budget has not charged.
func (r *bodyRead) grow(ctx context.Context, buf []byte, size int64) ([]byte, error) {
	if err := r.charge(ctx, size); err != nil {
		return nil, err
	}
	grown := make([]byte, len(buf), size)
	copy(grown, buf)
	r.refund(int64(cap(buf)))
	return grown, nil
}
