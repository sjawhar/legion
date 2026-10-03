package shimwire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkReader hands out exactly one recorded chunk per Read, the way a socket hands out exactly
// what one kernel read returned: the split points a stream reader has to survive are the ones
// the test chooses, not the ones a buffer happens to produce.
type chunkReader struct{ chunks [][]byte }

func (c *chunkReader) Read(p []byte) (int, error) {
	for len(c.chunks) > 0 && len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	return n, nil
}

// The reason line-reader.ts decodes through a streaming TextDecoder (line-reader.ts:9-21): a
// multi-byte character a socket read splits in two must be reassembled, never delivered as two
// replacement characters. Split the same frame at every byte offset; every split yields the
// identical line.
func TestALineSurvivesEveryByteOffsetSplitOfAMultiByteCharacter(t *testing.T) {
	frame := `{"type":"agent_end","note":"日本語 🙂 ünïcödé"}`
	wire := []byte(frame + "\n")
	for split := 1; split < len(wire); split++ {
		reader := NewReader(&chunkReader{chunks: [][]byte{wire[:split:split], wire[split:]}})
		line, err := reader.ReadLine()
		if err != nil {
			t.Fatalf("split at %d: ReadLine: %v", split, err)
		}
		if string(line) != frame {
			t.Fatalf("split at %d: ReadLine = %q, want %q", split, line, frame)
		}
	}
}

// The framing the shipped reader produces: trimmed lines, and an empty one never reaches the
// caller or spends a backlog slot (line-reader.ts:24-26).
func TestBlankAndPaddedLinesAreTrimmedAndSkipped(t *testing.T) {
	reader := NewReader(strings.NewReader("\n  \r\n  {\"type\":\"agent_start\"}  \n\n{\"type\":\"agent_end\"}\n\n"))
	for _, want := range []string{`{"type":"agent_start"}`, `{"type":"agent_end"}`} {
		line, err := reader.ReadLine()
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		if string(line) != want {
			t.Fatalf("ReadLine = %q, want %q", line, want)
		}
	}
	if _, err := reader.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine at the end = %v, want io.EOF", err)
	}
}

// One chunk line is ~350 KiB — larger than any read the transport performs in one go — so the
// reader has to accumulate across reads and still hand back one whole line (socket-writer.ts:20-24).
func TestALineLargerThanTheReadBufferArrivesWhole(t *testing.T) {
	line := chunkLine(t)
	reader := NewReader(&chunkReader{chunks: split(append(append([]byte{}, line...), '\n'), 4096)})
	got, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("ReadLine returned %d bytes, want the %d-byte line back", len(got), len(line))
	}
}

// The shipped TypeScript reader buffers without a bound; a Go daemon that did the same would let
// one peer grow the process until it died. The protocol's own plain-frame limit is the bound, and
// crossing it is named, not truncated to something that would parse as a different frame.
func TestALineOverThePlainFrameLimitIsRefusedByName(t *testing.T) {
	wire := append(bytes.Repeat([]byte("x"), MaxFrameBytes), '\n')
	reader := NewReader(&chunkReader{chunks: split(wire, 64<<10)})
	_, err := reader.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("ReadLine = %v, want ErrLineTooLong", err)
	}
	// The stream is desynchronised mid-line; it never resumes into the middle of a frame.
	if _, again := reader.ReadLine(); !errors.Is(again, ErrLineTooLong) {
		t.Fatalf("second ReadLine = %v, want the same ErrLineTooLong", again)
	}
}

// A process killed mid-write leaves a partial line. It is not a frame: the shipped reader drops
// its buffer rather than emit it (line-reader.ts:30-33).
func TestATrailingPartialLineIsNeverDeliveredAsAFrame(t *testing.T) {
	reader := NewReader(strings.NewReader("{\"type\":\"agent_start\"}\n{\"type\":\"agent_"))
	if _, err := reader.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if _, err := reader.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine after a partial line = %v, want io.EOF", err)
	}
}

// split cuts b into reads of at most size bytes.
func split(b []byte, size int) [][]byte {
	var chunks [][]byte
	for len(b) > size {
		chunks = append(chunks, b[:size:size])
		b = b[size:]
	}
	return append(chunks, b)
}
