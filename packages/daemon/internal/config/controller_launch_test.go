package config

import (
	"strings"
	"testing"
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
