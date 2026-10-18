package config

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestWorkerBackendsDefaultEveryRoleToOmp(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		if got := cfg.BackendFor(role); got != runtime.BackendOmp {
			t.Errorf("BackendFor(%s) = %q, want omp", role, got)
		}
	}
}

func TestWorkerBackendsChooseClaudePerRole(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"worker_backends: {implementer: claude, planner: omp}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.BackendFor(claim.RoleImplementer); got != runtime.BackendClaude {
		t.Errorf("BackendFor(implementer) = %q, want claude", got)
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleTester, claim.RoleArchitect} {
		if got := cfg.BackendFor(role); got != runtime.BackendOmp {
			t.Errorf("BackendFor(%s) = %q, want omp", role, got)
		}
	}
}

func TestWorkerBackendsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{"a list", "worker_backends: [implementer]",
			"worker_backends must be a mapping of a phase worker's role to its backend (omp or claude), e.g. {implementer: claude}"},
		{"the architect", "worker_backends: {architect: claude}",
			"worker_backends names architect, which runs only on omp: an architect is woken by notices on its Envoy role topic, and Claude Code's print mode delivers no channel notification"},
		{"the controller", "worker_backends: {controller: claude}",
			`worker_backends names "controller", which is not a phase worker's role (planner, implementer, tester, reviewer, merger)`},
		{"an unknown role", "worker_backends: {coder: claude}",
			`worker_backends names "coder", which is not a phase worker's role (planner, implementer, tester, reviewer, merger)`},
		{"an unknown backend", "worker_backends: {implementer: codex}",
			`worker_backends.implementer must be omp or claude (got "codex")`},
		{"a backend that is not a scalar", "worker_backends: {implementer: [claude]}",
			"worker_backends.implementer must be omp or claude"},
		{"a role named twice", "worker_backends: {implementer: claude, implementer: omp}",
			"worker_backends names implementer twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.line+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWorkerBackendsRefuseClaudeUnderKubernetes(t *testing.T) {
	body := kubernetesWith("", "") + "worker_backends: {implementer: claude}\n"
	_, err := LoadForValidation(writeConfigFile(t, body), noEnv)
	want := "worker_backends.implementer is claude, which runs only under runtime: tmux: the worker image carries no Claude Code"
	if err == nil || err.Error() != want {
		t.Fatalf("Load error = %v, want %q", err, want)
	}
}
