// packages/envoy/internal/broker/airlockproxy/address.go
// Where the proxy may dial. A command's CONNECT names the host, so without this check the proxy
// is an open relay into the cluster the broker runs in: the pod's own services, the node's
// metadata server, the broker's own API and its database all answer a dial from this process,
// and a tunnel would carry whatever a command asked for straight to them. Every dial this
// package makes - a tunnel's and a bound host's alike - goes through refuseLocalAddress first.
package airlockproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// clusterRange is 100.64.0.0/10, the shared address space of RFC 6598. Kubernetes clusters and
// cloud networks hand node and pod addresses out of it, and netip's own predicates do not name
// it, so it is spelled out here.
var clusterRange = netip.MustParsePrefix("100.64.0.0/10")

// resolveHost is the default of Server.resolve: the addresses a host name has right now, or the
// address a host that is an address literal already is.
func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
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

// refuseLocalAddress answers why the proxy will not dial host, and nil when it may. A name that
// resolves to several addresses is refused when ANY of them is one the proxy never dials:
// answering the request would otherwise depend on which address the dialer happened to pick,
// and a name under the caller's control can answer with one public address beside one private
// one precisely to make that choice.
//
// The check is on the addresses the name has at this moment. A name that answers one address
// here and another to the dial below it - DNS rebinding - is not covered by this check alone;
// what the proxy puts into such a request is nothing, since a bound host's rules are read from
// the name rather than from the address, and a tunnel carries only what the command already
// held.
func (s *Server) refuseLocalAddress(ctx context.Context, host string) error {
	addrs, err := s.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%s resolves to no address", host)
	}
	for _, addr := range addrs {
		if rule := localRule(addr); rule != "" {
			return fmt.Errorf("%s resolves to %s, %s, which this proxy never dials", host, addr, rule)
		}
	}
	return nil
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
