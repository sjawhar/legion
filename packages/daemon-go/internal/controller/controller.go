// Package controller is the project's interactive controller as the daemon knows it: the
// capability `legion controller start` fetched with the operator's bearer, and the session that
// registered with it (LEGION-206 Requirement 11: "`legion controller start` on the operator's
// machine, its external record on `controllerLocator`"). The daemon never launches this process
// and has nothing of it to stop or resume: it holds the record, and Prober reads the session's
// liveness from the Envoy role registry.
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
