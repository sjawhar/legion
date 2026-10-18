package config

import (
	"slices"
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
	cfg, err := Load(writeConfigFile(t, minimalFile+"worker_backends: {implementer: claude, planner: omp, architect: omp}\n"), noEnv)
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

// Backends is each backend the daemon launches on once, Oh My Pi first whenever any role runs on
// it, and only Oh My Pi when worker_backends is absent.
func TestBackendsAreTheBackendsInUse(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       []runtime.Backend
	}{
		{"no worker_backends", "", []runtime.Backend{runtime.BackendOmp}},
		{"one role on claude", "worker_backends: {implementer: claude}\n", []runtime.Backend{runtime.BackendOmp, runtime.BackendClaude}},
		{"every phase worker on claude", "worker_backends: {planner: claude, implementer: claude, tester: claude, reviewer: claude, merger: claude}\n",
			[]runtime.Backend{runtime.BackendOmp, runtime.BackendClaude}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile+tc.line), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Backends(); !slices.Equal(got, tc.want) {
				t.Errorf("Backends() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkerBackendsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{"a list", "worker_backends: [implementer]",
			"worker_backends must be a mapping of an issue role to its backend (claude, omp), e.g. {implementer: claude}"},
		{"the architect on claude", "worker_backends: {architect: claude}",
			"worker_backends.architect is claude (Claude Code), which runs only planner, implementer, tester, reviewer, merger: " +
				"an architect is woken by notices on its Envoy role topic and the controller by its own, and Claude Code's print mode delivers no channel notification"},
		{"the controller", "worker_backends: {controller: omp}",
			`worker_backends names "controller", which is not an issue role (architect, planner, implementer, tester, reviewer, merger)`},
		{"an unknown role", "worker_backends: {coder: claude}",
			`worker_backends names "coder", which is not an issue role (architect, planner, implementer, tester, reviewer, merger)`},
		{"an unknown backend", "worker_backends: {implementer: codex}",
			`worker_backends.implementer must be one of claude, omp (got "codex")`},
		{"a backend that is not a scalar", "worker_backends: {implementer: [claude]}",
			"worker_backends.implementer must be one of claude, omp"},
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
	want := "worker_backends.implementer is claude, which runs only under runtime: tmux"
	if err == nil || err.Error() != want {
		t.Fatalf("Load error = %v, want %q", err, want)
	}
}
