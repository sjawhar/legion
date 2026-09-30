// packages/envoy/internal/broker/helper/credlog_test.go
//go:build linux

package helper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// proofShaped matches what a bearer in a log line would look like: a JWS or JWT (base64url JSON,
// "eyJ…", dot-separated segments) or PEM key material.
var proofShaped = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}|[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|-----BEGIN`)

// TestTheHelperLogsEveryChangeOfTheLauncherCredential: a machine login installing the credential
// and a broker refusal clearing it each leave one line in the journal, carrying identifiers only —
// the credential id, the operator the login was signed with, the broker's code — and never a
// proof, a request object or key material. The operator file changes while the login is pending,
// and the line still names the operator the login was signed with.
func TestTheHelperLogsEveryChangeOfTheLauncherCredential(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	operator := operatorFile(t, "sjawhar")
	b := &Broker{URL: f.srv.URL, OperatorFile: operator, HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil))}
	sess, _ := newSession(1, 1, "h:1:1", nil)

	f.mu.Lock()
	f.loginOutcome = "pending"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operator, []byte("someone-else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.loginOutcome = "issued"
	f.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second) // pollLogin's next poll comes 2 s after the pending one
	for b.LoginStatus().State != "issued" {
		if time.Now().After(deadline) {
			t.Fatalf("the login never issued: %+v", b.LoginStatus())
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}); !proofShaped.Match(keyPEM) {
		t.Fatalf("control: PEM key material must look proof-shaped to the check below: %s", keyPEM)
	}
	if m := proofShaped.FindString(out.String()); m != "" {
		t.Fatalf("a log line carries something proof-shaped: %q in %s", m, out.String())
	}
	if strings.Contains(out.String(), proof) {
		t.Fatal("a log line carries the proof the broker saw")
	}
}
