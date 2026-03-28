package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Service struct {
	MachineID string
	NATSURLs  []string
	Port      int
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
	return Service{MachineID: machine, NATSURLs: urls, Port: port}, nil
}
