package rules

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
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
	d, err = set.Evaluate("DEEL_API_KEY", Requester{Kind: "pod"})
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
    source: example/agent-secrets/X
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
      - {kind: box, operator: sjawhar, decision: automatic}
`)
	data = withApprovers(data, "sjawhar")
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
    source: example/agent-secrets/inject-api-key
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

// TestApprovalDeniesWhenApproverHasNoLiveKeys pins the fail-closed rule Parse's cross-checks
// exist to make safe: approver: login:<name> is accepted at parse time with zero declared keys
// (a login can be named in the approvers section before its first key is registered), but
// Evaluate then refuses the approval outright rather than resolving to an approver nobody can
// ever assert as — the same "approval with nobody to approve" refusal that previously fired for
// an unknown issue_assignee.
func TestApprovalDeniesWhenApproverHasNoLiveKeys(t *testing.T) {
	data := withApprovers(oneSecret("{kind: pod, decision: approval, approver: 'login:bob'}"))
	data = append(data, []byte("    bob: {}\n")...)
	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d, err := set.Evaluate("X", Requester{Kind: "pod"})
	if err != nil || d.Outcome != "deny" {
		t.Fatalf("approval for a login with zero declared keys must deny fail-closed: %+v %v", d, err)
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

// testAAGUID is a fixed, otherwise-meaningless AAGUID used everywhere a test needs a
// structurally valid (but never cryptographically verified — see rules.go's package doc comment
// on the shape/crypto split) approvers.aaguids entry.
const testAAGUID = "ee882879-721c-4913-9775-3dfcce97072a"

// withApprovers appends a minimal, structurally-valid approvers: section to a rules YAML
// document, declaring one fake seeded key per named login — enough to satisfy the
// login:/operator: cross-check and to give Evaluate a non-empty key count, without any real
// WebAuthn material (Parse never verifies attestation; see TestApproversSectionParses for a
// fixture built from a real registration).
func withApprovers(doc []byte, logins ...string) []byte {
	nonce := strings.Repeat("a", 64)
	s := string(doc) + "approvers:\n  origin: https://dispatch.test\n  aaguids: [\"" + testAAGUID + "\"]\n  logins:\n"
	for _, login := range logins {
		s += "    " + login + ":\n      keys:\n        - credential_id: \"" + login + "-cred\"\n" +
			"          registration:\n            challenge_nonce: \"" + nonce + "\"\n" +
			"            response: {id: \"x\"}\n          seed: true\n"
	}
	return []byte(s)
}

// TestLoginsCompareCaseInsensitively pins that a rules author's casing of a GitHub login never
// makes a rule unsatisfiable: operators and login: approvers are compared the way Dispatch
// compares logins, trimmed and lowercased.
func TestLoginsCompareCaseInsensitively(t *testing.T) {
	data := withApprovers(oneSecret("{kind: box, operator: SJawhar, decision: approval, approver: 'login:Xodarap'}"), "xodarap")
	set, err := Parse(data)
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
	data := withApprovers(oneSecret(
		"{kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: automatic}",
		"{kind: pod, service_account: 'system:serviceaccount:legion:reviewer', decision: approval, approver: 'login:alice'}",
	), "alice")
	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for subject, want := range map[string]string{
		"system:serviceaccount:legion:worker":   "automatic",
		"system:serviceaccount:legion:reviewer": "approval",
		"system:serviceaccount:default:other":   "deny",
		"":                                      "deny",
	} {
		d, err := set.Evaluate("X", Requester{Kind: "pod", Subject: subject})
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

// TestIssueAssigneeIsRefused pins that approver: issue_assignee is refused for every requester
// kind: approvals now resolve to a WebAuthn-attested login (the approvers section), never a
// Dispatch issue's assignee, so the only valid approver values are operator and login:<name>.
func TestIssueAssigneeIsRefused(t *testing.T) {
	for name, entry := range map[string]string{
		"box":  "{kind: box, operator: sjawhar, decision: approval, approver: issue_assignee}",
		"host": "{kind: host, operator: sjawhar, decision: approval, approver: issue_assignee}",
		"pod":  "{kind: pod, decision: approval, approver: issue_assignee}",
	} {
		if _, err := Parse(oneSecret(entry)); err == nil ||
			!strings.Contains(err.Error(), "approver must be operator or login:") {
			t.Errorf("%s + issue_assignee: Parse = %v, want a refusal naming \"approver must be operator or login:\"", name, err)
		}
	}
}

// TestLoginApproverNeedsAKeyEntry pins that approver: login:<name> is refused at parse time
// unless approvers.logins has an entry for <name> at all — zero keys is a valid entry (denies at
// evaluation time instead, see TestApprovalDeniesWhenApproverHasNoLiveKeys); no entry whatsoever
// is an authoring error caught immediately.
func TestLoginApproverNeedsAKeyEntry(t *testing.T) {
	entry := "{kind: pod, decision: approval, approver: 'login:bob'}"
	if _, err := Parse(oneSecret(entry)); err == nil || !strings.Contains(err.Error(), "login:bob has no approvers entry") {
		t.Fatalf("login:bob with no approvers section at all: Parse = %v, want a refusal naming \"login:bob has no approvers entry\"", err)
	}
	data := oneSecret(entry)
	data = append(data, []byte("approvers:\n  origin: https://dispatch.test\n  aaguids: [\""+testAAGUID+"\"]\n  logins:\n    bob: {}\n")...)
	if _, err := Parse(data); err != nil {
		t.Fatalf("login:bob with a zero-key approvers entry must still parse: %v", err)
	}
}

// TestOperatorApproverNeedsTheOperatorsLogin pins that approver: operator is refused at parse
// time unless approvers.logins has an entry for the matching entry's own operator.
func TestOperatorApproverNeedsTheOperatorsLogin(t *testing.T) {
	entry := "{kind: box, operator: sjawhar, decision: approval, approver: operator}"
	if _, err := Parse(oneSecret(entry)); err == nil || !strings.Contains(err.Error(), "operator sjawhar has no approvers entry") {
		t.Fatalf("operator sjawhar with no approvers section at all: Parse = %v, want a refusal naming \"operator sjawhar has no approvers entry\"", err)
	}
	data := oneSecret(entry)
	data = append(data, []byte("approvers:\n  origin: https://dispatch.test\n  aaguids: [\""+testAAGUID+"\"]\n  logins:\n    sjawhar: {}\n")...)
	if _, err := Parse(data); err != nil {
		t.Fatalf("operator sjawhar with a zero-key approvers entry must still parse: %v", err)
	}
}

// TestOriginMustBeAbsoluteHTTPSWithoutPath pins the approvers.origin loader rule.
func TestOriginMustBeAbsoluteHTTPSWithoutPath(t *testing.T) {
	base := "version: 1\nsecrets: {}\napprovers:\n  origin: %s\n  aaguids: [\"" + testAAGUID + "\"]\n  logins: {}\n"
	for name, origin := range map[string]string{
		"no scheme":         "dispatch.test",
		"http, not https":   "http://dispatch.test",
		"has a path":        "https://dispatch.test/",
		"has a nested path": "https://dispatch.test/api",
	} {
		if _, err := Parse([]byte(fmt.Sprintf(base, origin))); err == nil {
			t.Errorf("%s: origin %q must be refused", name, origin)
		}
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "https://dispatch.test"))); err != nil {
		t.Fatalf("a valid absolute https origin with no path must parse: %v", err)
	}
}

// TestApproversSectionParses builds a real registration (webauthntest, the same software
// authenticator Task 4's approvers package tests against) for one seeded key and confirms Parse
// carries the section's origin, AAGUIDs, and every KeyEntry field through unchanged.
func TestApproversSectionParses(t *testing.T) {
	ca := webauthntest.NewCA(t)
	aaguid := uuid.MustParse(testAAGUID)
	auth := ca.NewAuthenticator(t, aaguid)
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	registration := auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:])
	credentialID := base64.RawURLEncoding.EncodeToString(auth.CredentialID)

	data := []byte("version: 1\nsecrets: {}\napprovers:\n  origin: https://dispatch.test\n  aaguids: [\"" + testAAGUID + "\"]\n  logins:\n" +
		"    sjawhar:\n      keys:\n        - credential_id: \"" + credentialID + "\"\n" +
		"          registration:\n            challenge_nonce: \"" + nonce + "\"\n" +
		"            response: " + string(registration) + "\n          seed: true\n")

	set, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if set.Approvers.Origin != "https://dispatch.test" {
		t.Fatalf("origin = %q", set.Approvers.Origin)
	}
	if len(set.Approvers.AAGUIDs) != 1 || set.Approvers.AAGUIDs[0] != aaguid {
		t.Fatalf("aaguids = %v, want [%s]", set.Approvers.AAGUIDs, aaguid)
	}
	keys := set.Approvers.Logins["sjawhar"]
	if len(keys) != 1 {
		t.Fatalf("logins[sjawhar] = %+v, want one key", keys)
	}
	k := keys[0]
	if k.CredentialID != credentialID || k.ChallengeNonce != nonce || !k.Seed || k.Endorsement != nil {
		t.Fatalf("parsed key entry = %+v", k)
	}
	var got, want any
	if err := json.Unmarshal(k.Registration, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(registration, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registration round-trip = %v, want %v", got, want)
	}
}
