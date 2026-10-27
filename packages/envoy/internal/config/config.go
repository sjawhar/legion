package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
)

type Service struct {
	MachineID string
	NATSURLs  []string
	// NATSReplicas is ENVOY_NATS_REPLICAS (ParseNATSReplicas): the deployment's replica count for
	// the stream and every bucket the listener opens.
	NATSReplicas int
	// StreamMaxBytes is ENVOY_STREAM_MAX_BYTES (ParseStreamMaxBytes): the stream's byte limit, or 0
	// when unset, which sets none and keeps a deployed one.
	StreamMaxBytes int64
	ListenHost     string
	Port           int
	// AllowedHosts are the ENVOY_ALLOWED_HOSTS names, each as HostName writes it, that an
	// unauthenticated listener serves /v1 to beside loopback.
	AllowedHosts []string
	// SubscribeAllTopics is ENVOY_SUBSCRIBE_ALL_TOPICS: whether the listener also subscribes every
	// notifications topic over core NATS, which only a grant that allows `notifications.>` permits.
	SubscribeAllTopics bool
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
	replicas, err := ParseNATSReplicas(os.Getenv(bus.ReplicasEnvVar))
	if err != nil {
		return Service{}, err
	}
	streamMaxBytes, err := ParseStreamMaxBytes(os.Getenv(bus.StreamMaxBytesEnvVar))
	if err != nil {
		return Service{}, err
	}
	allowedHosts, err := parseAllowedHosts(os.Getenv("ENVOY_ALLOWED_HOSTS"))
	if err != nil {
		return Service{}, err
	}
	subscribeAllTopics, err := parseSubscribeAllTopics(os.Getenv("ENVOY_SUBSCRIBE_ALL_TOPICS"))
	if err != nil {
		return Service{}, err
	}
	return Service{
		AllowedHosts:       allowedHosts,
		MachineID:          machine,
		NATSURLs:           urls,
		NATSReplicas:       replicas,
		StreamMaxBytes:     streamMaxBytes,
		ListenHost:         listenHost,
		Port:               port,
		SubscribeAllTopics: subscribeAllTopics,
	}, nil
}

// MaxNATSReplicas is the most copies NATS keeps of a stream or a bucket. aws:WebhookListener's
// rendering (MaxNatsReplicas in github.com/sjawhar/envoy/infra/aws/rendering) refuses a larger
// natsReplicas by hand, so a component never deploys a listener this package would refuse.
const MaxNATSReplicas = 5

// ParseNATSReplicas reads ENVOY_NATS_REPLICAS (bus.ReplicasEnvVar), the deployment's replica
// count: unset or blank is 1, and otherwise a whole number from 1 to MaxNATSReplicas. Anything else
// (0, a negative number, more than NATS keeps, a word) is refused naming the variable and the
// value, at load rather than as a server's refusal at the stream. envoy-listener (Load) and
// envoy-dispatch read the setting through it.
func ParseNATSReplicas(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 1, nil
	}
	replicas, err := strconv.Atoi(value)
	if err != nil || replicas < 1 || replicas > MaxNATSReplicas {
		return 0, fmt.Errorf("invalid %s %q: want a whole number of copies from 1 to %d", bus.ReplicasEnvVar, value, MaxNATSReplicas)
	}
	return replicas, nil
}

// ParseStreamMaxBytes reads ENVOY_STREAM_MAX_BYTES (bus.StreamMaxBytesEnvVar), the stream's byte
// limit: unset or blank is 0, which sets no limit (a deployed stream keeps the one it has), and
// otherwise a positive whole number of bytes. 0, a negative number or a word is refused naming the
// variable and the value. envoy-listener (Load) and envoy-dispatch read the setting through it.
func ParseStreamMaxBytes(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("invalid %s %q: want a positive whole number of bytes, or unset for no limit", bus.StreamMaxBytesEnvVar, value)
	}
	return limit, nil
}

// parseSubscribeAllTopics reads ENVOY_SUBSCRIBE_ALL_TOPICS: unset, empty or "0" off, "1" on. Any
// other value, a padded "1" among them, is refused rather than read as off, so a typo such as
// "true" never leaves the opt-in silently off.
func parseSubscribeAllTopics(raw string) (bool, error) {
	switch raw {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("invalid ENVOY_SUBSCRIBE_ALL_TOPICS %q: want 1 to subscribe every notifications topic, or 0 or unset not to", raw)
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
//
// CheckAllowedHosts in github.com/sjawhar/envoy/infra/core carries this rule by hand on the
// plugin's side, so a component is refused before it deploys a listener that would not boot, and
// it is deliberately stricter: it applies the shape rule to the entry as written, so it refuses a
// padded or an empty entry this function trims or skips, because it renders the list comma-joined
// into the unit's docker run argument. Every entry refused here is refused there too, and
// packages/envoy/infra/core/allowed_hosts_agreement_test.go runs the same entries through both
// rules and pins that asymmetry.
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
