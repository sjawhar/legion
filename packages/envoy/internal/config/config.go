package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type Service struct {
	MachineID    string
	NATSURLs     []string
	NATSReplicas int
	ListenHost   string
	Port         int
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
	return Service{
		MachineID:    machine,
		NATSURLs:     urls,
		NATSReplicas: replicas,
		ListenHost:   listenHost,
		Port:         port,
	}, nil
}
