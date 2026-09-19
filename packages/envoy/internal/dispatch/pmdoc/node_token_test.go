package pmdoc

import "testing"

func TestTokenJSONTracksMarksButNotIdentityOrServerState(t *testing.T) {
	base, err := Parse(":::ask{#decision urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	baseToken, err := base.TokenJSON()
	if err != nil {
		t.Fatalf("token base: %v", err)
	}

	identityOnly := cloneNode(base)
	identityOnly.Children[0].Attrs[BlockIDAttr] = "moved-decision"
	identityToken, err := identityOnly.TokenJSON()
	if err != nil {
		t.Fatalf("token identity: %v", err)
	}
	if string(identityToken) != string(baseToken) {
		t.Fatalf("identity changed token\nbase: %s\n got: %s", baseToken, identityToken)
	}

	serverState := cloneNode(base)
	serverState.Children[0].Attrs["state"] = "answered"
	serverToken, err := serverState.TokenJSON()
	if err != nil {
		t.Fatalf("token server state: %v", err)
	}
	if string(serverToken) != string(baseToken) {
		t.Fatalf("server state changed token\nbase: %s\n got: %s", baseToken, serverToken)
	}

	marked := cloneNode(base)
	text := marked.Children[0].Children[0].Children[0]
	text.Marks = []Mark{{Type: "proofComment", Attrs: Attrs{"id": "comment-1", "by": "user:alice"}}}
	markedToken, err := marked.TokenJSON()
	if err != nil {
		t.Fatalf("token marked: %v", err)
	}
	if string(markedToken) == string(baseToken) {
		t.Fatal("inline mark did not change token")
	}
}
