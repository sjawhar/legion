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
		for runtime, file := range map[string]string{"kubernetes": kubernetesFile, "tmux": minimalFile} {
			cfg, err := LoadForValidation(writeConfigFile(t, "controller: "+string(value)+"\n"+file), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation with controller: %s under %s: %v", value, runtime, err)
			}
			if cfg.ControllerLaunch != value {
				t.Errorf("ControllerLaunch under %s = %q, want %q", runtime, cfg.ControllerLaunch, value)
			}
		}
	}
}

// The controller works the project's Dispatch issues — an Agent Sandbox pod's under kubernetes, a
// pane's under tmux — so `controller: daemon` is refused where the daemon has no Dispatch to hand
// it, naming why, and so is a value that is neither launcher.
func TestTheControllerKeyIsRefusedWhereTheDaemonCannotLaunchOne(t *testing.T) {
	const noDispatch = "project: DEMO\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"neither launcher", "controller: pod\n" + kubernetesFile, "controller must be 'operator' or 'daemon'"},
		{"blank", "controller: \"\"\n" + kubernetesFile, "controller must be 'operator' or 'daemon'"},
		{"under tmux with no Dispatch", "controller: daemon\n" + noDispatch, "controller: daemon needs dispatch_url: the controller the daemon launches works the project's Dispatch issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// The controller's pod is sized as an issue pod is, by its own key under
// runtime.kubernetes.resources, which only a daemon that launches the controller reads: under
// `controller: operator` the key would size nothing, so it is refused, naming why, an entry that
// sets no field included — and only the key the file wrote is, never the default every settled
// block holds for the controller.
func TestTheControllersPodIsSizedOnlyWhereTheDaemonLaunchesIt(t *testing.T) {
	const sized = "    resources: {controller: {cpu: 2, memory: 8Gi}}\n"
	cfg, err := LoadForValidation(writeConfigFile(t, "controller: daemon\n"+kubernetesFile+sized), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation with the controller's resources under controller: daemon: %v", err)
	}
	want := Reservation{CPU: "2", CPURequest: "2", Memory: "8Gi", MemoryRequest: "8Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"}
	if got := cfg.Runtime.Kubernetes.Resources.Controller; got != want {
		t.Errorf("the controller's resources = %+v, want %+v", got, want)
	}
	for _, launch := range []string{"", "controller: operator\n"} {
		cfg, err := LoadForValidation(writeConfigFile(t, launch+kubernetesFile), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation with %q and no resources key: %v", launch, err)
		}
		if got, want := cfg.Runtime.Kubernetes.Resources.Controller, DefaultResources().Controller; got != want {
			t.Errorf("with %q the controller's default reservation = %+v, want %+v", launch, got, want)
		}
	}
	for name, body := range map[string]string{
		"no controller key":        kubernetesFile + sized,
		"controller: operator":     "controller: operator\n" + kubernetesFile + sized,
		"an entry setting nothing": kubernetesFile + "    resources: {controller: {}}\n",
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
