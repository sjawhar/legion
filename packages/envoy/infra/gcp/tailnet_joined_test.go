package gcp_test

import (
	"os"
	"os/exec"
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

func runJoined(t *testing.T, states ...string) joinedRun {
	t.Helper()
	var doc gcp.CloudConfig
	require.NoError(t, yamlUnmarshalGolden(t, &doc))
	script := filepath.Join(t.TempDir(), "tailnet-joined")
	require.NoError(t, os.WriteFile(script, []byte(fileOf(t, doc, "/etc/envoy/tailnet-joined")), 0o700))
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(dockerStub), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sleep"), []byte(sleepStub), 0o700))
	logs := t.TempDir()
	disk := t.TempDir()
	run := joinedRun{marker: filepath.Join(disk, "tailnet-joined"), key: filepath.Join(t.TempDir(), "tailnet-auth-key")}
	require.NoError(t, os.WriteFile(run.key, []byte(authKey), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(logs, "docker"), nil, 0o600))

	cmd := exec.Command("/bin/sh", script, run.marker, run.key)
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_LOG=" + filepath.Join(logs, "docker"),
		"SLEEP_LOG=" + filepath.Join(logs, "sleeps"),
		"STATES=" + strings.Join(states, " "),
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Empty(t, string(out), "the script prints nothing")
	docker, err := os.ReadFile(filepath.Join(logs, "docker"))
	require.NoError(t, err)
	run.docker = strings.Split(strings.TrimSuffix(string(docker), "\n"), "\n")
	if sleeps, err := os.ReadFile(filepath.Join(logs, "sleeps")); err == nil {
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

	started := strings.Join(doc.Runcmd, "\n")
	require.Less(t, strings.Index(started, "start tailscaled.service"), strings.Index(started, "start tailnet-joined.service"))
	require.Contains(t, started, "start tailnet-joined.service")
}
