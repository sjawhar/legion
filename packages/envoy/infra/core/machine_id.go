package core

import (
	"strings"
	"unicode"
)

// maxMachineIDBytes keeps "listener-"+id within 255 bytes.
const maxMachineIDBytes = 246

const machineIDNameRefusal = ", which the listener's NATS names or the hub's grant entries cannot carry"

// CheckMachineID refuses a machine id that cannot safely name a listener's NATS identities or
// ride in a hub grant entry or the watchdog's files. It also caps the id so that the durable
// listener-<id> fits NATS's 255-byte consumer-name limit. The refusal text states exactly this.
// bus.ListenerDurable and bus.ListenerQueue replace each ordinary space with a hyphen and produce
// listener-<id> and envoy-listener-<id>. nats-server refuses a durable name holding a dot, wildcard,
// path separator or whitespace. The rule is stricter than NATS where a hub grant entry
// (core/nats_auth.go) or the watchdog's heredoc and environment file could not carry the
// character: a control character or whitespace other than an ordinary space. The listener trims ENVOY_MACHINE_ID while loading it,
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
	if len(id) > maxMachineIDBytes {
		return InputErrorf(input, "%s is %d bytes, over the %d the durable name listener-<id> allows within NATS's 255-byte consumer-name limit", input, len(id), maxMachineIDBytes)
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
