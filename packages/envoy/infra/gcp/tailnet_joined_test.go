package gcp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/gcp"
)

// tailnetJoined is the script the golden writes to /etc/envoy/tailnet-joined, run here by /bin/sh
// with jq from this machine, and docker and sleep stubbed first on PATH: docker answers each
// `docker exec tailscaled tailscale status --json` with the next of the states it is given, and
// fails for the state "down", as it does while the container is not up yet.
const dockerStub = `#!/bin/sh
printf '%s\n' "$*" >>"$DOCKER_LOG"
n=$(wc -l <"$DOCKER_LOG")
state=$(printf '%s\n' $STATES | sed -n "${n}p")
[ "$state" = down ] && exit 1
printf '{"BackendState":"%s","Self":{}}\n' "$state"
`

// joinedRun is one run of the script: the docker calls, the sleeps, and the marker and key file.
type joinedRun struct {
	docker, sleeps []string
	marker, key    string
}

// joinedHarness is the golden's joined script with docker and sleep stubbed, a data disk directory
// for the marker and a key file, which persist across the runs of one test, as across boots.
type joinedHarness struct {
	script, bin, logs string
	marker, key       string
}

func newJoinedHarness(t *testing.T) *joinedHarness {
	t.Helper()
	var doc gcp.CloudConfig
	require.NoError(t, yamlUnmarshalGolden(t, &doc))
	h := &joinedHarness{
		script: filepath.Join(t.TempDir(), "tailnet-joined"),
		bin:    t.TempDir(),
		logs:   t.TempDir(),
		marker: filepath.Join(t.TempDir(), "tailnet-joined"),
		key:    filepath.Join(t.TempDir(), "tailnet-auth-key"),
	}
	require.NoError(t, os.WriteFile(h.script, []byte(fileOf(t, doc, "/etc/envoy/tailnet-joined")), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(h.bin, "docker"), []byte(dockerStub), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(h.bin, "sleep"), []byte(sleepStub), 0o700))
	require.NoError(t, os.WriteFile(h.key, []byte(authKey), 0o600))
	return h
}

// run runs the script once, as one boot's tailnet-joined.service does, with docker answering the
// states given, and returns what it printed and its error.
func (h *joinedHarness) run(t *testing.T, states ...string) (string, error) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(h.logs, "docker"), nil, 0o600))
	require.NoError(t, os.RemoveAll(filepath.Join(h.logs, "sleeps")))
	cmd := shellCommand(t, h.script, h.marker, h.key)
	cmd.Env = []string{
		"PATH=" + h.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_LOG=" + filepath.Join(h.logs, "docker"),
		"SLEEP_LOG=" + filepath.Join(h.logs, "sleeps"),
		"STATES=" + strings.Join(states, " "),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runJoined(t *testing.T, states ...string) joinedRun {
	t.Helper()
	h := newJoinedHarness(t)
	out, err := h.run(t, states...)
	require.NoError(t, err, out)
	require.Empty(t, out, "the script prints nothing")
	run := joinedRun{marker: h.marker, key: h.key}
	docker, err := os.ReadFile(filepath.Join(h.logs, "docker"))
	require.NoError(t, err)
	run.docker = strings.Split(strings.TrimSuffix(string(docker), "\n"), "\n")
	if sleeps, err := os.ReadFile(filepath.Join(h.logs, "sleeps")); err == nil {
		run.sleeps = strings.Fields(string(sleeps))
	}
	return run
}

func yamlUnmarshalGolden(t *testing.T, doc *gcp.CloudConfig) error {
	t.Helper()
	return yaml.Unmarshal([]byte(readGolden(t, "cloud-config.golden")), doc)
}

// Once Tailscale reports the node Running, the script writes the marker on the data disk and
// empties the key file, keeping the file itself, which tailscaled's container binds: the key stays
// on the VM no longer than the first login needs, and no later boot fetches it.
func TestTailnetJoinedWaitsForTheNodeToRunThenDropsTheKey(t *testing.T) {
	run := runJoined(t, "down", "NeedsLogin", "Starting", "Running")
	require.Equal(t, []string{"5", "5", "5"}, run.sleeps, "it asks again every 5 seconds")
	require.Len(t, run.docker, 4)
	for _, call := range run.docker {
		require.Equal(t, "exec tailscaled tailscale --socket=/tmp/tailscaled.sock status --json", call)
	}
	_, err := os.Stat(run.marker)
	require.NoError(t, err, "the marker is written")
	key, err := os.ReadFile(run.key)
	require.NoError(t, err, "the key file stays, for tailscaled's bind mount")
	require.Empty(t, key, "the key is dropped")
}

func TestTailnetJoinedAtOnceWhenTheNodeRuns(t *testing.T) {
	run := runJoined(t, "Running")
	require.Empty(t, run.sleeps)
	_, err := os.Stat(run.marker)
	require.NoError(t, err)
}

// The key is dropped before the marker is written, so a drop that fails leaves no marker: the
// script exits non-zero without one, its unit runs it again 10 seconds later, and every later
// boot fetches and drops the key again until a drop succeeds. Here the key file cannot be emptied
// (its path is a directory, which refuses the truncation whoever runs the script); the next run,
// as the next boot, finds a key file it can empty and writes the marker.
func TestTailnetJoinedWritesNoMarkerWhenTheKeyCannotBeDropped(t *testing.T) {
	h := newJoinedHarness(t)
	require.NoError(t, os.Remove(h.key))
	require.NoError(t, os.Mkdir(h.key, 0o700))
	out, err := h.run(t, "Running")
	require.Error(t, err, "a failed drop is a failed run, which its unit retries")
	_, statErr := os.Stat(h.marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "no marker, so the next boot fetches and drops the key again: %s", out)

	require.NoError(t, os.Remove(h.key))
	require.NoError(t, os.WriteFile(h.key, []byte(authKey), 0o600))
	out, err = h.run(t, "Running")
	require.NoError(t, err, out)
	_, statErr = os.Stat(h.marker)
	require.NoError(t, statErr, "the next run writes the marker")
	key, err := os.ReadFile(h.key)
	require.NoError(t, err)
	require.Empty(t, key, "and has dropped the key first")
}

// The fetch is skipped at every boot after the marker is written, and runs only on a mounted data
// disk, where the marker is; tailnet-joined starts after tailscaled and watches for the marker's
// absence too.
func TestCloudConfigFetchesTheKeyOnlyUntilTheHubHasJoined(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)
	fetch := fileOf(t, doc, "/etc/systemd/system/fetch-tailnet-key.service")
	require.Contains(t, fetch, "\nConditionPathExists=!/mnt/disks/data/tailnet-joined\n")
	require.Contains(t, fetch, "\nAssertPathIsMountPoint=/mnt/disks/data\n")

	joined := fileOf(t, doc, "/etc/systemd/system/tailnet-joined.service")
	require.Contains(t, joined, "\nAfter=tailscaled.service\n")
	require.Contains(t, joined, "\nConditionPathExists=!/mnt/disks/data/tailnet-joined\n")
	require.Contains(t, joined, "\nAssertPathIsMountPoint=/mnt/disks/data\n")
	require.Contains(t, joined, "\nExecStart=/etc/envoy/tailnet-joined /mnt/disks/data/tailnet-joined /run/envoy/tailnet-auth-key\n")
	require.Contains(t, joined, "\nType=simple\n", "its start returns at once, so runcmd goes on to NATS")
	require.Contains(t, joined, "\nRestart=on-failure\nRestartSec=10\n", "a failed key drop is tried again this boot")

	started := strings.Join(doc.Runcmd, "\n")
	require.Less(t, strings.Index(started, "start tailscaled.service"), strings.Index(started, "start tailnet-joined.service"))
	require.Contains(t, started, "start tailnet-joined.service")
}
