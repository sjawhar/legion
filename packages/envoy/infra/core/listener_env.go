// Package core holds the logic Envoy's infrastructure components share that touches no cloud.
package core

import (
	"fmt"
	"net"
	"regexp"
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
	MachineID   string // ENVOY_MACHINE_ID
	Port        int    // PORT, 1-65535: a component resolves its default, DefaultListenerPort
	ListenHost  string // "" leaves ENVOY_LISTEN_HOST unset, so the listener binds 127.0.0.1
	HostBridge  string // "" leaves ENVOY_HOST_BRIDGE unset
	SOCKS5Proxy string // "" leaves ENVOY_NATS_SOCKS5_PROXY unset
	// AllowedHosts are the names a client may reach the listener's /v1 interface by,
	// ENVOY_ALLOWED_HOSTS. An empty list leaves the variable unset, and a listener that requires
	// no bearer then answers 421 to every request whose Host is not loopback
	// (cmd/listener/apiauth.go, servesHost); an entry is rendered as written, since the listener
	// normalizes what it reads.
	AllowedHosts []string
	// SubscribeAllTopics renders ENVOY_SUBSCRIBE_ALL_TOPICS=1, which has the listener also hold a
	// core subscription on every notification topic, so a session served on its machine receives a
	// topic the stream does not store. Only a NATS user whose grant allows `notifications.>`
	// (HostPermissions) may set it: the listener's start ends when NATS refuses that subscription,
	// as it does for ListenerPermissions and DispatchPermissions. The component cannot read the
	// user's grant, so the operator who knows it opts in; false renders nothing.
	SubscribeAllTopics bool
}

// TailnetHostName is the address a client reaches the listener of the tailnet machine hostname by,
// a tailnet's MagicDNS giving every machine a name under its tailnet's DNS name, and so the entry
// that machine's listener must serve for a client to be answered at all.
func TailnetHostName(hostname, tailnetDNSName string) string {
	return hostname + "." + tailnetDNSName
}

// hostEntry is an allowed host's shape: a name or an IPv4 address, or an IPv6 address with or
// without its brackets, and never a port, a scheme or a path. dnsName is a host name
// allowedHostName has normalized: dot-separated labels of letters, digits and inner hyphens.
// Both mirror packages/envoy/internal/config, whose parseAllowedHosts refuses at boot what Check
// refuses here, and which core may not import, since the plugin's release is cut from what
// `go list -deps ./cmd/...` names; core/allowed_hosts_agreement_test.go holds the two to one rule.
var (
	hostEntry = regexp.MustCompile(`^(\[[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*\]|[0-9A-Fa-f:.]+|[A-Za-z0-9.-]+)$`)
	dnsName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// allowedHostName is an entry as the listener compares it, config.HostName's normalization: the
// port stripped, the brackets taken off an IPv6 address, ASCII letters lower-cased, and one
// trailing dot dropped. Only ASCII is folded, as DNS folds it.
func allowedHostName(entry string) string {
	if name, _, err := net.SplitHostPort(entry); err == nil {
		entry = name
	} else if strings.HasPrefix(entry, "[") && strings.HasSuffix(entry, "]") {
		entry = entry[1 : len(entry)-1]
	}
	return strings.TrimSuffix(strings.Map(asciiLower, entry), ".")
}

func asciiLower(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}

// CheckAllowedHosts refuses every entry of hosts the listener would refuse in its own
// ENVOY_ALLOWED_HOSTS at boot, so a component that renders one is refused before it registers
// anything rather than deploying a listener that will not start. It is the one rule, and input is
// the caller's own path for the list — "allowedHosts" for a listener's config, and the
// component's spelling, such as "listener.allowedHosts", where the component renders entries of
// its own beside the operator's: each refusal names input and the entry's index within hosts, so
// an operator reads the index of the entry they wrote. A seed is refused first, naming the entry
// and never its value, since the refusal below quotes it.
//
// This mirrors parseAllowedHosts in packages/envoy/internal/config, by hand: core may not import
// that package, since a non-test import would put it in `go list -deps ./cmd/...`, the path list
// a plugin release is cut from (packages/envoy/infra/AGENTS.md, "Release contract"), so an edit
// to the listener's config would cut a plugin release. core/allowed_hosts_agreement_test.go runs
// the same entries through both rules and keeps the copy honest, the asymmetry included: the rule
// here runs one way, since parseAllowedHosts trims an entry and skips an empty one before it
// applies the shape rule, while this applies it to the entry as written. That extra strictness is
// deliberate — Env renders the list comma-joined into the unit's docker run argument, where a
// space would split the argument and an empty entry would render a stray comma.
func CheckAllowedHosts(input string, hosts []string) error {
	for i, entry := range hosts {
		if bus.HoldsSeed(entry) {
			return InputErrorf(input, "%w", SeedRefusal(fmt.Sprintf("%s[%d]", input, i)))
		}
		_, _, withPort := net.SplitHostPort(entry)
		name := allowedHostName(entry)
		if withPort == nil || !hostEntry.MatchString(entry) || (net.ParseIP(name) == nil && !dnsName.MatchString(name)) {
			return InputErrorf(input, "%s[%d] is %q: want a host name or address with no port, scheme or path", input, i, entry)
		}
	}
	return nil
}

// Check refuses a config the listener cannot run on. Each refusal is an InputError.
//
// CheckMachineID owns the machine-id rule and CheckAllowedHosts the allowed-host rule.
// ENVOY_HOST_BRIDGE is read untrimmed into every local
// delivery's URL (internal/session/session.go), so a host bridge with surrounding whitespace is
// refused. Port is always given, since a health check calls it. No value may hold a control
// character: a watchdog writes the environment, one variable per line, into a heredoc of a script
// that runs as root on the host, where a line break would end the heredoc and run what follows it.
// Nor may one hold an nkey seed (bus.HoldsSeed), which the environment would carry, in the
// container's inputs and the watchdog's env file; that refusal names the field and never quotes
// the value, and comes before any other refusal of that field.
func (c ListenerConfig) Check() error {
	if bus.HoldsSeed(c.MachineID) {
		return InputErrorf("machineId", "%w", SeedRefusal("machineId"))
	}
	if err := CheckMachineID("machineId", c.MachineID); err != nil {
		return err
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
	if err := CheckAllowedHosts("allowedHosts", c.AllowedHosts); err != nil {
		return err
	}
	if c.SOCKS5Proxy != "" {
		if err := bus.CheckSOCKS5Proxy(c.SOCKS5Proxy); err != nil {
			return InputErrorf("socks5Proxy", "%s %w", bus.SOCKS5ProxyEnvVar, err)
		}
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
// does not vouch for, since the listener refuses such a server without it. ENVOY_ALLOWED_HOSTS
// follows the ENVOY_LISTEN_HOST block, and only when the config lists a host, so a listener
// without one renders what it always rendered. ENVOY_SUBSCRIBE_ALL_TOPICS=1 comes last, and only
// when SubscribeAllTopics is set, for the same reason: every rendering made before the opt-in
// existed, the hub's cloud-config and Dispatch's container among them, stays byte for byte.
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
	if len(c.AllowedHosts) > 0 {
		env = append(env, "ENVOY_ALLOWED_HOSTS="+strings.Join(c.AllowedHosts, ","))
	}
	env = append(env,
		"ENVOY_MACHINE_ID="+c.MachineID,
		"NATS_URLS="+strings.Join(natsURLs, ","),
	)
	if remote {
		env = append(env, bus.AllowRemoteEnvVar+"=1")
	} else if c.SOCKS5Proxy != "" {
		return nil, InputErrorf("socks5Proxy", "socks5Proxy needs a remote natsUrls URL because %s is set only with %s=1",
			bus.SOCKS5ProxyEnvVar, bus.AllowRemoteEnvVar)
	}
	if c.SOCKS5Proxy != "" {
		env = append(env, bus.SOCKS5ProxyEnvVar+"="+c.SOCKS5Proxy)
	}
	if c.HostBridge != "" {
		env = append(env, "ENVOY_HOST_BRIDGE="+c.HostBridge)
	}
	if c.SubscribeAllTopics {
		env = append(env, "ENVOY_SUBSCRIBE_ALL_TOPICS=1")
	}
	return env, nil
}
