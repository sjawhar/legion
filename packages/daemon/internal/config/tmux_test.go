package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The mapping form of the tmux runtime, `runtime: {tmux: {...}}`, settles what every pane is told
// beyond the runtime's own: its plain variables as written, and its Oh My Pi overlays each resolved
// against the file's directory, in file order, each once. A `_FILE` pointer is a plain variable: it
// names a file, never holds a secret.
func TestTheTmuxBlockSettlesPaneVariablesAndOverlays(t *testing.T) {
	path := writeConfigFile(t, minimalFile+`runtime:
  tmux:
    pane_env:
      ELAN_HOME: /opt/elan
      DISPATCH_PUBLIC_URL: https://dispatch.example
      LEDGER_DSN_FILE: /etc/legion-ledger/dsn
    pi_config_files: [overlay.yml, /etc/legion-operator/project.yml, overlay.yml]
`)
	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Runtime.Name != "tmux" || cfg.Runtime.Kubernetes != nil {
		t.Fatalf("Runtime = %+v, want tmux with no kubernetes block", cfg.Runtime)
	}
	want := &Tmux{
		PaneEnv:       map[string]string{"ELAN_HOME": "/opt/elan", "DISPATCH_PUBLIC_URL": "https://dispatch.example", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn"},
		PiConfigFiles: []string{filepath.Join(filepath.Dir(path), "overlay.yml"), "/etc/legion-operator/project.yml"},
	}
	if !reflect.DeepEqual(cfg.Runtime.Tmux, want) {
		t.Fatalf("Runtime.Tmux = %+v, want %+v", cfg.Runtime.Tmux, want)
	}
}

// The scalar `runtime: tmux` and a file naming no runtime settle no block; an empty block is the
// tmux runtime with nothing added.
func TestTheTmuxRuntimeWithoutABlockAddsNothing(t *testing.T) {
	for name, body := range map[string]string{
		"no runtime key":  minimalFile,
		"the tmux scalar": minimalFile + "runtime: tmux\n",
	} {
		cfg, err := LoadForValidation(writeConfigFile(t, body), noEnv)
		if err != nil {
			t.Fatalf("%s: LoadForValidation: %v", name, err)
		}
		if cfg.Runtime.Name != "tmux" || cfg.Runtime.Tmux != nil {
			t.Errorf("%s: Runtime = %+v, want tmux with no block", name, cfg.Runtime)
		}
	}
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"runtime: {tmux: }\n"), noEnv)
	if err != nil {
		t.Fatalf("an empty tmux block: LoadForValidation: %v", err)
	}
	if cfg.Runtime.Name != "tmux" || cfg.Runtime.Tmux == nil || len(cfg.Runtime.Tmux.PaneEnv) != 0 || len(cfg.Runtime.Tmux.PiConfigFiles) != 0 {
		t.Errorf("an empty tmux block: Runtime = %+v, want tmux with an empty block", cfg.Runtime)
	}
}

// `runtime.tmux.take_over: kubernetes` is the operator's choice to take over the claims a daemon
// running the project under kubernetes left in its store; without the key the block takes over
// nothing.
func TestTheTmuxBlockTakesOverKubernetesOnlyWhenTheKeySaysSo(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"runtime: {tmux: {take_over: kubernetes}}\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Runtime.Tmux == nil || cfg.Runtime.Tmux.TakeOver != TakeOverKubernetes {
		t.Fatalf("Runtime.Tmux = %+v, want take_over kubernetes", cfg.Runtime.Tmux)
	}
	cfg, err = LoadForValidation(writeConfigFile(t, minimalFile+"runtime: {tmux: {pane_env: {ELAN_HOME: /opt/elan}}}\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Runtime.Tmux == nil || cfg.Runtime.Tmux.TakeOver != "" {
		t.Fatalf("Runtime.Tmux = %+v, want no take_over", cfg.Runtime.Tmux)
	}
}

// A take_over naming anything but kubernetes is refused at load, naming the key and the value; so
// is a pane variable that places the session a resume reads, one the runtime sets on a Claude Code
// pane, a launch secret's pointer, and a value that is a URL carrying a password — each named with
// why, never with its value.
func TestTheTmuxBlockRefusesWhatTheRuntimeOwnsOrWhatHoldsAPassword(t *testing.T) {
	const password = "hunter2-load"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"take_over naming another runtime", "runtime: {tmux: {take_over: tmux}}\n",
			`runtime.tmux.take_over is "tmux": the one runtime whose claims the tmux runtime takes over is kubernetes`},
		{"take_over not a string", "runtime: {tmux: {take_over: [kubernetes]}}\n", "runtime.tmux.take_over must be a string"},
		{"Oh My Pi's session store", "runtime: {tmux: {pane_env: {OMP_SESSION_STORAGE: sql}}}\n",
			"runtime.tmux.pane_env names OMP_SESSION_STORAGE: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"Oh My Pi's session database", "runtime: {tmux: {pane_env: {OMP_SESSION_SQL_DSN_FILE: /etc/acme/dsn}}}\n",
			"runtime.tmux.pane_env names OMP_SESSION_SQL_DSN_FILE: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"Oh My Pi's agent directory", "runtime: {tmux: {pane_env: {PI_CODING_AGENT_DIR: /srv/agent}}}\n",
			"runtime.tmux.pane_env names PI_CODING_AGENT_DIR: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"Claude Code's directory", "runtime: {tmux: {pane_env: {CLAUDE_CONFIG_DIR: /srv/claude}}}\n",
			"runtime.tmux.pane_env names CLAUDE_CONFIG_DIR: the runtime sets it on every Claude Code pane itself, after the operator's variables"},
		{"the Envoy bearer's pointer", "runtime: {tmux: {pane_env: {ENVOY_TOKEN_FILE: /etc/acme/envoy}}}\n",
			"runtime.tmux.pane_env names ENVOY_TOKEN_FILE: it points at the launch secret ENVOY_TOKEN, which the runtime hands every launch as a file of the launch's own"},
		{"a URL with a password", "runtime: {tmux: {pane_env: {WIDGETS_DATABASE_URL: \"postgres://acme:" + password + "@db.internal.example/widgets\"}}}\n",
			"runtime.tmux.pane_env names WIDGETS_DATABASE_URL: its value is a URL carrying a password: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("the refusal %q holds the value's password", err)
			}
		})
	}
}
