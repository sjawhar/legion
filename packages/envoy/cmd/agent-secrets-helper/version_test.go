// packages/envoy/cmd/agent-secrets-helper/version_test.go

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionNamesTheStampedRelease: the sitting runbook checks a helper restart with
// `agent-secrets-helper --version`, so a build stamped as the release job stamps it prints exactly
// the tag, and does so without any of the configuration `serve` needs (it answers before
// loadConfig). Any other build says devel rather than naming a release it is not.
func TestVersionNamesTheStampedRelease(t *testing.T) {
	dir := t.TempDir()
	stamped, local := filepath.Join(dir, "stamped"), filepath.Join(dir, "local")
	build := func(out string, args ...string) {
		t.Helper()
		cmd := exec.Command("go", append(append([]string{"build"}, args...), "-o", out, ".")...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, b)
		}
	}
	build(stamped, "-ldflags", "-X github.com/sjawhar/envoy/internal/buildversion.tag=legion-envoy-v9.9.9")
	build(local)

	run := func(binary string) string {
		t.Helper()
		cmd := exec.Command(binary, "--version")
		cmd.Env = []string{"PATH=/usr/bin:/bin"} // no HOME, XDG_RUNTIME_DIR or AGENT_SECRETS_URL
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s --version: %v", binary, err)
		}
		return string(out)
	}
	if got := run(stamped); got != "agent-secrets-helper legion-envoy-v9.9.9\n" {
		t.Fatalf("stamped --version: %q", got)
	}
	if got := run(local); !strings.HasPrefix(got, "agent-secrets-helper devel") {
		t.Fatalf("local --version: %q, want agent-secrets-helper devel…", got)
	}
}
