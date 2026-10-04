package shimwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// agentEnd builds the frame that actually gets chunked: the only worker frame whose size grows
// with the whole transcript (worker-rpc.ts:65-68).
func agentEnd(t *testing.T, size int) []byte {
	t.Helper()
	head := `{"type":"agent_end","messages":[{"role":"user","content":"`
	tail := `"}]}`
	if size < len(head)+len(tail) {
		t.Fatalf("size %d is smaller than the frame's own envelope", size)
	}
	return []byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail)
}

// chunkLine is one real rpc_chunk line — a full 256 KiB payload, ~350 KiB on the wire — for the
// reader and writer tests that need a line no single transport read or write will carry.
func chunkLine(t *testing.T) []byte {
	t.Helper()
	chunks, err := SplitChunks("agent-end-1", agentEnd(t, 3<<20+17))
	if err != nil {
		t.Fatalf("SplitChunks: %v", err)
	}
	line, err := json.Marshal(chunks[0])
	if err != nil {
		t.Fatalf("Marshal chunk: %v", err)
	}
	return line
}

// The round trip the protocol exists for: a frame far over the plain-frame limit goes out as
// 256 KiB base64 slices in order (fake-omp-rpc.ts:50-65) and comes back byte for byte.
func TestAThreeMebibyteFrameRoundTripsThroughChunks(t *testing.T) {
	frame := agentEnd(t, 3<<20+17)
	chunks, err := SplitChunks("agent-end-1", frame)
	if err != nil {
		t.Fatalf("SplitChunks: %v", err)
	}
	if want := (len(frame) + ChunkPayloadBytes - 1) / ChunkPayloadBytes; len(chunks) != want {
		t.Fatalf("SplitChunks produced %d chunks, want %d", len(chunks), want)
	}
	// The encoder's own slicing: chunk i is base64 of the frame's i-th 256 KiB slice.
	if want := base64.StdEncoding.EncodeToString(frame[:ChunkPayloadBytes]); chunks[0].Data != want {
		t.Fatalf("chunk 0 payload is not the frame's first %d bytes", ChunkPayloadBytes)
	}
	var reassembler Reassembler
	for i, chunk := range chunks {
		if chunk.ChunkID != "agent-end-1" || chunk.Index != i || chunk.Count != len(chunks) || chunk.ByteLength != len(frame) {
			t.Fatalf("chunk %d metadata = %#v", i, chunk)
		}
		got, pushErr := reassembler.Push(chunk)
		if pushErr != nil {
			t.Fatalf("Push(chunk %d): %v", i, pushErr)
		}
		if i < len(chunks)-1 {
			if got != nil {
				t.Fatalf("Push(chunk %d) returned a frame before the sequence was complete", i)
			}
			continue
		}
		if !bytes.Equal(got, frame) {
			t.Fatalf("reassembled %d bytes, want the original %d", len(got), len(frame))
		}
	}
}

// A frame that fits one line is sent as one line; the decoder refuses a sequence declaring less
// than the plain-frame limit (worker-rpc.ts:262), so producing one would be sending a sequence
// no peer will accept.
func TestSplitRefusesAFrameThatFitsOneLine(t *testing.T) {
	_, err := SplitChunks("agent-end-1", agentEnd(t, MaxFrameBytes-1))
	if !errors.Is(err, ErrFrameFitsOneLine) {
		t.Fatalf("SplitChunks = %v, want ErrFrameFitsOneLine", err)
	}
}

// The reassembly ceiling (worker-rpc.ts:263): a frame over it is refused at the source, so the
// peer never has to discover it 64 MiB in.
func TestSplitRefusesAFrameOverTheReassemblyLimit(t *testing.T) {
	_, err := SplitChunks("agent-end-1", make([]byte, MaxReassembledBytes+1))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("SplitChunks = %v, want ErrFrameTooLarge", err)
	}
}

// Every bound the shipped decoder enforces (worker-rpc.ts:248-330), each refused by name rather
// than dispatched as a short frame. A sequence claiming more than 64 MiB — by declared length,
// by chunk count, or by the bytes it actually sends — never completes.
func TestEveryBrokenChunkSequenceIsRefusedByName(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), ChunkPayloadBytes))
	chunk := func(index, count, byteLength int) RPCChunk {
		return RPCChunk{ChunkID: "c1", Index: index, Count: count, ByteLength: byteLength, Data: payload}
	}
	// The smallest length a sequence may declare is the plain-frame limit itself
	// (worker-rpc.ts:262) — four full payloads, so four chunks carry it exactly.
	const declared = MaxFrameBytes
	const chunks = declared / ChunkPayloadBytes
	for _, tc := range []struct {
		name    string
		prelude []RPCChunk
		last    RPCChunk
		want    error
	}{
		{"empty chunk id", nil, RPCChunk{Index: 0, Count: chunks, ByteLength: declared, Data: payload}, ErrChunkMetadata},
		{
			"chunk id over 128 bytes",
			nil,
			RPCChunk{ChunkID: strings.Repeat("c", 129), Index: 0, Count: chunks, ByteLength: declared, Data: payload},
			ErrChunkMetadata,
		},
		{"negative index", nil, chunk(-1, chunks, declared), ErrChunkMetadata},
		{"index at count", nil, chunk(chunks, chunks, declared), ErrChunkMetadata},
		{"a count of one is a plain frame", nil, chunk(0, 1, declared), ErrChunkMetadata},
		{
			"count over the 64 MiB ceiling",
			nil,
			chunk(0, MaxReassembledBytes/ChunkPayloadBytes+1, declared),
			ErrChunkMetadata,
		},
		{"declared length under the plain-frame limit", nil, chunk(0, chunks, MaxFrameBytes-1), ErrChunkMetadata},
		{"declared length over 64 MiB", nil, chunk(0, chunks, MaxReassembledBytes+1), ErrChunkMetadata},
		{"empty payload", nil, RPCChunk{ChunkID: "c1", Index: 0, Count: chunks, ByteLength: declared}, ErrChunkData},
		{
			"payload that is not canonical base64",
			nil,
			RPCChunk{ChunkID: "c1", Index: 0, Count: chunks, ByteLength: declared, Data: "not base64!"},
			ErrChunkData,
		},
		{
			"payload over the transport limit",
			nil,
			RPCChunk{
				ChunkID:    "c1",
				Index:      0,
				Count:      chunks,
				ByteLength: declared,
				Data:       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), ChunkPayloadBytes+1)),
			},
			ErrChunkData,
		},
		{"a sequence that does not start at index 0", nil, chunk(1, chunks, declared), ErrChunkSequence},
		{
			"a chunk id that changes mid-sequence",
			[]RPCChunk{chunk(0, chunks, declared)},
			RPCChunk{ChunkID: "c2", Index: 1, Count: chunks, ByteLength: declared, Data: payload},
			ErrChunkSequence,
		},
		{
			"a count that changes mid-sequence",
			[]RPCChunk{chunk(0, chunks, declared)},
			chunk(1, chunks+1, declared),
			ErrChunkSequence,
		},
		{
			"a declared length that changes mid-sequence",
			[]RPCChunk{chunk(0, chunks, declared)},
			chunk(1, chunks, declared+1),
			ErrChunkSequence,
		},
		{
			"an index out of order",
			[]RPCChunk{chunk(0, chunks, declared)},
			chunk(2, chunks, declared),
			ErrChunkSequence,
		},
		{
			// The declared length is reached at chunk 3 of 8: the sequence is still expecting
			// more, and the bytes it is about to receive cannot belong to it.
			"more bytes than the sequence declared",
			[]RPCChunk{
				chunk(0, 2*chunks, declared),
				chunk(1, 2*chunks, declared),
				chunk(2, 2*chunks, declared),
				chunk(3, 2*chunks, declared),
			},
			chunk(4, 2*chunks, declared),
			ErrChunkSequence,
		},
		{
			"fewer bytes than the sequence declared",
			[]RPCChunk{chunk(0, chunks, declared), chunk(1, chunks, declared), chunk(2, chunks, declared)},
			RPCChunk{
				ChunkID:    "c1",
				Index:      chunks - 1,
				Count:      chunks,
				ByteLength: declared,
				Data:       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), ChunkPayloadBytes-1)),
			},
			ErrChunkSequence,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reassembler Reassembler
			for i, prelude := range tc.prelude {
				if _, err := reassembler.Push(prelude); err != nil {
					t.Fatalf("prelude chunk %d: %v", i, err)
				}
			}
			if _, err := reassembler.Push(tc.last); !errors.Is(err, tc.want) {
				t.Fatalf("Push = %v, want %v", err, tc.want)
			}
			// A refused sequence is dropped, never continued: the next chunk starts over, and a
			// chunk that is not an index 0 has nothing to join.
			if _, again := reassembler.Push(chunk(1, chunks, declared)); !errors.Is(again, ErrChunkSequence) {
				t.Fatalf("the sequence survived its own refusal: %v", again)
			}
		})
	}
}

// What the bytes have to be once they are back together (worker-rpc.ts:312-329): valid UTF-8,
// valid JSON, and an object — the same three things a plain line has to be.
func TestAReassembledPayloadThatIsNotAFrameIsRefusedByName(t *testing.T) {
	invalidUTF8 := agentEnd(t, MaxFrameBytes+2)
	invalidUTF8[len(invalidUTF8)-8] = 0xff
	for _, tc := range []struct {
		name    string
		payload []byte
		reason  string
	}{
		{"not valid utf-8", invalidUTF8, "utf-8"},
		{"not valid json", bytes.Repeat([]byte("x"), MaxFrameBytes+2), "json"},
		{"a json array, not a frame", []byte(`[` + strings.Repeat(`"x",`, 300_000) + `"x"]`), "object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reassembler Reassembler
			var err error
			for _, c := range rawChunks(t, "c1", tc.payload) {
				if _, err = reassembler.Push(c); err != nil {
					break
				}
			}
			if !errors.Is(err, ErrChunkPayload) {
				t.Fatalf("Push = %v, want ErrChunkPayload", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Push = %v, want the refusal to name %q", err, tc.reason)
			}
		})
	}
}

// A plain frame arriving mid-sequence means the sender abandoned it; the half-built frame is
// dropped and named rather than joined to whatever comes next (worker-rpc.ts:348-349).
func TestAnInterruptedSequenceIsDroppedByName(t *testing.T) {
	var reassembler Reassembler
	if err := reassembler.Interrupt(); err != nil {
		t.Fatalf("Interrupt with no sequence pending = %v, want nil", err)
	}
	chunks, err := SplitChunks("agent-end-1", agentEnd(t, 3<<20))
	if err != nil {
		t.Fatalf("SplitChunks: %v", err)
	}
	if _, err = reassembler.Push(chunks[0]); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err = reassembler.Interrupt(); !errors.Is(err, ErrChunkSequence) {
		t.Fatalf("Interrupt with a sequence pending = %v, want ErrChunkSequence", err)
	}
	if _, err = reassembler.Push(chunks[1]); !errors.Is(err, ErrChunkSequence) {
		t.Fatalf("Push after an interrupt = %v, want the dropped sequence to refuse it", err)
	}
}

// rawChunks slices arbitrary bytes into a well-formed sequence, so a test can put a payload
// through the transport that the splitter itself would never produce.
func rawChunks(t *testing.T, chunkID string, payload []byte) []RPCChunk {
	t.Helper()
	count := (len(payload) + ChunkPayloadBytes - 1) / ChunkPayloadBytes
	if count < 2 {
		t.Fatalf("a payload of %d bytes is one chunk, not a sequence", len(payload))
	}
	chunks := make([]RPCChunk, 0, count)
	for i := range count {
		end := min((i+1)*ChunkPayloadBytes, len(payload))
		chunks = append(chunks, RPCChunk{
			ChunkID:    chunkID,
			Index:      i,
			Count:      count,
			ByteLength: len(payload),
			Data:       base64.StdEncoding.EncodeToString(payload[i*ChunkPayloadBytes : end]),
		})
	}
	return chunks
}
