package core

import (
	"strings"
	"unicode"
)

const machineIDNameRefusal = ", which cannot appear in the durable name listener-<id> or the queue group envoy-listener-<id>"

// CheckMachineID refuses a machine id that cannot name both of a listener's NATS identities.
// bus.ListenerDurable and bus.ListenerQueue replace each ordinary space with a hyphen and produce
// listener-<id> and envoy-listener-<id>. nats.go's checkConsumerName refuses an empty consumer name
// and a name containing a dot or space; the generated durable contains no space. NATS subjects and
// queue groups additionally cannot carry a wildcard, slash, backslash, control character, or
// whitespace other than an ordinary space. The listener trims ENVOY_MACHINE_ID while loading it,
// so leading or trailing whitespace is refused rather than allowed to change either generated
// name. Each refusal is an InputError naming input. Only a printable delimiter is quoted; control
// and whitespace characters are described without being echoed.
func CheckMachineID(input, id string) error {
	if id == "" {
		return InputErrorf(input, "%s is empty", input)
	}
	if strings.TrimSpace(id) != id {
		return InputErrorf(input, "%s has leading or trailing whitespace", input)
	}
	for _, r := range id {
		switch r {
		case '.', '*', '>', '/', '\\':
			return InputErrorf(input, "%s contains %q%s", input, r, machineIDNameRefusal)
		}
		if unicode.IsControl(r) {
			return InputErrorf(input, "%s contains a control character%s", input, machineIDNameRefusal)
		}
		if r != ' ' && unicode.IsSpace(r) {
			return InputErrorf(input, "%s contains whitespace other than a space%s", input, machineIDNameRefusal)
		}
	}
	return nil
}
