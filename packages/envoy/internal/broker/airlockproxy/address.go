// packages/envoy/internal/broker/airlockproxy/address.go
// Where the proxy may dial, and what it dials there. A command's CONNECT names the host, so
// without this check the proxy is an open relay into the cluster the broker runs in: the pod's
// own services, the node's metadata server, the broker's own API and its database all answer a
// dial from this process, and a tunnel would carry whatever a command asked for straight to
// them. Every dial this package makes - a tunnel's, a bound host's and a plain-HTTP request's
// alike - resolves the name once here, refuses the whole name when any one of its addresses is
// one the proxy never dials, and then dials those addresses rather than the name. So a name
// that answers one address to the check and another to the dial, which is DNS rebinding,
// reaches the address that was checked or nothing at all.
package airlockproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// clusterRange is 100.64.0.0/10, the shared address space of RFC 6598. Kubernetes clusters and
// cloud networks hand node and pod addresses out of it, and netip's own predicates do not name
// it, so it is spelled out here.
var clusterRange = netip.MustParsePrefix("100.64.0.0/10")

// ResolveHost is the default of Server.resolve: the addresses a host name has right now, or the
// address a host that is an address literal already is. It is exported because a caller that
// gives Options.Resolve a resolver of its own - cmd/broker, whose BROKER_AIRLOCK_TEST_RESOLVE
// answers for the hosts it names and nothing else - falls back to exactly this for every other
// host, and a second copy of it would be a second rule about where a name is.
func ResolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i, addr := range addrs {
		addrs[i] = addr.Unmap()
	}
	return addrs, nil
}

// checkedAddresses answers the addresses the proxy may dial for host at port, in the order a
// dial must try them, and why it may dial none of them. A name that resolves to several
// addresses is refused when ANY of them is one the proxy never dials: answering the request
// would otherwise depend on which address the dialer happened to pick, and a name under the
// caller's control can answer with one public address beside one private one precisely to make
// that choice.
//
// What it answers is what is dialled (dialAddrs), so the check and the dial are one reading of
// one name: nothing below resolves that name again, and a name that answers another address to
// a later lookup - DNS rebinding - never reaches this process's socket. The host name goes on
// being the TLS server name upstream, since that is the name a service's certificate is read
// against and the name the rules are read for.
//
// An address Options.DialAllow names is dialled whatever it resolves to, which is what lets one
// run reach a stand-in service on loopback while every other loopback address stays refused.
func (s *Server) checkedAddresses(ctx context.Context, host, port string) ([]netip.Addr, error) {
	addrs, err := s.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolves to no address", host)
	}
	if slices.Contains(s.dialAllow, net.JoinHostPort(host, port)) {
		return addrs, nil
	}
	for _, addr := range addrs {
		if rule := localRule(addr); rule != "" {
			return nil, fmt.Errorf("%s resolves to %s, %s, which this proxy never dials", host, addr, rule)
		}
	}
	return addrs, nil
}

// dialChecked is one dial of a host outside the broker, check and all: the addresses the name
// has now, the refusal of the whole name when any of them is one this proxy never dials, and a
// dial of those addresses. It is the DialContext of the transport every bound host's request and
// every plain-HTTP request goes out on, where a refusal and a failed dial are one answer to the
// caller. The two paths that must refuse a request before they take a command's connection over
// - a CONNECT and a plain-HTTP request - read checkedAddresses themselves, so that their refusal
// is a 403 rather than a 502, and hand what it answered to dialAddrs.
func (s *Server) dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := s.checkedAddresses(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return s.dialAddrs(ctx, network, addrs, port)
}

// dialAddrs dials the addresses in order and answers the first connection one of them gave, so
// that a name whose first address is down still reaches the service through its second, as a
// dialer resolving the name for itself would. It resolves nothing: these are the addresses
// checkedAddresses read and checked, and they are the addresses the socket is opened to.
func (s *Server) dialAddrs(ctx context.Context, network string, addrs []netip.Addr, port string) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no address to dial")
	}
	var errs []error
	for _, addr := range addrs {
		conn, err := s.dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// localRule names the rule that refuses addr, and "" for an address the proxy may dial. Each
// answer names the rule and the address family, because an operator reading a refusal has to
// know which of a name's addresses it was about: a name with one public IPv4 address and one
// link-local IPv6 address is refused, and the line says which half did it.
func localRule(addr netip.Addr) string {
	// A 4-in-6 address is the IPv4 address it holds, for every predicate below and for the
	// family each answer names.
	addr = addr.Unmap()
	family := "IPv6"
	if addr.Is4() {
		family = "IPv4"
	}
	switch {
	case addr.IsLoopback():
		// 127.0.0.0/8 and ::1: the broker's own pod.
		return "a loopback " + family + " address"
	case clusterRange.Contains(addr):
		// 100.64.0.0/10: the cluster's own nodes and pods.
		return "a cluster " + family + " address"
	case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10: the node's metadata server among them.
		return "a link-local " + family + " address"
	case addr.IsPrivate():
		// 10/8, 172.16/12 and 192.168/16; fc00::/7, which is unique-local rather than private.
		if family == "IPv6" {
			return "a unique-local IPv6 address"
		}
		return "a private IPv4 address"
	case !addr.IsGlobalUnicast():
		// Everything left that is not a host on the public internet: the unspecified address,
		// multicast, the IPv4 broadcast address, and the interface-local scopes.
		return "not a global unicast " + family + " address"
	}
	return ""
}
