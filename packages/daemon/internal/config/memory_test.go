package config

import (
	"reflect"
	"strings"
	"testing"
)

// memoryFile is minimalFile with block as DEMO's `memory` value and extra appended at the top
// level, so a case names only what it is about.
func memoryFile(block, extra string) string {
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {memory: "+block+"}\n", 1) + extra
}

// kubernetesMemoryFile is kubernetesFile with block as LEGSMOKE's `memory` value and the rolePod
// mounts, which every role's pod and the image probe carry.
func kubernetesMemoryFile(block string) string {
	return strings.Replace(kubernetesFile, "  LEGSMOKE: {}\n", "  LEGSMOKE: {memory: "+block+"}\n", 1) + rolePod
}

// projects.<KEY>.memory carries the mode, the overlays and the instructions path the file names,
// the overlays in file order; a key the block leaves out, and a project with no block at all, are
// the zero value, which is off.
func TestProjectMemoryCarriesTheModeTheOverlaysAndTheInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        Memory
		wantOff     bool
	}{
		{"all three keys", "{mode: on, config_files: [/etc/legion-operator/memory.yml, /etc/legion-operator/memory-extra.yml], instructions: /etc/legion-operator/memory.md}",
			Memory{
				Mode:             MemoryOn,
				ConfigFiles:      []string{"/etc/legion-operator/memory.yml", "/etc/legion-operator/memory-extra.yml"},
				InstructionsPath: "/etc/legion-operator/memory.md",
			}, false},
		{"no instructions", "{mode: random, config_files: [/etc/legion-operator/memory.yml]}",
			Memory{Mode: MemoryRandom, ConfigFiles: []string{"/etc/legion-operator/memory.yml"}}, false},
		{"no mode", "{config_files: [/etc/legion-operator/memory.yml], instructions: /etc/legion-operator/memory.md}",
			Memory{
				ConfigFiles:      []string{"/etc/legion-operator/memory.yml"},
				InstructionsPath: "/etc/legion-operator/memory.md",
			}, true},
		{"the mode alone", "{mode: off}", Memory{Mode: MemoryOff}, true},
		{"an empty block", "{}", Memory{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, memoryFile(tc.block, "")), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			memory := cfg.Projects["DEMO"].Memory
			if !reflect.DeepEqual(memory, tc.want) {
				t.Fatalf("Memory = %+v, want %+v", memory, tc.want)
			}
			if memory.Off() != tc.wantOff {
				t.Errorf("Memory.Off() = %v, want %v", memory.Off(), tc.wantOff)
			}
		})
	}

	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load without the key: %v", err)
	}
	memory := cfg.Projects["DEMO"].Memory
	if !reflect.DeepEqual(memory, Memory{}) || !memory.Off() {
		t.Errorf("Memory without the key = %+v (off %v), want the zero value, which is off", memory, memory.Off())
	}
}

// Every shape no tree could run is refused at load, naming the key: a block that is not a mapping,
// a mode that is not one of the three words, a list that is empty or not a list of strings, a path
// that is relative, unclean, holds PI_CONFIG_FILES' separator or repeats another, a mode other
// than off that names no overlay, and a mode other than off while an issue role runs on a harness
// that reads no overlay.
func TestProjectMemoryRefusals(t *testing.T) {
	for _, tc := range []struct{ name, block, extra, want string }{
		{"not a mapping", "[/etc/legion-operator/memory.yml]", "",
			"projects.DEMO.memory must be a mapping"},
		{"an unknown key", "{group: on}", "",
			"unknown key projects.DEMO.memory.group"},
		{"a mode that is no word of the three", "{mode: sometimes, config_files: [/etc/a.yml]}", "",
			`projects.DEMO.memory.mode must be off, on, or random (got "sometimes")`},
		{"a mode that is not a string", "{mode: [on]}", "",
			"projects.DEMO.memory.mode must be a string"},
		{"an empty list", "{mode: on, config_files: []}", "",
			"projects.DEMO.memory.config_files names no overlay: give the project at least one path, or leave it out"},
		{"a list of no strings", "{config_files: [1]}", "",
			"projects.DEMO.memory.config_files must be an array of non-empty strings"},
		{"a relative overlay", "{config_files: [memory.yml]}", "",
			`projects.DEMO.memory.config_files entry "memory.yml" must be a clean absolute path: a memory-group role's Oh My Pi reads it from wherever it starts`},
		{"an unclean overlay", "{config_files: [/etc/../etc/memory.yml]}", "",
			`projects.DEMO.memory.config_files entry "/etc/../etc/memory.yml" must be a clean absolute path: a memory-group role's Oh My Pi reads it from wherever it starts`},
		{"the separator", "{config_files: [/etc/a:b.yml]}", "",
			"projects.DEMO.memory.config_files entry \"/etc/a:b.yml\" holds `:`, which separates the overlays PI_CONFIG_FILES names"},
		{"a duplicate overlay", "{config_files: [/etc/a.yml, /etc/b.yml, /etc/a.yml]}", "",
			"projects.DEMO.memory.config_files names /etc/a.yml twice"},
		{"a relative instructions path", "{instructions: memory.md}", "",
			`projects.DEMO.memory.instructions "memory.md" must be a clean absolute path: the daemon reads it at boot from wherever it starts`},
		{"an unclean instructions path", "{instructions: /etc/../etc/memory.md}", "",
			`projects.DEMO.memory.instructions "/etc/../etc/memory.md" must be a clean absolute path: the daemon reads it at boot from wherever it starts`},
		{"an instructions path that is not a string", "{instructions: [/etc/memory.md]}", "",
			"projects.DEMO.memory.instructions must be a string"},
		{"a mode with no overlays", "{mode: on, instructions: /etc/legion-operator/memory.md}", "",
			"projects.DEMO.memory is on and names no config_files: a memory-group tree's roles read their memory overlays from that key, so name at least one, or set mode: off"},
		{"a role on claude", "{mode: random, config_files: [/etc/a.yml]}", "worker_backends: {implementer: claude}\n",
			"projects.DEMO.memory is random, and worker_backends.implementer is claude, which reads no PI_CONFIG_FILES: put every issue role on omp, or set mode: off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, memoryFile(tc.block, tc.extra)), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Under kubernetes a memory overlay must be a file one of the operator's mounts holds, since a
// role's pod and the image probe see only those and the image: a sub_path mount of the file, an
// item a directory mount of a ConfigMap projects, or any file under a whole Secret volume. A path
// no mount holds, a directory mount itself and a key the projection leaves out are refused at
// load, which is what `legion start --check-config` runs.
func TestProjectMemoryUnderKubernetesMustBeInTheOperatorsMounts(t *testing.T) {
	for _, tc := range []struct{ name, files, want string }{
		{"the operator's own sub_path mount", "[/etc/legion-operator/overlay.yml]", ""},
		{"an item the directory mount projects", "[/etc/legion-route/roles/fallback.yml]", ""},
		{"a file under a whole secret", "[/etc/legion-creds/anything.yml]", ""},
		{"a file no mount holds", "[/etc/legion-operator/memory.yml]",
			"projects.LEGSMOKE.memory.config_files entry /etc/legion-operator/memory.yml is under no runtime.kubernetes.pod.volume_mounts mount_path: a role's pod and the image probe's see only the image and the operator's mounts, so mount the ConfigMap that holds it there"},
		{"the directory mount itself", "[/etc/legion-route]",
			"projects.LEGSMOKE.memory.config_files entry /etc/legion-route is the mount_path of volume operator-route, which mounts as a directory: name a file in it, or mount the file with sub_path"},
		{"a key the projection leaves out", "[/etc/legion-route/memory.yml]",
			"projects.LEGSMOKE.memory.config_files entry /etc/legion-route/memory.yml is not among the items volume operator-route projects at /etc/legion-route (overlay.yml, roles/fallback.yml): add an item with path memory.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesMemoryFile("{mode: on, config_files: "+tc.files+"}")), noEnv)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadForValidation: %v", err)
			case tc.want == "" && len(cfg.Projects["LEGSMOKE"].Memory.ConfigFiles) != 1:
				t.Fatalf("Memory.ConfigFiles = %v, want the project's one file", cfg.Projects["LEGSMOKE"].Memory.ConfigFiles)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// `mode: off` with the two path keys is accepted and kept as written, so an operator moving a
// project to `on` moves one key rather than three. Nothing of an off block reaches a tree, so what
// the other refusals ask of a live block is not asked of this one: a role on Claude Code and an
// overlay outside every pod mount are both accepted while the mode is off.
func TestProjectMemoryOffKeepsThePathsItIsGiven(t *testing.T) {
	block := "{mode: off, config_files: [/etc/legion-operator/memory.yml], instructions: /etc/legion-operator/memory.md}"
	cfg, err := Load(writeConfigFile(t, memoryFile(block, "worker_backends: {implementer: claude}\n")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Memory{
		Mode:             MemoryOff,
		ConfigFiles:      []string{"/etc/legion-operator/memory.yml"},
		InstructionsPath: "/etc/legion-operator/memory.md",
	}
	memory := cfg.Projects["DEMO"].Memory
	if !reflect.DeepEqual(memory, want) || !memory.Off() {
		t.Fatalf("Memory = %+v (off %v), want %+v and off", memory, memory.Off(), want)
	}

	if _, err := LoadForValidation(writeConfigFile(t, kubernetesMemoryFile("{mode: off, config_files: [/etc/legion-operator/memory.yml]}")), noEnv); err != nil {
		t.Fatalf("LoadForValidation with an off block outside every mount: %v", err)
	}
}
