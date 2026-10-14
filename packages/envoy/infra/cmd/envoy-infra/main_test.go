package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/infra/seed"
)

// TestVersionPrintsTheStampedRelease: the release job stamps the CLI with
// `-ldflags "-X main.version=<X.Y.Z>"`, so a build stamped that way prints exactly that version
// and nothing else, without any AWS configuration. Any other build prints 0.0.0-dev, as the plugin
// does, rather than naming a release it is not.
func TestVersionPrintsTheStampedRelease(t *testing.T) {
	dir := t.TempDir()
	stamped, local := filepath.Join(dir, "stamped"), filepath.Join(dir, "local")
	build := func(out string, args ...string) {
		t.Helper()
		cmd := exec.Command("go", append(append([]string{"build"}, args...), "-o", out, ".")...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, b)
		}
	}
	build(stamped, "-ldflags", "-X main.version=9.9.9")
	build(local)

	for _, c := range []struct{ binary, want string }{{stamped, "9.9.9\n"}, {local, "0.0.0-dev\n"}} {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(c.binary, "--version")
		cmd.Env = []string{"PATH=/usr/bin:/bin"} // no HOME, AWS_* or shared AWS files
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s --version: %v; stderr %q", c.binary, err, stderr.String())
		}
		if stdout.String() != c.want || stderr.Len() != 0 {
			t.Errorf("%s --version: stdout %q, stderr %q; want stdout %q and no stderr", c.binary, stdout.String(), stderr.String(), c.want)
		}
	}
}

// noStore fails the test if a command opens a store: neither --version nor the usage reads one.
func noStore(t *testing.T) seed.Stores {
	return seed.Stores{seed.AWSSecretsManager: func(context.Context, string, string) (seed.Store, error) {
		t.Error("a store was opened")
		return nil, errors.New("no store in this test")
	}}
}

// TestVersionStandsAlone: --version followed by another word is a usage error, which echoes
// neither word, since a seed pasted there must not reach the terminal.
func TestVersionStandsAlone(t *testing.T) {
	const canary = "SUCANARYNOTASEED"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version", canary}, &stdout, &stderr, noStore(t)); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want none", stdout.String())
	}
	if !strings.Contains(stderr.String(), "envoy-infra: the command is seed, or --version alone\n") {
		t.Errorf("stderr %q does not say what the command takes", stderr.String())
	}
	if strings.Contains(stderr.String(), canary) {
		t.Errorf("stderr echoes the word given: %q", stderr.String())
	}
}

// TestHelpNamesVersion: the usage names --version beside the seed command's own usage.
func TestHelpNamesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr, noStore(t)); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, line := range []string{"usage: envoy-infra --version\n", "usage: envoy-infra seed {check|generate|verify}"} {
		if !strings.Contains(stderr.String(), line) {
			t.Errorf("the usage lacks %q: %q", line, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "the command is seed") {
		t.Errorf("--help reports a usage error: %q", stderr.String())
	}
}
