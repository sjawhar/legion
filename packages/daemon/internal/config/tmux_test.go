package config

import (
	"path/filepath"
	"reflect"
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
