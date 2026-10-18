package config

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// WorkerBackend is the agent a phase worker's role runs on (`worker_backends`).
type WorkerBackend string

const (
	// WorkerBackendOmp is Oh My Pi, `omp --mode rpc` behind `legion worker-shim`: every role's
	// backend unless the file names another.
	WorkerBackendOmp WorkerBackend = "omp"
	// WorkerBackendClaude is Claude Code, `claude -p` in stream-json behind `legion claude-bridge`,
	// which `legion worker-shim` runs in Oh My Pi's place.
	WorkerBackendClaude WorkerBackend = "claude"
)

// backendRoles are the roles `worker_backends` may name: the phase workers. The architect is
// refused: it is woken by notices on its Envoy role topic, which reach a Claude Code session only as
// channel notifications, and `claude -p` delivers none (measured on Claude Code 2.1.296). The
// controller is not a phase worker and has no issue.
var backendRoles = []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}

// BackendFor is the backend role runs on: the one `worker_backends` names, else Oh My Pi.
func (c Config) BackendFor(role claim.Role) WorkerBackend {
	if backend, ok := c.WorkerBackends[role]; ok {
		return backend
	}
	return WorkerBackendOmp
}

// readWorkerBackends is `worker_backends`: a mapping of a phase worker's role to its backend, `omp`
// or `claude`. A role named twice, a role that is not a phase worker's, and a backend that is not
// one of the two are each refused naming the entry.
func readWorkerBackends(value *yaml.Node, key string) (map[claim.Role]WorkerBackend, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a phase worker's role to its backend (omp or claude), e.g. {implementer: claude}", key)
	}
	backends := map[claim.Role]WorkerBackend{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, backendNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		switch {
		case role == claim.RoleArchitect:
			return nil, fmt.Errorf("%s names architect, which runs only on omp: an architect is woken by notices on its Envoy role topic, and Claude Code's print mode delivers no channel notification", key)
		case !slices.Contains(backendRoles, role):
			return nil, fmt.Errorf("%s names %q, which is not a phase worker's role (planner, implementer, tester, reviewer, merger)", key, roleNode.Value)
		}
		if _, seen := backends[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		var backend string
		if backendNode.Kind != yaml.ScalarNode || backendNode.Decode(&backend) != nil {
			return nil, fmt.Errorf("%s.%s must be omp or claude", key, role)
		}
		switch WorkerBackend(backend) {
		case WorkerBackendOmp, WorkerBackendClaude:
			backends[role] = WorkerBackend(backend)
		default:
			return nil, fmt.Errorf("%s.%s must be omp or claude (got %q)", key, role, backend)
		}
	}
	return backends, nil
}

// checkWorkerBackends refuses a Claude Code role under runtime kubernetes: the worker image carries
// no Claude Code and a pod's role launch runs Oh My Pi alone.
func checkWorkerBackends(cfg Config) error {
	if cfg.Runtime.Name != "kubernetes" {
		return nil
	}
	for _, role := range backendRoles {
		if cfg.BackendFor(role) == WorkerBackendClaude {
			return fmt.Errorf("worker_backends.%s is claude, which runs only under runtime: tmux: the worker image carries no Claude Code", role)
		}
	}
	return nil
}
