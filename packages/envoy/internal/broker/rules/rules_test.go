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

func TestEmptyRequestersDeniesEveryone(t *testing.T) {
	data := []byte(`version: 1
secrets:
  DEEL_API_KEY:
    source: production/agent-secrets/deel-api-key
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters: []
`)
	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := set.Evaluate("DEEL_API_KEY", Requester{Kind: "box", Operator: "sjawhar"}); err != nil || d.Outcome != "deny" {
		t.Fatalf("box: %+v %v", d, err)
	}
	if d, err := set.Evaluate("DEEL_API_KEY", Requester{Kind: "host", Operator: "sjawhar"}); err != nil || d.Outcome != "deny" {
		t.Fatalf("host: %+v %v", d, err)
	}
	if d, err := set.Evaluate("DEEL_API_KEY", Requester{Kind: "pod"}); err != nil || d.Outcome != "deny" {
		t.Fatalf("pod: %+v %v", d, err)
	}
}

func TestApprovalWithNoApproverDenies(t *testing.T) {
	data := []byte(`version: 1
secrets:
  DEEL_API_KEY:
    source: production/agent-secrets/deel-api-key
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - kind: pod
        decision: approval
        approver: issue_assignee
`)
	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d, err := set.Evaluate("DEEL_API_KEY", Requester{Kind: "pod"})
	if err != nil || d.Outcome != "deny" {
		t.Fatalf("expected deny when no issue assignee is known, got %+v %v", d, err)
	}
}

const singleDocBase = "version: 1\nsecrets:\n  X:\n    source: s\n    owner: o\n    delivery: inject\n    max_lifetime_seconds: 60\n    requesters: []\n"

func TestSingleDocumentAccepted(t *testing.T) {
	cases := map[string]string{
		"plain document":                    singleDocBase,
		"trailing whitespace and comments":  singleDocBase + "\n# a trailing comment\n\n",
		"explicit end marker plus comments": singleDocBase + "...\n# a trailing comment\n",
		"trailing bare document separator":  singleDocBase + "---\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err != nil {
			t.Fatalf("%s: expected acceptance, got %v", name, err)
		}
	}
}

func TestMultiDocumentRefused(t *testing.T) {
	data := []byte(singleDocBase + "---\nanything: goes\n")
	if _, err := Parse(data); err == nil {
		t.Fatal("expected multi-document rules file to be refused")
	}
}

func TestRealDocumentAfterEmptyExtraRefused(t *testing.T) {
	data := []byte(singleDocBase + "---\n# empty second\n---\nfoo: bar\n")
	if _, err := Parse(data); err == nil {
		t.Fatal("expected a real third document, following an empty second one, to be refused")
	}
}

// oneSecret is a rules file with one secret X whose requesters list is entries, one YAML flow
// mapping per line.
func oneSecret(entries ...string) []byte {
	doc := "version: 1\nsecrets:\n  X:\n    source: s\n    owner: o\n    delivery: inject\n    max_lifetime_seconds: 60\n    requesters:\n"
	for _, e := range entries {
		doc += "      - " + e + "\n"
	}
	return []byte(doc)
}

// TestLoginsCompareCaseInsensitively pins that a rules author's casing of a GitHub login never
// makes a rule unsatisfiable: operators and login: approvers are compared the way Dispatch
// compares logins, trimmed and lowercased.
func TestLoginsCompareCaseInsensitively(t *testing.T) {
	set, err := Parse(oneSecret("{kind: box, operator: SJawhar, decision: approval, approver: 'login:Xodarap'}"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := set.Evaluate("X", Requester{Kind: "box", Operator: "sjawhar"})
	if err != nil || d.Outcome != "approval" || d.Approver != "xodarap" {
		t.Fatalf("box/sjawhar against operator SJawhar, approver login:Xodarap = %+v %v, want approval by xodarap", d, err)
	}
}

// TestUnsatisfiableEntriesRefused pins the load-time refusals for entries no requester could ever
// satisfy or whose field would be silently ignored.
func TestUnsatisfiableEntriesRefused(t *testing.T) {
	for name, entry := range map[string]string{
		"login: with no name":         "{kind: box, operator: sjawhar, decision: approval, approver: 'login:'}",
		"login: with only blanks":     "{kind: box, operator: sjawhar, decision: approval, approver: 'login:  '}",
		"operator approver for a pod": "{kind: pod, decision: approval, approver: operator}",
		"approver with automatic":     "{kind: box, operator: sjawhar, decision: automatic, approver: operator}",
		"service_account on a box":    "{kind: box, operator: sjawhar, service_account: 'system:serviceaccount:legion:worker', decision: automatic}",
		"malformed service_account":   "{kind: pod, service_account: 'legion/worker', decision: automatic}",
	} {
		if _, err := Parse(oneSecret(entry)); err == nil || !strings.Contains(err.Error(), "requesters[0]") {
			t.Errorf("%s: Parse = %v, want a refusal naming requesters[0]", name, err)
		}
	}
}

// TestPodEntriesScopeToServiceAccounts pins service_account: a pod entry naming one matches only
// pods whose verified subject is that account, two entries may name two accounts, and an entry
// with no service_account (which matches every pod) cannot sit beside another pod entry.
func TestPodEntriesScopeToServiceAccounts(t *testing.T) {
	set, err := Parse(oneSecret(
		"{kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: automatic}",
		"{kind: pod, service_account: 'system:serviceaccount:legion:reviewer', decision: approval, approver: issue_assignee}",
	))
	if err != nil {
		t.Fatal(err)
	}
	for subject, want := range map[string]string{
		"system:serviceaccount:legion:worker":   "automatic",
		"system:serviceaccount:legion:reviewer": "approval",
		"system:serviceaccount:default:other":   "deny",
		"":                                      "deny",
	} {
		d, err := set.Evaluate("X", Requester{Kind: "pod", Subject: subject, IssueAssignee: "alice"})
		if err != nil || d.Outcome != want {
			t.Errorf("pod as %q: %+v %v, want %s", subject, d, err, want)
		}
	}
	if _, err := Parse(oneSecret(
		"{kind: pod, decision: automatic}",
		"{kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: deny}",
	)); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("a pod entry for every account beside one for a named account: Parse = %v, want ambiguous", err)
	}
	unscoped, err := Parse(oneSecret("{kind: pod, decision: automatic}"))
	if err != nil {
		t.Fatal(err)
	}
	if d, err := unscoped.Evaluate("X", Requester{Kind: "pod", Subject: "system:serviceaccount:any:thing"}); err != nil || d.Outcome != "automatic" {
		t.Fatalf("an entry with no service_account must still match every pod: %+v %v", d, err)
	}
}

// TestIssueAssigneeApprovalRefusedForBoxOrHost pins that approver: issue_assignee is valid only
// for a pod requester: a box or host has no approving issue, so a rule mixing the two is an
// authoring error refused at parse time (F1-NEW).
func TestIssueAssigneeApprovalRefusedForBoxOrHost(t *testing.T) {
	for name, entry := range map[string]string{
		"box":  "{kind: box, operator: sjawhar, decision: approval, approver: issue_assignee}",
		"host": "{kind: host, operator: sjawhar, decision: approval, approver: issue_assignee}",
	} {
		if _, err := Parse(oneSecret(entry)); err == nil ||
			!strings.Contains(err.Error(), "issue_assignee") || !strings.Contains(err.Error(), "requesters[0]") {
			t.Errorf("%s + issue_assignee: Parse = %v, want a refusal naming issue_assignee and requesters[0]", name, err)
		}
	}
	if _, err := Parse(oneSecret("{kind: pod, decision: approval, approver: issue_assignee}")); err != nil {
		t.Fatalf("pod + issue_assignee must still parse: %v", err)
	}
}
