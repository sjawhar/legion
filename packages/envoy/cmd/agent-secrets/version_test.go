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
	for _, tc := range []struct {
		binary string
		check  func(string) bool
		want   string
	}{
		{stamped, func(s string) bool { return s == "agent-secrets legion-envoy-v9.9.9\n" }, "agent-secrets legion-envoy-v9.9.9"},
		{buildAgentSecrets(t), func(s string) bool { return strings.HasPrefix(s, "agent-secrets devel") }, "agent-secrets devel…"},
	} {
		out, err := exec.Command(tc.binary, "--version").Output()
		if err != nil || !tc.check(string(out)) {
			t.Fatalf("%s --version: %q (%v); want %s", tc.binary, out, err, tc.want)
		}
	}
}
