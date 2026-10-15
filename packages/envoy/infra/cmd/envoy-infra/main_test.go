package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/seed"
)

// buildCLI builds this package's binary at out with the extra go build arguments args.
func buildCLI(t *testing.T, out string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", append(append([]string{"build"}, args...), "-o", out, ".")...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %v: %v\n%s", args, err, b)
	}
}

// awsEnv is the whole environment of the binary run against endpoint: dummy static credentials, a
// home with no AWS files and no instance metadata, so the SDK reaches nothing but endpoint.
func awsEnv(t *testing.T, endpoint string) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"PATH=/usr/bin:/bin", "HOME=" + home,
		"AWS_ENDPOINT_URL=" + endpoint, "AWS_ACCESS_KEY_ID=AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY=example-signing-key",
		"AWS_CONFIG_FILE=" + filepath.Join(home, "config"), "AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(home, "credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
	}
}

// TestVersionPrintsTheStampedRelease: the release job stamps the CLI with
// `-ldflags "-X main.version=<X.Y.Z>"`, so a build stamped that way prints exactly that version
// and nothing else, without any AWS configuration. Any other build prints 0.0.0-dev, as the plugin
// does, rather than naming a release it is not.
func TestVersionPrintsTheStampedRelease(t *testing.T) {
	dir := t.TempDir()
	stamped, local := filepath.Join(dir, "stamped"), filepath.Join(dir, "local")
	buildCLI(t, stamped, "-ldflags", "-X main.version=9.9.9")
	buildCLI(t, local)

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

// brokenStdout fails every write, as stdout on a full disk (/dev/full) does. A closed stdout does
// not: the Go runtime opens it on the null device, where writes succeed.
type brokenStdout struct{}

func (brokenStdout) Write([]byte) (int, error) {
	return 0, errors.New("write /dev/stdout: bad file descriptor")
}

// TestVersionItCannotPrintExitsOne: a --version whose answer never reached stdout does not exit 0,
// so a script that reads it is not told the version was printed.
func TestVersionItCannotPrintExitsOne(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, brokenStdout{}, &stderr, noStore(t)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if want := "envoy-infra: could not print the version: write /dev/stdout: bad file descriptor\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
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

// TestAnInterruptDuringTheWriteEndsAsAnErrorAfterWriting: an interrupt while PutSecretValue is in
// flight cancels the call rather than killing the process, so generate ends as any write whose
// answer never came does: exit 1, the public key it printed before the write, `version id:
// unknown`, and the error telling the operator to check the secret.
func TestAnInterruptDuringTheWriteEndsAsAnErrorAfterWriting(t *testing.T) {
	const name = "example/envoy/nats-nkey-seed-example-host-a"
	binary := filepath.Join(t.TempDir(), "envoy-infra")
	buildCLI(t, binary)
	putArrived, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch r.Header.Get("X-Amz-Target") {
		case "secretsmanager.DescribeSecret":
			_, _ = w.Write([]byte(`{"ARN": "arn:aws:secretsmanager:us-east-1:000000000000:secret:` + name + `", "Name": "` + name + `"}`))
		case "secretsmanager.ListSecretVersionIds":
			_, _ = w.Write([]byte(`{"Versions": []}`))
		case "secretsmanager.PutSecretValue":
			close(putArrived)
			select { // the write hangs until the client gives up on it, or the test ends
			case <-r.Context().Done():
			case <-release:
			}
		default:
			t.Errorf("unexpected request %q", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	defer close(release)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, "seed", "generate", "--store", "aws-secrets-manager", "--name", name, "--region", "us-east-1")
	cmd.Env = awsEnv(t, server.URL)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-putArrived:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("PutSecretValue never arrived; stderr %q", stderr.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("generate ended with %v, want exit 1; stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	key, rest, _ := strings.Cut(strings.TrimPrefix(stdout.String(), "public key: "), "\n")
	if !core.IsUserPublicKey(key) || rest != "version id: unknown\n" {
		t.Errorf("stdout %q, want the public key and `version id: unknown`", stdout.String())
	}
	if want := "error after writing: put-secret-value " + name + " failed: canceled; whether a version was written is unknown, run check\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

// TestGenerateRefusesANullStdout: generate whose stdout is the null device would print the public
// key nowhere and write the seed, so it refuses before any call. A closed stdout is the same case,
// since the Go runtime opens a closed fd 0-2 on the null device at start (runtime/fds_unix.go).
func TestGenerateRefusesANullStdout(t *testing.T) {
	const name = "example/envoy/nats-nkey-seed-example-host-a"
	binary := filepath.Join(t.TempDir(), "envoy-infra")
	buildCLI(t, binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("generate made a call with a null stdout: %q", r.Header.Get("X-Amz-Target"))
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	env := awsEnv(t, server.URL)
	args := []string{"seed", "generate", "--store", "aws-secrets-manager", "--name", name, "--region", "us-east-1"}
	want := "refused: " + name + ": stdout is the null device, where the public key would be lost (a closed stdout is " +
		"one too: Go opens it on /dev/null); send stdout somewhere you can read. Nothing was written\n"
	for _, c := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		// os/exec gives a command with no Stdout the null device.
		{"stdout on /dev/null", exec.Command(binary, args...)},
		{"stdout closed", exec.Command("/bin/sh", append([]string{"-c", `exec "$0" "$@" >&-`, binary}, args...)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			c.cmd.Env, c.cmd.Stderr = env, &stderr
			err := c.cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("generate ended with %v, want exit 2; stderr %q", err, stderr.String())
			}
			if stderr.String() != want {
				t.Errorf("stderr %q, want %q", stderr.String(), want)
			}
		})
	}
}
