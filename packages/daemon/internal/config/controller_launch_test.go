package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Who launches the project's controller is the deployment's choice: the operator, with `legion
// controller start` on a machine of theirs, unless the file says `controller: daemon`, so a
// deployment that runs the operator's controller elsewhere keeps working with no edit.
func TestTheOperatorLaunchesTheControllerUnlessTheFileSaysTheDaemonDoes(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.ControllerLaunch != ControllerLaunchOperator {
		t.Fatalf("ControllerLaunch = %q with no controller key, want %q", cfg.ControllerLaunch, ControllerLaunchOperator)
	}
	for _, value := range []ControllerLaunch{ControllerLaunchOperator, ControllerLaunchDaemon} {
		cfg, err := LoadForValidation(writeConfigFile(t, "controller: "+string(value)+"\n"+kubernetesFile), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation with controller: %s: %v", value, err)
		}
		if cfg.ControllerLaunch != value {
			t.Errorf("ControllerLaunch = %q, want %q", cfg.ControllerLaunch, value)
		}
	}
}

// The daemon launches its controller as an Agent Sandbox pod, so `controller: daemon` is refused
// where there is no cluster, naming why, and so is a value that is neither launcher.
func TestTheControllerKeyIsRefusedWhereTheDaemonCannotLaunchOne(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"neither launcher", "controller: pod\n" + kubernetesFile, "controller must be 'operator' or 'daemon'"},
		{"blank", "controller: \"\"\n" + kubernetesFile, "controller must be 'operator' or 'daemon'"},
		{"under tmux", "controller: daemon\n" + minimalFile, "controller: daemon needs runtime: kubernetes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// The controller's pod is sized as a workflow role's pod is, by its own key under
// runtime.kubernetes.resources, which only a daemon that launches the controller reads: under
// `controller: operator` the key would size nothing, so it is refused, naming why — and only the
// key the file wrote is, never the default every settled block holds for the controller.
func TestTheControllersPodIsSizedOnlyWhereTheDaemonLaunchesIt(t *testing.T) {
	const sized = "    resources: {controller: {cpu: 2, memory: 8Gi}}\n"
	cfg, err := LoadForValidation(writeConfigFile(t, "controller: daemon\n"+kubernetesFile+sized), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation with the controller's resources under controller: daemon: %v", err)
	}
	want := RoleResources{CPU: "2", Memory: "8Gi"}
	if got := cfg.Runtime.Kubernetes.Resources[claim.RoleController]; got != want {
		t.Errorf("the controller's resources = %+v, want %+v", got, want)
	}
	for _, launch := range []string{"", "controller: operator\n"} {
		cfg, err := LoadForValidation(writeConfigFile(t, launch+kubernetesFile), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation with %q and no resources key: %v", launch, err)
		}
		if got, want := cfg.Runtime.Kubernetes.Resources[claim.RoleController], DefaultResources()[claim.RoleController]; got != want {
			t.Errorf("with %q the controller's default reservation = %+v, want %+v", launch, got, want)
		}
	}
	for name, body := range map[string]string{
		"no controller key":    kubernetesFile + sized,
		"controller: operator": "controller: operator\n" + kubernetesFile + sized,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, body), noEnv)
			const want = "runtime.kubernetes.resources.controller sizes the pod of the controller the daemon launches, and controller: operator launches none"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("LoadForValidation = %v, want a refusal naming %q", err, want)
			}
		})
	}
}
