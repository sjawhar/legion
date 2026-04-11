package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
)

// SessionLookup abstracts session port/machine resolution.
// Exactly one implementation is active at runtime — never both.
type SessionLookup interface {
	Get(sessionID string) (SessionEntry, error)
	Put(sessionID string, entry SessionEntry) error
	Delete(sessionID string) error
}

// RegistryMode selects the session registry implementation.
const (
	RegistryModeKV   = "kv"
	RegistryModeFile = "file"
)

// NewSessionLookup creates the appropriate SessionLookup based on mode.
//
//   - "kv" (default): KV-backed registry via NATS JetStream.
//   - "file": file-backed registry reading from dir.
//
// When KV is selected, dir is unused. When file is selected, conn is unused.
// Never both at once — eliminates the dual-registry disagreement.
func NewSessionLookup(mode string, conn *nats.Conn, dir string, machineID string, opts ...SessionRegistryOption) (SessionLookup, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RegistryModeFile:
		if dir == "" {
			return nil, fmt.Errorf("ENVOY_REGISTRY_DIR is required when ENVOY_SESSION_REGISTRY=file")
		}
		return &FileRegistry{Dir: dir, MachineID: machineID}, nil
	case RegistryModeKV, "":
		if conn == nil {
			return nil, fmt.Errorf("NATS connection is required when ENVOY_SESSION_REGISTRY=kv")
		}
		return OpenSessionRegistry(conn, opts...)
	default:
		return nil, fmt.Errorf("unknown ENVOY_SESSION_REGISTRY value: %q (expected \"kv\" or \"file\")", mode)
	}
}

// ParseRegistryMode reads ENVOY_SESSION_REGISTRY from the environment.
// Returns "kv" when unset or empty (the default).
func ParseRegistryMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("ENVOY_SESSION_REGISTRY")))
	if mode == "" {
		return RegistryModeKV
	}
	return mode
}
