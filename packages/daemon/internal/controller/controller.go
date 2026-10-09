// Package controller is the project's controller as the daemon's record holds it: the capability
// a session registers with — the one `legion controller start` fetched with the operator's bearer,
// or, under `controller: daemon`, one nobody holds, minted when a launch of the daemon's own
// controller registered with its boot token — and the session that registered, which
// `controllerLocator` reports. The operator's controller is no process of the daemon's, which has
// nothing of it to stop or resume; Prober reads that session's liveness from the Envoy role
// registry. The daemon's own controller is a claim it supervises (internal/daemon's
// controllerKeeper).
package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"time"
)

// Record is the controller as the daemon's store holds it. Generation counts the capabilities
// minted for the project: `POST /legion/v1/controller/secret`'s, and each UnheldCapability the
// daemon records; each mint replaces the hash and clears the registration, so a session registers
// with the capability of the current generation or not at all. Session, SecretHash, and
// RegisteredAt are empty until one does.
type Record struct {
	Generation     uint64
	CapabilityHash []byte
	Session        string
	SecretHash     []byte
	RegisteredAt   time.Time
}

// Registered says whether a session holds the current capability's registration.
func (r Record) Registered() bool { return r.Session != "" }

// UnheldCapability is a controller capability hash that no token matches: the SHA-256 of a fresh
// random secret, discarded on return, so no process, Secret or log ever holds what it hashes. The
// daemon records it whenever no session may register through the operator's capability path, which
// compares a registration's token with the record: when a launch of its own controller registers
// (`controller: daemon`), and when a daemon switched back to `controller: operator` stops that
// controller. The invariant both keep is that the record's capability is the secret `legion
// controller start` was handed or one nobody holds, never a launch's boot token: a restart forgets
// the tokens of launches since replaced, and a recorded one would then register through that path,
// outside its claim's generation fence.
func UnheldCapability() []byte {
	sum := sha256.Sum256([]byte(rand.Text()))
	return sum[:]
}
