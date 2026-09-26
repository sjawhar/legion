package rules

import (
	"os"
	"strings"
	"testing"
)

func TestValidFileEvaluates(t *testing.T) {
	data, _ := os.ReadFile("testdata/valid.yaml")
	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d, err := set.Evaluate("DEEL_API_KEY", Requester{Kind: "box", Operator: "sjawhar"})
	if err != nil || d.Outcome != "approval" || d.Approver != "sjawhar" || d.Delivery != "inject" {
		t.Fatalf("box/sjawhar: %+v %v", d, err)
	}
	d, err = set.Evaluate("DEEL_API_KEY", Requester{Kind: "pod", IssueAssignee: "alice"})
	if err != nil || d.Outcome != "approval" || d.Approver != "alice" {
		t.Fatalf("pod: %+v %v", d, err)
	}
	d, err = set.Evaluate("DEEL_API_KEY", Requester{Kind: "box", Operator: "mallory"})
	if err != nil || d.Outcome != "deny" {
		t.Fatalf("unmatched requester must deny: %+v %v", d, err)
	}
	if _, err := set.Evaluate("NOPE", Requester{Kind: "box", Operator: "sjawhar"}); err != ErrUnknownSecret {
		t.Fatalf("unknown secret: %v", err)
	}
}

func TestAmbiguousRequesterRefused(t *testing.T) {
	data := []byte(`version: 1
secrets:
  X:
    source: dev1/agent-secrets/X
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
      - {kind: box, operator: sjawhar, decision: automatic}
`)
	_, err := Parse(data)
	if err == nil || !strings.Contains(err.Error(), "ambiguous requester") {
		t.Fatalf("expected ambiguous requester refusal, got %v", err)
	}
}

func TestMissingRequestersRefused(t *testing.T) {
	data := []byte("version: 1\nsecrets:\n  X:\n    source: s\n    owner: o\n    delivery: inject\n    max_lifetime_seconds: 60\n")
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "requesters") {
		t.Fatalf("expected missing-requesters refusal, got %v", err)
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	data := []byte("version: 1\nsecrets:\n  X:\n    source: s\n    owner: o\n    delivery: inject\n    max_lifetime_seconds: 60\n    requesters: []\n    colour: red\n")
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("expected unknown-key refusal naming colour, got %v", err)
	}
}
