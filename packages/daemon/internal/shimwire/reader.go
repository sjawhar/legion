package shimwire

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrLineTooLong is a line over the protocol's plain-frame limit, read or written. A frame that
// large is chunked (see SplitChunks); anything else at that size is a peer this end will not
// buffer for.
var ErrLineTooLong = errors.New("shimwire: line exceeds the plain-frame limit")

// The bytes of a line excluding the newline that terminates it: MaxFrameBytes is the limit
// including it (worker-rpc.ts:8-9).
const maxLineBytes = MaxFrameBytes - 1

// The initial read buffer. A plain frame is a few hundred bytes; only an rpc_chunk line (~350
// KiB) grows past this, and it grows once per reader.
const readBufferBytes = 64 << 10

// Reader splits a byte stream into the newline-delimited lines the NDJSON framing produces,
// trimmed, never empty.
//
// A multi-byte UTF-8 character split across two reads is reassembled rather than turned into
// replacement characters — the reason the shipped reader decodes through a streaming TextDecoder
// (line-reader.ts:9-21). Here the line is bytes until it is whole, so a split character cannot
// be observed at all: 0x0A never appears inside a multi-byte sequence.
//
// One difference from the shipped reader, which buffers without a bound: a line over
// maxLineBytes is ErrLineTooLong, and the reader stays failed. The stream is mid-line at that
// point and there is no honest way to resume it — a caller closes the connection, the way the
// listener does with a hello it refuses (worker-stream-listener.ts:112-116).
type Reader struct {
	src *bufio.Reader
	acc []byte
	err error
}

// NewReader reads frames from src.
func NewReader(src io.Reader) *Reader {
	return &Reader{src: bufio.NewReaderSize(src, readBufferBytes)}
}

// ReadLine is the next non-empty line, without its newline. It returns io.EOF at the end of the
// stream, dropping any trailing partial line: a process killed mid-write leaves bytes that are
// not a frame, and the shipped reader drops them too (line-reader.ts:30-33).
//
// The returned slice is valid until the next call, like bufio.Scanner's; a caller that keeps a
// line past that copies it.
func (r *Reader) ReadLine() ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	for {
		slice, err := r.src.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(r.acc)+len(slice) > maxLineBytes {
				r.err = fmt.Errorf("%w of %d bytes", ErrLineTooLong, MaxFrameBytes)
				r.acc = nil
				return nil, r.err
			}
			r.acc = append(r.acc, slice...)
			continue
		}
		if err != nil {
			r.err = err
			r.acc = nil
			return nil, err
		}
		line := slice
		if len(r.acc) > 0 {
			if len(r.acc)+len(slice) > maxLineBytes+1 {
				r.err = fmt.Errorf("%w of %d bytes", ErrLineTooLong, MaxFrameBytes)
				r.acc = nil
				return nil, r.err
			}
			r.acc = append(r.acc, slice...)
			line = r.acc
			// The accumulator is reused from here; the line the caller gets is valid exactly
			// until the next call, which is the contract above.
			r.acc = r.acc[:0]
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			return line, nil
		}
	}
}
