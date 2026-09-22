package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bug socket-writer.ts exists for (socket-writer.ts:19-25): an ~350 KiB rpc_chunk line
// against a unix socket buffer of roughly 200 KiB. A writer that let the transport take only
// what fits would deliver a truncated, unparseable frame. Here the reader deliberately waits
// until the buffer is full before it starts draining.
func TestALineLargerThanTheSocketBufferIsDeliveredWhole(t *testing.T) {
	line := chunkLine(t)
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "w.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	written := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			written <- acceptErr
			return
		}
		defer conn.Close()
		written <- NewWriter(conn).WriteLine(line)
	}()

	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Let the writer fill the kernel buffer and park before anything is read from it.
	time.Sleep(100 * time.Millisecond)
	got, err := NewReader(conn).ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if writeErr := <-written; writeErr != nil {
		t.Fatalf("WriteLine: %v", writeErr)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("read %d bytes, want the whole %d-byte line", len(got), len(line))
	}
}

// Two goroutines share one stream in the shim (the wrapped process's stdout and the daemon's own
// answers). Two frames must never interleave on the wire, and neither may lose its newline.
func TestConcurrentFramesNeverInterleaveOnTheWire(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	writer := NewWriter(client)

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		reader := NewReader(server)
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			lines <- string(line)
		}
	}()

	var writers sync.WaitGroup
	for w := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 20 {
				frame := Response{
					ID:      fmt.Sprintf("w%d-%d", w, i),
					Command: TypePrompt,
					Success: true,
					// Long enough that a partial write would land inside another frame.
					Data: json.RawMessage(`"` + strings.Repeat("x", 4096) + `"`),
				}
				if err := writer.WriteFrame(frame); err != nil {
					t.Errorf("WriteFrame: %v", err)
					return
				}
			}
		}()
	}
	writers.Wait()
	client.Close()

	seen := map[string]bool{}
	for line := range lines {
		frame, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("Decode(%.80q): %v", line, err)
		}
		response, ok := frame.(Response)
		if !ok {
			t.Fatalf("Decode(%.80q) = %#v, want a Response", line, frame)
		}
		seen[response.ID] = true
	}
	if len(seen) != 8*20 {
		t.Fatalf("read %d distinct frames, want %d", len(seen), 8*20)
	}
}

// A newline inside a line would split one frame into two on the wire. JSON escapes its own, so a
// caller that hands one over has a bug worth naming rather than a stream to desynchronise.
func TestALineCarryingItsOwnNewlineIsRefused(t *testing.T) {
	var sink bytes.Buffer
	err := NewWriter(&sink).WriteLine([]byte("{\"type\":\"agent_start\"}\n{\"type\":\"agent_end\"}"))
	if !errors.Is(err, ErrEmbeddedNewline) {
		t.Fatalf("WriteLine = %v, want ErrEmbeddedNewline", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("wrote %d bytes for a refused line, want none", sink.Len())
	}
}

// The plain-frame limit is the peer's read limit too (worker-rpc.ts:8-12): a frame over it never
// arrives, so it is refused at the source and chunked instead.
func TestAFrameOverThePlainFrameLimitIsRefusedRatherThanSent(t *testing.T) {
	var sink bytes.Buffer
	err := NewWriter(&sink).WriteLine(bytes.Repeat([]byte("x"), MaxFrameBytes))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("WriteLine = %v, want ErrLineTooLong", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("wrote %d bytes for a refused line, want none", sink.Len())
	}
}

// Every frame is one line terminated by exactly one newline — the framing the reader on the
// other side splits on (socket-writer.ts:51).
func TestEachFrameIsOneNewlineTerminatedLine(t *testing.T) {
	var sink bytes.Buffer
	writer := NewWriter(&sink)
	for _, frame := range []Frame{Shutdown{}, AgentStart{DeliveryID: "d1"}, AgentEnd{}} {
		if err := writer.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame(%#v): %v", frame, err)
		}
	}
	want := "{\"type\":\"shutdown\"}\n{\"type\":\"agent_start\",\"deliveryId\":\"d1\"}\n{\"type\":\"agent_end\"}\n"
	if sink.String() != want {
		t.Fatalf("wrote %q, want %q", sink.String(), want)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
