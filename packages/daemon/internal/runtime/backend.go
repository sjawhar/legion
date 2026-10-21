package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Backend is the agent harness a launch runs. Every harness runs behind `legion worker-shim`, which
// speaks Oh My Pi's RPC frames to it, so the worker stream and supervision are the same for all of
// them; what differs between them before a runtime launches one is its entry in Backends.
type Backend string

const (
	// BackendOmp is Oh My Pi, `omp --mode rpc` behind `legion worker-shim`. A spec whose Backend is
	// "" runs on it too (Of).
	BackendOmp Backend = "omp"
	// BackendClaude is Claude Code, `claude -p` in stream-json behind `legion claude-bridge`, which
	// `legion worker-shim` runs in Oh My Pi's place.
	BackendClaude Backend = "claude"
)

// BackendInfo is what the daemon knows of a harness before any runtime launches it.
type BackendInfo struct {
	// Name is the harness as an operator reads it in a refusal or a log line.
	Name string
	// Roles are the roles it may run; nil is every role. `worker_backends` (config) and every
	// launch (ValidateSpawnSpec) are held to it.
	Roles []claim.Role
	// Why is the reason it runs no other role, the rest of the refusal of one it does not run.
	Why string
	// Runtimes are the runtimes that launch it, by their configuration names (`runtime:`).
	Runtimes []string
	// PromptPart names a daemon prompt part (prompts.Composer.Part) a launch on it is told after its
	// role's parts; "" is none.
	PromptPart string
}

// Backends is every harness a launch can run on. Oh My Pi runs every role under either runtime.
// Claude Code runs the phase workers under tmux alone, since the worker image carries no Claude
// Code; it is told claude-mechanics.md, the role prompts' Oh My Pi tool names as Claude Code names
// them. A harness is added here, as a tmux Launcher (runtime/tmux), and as a boot resolver
// (daemon's launchers.go).
var Backends = map[Backend]BackendInfo{
	BackendOmp: {Name: "OMP", Runtimes: []string{"tmux", "kubernetes"}},
	BackendClaude: {
		Name:  "Claude Code",
		Roles: []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger},
		Why: "an architect is woken by notices on its Envoy role topic and the controller by its own, " +
			"and Claude Code's print mode delivers no channel notification",
		Runtimes:   []string{"tmux"},
		PromptPart: "claude-mechanics.md",
	},
}

// Of is the backend a spec runs on: backend, or BackendOmp for "".
func Of(backend Backend) Backend {
	if backend == "" {
		return BackendOmp
	}
	return backend
}

// BackendNames is every backend's name, sorted: what a refusal of an unknown one lists.
func BackendNames() []string {
	names := make([]string, 0, len(Backends))
	for backend := range Backends {
		names = append(names, string(backend))
	}
	slices.Sort(names)
	return names
}

// LookupBackend is the entry of the backend a spec runs on (Of), or an error naming every backend.
func LookupBackend(backend Backend) (BackendInfo, error) {
	info, ok := Backends[Of(backend)]
	if !ok {
		return BackendInfo{}, fmt.Errorf("backend %q is not one of %s", backend, strings.Join(BackendNames(), ", "))
	}
	return info, nil
}

// Runs is whether the backend may run role.
func (b BackendInfo) Runs(role claim.Role) bool {
	return b.Roles == nil || slices.Contains(b.Roles, role)
}

// LaunchedBy is whether the runtime the configuration names runtime launches the backend.
func (b BackendInfo) LaunchedBy(runtime string) bool {
	return slices.Contains(b.Runtimes, runtime)
}

// JoinRoles is roles, comma-separated, for a refusal.
func JoinRoles(roles []claim.Role) string {
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = string(role)
	}
	return strings.Join(names, ", ")
}
