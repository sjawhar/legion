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

// ListenerEnv returns the listener's non-secret environment, KEY=value, in a fixed order.
//
// The listener requires ENVOY_MACHINE_ID and NATS_URLS and refuses either blank
// (internal/config/config.go), and splits NATS_URLS on commas, so a URL holding a comma or nothing
// is refused here, by its index. ENVOY_ALLOW_REMOTE_NATS=1 is set exactly when a URL names a
// server bus.LocalNATSURL does not vouch for, since the listener refuses such a server without it.
func ListenerEnv(c ListenerConfig) ([]string, error) {
	if strings.TrimSpace(c.MachineID) == "" {
		return nil, errors.New("machineId is required")
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
	port := c.Port
	if port == 0 {
		port = defaultListenerPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is outside 1-65535", c.Port)
	}
	env := []string{
		"PORT=" + strconv.Itoa(port),
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
