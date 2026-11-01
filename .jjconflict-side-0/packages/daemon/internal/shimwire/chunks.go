package shimwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// The refusals a chunk sequence can earn. Each is a bound the shipped decoder already enforces
// (worker-rpc.ts:246-330); the difference is that the sequence is refused by name rather than
// dropped into a log line, so a caller can say which bound broke.
var (
	// ErrChunkMetadata is a chunk whose own numbers are impossible: the ids, the index and
	// count, or a declared length outside the band a sequence may carry (worker-rpc.ts:248-267).
	ErrChunkMetadata = errors.New("shimwire: invalid rpc chunk metadata")
	// ErrChunkData is a payload that is not canonical base64, or is larger than one chunk may
	// carry (worker-rpc.ts:149-157, 275-278).
	ErrChunkData = errors.New("shimwire: invalid rpc chunk data")
	// ErrChunkSequence is a chunk that does not continue the sequence in progress: a wrong
	// start, a changed header, a gap, or a total that disagrees with the declared length
	// (worker-rpc.ts:280-309, 348-349).
	ErrChunkSequence = errors.New("shimwire: invalid rpc chunk sequence")
	// ErrChunkPayload is a reassembled payload that is not a frame (worker-rpc.ts:312-329).
	ErrChunkPayload = errors.New("shimwire: reassembled rpc chunk payload is not a frame")
	// ErrFrameFitsOneLine is a frame small enough to send plainly. The decoder refuses a
	// sequence declaring less than the plain-frame limit (worker-rpc.ts:262), so chunking one
	// would be sending a sequence no peer accepts.
	ErrFrameFitsOneLine = errors.New("shimwire: frame fits one line and is never chunked")
	// ErrFrameTooLarge is a frame over the reassembly limit (worker-rpc.ts:263).
	ErrFrameTooLarge = errors.New("shimwire: frame exceeds the reassembly limit")
)

// maxChunkCount is the most chunks a sequence may declare: the reassembly limit in full
// payloads (worker-rpc.ts:260).
const maxChunkCount = MaxReassembledBytes / ChunkPayloadBytes

// maxChunkIDBytes bounds the sequence identifier (worker-rpc.ts:251).
const maxChunkIDBytes = 128

// SplitChunks slices one logical frame into the rpc_chunk sequence that carries it: base64 of
// each ChunkPayloadBytes slice, in order, every chunk declaring the whole frame's length
// (fake-omp-rpc.ts:50-65). Only a frame between the plain-frame limit and the reassembly limit
// has a sequence: below it the frame is one line, above it no peer will put it back together.
func SplitChunks(chunkID string, frame []byte) ([]RPCChunk, error) {
	if chunkID == "" || len(chunkID) > maxChunkIDBytes {
		return nil, fmt.Errorf("%w: chunk id of %d bytes", ErrChunkMetadata, len(chunkID))
	}
	if len(frame) < MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameFitsOneLine, len(frame), MaxFrameBytes)
	}
	if len(frame) > MaxReassembledBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, len(frame), MaxReassembledBytes)
	}
	count := (len(frame) + ChunkPayloadBytes - 1) / ChunkPayloadBytes
	chunks := make([]RPCChunk, 0, count)
	for index := range count {
		end := min((index+1)*ChunkPayloadBytes, len(frame))
		chunks = append(chunks, RPCChunk{
			ChunkID:    chunkID,
			Index:      index,
			Count:      count,
			ByteLength: len(frame),
			Data:       base64.StdEncoding.EncodeToString(frame[index*ChunkPayloadBytes : end]),
		})
	}
	return chunks, nil
}

// Reassembler puts one connection's chunk sequences back together, one at a time — the order
// the transport guarantees and the shipped decoder assumes (worker-rpc.ts:246-331). Its zero
// value is ready. It is not safe for concurrent use: one connection, one reader.
type Reassembler struct {
	active     bool
	chunkID    string
	count      int
	byteLength int
	nextIndex  int
	payload    []byte
}

// Push adds one chunk. It returns the whole logical frame once the last one lands, nil while
// more are expected, and an error naming the bound a chunk broke — which also drops the
// sequence, so the next chunk has to start a new one. Nothing is ever returned truncated.
//
// The frame is returned as bytes rather than a decoded Frame so nothing is lost on the way
// through: Decode reads it exactly as it reads a plain line.
func (r *Reassembler) Push(chunk RPCChunk) ([]byte, error) {
	if err := chunk.validate(); err != nil {
		r.drop()
		return nil, err
	}
	payload, err := decodeChunkData(chunk.Data)
	if err != nil {
		r.drop()
		return nil, err
	}
	if !r.active {
		if chunk.Index != 0 {
			r.drop()
			return nil, fmt.Errorf("%w: a sequence starts at index 0, not %d", ErrChunkSequence, chunk.Index)
		}
		r.active = true
		r.chunkID, r.count, r.byteLength, r.nextIndex = chunk.ChunkID, chunk.Count, chunk.ByteLength, 0
		r.payload = r.payload[:0]
	}
	if r.chunkID != chunk.ChunkID || r.count != chunk.Count || r.byteLength != chunk.ByteLength || r.nextIndex != chunk.Index {
		r.drop()
		return nil, fmt.Errorf(
			"%w: expected chunk %d of %q (%d chunks, %d bytes), got chunk %d of %q (%d chunks, %d bytes)",
			ErrChunkSequence, r.nextIndex, r.chunkID, r.count, r.byteLength,
			chunk.Index, chunk.ChunkID, chunk.Count, chunk.ByteLength,
		)
	}
	r.payload = append(r.payload, payload...)
	r.nextIndex++
	if len(r.payload) > r.byteLength {
		declared := r.byteLength
		r.drop()
		return nil, fmt.Errorf("%w: more bytes than the declared %d", ErrChunkSequence, declared)
	}
	if r.nextIndex < r.count {
		return nil, nil
	}
	if len(r.payload) != r.byteLength {
		got, declared := len(r.payload), r.byteLength
		r.drop()
		return nil, fmt.Errorf("%w: %d bytes for a declared %d", ErrChunkSequence, got, declared)
	}
	frame := r.payload
	r.active = false
	// The frame is handed to the caller, so the next sequence starts on its own memory.
	r.payload = nil
	if err := validReassembled(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// Interrupt drops a half-built sequence when something that is not a chunk arrives on the same
// stream: the sender abandoned it, and joining its chunks to whatever comes next would build a
// frame nobody sent (worker-rpc.ts:348-349). It reports what it dropped, and nothing when there
// was nothing to drop.
func (r *Reassembler) Interrupt() error {
	if !r.active {
		return nil
	}
	chunkID, index, count := r.chunkID, r.nextIndex, r.count
	r.drop()
	return fmt.Errorf(
		"%w: %q abandoned after %d of %d chunks", ErrChunkSequence, chunkID, index, count,
	)
}

func (r *Reassembler) drop() {
	r.active = false
	r.payload = r.payload[:0]
}

// validate is every rule the shipped decoder applies to a chunk's own numbers before it looks at
// the sequence (worker-rpc.ts:248-267).
func (c RPCChunk) validate() error {
	switch {
	case c.ChunkID == "" || len(c.ChunkID) > maxChunkIDBytes:
		return fmt.Errorf("%w: chunk id of %d bytes", ErrChunkMetadata, len(c.ChunkID))
	case c.Count < 2 || c.Count > maxChunkCount:
		return fmt.Errorf("%w: a sequence of %d chunks", ErrChunkMetadata, c.Count)
	case c.Index < 0 || c.Index >= c.Count:
		return fmt.Errorf("%w: chunk %d of %d", ErrChunkMetadata, c.Index, c.Count)
	case c.ByteLength < MaxFrameBytes || c.ByteLength > MaxReassembledBytes:
		return fmt.Errorf("%w: a declared length of %d bytes", ErrChunkMetadata, c.ByteLength)
	}
	return nil
}

// decodeChunkData decodes one chunk's payload, refusing anything that is not canonical base64 —
// the shipped decoder re-encodes to check exactly that (worker-rpc.ts:151-157) — and anything
// larger than one chunk may carry (worker-rpc.ts:275-278).
func decodeChunkData(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("%w: empty payload", ErrChunkData)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrChunkData, err)
	}
	if len(payload) > ChunkPayloadBytes {
		return nil, fmt.Errorf(
			"%w: %d bytes, limit %d", ErrChunkData, len(payload), ChunkPayloadBytes,
		)
	}
	return payload, nil
}

// validReassembled is what the bytes have to be once they are whole: the three checks the
// shipped decoder makes before dispatching the frame (worker-rpc.ts:312-329). UTF-8 is checked
// explicitly because Go's JSON scanner, unlike a fatal TextDecoder, does not mind an invalid
// byte inside a string.
func validReassembled(frame []byte) error {
	if !utf8.Valid(frame) {
		return fmt.Errorf("%w: not valid utf-8", ErrChunkPayload)
	}
	if !json.Valid(frame) {
		return fmt.Errorf("%w: not valid json", ErrChunkPayload)
	}
	if trimmed := bytes.TrimLeft(frame, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: not a json object", ErrChunkPayload)
	}
	return nil
}
