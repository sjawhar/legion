package bus

import (
	"context"
	"errors"
	"fmt"
	"net"
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

// CheckSOCKS5Proxy refuses a proxy address that is not host:port: one with a scheme, no host, or
// no port number. Its error names the value and not the setting, which its caller names.
func CheckSOCKS5Proxy(value string) error {
	if value == "" {
		return errors.New("empty; want host:port")
	}
	if strings.Contains(value, "://") {
		return fmt.Errorf("%q is a URL; want host:port, with no scheme", value)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", value, err)
	}
	if host == "" {
		return fmt.Errorf("%q names no host; want host:port", value)
	}
	if number, err := strconv.ParseUint(port, 10, 16); err != nil || number == 0 {
		return fmt.Errorf("%q has no port number; want host:port", value)
	}
	return nil
}

// socks5Dialer is the nats.CustomDialer a proxied connect installs. Each Dial is bounded by
// timeout, the bus's connect timeout, since nats.go applies its own only to its default dialer.
// It records the last error a Dial returned, for the message connectWithContext builds when
// nats.go reports only ErrNoServers (nats.go discards "connection refused"). The record is safe
// for concurrent use: nats.go dials from its own reconnect goroutine (nats.go@v1.50.0:3038,
// doReconnect → createConn) while a later recovery dial on the bus's goroutine reads it.
type socks5Dialer struct {
	address string              // the proxy's host:port, for the error that names it
	proxy   proxy.ContextDialer // golang.org/x/net/proxy.SOCKS5 over a net.Dialer
	timeout time.Duration
	lastErr atomic.Pointer[error]
}

// Dial connects through the proxy and records the error it returns. Pointer receiver: the
// record must land on the one dialer every connect of this Client installs, not on a copy.
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
		return nil, fmt.Errorf("%s: %w", SOCKS5ProxyEnvVar, err)
	}
	// proxy.SOCKS5 hands a host that is not an IP address to the proxy as a domain name, unresolved
	// (AddrTypeFQDN), and its Dialer bounds the handshake by the context Dial passes (ContextDialer).
	dialer, err := proxy.SOCKS5("tcp", address, nil, &net.Dialer{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SOCKS5ProxyEnvVar, err)
	}
	return &socks5Dialer{address: address, proxy: dialer.(proxy.ContextDialer), timeout: connectTimeout}, nil
}
