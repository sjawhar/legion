package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Kubernetes is the settled `runtime.kubernetes` block: where the Sandbox runtime runs its pods and
// what they carry. Every value is the file's own text (a path resolved against the file's
// directory); the daemon translates it into the runtime's options.
type Kubernetes struct {
	Namespace string
	// Image is the worker image, pinned by digest.
	Image string
	// StorageClass is the tree volume's class; required, because the cluster has no default.
	StorageClass string
	// TreeVolume is the tree volume's size, a Kubernetes quantity; 20Gi when the file sets none.
	TreeVolume string
	// Kubeconfig is the credentials file the daemon's client reads; "" means in-cluster
	// credentials. Context names the context in it to use, "" for its current context.
	Kubeconfig string
	Context    string
	Scheduling Scheduling
	// Resources are each role's container requests and limits. A role absent here gets none,
	// which is the default for every role: one tree runs per node, and the pool's floor sizes it.
	Resources map[claim.Role]RoleResources
	Gateway   Gateway
	// Pod is what the operator adds to every pod (runtime.kubernetes.pod).
	Pod PodConfig
}

// Scheduling is where the pods may run beyond the Legion pool, which the runtime selects itself.
type Scheduling struct {
	NodeSelector  map[string]string
	Tolerations   []Toleration
	PriorityClass string
}

// Toleration is one `scheduling.tolerations` entry. Value is "" under operator Exists.
type Toleration struct{ Key, Operator, Value, Effect string }

// RoleResources are one role's container requests and limits.
type RoleResources struct{ Requests, Limits Quantities }

// Quantities are Kubernetes quantities for the three resources a role may set; "" leaves one unset.
type Quantities struct{ CPU, Memory, EphemeralStorage string }

// PodConfig is `runtime.kubernetes.pod`, what the operator adds to every pod Legion runs, the image
// probe's included, in the API's own types: variables and volume mounts for the agent's container,
// the volumes they mount (each a Secret, a ConfigMap, or a projection of ServiceAccount tokens,
// Secrets, and ConfigMaps), and the ServiceAccount the pods run as. It passes through as written:
// the loader refuses a shape no pod could carry, the daemon's boot a name or path of Legion's own
// or the worker image's (daemon.CheckOperatorPod), and the API server the rest when it creates the
// image probe's pod, which carries it, at boot.
type PodConfig struct {
	Env            map[string]string
	Volumes        []corev1.Volume
	VolumeMounts   []corev1.VolumeMount
	ServiceAccount string
}

// Gateway is how a pod reaches the model: the model gateway's base URL, and the service account
// whose projected token (for Audience, rotated within TokenExpiry) is the pod's key there.
type Gateway struct {
	URL, Audience, ServiceAccount string
	TokenExpiry                   time.Duration
}

const (
	kubernetesKey     = "runtime.kubernetes"
	podKey            = kubernetesKey + ".pod"
	defaultTreeVolume = "20Gi"
	// minTokenExpiry is the shortest projected service account token Kubernetes issues, and
	// maxTokenExpiry the longest the cluster's admission policy admits for a Legion pod
	// (agent-c #20053, the legion-sandbox-pods fence).
	minTokenExpiry = 600
	maxTokenExpiry = 3600
	// poolLabel is the node label that selects the Legion pool. The runtime sets it on every pod,
	// and the cluster's admission policy requires its value.
	poolLabel = "legion.dev/pool"
)

// imageDigestRef is a reference pinned by digest, the shipped `parseImageDigestRef`
// (packages/daemon/src/daemon/image-ref.ts): the daemon must know exactly which image it probed.
var imageDigestRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// readKubernetes reads the `runtime.kubernetes` block, refusing any member it does not model and
// any value a pod could not run with. What depends on keys outside the block is checked once the
// whole file is read (checkKubernetesKeys).
func readKubernetes(value *yaml.Node) (*Kubernetes, error) {
	if value.Kind != yaml.MappingNode {
		return nil, errors.New("runtime.kubernetes must be a mapping")
	}
	fields, err := members(value, kubernetesKey, "namespace", "image", "storage_class", "tree_volume",
		"kubeconfig", "context", "scheduling", "resources", "gateway", "pod", "session_store", "session_dsn_secret",
		"role_profiles")
	if err != nil {
		return nil, err
	}
	if fields["role_profiles"] != nil {
		return nil, errors.New("unknown key runtime.kubernetes.role_profiles: each role's requests and limits are set under runtime.kubernetes.resources, and a role absent there gets none")
	}
	block := &Kubernetes{TreeVolume: defaultTreeVolume}
	if block.Namespace, err = requiredString(fields["namespace"], kubernetesKey+".namespace", ""); err != nil {
		return nil, err
	}
	if block.Image, err = requiredString(fields["image"], kubernetesKey+".image", ""); err != nil {
		return nil, err
	}
	if !imageDigestRef.MatchString(block.Image) {
		return nil, errors.New("runtime.kubernetes.image must be pinned by digest (@sha256:…)")
	}
	if block.StorageClass, err = requiredString(fields["storage_class"], kubernetesKey+".storage_class",
		": the cluster has no default storage class for the tree volume"); err != nil {
		return nil, err
	}
	treeVolume, err := readQuantity(fields["tree_volume"], kubernetesKey+".tree_volume")
	if err != nil {
		return nil, err
	}
	if treeVolume != "" {
		block.TreeVolume = treeVolume
	}
	if block.Kubeconfig, err = optionalString(fields["kubeconfig"], kubernetesKey+".kubeconfig"); err != nil {
		return nil, err
	}
	if block.Context, err = optionalString(fields["context"], kubernetesKey+".context"); err != nil {
		return nil, err
	}
	if block.Context != "" && block.Kubeconfig == "" {
		return nil, errors.New("runtime.kubernetes.context names a kubeconfig context, so it requires runtime.kubernetes.kubeconfig")
	}
	if err := checkSessionStore(fields["session_store"], fields["session_dsn_secret"]); err != nil {
		return nil, err
	}
	if block.Scheduling, err = readScheduling(fields["scheduling"]); err != nil {
		return nil, err
	}
	if block.Resources, err = readResources(fields["resources"]); err != nil {
		return nil, err
	}
	if block.Gateway, err = readGateway(fields["gateway"]); err != nil {
		return nil, err
	}
	if block.Pod, err = readPod(fields["pod"]); err != nil {
		return nil, err
	}
	if account := block.Pod.ServiceAccount; account != "" && account != block.Gateway.ServiceAccount {
		return nil, fmt.Errorf("%s.service_account %s differs from %s.gateway.service_account %s: a pod runs as one account",
			podKey, account, kubernetesKey, block.Gateway.ServiceAccount)
	}
	return block, nil
}

// readPod reads `runtime.kubernetes.pod`, refusing a shape no pod could carry.
func readPod(value *yaml.Node) (PodConfig, error) {
	if value == nil {
		return PodConfig{}, nil
	}
	if value.Kind != yaml.MappingNode {
		return PodConfig{}, fmt.Errorf("%s must be a mapping", podKey)
	}
	fields, err := members(value, podKey, "env", "volumes", "volume_mounts", "service_account")
	if err != nil {
		return PodConfig{}, err
	}
	var pod PodConfig
	if pod.Env, err = readPodEnv(fields["env"]); err != nil {
		return PodConfig{}, err
	}
	if pod.Volumes, err = readPodVolumes(fields["volumes"]); err != nil {
		return PodConfig{}, err
	}
	if pod.VolumeMounts, err = readPodMounts(fields["volume_mounts"], pod.Volumes); err != nil {
		return PodConfig{}, err
	}
	if pod.ServiceAccount, err = optionalString(fields["service_account"], podKey+".service_account"); err != nil {
		return PodConfig{}, err
	}
	return pod, nil
}

// readPodEnv is `pod.env`: a mapping of variable to value, refusing a variable shaped like a
// credential's (runtime.IsSecretLikeName, its `_FILE` pointers allowed, as a spec's Env is judged
// in runtime.ValidateSpawnSpec): a credential travels in a Secret, never as a plain value in the
// pod's spec.
func readPodEnv(value *yaml.Node) (map[string]string, error) {
	const key = podKey + ".env"
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of variable to value", key)
	}
	env := map[string]string{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		nameNode, entry := value.Content[i], value.Content[i+1]
		name := nameNode.Value
		if nameNode.Kind != yaml.ScalarNode || !envVarName.MatchString(name) {
			return nil, fmt.Errorf("%s key %q %s", key, name, envNameRule)
		}
		var text string
		if entry.Kind != yaml.ScalarNode || entry.Tag == "!!null" || entry.Decode(&text) != nil {
			return nil, fmt.Errorf("%s value for %s must be a string", key, name)
		}
		switch _, twice := env[name]; {
		case twice:
			return nil, fmt.Errorf("%s names %s twice", key, name)
		case runtime.IsSecretLikeName(name) && !strings.HasSuffix(name, "_FILE"):
			return nil, fmt.Errorf("%s sets %s, a credential-shaped name: a credential comes from a Secret, so mount one with %s.volumes or name its key in provider_keys",
				key, name, podKey)
		}
		env[name] = text
	}
	return env, nil
}

// readPodVolumes is `pod.volumes`: each named once, with exactly one source.
func readPodVolumes(value *yaml.Node) ([]corev1.Volume, error) {
	const key = podKey + ".volumes"
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	named := map[string]bool{}
	volumes := make([]corev1.Volume, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := members(entry, field, "name", "secret", "config_map", "projected")
		if err != nil {
			return nil, err
		}
		var volume corev1.Volume
		if volume.Name, err = requiredString(fields["name"], field+".name", ""); err != nil {
			return nil, err
		}
		switch {
		case named[volume.Name]:
			return nil, fmt.Errorf("%s names %s twice", key, volume.Name)
		case setCount(fields, "secret", "config_map", "projected") != 1:
			return nil, fmt.Errorf("%s must set exactly one of secret, config_map, projected", field)
		}
		named[volume.Name] = true
		var name string
		var items []corev1.KeyToPath
		switch {
		case fields["secret"] != nil:
			if name, items, err = readObjectSource(fields["secret"], field+".secret"); err == nil {
				volume.Secret = &corev1.SecretVolumeSource{SecretName: name, Items: items}
			}
		case fields["config_map"] != nil:
			if name, items, err = readObjectSource(fields["config_map"], field+".config_map"); err == nil {
				volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Items: items}
			}
		default:
			volume.Projected, err = readProjected(fields["projected"], field+".projected")
		}
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

// setCount is how many of names fields sets.
func setCount(fields map[string]*yaml.Node, names ...string) int {
	count := 0
	for _, name := range names {
		if fields[name] != nil {
			count++
		}
	}
	return count
}

// readObjectSource is a Secret or ConfigMap source: its name, and the keys it projects, each with
// the file it becomes; every key, each a file of its own name, when it lists none.
func readObjectSource(value *yaml.Node, key string) (string, []corev1.KeyToPath, error) {
	if value.Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "name", "items")
	if err != nil {
		return "", nil, err
	}
	name, err := requiredString(fields["name"], key+".name", "")
	if err != nil {
		return "", nil, err
	}
	items := fields["items"]
	if items == nil {
		return name, nil, nil
	}
	if items.Kind != yaml.SequenceNode {
		return "", nil, fmt.Errorf("%s.items must be an array", key)
	}
	var projected []corev1.KeyToPath
	for index, entry := range items.Content {
		field := fmt.Sprintf("%s.items[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return "", nil, fmt.Errorf("%s must be a mapping", field)
		}
		item, err := members(entry, field, "key", "path")
		if err != nil {
			return "", nil, err
		}
		var one corev1.KeyToPath
		if one.Key, err = requiredString(item["key"], field+".key", ""); err != nil {
			return "", nil, err
		}
		if one.Path, err = requiredString(item["path"], field+".path", ""); err != nil {
			return "", nil, err
		}
		projected = append(projected, one)
	}
	return name, projected, nil
}

// readProjected is a projected volume: one or more sources, each exactly one of a ServiceAccount
// token, a Secret, or a ConfigMap.
func readProjected(value *yaml.Node, key string) (*corev1.ProjectedVolumeSource, error) {
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "sources")
	if err != nil {
		return nil, err
	}
	sources := fields["sources"]
	switch {
	case sources != nil && sources.Kind != yaml.SequenceNode:
		return nil, fmt.Errorf("%s.sources must be an array", key)
	case sources == nil || len(sources.Content) == 0:
		return nil, fmt.Errorf("%s.sources must name at least one source", key)
	}
	projected := &corev1.ProjectedVolumeSource{}
	for index, entry := range sources.Content {
		field := fmt.Sprintf("%s.sources[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		kinds, err := members(entry, field, "service_account_token", "secret", "config_map")
		if err != nil {
			return nil, err
		}
		if setCount(kinds, "service_account_token", "secret", "config_map") != 1 {
			return nil, fmt.Errorf("%s must set exactly one of service_account_token, secret, config_map", field)
		}
		var source corev1.VolumeProjection
		var name string
		var items []corev1.KeyToPath
		switch {
		case kinds["service_account_token"] != nil:
			source.ServiceAccountToken, err = readTokenProjection(kinds["service_account_token"], field+".service_account_token")
		case kinds["secret"] != nil:
			if name, items, err = readObjectSource(kinds["secret"], field+".secret"); err == nil {
				source.Secret = &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Items: items}
			}
		default:
			if name, items, err = readObjectSource(kinds["config_map"], field+".config_map"); err == nil {
				source.ConfigMap = &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Items: items}
			}
		}
		if err != nil {
			return nil, err
		}
		projected.Sources = append(projected.Sources, source)
	}
	return projected, nil
}

// readTokenProjection is a projected ServiceAccount token: the file it is written to, and the
// audience and lifetime it is issued for when set (the API server's own audience and default
// lifetime when not).
func readTokenProjection(value *yaml.Node, key string) (*corev1.ServiceAccountTokenProjection, error) {
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "audience", "expiration_seconds", "path")
	if err != nil {
		return nil, err
	}
	token := &corev1.ServiceAccountTokenProjection{}
	if token.Path, err = requiredString(fields["path"], key+".path", ""); err != nil {
		return nil, err
	}
	if token.Audience, err = optionalString(fields["audience"], key+".audience"); err != nil {
		return nil, err
	}
	expiry, err := readInt(fields["expiration_seconds"], key+".expiration_seconds")
	switch {
	case err != nil:
		return nil, err
	case expiry != nil && *expiry <= 0:
		return nil, fmt.Errorf("%s.expiration_seconds must be a positive integer", key)
	case expiry != nil:
		seconds := int64(*expiry)
		token.ExpirationSeconds = &seconds
	}
	return token, nil
}

// readPodMounts is `pod.volume_mounts`: each a volume of `pod.volumes` at a clean absolute path no
// other mount of the operator's takes.
func readPodMounts(value *yaml.Node, volumes []corev1.Volume) ([]corev1.VolumeMount, error) {
	const key = podKey + ".volume_mounts"
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	declared := map[string]bool{}
	for _, volume := range volumes {
		declared[volume.Name] = true
	}
	mountedBy := map[string]string{}
	mounts := make([]corev1.VolumeMount, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := members(entry, field, "volume", "mount_path", "sub_path", "read_only")
		if err != nil {
			return nil, err
		}
		mount := corev1.VolumeMount{ReadOnly: true}
		if mount.Name, err = requiredString(fields["volume"], field+".volume", ""); err != nil {
			return nil, err
		}
		if !declared[mount.Name] {
			return nil, fmt.Errorf("%s.volume %s names no volume of %s.volumes", field, mount.Name, podKey)
		}
		if mount.MountPath, err = requiredString(fields["mount_path"], field+".mount_path", ""); err != nil {
			return nil, err
		}
		if !path.IsAbs(mount.MountPath) || path.Clean(mount.MountPath) != mount.MountPath {
			return nil, fmt.Errorf("%s.mount_path %s must be a clean absolute path", field, mount.MountPath)
		}
		if earlier, taken := mountedBy[mount.MountPath]; taken {
			return nil, fmt.Errorf("%s.mount_path %s is also %s's", field, mount.MountPath, earlier)
		}
		mountedBy[mount.MountPath] = field
		if mount.SubPath, err = optionalString(fields["sub_path"], field+".sub_path"); err != nil {
			return nil, err
		}
		if readOnly := fields["read_only"]; readOnly != nil {
			if readOnly.Tag != "!!bool" || readOnly.Decode(&mount.ReadOnly) != nil {
				return nil, fmt.Errorf("%s.read_only must be true or false", field)
			}
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

// resolveKubernetes settles `runtime: kubernetes`: the keys outside the block every pod needs, the
// provider keys every pod mounts from the providers Secret, and the block, its kubeconfig resolved
// against the file's directory.
func resolveKubernetes(file fileConfig, configDir string, cfg *Config) error {
	if err := checkKubernetesKeys(file); err != nil {
		return err
	}
	block := *file.Kubernetes
	if err := checkPodProviderKeys(file.ProviderKeys, block.Pod); err != nil {
		return err
	}
	if block.Kubeconfig != "" {
		block.Kubeconfig = underConfig(block.Kubeconfig, configDir)
	}
	cfg.Runtime = Runtime{Name: "kubernetes", Kubernetes: &block}
	return nil
}

// checkPodProviderKeys refuses a provider key the worker's shim cannot export into Oh My Pi's
// environment beside the operator's own variables (shim.ReadProviderEnv): one naming a variable
// `pod.env` sets, which the shim refuses to override, and one whose `<NAME>_FILE` pointer it sets,
// which the shim skips as a secret the process reads by file. The daemon's boot refuses one that
// collides with a variable of Legion's (daemon.CheckOperatorPod).
func checkPodProviderKeys(keys []ProviderKey, pod PodConfig) error {
	for _, key := range keys {
		_, podSets := pod.Env[key.Env]
		_, podPoints := pod.Env[key.Env+"_FILE"]
		switch {
		case podSets:
			return fmt.Errorf("provider_keys names %s, which %s.env also sets: the shim refuses to export a key its own environment names", key.Env, podKey)
		case podPoints:
			return fmt.Errorf("provider_keys names %s, whose pointer %s_FILE %s.env sets: the shim would skip the key", key.Env, key.Env, podKey)
		}
	}
	return nil
}

// checkSessionStore reads `session_store` and `session_dsn_secret` only to refuse what the Go
// runtime does not do: a pod's session lives on the tree volume until Stage 6 adds the database.
func checkSessionStore(store, dsnSecret *yaml.Node) error {
	name, err := readString(store, kubernetesKey+".session_store")
	if err != nil {
		return err
	}
	switch {
	case name == nil || *name == "pvc":
	case *name == "postgres":
		return errors.New("runtime.kubernetes.session_store postgres is not supported until Stage 6: a pod's session lives on the tree volume (pvc)")
	default:
		return errors.New("runtime.kubernetes.session_store must be 'pvc' or 'postgres'")
	}
	if dsnSecret != nil {
		return errors.New("runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is pvc; remove it")
	}
	return nil
}

// readQuantity reads a positive Kubernetes quantity as the file wrote it, "" when unset. A bare
// YAML number (`tree_volume: 20`) is its text. Zero is refused with the negatives: it is no size,
// and the runtime refuses a zero tree volume.
func readQuantity(value *yaml.Node, key string) (string, error) {
	if value == nil || value.Tag == "!!null" {
		return "", nil
	}
	if value.Kind == yaml.ScalarNode {
		if quantity, err := resource.ParseQuantity(value.Value); err == nil && quantity.Sign() > 0 {
			return value.Value, nil
		}
	}
	return "", fmt.Errorf("%s must be a positive Kubernetes quantity (e.g. 20Gi or 500m)", key)
}

func readScheduling(value *yaml.Node) (Scheduling, error) {
	const key = kubernetesKey + ".scheduling"
	if value == nil {
		return Scheduling{}, nil
	}
	if value.Kind != yaml.MappingNode {
		return Scheduling{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "node_selector", "tolerations", "priority_class")
	if err != nil {
		return Scheduling{}, err
	}
	var scheduling Scheduling
	if scheduling.NodeSelector, err = readNodeSelector(fields["node_selector"], key+".node_selector"); err != nil {
		return Scheduling{}, err
	}
	if scheduling.Tolerations, err = readTolerations(fields["tolerations"], key+".tolerations"); err != nil {
		return Scheduling{}, err
	}
	if scheduling.PriorityClass, err = optionalString(fields["priority_class"], key+".priority_class"); err != nil {
		return Scheduling{}, err
	}
	return scheduling, nil
}

// readNodeSelector is a mapping of label to value, merged by the runtime over the Legion pool's own
// label, which it may not name: the admission policy requires the pool's value.
func readNodeSelector(value *yaml.Node, key string) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	refusal := fmt.Errorf("%s must be a mapping of strings", key)
	if value.Kind != yaml.MappingNode {
		return nil, refusal
	}
	var selector map[string]string
	for i := 0; i+1 < len(value.Content); i += 2 {
		label, entry := value.Content[i].Value, value.Content[i+1]
		var text string
		if entry.Kind != yaml.ScalarNode || entry.Tag == "!!null" || entry.Decode(&text) != nil {
			return nil, refusal
		}
		if label == poolLabel {
			return nil, fmt.Errorf("%s must not set %s: the runtime selects the Legion pool itself, and legion-sandbox-pods requires its value", key, poolLabel)
		}
		if _, exists := selector[label]; exists {
			return nil, fmt.Errorf("%s names %s twice", key, label)
		}
		if selector == nil {
			selector = map[string]string{}
		}
		selector[label] = text
	}
	return selector, nil
}

// readTolerations is the shipped `parseScheduling` tolerations rule (config.ts): key, operator
// Equal or Exists, an effect, and a value only under Equal.
func readTolerations(value *yaml.Node, key string) ([]Toleration, error) {
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	tolerations := make([]Toleration, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := members(entry, field, "key", "operator", "value", "effect")
		if err != nil {
			return nil, err
		}
		var toleration Toleration
		for _, part := range []struct {
			name   string
			target *string
		}{
			{"key", &toleration.Key}, {"operator", &toleration.Operator},
			{"value", &toleration.Value}, {"effect", &toleration.Effect},
		} {
			read, err := readString(fields[part.name], field+"."+part.name)
			if err != nil {
				return nil, err
			}
			if read != nil {
				*part.target = *read
			}
		}
		switch {
		case strings.TrimSpace(toleration.Key) == "":
			return nil, fmt.Errorf("%s.key must not be empty", field)
		case toleration.Operator != "Equal" && toleration.Operator != "Exists":
			return nil, fmt.Errorf("%s.operator must be Equal or Exists", field)
		case toleration.Effect != "NoSchedule" && toleration.Effect != "NoExecute" && toleration.Effect != "PreferNoSchedule":
			return nil, fmt.Errorf("%s.effect must be NoSchedule, NoExecute, or PreferNoSchedule", field)
		case toleration.Operator == "Exists" && toleration.Value != "":
			return nil, fmt.Errorf("%s.value must be omitted with operator Exists", field)
		}
		tolerations = append(tolerations, toleration)
	}
	return tolerations, nil
}

// readResources is a mapping of role to that role's requests and limits.
func readResources(value *yaml.Node) (map[claim.Role]RoleResources, error) {
	const key = kubernetesKey + ".resources"
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of role to requests and limits", key)
	}
	roles := make([]string, len(claim.Roles))
	for i, role := range claim.Roles {
		roles[i] = string(role)
	}
	var resources map[claim.Role]RoleResources
	for i := 0; i+1 < len(value.Content); i += 2 {
		name, entry := value.Content[i].Value, value.Content[i+1]
		role := claim.Role(name)
		if !claim.IsRole(role) {
			return nil, fmt.Errorf("%s key %q must be a role (%s)", key, name, strings.Join(roles, ", "))
		}
		if _, exists := resources[role]; exists {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		field := key + "." + name
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := members(entry, field, "requests", "limits")
		if err != nil {
			return nil, err
		}
		var read RoleResources
		if read.Requests, err = readQuantities(fields["requests"], field+".requests"); err != nil {
			return nil, err
		}
		if read.Limits, err = readQuantities(fields["limits"], field+".limits"); err != nil {
			return nil, err
		}
		if resources == nil {
			resources = map[claim.Role]RoleResources{}
		}
		resources[role] = read
	}
	return resources, nil
}

func readQuantities(value *yaml.Node, key string) (Quantities, error) {
	if value == nil {
		return Quantities{}, nil
	}
	if value.Kind != yaml.MappingNode {
		return Quantities{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "cpu", "memory", "ephemeral_storage")
	if err != nil {
		return Quantities{}, err
	}
	var quantities Quantities
	for _, part := range []struct {
		name   string
		target *string
	}{
		{"cpu", &quantities.CPU}, {"memory", &quantities.Memory}, {"ephemeral_storage", &quantities.EphemeralStorage},
	} {
		if *part.target, err = readQuantity(fields[part.name], key+"."+part.name); err != nil {
			return Quantities{}, err
		}
	}
	return quantities, nil
}

// readGateway reads the model gateway, required with every member: a pod has no other way to a model.
func readGateway(value *yaml.Node) (Gateway, error) {
	const key = kubernetesKey + ".gateway"
	if value == nil {
		return Gateway{}, errors.New(key + " is required: a pod reaches the model only through the gateway, with its projected service account token")
	}
	if value.Kind != yaml.MappingNode {
		return Gateway{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "url", "audience", "service_account", "token_expiry_seconds")
	if err != nil {
		return Gateway{}, err
	}
	var gateway Gateway
	url, err := requiredString(fields["url"], key+".url", "")
	if err != nil {
		return Gateway{}, err
	}
	if gateway.URL, err = baseURL(url, key+".url"); err != nil {
		return Gateway{}, err
	}
	if gateway.Audience, err = requiredString(fields["audience"], key+".audience", ""); err != nil {
		return Gateway{}, err
	}
	if gateway.ServiceAccount, err = requiredString(fields["service_account"], key+".service_account", ""); err != nil {
		return Gateway{}, err
	}
	const expiryKey = key + ".token_expiry_seconds"
	expiry, err := readInt(fields["token_expiry_seconds"], expiryKey)
	switch {
	case err != nil:
		return Gateway{}, err
	case expiry == nil:
		return Gateway{}, fmt.Errorf("%s is required", expiryKey)
	case *expiry < minTokenExpiry:
		return Gateway{}, fmt.Errorf("%s must be at least %d (the kubelet's minimum)", expiryKey, minTokenExpiry)
	case *expiry > maxTokenExpiry:
		return Gateway{}, fmt.Errorf("%s must be at most %d (the longest pod token the cluster's admission policy admits)", expiryKey, maxTokenExpiry)
	}
	gateway.TokenExpiry = time.Duration(*expiry) * time.Second
	return gateway, nil
}

// checkKubernetesKeys refuses a file whose keys outside the block leave a pod unable to run, or
// set something no pod uses. Every URL a pod dials must be one it can reach, so none of them may
// fall back to a loopback default; the host's Oh My Pi has no counterpart in a pod.
func checkKubernetesKeys(file fileConfig) error {
	for _, rule := range []struct {
		key    string
		absent bool
		why    string
	}{
		{"daemon_url", file.DaemonURL == nil, "a pod cannot reach the daemon's loopback address"},
		{"envoy_url", file.EnvoyURL == nil, "a pod cannot reach the loopback listener it defaults to"},
		{"nats_urls", len(file.NatsURLs) == 0, "every pod's Envoy client connects to NATS"},
		{"envoy_token_file", file.EnvoyTokenFile == nil, "every pod receives the Envoy bearer"},
		{"operator_token_file", file.OperatorTokenFile == nil, "the daemon cannot launch the controller there; legion controller start presents this token"},
		{"dispatch_url", file.DispatchURL == nil, "the workflow is what launches every pod"},
		{"github_apps", file.GitHubApps == nil, "every pod's workspace is cloned with the implement App's token"},
		{"projects", file.Projects == nil, "every pod's workspace is its project's repository"},
	} {
		if rule.absent {
			return fmt.Errorf("%s is required when runtime is kubernetes: %s", rule.key, rule.why)
		}
	}
	for _, rule := range []struct {
		key string
		set bool
		why string
	}{
		{"omp_invocation", file.OmpInvocation != nil, "every pod runs the worker image's Oh My Pi"},
		{"omp_launch_prefix", file.OmpLaunchPrefix != nil, "every pod runs the worker image's Oh My Pi"},
	} {
		if rule.set {
			return fmt.Errorf("%s is not used when runtime is kubernetes: %s; remove %s", rule.key, rule.why, rule.key)
		}
	}
	return nil
}

// checkPodReachable refuses an address pods are handed that no pod can reach. Every pod's shim
// dials the worker stream at tcp://<bind>:<worker_stream_port>, so bind must name one of the
// daemon host's own addresses, never loopback or the unspecified address it would listen on. Every
// Legion URL a pod is handed - daemon_url, envoy_url, dispatch_url, and each nats_urls entry - must
// name neither: a loopback host is, in a pod, the pod itself, and the unspecified address is no host
// at all.
func checkPodReachable(cfg Config) error {
	if ip := net.ParseIP(cfg.Bind); strings.EqualFold(cfg.Bind, "localhost") || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return fmt.Errorf("bind %s is not an address a pod can reach, and every pod's shim dials the worker stream at tcp://%s; bind the daemon host's own address when runtime is kubernetes",
			cfg.Bind, net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.WorkerStreamPort)))
	}
	addresses := []struct{ key, value string }{
		{"daemon_url", cfg.DaemonURL}, {"envoy_url", cfg.EnvoyURL}, {"dispatch_url", cfg.DispatchURL},
	}
	for _, raw := range cfg.NatsURLs {
		addresses = append(addresses, struct{ key, value string }{"nats_urls", raw})
	}
	for _, address := range addresses {
		parsed, err := url.Parse(address.value)
		if err != nil {
			return fmt.Errorf("%s must be a valid URL", address.key)
		}
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		var why string
		switch {
		case strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback():
			why = "names a loopback host, which in a pod is the pod itself"
		case ip != nil && ip.IsUnspecified():
			why = "names the unspecified address, which is no host a pod can dial"
		default:
			continue
		}
		return fmt.Errorf("%s %s %s; name the host pods reach it at when runtime is kubernetes", address.key, address.value, why)
	}
	return nil
}
