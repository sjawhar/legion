package config

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// role_config_files gives each role it names its own overlays, in file order, under either
// runtime, the daemon-launched controller among them; a file that names none leaves the map nil.
func TestRoleConfigFilesGiveEachRoleItsOwnOverlays(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"controller: daemon\nrole_config_files:\n"+
		"  implementer: [/etc/legion-operator/fallback.yml, /etc/legion-operator/implementer.yml]\n"+
		"  controller: [/etc/legion-operator/fallback.yml]\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[claim.Role][]string{
		claim.RoleImplementer: {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/implementer.yml"},
		claim.RoleController:  {"/etc/legion-operator/fallback.yml"},
	}
	if !reflect.DeepEqual(cfg.RoleConfigFiles, want) {
		t.Errorf("RoleConfigFiles = %v, want %v", cfg.RoleConfigFiles, want)
	}

	cfg, err = Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load without the key: %v", err)
	}
	if cfg.RoleConfigFiles != nil {
		t.Errorf("RoleConfigFiles without the key = %v, want nil", cfg.RoleConfigFiles)
	}
}

// Every shape no role could load is refused at load, naming the entry: a name that is not a Legion
// role, an empty list, a relative or unclean path, one holding PI_CONFIG_FILES' separator, a repeat,
// the controller's entry with no controller the daemon launches, a role on a harness that reads
// no overlay, and an overlay every pane already names.
func TestRoleConfigFilesRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not a mapping", "role_config_files: [/etc/a.yml]\n",
			"role_config_files must be a mapping of a role to the absolute paths of its Oh My Pi overlays, e.g. {implementer: [/etc/legion-operator/fallback.yml]}"},
		{"an unknown role", "role_config_files: {coder: [/etc/a.yml]}\n",
			`role_config_files names "coder", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a role named twice", "role_config_files: {tester: [/etc/a.yml], tester: [/etc/b.yml]}\n",
			"role_config_files names tester twice"},
		{"an empty list", "role_config_files: {tester: []}\n",
			"role_config_files.tester names no overlay: give the role at least one path, or leave it out"},
		{"a list of no strings", "role_config_files: {tester: [1]}\n",
			"role_config_files.tester must be an array of non-empty strings"},
		{"a relative path", "role_config_files: {tester: [overlay.yml]}\n",
			`role_config_files.tester entry "overlay.yml" must be a clean absolute path: the role's Oh My Pi reads it from wherever it starts`},
		{"an unclean path", "role_config_files: {tester: [/etc/../etc/a.yml]}\n",
			`role_config_files.tester entry "/etc/../etc/a.yml" must be a clean absolute path: the role's Oh My Pi reads it from wherever it starts`},
		{"the separator", "role_config_files: {tester: [/etc/a:b.yml]}\n",
			"role_config_files.tester entry \"/etc/a:b.yml\" holds `:`, which separates the overlays PI_CONFIG_FILES names"},
		{"a duplicate", "role_config_files: {tester: [/etc/a.yml, /etc/b.yml, /etc/a.yml]}\n",
			"role_config_files.tester names /etc/a.yml twice"},
		{"the controller under controller: operator", "role_config_files: {controller: [/etc/a.yml]}\n",
			"role_config_files.controller gives overlays to the controller the daemon launches, and controller: operator launches none: set controller: daemon or drop the key"},
		{"a role on claude", "worker_backends: {implementer: claude}\nrole_config_files: {implementer: [/etc/a.yml]}\n",
			"role_config_files.implementer names Oh My Pi overlays, and worker_backends.implementer is claude, which reads none: drop the role from one of the two"},
		{"an overlay every pane names", "runtime: {tmux: {pi_config_files: [/etc/legion-operator/overlay.yml]}}\nrole_config_files: {tester: [/etc/legion-operator/overlay.yml]}\n",
			"role_config_files.tester entry /etc/legion-operator/overlay.yml is already every role's overlay (runtime.tmux.pi_config_files): name only what this role adds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// rolePod is a pod block whose ConfigMap volume projects two items into a directory mount and
// mounts a third key by sub_path, beside a whole Secret volume.
const rolePod = `    pod:
      env: {PI_CONFIG_FILES: /etc/legion-operator/overlay.yml}
      volumes:
        - name: operator-route
          config_map: {name: legion-operator-route, items: [{key: overlay.yml, path: overlay.yml}, {key: fallback.yml, path: roles/fallback.yml}]}
        - name: operator-creds
          secret: {name: legion-operator-creds}
      volume_mounts:
        - {volume: operator-route, mount_path: /etc/legion-operator/overlay.yml, sub_path: overlay.yml}
        - {volume: operator-route, mount_path: /etc/legion-route}
        - {volume: operator-creds, mount_path: /etc/legion-creds}
`

// Under kubernetes a role's overlay must be a file one of the operator's mounts holds, since a pod
// and the image probe see only those and the image: a sub_path mount of the file, an item a
// directory mount of a ConfigMap projects, or any file under a whole Secret volume. A path no mount
// holds, a directory mount itself, a key the projection leaves out, and the operator's own overlay
// again are refused at load, which is what `legion start --check-config` runs.
func TestRoleConfigFilesUnderKubernetesMustBeInTheOperatorsMounts(t *testing.T) {
	for _, tc := range []struct{ name, files, want string }{
		{"an item the directory mount projects", "[/etc/legion-route/roles/fallback.yml]", ""},
		{"a file under a whole secret", "[/etc/legion-creds/anything.yml]", ""},
		{"a file no mount holds", "[/etc/legion-operator/fallback.yml]",
			"role_config_files.implementer entry /etc/legion-operator/fallback.yml is under no runtime.kubernetes.pod.volume_mounts mount_path: a role's pod and the image probe's see only the image and the operator's mounts, so mount the ConfigMap that holds it there"},
		{"the directory mount itself", "[/etc/legion-route]",
			"role_config_files.implementer entry /etc/legion-route is the mount_path of volume operator-route, which mounts as a directory: name a file in it, or mount the file with sub_path"},
		{"a key the projection leaves out", "[/etc/legion-route/fallback.yml]",
			"role_config_files.implementer entry /etc/legion-route/fallback.yml is not among the items volume operator-route projects at /etc/legion-route (overlay.yml, roles/fallback.yml): add an item with path fallback.yml"},
		{"the operator's own overlay", "[/etc/legion-operator/overlay.yml]",
			"role_config_files.implementer entry /etc/legion-operator/overlay.yml is already every role's overlay (runtime.kubernetes.pod.env PI_CONFIG_FILES): name only what this role adds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+rolePod+"role_config_files: {implementer: "+tc.files+"}\n"), noEnv)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadForValidation: %v", err)
			case tc.want == "" && len(cfg.RoleConfigFiles[claim.RoleImplementer]) != 1:
				t.Fatalf("RoleConfigFiles = %v, want the implementer's one file", cfg.RoleConfigFiles)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
