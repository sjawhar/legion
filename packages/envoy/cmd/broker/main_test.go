// packages/envoy/cmd/broker/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refuseDevAttestationRootInProduction is main's own -dev-attestation-root/BROKER_RULES_S3_URI
// refusal (ruling 10), extracted so this test can drive it without exiting the test process.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevAttestationRootRefusedWithRulesS3URI(t *testing.T) {
	err := refuseDevAttestationRootInProduction("/tmp/dev-ca.pem", "s3://bucket/agent-secret-rules.yaml")
	if err == nil {
		t.Fatal("-dev-attestation-root with BROKER_RULES_S3_URI set: want an error, got nil")
	}
	const want = "-dev-attestation-root is a development flag; production loads rules from S3 and trusts the embedded Yubico roots"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestMainRefusesDevAttestationRootWithRulesS3URI drives the REAL compiled binary's main(), not
// refuseDevAttestationRootInProduction directly — the prior three tests below prove only that the
// helper function itself is correct, and a commit already once deleted its only call site from
// main() while every one of those tests, go build, and go vet all stayed green, because none of
// them exercises the actual boot wiring. This builds cmd/broker once, execs it with both
// -dev-attestation-root and BROKER_RULES_S3_URI set (plus just enough other required BROKER_*
// variables for config.Load to succeed — the refusal runs immediately after config.Load and
// before store.Open, so no real Postgres or AWS credential is ever needed), and asserts the
// process exits non-zero naming the refusal reason on stderr. A future regression that drops the
// fatal(refuseDevAttestationRootInProduction(...)) call again would make this test time out
// waiting for a process that instead tries to open a nonexistent database, or exit 0/with an
// unrelated error — either way, it fails here where the unit tests above cannot catch it.
func TestMainRefusesDevAttestationRootWithRulesS3URI(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "broker")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/broker: %v\n%s", err, out)
	}

	caFile := filepath.Join(t.TempDir(), "dev-ca.pem")
	if err := os.WriteFile(caFile, []byte("not a real certificate, never read: refused before any PEM parse"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath, "-dev-attestation-root", caFile)
	cmd.Env = append(os.Environ(),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://broker.invalid",
		"BROKER_UI_ORIGIN=https://dispatch.invalid",
		"BROKER_UI_TOKEN=test-token-0123456789abcdef0123456789abcdef",
		"BROKER_RULES_S3_URI=s3://bucket/agent-secret-rules.yaml",
	)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker -dev-attestation-root with BROKER_RULES_S3_URI set: want a nonzero exit, got err=%v output=%s", err, out)
	}
	const wantSubstring = "-dev-attestation-root is a development flag; production loads rules from S3 and trusts the embedded Yubico roots"
	if !strings.Contains(string(out), wantSubstring) {
		t.Fatalf("broker refused to boot (exit %d) but its output didn't name the reason: %s\nwant it to contain: %s",
			exitErr.ExitCode(), out, wantSubstring)
	}
}

func TestDevAttestationRootAloneIsFine(t *testing.T) {
	if err := refuseDevAttestationRootInProduction("/tmp/dev-ca.pem", ""); err != nil {
		t.Fatalf("-dev-attestation-root with no BROKER_RULES_S3_URI: want nil, got %v", err)
	}
}

func TestNoDevAttestationRootIsFineEvenWithRulesS3URI(t *testing.T) {
	if err := refuseDevAttestationRootInProduction("", "s3://bucket/agent-secret-rules.yaml"); err != nil {
		t.Fatalf("no -dev-attestation-root: want nil, got %v", err)
	}
}
