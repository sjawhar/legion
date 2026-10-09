package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

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
	// StorageClass is the issue volumes' class; required, because the cluster has no default.
	StorageClass string
	// IssueVolume is each issue's volume's size, a Kubernetes quantity, one volume per issue (the
	// controller's pod owns one of its own); 20Gi when the file sets none.
	IssueVolume string
	// Kubeconfig is the credentials file the daemon's client reads; "" means in-cluster
	// credentials. Context names the context in it to use, "" for its current context.
	Kubeconfig string
	Context    string
	Scheduling Scheduling
	// Resources are each role's container reservation — the six workflow roles' and the
	// controller's, whose pod a daemon under `controller: daemon` launches. After load every role is
	// present with every field filled: the file's value where runtime.kubernetes.resources.<role>
	// sets one, the daemon's default (DefaultResources) otherwise, field by field. CPU and Memory
	// are each both the container's request and its limit, so every Legion pod is Guaranteed and
	// bursts past nothing; at the defaults a six-role issue pod sums to 3 CPU and 15 GiB.
	// EphemeralStorage bounds what the container writes to the node's disk — its root filesystem,
	// which no volume backs: the role's $HOME, Oh My Pi's state home with its Chromium profiles and
	// logs, the Go and Bun caches — since pods of unrelated trees share a node and one role filling
	// the node's disk would put every pod on it under DiskPressure; a container past its own limit
	// has its pod evicted, the offending issue's alone. EphemeralStorageRequest is what the scheduler
	// fits to the node's allocatable ephemeral storage (its root volume), small by default so that it
	// binds almost nothing.
	Resources map[claim.Role]RoleResources
	// Pod is what the operator adds to every pod (runtime.kubernetes.pod).
	Pod PodConfig
	// AgentSecrets is the secrets broker every pod is enrolled with (runtime.kubernetes.agent_secrets);
	// nil when the deployment enrolls none, in which case pods carry no token for it.
	AgentSecrets *AgentSecretsConfig
}

// Scheduling is where the pods may run beyond the Legion pool, which the runtime selects itself.
type Scheduling struct {
	NodeSelector  map[string]string
	Tolerations   []Toleration
	PriorityClass string
}

// Toleration is one `scheduling.tolerations` entry. Value is "" under operator Exists.
type Toleration struct{ Key, Operator, Value, Effect string }

// RoleResources are one role's reservation, each a Kubernetes quantity: the cpu and memory every
// container of the role carries as both request and limit, and the ephemeral storage it carries as
// a limit (EphemeralStorage) and as a request (EphemeralStorageRequest, at most the limit). "" is
// unset only while the file is read; the settled block holds every field (DefaultResources fills
// what the file leaves out).
type RoleResources struct{ CPU, Memory, EphemeralStorage, EphemeralStorageRequest string }

// Reserved reports whether the role's pod reserves its CPU and memory and is bounded in both: CPU
// and Memory each set, each the container's request and its limit. It is the resource-limits
// capability's measure (capabilities.Deployment.RolesWithoutResources); a settled Kubernetes block
// holds every role reserved, since DefaultResources fills what the file leaves out, and only a
// Kubernetes value built without the loader (a test's) can report a role unreserved.
func (r RoleResources) Reserved() bool {
	return r.CPU != "" && r.Memory != ""
}

// defaultResources is each role's reservation when the file sets none: the roles that build and
// test a change get the most, the roles that read and write get less, and the controller, which
// runs alone in its pod, gets a pod of its own size. The image probe pod takes the controller's.
// The ephemeral-storage limit bounds the role's writes to the node's disk (the container's root
// filesystem: $HOME, the state home, the Go and Bun caches a build fills), 20Gi for the two roles
// that build, 10Gi for the rest; the request is 1Gi for every role, since the scheduler fits it to
// the node's allocatable ephemeral storage — its root volume, which nobody has sized for these
// pods — and a small request binds scheduling to almost nothing. An operator who knows the root
// volume raises the request so the scheduler reserves disk.
var defaultResources = map[claim.Role]RoleResources{
	claim.RoleArchitect:   {CPU: "250m", Memory: "1Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
	claim.RolePlanner:     {CPU: "250m", Memory: "1Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
	claim.RoleImplementer: {CPU: "750m", Memory: "4Gi", EphemeralStorage: "20Gi", EphemeralStorageRequest: "1Gi"},
	claim.RoleTester:      {CPU: "750m", Memory: "4Gi", EphemeralStorage: "20Gi", EphemeralStorageRequest: "1Gi"},
	claim.RoleReviewer:    {CPU: "750m", Memory: "4Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
	claim.RoleMerger:      {CPU: "250m", Memory: "1Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
	claim.RoleController:  {CPU: "1", Memory: "4Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
}

// DefaultResources is every role's default reservation, the controller's included, as a fresh map
// the caller may edit: what Kubernetes.Resources holds when runtime.kubernetes.resources is absent.
func DefaultResources() map[claim.Role]RoleResources {
	return maps.Clone(defaultResources)
}

// PodConfig is `runtime.kubernetes.pod`, what the operator adds to every pod Legion runs, the image
// probe's included, in the API's own types: variables and volume mounts for the agent's container,
// the volumes they mount (each a Secret, a ConfigMap, or a projection of ServiceAccount tokens,
// Secrets, and ConfigMaps), and the ServiceAccount the pods run as. It passes through as written:
// the loader refuses a shape no pod could carry, the Sandbox runtime a name or path of Legion's own
// or the worker image's (sandbox.CheckPod), and the API server the rest when it creates the
// image probe's pod, which carries it, at boot.
type PodConfig struct {
	Env            map[string]string
	Volumes        []corev1.Volume
	VolumeMounts   []corev1.VolumeMount
	ServiceAccount string
}

const (
	kubernetesKey      = "runtime.kubernetes"
	podKey             = kubernetesKey + ".pod"
	defaultIssueVolume = "20Gi"
	// poolLabel is the node label that selects the Legion pool. The runtime sets it on every pod,
	// and the cluster's admission policy requires its value.
	poolLabel = "legion.dev/pool"
)

// imageDigestRef is a reference pinned by digest: the daemon must know exactly which image it
// probed.
var imageDigestRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// AgentSecretsConfig is `runtime.kubernetes.agent_secrets`: the broker's base
// URL, the email of the person the daemon's own machine logins are approved by (the daemon runs its
// own login at boot, on a background context, and logs the confirmation code once; no file ever
// carries a launcher credential, since Login wins and holds it only in process memory), the
// audience of the projected token every pod carries for it, and that token's lifetime. The audience
// defaults to the broker's own (`agent-secrets`) and the lifetime to 3600 s, the most the cluster's
// admission policy admits for a Legion worker token; the API server issues none under 600.
type AgentSecretsConfig struct {
	URL                string
	Operator           string
	Audience           string
	TokenExpirySeconds int
}

const (
	agentSecretsKey             = kubernetesKey + ".agent_secrets"
	defaultAgentSecretsAudience = "agent-secrets"
	defaultTokenExpirySeconds   = 3600
	minTokenExpirySeconds       = 600
	maxTokenExpirySeconds       = 3600
)

// readAgentSecrets reads `runtime.kubernetes.agent_secrets`: nil when absent, every member checked.
func readAgentSecrets(value *yaml.Node) (*AgentSecretsConfig, error) {
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", agentSecretsKey)
	}
	fields, err := members(value, agentSecretsKey, "url", "operator", "audience", "token_expiry_seconds")
	if err != nil {
		return nil, err
	}
	block := &AgentSecretsConfig{Audience: defaultAgentSecretsAudience, TokenExpirySeconds: defaultTokenExpirySeconds}
	if block.URL, err = requiredString(fields["url"], agentSecretsKey+".url", ""); err != nil {
		return nil, err
	}
	u, err := url.Parse(block.URL)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("%s.url must be an absolute URL with no path, query, fragment or user; got %q", agentSecretsKey, block.URL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("%s.url must use https unless the host is a loopback address; got %q", agentSecretsKey, block.URL)
	}
	block.URL = strings.TrimSuffix(block.URL, "/")
	if block.Operator, err = requiredString(fields["operator"], agentSecretsKey+".operator", ""); err != nil {
		return nil, err
	}
	if audience, err := optionalString(fields["audience"], agentSecretsKey+".audience"); err != nil {
		return nil, err
	} else if audience != "" {
		block.Audience = audience
	}
	if fields["token_expiry_seconds"] != nil {
		expiry, err := readInt(fields["token_expiry_seconds"], agentSecretsKey+".token_expiry_seconds")
		if err != nil {
			return nil, err
		}
		if expiry == nil || *expiry < minTokenExpirySeconds || *expiry > maxTokenExpirySeconds {
			return nil, fmt.Errorf("%s.token_expiry_seconds must be between %d and %d", agentSecretsKey, minTokenExpirySeconds, maxTokenExpirySeconds)
		}
		block.TokenExpirySeconds = *expiry
	}
	return block, nil
}

// isLoopbackHost is localhost, with or without the root's trailing dot, or a loopback IP address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isUnspecifiedHost is the unspecified address (`0.0.0.0`, `::`): no host at all, which only a
// listener may bind.
func isUnspecifiedHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// readAdvertiseHost is `advertise_host`: an IP address or a DNS-1123 name, the host part alone of
// every pod's `--connect tcp://<host>:<port>`. The daemon adds the worker stream's port itself
// (shimAddress, internal/daemon/daemon.go), and the shim parses that address as a URL, so anything
// else (a scheme, a port, brackets, a path, a user, a query, a space or an underscore) would build
// an address no pod's shim dials.
func readAdvertiseHost(value *yaml.Node, key string) (*string, error) {
	read, err := readNonEmptyString(value, key)
	if err != nil || read == nil {
		return read, err
	}
	if host := *read; net.ParseIP(host) == nil && len(validation.IsDNS1123Subdomain(strings.ToLower(host))) != 0 {
		return nil, fmt.Errorf("%s must be an IP address or a DNS name, with no scheme, port, path or brackets: the daemon adds worker_stream_port itself (%s)", key, host)
	}
	return read, nil
}

// readKubernetes reads the `runtime.kubernetes` block, refusing any member it does not model and
// any value a pod could not run with. A key an earlier shape of the block had (tree_volume, the
// TypeScript daemon's role_profiles and gateway) is known so that its refusal names what replaced
// it, which is what `legion start --check-config` shows the operator to edit. What depends on keys
// outside the block is checked once the whole file is read (checkKubernetesKeys), and the
// reservations the file leaves out are filled when the block is settled (resolveKubernetes).
func readKubernetes(value *yaml.Node) (*Kubernetes, error) {
	if value.Kind != yaml.MappingNode {
		return nil, errors.New("runtime.kubernetes must be a mapping")
	}
	fields, err := members(value, kubernetesKey, "namespace", "image", "storage_class", "issue_volume", "tree_volume",
		"kubeconfig", "context", "scheduling", "resources", "gateway", "pod", "session_store", "session_dsn_secret",
		"role_profiles", "agent_secrets")
	if err != nil {
		return nil, err
	}
	if fields["tree_volume"] != nil {
		return nil, errors.New("runtime.kubernetes.tree_volume is now issue_volume: one volume per issue")
	}
	if fields["role_profiles"] != nil {
		return nil, errors.New("unknown key runtime.kubernetes.role_profiles: a role's reservation is runtime.kubernetes.resources.<role>, a cpu and a memory each both request and limit, and a role absent there takes the daemon's default")
	}
	if fields["gateway"] != nil {
		return nil, errors.New("runtime.kubernetes.gateway was removed (LEGION-270): configure pods with runtime.kubernetes.pod (docs/kubernetes.md, Operator configuration)")
	}
	block := &Kubernetes{IssueVolume: defaultIssueVolume}
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
		": the cluster has no default storage class for the issue volumes"); err != nil {
		return nil, err
	}
	issueVolume, err := readQuantity(fields["issue_volume"], kubernetesKey+".issue_volume")
	if err != nil {
		return nil, err
	}
	if issueVolume != "" {
		block.IssueVolume = issueVolume
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
	if block.Pod, err = readPod(fields["pod"]); err != nil {
		return nil, err
	}
	if block.AgentSecrets, err = readAgentSecrets(fields["agent_secrets"]); err != nil {
		return nil, err
	}
	return block, nil
}

// ReadPodFile reads a `runtime.kubernetes.pod` block kept in a file of its own, such as an
// operator's own route (deploy/kubernetes/operator-route/pod.yml), with every check the
// loader makes of the block inside a legion.yaml.
func ReadPodFile(path string) (PodConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PodConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return PodConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(document.Content) != 1 {
		return PodConfig{}, fmt.Errorf("%s holds no %s mapping", path, podKey)
	}
	pod, err := readPod(document.Content[0])
	if err != nil {
		return PodConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return pod, nil
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
// credential's value (runtime.HoldsSecretValue, as a spec's Env is judged in
// runtime.ValidateSpawnSpec): a credential travels in a Secret, never as a plain value in the pod's
// spec.
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
		case runtime.HoldsSecretValue(name):
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

// minTokenExpiry is the shortest lifetime, in seconds, the API server issues a projected
// ServiceAccount token for.
const minTokenExpiry = 600

// readTokenProjection is a projected ServiceAccount token: the file it is written to, and the
// audience and lifetime it is issued for when set (the API server's own audience and default
// lifetime when not). An audience still holding a ${…} placeholder is refused: nothing in Legion
// expands one, so the pod would carry a token for the placeholder's text and every call made with
// it would be refused.
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
	if strings.Contains(token.Audience, "${") {
		return nil, fmt.Errorf("%s.audience %q is an unfilled placeholder: put the audience the token is for in its place", key, token.Audience)
	}
	expiry, err := readInt(fields["expiration_seconds"], key+".expiration_seconds")
	switch {
	case err != nil:
		return nil, err
	case expiry != nil && *expiry <= 0:
		return nil, fmt.Errorf("%s.expiration_seconds must be a positive integer", key)
	case expiry != nil && *expiry < minTokenExpiry:
		return nil, fmt.Errorf("%s.expiration_seconds must be at least %d: the API server issues no projected token for less than 10 minutes", key, minTokenExpiry)
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

// resolveKubernetes settles `runtime: kubernetes`: the keys outside the block every pod needs, and
// the block, its kubeconfig resolved against the file's directory and every role's reservation
// filled — the file's cpu, memory, ephemeral storage and its request where it set them, the
// default for each field it left out — so the daemon translates seven complete reservations and
// never one with a side missing. A role whose settled ephemeral-storage request exceeds its limit
// is refused naming both, since the API server would refuse the pod (the file set one of the two,
// or both: the defaults never exceed). The file's own block (file.Kubernetes) keeps only what the
// file set, which resolveControllerLaunch reads to tell a controller the operator sized from one
// the default sized.
func resolveKubernetes(file fileConfig, configDir string, cfg *Config) error {
	if err := checkKubernetesKeys(file); err != nil {
		return err
	}
	block := *file.Kubernetes
	if block.Kubeconfig != "" {
		block.Kubeconfig = underConfig(block.Kubeconfig, configDir)
	}
	block.Resources = DefaultResources()
	for role, set := range file.Kubernetes.Resources {
		settled := block.Resources[role]
		block.Resources[role] = RoleResources{
			CPU: cmp.Or(set.CPU, settled.CPU), Memory: cmp.Or(set.Memory, settled.Memory),
			EphemeralStorage:        cmp.Or(set.EphemeralStorage, settled.EphemeralStorage),
			EphemeralStorageRequest: cmp.Or(set.EphemeralStorageRequest, settled.EphemeralStorageRequest),
		}
	}
	for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
		settled := block.Resources[role]
		request, limit := resource.MustParse(settled.EphemeralStorageRequest), resource.MustParse(settled.EphemeralStorage)
		if request.Cmp(limit) > 0 {
			return fmt.Errorf("%s.resources.%s.ephemeral_storage_request %s exceeds ephemeral_storage %s", kubernetesKey, role, settled.EphemeralStorageRequest, settled.EphemeralStorage)
		}
	}
	cfg.Runtime = Runtime{Name: "kubernetes", Kubernetes: &block}
	return nil
}

// checkSessionStore reads `session_store` and `session_dsn_secret` only to refuse what the Go
// runtime does not do: a pod's session lives on its issue's volume until Stage 6 adds the database.
func checkSessionStore(store, dsnSecret *yaml.Node) error {
	name, err := readString(store, kubernetesKey+".session_store")
	if err != nil {
		return err
	}
	switch {
	case name == nil || *name == "pvc":
	case *name == "postgres":
		return errors.New("runtime.kubernetes.session_store postgres is not supported until Stage 6: a pod's session lives on its issue's volume (pvc)")
	default:
		return errors.New("runtime.kubernetes.session_store must be 'pvc' or 'postgres'")
	}
	if dsnSecret != nil {
		return errors.New("runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is pvc; remove it")
	}
	return nil
}

// readQuantity reads a positive Kubernetes quantity as the file wrote it, "" when unset. A bare
// YAML number (`issue_volume: 20`) is its text. Zero is refused with the negatives: it is no size,
// and the runtime refuses a zero issue volume.
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
			return nil, fmt.Errorf("%s must not set %s: the runtime selects the Legion pool itself, and the cluster's admission policy requires its value", key, poolLabel)
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

// readTolerations reads the scheduling tolerations: each a key, operator Equal or Exists, an
// effect, and a value only under Equal.
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

// readResources is a mapping of role to that role's reservation, the file's own: each workflow
// role, and the controller, whose pod a daemon under `controller: daemon` launches
// (resolveControllerLaunch refuses its key otherwise). A role's entry sets any of `cpu`, `memory`,
// `ephemeral_storage` (the container's limit on the node's disk) and `ephemeral_storage_request`
// (what the scheduler reserves of it); what it leaves out is "" here and the default once settled
// (resolveKubernetes, which also holds the request to the limit). The keys of the earlier shape, a
// role's `requests` and `limits` mappings, are known so that each is refused naming the shape that
// replaced it.
func readResources(value *yaml.Node) (map[claim.Role]RoleResources, error) {
	const key = kubernetesKey + ".resources"
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of role to its cpu and memory reservation", key)
	}
	roles := make([]string, 0, len(claim.Roles)+1)
	for _, role := range claim.Roles {
		roles = append(roles, string(role))
	}
	roles = append(roles, string(claim.RoleController))
	var resources map[claim.Role]RoleResources
	for i := 0; i+1 < len(value.Content); i += 2 {
		name, entry := value.Content[i].Value, value.Content[i+1]
		role := claim.Role(name)
		if !claim.IsRole(role) && role != claim.RoleController {
			return nil, fmt.Errorf("%s key %q must be a role (%s)", key, name, strings.Join(roles, ", "))
		}
		if _, exists := resources[role]; exists {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		field := key + "." + name
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping of cpu and memory", field)
		}
		fields, err := members(entry, field, "cpu", "memory", "ephemeral_storage", "ephemeral_storage_request", "requests", "limits")
		if err != nil {
			return nil, err
		}
		for _, gone := range []string{"requests", "limits"} {
			if fields[gone] != nil {
				return nil, fmt.Errorf("%s.%s is gone: a role's reservation is one cpu and one memory, each both its request and its limit", field, gone)
			}
		}
		var read RoleResources
		if read.CPU, err = readQuantity(fields["cpu"], field+".cpu"); err != nil {
			return nil, err
		}
		if read.Memory, err = readQuantity(fields["memory"], field+".memory"); err != nil {
			return nil, err
		}
		if read.EphemeralStorage, err = readQuantity(fields["ephemeral_storage"], field+".ephemeral_storage"); err != nil {
			return nil, err
		}
		if read.EphemeralStorageRequest, err = readQuantity(fields["ephemeral_storage_request"], field+".ephemeral_storage_request"); err != nil {
			return nil, err
		}
		if resources == nil {
			resources = map[claim.Role]RoleResources{}
		}
		resources[role] = read
	}
	return resources, nil
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
		{"operator_token_file", file.OperatorTokenFile == nil, "legion claims presents this token, as legion controller start does under controller: operator"},
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
// dials the worker stream at tcp://<host>:<worker_stream_port>, the host being AdvertiseHost when
// the file sets one and Bind otherwise, so that host may be neither loopback nor the unspecified
// address. Bind beside an AdvertiseHost is only where the daemon listens, so it may be the
// unspecified address, but not loopback, where no pod reaches the listener at all. Every Legion URL
// a pod is handed - daemon_url, envoy_url, dispatch_url, and each nats_urls entry - must name
// neither: a loopback host is, in a pod, the pod itself, and the unspecified address is no host at
// all.
func checkPodReachable(cfg Config) error {
	key, host, why := "bind", cfg.Bind, "bind the daemon host's own address when runtime is kubernetes"
	if cfg.AdvertiseHost != "" {
		if isLoopbackHost(cfg.Bind) {
			return fmt.Errorf("bind %s is loopback, where no pod reaches the worker stream, whatever advertise_host names; bind 0.0.0.0 or the daemon host's own address when runtime is kubernetes", cfg.Bind)
		}
		key, host, why = "advertise_host", cfg.AdvertiseHost, "name the host pods reach it at when runtime is kubernetes"
	}
	if isLoopbackHost(host) || isUnspecifiedHost(host) {
		return fmt.Errorf("%s %s is not an address a pod can reach, and every pod's shim dials the worker stream at tcp://%s; %s",
			key, host, net.JoinHostPort(host, strconv.Itoa(cfg.WorkerStreamPort)), why)
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
		var why string
		switch {
		case isLoopbackHost(host):
			why = "names a loopback host, which in a pod is the pod itself"
		case isUnspecifiedHost(host):
			why = "names the unspecified address, which is no host a pod can dial"
		default:
			continue
		}
		return fmt.Errorf("%s %s %s; name the host pods reach it at when runtime is kubernetes", address.key, address.value, why)
	}
	return nil
}
