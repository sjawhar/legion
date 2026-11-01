package shimwire

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// maxWorkspaceBundleBytes is runtime.MaxWorkspaceBundleBytes, the largest bundle a release
// carries, named here rather than imported: the wire has no opinion on the release's bound, and
// this package depends on nothing of the daemon's.
const maxWorkspaceBundleBytes = 40 << 20

// A release's bundle is the one frame of the shim's that does not fit a line. The whole of it —
// 40 MiB of bundle, the saved commits and the bookmarks — has to reach the daemon through the
// lane the shim already has: rpc_chunk out of SplitChunks, reassembled by the daemon's reader.
func TestAFortyMebibyteBundleResultCrossesTheShimsChunkLane(t *testing.T) {
	saved := BundleWorkspaceResult{
		ID:        "bundle-1",
		OK:        true,
		SizeBytes: maxWorkspaceBundleBytes,
		Bundle:    bytes.Repeat([]byte("PACKjj\x00\x02"), maxWorkspaceBundleBytes/8),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
	}
	if len(saved.Bundle) != maxWorkspaceBundleBytes {
		t.Fatalf("the bundle under test is %d bytes, want %d", len(saved.Bundle), maxWorkspaceBundleBytes)
	}
	if err := saved.Validate(); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) <= MaxFrameBytes {
		t.Fatalf("a %d MiB bundle encodes to %d bytes, which fits one line: the chunk lane is not what carries it", maxWorkspaceBundleBytes>>20, len(line))
	}
	chunks, err := SplitChunks("seq-1", line)
	if err != nil {
		t.Fatalf("split the result: %v", err)
	}

	// The shim's side: every chunk is one line of the stream, each inside the frame limit the
	// writer refuses a line over.
	var stream bytes.Buffer
	writer := NewWriter(&stream)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteLine(encoded); err != nil {
			t.Fatalf("chunk %d of %d: %v", chunk.Index, chunk.Count, err)
		}
	}

	// The daemon's side: its reader, its reassembler, and the frame it dispatches.
	reader := NewReader(bytes.NewReader(stream.Bytes()))
	var chunking Reassembler
	var whole []byte
	for whole == nil {
		read, err := reader.ReadLine()
		if err != nil {
			t.Fatalf("read a chunk: %v", err)
		}
		frame, err := Decode(read)
		if err != nil {
			t.Fatal(err)
		}
		chunk, ok := frame.(RPCChunk)
		if !ok {
			t.Fatalf("the lane carried a %s frame, want rpc_chunk", frame.FrameType())
		}
		if whole, err = chunking.Push(chunk); err != nil {
			t.Fatalf("reassemble chunk %d: %v", chunk.Index, err)
		}
	}
	frame, err := Decode(whole)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := frame.(BundleWorkspaceResult)
	if !ok {
		t.Fatalf("the reassembled frame is a %s, want %s", frame.FrameType(), TypeBundleWorkspaceResult)
	}
	if !bytes.Equal(got.Bundle, saved.Bundle) {
		t.Fatalf("the bundle crossed as %d bytes, want the %d sent", len(got.Bundle), len(saved.Bundle))
	}
	if got.ID != saved.ID || got.Head != saved.Head || got.SizeBytes != saved.SizeBytes || !got.OK {
		t.Fatalf("the result crossed as %q/%q/%d/%v", got.ID, got.Head, got.SizeBytes, got.OK)
	}
	if !slices.Equal(got.Tips, saved.Tips) || !maps.Equal(got.Bookmarks, saved.Bookmarks) || !got.At.Equal(saved.At) {
		t.Fatalf("the saved commits crossed as %v / %v / %s", got.Tips, got.Bookmarks, got.At)
	}
}

// The daemon reads a refusal as the reason a release stopped, so it has to carry the size that
// was refused even though no bundle comes with it.
func TestABundleResultRefusalNamesTheSizeAndCarriesNoBundle(t *testing.T) {
	refusal := BundleWorkspaceResult{
		ID:        "bundle-2",
		SizeBytes: maxWorkspaceBundleBytes + 1,
		Error:     "worker-shim: the workspace's unpushed work is 41943041 bytes, over the 41943040 the release carries",
	}
	if err := refusal.Validate(); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(refusal)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > MaxFrameBytes {
		t.Fatalf("a refusal is %d bytes: it must fit one line", len(line))
	}
	frame, err := Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := frame.(BundleWorkspaceResult)
	if !ok {
		t.Fatalf("a refusal decodes to a %s", frame.FrameType())
	}
	if got.OK || len(got.Bundle) != 0 {
		t.Fatalf("the refusal decodes as ok=%v with %d bundle bytes", got.OK, len(got.Bundle))
	}
	if got.SizeBytes != maxWorkspaceBundleBytes+1 || !strings.Contains(got.Error, "41943041") {
		t.Fatalf("the refusal reads %d bytes and %q, want the size it refused in both", got.SizeBytes, got.Error)
	}
	// A refusal with no reason is a frame the daemon could only record as an empty string.
	if err := (BundleWorkspaceResult{ID: "bundle-3"}).Validate(); err == nil {
		t.Fatal("a refusal with no reason was accepted")
	}
	// So is a request the shim cannot act on: the daemon is its only sender.
	for name, request := range map[string]BundleWorkspace{
		"no id":         {Issue: "INFRA-945", MaxBytes: 1, TimeoutMs: 1},
		"no issue key":  {ID: "x", Issue: "legion/INFRA-945\") | all()", MaxBytes: 1, TimeoutMs: 1},
		"no bound":      {ID: "x", Issue: "INFRA-945", TimeoutMs: 1},
		"no budget":     {ID: "x", Issue: "INFRA-945", MaxBytes: 1},
		"a lowered key": {ID: "x", Issue: "infra-945", MaxBytes: 1, TimeoutMs: 1},
	} {
		if err := request.Validate(); err == nil {
			t.Errorf("a bundle-workspace with %s was accepted", name)
		}
	}
	if err := (BundleWorkspace{ID: "x", Issue: "INFRA-945", MaxBytes: maxWorkspaceBundleBytes, TimeoutMs: 1000}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// The launcher reads one frame per line and reassembles nothing of its own, so a bundle on its way
// to a pod is sliced here: every slice inside the frame limit, every slice declaring the digest of
// the body they rebuild, and the body itself holding the bundle raw so a 40 MiB save is base64'd
// once rather than twice.
func TestAWorkspaceBundleSlicesIntoFramesUnderTheLineLimit(t *testing.T) {
	saved := WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte("PACKjj\x00\x02"), (3<<20)/8),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e", "side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:        time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
	}
	frames, err := SplitWorkspaceBundle("restore-1", saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("a %d-byte bundle sliced into %d frames", len(saved.Bundle), len(frames))
	}
	var body []byte
	for index, frame := range frames {
		if err := frame.Validate(); err != nil {
			t.Fatalf("slice %d: %v", index, err)
		}
		if frame.Index != index || frame.Count != len(frames) || frame.ID != "restore-1" {
			t.Fatalf("slice %d is %d of %d under %q", index, frame.Index, frame.Count, frame.ID)
		}
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if len(line) >= MaxFrameBytes {
			t.Fatalf("slice %d encodes to %d bytes, which the launcher's reader refuses", index, len(line))
		}
		body = append(body, frame.Payload...)
	}
	if digest := BundleDigest(body); digest != frames[0].SHA256 {
		t.Fatalf("the body rebuilds to %s, not the %s the slices declared", digest, frames[0].SHA256)
	}
	got, err := ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bundle, saved.Bundle) || got.Head != saved.Head {
		t.Fatalf("the body rebuilt %d bundle bytes on %s, want %d on %s", len(got.Bundle), got.Head, len(saved.Bundle), saved.Head)
	}
	if !slices.Equal(got.Tips, saved.Tips) || !maps.Equal(got.Bookmarks, saved.Bookmarks) || !got.At.Equal(saved.At) {
		t.Fatalf("the body rebuilt %v / %v / %s", got.Tips, got.Bookmarks, got.At)
	}
	// The bundle rides raw behind one header line: the body is barely longer than the bundle,
	// where a base64 member would have made it a third longer again.
	if len(body) > len(saved.Bundle)+4096 {
		t.Fatalf("the body is %d bytes for a %d-byte bundle: the bundle is not raw in it", len(body), len(saved.Bundle))
	}
	if _, err := ParseWorkspaceBundle([]byte(`{"head":"x"}`)); err == nil {
		t.Fatal("a body with no bundle behind its header was accepted")
	}
	if _, err := SplitWorkspaceBundle("", saved); err == nil {
		t.Fatal("a sequence with no id was accepted")
	}
}
