package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// Gateway is how a pod reaches the model: the model gateway's base URL, and the service account
// whose projected token (for Audience, rotated within TokenExpiry) is the pod's key there.
type Gateway struct {
	URL, Audience, ServiceAccount string
	TokenExpiry                   time.Duration
}

const (
	kubernetesKey     = "runtime.kubernetes"
	defaultTreeVolume = "20Gi"
	// minTokenExpiry is the shortest projected service account token Kubernetes issues.
	minTokenExpiry = 600
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
		"kubeconfig", "context", "scheduling", "resources", "gateway", "session_store", "session_dsn_secret",
		"role_profiles")
	if err != nil {
		return nil, err
	}
	if fields["role_profiles"] != nil {
		return nil, errors.New("unknown key runtime.kubernetes.role_profiles: each role's requests and limits are set under runtime.kubernetes.resources, and a role absent there gets none")
	}
	block := &Kubernetes{}

	namespace, err := readNonEmptyString(fields["namespace"], kubernetesKey+".namespace")
	if err != nil {
		return nil, err
	}
	if namespace == nil {
		return nil, errors.New("runtime.kubernetes.namespace is required")
	}
	block.Namespace = *namespace

	image, err := readString(fields["image"], kubernetesKey+".image")
	if err != nil {
		return nil, err
	}
	if image == nil {
		return nil, errors.New("runtime.kubernetes.image is required")
	}
	if !imageDigestRef.MatchString(*image) {
		return nil, errors.New("runtime.kubernetes.image must be pinned by digest (@sha256:…)")
	}
	block.Image = *image

	storageClass, err := readNonEmptyString(fields["storage_class"], kubernetesKey+".storage_class")
	if err != nil {
		return nil, err
	}
	if storageClass == nil {
		return nil, errors.New("runtime.kubernetes.storage_class is required: the cluster has no default storage class for the tree volume")
	}
	block.StorageClass = *storageClass

	treeVolume, err := readQuantity(fields["tree_volume"], kubernetesKey+".tree_volume")
	if err != nil {
		return nil, err
	}
	block.TreeVolume = defaultTreeVolume
	if treeVolume != "" {
		block.TreeVolume = treeVolume
	}

	kubeconfig, err := readNonEmptyString(fields["kubeconfig"], kubernetesKey+".kubeconfig")
	if err != nil {
		return nil, err
	}
	kubeContext, err := readNonEmptyString(fields["context"], kubernetesKey+".context")
	if err != nil {
		return nil, err
	}
	if kubeContext != nil && kubeconfig == nil {
		return nil, errors.New("runtime.kubernetes.context names a kubeconfig context, so it requires runtime.kubernetes.kubeconfig")
	}
	if kubeconfig != nil {
		block.Kubeconfig = *kubeconfig
	}
	if kubeContext != nil {
		block.Context = *kubeContext
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
	if fields["gateway"] == nil {
		return nil, errors.New("runtime.kubernetes.gateway is required: a pod reaches the model only through the gateway, with its projected service account token")
	}
	if block.Gateway, err = readGateway(fields["gateway"]); err != nil {
		return nil, err
	}
	return block, nil
}

// members returns a mapping's members by name, refusing a name it does not know or names twice. A
// member whose value is null is left out, as an unset key.
func members(value *yaml.Node, key string, known ...string) (map[string]*yaml.Node, error) {
	found := map[string]*yaml.Node{}
	seen := map[string]bool{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		name, member := value.Content[i].Value, value.Content[i+1]
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("unknown key %s.%s", key, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		seen[name] = true
		if member.Tag != "!!null" {
			found[name] = member
		}
	}
	return found, nil
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
// YAML number (`tree_volume: 20`) is its text. Zero is refused with the negatives: the runtime
// reads a zero tree volume as its default.
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
	priorityClass, err := readNonEmptyString(fields["priority_class"], key+".priority_class")
	if err != nil {
		return Scheduling{}, err
	}
	if priorityClass != nil {
		scheduling.PriorityClass = *priorityClass
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

// readGateway reads the model gateway, every member required: a pod has no other way to a model.
func readGateway(value *yaml.Node) (Gateway, error) {
	const key = kubernetesKey + ".gateway"
	if value.Kind != yaml.MappingNode {
		return Gateway{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "url", "audience", "service_account", "token_expiry_seconds")
	if err != nil {
		return Gateway{}, err
	}
	var gateway Gateway
	url, err := readString(fields["url"], key+".url")
	if err != nil {
		return Gateway{}, err
	}
	if url == nil {
		return Gateway{}, fmt.Errorf("%s.url is required", key)
	}
	if gateway.URL, err = baseURL(*url, key+".url"); err != nil {
		return Gateway{}, err
	}
	for _, part := range []struct {
		name   string
		target *string
	}{
		{"audience", &gateway.Audience}, {"service_account", &gateway.ServiceAccount},
	} {
		read, err := readNonEmptyString(fields[part.name], key+"."+part.name)
		if err != nil {
			return Gateway{}, err
		}
		if read == nil {
			return Gateway{}, fmt.Errorf("%s.%s is required", key, part.name)
		}
		*part.target = *read
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
	case *expiry > maxTimerSeconds:
		return Gateway{}, fmt.Errorf("%s must be at most %d", expiryKey, maxTimerSeconds)
	}
	gateway.TokenExpiry = time.Duration(*expiry) * time.Second
	return gateway, nil
}

// checkKubernetesKeys refuses a file whose keys outside the block leave a pod unable to run, or
// set something no pod uses. Every URL a pod dials must be one it can reach, so none of them may
// fall back to a loopback default; the host's Oh My Pi and its provider keys have no counterpart
// in a pod.
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
	for _, rule := range []struct{ key, why string }{
		{"omp_invocation", "every pod runs the worker image's Oh My Pi"},
		{"omp_launch_prefix", "every pod runs the worker image's Oh My Pi"},
		{"provider_keys", "a pod reaches the model through runtime.kubernetes.gateway, and no provider key reaches a pod"},
	} {
		if file.set[rule.key] {
			return fmt.Errorf("%s is not used when runtime is kubernetes: %s; remove %s", rule.key, rule.why, rule.key)
		}
	}
	return nil
}
