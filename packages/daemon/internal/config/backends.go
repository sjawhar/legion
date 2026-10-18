package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// BackendFor is the backend role runs on: the one `worker_backends` names, else Oh My Pi.
func (c Config) BackendFor(role claim.Role) runtime.Backend {
	if backend, ok := c.WorkerBackends[role]; ok {
		return backend
	}
	return runtime.BackendOmp
}

// Backends are the backends the daemon launches agents on, each once, in the order the workflow
// reaches the first role on it: every issue role's (BackendFor), then the controller's when the
// daemon launches it. Boot resolves each one's launch (readTmux).
func (c Config) Backends() []runtime.Backend {
	roles := slices.Clone(claim.Roles)
	if c.ControllerLaunch == ControllerLaunchDaemon {
		roles = append(roles, claim.RoleController)
	}
	var backends []runtime.Backend
	for _, role := range roles {
		if backend := c.BackendFor(role); !slices.Contains(backends, backend) {
			backends = append(backends, backend)
		}
	}
	return backends
}

// readWorkerBackends is `worker_backends`: a mapping of an issue role to the backend it runs on
// (runtime.Backends). A role named twice, a name that is not an issue role, a backend there is no
// entry for, and a backend that does not run the role are each refused naming the entry.
func readWorkerBackends(value *yaml.Node, key string) (map[claim.Role]runtime.Backend, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	names := strings.Join(runtime.BackendNames(), ", ")
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of an issue role to its backend (%s), e.g. {implementer: claude}", key, names)
	}
	backends := map[claim.Role]runtime.Backend{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, backendNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(claim.Roles, role) {
			return nil, fmt.Errorf("%s names %q, which is not an issue role (%s)", key, roleNode.Value, runtime.JoinRoles(claim.Roles))
		}
		if _, seen := backends[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		var name string
		if backendNode.Kind != yaml.ScalarNode || backendNode.Decode(&name) != nil {
			return nil, fmt.Errorf("%s.%s must be one of %s", key, role, names)
		}
		backend := runtime.Backend(name)
		info, ok := runtime.Backends[backend]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s.%s must be one of %s (got %q)", key, role, names, name)
		case !info.Runs(role):
			return nil, fmt.Errorf("%s.%s is %s (%s), which runs only %s: %s", key, role, backend, info.Name, runtime.JoinRoles(info.Roles), info.Why)
		}
		backends[role] = backend
	}
	return backends, nil
}

// checkWorkerBackends refuses a role on a backend the configured runtime does not launch
// (runtime.Backends): every pod runs the worker image's Oh My Pi.
func checkWorkerBackends(cfg Config) error {
	for _, role := range claim.Roles {
		backend := cfg.BackendFor(role)
		if info := runtime.Backends[backend]; !info.LaunchedBy(cfg.Runtime.Name) {
			return fmt.Errorf("worker_backends.%s is %s, which runs only under runtime: %s", role, backend, strings.Join(info.Runtimes, " or "))
		}
	}
	return nil
}
