// packages/envoy/cmd/agent-secrets/version_test.go

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionNamesTheStampedReleaseAndOtherwiseSaysDevel: the release job stamps its tag into the
// binary (release-envoy-listener.yaml), and the sitting runbook checks a restart with
// `--version`, so a stamped build prints exactly the tag. Any other build says devel rather than
// naming a release it is not.
func TestVersionNamesTheStampedReleaseAndOtherwiseSaysDevel(t *testing.T) {
	stamped := filepath.Join(t.TempDir(), "agent-secrets")
	build := exec.Command("go", "build", "-ldflags", "-X github.com/sjawhar/envoy/internal/buildversion.tag=legion-envoy-v9.9.9", "-o", stamped, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("stamped build: %v\n%s", err, out)
	}
	if out, err := exec.Command(stamped, "--version").Output(); err != nil || string(out) != "agent-secrets legion-envoy-v9.9.9\n" {
		t.Fatalf("stamped --version: %q (%v); want agent-secrets legion-envoy-v9.9.9", out, err)
	}
	if out, err := exec.Command(buildAgentSecrets(t), "--version").Output(); err != nil || !strings.HasPrefix(string(out), "agent-secrets devel") {
		t.Fatalf("local --version: %q (%v); want agent-secrets devel…", out, err)
	}
}
