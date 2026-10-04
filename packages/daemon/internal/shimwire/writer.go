package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrEmbeddedNewline is a line carrying a newline of its own, which would split one frame into
// two on the wire. JSON escapes its own newlines, so this is a caller's bug, not a peer's.
var ErrEmbeddedNewline = errors.New("shimwire: a frame line may not contain a newline")

// Writer writes whole frames to a stream, one newline-terminated line each.
//
// The bug the shipped writer exists for is a partial write: a ~350 KiB rpc_chunk line against a
// unix socket buffer of roughly 200 KiB, where the bytes the kernel refused were dropped and
// every chunked agent_end reached the daemon truncated (socket-writer.ts:19-25). Its queue and
// its drain handler are the netpoller's job here — an io.Writer either writes everything or
// reports why it could not, and a net.Conn parks the goroutine until the peer drains. What is
// left for this type is the part Go does not give: one frame is one Write, under a lock, so two
// goroutines sharing a stream can never interleave their lines or lose a newline between them.
type Writer struct {
	mu   sync.Mutex
	dst  io.Writer
	buf  bytes.Buffer
	json *json.Encoder
}

// NewWriter writes frames to dst.
func NewWriter(dst io.Writer) *Writer {
	w := &Writer{dst: dst}
	w.json = json.NewEncoder(&w.buf)
	// JSON.stringify does not escape <, > or &, and a prompt message is arbitrary task text. A
	// Go peer writes the same bytes the shipped one does.
	w.json.SetEscapeHTML(false)
	return w
}

// WriteFrame encodes f and writes it as one line.
func (w *Writer) WriteFrame(f Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Reset()
	// Encode terminates the value with the newline that frames it.
	if err := w.json.Encode(f); err != nil {
		return fmt.Errorf("encode the %s frame: %w", f.FrameType(), err)
	}
	return w.writeLocked()
}

// WriteLine writes an already-encoded frame as one line. The newline is this writer's.
func (w *Writer) WriteLine(line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Reset()
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	return w.writeLocked()
}

// writeLocked sends the buffered line — newline included — after refusing the two shapes that
// would desynchronise the peer's framing rather than reach it as one frame.
func (w *Writer) writeLocked() error {
	line := w.buf.Bytes()
	if i := bytes.IndexByte(line[:len(line)-1], '\n'); i >= 0 {
		return fmt.Errorf("%w (at byte %d of %d)", ErrEmbeddedNewline, i, len(line)-1)
	}
	if len(line) > MaxFrameBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrLineTooLong, len(line), MaxFrameBytes)
	}
	_, err := w.dst.Write(line)
	return err
}
