package gcp_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/gcp"
)

// The script these tests run is the one the golden cloud-config writes, byte for byte, run as
// the VM runs it: by /bin/sh with curl, jq and base64, which Container-Optimized OS ships. The
// metadata server and Secret Manager are fakes it reaches through GCE_METADATA_HOST and
// SECRET_MANAGER_ENDPOINT; sleep is a stub that records how long the script asked to wait.

const (
	fetchSecret = "example-hub-tailnet-auth-key"
	token       = "ya29.example-token"
	accessPath  = "/v1/projects/example-project/secrets/" + fetchSecret + "/versions/latest:access"
)

// fetchScript returns the content the golden cloud-config writes to /etc/envoy/fetch-tailnet-key.
func fetchScript(t *testing.T) string {
	t.Helper()
	var doc gcp.CloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(readGolden(t, "cloud-config.golden")), &doc))
	return fileOf(t, doc, "/etc/envoy/fetch-tailnet-key")
}

// accessResponse is Secret Manager's answer to an access call for a version holding payload.
func accessResponse(payload string) string {
	return `{
  "name": "projects/123456789012/secrets/` + fetchSecret + `/versions/1",
  "payload": {
    "data": "` + base64.StdEncoding.EncodeToString([]byte(payload)) + `",
    "dataCrc32c": "1234567890"
  }
}
`
}

const permissionDenied = `{
  "error": {
    "code": 403,
    "message": "Permission 'secretmanager.versions.access' denied for resource 'projects/example-project/secrets/` + fetchSecret + `/versions/latest' (or it may not exist).",
    "status": "PERMISSION_DENIED"
  }
}
`

// fakeGCP is a metadata server, which answers only with the Metadata-Flavor header, and a Secret
// Manager, which answers access calls carrying the metadata server's token with access, called
// with the 1-based number of the call.
type fakeGCP struct {
	metadata, secrets *httptest.Server
	accessCalls       atomic.Int32
}

func newFakeGCP(t *testing.T, access func(w http.ResponseWriter, call int32)) *fakeGCP {
	t.Helper()
	f := &fakeGCP{}
	f.metadata = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor:Google header", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/computeMetadata/v1/project/project-id":
			fmt.Fprint(w, "example-project")
		case "/computeMetadata/v1/instance/service-accounts/default/token":
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3599,"token_type":"Bearer"}`, token)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.metadata.Close)
	f.secrets = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != accessPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		access(w, f.accessCalls.Add(1))
	}))
	t.Cleanup(f.secrets.Close)
	return f
}

// fetchRun is one run of the script: its output, its exit code, the directory holding the key
// file, and the argument of each sleep it ran, in order.
type fetchRun struct {
	stdout, stderr string
	exitCode       int
	dir, out       string
	sleeps         []string
}

// sleepStub stands in for sleep(1) first on the script's PATH: it records its argument in the
// file SLEEP_LOG names and returns at once, so a run takes no wall time between attempts and the
// test reads the spacing the script asked for.
const sleepStub = `#!/bin/sh
printf '%s\n' "$1" >>"$SLEEP_LOG"
`

// fetch runs the script against f, writing to a key file that already exists, empty and mode
// 0600, as the cloud-config's bootcmd leaves it.
func fetch(t *testing.T, f *fakeGCP) fetchRun {
	t.Helper()
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fetch-tailnet-key")
	require.NoError(t, os.WriteFile(script, []byte(fetchScript(t)), 0o700))
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "sleep"), []byte(sleepStub), 0o700))
	sleepLog := filepath.Join(scriptDir, "sleeps")
	dir := t.TempDir()
	out := filepath.Join(dir, "tailnet-auth-key")
	require.NoError(t, os.WriteFile(out, nil, 0o600))

	cmd := shellCommand(t, script, out, fetchSecret)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SLEEP_LOG=" + sleepLog,
		"GCE_METADATA_HOST=" + f.metadata.Listener.Addr().String(),
		"SECRET_MANAGER_ENDPOINT=" + f.secrets.URL,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := fetchRun{stdout: stdout.String(), stderr: stderr.String(), dir: dir, out: out}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		run.exitCode = exit.ExitCode()
	} else {
		require.NoError(t, err)
	}
	sleeps, err := os.ReadFile(sleepLog)
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
		run.sleeps = strings.Fields(string(sleeps))
	}
	return run
}

// requireNothingSecretPrinted requires neither the key, its base64 form nor the token on the
// script's output.
func requireNothingSecretPrinted(t *testing.T, run fetchRun) {
	t.Helper()
	for _, secret := range []string{authKey, base64.StdEncoding.EncodeToString([]byte(authKey)), token} {
		require.NotContains(t, run.stdout, secret)
		require.NotContains(t, run.stderr, secret)
	}
}

// requireOnlyTheKeyFile requires the key file's directory to hold the key file alone, with mode
// 0600 and content want.
func requireOnlyTheKeyFile(t *testing.T, run fetchRun, want string) {
	t.Helper()
	entries, err := os.ReadDir(run.dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the key file's directory holds no temporary file")
	info, err := os.Stat(run.out)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err := os.ReadFile(run.out)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func TestFetchKeyWritesTheKeyReadableByRootAlone(t *testing.T) {
	t.Parallel()
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) { fmt.Fprint(w, accessResponse(authKey)) })
	run := fetch(t, f)

	require.Equal(t, 0, run.exitCode, "stderr: %s", run.stderr)
	require.Empty(t, run.stdout)
	require.Empty(t, run.stderr)
	requireOnlyTheKeyFile(t, run, authKey)
	require.Equal(t, int32(1), f.accessCalls.Load())
	require.Empty(t, run.sleeps, "a first attempt that succeeds waits for nothing")
}

// A grant on a new secret can take a while to reach Secret Manager, so a refusal is tried again,
// six times in all, ten seconds apart, before the script fails.
func TestFetchKeyFailsOnAPermissionDeniedAfterSixAttempts(t *testing.T) {
	t.Parallel()
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, permissionDenied)
	})
	run := fetch(t, f)

	require.NotEqual(t, 0, run.exitCode)
	require.Equal(t, int32(6), f.accessCalls.Load())
	require.Equal(t, []string{"10", "10", "10", "10", "10"}, run.sleeps, "ten seconds between attempts")
	require.Contains(t, run.stderr, "no key after 6 attempts")
	requireNothingSecretPrinted(t, run)
	requireOnlyTheKeyFile(t, run, "")
}

func TestFetchKeyRecoversFromAFailedAttempt(t *testing.T) {
	t.Parallel()
	f := newFakeGCP(t, func(w http.ResponseWriter, call int32) {
		if call == 1 {
			http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, accessResponse(authKey))
	})
	run := fetch(t, f)

	require.Equal(t, 0, run.exitCode, "stderr: %s", run.stderr)
	require.Equal(t, int32(2), f.accessCalls.Load())
	require.Equal(t, []string{"10"}, run.sleeps, "ten seconds before the second attempt")
	requireNothingSecretPrinted(t, run)
	requireOnlyTheKeyFile(t, run, authKey)
}

// A version whose payload is empty never becomes the key file: the tailscale CLI reads a key file
// trimmed, so an empty one is no key at all (v1.104.1 cmd/tailscale/cli/up.go:213-224).
func TestFetchKeyRefusesAnEmptyPayload(t *testing.T) {
	t.Parallel()
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) { fmt.Fprint(w, accessResponse("")) })
	run := fetch(t, f)

	require.NotEqual(t, 0, run.exitCode)
	require.Equal(t, int32(6), f.accessCalls.Load())
	require.Equal(t, []string{"10", "10", "10", "10", "10"}, run.sleeps, "ten seconds between attempts")
	require.Contains(t, run.stderr, "holds no key")
	requireOnlyTheKeyFile(t, run, "")
}
