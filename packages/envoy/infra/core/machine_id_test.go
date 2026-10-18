package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// CheckMachineID refuses the ids a listener name or hub grant entry cannot carry, and any over 246 bytes: an
// interior ordinary space is accepted because both names replace it with a hyphen; every character
// class either name cannot carry is refused without echoing an unsafe character.
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
