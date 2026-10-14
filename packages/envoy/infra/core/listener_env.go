// Package core holds the logic Envoy's infrastructure components share that touches no cloud.
package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
)

// defaultListenerPort is the port the listener serves on when PORT is unset
// (cmd/listener/main.go: config.Load(9020)).
const defaultListenerPort = 9020

// EnvNATSSeed names the variable holding the nkey seed of the NATS user the listener connects as.
// Its value is a secret, so ListenerEnv never renders it: the component appends it.
const EnvNATSSeed = "NATS_NKEY_SEED"

// ListenerConfig is what a listener's environment is built from.
type ListenerConfig struct {
	MachineID  string   // ENVOY_MACHINE_ID
	NATSURLs   []string // NATS_URLS, joined with commas
	Port       int      // 0 means 9020
	HostBridge string   // "" leaves ENVOY_HOST_BRIDGE unset
}

// CheckPlain refuses what ListenerEnv would refuse of the fields a component knows when it is
// constructed: every field but NATSURLs, which may come from another resource or stack and so be
// known only later. A component calls it before it registers anything.
//
// The listener requires ENVOY_MACHINE_ID and refuses it blank (internal/config/config.go), and
// reads ENVOY_HOST_BRIDGE untrimmed into every local delivery's URL (internal/session/session.go),
// so a host bridge with surrounding whitespace is refused. A Port of 0 means 9020.
func (c ListenerConfig) CheckPlain() error {
	if strings.TrimSpace(c.MachineID) == "" {
		return errors.New("machineId is required")
	}
	if port := c.port(); port < 1 || port > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", c.Port)
	}
	if strings.TrimSpace(c.HostBridge) != c.HostBridge {
		return fmt.Errorf("hostBridge has leading or trailing whitespace: %q", c.HostBridge)
	}
	return nil
}

// port is the port the listener serves on.
func (c ListenerConfig) port() int {
	if c.Port == 0 {
		return defaultListenerPort
	}
	return c.Port
}

// ListenerEnv returns the listener's non-secret environment, KEY=value, in a fixed order.
//
// It refuses what CheckPlain refuses, then each of NATSURLs: the listener refuses NATS_URLS blank
// (internal/config/config.go) and splits it on commas, so a URL holding a comma or nothing is
// refused here, by its index. ENVOY_ALLOW_REMOTE_NATS=1 is set exactly when a URL names a server
// bus.LocalNATSURL does not vouch for, since the listener refuses such a server without it.
func ListenerEnv(c ListenerConfig) ([]string, error) {
	if err := c.CheckPlain(); err != nil {
		return nil, err
	}
	if len(c.NATSURLs) == 0 {
		return nil, errors.New("natsUrls needs at least one URL")
	}
	remote := false
	for i, u := range c.NATSURLs {
		if strings.TrimSpace(u) == "" {
			return nil, fmt.Errorf("natsUrls[%d] is empty", i)
		}
		if strings.Contains(u, ",") {
			return nil, fmt.Errorf("natsUrls[%d] contains a comma, which separates URLs in NATS_URLS", i)
		}
		if !bus.LocalNATSURL(u) {
			remote = true
		}
	}
	env := []string{
		"PORT=" + strconv.Itoa(c.port()),
		"ENVOY_MACHINE_ID=" + c.MachineID,
		"NATS_URLS=" + strings.Join(c.NATSURLs, ","),
	}
	if remote {
		env = append(env, bus.AllowRemoteEnvVar+"=1")
	}
	if c.HostBridge != "" {
		env = append(env, "ENVOY_HOST_BRIDGE="+c.HostBridge)
	}
	return env, nil
}
