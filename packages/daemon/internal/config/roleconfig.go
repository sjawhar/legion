package config

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// RoleConfigRoles are the roles `role_config_files` may name, in the order the workflow reaches
// them: every issue role, then the controller, which only `controller: daemon` launches.
var RoleConfigRoles = append(slices.Clone(claim.Roles), claim.RoleController)

// readRoleConfigFiles is `role_config_files`: a mapping of a Legion role (RoleConfigRoles) to the
// absolute paths of the Oh My Pi settings overlays that role alone names in PI_CONFIG_FILES, after
// every role's own (runtime.tmux.pi_config_files, or PI_CONFIG_FILES in runtime.kubernetes.pod.env).
// A role named twice or not a Legion role, a list that is empty or not a list of strings, and an
// entry that is relative, unclean, holds PI_CONFIG_FILES' separator `:` or repeats another of the
// role's are each refused naming the entry: a relative path would resolve against wherever the
// role's Oh My Pi starts, which is no file of the operator's.
func readRoleConfigFiles(value *yaml.Node, key string) (map[claim.Role][]string, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role to the absolute paths of its Oh My Pi overlays, e.g. {implementer: [/etc/legion-operator/fallback.yml]}", key)
	}
	files := map[claim.Role][]string{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, listNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(RoleConfigRoles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion role (%s)", key, roleNode.Value, runtime.JoinRoles(RoleConfigRoles))
		}
		if _, seen := files[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		field := key + "." + string(role)
		list, err := readStrings(listNode, field)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("%s names no overlay: give the role at least one path, or leave it out", field)
		}
		for i, file := range list {
			switch {
			case !filepath.IsAbs(file) || filepath.Clean(file) != file:
				return nil, fmt.Errorf("%s entry %q must be a clean absolute path: the role's Oh My Pi reads it from wherever it starts", field, file)
			case strings.Contains(file, ":"):
				return nil, fmt.Errorf("%s entry %q holds `:`, which separates the overlays PI_CONFIG_FILES names", field, file)
			case slices.Contains(list[:i], file):
				return nil, fmt.Errorf("%s names %s twice", field, file)
			}
		}
		files[role] = list
	}
	return files, nil
}

// checkRoleConfigFiles refuses what `role_config_files` names that no role could load as named,
// once the rest of the file is settled: the controller's entry when the daemon launches no
// controller (controller: operator); a role on a harness other than Oh My Pi (worker_backends),
// which reads no PI_CONFIG_FILES; an overlay every role already names (the operator's own); and,
// under runtime kubernetes, an overlay outside every runtime.kubernetes.pod mount
// (checkRoleConfigMount), since a pod sees only the image and the operator's mounts, and a file the
// image ships is no operator's. Under tmux a file the host lacks is refused at boot and by
// `legion start --check-config`, which read the host (internal/daemon, readBoot); under kubernetes
// the image probe reads every role's overlays in the mounts a role sees.
func checkRoleConfigFiles(cfg Config) error {
	everyRole := operatorOverlays(cfg)
	for _, role := range RoleConfigRoles {
		files := cfg.RoleConfigFiles[role]
		if len(files) == 0 {
			continue
		}
		field := "role_config_files." + string(role)
		if role == claim.RoleController && cfg.ControllerLaunch != ControllerLaunchDaemon {
			return fmt.Errorf("%s gives overlays to the controller the daemon launches, and controller: operator launches none: set controller: daemon or drop the key", field)
		}
		if backend := cfg.BackendFor(role); backend != runtime.BackendOmp {
			return fmt.Errorf("%s names Oh My Pi overlays, and worker_backends.%s is %s, which reads none: drop the role from one of the two", field, role, backend)
		}
		for _, file := range files {
			if slices.Contains(everyRole.files, file) {
				return fmt.Errorf("%s entry %s is already every role's overlay (%s): name only what this role adds", field, file, everyRole.key)
			}
			if k := cfg.Runtime.Kubernetes; k != nil {
				if err := checkRoleConfigMount(k.Pod, field, file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// everyRoleOverlays are the overlays every role names before its own, and the key they come from.
type everyRoleOverlays struct {
	key   string
	files []string
}

// operatorOverlays is the operator's overlays every role names: runtime.tmux.pi_config_files under
// tmux, and PI_CONFIG_FILES in runtime.kubernetes.pod.env under kubernetes.
func operatorOverlays(cfg Config) everyRoleOverlays {
	if k := cfg.Runtime.Kubernetes; k != nil {
		const key = podKey + ".env PI_CONFIG_FILES"
		value := k.Pod.Env["PI_CONFIG_FILES"]
		if value == "" {
			return everyRoleOverlays{key: key}
		}
		return everyRoleOverlays{key: key, files: strings.Split(value, ":")}
	}
	if t := cfg.Runtime.Tmux; t != nil {
		return everyRoleOverlays{key: "runtime.tmux.pi_config_files", files: t.PiConfigFiles}
	}
	return everyRoleOverlays{}
}

// checkRoleConfigMount refuses file, an overlay of role_config_files' field, unless a mount of the
// operator's pod block (runtime.kubernetes.pod.volume_mounts), which every role's pod and the
// image probe's carry, holds it: the deepest mount at or above it, as the kernel resolves the
// path. A mount at the file itself must be a sub_path mount, since a whole volume mounts as a
// directory; and under a whole ConfigMap or Secret volume that projects items, the file must be one
// of them, since the kubelet writes nothing else there. Whether the ConfigMap or Secret holds the
// key is the cluster's to say, which the image probe reads.
func checkRoleConfigMount(pod PodConfig, field, file string) error {
	var holder *corev1.VolumeMount
	for i, mount := range pod.VolumeMounts {
		if file != mount.MountPath && !strings.HasPrefix(file, mount.MountPath+"/") {
			continue
		}
		if holder == nil || len(mount.MountPath) > len(holder.MountPath) {
			holder = &pod.VolumeMounts[i]
		}
	}
	if holder == nil {
		return fmt.Errorf("%s entry %s is under no %s.volume_mounts mount_path: a role's pod and the image probe's see only the image and the operator's mounts, so mount the ConfigMap that holds it there", field, file, podKey)
	}
	if holder.SubPath != "" {
		return nil
	}
	if file == holder.MountPath {
		return fmt.Errorf("%s entry %s is the mount_path of volume %s, which mounts as a directory: name a file in it, or mount the file with sub_path", field, file, holder.Name)
	}
	items := projectedItems(pod.Volumes, holder.Name)
	if items == nil {
		return nil
	}
	rel := strings.TrimPrefix(file, holder.MountPath+"/")
	var paths []string
	for _, item := range items {
		if path.Clean(item.Path) == rel {
			return nil
		}
		paths = append(paths, item.Path)
	}
	return fmt.Errorf("%s entry %s is not among the items volume %s projects at %s (%s): add an item with path %s", field, file, holder.Name, holder.MountPath, strings.Join(paths, ", "), rel)
}

// projectedItems are the items the pod block's ConfigMap or Secret volume named name projects, nil
// when it is another kind of volume or projects every key.
func projectedItems(volumes []corev1.Volume, name string) []corev1.KeyToPath {
	for _, v := range volumes {
		if v.Name != name {
			continue
		}
		switch {
		case v.ConfigMap != nil:
			return v.ConfigMap.Items
		case v.Secret != nil:
			return v.Secret.Items
		}
	}
	return nil
}
