package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// MemoryMode is `projects.<KEY>.memory.mode`: what the daemon gives each tree the project opens.
// MemoryOff gives no tree a memory group, MemoryOn gives every new tree one, and MemoryRandom
// draws for each new tree.
type MemoryMode string

const (
	// MemoryOff is the mode of a project that names none: the field's zero value is the empty
	// string, which Memory.Off reads as off beside this word.
	MemoryOff    MemoryMode = "off"
	MemoryOn     MemoryMode = "on"
	MemoryRandom MemoryMode = "random"
)

// memoryModes are the words `mode` takes, in the order its refusal names them.
var memoryModes = []MemoryMode{MemoryOff, MemoryOn, MemoryRandom}

// Memory is a project's `memory` block, which varies by tree where `role_config_files` varies by
// role: Mode decides each new tree's memory group; ConfigFiles are the Oh My Pi settings overlays
// every role of a memory-group tree names in PI_CONFIG_FILES, after the operator's own and the
// role's own (role_config_files); and InstructionsPath is the one markdown file the daemon reads at
// boot and appends to every such role's system prompt, after the deployment's own instructions. A
// project whose file names no block is off, and an off project passes none of the three on.
type Memory struct {
	Mode             MemoryMode
	ConfigFiles      []string
	InstructionsPath string
}

// Word is the mode as a draw records it: one of the three words, never the empty string a project
// that names no block leaves, so every record of a draw reads back a mode an operator could have
// written and no reader of one needs a fourth name for the absent word.
func (memory Memory) Word() MemoryMode {
	if memory.Mode == "" {
		return MemoryOff
	}
	return memory.Mode
}

// readMemory is a project's `memory`: `mode`, one of the three words (absent is off);
// `config_files`, the absolute paths of the overlays a memory-group tree's roles name after their
// own; and `instructions`, the absolute path of the file the daemon reads. A path that is relative
// or unclean, that holds PI_CONFIG_FILES' separator `:`, or that the key names twice is refused
// naming it, as role_config_files refuses one (readRoleConfigFiles): a relative path would resolve
// against wherever the reader starts, which is no file of the operator's. `off` with either path
// key is taken as written, so an operator moving a project to `on` moves one key.
func readMemory(value *yaml.Node, key string) (Memory, error) {
	if value.Kind != yaml.MappingNode {
		return Memory{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "config_files", "instructions", "mode")
	if err != nil {
		return Memory{}, err
	}
	var memory Memory
	mode, err := stringOf(fields["mode"], key+".mode")
	if err != nil {
		return Memory{}, err
	}
	if mode != "" {
		if memory.Mode = MemoryMode(mode); !slices.Contains(memoryModes, memory.Mode) {
			return Memory{}, fmt.Errorf("%s.mode must be off, on, or random (got %q)", key, mode)
		}
	}
	if files := fields["config_files"]; files != nil {
		if memory.ConfigFiles, err = readMemoryConfigFiles(files, key+".config_files"); err != nil {
			return Memory{}, err
		}
	}
	if instructions := fields["instructions"]; instructions != nil {
		field := key + ".instructions"
		if memory.InstructionsPath, err = stringOf(instructions, field); err != nil {
			return Memory{}, err
		}
		if !filepath.IsAbs(memory.InstructionsPath) || filepath.Clean(memory.InstructionsPath) != memory.InstructionsPath {
			return Memory{}, fmt.Errorf("%s %q must be a clean absolute path: the daemon reads it at boot from wherever it starts", field, memory.InstructionsPath)
		}
	}
	return memory, nil
}

// readMemoryConfigFiles is a `memory` block's `config_files`: at least one clean absolute path,
// each named once, none holding PI_CONFIG_FILES' separator. An empty list is refused rather than
// taken as no overlay, since a block that wants none leaves the key out.
func readMemoryConfigFiles(value *yaml.Node, key string) ([]string, error) {
	files, err := readStrings(value, key)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s names no overlay: give the project at least one path, or leave it out", key)
	}
	for index, file := range files {
		switch {
		case !filepath.IsAbs(file) || filepath.Clean(file) != file:
			return nil, fmt.Errorf("%s entry %q must be a clean absolute path: a memory-group role's Oh My Pi reads it from wherever it starts", key, file)
		case strings.Contains(file, ":"):
			return nil, fmt.Errorf("%s entry %q holds `:`, which separates the overlays PI_CONFIG_FILES names", key, file)
		case slices.Contains(files[:index], file):
			return nil, fmt.Errorf("%s names %s twice", key, file)
		}
	}
	return files, nil
}

// checkMemory refuses what a project's `memory` block asks for that no tree could run as asked,
// once the rest of the file is settled: a mode other than off that names no overlay; a block that
// names overlays while worker_backends puts an issue role on a harness other than Oh My Pi, which
// reads no PI_CONFIG_FILES; and, under runtime kubernetes, an overlay outside every
// runtime.kubernetes.pod mount (checkRoleConfigMount), since a pod sees only the image and the
// operator's mounts, and a file the image ships is no operator's.
//
// Only the first is about the mode. The other two hold whenever the block names overlays, `off`
// included: the mode decides which new trees are drawn into the memory group, and a tree already
// drawn into it keeps its overlays while the block names them, so an overlay no role could load
// would stop such a tree's roles under any mode. Taking the files away is removing them from the
// block, not moving the mode.
func checkMemory(cfg Config) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Projects)) {
		memory := cfg.Projects[name].Memory
		field := "projects." + name + ".memory"
		if memory.Mode != "" && memory.Mode != MemoryOff && len(memory.ConfigFiles) == 0 {
			return fmt.Errorf("%s is %s and names no config_files: a memory-group tree's roles read their memory overlays from that key, so name at least one, or set mode: off", field, memory.Mode)
		}
		if len(memory.ConfigFiles) == 0 {
			continue
		}
		for _, role := range claim.Roles {
			if backend := cfg.BackendFor(role); backend != runtime.BackendOmp {
				return fmt.Errorf("%s names config_files, and worker_backends.%s is %s, which reads no PI_CONFIG_FILES: put every issue role on %s, or take the overlays out of the block", field, role, backend, runtime.BackendOmp)
			}
		}
		if k := cfg.Runtime.Kubernetes; k != nil {
			for _, file := range memory.ConfigFiles {
				if err := checkRoleConfigMount(k.Pod, field+".config_files", file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
