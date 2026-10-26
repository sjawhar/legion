package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Tmux is the settled `runtime.tmux` block, the mapping form of `runtime: tmux`: what every pane
// of the tmux runtime is told beyond what the runtime tells it itself. Nil when the file says
// `runtime: tmux`, names no runtime, or names kubernetes.
type Tmux struct {
	// PaneEnv is `runtime.tmux.pane_env`: plain variables added to every pane's environment, by
	// name, for what a host's tools read and the daemon's allow-listed environment does not carry
	// (a toolchain's home, a cache directory, a public dashboard URL). A name the runtime sets
	// itself is refused when the runtime is built (tmux.New), and a credential-shaped one here: a
	// secret reaches a pane as a file, never as a value on tmux's command line.
	PaneEnv map[string]string
	// PiConfigFiles is `runtime.tmux.pi_config_files`: Oh My Pi settings overlays every pane names
	// in PI_CONFIG_FILES, in file order, before the turn-scoping overlay the runtime writes, so the
	// later of them outranks the earlier and the turn-scoping overlay's two keys outrank them all. A
	// relative path is resolved against the file's directory.
	PiConfigFiles []string
}

// readTmux reads the `runtime.tmux` block: `pane_env`, a mapping of variable to plain value, and
// `pi_config_files`, a list of overlay paths, each optional. A path holding the list separator `:`
// is refused, since PI_CONFIG_FILES could not name it.
func readTmux(value *yaml.Node) (*Tmux, error) {
	block := &Tmux{}
	if value.Tag == "!!null" {
		return block, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, errors.New("runtime.tmux must be a mapping")
	}
	fields, err := members(value, "runtime.tmux", "pane_env", "pi_config_files")
	if err != nil {
		return nil, err
	}
	if node, ok := fields["pane_env"]; ok {
		if block.PaneEnv, err = readPaneEnv(node, "runtime.tmux.pane_env"); err != nil {
			return nil, err
		}
	}
	if node, ok := fields["pi_config_files"]; ok {
		if block.PiConfigFiles, err = readStrings(node, "runtime.tmux.pi_config_files"); err != nil {
			return nil, err
		}
		for _, path := range block.PiConfigFiles {
			if strings.Contains(path, ":") {
				return nil, fmt.Errorf("runtime.tmux.pi_config_files entry %q holds `:`, which separates the overlays PI_CONFIG_FILES names", path)
			}
		}
	}
	return block, nil
}

// readPaneEnv is `runtime.tmux.pane_env`: each name an environment variable's, PATH refused (the
// runtime composes a pane's PATH itself), a credential-shaped name refused, and each value a plain
// scalar.
func readPaneEnv(value *yaml.Node, key string) (map[string]string, error) {
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of variable name to value", key)
	}
	env := map[string]string{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		nameNode, valueNode := value.Content[i], value.Content[i+1]
		name := nameNode.Value
		switch {
		case nameNode.Kind != yaml.ScalarNode || !envVarName.MatchString(name):
			return nil, fmt.Errorf("%s key %q %s", key, name, envNameRule)
		case name == "PATH":
			return nil, fmt.Errorf("%s names PATH, which the runtime composes for every pane (the daemon's own PATH behind its launcher directory)", key)
		case runtime.HoldsSecretValue(name):
			return nil, fmt.Errorf("%s names %s, a credential-shaped name: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line", key, name)
		}
		if _, seen := env[name]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		var read string
		if valueNode.Kind != yaml.ScalarNode || valueNode.Tag == "!!null" || valueNode.Decode(&read) != nil {
			return nil, fmt.Errorf("%s.%s must be a string", key, name)
		}
		env[name] = read
	}
	return env, nil
}

// resolveTmux settles the file's `runtime.tmux` block: each overlay path resolved against the
// file's directory, in file order, each once.
func resolveTmux(block *Tmux, configDir string) *Tmux {
	if block == nil {
		return nil
	}
	settled := &Tmux{PaneEnv: block.PaneEnv}
	for _, path := range block.PiConfigFiles {
		if resolved := underConfig(path, configDir); !slices.Contains(settled.PiConfigFiles, resolved) {
			settled.PiConfigFiles = append(settled.PiConfigFiles, resolved)
		}
	}
	return settled
}
