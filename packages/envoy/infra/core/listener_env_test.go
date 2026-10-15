package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// heredocEscape is a URL that, written raw into the watchdog's env-file heredoc, would end the
// heredoc and run a command of its own in the create script, which runs as root on the host.
const heredocEscape = "nats://hub.internal.example:4222\nENVEOF\ntouch /root/injected\ncat > /dev/null << 'ENVEOF'"

// requireInputError requires err to be a core.InputError refusing input, whose message holds msg.
func requireInputError(t *testing.T, err error, input, msg string) {
	t.Helper()
	require.ErrorContains(t, err, msg)
	var refused *core.InputError
	require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
	require.Equal(t, input, refused.Input)
}

// Check refuses every value of the config the listener cannot run on, each naming its input. This
// is the one table of those refusals: a component that calls Check tests only that it does, and
// where its refusal goes.
func TestListenerConfigCheck(t *testing.T) {
	cases := []struct {
		name  string
		in    core.ListenerConfig
		input string // the input a refusal names
		err   string
	}{
		{name: "the default port", in: core.ListenerConfig{MachineID: "m", Port: core.DefaultListenerPort}},
		{name: "a host bridge", in: core.ListenerConfig{MachineID: "m", Port: core.DefaultListenerPort, HostBridge: "127.0.0.1"}},
		{name: "no machine id", in: core.ListenerConfig{Port: 9020}, input: "machineId", err: "machineId is required"},
		{name: "a blank machine id", in: core.ListenerConfig{MachineID: " ", Port: 9020}, input: "machineId", err: "machineId is required"},
		{name: "a machine id with a newline", in: core.ListenerConfig{MachineID: "a\nENVEOF\nid", Port: 9020}, input: "machineId", err: `machineId contains a control character: "a\nENVEOF\nid"`},
		{name: "a machine id with a tab", in: core.ListenerConfig{MachineID: "example\thost", Port: 9020}, input: "machineId", err: `machineId contains a control character: "example\thost"`},
		{name: "port 0, which is no port", in: core.ListenerConfig{MachineID: "m"}, input: "port", err: "port 0 is outside 1-65535"},
		{name: "a negative port", in: core.ListenerConfig{MachineID: "m", Port: -1}, input: "port", err: "port -1 is outside 1-65535"},
		{name: "a port above 65535", in: core.ListenerConfig{MachineID: "m", Port: 65536}, input: "port", err: "port 65536 is outside 1-65535"},
		{name: "a blank host bridge", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: " "}, input: "hostBridge", err: `hostBridge has leading or trailing whitespace: " "`},
		{name: "a host bridge with leading whitespace", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: " 127.0.0.1"}, input: "hostBridge", err: `hostBridge has leading or trailing whitespace: " 127.0.0.1"`},
		{name: "a host bridge with trailing whitespace", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: "127.0.0.1 "}, input: "hostBridge", err: `hostBridge has leading or trailing whitespace: "127.0.0.1 "`},
		{name: "a host bridge with a newline inside", in: core.ListenerConfig{MachineID: "m", Port: 9020, HostBridge: "127.0.0.1\nENVEOF"}, input: "hostBridge", err: `hostBridge contains a control character: "127.0.0.1\nENVEOF"`},
		{name: "a listen host", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0"}},
		{name: "a listen host with a control character", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0\n"}, input: "listenHost", err: `listenHost contains a control character: "0.0.0.0\n"`},
		// A seed is refused before the control-character refusal that would quote it.
		{name: "a listen host holding a seed", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: readmeSeed + "\n"}, input: "listenHost", err: "listenHost holds an nkey seed (not echoed)"},
		{
			name:  "a listen host holding a seed, lowercase and split across DNS labels",
			in:    core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: strings.ToLower(readmeSeed[:29] + "." + readmeSeed[29:])},
			input: "listenHost", err: "listenHost holds an nkey seed (not echoed)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.Check()
			env, envErr := c.in.Env([]string{"nats://127.0.0.1:4222"})
			if c.err != "" {
				requireInputError(t, err, c.input, c.err)
				require.NotContains(t, strings.ToUpper(err.Error()), readmeSeed[:12], "the refusal quotes no part of the seed")
				// Env runs Check itself, so a caller that skips Check still gets its refusal.
				require.Nil(t, env)
				requireInputError(t, envErr, c.input, c.err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, envErr)
		})
	}
}

// Env runs Check, then renders the environment, and refuses the URLs itself.
func TestListenerConfigEnv(t *testing.T) {
	cases := []struct {
		name string
		in   core.ListenerConfig
		urls []string
		want []string
		err  string // a refusal of natsUrls
	}{
		{
			name: "remote NATS, host bridge, the default port",
			in:   core.ListenerConfig{MachineID: "example-host-a", Port: core.DefaultListenerPort, HostBridge: "127.0.0.1"},
			urls: []string{"nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=example-host-a", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"},
		},
		{
			name: "loopback NATS needs no remote opt-in",
			in:   core.ListenerConfig{MachineID: "m", Port: 9021},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9021", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "any remote URL in the list opts in",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020},
			urls: []string{"nats://localhost:4222", "nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://localhost:4222,nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{
			name: "a listen host, right after the port",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0", HostBridge: "127.0.0.1"},
			urls: []string{"nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"},
		},
		{name: "no URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, err: "natsUrls needs at least one URL"},
		{name: "a comma in a URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://a:1,nats://b:2"}, err: "natsUrls[0] contains a comma"},
		{name: "an empty URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{" "}, err: "natsUrls[0] is empty"},
		{name: "a refused URL is named by its index", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://127.0.0.1:4222", ""}, err: "natsUrls[1] is empty"},
		{name: "a URL with a newline", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{heredocEscape}, err: "natsUrls[0] contains a control character"},
		{name: "a URL with any control character, by its index", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://127.0.0.1:4222", "nats://hub.internal.example:4222\r"}, err: "natsUrls[1] contains a control character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Env(c.urls)
			if c.err != "" {
				requireInputError(t, err, "natsUrls", c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}
