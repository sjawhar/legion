package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// CheckMachineID refuses the ids a hub grant entry or the watchdog's files cannot carry, the
// characters nats-server refuses in a durable name, and any id over 246 bytes: an interior ordinary
// space is accepted because both generated names replace it with a hyphen; a refused character is
// never echoed unless it is a printable delimiter.
func TestCheckMachineID(t *testing.T) {
	const carry = ", which the listener's NATS names or the hub's grant entries cannot carry"
	for _, row := range []struct {
		name, id, want string
	}{
		{name: "letters, digits, hyphens and underscores", id: "example-host_1"},
		{name: "an interior ordinary space", id: "example host 1"},
		{name: "empty", want: "users[0].machineId is empty"},
		{name: "only ordinary spaces", id: "   ", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "leading ordinary space", id: " example-host", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "trailing ordinary space", id: "example-host ", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "leading non-breaking space", id: "\u00a0example-host", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "a dot", id: "example.host", want: "users[0].machineId contains '.'" + carry},
		{name: "a star", id: "example*host", want: "users[0].machineId contains '*'" + carry},
		{name: "a full wildcard", id: "example>host", want: "users[0].machineId contains '>'" + carry},
		{name: "a slash", id: "example/host", want: "users[0].machineId contains '/'" + carry},
		{name: "a backslash", id: `example\host`, want: `users[0].machineId contains '\\'` + carry},
		{name: "an interior tab", id: "example\thost", want: "users[0].machineId contains a control character" + carry},
		{name: "an interior newline", id: "example\nhost", want: "users[0].machineId contains a control character" + carry},
		{name: "an interior non-breaking space", id: "example\u00a0host", want: "users[0].machineId contains whitespace other than a space" + carry},
		{name: "the longest id the durable name allows", id: strings.Repeat("a", 246)},
		{name: "an id one byte too long", id: strings.Repeat("a", 247), want: "users[0].machineId is 247 bytes, over the 246 the durable name listener-<id> allows within NATS's 255-byte consumer-name limit"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := core.CheckMachineID("users[0].machineId", row.id)
			if row.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "users[0].machineId", refused.Input)
		})
	}
}
