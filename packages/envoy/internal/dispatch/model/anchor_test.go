package model

import (
	"encoding/json"
	"testing"
)

// An anchor row an earlier release wrote can hold an artifact_id that is not a JSON string. A
// string is read as the document it names; any other JSON value leaves ArtifactID empty, which
// names no document, rather than failing the decode and with it the whole read. Every other field
// of the anchor decodes the same either way.
func TestDecodeAnchorReadsOnlyAStringArtifactID(t *testing.T) {
	const documentID = "11111111-1111-4111-8111-111111111111"
	for _, stored := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "string", value: `"` + documentID + `"`, want: documentID},
		{name: "number", value: `5`},
		{name: "null", value: `null`},
		{name: "object", value: `{"id": "` + documentID + `"}`},
		{name: "bool", value: `true`},
	} {
		t.Run(stored.name, func(t *testing.T) {
			var anchor Anchor
			encoded := `{"artifact_id": ` + stored.value + `, "block_id": "b-7", "mark_id": "m-7", "version": 3, "quote": "the lazy dog", "orphaned": true}`
			if err := DecodeAnchor([]byte(encoded), &anchor); err != nil {
				t.Fatalf("decode the anchor: %v", err)
			}
			if anchor.ArtifactID != stored.want {
				t.Errorf("artifact_id = %q, want %q", anchor.ArtifactID, stored.want)
			}
			if anchor.BlockID == nil || *anchor.BlockID != "b-7" {
				t.Errorf("block_id = %#v, want %q", anchor.BlockID, "b-7")
			}
			if anchor.MarkID != "m-7" || anchor.Version != 3 || anchor.Quote != "the lazy dog" || !anchor.Orphaned {
				t.Errorf("the anchor's other fields = %#v, want the mark, version, quote and orphaned flag it was encoded with", anchor)
			}
		})
	}
}

// Only artifact_id is read leniently: another field of the wrong type is still a decode error, so
// no malformed anchor passes as a valid one. That holds whichever field the decoder reaches
// first, so the lenient read of a number artifact_id never swallows the error beside it.
func TestDecodeAnchorStillFailsOnAnotherMalformedField(t *testing.T) {
	const documentID = "11111111-1111-4111-8111-111111111111"
	for _, encoded := range []struct{ name, value string }{
		{name: "a string artifact_id", value: `{"artifact_id": "` + documentID + `", "version": "seven"}`},
		{name: "a number artifact_id first", value: `{"artifact_id": 5, "version": "seven"}`},
		{name: "a number artifact_id second", value: `{"version": "seven", "artifact_id": 5}`},
	} {
		t.Run(encoded.name, func(t *testing.T) {
			var anchor Anchor
			if err := DecodeAnchor([]byte(encoded.value), &anchor); err == nil {
				t.Fatalf("decoding a string version = %#v, want an error", anchor)
			}
		})
	}
}

// An anchor encodes as it always did: artifact_id is a string on the wire, whatever a stored row
// held.
func TestAnchorEncodesArtifactIDAsAString(t *testing.T) {
	encoded, err := json.Marshal(Anchor{ArtifactID: "11111111-1111-4111-8111-111111111111", MarkID: "m-1", Version: 2, Quote: "q"})
	if err != nil {
		t.Fatalf("encode the anchor: %v", err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode the encoded anchor: %v", err)
	}
	if fields["artifact_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("encoded anchor = %s, want artifact_id as the document id string", encoded)
	}
}
