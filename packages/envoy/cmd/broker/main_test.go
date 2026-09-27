// packages/envoy/cmd/broker/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refuseDevAttestationRootInProduction is main's own -dev-attestation-root/BROKER_RULES_S3_URI
// refusal (ruling 10), extracted so this test can drive it without exiting the test process.
package main

import "testing"

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
