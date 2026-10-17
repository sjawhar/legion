package gcp_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The script these tests run is the listener's seed script the golden cloud-config writes, byte for
// byte, with the golden's fetch-secret beside it, as the VM holds both, run by /bin/sh against the
// same fakes of the metadata server and Secret Manager the fetcher's own tests use.

// seedEnvRun is one run of the script: its output, its exit code, and the env file's directory.
type seedEnvRun struct {
	stdout, stderr string
	exitCode       int
	dir, envFile   string
}

// seedEnv runs the golden's listener-seed-env, with the golden's fetch-secret beside it, against f.
func seedEnv(t *testing.T, f *fakeGCP) seedEnvRun {
	t.Helper()
	var golden gcp.CloudConfig
	require.NoError(t, yamlUnmarshalGolden(t, &golden))
	scriptDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scriptDir, "fetch-secret"), []byte(fileOf(t, golden, "/etc/envoy/fetch-secret")), 0o700))
	script := filepath.Join(scriptDir, "listener-seed-env")
	require.NoError(t, os.WriteFile(script, []byte(fileOf(t, golden, "/etc/envoy/listener-seed-env")), 0o700))
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "sleep"), []byte(sleepStub), 0o700))
	dir := t.TempDir()
	envFile := filepath.Join(dir, "listener.env")

	cmd := shellCommand(t, script, envFile, fetchSecret)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SLEEP_LOG=" + filepath.Join(scriptDir, "sleeps"),
		"GCE_METADATA_HOST=" + f.metadata.Listener.Addr().String(),
		"SECRET_MANAGER_ENDPOINT=" + f.secrets.URL,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := seedEnvRun{stdout: stdout.String(), stderr: stderr.String(), dir: dir, envFile: envFile}
	if err != nil {
		run.exitCode = cmd.ProcessState.ExitCode()
		require.NotEqual(t, -1, run.exitCode, "the script ran: %v", err)
	}
	return run
}

// The env file holds the seed as the one variable NATS_NKEY_SEED, readable by root alone, and the
// directory holds nothing else: neither the raw fetch nor a temporary file is left behind.
func TestListenerSeedEnvWritesTheSeedAsOneVariableReadableByRootAlone(t *testing.T) {
	t.Parallel()
	seed, _ := testnats.User(t)
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) { fmt.Fprint(w, accessResponse(seed)) })
	run := seedEnv(t, f)

	require.Equal(t, 0, run.exitCode, "stderr: %s", run.stderr)
	require.Empty(t, run.stdout)
	require.Empty(t, run.stderr)
	got, err := os.ReadFile(run.envFile)
	require.NoError(t, err)
	require.Equal(t, core.EnvNATSSeed+"="+seed+"\n", string(got))
	info, err := os.Stat(run.envFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(run.dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the env file's directory holds no raw or temporary file")
}

// A seed of two lines would give the listener a second variable of the secret's choosing, so it is
// refused, and no env file is written.
func TestListenerSeedEnvRefusesASeedOfMoreThanOneLine(t *testing.T) {
	t.Parallel()
	seed, _ := testnats.User(t)
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) {
		fmt.Fprint(w, accessResponse(seed+"\nENVOY_API_TOKEN=x"))
	})
	run := seedEnv(t, f)

	require.NotEqual(t, 0, run.exitCode)
	require.Contains(t, run.stderr, "holds more than one line")
	require.NotContains(t, run.stderr, seed)
	entries, err := os.ReadDir(run.dir)
	require.NoError(t, err)
	require.Empty(t, entries, "no env file is written")
}

// A fetch that fails every attempt fails the script, so the listener's start fails and its unit
// tries again, and the previous boot's env file is not replaced.
func TestListenerSeedEnvFailsWhenTheFetchFails(t *testing.T) {
	t.Parallel()
	f := newFakeGCP(t, func(w http.ResponseWriter, _ int32) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, permissionDenied)
	})
	run := seedEnv(t, f)

	require.NotEqual(t, 0, run.exitCode)
	require.Contains(t, run.stderr, "nothing fetched from "+fetchSecret)
	entries, err := os.ReadDir(run.dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// A start timeout sends TERM while fetch-secret holds the raw seed. The handler must delete it:
// dash does not run an EXIT trap for that signal.
func TestListenerSeedEnvRemovesTheRawSeedWhenTerminatedDuringFetch(t *testing.T) {
	t.Parallel()
	var golden gcp.CloudConfig
	require.NoError(t, yamlUnmarshalGolden(t, &golden))
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "listener-seed-env")
	require.NoError(t, os.WriteFile(script, []byte(fileOf(t, golden, "/etc/envoy/listener-seed-env")), 0o700))
	started := filepath.Join(scriptDir, "fetch-started")
	require.NoError(t, os.WriteFile(filepath.Join(scriptDir, "fetch-secret"), []byte(`#!/bin/sh
printf seed >"$1"
: >"$FETCH_STARTED"
while :; do sleep 1; done
`), 0o700))
	dir := t.TempDir()

	cmd := shellCommand(t, script, filepath.Join(dir, "listener.env"), fetchSecret)
	cmd.Env = []string{"FETCH_STARTED=" + started, "PATH=" + os.Getenv("PATH")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM))
	require.Error(t, cmd.Wait())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the raw seed is removed when the start is terminated")
}
