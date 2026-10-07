// Package controller is the project's controller as the daemon's record holds it: the capability
// it registered with — the one `legion controller start` fetched with the operator's bearer, or,
// under `controller: daemon`, the boot token of the daemon's own launch — and the session that
// registered with it, which `controllerLocator` reports. The operator's controller is no process
// of the daemon's, which has nothing of it to stop or resume; Prober reads that session's liveness
// from the Envoy role registry. The daemon's own controller is a claim it supervises
// (internal/daemon's controllerKeeper).
package controller

import "time"

// Record is the controller as the daemon's store holds it. Generation counts the capabilities
// `POST /legion/v1/controller/secret` has minted for the project; each mint replaces the hash and
// clears the registration, so a session registers with the capability of the current generation
// or not at all. Session, SecretHash, and RegisteredAt are empty until one does.
type Record struct {
	Generation     uint64
	CapabilityHash []byte
	Session        string
	SecretHash     []byte
	RegisteredAt   time.Time
}

// Registered says whether a session holds the current capability's registration.
func (r Record) Registered() bool { return r.Session != "" }
