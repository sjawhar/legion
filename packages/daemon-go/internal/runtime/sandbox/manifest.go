package sandbox

import (
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The pod's containers.
const (
	initContainer = "workspace-init"
	mainContainer = "worker"
)

// The pod's volumes.
const (
	bootVolume      = "boot"
	provisionVolume = "provision"
	stateVolume     = "state"
	tempVolume      = "tmp"
	configVolume    = "config"
	gatewayVolume   = "gateway"
)

// maxArgBytes is Linux's MAX_ARG_STRLEN, the largest single argv string exec accepts, counting
// its terminating NUL. The system prompt is one argument, so it is the one that can reach it.
const maxArgBytes = 131072

// The Legion pool every pod runs on: its taint, tolerated, and its label, selected
// (agent-c components/legion: the `legion` NodePool). The gVisor RuntimeClass's own selector also
// matches another pool, so the pool label is what keeps a pod on Legion's nodes.
const (
	poolKey   = "legion.dev/pool"
	poolValue = "legion"
	gvisor    = "gvisor"
)

// The pod runs as the image's legion user.
const podUser = 1000

// dispatchTokenKey is the claim Secret's key for the Dispatch bearer, which the runtime writes
// into every claim's Secret when Dispatch is configured, beside the boot and provisioning tokens.
const dispatchTokenKey = "DISPATCH_TOKEN"

// runtimeOwned are the main container's variables the runtime sets itself: exactly the names
// mainEnvironment sets (TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime), which the
// shared validator refuses in a spec's Env and as a secret's pointer.
var runtimeOwned = map[string]bool{
	"LEGION_DAEMON_API": true, "LEGION_TREE": true, "LEGION_ISSUE": true, "LEGION_ROLE": true,
	"LEGION_GENERATION": true, "LEGION_PROJECT": true, "LEGION_DAEMON_URL": true,
	"LEGION_STATE_DIR": true, "LEGION_WORKSPACE": true, "ENVOY_NATS_URL": true, "ENVOY_URL": true,
	"DISPATCH_URL": true, "LEGION_MODEL_GATEWAY_URL": true, "LEGION_GH_PATH": true,
	"LEGION_GIT_PATH": true, "LEGION_JJ_PATH": true, "LEGION_CREDENTIAL_HELPER": true, "PATH": true,
	"PI_SHELL_PREFIX":     true,
	"GIT_TERMINAL_PROMPT": true, "XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true,
	"XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "POD_UID": true, "LEGION_BOOT_TOKEN_FILE": true,
	"DISPATCH_TOKEN_FILE": true,
}

// launch is one relaunch's inputs, checked and resolved before anything touches the cluster.
type launch struct {
	spec runtime.SpawnSpec
	name string
	// repo is the repository workspace-init provisions, and workspace the issue's workspace it
	// provisions on the tree volume (workspace.Location under TreeRoot).
	repo, workspace string
	// secrets are the claim Secret's keys beside the boot and provisioning tokens, each reaching
	// the main container as a `<NAME>_FILE` pointer: the spec's, and the Dispatch bearer when
	// Dispatch is configured.
	secrets map[string]string
	// root is the tree's root claim, whose Sandbox owns the tree volume; isRoot is spec.Claim
	// being it.
	root   claim.Token
	isRoot bool
	// prompt is the one --append-system-prompt value.
	prompt string
	// resumeFile is the recorded session in the main container's path; "" for a Spawn.
	resumeFile string
}

// prepare checks spec and resolves everything a launch needs from it, reading the prompt files on
// the daemon's disk, so a launch that cannot be honoured is refused before any API call: the
// shared refusal (runtime.ValidateSpawnSpec), then the sandbox's own. A pod provisions its
// workspace from a repository, so a spec with none is refused, and so is a secret named for a key
// the runtime writes into the claim's Secret itself.
func (r *Runtime) prepare(spec runtime.SpawnSpec) (launch, error) {
	if err := runtime.ValidateSpawnSpec(spec, runtimeOwned); err != nil {
		return launch{}, err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("sandbox launch %s: "+format, append([]any{spec.Claim}, args...)...)
	}
	for _, name := range sortedKeys(spec.Secrets) {
		if name == bootTokenKey || name == provisionTokenKey || name == dispatchTokenKey {
			return launch{}, refuse("secret %s is a key the runtime writes itself", name)
		}
	}
	if spec.Repository == "" {
		return launch{}, refuse("no repository: a pod's init container provisions the issue's workspace from one")
	}
	working, err := workspace.Location(TreeRoot, spec.Repository, spec.Issue)
	if err != nil {
		return launch{}, refuse("%v", err)
	}
	root, err := claim.NewToken(spec.Project, spec.Tree, claim.RoleArchitect)
	if err != nil {
		return launch{}, refuse("the tree's root claim: %v", err)
	}
	prompt, err := systemPrompt(spec.Prompt)
	if err != nil {
		return launch{}, refuse("%v", err)
	}
	secrets := maps.Clone(spec.Secrets)
	if r.dispatchToken != "" {
		if secrets == nil {
			secrets = map[string]string{}
		}
		secrets[dispatchTokenKey] = r.dispatchToken
	}
	l := launch{
		spec: spec, name: SandboxName(spec.Claim), repo: spec.Repository, workspace: working.Dir, secrets: secrets,
		root: root, isRoot: root == spec.Claim, prompt: prompt,
	}
	if spec.ResumeSessionFile != "" {
		if _, err := initSessionPath(spec.ResumeSessionFile); err != nil {
			return launch{}, refuse("%v", err)
		}
		l.resumeFile = spec.ResumeSessionFile
	}
	argv := l.agentArgv(r.agent)
	for i, arg := range argv {
		if len(arg)+1 > maxArgBytes {
			what := fmt.Sprintf("agent argument #%d", i)
			if i > 0 && strings.HasPrefix(argv[i-1], "--") {
				what = "the value of " + argv[i-1]
			}
			return launch{}, refuse("%s is %d bytes; a single argv string may not exceed %d with its NUL (Linux MAX_ARG_STRLEN)",
				what, len(arg), maxArgBytes)
		}
	}
	return l, nil
}

// systemPrompt is the one --append-system-prompt value, the same text tmux's pane shell builds
// (runtime/tmux/prompt.go): the role prompt files concatenated, then the addressing sentence, then
// the deployment instructions, separated by a blank line, each file's trailing newlines dropped as
// `$(cat …)` drops them. A pod cannot read the daemon's files, so the text is inlined here.
func systemPrompt(parts runtime.PromptParts) (string, error) {
	var role strings.Builder
	for _, path := range parts.RolePromptPaths {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		role.Write(text)
	}
	fragments := []string{strings.TrimRight(role.String(), "\n")}
	if parts.Addressing != "" {
		fragments = append(fragments, parts.Addressing)
	}
	if parts.DeploymentInstructionsPath != "" {
		text, err := os.ReadFile(parts.DeploymentInstructionsPath)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		fragments = append(fragments, strings.TrimRight(string(text), "\n"))
	}
	return strings.Join(fragments, "\n\n"), nil
}

// initSessionPath is where the init container sees a main-container session file: the volume's
// sessions directory is mounted at Oh My Pi's sessions directory in the main container and sits
// under TreeRoot in the init container. A session anywhere else is not on the volume, so no pod
// can resume it (k8s-manifests.ts:168-181).
func initSessionPath(file string) (string, error) {
	rest, ok := strings.CutPrefix(file, ompSessionsDir+"/")
	if !ok || rest == "" || filepath.Clean(rest) != rest || strings.HasPrefix(rest, "../") {
		return "", fmt.Errorf("recorded OMP session file %s is not under %s, the only directory a pod keeps sessions in; it cannot be resumed on this runtime",
			file, ompSessionsDir)
	}
	return TreeRoot + "/" + SessionsSubPath + "/" + rest, nil
}

// agentArgv is the command the shim runs: the agent, `--resume` on the recorded session when
// resuming, then RPC mode and the system prompt.
func (l launch) agentArgv(agent []string) []string {
	argv := slices.Clone(agent)
	if l.resumeFile != "" {
		argv = append(argv, "--resume="+l.resumeFile)
	}
	return append(argv, "--mode", "rpc", "--append-system-prompt", l.prompt)
}

// labels are every object of the claim's labels (decision 1).
func (r *Runtime) labels(spec runtime.SpawnSpec) map[string]string {
	return map[string]string{
		labelProject: r.project,
		labelTree:    labelValue(spec.Tree),
		labelIssue:   labelValue(spec.Issue),
		labelRole:    string(spec.Role),
	}
}

// sandboxManifest is the Sandbox a relaunch creates when none exists: Suspended, so no pod starts
// before the claim's Secret is written, with the root's tree volume template on the root.
func (r *Runtime) sandboxManifest(l launch, affinity bool) sandbox {
	s := sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: l.name, Namespace: r.namespace, Labels: r.labels(l.spec)},
		Spec:       sandboxSpec{PodTemplate: r.podTemplate(l, affinity), OperatingMode: modeSuspended},
	}
	if l.isRoot {
		storageClass := r.storageClass
		s.Spec.VolumeClaimTemplates = []volumeClaimTemplate{{
			Metadata: volumeClaimMetadata{Name: treeVolume, Labels: r.labels(l.spec)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &storageClass,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: r.treeVolume},
				},
			},
		}}
	}
	return s
}

// podTemplate is the pod a launch runs (decisions 7 and 10). Every tree pod mounts the tree volume
// by its claim's name, the root included: the controller replaces the root's `tree` volume with the
// same claim from its template, so root and workers read alike. The pod runs as the gateway's
// ServiceAccount, and its worker container alone mounts the gateway token (C6).
//
// affinity is whether another pod of the tree is scheduled right now. The tree volume is a
// single-node EBS volume every tree pod mounts, so a pod placed on another node would fail to
// attach it; with no other pod scheduled, any node will do. The controller applies a template only
// to the next pod it creates, so the template is rebuilt for every relaunch.
//
// Every tree pod also refuses a node that holds a pod of another tree (Stage 4b decision 2): the
// pool's floor sizes a node for one tree, and pods carry no requests, since under required
// colocation the first pod placed decides the node and a request on a later one would strand it.
// The selector is the tree label present and not this tree's, so a pod with no tree label, the
// image probe's, never counts.
func (r *Runtime) podTemplate(l launch, affinity bool) podTemplate {
	resources := r.resources[l.spec.Role]
	legion := r.tools.Legion
	helper := "!" + legion + " credential"
	gateway, gatewayMount := gatewayTokenVolume(r.gateway)
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: new(int64(math.Ceil(r.terminationGrace.Seconds()))),
		AutomountServiceAccountToken:  new(false),
		ServiceAccountName:            r.gateway.ServiceAccount,
		EnableServiceLinks:            new(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
		},
		RuntimeClassName:  new(gvisor),
		NodeSelector:      r.nodeSelector(),
		Tolerations:       r.tolerations(),
		PriorityClassName: r.scheduling.PriorityClass,
		Volumes:           append(r.volumes(l), gateway),
		InitContainers: []corev1.Container{{
			Name:  initContainer,
			Image: r.image,
			Command: []string{
				legion, "workspace-init", "--issue", l.spec.Issue, "--repo", l.repo, "--root", TreeRoot,
				"--credential-helper", helper,
			},
			Env:        r.initEnvironment(l),
			WorkingDir: TreeRoot,
			VolumeMounts: []corev1.VolumeMount{
				{Name: treeVolume, MountPath: TreeRoot},
				{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
				{Name: tempVolume, MountPath: initTempDir},
				{Name: configVolume, MountPath: xdgConfigHome},
			},
			Resources:       resources,
			SecurityContext: restrictedContainer(),
		}},
		Containers: []corev1.Container{{
			Name:  mainContainer,
			Image: r.image,
			Command: append([]string{
				legion, "worker-shim", "--connect", r.streamURL, "--boot-token-file", BootDir + "/" + bootTokenKey, "--",
			}, l.agentArgv(r.agent)...),
			Env:        r.mainEnvironment(l, helper),
			WorkingDir: l.workspace,
			VolumeMounts: []corev1.VolumeMount{
				{Name: treeVolume, MountPath: TreeRoot},
				{Name: treeVolume, MountPath: ompSessionsDir, SubPath: SessionsSubPath},
				{Name: bootVolume, MountPath: BootDir, ReadOnly: true},
				{Name: stateVolume, MountPath: StateDir},
				{Name: configVolume, MountPath: xdgConfigHome},
				gatewayMount,
			},
			Resources:       resources,
			SecurityContext: restrictedContainer(),
		}},
	}
	tree := labelValue(l.spec.Tree)
	spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: labelTree, Operator: metav1.LabelSelectorOpExists},
				{Key: labelTree, Operator: metav1.LabelSelectorOpNotIn, Values: []string{tree}},
			}},
			TopologyKey: corev1.LabelHostname,
		}},
	}}
	if affinity {
		spec.Affinity.PodAffinity = &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelProject: r.project, labelTree: tree}},
				TopologyKey:   corev1.LabelHostname,
			}},
		}
	}
	for i := range spec.InitContainers {
		kubeletLiteral(&spec.InitContainers[i])
	}
	for i := range spec.Containers {
		kubeletLiteral(&spec.Containers[i])
	}
	return podTemplate{
		Metadata: podMetadata{
			Labels:      r.labels(l.spec),
			Annotations: map[string]string{"karpenter.sh/do-not-disrupt": "true"},
		},
		Spec: spec,
	}
}

// kubeletLiteral escapes a container's command and env values against the kubelet's expansion,
// in which `$(NAME)` is another variable's value and `$$` a literal `$`: every `$` is doubled, so
// the process receives the text as written, the inlined system prompt and the operator's
// instructions included, as a tmux pane does.
func kubeletLiteral(c *corev1.Container) {
	for i := range c.Command {
		c.Command[i] = strings.ReplaceAll(c.Command[i], "$", "$$")
	}
	for i := range c.Env {
		c.Env[i].Value = strings.ReplaceAll(c.Env[i].Value, "$", "$$")
	}
}

// volumes are the tree volume, the claim's Secret projected twice (its boot half for the main
// container, its provisioning token for the init container alone), and three in-memory
// directories: the main container's state directory, the init container's TMPDIR, and the XDG
// config home both containers share.
func (r *Runtime) volumes(l launch) []corev1.Volume {
	boot := []corev1.KeyToPath{{Key: bootTokenKey, Path: bootTokenKey}}
	for _, name := range sortedKeys(l.secrets) {
		boot = append(boot, corev1.KeyToPath{Key: name, Path: name})
	}
	memory := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
	return []corev1.Volume{
		{Name: treeVolume, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: TreeClaimName(l.root)},
		}},
		{Name: bootVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName(l.name), Items: boot, DefaultMode: new(int32(0o440)),
		}}},
		{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName(l.name), Items: []corev1.KeyToPath{{Key: provisionTokenKey, Path: provisionTokenKey}},
			DefaultMode: new(int32(0o440)),
		}}},
		{Name: stateVolume, VolumeSource: memory},
		{Name: tempVolume, VolumeSource: memory},
		{Name: configVolume, VolumeSource: memory},
	}
}

// The XDG base directories, at the standard offsets from the image's HOME, the same in both
// containers. The config home is the pod's shared in-memory volume: jj keeps a repository's
// `--repo` configuration under $XDG_CONFIG_HOME/jj/repos/, so what workspace-init sets there
// (git.abandon-unreachable-commits false, no repository identity) is what the agent's jj reads.
// It starts empty in every pod, so nothing an agent wrote reaches the init container's jj.
const (
	xdgConfigHome = podHome + "/.config"
	xdgCacheHome  = podHome + "/.cache"
	xdgDataHome   = podHome + "/.local/share"
	xdgStateHome  = podHome + "/.local/state"
)

func xdgEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "XDG_CONFIG_HOME", Value: xdgConfigHome},
		{Name: "XDG_CACHE_HOME", Value: xdgCacheHome},
		{Name: "XDG_DATA_HOME", Value: xdgDataHome},
		{Name: "XDG_STATE_HOME", Value: xdgStateHome},
	}
}

// initEnvironment is workspace-init's contract (research runtime §2.3). Its PATH is the image's
// alone, naming no directory on the tree volume, so the git and jj it resolves from PATH are
// never ones an agent put there; it carries no tool-path variables. A resume names the recorded
// session the command must find on the volume, and a relaunch after the volume was lost names the
// ref the recreated workspace is recovered from; both are workspace-init's alone, never the
// agent's.
func (r *Runtime) initEnvironment(l launch) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "PATH", Value: imagePath},
		{Name: "TMPDIR", Value: initTempDir},
		{Name: "LEGION_PROVISION_TOKEN_FILE", Value: ProvisionDir + "/" + provisionTokenKey},
		{Name: "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS", Value: strconv.FormatInt(r.initWaitSeconds(), 10)},
	}
	if l.resumeFile != "" {
		path, _ := initSessionPath(l.resumeFile) // checked by prepare
		env = append(env, corev1.EnvVar{Name: "LEGION_RESUME_SESSION_FILE", Value: path})
	}
	if l.spec.WorkspaceRecoveredFrom != "" {
		env = append(env, corev1.EnvVar{Name: "LEGION_WORKSPACE_RECOVERED_FROM", Value: l.spec.WorkspaceRecoveredFrom})
	}
	return append(env, xdgEnvironment()...)
}

// initWaitSeconds bounds a wait on another pod's workspace-init: ceil(boot timeout) × (intervals
// + 1), the whole time the daemon tolerates a pod that is alive but unregistered, plus one
// interval, so no wait gives up while the daemon would still allow the pod it waits on
// (runtime-kubernetes.ts:390-399).
func (r *Runtime) initWaitSeconds() int64 {
	return int64(math.Ceil(r.bootTimeout.Seconds())) * int64(r.bootIntervals+1)
}

// mainEnvironment is the pane contract with a pod's values (decision 10): the variables every
// tmux pane is told (runtime/tmux/spawn.go, panePairs) and the model gateway's URL, which the
// image's Oh My Pi profile routes its provider to, then the spec's own, then one `<NAME>_FILE`
// pointer per secret into the boot projection. POD_UID is the pod's own incarnation, from the
// downward API.
func (r *Runtime) mainEnvironment(l launch, credentialHelper string) []corev1.EnvVar {
	spec := l.spec
	var env []corev1.EnvVar
	add := func(name, value string) { env = append(env, corev1.EnvVar{Name: name, Value: value}) }
	add("LEGION_DAEMON_API", "go")
	add("LEGION_TREE", spec.Tree)
	add("LEGION_ISSUE", spec.Issue)
	add("LEGION_ROLE", string(spec.Role))
	add("LEGION_GENERATION", strconv.FormatUint(spec.Generation, 10))
	add("LEGION_PROJECT", spec.Project)
	if r.daemonURL != "" {
		add("LEGION_DAEMON_URL", r.daemonURL)
	}
	add("LEGION_STATE_DIR", StateDir)
	add("LEGION_WORKSPACE", l.workspace)
	if len(r.natsURLs) > 0 {
		add("ENVOY_NATS_URL", strings.Join(r.natsURLs, ","))
	}
	if r.envoyURL != "" {
		add("ENVOY_URL", r.envoyURL)
	}
	if r.dispatchURL != "" {
		add("DISPATCH_URL", r.dispatchURL)
	}
	add("LEGION_MODEL_GATEWAY_URL", r.gateway.URL)
	add("LEGION_GH_PATH", r.tools.GH)
	add("LEGION_GIT_PATH", r.tools.Git)
	add("LEGION_JJ_PATH", r.tools.JJ)
	add("LEGION_CREDENTIAL_HELPER", credentialHelper)
	legionDir := filepath.Dir(r.tools.Legion)
	add("PATH", workerBin+":"+legionDir+":"+imagePath)
	add("PI_SHELL_PREFIX", shellprefix.For(workerBin, legionDir))
	add("GIT_TERMINAL_PROMPT", "0")
	env = append(env, xdgEnvironment()...)
	env = append(env, corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
	}})
	for _, name := range sortedKeys(spec.Env) {
		add(name, spec.Env[name])
	}
	add("LEGION_BOOT_TOKEN_FILE", BootDir+"/"+bootTokenKey)
	for _, name := range sortedKeys(l.secrets) {
		add(name+"_FILE", BootDir+"/"+name)
	}
	return env
}

// nodeSelector is the Legion pool's label and the configured selector, which configure keeps off
// the pool's key.
func (r *Runtime) nodeSelector() map[string]string {
	selector := map[string]string{poolKey: poolValue}
	for key, value := range r.scheduling.NodeSelector {
		selector[key] = value
	}
	return selector
}

// tolerations are the Legion pool's taint, tolerated, then the configured ones.
func (r *Runtime) tolerations() []corev1.Toleration {
	return append([]corev1.Toleration{{
		Key: poolKey, Operator: corev1.TolerationOpEqual, Value: poolValue, Effect: corev1.TaintEffectNoSchedule,
	}}, r.scheduling.Tolerations...)
}

// restrictedContainer is the Pod Security "restricted" container context
// (k8s-manifests.ts:44-48).
func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
