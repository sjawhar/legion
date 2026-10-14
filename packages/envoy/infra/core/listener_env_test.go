package core_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// requireInputError requires err to be a core.InputError refusing input, whose message holds msg.
func requireInputError(t *testing.T, err error, input, msg string) {
	t.Helper()
	require.ErrorContains(t, err, msg)
	var refused *core.InputError
	require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
	require.Equal(t, input, refused.Input)
}

func TestListenerEnv(t *testing.T) {
	cases := []struct {
		name  string
		in    core.ListenerConfig
		want  []string
		input string // the input a refusal names
		err   string
	}{
		{
			name: "remote NATS, host bridge, default port",
			in:   core.ListenerConfig{MachineID: "example-host-a", NATSURLs: []string{"nats://hub.internal.example:4222"}, HostBridge: "127.0.0.1"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=example-host-a", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"},
		},
		{
			name: "loopback NATS needs no remote opt-in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9021},
			want: []string{"PORT=9021", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "any remote URL in the list opts in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://localhost:4222", "nats://hub.internal.example:4222"}},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://localhost:4222,nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{name: "machine id required", in: core.ListenerConfig{NATSURLs: []string{"nats://127.0.0.1:4222"}}, input: "machineId", err: "machineId is required"},
		{name: "a blank machine id is refused", in: core.ListenerConfig{MachineID: " ", NATSURLs: []string{"nats://127.0.0.1:4222"}}, input: "machineId", err: "machineId is required"},
		{name: "a NATS URL required", in: core.ListenerConfig{MachineID: "m"}, input: "natsUrls", err: "natsUrls needs at least one URL"},
		{name: "a comma in a URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://a:1,nats://b:2"}}, input: "natsUrls", err: "natsUrls[0] contains a comma"},
		{name: "an empty URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{" "}}, input: "natsUrls", err: "natsUrls[0] is empty"},
		{name: "a refused URL is named by its index", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222", ""}}, input: "natsUrls", err: "natsUrls[1] is empty"},
		{name: "a negative port is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: -1}, input: "port", err: "port -1 is outside 1-65535"},
		{name: "a port above 65535 is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 65536}, input: "port", err: "port 65536 is outside 1-65535"},
		{name: "a blank host bridge is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, HostBridge: " "}, input: "hostBridge", err: "hostBridge has leading or trailing whitespace"},
		{name: "a host bridge with trailing whitespace is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, HostBridge: "127.0.0.1 "}, input: "hostBridge", err: "hostBridge has leading or trailing whitespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := core.ListenerEnv(c.in)
			if c.err != "" {
				requireInputError(t, err, c.input, c.err)
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
		name  string
		in    core.ListenerConfig
		input string // the input a refusal names
		err   string
	}{
		{name: "no natsUrls yet", in: core.ListenerConfig{MachineID: "m", HostBridge: "127.0.0.1"}},
		{name: "the default port", in: core.ListenerConfig{MachineID: "m"}},
		{name: "a blank machine id", in: core.ListenerConfig{MachineID: " "}, input: "machineId", err: "machineId is required"},
		{name: "a port above 65535", in: core.ListenerConfig{MachineID: "m", Port: 65536}, input: "port", err: "port 65536 is outside 1-65535"},
		{name: "a negative port", in: core.ListenerConfig{MachineID: "m", Port: -1}, input: "port", err: "port -1 is outside 1-65535"},
		{name: "a host bridge with leading whitespace", in: core.ListenerConfig{MachineID: "m", HostBridge: " 127.0.0.1"}, input: "hostBridge", err: `hostBridge has leading or trailing whitespace: " 127.0.0.1"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.CheckPlain()
			if c.err != "" {
				requireInputError(t, err, c.input, c.err)
				return
			}
			require.NoError(t, err)
		})
	}
}
