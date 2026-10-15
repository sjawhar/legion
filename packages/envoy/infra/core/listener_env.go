// Package core holds the logic Envoy's infrastructure components share that touches no cloud.
package core

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/bus"
)

// DefaultListenerPort is the port the listener serves on when PORT is unset (cmd/listener/main.go,
// `config.Load(9020)`), and so a component's default for its port.
const DefaultListenerPort = 9020

// EnvNATSSeed names the variable holding the nkey seed of the NATS user the listener connects as,
// the one its bus client reads the seed from. Its value is a secret, so Env never renders it: the
// component appends it.
const EnvNATSSeed = bus.NkeySeedEnvVar

// ListenerConfig is the part of a listener's environment a component knows when it is constructed.
// NATS_URLS is not in it: the URLs may come from another resource or stack and so be known only
// later. A component checks the config once, before it registers anything, and Env adds the URLs
// when they are known.
type ListenerConfig struct {
	MachineID  string // ENVOY_MACHINE_ID
	Port       int    // PORT, 1-65535: a component resolves its default, DefaultListenerPort
	ListenHost string // "" leaves ENVOY_LISTEN_HOST unset, so the listener binds 127.0.0.1
	HostBridge string // "" leaves ENVOY_HOST_BRIDGE unset
}

// Check refuses a config the listener cannot run on. Each refusal is an InputError.
//
// The listener requires ENVOY_MACHINE_ID and refuses it blank (internal/config/config.go), and
// reads ENVOY_HOST_BRIDGE untrimmed into every local delivery's URL (internal/session/session.go),
// so a host bridge with surrounding whitespace is refused. Port is always given, since a health
// check calls it. No value may hold a control character: a watchdog writes the environment, one
// variable per line, into a heredoc of a script that runs as root on the host, where a line break
// would end the heredoc and run what follows it. Nor may one hold an nkey seed (bus.HoldsSeed),
// which the environment would carry, in the container's inputs and the watchdog's env file; that
// refusal names the field and never quotes the value, and comes before any refusal that quotes it.
func (c ListenerConfig) Check() error {
	if strings.TrimSpace(c.MachineID) == "" {
		return InputErrorf("machineId", "machineId is required")
	}
	if bus.HoldsSeed(c.MachineID) {
		return InputErrorf("machineId", "%w", SeedRefusal("machineId"))
	}
	if strings.ContainsFunc(c.MachineID, unicode.IsControl) {
		return InputErrorf("machineId", "machineId contains a control character: %q", c.MachineID)
	}
	if c.Port < 1 || c.Port > 65535 {
		return InputErrorf("port", "port %d is outside 1-65535", c.Port)
	}
	if bus.HoldsSeed(c.HostBridge) {
		return InputErrorf("hostBridge", "%w", SeedRefusal("hostBridge"))
	}
	if strings.TrimSpace(c.HostBridge) != c.HostBridge {
		return InputErrorf("hostBridge", "hostBridge has leading or trailing whitespace: %q", c.HostBridge)
	}
	if strings.ContainsFunc(c.HostBridge, unicode.IsControl) {
		return InputErrorf("hostBridge", "hostBridge contains a control character: %q", c.HostBridge)
	}
	if bus.HoldsSeed(c.ListenHost) {
		return InputErrorf("listenHost", "%w", SeedRefusal("listenHost"))
	}
	if strings.ContainsFunc(c.ListenHost, unicode.IsControl) {
		return InputErrorf("listenHost", "listenHost contains a control character: %q", c.ListenHost)
	}
	return nil
}

// Env returns the listener's non-secret environment, KEY=value, in a fixed order, for the config
// and the NATS servers natsURLs. It runs Check first, so no caller renders a config Check refuses;
// a component still runs Check before it registers anything, since Env runs only once the URLs
// are known.
//
// It then checks natsURLs: the listener refuses NATS_URLS blank (internal/config/config.go) and
// splits it on commas, so a URL holding a comma or nothing is refused here, by its index, and so is
// one holding a control character, for Check's reason, or an nkey seed anywhere in it, as a
// password in its userinfo would be. Each refusal is an InputError naming natsUrls, and none quotes
// the URL. ENVOY_ALLOW_REMOTE_NATS=1 is set exactly when a URL names a server bus.LocalNATSURL
// does not vouch for, since the listener refuses such a server without it.
func (c ListenerConfig) Env(natsURLs []string) ([]string, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	if len(natsURLs) == 0 {
		return nil, InputErrorf("natsUrls", "natsUrls needs at least one URL")
	}
	remote := false
	for i, u := range natsURLs {
		if bus.HoldsSeed(u) {
			return nil, InputErrorf("natsUrls", "%w", SeedRefusal(fmt.Sprintf("natsUrls[%d]", i)))
		}
		if strings.TrimSpace(u) == "" {
			return nil, InputErrorf("natsUrls", "natsUrls[%d] is empty", i)
		}
		if strings.Contains(u, ",") {
			return nil, InputErrorf("natsUrls", "natsUrls[%d] contains a comma, which separates URLs in NATS_URLS", i)
		}
		if strings.ContainsFunc(u, unicode.IsControl) {
			return nil, InputErrorf("natsUrls", "natsUrls[%d] contains a control character", i)
		}
		if !bus.LocalNATSURL(u) {
			remote = true
		}
	}
	env := []string{"PORT=" + strconv.Itoa(c.Port)}
	if c.ListenHost != "" {
		env = append(env, "ENVOY_LISTEN_HOST="+c.ListenHost)
	}
	env = append(env,
		"ENVOY_MACHINE_ID="+c.MachineID,
		"NATS_URLS="+strings.Join(natsURLs, ","),
	)
	if remote {
		env = append(env, bus.AllowRemoteEnvVar+"=1")
	}
	if c.HostBridge != "" {
		env = append(env, "ENVOY_HOST_BRIDGE="+c.HostBridge)
	}
	return env, nil
}
