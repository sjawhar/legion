package model

import (
	"encoding/json"
	"testing"
)

// A typed payload spells the reference keys in its struct tags and a map payload spells them in
// NameReferenceChanges. They are the same two keys on the wire, so a reader cannot be right for
// one producer and wrong for another.
func TestTypedAndMapPayloadsNameTheSameReferenceKeys(t *testing.T) {
	changes := ReferenceChanges{
		Targets:   []ChangedReference{{Kind: "issue", ID: "CORE-1"}},
		Truncated: true,
	}
	encoded, err := json.Marshal(NewAskEventPayload(Ask{ID: "ask-1"}, changes))
	if err != nil {
		t.Fatalf("encode typed payload: %v", err)
	}
	typed := map[string]any{}
	if err := json.Unmarshal(encoded, &typed); err != nil {
		t.Fatalf("decode typed payload: %v", err)
	}
	mapped := map[string]any{}
	NameReferenceChanges(mapped, changes)

	for _, key := range []string{ReferencesChangedKey, ReferencesChangedTruncatedKey} {
		if _, named := typed[key]; !named {
			t.Fatalf("typed payload does not carry %q: %s", key, encoded)
		}
		if _, named := mapped[key]; !named {
			t.Fatalf("map payload does not carry %q: %#v", key, mapped)
		}
	}
	if len(mapped) != 2 {
		t.Fatalf("map payload keys = %#v, want exactly the two reference keys", mapped)
	}
}

// The ask backlink count is one wire key three producers spell: the Ask struct tag, the issue
// detail's ask rows and the map-shaped ask.* payloads an event read stamps it onto.
func TestAskCarriesTheBacklinkCountKey(t *testing.T) {
	count := 3
	encoded, err := json.Marshal(Ask{ID: "ask-1", ReferencedByCount: &count})
	if err != nil {
		t.Fatalf("encode ask: %v", err)
	}
	ask := map[string]any{}
	if err := json.Unmarshal(encoded, &ask); err != nil {
		t.Fatalf("decode ask: %v", err)
	}
	if ask[ReferencedByCountKey] != float64(count) {
		t.Fatalf("ask[%q] = %#v, want %d: %s", ReferencedByCountKey, ask[ReferencedByCountKey], count, encoded)
	}
}
