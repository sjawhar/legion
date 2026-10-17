package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Service struct {
	MachineID    string
	NATSURLs     []string
	NATSReplicas int
	ListenHost   string
	Port         int
	// AllowedHosts are the ENVOY_ALLOWED_HOSTS names, each as HostName writes it, that an
	// unauthenticated listener serves /v1 to beside loopback.
	AllowedHosts []string
}

func (s Service) ListenAddress() string {
	return net.JoinHostPort(s.ListenHost, strconv.Itoa(s.Port))
}

func Load(defaultPort int) (Service, error) {
	machine := strings.TrimSpace(os.Getenv("ENVOY_MACHINE_ID"))
	if machine == "" {
		return Service{}, fmt.Errorf("ENVOY_MACHINE_ID is required")
	}
	raw := strings.TrimSpace(os.Getenv("NATS_URLS"))
	if raw == "" {
		return Service{}, fmt.Errorf("NATS_URLS is required")
	}
	urls := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' })
	for i, item := range urls {
		urls[i] = strings.TrimSpace(item)
	}
	port := defaultPort
	if value := strings.TrimSpace(os.Getenv("PORT")); value != "" {
		next, err := strconv.Atoi(value)
		if err != nil {
			return Service{}, fmt.Errorf("invalid PORT: %w", err)
		}
		port = next
	}
	listenHost := strings.TrimSpace(os.Getenv("ENVOY_LISTEN_HOST"))
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	replicas := 1
	if value := strings.TrimSpace(os.Getenv("ENVOY_NATS_REPLICAS")); value != "" {
		next, err := strconv.Atoi(value)
		if err != nil {
			return Service{}, fmt.Errorf("invalid ENVOY_NATS_REPLICAS: %w", err)
		}
		replicas = next
	}
	allowedHosts, err := parseAllowedHosts(os.Getenv("ENVOY_ALLOWED_HOSTS"))
	if err != nil {
		return Service{}, err
	}
	return Service{
		AllowedHosts: allowedHosts,
		MachineID:    machine,
		NATSURLs:     urls,
		NATSReplicas: replicas,
		ListenHost:   listenHost,
		Port:         port,
	}, nil
}

// hostEntry is an ENVOY_ALLOWED_HOSTS entry's shape: a name or an IPv4 address, or an IPv6
// address with or without its brackets, and never a port, a scheme or a path.
var hostEntry = regexp.MustCompile(`^(\[[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*\]|[0-9A-Fa-f:.]+|[A-Za-z0-9.-]+)$`)

// dnsName is a host name HostName has normalized: dot-separated labels of letters, digits and
// inner hyphens.
var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// parseAllowedHosts reads ENVOY_ALLOWED_HOSTS: comma-separated host names or addresses, blank
// entries skipped. An entry that is not a host name or an IP address (one carrying a port, a
// scheme, a path or a stray bracket) is refused naming it, since a Host is compared by its name
// alone.
func parseAllowedHosts(raw string) ([]string, error) {
	var hosts []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, _, withPort := net.SplitHostPort(entry)
		name := HostName(entry)
		if withPort == nil || !hostEntry.MatchString(entry) || (net.ParseIP(name) == nil && !dnsName.MatchString(name)) {
			return nil, fmt.Errorf("invalid ENVOY_ALLOWED_HOSTS entry %q: want a host name or address with no port, scheme or path", entry)
		}
		hosts = append(hosts, name)
	}
	return hosts, nil
}

// HostName is a Host header's or a host entry's name as the listener compares it: the port
// stripped, the brackets taken off an IPv6 address, ASCII letters lower-cased, and one trailing dot
// dropped, since DNS reads "name." as "name". Only ASCII is folded, as DNS folds it, so no
// non-ASCII letter (U+0130 lower-cases to "i") can spell an allowed name.
func HostName(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(strings.Map(asciiLower, host), ".")
}

func asciiLower(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}
