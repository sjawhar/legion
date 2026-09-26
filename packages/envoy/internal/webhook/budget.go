package webhook

import (
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
// declared. A request whose next piece does not fit waits for room, except one request at a time,
// which may take the total past the limit. That right goes to a request whose piece does not fit
// while no other holds it or while the total is within the limit, so another request can take it
// whenever the total is back within the limit, and a request waits on the holder only while the
// total is past it. The holder can take the total past the limit by at most its own body (twice
// its buffer for the moment the buffer grows).
//
// The holder need not be reading. A connection that has sent nothing never asks for room, but a
// request asks as soon as its buffer is full, before its next byte arrives, and any sender can
// stop once its request holds the right. If that request's growth took the total past the limit,
// no request can charge a byte, however small, until the others give back enough to bring the
// total within it or the stalled request ends at the server's read timeout.
type bodyBudget struct {
	mu    sync.Mutex
	limit int64
	held  int64
	most  int64         // the most held at once, which the bound above caps
	over  *bodyRead     // the one request that may take held past limit, or nil
	room  chan struct{} // closed and replaced whenever held falls or over is given back
}

func newBodyBudget(limit int64) *bodyBudget {
	return &bodyBudget{limit: limit, room: make(chan struct{})}
}

// mostHeld is the most the budget has held at once.
func (b *bodyBudget) mostHeld() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.most
}

// bodyRead is one request's read against a budget. It holds nothing until its body's bytes
// arrive, and release must be called once the request no longer holds its body.
type bodyRead struct {
	budget  *bodyBudget
	charged int64
}

// begin starts a request's read against the budget.
func (b *bodyBudget) begin() *bodyRead {
	return &bodyRead{budget: b}
}

// charge waits until n more bytes fit the budget or this read may go past the limit, then holds
// them. A read whose n does not fit takes the right to go past the limit when no other read holds
// it or the total is within the limit.
func (r *bodyRead) charge(ctx context.Context, n int64) error {
	b := r.budget
	for {
		b.mu.Lock()
		fits := b.held+n <= b.limit
		if !fits && (b.over == nil || b.over == r || b.held <= b.limit) {
			b.over = r
			fits = true
		}
		if fits {
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

// release gives back everything the read holds, and the right to go past the limit if it has it.
func (r *bodyRead) release() {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.charged == 0 && b.over != r {
		return
	}
	b.held -= r.charged
	r.charged = 0
	if b.over == r {
		b.over = nil
	}
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
	target := declared
	if target < 0 {
		target = limit + 1
	}
	head := make([]byte, min(bodyFirstRead, target))
	var buf []byte
	for {
		if declared >= 0 && int64(len(buf)) == declared {
			return buf, nil
		}
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
