package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// heredocEscape is a URL that, written raw into the watchdog's env-file heredoc, would end the
// heredoc and run a command of its own in the create script, which runs as root on the host.
const heredocEscape = "nats://hub.internal.example:4222\nENVEOF\ntouch /root/injected\ncat > /dev/null << 'ENVEOF'"

func TestListenerEnv(t *testing.T) {
	cases := []struct {
		name string
		in   core.ListenerConfig
		want []string
		err  string
	}{
		{
			name: "remote NATS, host bridge, the default port",
			in:   core.ListenerConfig{MachineID: "example-host-a", NATSURLs: []string{"nats://hub.internal.example:4222"}, Port: core.DefaultListenerPort, HostBridge: "127.0.0.1"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=example-host-a", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"},
		},
		{
			name: "loopback NATS needs no remote opt-in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9021},
			want: []string{"PORT=9021", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "any remote URL in the list opts in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://localhost:4222", "nats://hub.internal.example:4222"}, Port: 9020},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://localhost:4222,nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{name: "machine id required", in: core.ListenerConfig{NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020}, err: "machineId is required"},
		{name: "a blank machine id is refused", in: core.ListenerConfig{MachineID: " ", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020}, err: "machineId is required"},
		{name: "a NATS URL required", in: core.ListenerConfig{MachineID: "m", Port: 9020}, err: "natsUrls needs at least one URL"},
		{name: "a comma in a URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://a:1,nats://b:2"}, Port: 9020}, err: "natsUrls[0] contains a comma"},
		{name: "an empty URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{" "}, Port: 9020}, err: "natsUrls[0] is empty"},
		{name: "a refused URL is named by its index", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222", ""}, Port: 9020}, err: "natsUrls[1] is empty"},
		{name: "port 0 is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}}, err: "port 0 is outside 1-65535"},
		{name: "a negative port is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: -1}, err: "port -1 is outside 1-65535"},
		{name: "a port above 65535 is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 65536}, err: "port 65536 is outside 1-65535"},
		{name: "a blank host bridge is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020, HostBridge: " "}, err: "hostBridge has leading or trailing whitespace"},
		{name: "a host bridge with trailing whitespace is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020, HostBridge: "127.0.0.1 "}, err: "hostBridge has leading or trailing whitespace"},
		{name: "a URL with a newline is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{heredocEscape}, Port: 9020}, err: "natsUrls[0] contains a control character"},
		{name: "a URL with any control character is refused, by its index", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222", "nats://hub.internal.example:4222\r"}, Port: 9020}, err: "natsUrls[1] contains a control character"},
		{name: "a machine id with a newline is refused", in: core.ListenerConfig{MachineID: "a\nENVEOF\nid", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020}, err: `machineId contains a control character: "a\nENVEOF\nid"`},
		{name: "a host bridge with a newline inside is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9020, HostBridge: "127.0.0.1\nENVEOF"}, err: `hostBridge contains a control character: "127.0.0.1\nENVEOF"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := core.ListenerEnv(c.in)
			if c.err != "" {
				require.ErrorContains(t, err, c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

// CheckPlain checks only the fields a component knows at construct, so it passes a config whose
// natsUrls are not known yet and refuses what ListenerEnv would refuse of the others.
func TestListenerConfigCheckPlain(t *testing.T) {
	cases := []struct {
		name string
		in   core.ListenerConfig
		err  string
	}{
		{name: "no natsUrls yet", in: core.ListenerConfig{MachineID: "m", Port: core.DefaultListenerPort, HostBridge: "127.0.0.1"}},
		{name: "the default port", in: core.ListenerConfig{MachineID: "m", Port: core.DefaultListenerPort}},
		{name: "a blank machine id", in: core.ListenerConfig{MachineID: " ", Port: 9020}, err: "machineId is required"},
		{name: "a port above 65535", in: core.ListenerConfig{MachineID: "m", Port: 65536}, err: "port 65536 is outside 1-65535"},
		{name: "a negative port", in: core.ListenerConfig{MachineID: "m", Port: -1}, err: "port -1 is outside 1-65535"},
		{name: "port 0, which is no port", in: core.ListenerConfig{MachineID: "m"}, err: "port 0 is outside 1-65535"},
		{name: "a host bridge with leading whitespace", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: " 127.0.0.1"}, err: `hostBridge has leading or trailing whitespace: " 127.0.0.1"`},
		{name: "a machine id with a control character", in: core.ListenerConfig{MachineID: "example\thost", Port: 9020}, err: `machineId contains a control character: "example\thost"`},
		{name: "a host bridge with a control character inside", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: "127.0.0.1\nENVEOF"}, err: `hostBridge contains a control character: "127.0.0.1\nENVEOF"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.CheckPlain()
			if c.err != "" {
				require.ErrorContains(t, err, c.err)
				return
			}
			require.NoError(t, err)
		})
	}
}
