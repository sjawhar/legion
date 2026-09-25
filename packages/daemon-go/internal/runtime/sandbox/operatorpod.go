package sandbox

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// imageEnv are the variables the worker image's final stage sets (packages/daemon/docker/
// worker.Dockerfile, ENV), which every container inherits: HOME and OMP_PROFILE place Oh My Pi's
// profile, whose sessions directory a pod mounts from the tree volume; LEGION_OMP_PATH is the Oh
// My Pi the image probe proves the workers run; LEGION_ROLE_PROMPTS_DIR the role prompts it reads;
// and PATH, which the runtime sets as well. TestImageEnvIsWhatTheWorkerImageSets holds the list to
// the Dockerfile.
var imageEnv = []string{"HOME", "LEGION_OMP_PATH", "LEGION_ROLE_PROMPTS_DIR", "OMP_PROFILE", "PATH"}

// sessionPlacing are the pod baseline's variables (internal/podsafety) that decide where Oh My Pi
// keeps a session: PI_CONFIG_DIR names the config root the profile's agent directory, and its
// sessions, sit under, and OMP_SESSION_STORAGE whether a session is a file at all. A pod keeps its
// sessions as files on the tree volume, where a resume reads them, so the operator may not set
// them as it may the rest of the baseline.
var sessionPlacing = []string{"OMP_SESSION_STORAGE", "PI_CONFIG_DIR"}

// setter is who sets a variable a worker's Oh My Pi starts with, and whether the operator's pod
// env may set it too: the pod baseline yields to the operator's own value, and the operator's
// variables are its own.
type setter struct {
	who         string
	operatorMay bool
}

// podVariables maps every variable a worker's Oh My Pi finds set before the shim exports a
// provider key (shim.ReadProviderEnv) to who sets it: the runtime (runtimeOwned), the worker image
// (imageEnv), every launch's spec (the role App's git identity, and each launch secret's
// `<NAME>_FILE` pointer), the pod baseline, and the operator's own pod env. The first setter of a
// name is the one named.
func podVariables(pod Pod, launchSecrets []string) map[string]setter {
	set := map[string]setter{}
	add := func(name, who string, operatorMay bool) {
		if _, taken := set[name]; !taken {
			set[name] = setter{who, operatorMay}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(runtimeOwned)) {
		add(name, "Legion's runtime sets in every pod", false)
	}
	for _, name := range imageEnv {
		add(name, "the worker image sets (packages/daemon/docker/worker.Dockerfile ENV)", false)
	}
	for name := range (runtime.GitIdentity{}).Env() {
		add(name, "every launch sets (the git identity the role's GitHub App commits as)", false)
	}
	for _, name := range launchSecrets {
		add(name+"_FILE", "every launch sets (the pointer to the launch secret "+name+")", false)
	}
	for _, name := range sessionPlacing {
		add(name, "the pod baseline sets, and it decides where Oh My Pi keeps the session a resume reads", false)
	}
	for _, name := range podsafety.Variables() {
		add(name, "the pod baseline sets (internal/podsafety)", true)
	}
	for name := range pod.Env {
		add(name, "runtime.kubernetes.pod.env sets", true)
	}
	return set
}

// CheckPod refuses a piece of the operator's pod (runtime.kubernetes.pod) or a provider key that
// collides with what Legion puts in a pod, naming both. A variable the runtime, the image, or every
// launch sets would reach the agent's container twice, and one that places Oh My Pi's sessions
// would move them off the tree volume; a volume name would be in the pod twice; a mount at, under,
// or above a path Legion mounts, the image owns (imageOwnedPaths), or a tool runs from hides it or
// is hidden by it. A provider key must name a variable nothing else in the pod sets, since the shim
// refuses one its own environment names and would replace one Oh My Pi's environment gains after
// (the pod baseline), and skips one whose `<NAME>_FILE` pointer the pod sets (shim.ReadProviderEnv).
// launchSecrets are the secrets every launch's spec carries, by name. configure runs it, and the
// daemon before its boot and for `legion start --check-config`.
func CheckPod(pod Pod, providerKeys map[string]string, tools Tools, launchSecrets []string) error {
	variables := podVariables(pod, launchSecrets)
	for _, name := range slices.Sorted(maps.Keys(pod.Env)) {
		if s := variables[name]; !s.operatorMay {
			return fmt.Errorf("runtime.kubernetes.pod.env sets %s, which %s", name, s.who)
		}
	}
	volumes := legionVolumeNames()
	for i, volume := range pod.Volumes {
		if slices.Contains(volumes, volume.Name) {
			return fmt.Errorf("runtime.kubernetes.pod.volumes[%d].name %s is a volume Legion puts in every pod", i, volume.Name)
		}
	}
	owners := []struct {
		paths       []string
		owns, whose string
	}{
		{legionMountPaths(), "Legion mounts in every pod", "Legion's"},
		{append(imageOwnedPaths(), tools.GH, tools.Git, tools.JJ, tools.Legion), "the worker image owns", "the image's"},
	}
	for i, mount := range pod.VolumeMounts {
		for _, owner := range owners {
			for _, owned := range owner.paths {
				if overlaps(mount.MountPath, owned) {
					return fmt.Errorf("runtime.kubernetes.pod.volume_mounts[%d].mount_path %s overlaps %s, which %s: a mount may be neither at, under, nor above one of %s",
						i, mount.MountPath, owned, owner.owns, owner.whose)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(providerKeys)) {
		if s, set := variables[name]; set {
			return fmt.Errorf("provider_keys names %s, which %s: a provider key must name a variable nothing else in the pod sets", name, s.who)
		}
		if s, set := variables[name+"_FILE"]; set {
			return fmt.Errorf("provider_keys names %s, whose pointer %s_FILE %s: the shim skips a key whose pointer the pod sets", name, name, s.who)
		}
	}
	return nil
}

// overlaps reports whether one of two clean absolute paths is the other or lies under it.
func overlaps(a, b string) bool {
	under := func(child, parent string) bool { return parent == "/" || strings.HasPrefix(child, parent+"/") }
	return a == b || under(a, b) || under(b, a)
}
