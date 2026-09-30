// packages/envoy/internal/broker/helper/credlog_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// proofShaped matches what a bearer in a log line would look like: a JWS or JWT (base64url JSON,
// "eyJ…", dot-separated segments) or PEM key material.
var proofShaped = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}|[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|-----BEGIN`)

// TestTheHelperLogsEveryChangeOfTheLauncherCredential: a machine login installing the credential
// and a broker refusal clearing it each leave one line in the journal, carrying identifiers only —
// the credential id, the operator, the broker's code — and never a proof, a request object or key
// material.
func TestTheHelperLogsEveryChangeOfTheLauncherCredential(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "sjawhar"), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil))}
	sess, _ := newSession(1, 1, "h:1:1", nil)

	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	credentialID := b.cred.Load().id
	if _, _, err := b.Enroll(context.Background(), sess); err != nil {
		t.Fatalf("a fresh credential must enroll: %v", err)
	}
	f.mu.Lock()
	f.enrollUnauthorizedNext = 1
	f.mu.Unlock()
	if _, _, err := b.Enroll(context.Background(), sess); err == nil {
		t.Fatal("an enroll the broker refuses 401 LAUNCHER_INVALID must fail")
	}

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		delete(rec, "time")
		records = append(records, rec)
	}
	want := []map[string]any{
		{"level": "INFO", "msg": "machine login issued; the helper holds a launcher credential", "credential_id": credentialID, "operator": "sjawhar"},
		{"level": "WARN", "msg": "launcher credential refused; cleared", "credential_id": credentialID, "code": "LAUNCHER_INVALID"},
	}
	if len(records) != len(want) {
		t.Fatalf("log records %v; want exactly %v", records, want)
	}
	for i, rec := range records {
		if len(rec) != len(want[i]) {
			t.Fatalf("record %d has fields %v; want exactly %v", i, rec, want[i])
		}
		for k, v := range want[i] {
			if rec[k] != v {
				t.Fatalf("record %d field %s = %v; want %v (record %v)", i, k, rec[k], v, rec)
			}
		}
	}
	f.mu.Lock()
	proof := f.lastProof
	f.mu.Unlock()
	if proof == "" || !proofShaped.MatchString(proof) {
		t.Fatalf("control: the proof the broker saw (%q) must look proof-shaped to the check below", proof)
	}
	if m := proofShaped.FindString(out.String()); m != "" {
		t.Fatalf("a log line carries something proof-shaped: %q in %s", m, out.String())
	}
	if strings.Contains(out.String(), proof) {
		t.Fatal("a log line carries the proof the broker saw")
	}
}
