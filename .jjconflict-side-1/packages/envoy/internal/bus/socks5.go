package bus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// SOCKS5ProxyEnvVar names the variable that sends every bus NATS connection through a SOCKS5
// proxy, as host:port. Unset, the bus dials each server directly. Set, the proxy is handed each
// server's host name as the URL spells it, never resolved here, so a proxy that knows names this
// process's resolver does not (tailscaled's SOCKS5 server and MagicDNS) connects to them.
const SOCKS5ProxyEnvVar = "ENVOY_NATS_SOCKS5_PROXY"

// CheckSOCKS5Proxy refuses a proxy address that is not host:port: one that holds an nkey seed
// (HoldsSeed), whitespace around it, a scheme, an IPv6 address with a zone (every proxy a
// deployment names is loopback, and a zone is free text that could carry anything), a host that is
// neither an IP address nor a DNS name (dnsName), or a port that is not a number from 1 to 65535.
// Its error says what is wrong with the value and never prints it, since an operator can write a
// proxy with credentials in it and a refusal reaches the process's log; its caller names the
// setting. An address it admits is printed: a connect through it that fails names it, so an
// operator can debug the dial, which is why a seed, a valid DNS label, is refused here.
func CheckSOCKS5Proxy(value string) error {
	if value == "" {
		return errors.New("is empty; want host:port")
	}
	if HoldsSeed(value) {
		return errors.New("holds an nkey seed, which is never a proxy address")
	}
	if strings.TrimSpace(value) != value {
		return errors.New("has whitespace around it; want host:port")
	}
	if strings.Contains(value, "://") {
		return errors.New("is a URL; want host:port, with no scheme")
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return errors.New("is not host:port (an IPv6 host goes in brackets)")
	}
	if host == "" {
		return errors.New("names no host; want host:port")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return errors.New("names an IPv6 address with a zone; want host:port with no zone")
		}
	} else if !dnsName(host) {
		return errors.New("names a host that is neither an IP address nor a DNS name; want host:port")
	}
	if port == "" || strings.Trim(port, "0123456789") != "" {
		return errors.New("has no port number; want host:port")
	}
	if number, err := strconv.ParseUint(port, 10, 64); err != nil || number == 0 || number > 65535 {
		return errors.New("has a port out of range 1-65535")
	}
	return nil
}

// dnsName reports whether host is a syntactic DNS name as Go's resolver reads one: at most 253
// bytes, one trailing dot allowed, of labels of 1 to 63 letters, digits, hyphens and underscores
// (a Compose service name can hold one).
func dnsName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, b := range []byte(label) {
			if !('a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '-' || b == '_') {
				return false
			}
		}
	}
	return true
}

// socks5Dialer is the nats.CustomDialer a proxied connect installs. Each Dial is bounded by
// timeout: the bus's connect timeout, or what remains of a connect's deadline when that is shorter
// (bounded), since nats.go applies a connect's own timeout only to its default dialer. It records
// the last error a Dial returned, which connectWithContext names when nats.go reports only
// ErrNoServers (nats.go discards "connection refused"). The record is shared by every copy bounded
// makes and is safe for concurrent use: nats.go dials from its own reconnect goroutine
// (nats.go@v1.50.0:3038, doReconnect → createConn), and a client's background recovery and a
// publish's replacement dial can connect at once, each storing while the other loads.
type socks5Dialer struct {
	address string              // the proxy's host:port, for the error that names it
	proxy   proxy.ContextDialer // golang.org/x/net/proxy.SOCKS5 over a net.Dialer
	timeout time.Duration
	lastErr *atomic.Pointer[error]
}

// bounded is d with its timeout cut to remaining when that is shorter, sharing d's record.
func (d *socks5Dialer) bounded(remaining time.Duration) *socks5Dialer {
	if remaining >= d.timeout {
		return d
	}
	return &socks5Dialer{address: d.address, proxy: d.proxy, timeout: remaining, lastErr: d.lastErr}
}

// Dial connects through the proxy and records the error it returns.
func (d *socks5Dialer) Dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	conn, err := d.proxy.DialContext(ctx, network, address)
	d.lastErr.Store(&err)
	return conn, err
}

// LastError is the error the latest Dial returned, or nil.
func (d *socks5Dialer) LastError() error {
	if err := d.lastErr.Load(); err != nil {
		return *err
	}
	return nil
}

// Address is the proxy's host:port.
func (d *socks5Dialer) Address() string {
	return d.address
}

// socks5Proxy reads SOCKS5ProxyEnvVar through lookup. Unset is (nil, nil): a direct dial. Set, the
// value must be host:port (CheckSOCKS5Proxy); anything else is an error naming the variable, before
// anything dials. The *socks5Dialer it returns is the one value connectWithContext takes as its
// dialer, so the connect reads the address and the last error from it and never reads the lookup
// itself.
func socks5Proxy(lookup func(string) (string, bool)) (*socks5Dialer, error) {
	address, set := lookup(SOCKS5ProxyEnvVar)
	if !set {
		return nil, nil
	}
	if err := CheckSOCKS5Proxy(address); err != nil {
		return nil, fmt.Errorf("%s %w", SOCKS5ProxyEnvVar, err)
	}
	// proxy.SOCKS5 hands a host that is not an IP address to the proxy as a domain name, unresolved
	// (AddrTypeFQDN), and its Dialer bounds the handshake by the context Dial passes (ContextDialer).
	dialer, err := proxy.SOCKS5("tcp", address, nil, &net.Dialer{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SOCKS5ProxyEnvVar, err)
	}
	return &socks5Dialer{address: address, proxy: dialer.(proxy.ContextDialer), timeout: connectTimeout, lastErr: new(atomic.Pointer[error])}, nil
}
