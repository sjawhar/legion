// Package samplerwire is what the daemon and Legion's resource sampler (INFRA-1208) say to each
// other: the port the sampler binds, the two files the launcher writes and the sampler reads, and
// the kind rules an operator configures, with the one validator of a rule.
//
// It is a leaf package, as internal/shimwire is for the shim, and for the same reason: the daemon
// configuration (internal/config), the pod builder (internal/runtime/sandbox) and the launcher
// render these shapes, while internal/sampler reads a node's pods through
// k8s.io/client-go/kubernetes. A package that imported the sampler for a struct tag built 132
// client-go packages with it (`go list -deps`), which is what this split takes back
// (dispatch://INFRA-1208, the review of pull request 2786).
//
// Nothing here reads a process, a file or a cluster: the sampler's own reading, its series and
// its exposition are internal/sampler's.
package samplerwire

// Port is the port the sampler binds: the issue pod's `roles` container declares it as `sampler`
// and node mode's DaemonSet serves on it too (docs/sampler-metrics.md, "The port"). The bind
// address is every address and not loopback, since the collector that scrapes it runs outside
// the pod.
//
// It is the one value that decides the port: the container's declared port, the launcher's
// command line for the sampler it starts and the sampler's own default all read it here, so no
// two of them can name different ports.
const Port = 9464

// Roles is the launcher's roles file: each role's role-init process id, which is the root of that
// role's process subtree. The sampler reads the file again on every pass, so a generation that
// starts or ends between two passes is picked up without a restart, and a role between
// generations holds a pid that names no process, which the walk passes over.
//
// The type is exported because the launcher writes this file and the daemon reads it back: both
// sides render the same shape, and this is it.
type Roles map[string]int

// Volume is one entry of the pod's volumes file: the volume as the pod spec declares it and the
// paths this container sees it mounted at. A volume several roles mount holds one entry with one
// path for each mount, and is measured once — the first path that can be read — since every mount
// of it is the same filesystem and a sum over them would report it as many times as it is
// mounted.
//
// Medium is `memory` or `disk` as the pod spec declares it and never what /proc/mounts shows,
// where a disk emptyDir is a tmpfs too. Role is the role that owns the volume, and empty for one
// no single role owns, which is served as `shared`.
type Volume struct {
	Name   string   `json:"name"`
	Paths  []string `json:"paths"`
	Medium string   `json:"medium"`
	Role   string   `json:"role"`
}
