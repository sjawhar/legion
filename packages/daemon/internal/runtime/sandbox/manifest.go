package sandbox

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The pod's init containers: workspace-fetch, the one that holds the provisioning token and
// mounts nothing a tree agent can write; and workspace-init, which works on the tree volume
// without it. Each of the six role containers is named for its role.
const (
	fetchContainer = "workspace-fetch"
	initContainer  = "workspace-init"
)

// The pod's volumes.
const (
	provisionVolume = "provision"
	feedVolume      = "feed"
	stateVolume     = "state"
	tempVolume      = "tmp"
	configVolume    = "config"
	providersVolume = "providers"
)

// The agent-secrets volumes: agentSecretsTokenVolume alone carries the projected token for the
// broker's audience, and agentSecretsKeyVolume the memory-backed directory the client keeps its
// key and enrollment id in. Neither exists when the runtime enrolls no pod.
const (
	agentSecretsKeyVolume   = "agent-secrets-key"
	agentSecretsTokenVolume = "agent-secrets-token"
)

// maxArgBytes is Linux's MAX_ARG_STRLEN, the largest single argv string exec accepts, counting
// its terminating NUL. The system prompt is one argument, so it is the one that can reach it.
const maxArgBytes = 131072

// The Legion pool every pod runs on: its taint, tolerated, and its label, selected
// (the cluster's `legion` NodePool). The gVisor RuntimeClass's own selector also
// matches another pool, so the pool label is what keeps a pod on Legion's nodes.
const (
	poolKey   = "legion.dev/pool"
	poolValue = "legion"
	gvisor    = "gvisor"
)

// The pod runs as the image's legion user.
const podUser = 1000

// runtimeOwned are the main container's variables the runtime sets itself: exactly the names
// mainEnvironment sets for a tree agent or the controller
// (TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime), which the shared validator refuses
// in a spec's Env and as a secret's pointer, and CheckPod in the operator's pod. The image probe's
// container sets none of its own.
var runtimeOwned = map[string]bool{
	"LEGION_TREE": true, "LEGION_ISSUE": true, "LEGION_ROLE": true, "LEGION_CONTROLLER": true,
	"LEGION_GENERATION": true, "LEGION_PROJECT": true, "LEGION_DAEMON_URL": true,
	"LEGION_STATE_DIR": true, "LEGION_WORKSPACE": true, "ENVOY_NATS_URL": true, "ENVOY_URL": true,
	"DISPATCH_URL": true, "LEGION_GH_PATH": true,
	"LEGION_GIT_PATH": true, "LEGION_JJ_PATH": true, "LEGION_CREDENTIAL_HELPER": true, "PATH": true,
	"PI_SHELL_PREFIX": true, "GIT_TERMINAL_PROMPT": true, "LEGION_GRANT_FILE": true,
	"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
	"POD_UID": true, bootTokenKey + "_FILE": true, dispatchTokenKey + "_FILE": true,
	"AGENT_SECRETS_URL": true, "AGENT_SECRETS_KEY_DIR": true,
	"UV_PYTHON_INSTALL_DIR": true, "UV_CACHE_DIR": true, "UV_LINK_MODE": true,
}

// legionVolumeNames are the volumes Legion puts in a pod, an issue pod's, the controller's or the
// probe's, whose names the operator's volumes may not take: the shared ones and each launcher
// role's private ones. The agent-secrets volumes are reserved whether or not this deployment
// enrolls: an operator's pod may never claim them. A key volume is a workflow role's alone: the
// controller never enrolls (enrolledWith), so no pod carries one of its.
func legionVolumeNames() []string {
	names := []string{
		treeVolume, provisionVolume, feedVolume, tempVolume, configVolume, providersVolume, agentSecretsTokenVolume,
	}
	for _, role := range launcherRoles {
		names = append(names, roleVolume("launcher", role), roleVolume("private", role), roleVolume(stateVolume, role))
	}
	for _, role := range claim.Roles {
		names = append(names, roleVolume(agentSecretsKeyVolume, role))
	}
	slices.Sort(names)
	return names
}

// legionMountPaths are where Legion mounts a volume in the containers the operator's mounts join,
// each role container and the image probe's: an operator's mount may be neither at, under, nor
// above one. AgentSecretsKeyDir and AgentSecretsTokenDir are reserved whether or not this
// deployment enrolls.
func legionMountPaths() []string {
	paths := []string{
		TreeRoot, ompSessionsDir, LauncherDir, LauncherPrivateDir, StateDir, xdgConfigHome, ProvidersDir,
		AgentSecretsKeyDir, AgentSecretsTokenDir,
	}
	slices.Sort(paths)
	return paths
}

// launch is one relaunch's inputs, checked and resolved before anything touches the cluster.
type launch struct {
	spec runtime.SpawnSpec
	name string
	// roles are the pod's launcher roles, one container each: what its labels name (podRoles), every
	// workflow role for an issue pod and the controller alone for the project controller's.
	roles []claim.Role
	// turn keys the pod's launch turn (lockPod): the issue for an issue pod, the Sandbox's name for
	// the controller's, which no issue key can be.
	turn string
	// controller is the project controller's launch (`controller: daemon`): a one-role pod on a
	// volume of its own, which provisions no workspace and is told nothing of a tree, an issue or
	// GitHub.
	controller bool
	// workspace is the launchers' working directory: the issue's workspace workspace-init
	// provisions on the tree volume (workspace.Location under TreeRoot), or the controller's own
	// volume's root, TreeRoot.
	workspace string
	// secrets are the claim's launch credentials, each reaching the agent as a `<NAME>_FILE`
	// pointer into its generation's private directory: the boot token, the spec's but the providers
	// Secret's own (Options.ProvidersSecrets, which the runtime points at the providers mount
	// whatever the spec carries), and the Dispatch bearer when Dispatch is configured.
	secrets map[string]string
	// root is the claim whose Sandbox owns the volume the pod mounts: the tree's root claim, or the
	// controller's own; isRoot is spec's claim being it.
	root   claim.Token
	isRoot bool
	// prompt is the one --append-system-prompt value.
	prompt string
	// resumeFile is checked by the role launcher before it starts the child; an issue pod's shared
	// init checks tree storage, not a triggering role's transcript, and other stored sessions can
	// also require an existing tree. initResumeFile is the controller's resumeFile as its one init
	// container mounts its volume: a pod created to resume the controller holds the resume to its
	// session before any launcher starts, so a lost volume brings up a fresh controller.
	resumeFile       string
	initResumeFile   string
	expectTreeVolume bool
	// removableWorkspacesJSON is the tree's removable-workspace candidates (Options.Removable),
	// JSON-encoded together with their expiry, one object; "" when there are none. Not set by
	// prepare: relaunch calls setRemovable with what Options.Removable returns, last, under the
	// tree's launch turn — prepare runs long before that turn is even requested, so a list this
	// early could already be stale by the time a pod's manifest is actually written.
	removableWorkspacesJSON string
}

// setRemovable JSON-encodes candidates and notAfter into l.removableWorkspacesJSON as
// runtime.RemovableWorkspacesPayload, called from relaunch with Options.Removable's result and
// the launch time plus initWaitSeconds, once the tree's launch turn is held. The encoding cannot
// fail (plain strings and a time.Time), but initEnvironment has no error to return, so a refusal
// here is relaunch's own to surface before it ever patches the Sandbox.
func (l *launch) setRemovable(candidates []runtime.RemovableWorkspace, notAfter time.Time) error {
	if len(candidates) == 0 {
		return nil
	}
	encoded, err := json.Marshal(runtime.RemovableWorkspacesPayload{NotAfter: notAfter, Workspaces: candidates})
	if err != nil {
		return fmt.Errorf("sandbox launch %s: encode LEGION_REMOVABLE_WORKSPACES: %w", l.spec.Claim, err)
	}
	l.removableWorkspacesJSON = string(encoded)
	return nil
}

// prepare checks spec and resolves everything a launch needs from it, reading the prompt files on
// the daemon's disk, so a launch that cannot be honoured is refused before any API call: the
// shared refusal (runtime.ValidateSpawnSpec), then the sandbox's own. An issue pod provisions its
// workspace from a repository, so a workflow claim's spec with none is refused, and so is a secret
// named for the provisioning token, the one key the runtime writes whose pointer the worker
// container is never told (the boot token's and the Dispatch bearer's pointers are runtime-owned,
// so the shared refusal already covers them). The project controller's launch (`controller:
// daemon`) has no repository and no workspace: its one-role pod works in its own volume's root,
// which its Sandbox owns.
func (r *Runtime) prepare(spec runtime.SpawnSpec) (launch, error) {
	if err := runtime.ValidateSpawnSpec(spec, runtimeOwned); err != nil {
		return launch{}, err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("sandbox launch %s: "+format, append([]any{spec.Claim}, args...)...)
	}
	if _, ok := spec.Secrets[provisionTokenKey]; ok {
		return launch{}, refuse("secret %s is a key the runtime writes itself", provisionTokenKey)
	}
	l := launch{spec: spec, name: SandboxName(spec.Claim)}
	if spec.Role == claim.RoleController {
		l.turn, l.controller, l.workspace, l.root, l.isRoot = l.name, true, TreeRoot, spec.Claim, true
	} else {
		if spec.Repository.IsZero() {
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
		l.turn, l.workspace, l.root, l.isRoot = spec.Issue, working.Dir, root, claim.IsTreeArchitect(spec.Role, spec.Issue, spec.Tree)
	}
	roles, err := podRoles(r.labels(spec))
	if err != nil {
		return launch{}, refuse("its pod: %v", err)
	}
	l.roles = roles
	prompt, err := systemPrompt(spec.Prompt)
	if err != nil {
		return launch{}, refuse("%v", err)
	}
	secrets := maps.Clone(spec.Secrets)
	if secrets == nil {
		secrets = map[string]string{}
	}
	for _, name := range r.providersSecrets {
		delete(secrets, name)
	}
	if r.dispatchToken != "" {
		secrets[dispatchTokenKey] = r.dispatchToken
	}
	l.secrets, l.prompt = secrets, prompt
	if spec.ResumeSessionFile != "" {
		if err = validateSessionPath(spec.ResumeSessionFile); err != nil {
			return launch{}, refuse("%v", err)
		}
		l.resumeFile = spec.ResumeSessionFile
		l.expectTreeVolume = true
		if l.controller {
			l.initResumeFile = TreeRoot + "/" + SessionsSubPath + strings.TrimPrefix(spec.ResumeSessionFile, ompSessionsDir)
		}
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

// validateSessionPath keeps a recorded session on the volume every replacement pod mounts.
func validateSessionPath(file string) error {
	rest, ok := strings.CutPrefix(file, ompSessionsDir+"/")
	if !ok || rest == "" || filepath.Clean(rest) != rest || strings.HasPrefix(rest, "../") {
		return fmt.Errorf("recorded OMP session file %s is not under %s, the only directory a pod keeps sessions in; it cannot be resumed on this runtime",
			file, ompSessionsDir)
	}
	return nil
}

// agentArgv is the command the shim runs: the agent with no extension but the image's Legion
// plugin, `--resume` on the recorded session when resuming, then RPC mode and the system prompt.
// `--no-extensions` stops Oh My Pi discovering extensions, so a repository's .omp/extensions (or an
// `extensions:` setting) cannot register a provider or run in the agent; the plugin loads as the
// one explicit extension, its skills with it.
func (l launch) agentArgv(agent []string) []string {
	argv := append(slices.Clone(agent), "--no-extensions", "--extension", legionPlugin)
	if l.resumeFile != "" {
		argv = append(argv, "--resume="+l.resumeFile)
	}
	return append(argv, "--mode", "rpc", "--append-system-prompt", l.prompt)
}

// labels are a pod's resource labels, which name its launcher roles (podRoles). An issue pod's
// carry its tree and issue and no role: a role belongs to a process locator, not to a shared issue
// pod. The project controller's carry the controller role and no tree or issue: a tree pod's
// anti-affinity refuses a node holding a pod with any other tree label, and the controller belongs
// to no tree.
func (r *Runtime) labels(spec runtime.SpawnSpec) map[string]string {
	if spec.Role == claim.RoleController {
		return map[string]string{labelProject: r.project, labelRole: string(claim.RoleController)}
	}
	return map[string]string{
		labelProject: r.project,
		labelTree:    labelValue(spec.Tree),
		labelIssue:   labelValue(spec.Issue),
	}
}

// sandboxManifest is the Sandbox a relaunch creates when none exists: Suspended, so no pod starts
// before the claim's Secret is written, with the root's tree volume template on the root. Its pod
// template never runs: the relaunch's Running patch replaces it with the template the launch
// computes, affinity and all, before the controller creates a pod.
func (r *Runtime) sandboxManifest(l launch) sandbox {
	s := sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: l.name, Namespace: r.namespace, Labels: r.labels(l.spec)},
		Spec:       sandboxSpec{PodTemplate: r.podTemplate(l, false), OperatingMode: modeSuspended},
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
// same claim from its template, so root and workers read alike. The pod runs as the operator's
// ServiceAccount (the namespace's default when the operator names none), and its launcher
// containers mount the providers Secret's configured keys and the operator's mounts, are told the
// operator's variables, and start Oh My Pi on the pod's baseline (`--pod-safety`,
// internal/podsafety).
//
// Two init containers provision the issue's workspace, so the provisioning token never shares a
// process with anything a tree agent can write (Stage 4b Task 4b.6b): workspace-fetch mounts the
// provisioning Secret, its own TMPDIR, and the feed, and clones the repository from GitHub into
// the feed; workspace-init mounts the tree volume, the feed read-only, and the config home, and
// does all the tree volume's work from the feed, with no credential.
//
// The project controller's pod (`controller: daemon`) mounts its own volume the same way and runs
// its one launcher the same way, but provisions nothing: one init container makes its sessions
// directory and holds a resume to its session (`legion workspace-init controller`), it is placed on
// any Legion node, and it is never enrolled with the secrets broker, since it holds no human-tier
// key.
//
// colocate is whether another pod of the tree is scheduled right now, which decides the pod's
// affinity. The controller applies a template only to the next pod it creates, so the template is
// rebuilt for every relaunch.
func (r *Runtime) podTemplate(l launch, colocate bool) podTemplate {
	_, providersMounts := r.providers()
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: new(int64(math.Ceil(r.terminationGrace.Seconds()))),
		AutomountServiceAccountToken:  new(false),
		ServiceAccountName:            r.pod.ServiceAccount,
		EnableServiceLinks:            new(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
		},
		RuntimeClassName:  new(gvisor),
		NodeSelector:      r.nodeSelector(),
		Tolerations:       r.tolerations(),
		Affinity:          r.affinity(l, colocate),
		PriorityClassName: r.scheduling.PriorityClass,
		Volumes:           r.volumes(l),
		InitContainers:    r.initContainers(l),
		Containers:        r.launcherContainers(l, providersMounts),
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

// initContainers are a launch's init containers: an issue pod's workspace-fetch and
// workspace-init, or the controller's one workspace-init, which mounts its volume alone.
// `workspace-init controller` provisions nothing and waits on no lock: it is told the image's PATH
// and, when the pod is created to resume the controller, the session it must find. Each takes the
// resources of the role whose launch creates the pod.
func (r *Runtime) initContainers(l launch) []corev1.Container {
	legion, resources := r.tools.Legion, r.resources[l.spec.Role]
	if l.controller {
		env := []corev1.EnvVar{{Name: "PATH", Value: imagePath}}
		if l.initResumeFile != "" {
			env = append(env, corev1.EnvVar{Name: "LEGION_RESUME_SESSION_FILE", Value: l.initResumeFile})
		}
		return []corev1.Container{{
			Name:            initContainer,
			Image:           r.image,
			Command:         []string{legion, "workspace-init", "controller", "--root", TreeRoot},
			Env:             env,
			WorkingDir:      TreeRoot,
			VolumeMounts:    []corev1.VolumeMount{{Name: treeVolume, MountPath: TreeRoot}},
			Resources:       resources,
			SecurityContext: restrictedContainer(),
		}}
	}
	return []corev1.Container{{
		Name:       fetchContainer,
		Image:      r.image,
		Command:    []string{legion, "workspace-init", "fetch", "--repo", l.spec.Repository.String(), "--feed", FeedDir},
		Env:        fetchEnvironment(),
		WorkingDir: FeedDir,
		VolumeMounts: []corev1.VolumeMount{
			{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
			{Name: tempVolume, MountPath: initTempDir},
			{Name: feedVolume, MountPath: FeedDir},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}, {
		Name:  initContainer,
		Image: r.image,
		Command: []string{
			legion, "workspace-init", "provision", "--issue", l.spec.Issue, "--repo", l.spec.Repository.String(), "--root", TreeRoot,
			"--credential-helper", "!" + legion + " credential", "--feed", FeedDir,
		},
		Env:        r.initEnvironment(l),
		WorkingDir: TreeRoot,
		VolumeMounts: []corev1.VolumeMount{
			{Name: treeVolume, MountPath: TreeRoot},
			{Name: feedVolume, MountPath: FeedDir, ReadOnly: true},
			{Name: configVolume, MountPath: xdgConfigHome},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}}
}

// launcherContainers are the pod's launcher containers, one per role of l.roles: an issue pod's
// six, the controller's one. Each runs only `legion launcher`; the per-generation worker-shim argv
// and plain environment arrive in the launcher's start command, while values the kubelet must
// resolve (the downward API, the operator's secret refs) are set on every container.
func (r *Runtime) launcherContainers(l launch, providersMounts []corev1.VolumeMount) []corev1.Container {
	resolved, _ := r.launchEnvironment(l)
	containers := make([]corev1.Container, 0, len(l.roles))
	for _, role := range l.roles {
		containers = append(containers, corev1.Container{
			Name:  string(role),
			Image: r.image,
			Command: []string{
				r.tools.Legion, "launcher", connectFlag, r.streamURL, "--token-file", LauncherDir + "/" + LauncherTokenFile,
				"--sandbox", l.name, "--role", string(role), "--private-dir", LauncherPrivateDir,
				"--stop-grace", r.terminationGrace.String(),
			},
			Env:        slices.Clone(resolved),
			WorkingDir: l.workspace,
			VolumeMounts: slices.Concat([]corev1.VolumeMount{
				{Name: treeVolume, MountPath: TreeRoot},
				{Name: treeVolume, MountPath: ompSessionsDir, SubPath: SessionsSubPath},
				{Name: roleVolume("launcher", role), MountPath: LauncherDir, ReadOnly: true},
				{Name: roleVolume("private", role), MountPath: LauncherPrivateDir},
				{Name: roleVolume(stateVolume, role), MountPath: StateDir},
				{Name: configVolume, MountPath: xdgConfigHome},
			}, providersMounts, agentSecretsMounts(r.enrolledWith(role), role), r.pod.VolumeMounts),
			Resources:       r.resources[role],
			SecurityContext: restrictedContainer(),
		})
	}
	return containers
}

// kubeletLiteral escapes a container's command and env values against the kubelet's expansion,
// in which `$(NAME)` is another variable's value and `$$` a literal `$`: every `$` is doubled, so
// the process receives the text as written, the inlined system prompt and the operator's
// instructions included, as a tmux pane does.
func kubeletLiteral(c *corev1.Container) {
	for i := range c.Command {
		c.Command[i] = kubeletEscape(c.Command[i])
	}
	for i := range c.Env {
		c.Env[i].Value = kubeletEscape(c.Env[i].Value)
	}
}

// kubeletEscape is text as a container's command or env value carries it: every `$` doubled.
func kubeletEscape(text string) string {
	return strings.ReplaceAll(text, "$", "$$")
}

// volumes are the pod's volume and jj config home, an issue pod's init-only provisioning material,
// and each launcher role's token projection, private credential directory and state. The
// controller's pod provisions nothing, so it has no provisioning token, feed or TMPDIR.
func (r *Runtime) volumes(l launch) []corev1.Volume {
	providers, _ := r.providers()
	memory := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
	roleVolumes := make([]corev1.Volume, 0, len(l.roles)*3)
	for _, role := range l.roles {
		roleVolumes = append(roleVolumes,
			corev1.Volume{Name: roleVolume("launcher", role), VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: roleSecretName(l.name, role), Items: []corev1.KeyToPath{{Key: LauncherTokenFile, Path: LauncherTokenFile}},
				DefaultMode: new(int32(0o440)),
			}}},
			corev1.Volume{Name: roleVolume("private", role), VolumeSource: memory},
			corev1.Volume{Name: roleVolume(stateVolume, role), VolumeSource: memory},
		)
	}
	tree := corev1.Volume{Name: treeVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: TreeClaimName(l.root)},
	}}
	config := corev1.Volume{Name: configVolume, VolumeSource: memory}
	own := []corev1.Volume{tree, config}
	if !l.controller {
		own = []corev1.Volume{
			tree,
			{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName(l.name), Items: []corev1.KeyToPath{{Key: provisionTokenKey, Path: provisionTokenKey}},
				DefaultMode: new(int32(0o440)),
			}}},
			{Name: feedVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: tempVolume, VolumeSource: memory},
			config,
		}
	}
	return slices.Concat(own, roleVolumes, r.agentSecretsVolumes(l.roles), providers, r.pod.Volumes)
}

// enrolledWith is the secrets broker a launcher of role enrolls its agent with, nil when none:
// every workflow role enrolls when the runtime enrolls pods (runtime.kubernetes.agent_secrets), but
// the controller, which holds no human-tier key. The volumes, a launcher container's mounts, the
// shim's flags (launcherCommand), mainEnvironment and handedAddresses all ask it and read the broker
// from its answer, so the pod a launch builds and the addresses evaluate compares a running role
// with agree, and no caller reads a broker it was not handed.
func (r *Runtime) enrolledWith(role claim.Role) *AgentSecrets {
	if role == claim.RoleController {
		return nil
	}
	return r.agentSecrets
}

// agentSecretsVolumes are, for a pod whose roles enroll (enrolledWith), one shared projected
// ServiceAccount token volume (the admission policy permits one volume per audience) and one
// private memory key directory per enrolled role; none when no role of roles enrolls.
func (r *Runtime) agentSecretsVolumes(roles []claim.Role) []corev1.Volume {
	var volumes []corev1.Volume
	for _, role := range roles {
		broker := r.enrolledWith(role)
		if broker == nil {
			continue
		}
		if volumes == nil {
			expiry := int64(math.Ceil(broker.TokenExpiry.Seconds()))
			volumes = []corev1.Volume{{Name: agentSecretsTokenVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: new(int32(0o440)),
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: broker.Audience, ExpirationSeconds: &expiry, Path: AgentSecretsTokenFile,
				}}},
			}}}}
		}
		volumes = append(volumes, corev1.Volume{Name: roleVolume(agentSecretsKeyVolume, role), VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)},
		}})
	}
	return volumes
}

// agentSecretsMounts are role's launcher container's mounts of the agent-secrets volumes when it
// enrolls with broker, none when broker is nil: the one configured audience projection, shared,
// and the role's own key directory, which keeps its generated private key and enrollment id out of
// the other launcher containers.
func agentSecretsMounts(broker *AgentSecrets, role claim.Role) []corev1.VolumeMount {
	if broker == nil {
		return nil
	}
	return []corev1.VolumeMount{
		{Name: agentSecretsTokenVolume, MountPath: AgentSecretsTokenDir, ReadOnly: true},
		{Name: roleVolume(agentSecretsKeyVolume, role), MountPath: AgentSecretsKeyDir},
	}
}

// providers are the providers Secret's volume and its read-only mount at ProvidersDir: the
// configured keys alone, each a file named for the variable Oh My Pi reads, which is what the shim
// exports into Oh My Pi's environment (--provider-env-dir, shim.ReadProviderEnv), and each
// providers secret (Options.ProvidersSecrets), a file of its own name that the shim skips, since
// the container's `<NAME>_FILE` points at it (providersPointers). Neither without provider keys or
// providers secrets, so a deployment with none needs no such Secret.
func (r *Runtime) providers() ([]corev1.Volume, []corev1.VolumeMount) {
	if !r.mountsProviders() {
		return nil, nil
	}
	var items []corev1.KeyToPath
	for _, variable := range sortedKeys(r.providerKeys) {
		items = append(items, corev1.KeyToPath{Key: r.providerKeys[variable], Path: variable})
	}
	for _, name := range r.providersSecrets {
		items = append(items, corev1.KeyToPath{Key: name, Path: name})
	}
	return []corev1.Volume{{Name: providersVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: ProvidersSecretName(r.project), Items: items, DefaultMode: new(int32(0o440)),
		}}}},
		[]corev1.VolumeMount{{Name: providersVolume, MountPath: ProvidersDir, ReadOnly: true}}
}

// mountsProviders reports whether every pod mounts the providers Secret: some provider key or
// providers secret is configured.
func (r *Runtime) mountsProviders() bool {
	return len(r.providerKeys) > 0 || len(r.providersSecrets) > 0
}

// providersPointers are the `<NAME>_FILE` pointer of each providers secret to its file in the
// providers mount, in the worker's container and the image probe's alike: set by the runtime for
// every pod that mounts the Secret, whatever a spec carries, so the shim never exports the secret.
func (r *Runtime) providersPointers() []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, name := range r.providersSecrets {
		env = append(env, corev1.EnvVar{Name: name + "_FILE", Value: ProvidersDir + "/" + name})
	}
	return env
}

// operatorEnv is the operator's variables for the agent's container, in name order.
func (r *Runtime) operatorEnv() []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, name := range sortedKeys(r.pod.Env) {
		env = append(env, corev1.EnvVar{Name: name, Value: r.pod.Env[name]})
	}
	return env
}

// The XDG base directories, at the standard offsets from the image's HOME, the same in the
// workspace-init and main containers. The config home is the pod's shared in-memory volume: jj
// keeps a repository's `--repo` configuration under $XDG_CONFIG_HOME/jj/repos/, so what
// workspace-init sets there (git.abandon-unreachable-commits false, no repository identity) is
// what the agent's jj reads. It starts empty in every pod, so nothing an agent wrote reaches
// workspace-init's jj.
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

// uv's settings in the worker container, which the pod's environment hands the image's uv.
const (
	// uvPythonRoot holds the Pythons uv installs, one directory per issue (uvPythonDir), and
	// uvCacheDir its cache, both on the tree volume beside the workspaces. A project's .venv, in an
	// issue's workspace on that volume, links to an interpreter in that issue's directory, so every
	// later pod of the issue finds the interpreter and the environment works there as it is; the
	// cache lets every pod of the tree reuse what an earlier one downloaded.
	uvPythonRoot = TreeRoot + "/uv/python"
	uvCacheDir   = TreeRoot + "/uv/cache"
	// uvLinkMode is how uv puts a package from uvCacheDir into a .venv: a copy. With the cache and
	// the .venv on one filesystem uv would otherwise hardlink them, and an edit made in place in one
	// workspace's .venv would change the cache and every other .venv of the tree that installed the
	// package, the shared-inode failure LEGION-198 hit with bun's cache.
	uvLinkMode = "copy"
)

// uvPythonDir is issue's own directory under uvPythonRoot, named by the issue as a DNS label
// (dnsName). uv serializes the installs into a directory with a file lock there, and a gVisor
// pod's lock reaches no other pod (awaitTreeInitialized), so two pods first installing one Python
// into a shared directory at once can each delete the other's interpreter. One directory per issue
// keeps every other issue's pods out; only the pods of one issue share it.
func uvPythonDir(issue string) string {
	return uvPythonRoot + "/" + dnsName(issue, maxNameLength)
}

// fetchEnvironment is `workspace-init fetch`'s: the image's PATH alone, its own TMPDIR, and the
// mounted provisioning token. Its git reads no configuration but its own, so it is told no config
// home.
func fetchEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PATH", Value: imagePath},
		{Name: "TMPDIR", Value: initTempDir},
		{Name: "LEGION_PROVISION_TOKEN_FILE", Value: ProvisionDir + "/" + provisionTokenKey},
	}
}

// initEnvironment is `workspace-init provision`'s contract (research runtime §2.3), the one
// provisioning every role of the issue pod shares. Its PATH is the image's alone, naming no
// directory on the tree volume, so the git and jj it resolves from PATH are never ones an agent put
// there; it carries no tool-path variables, and it is never pointed at the provisioning token. It
// gives shared provisioning its tree-wide storage expectation and never carries one role's session
// path: a missing transcript must not prevent sibling launchers from starting. A relaunch after the
// volume was lost names the ref the recreated workspace is recovered from; both are
// workspace-init's alone, never the agent's. LEGION_ROLE and LEGION_GENERATION are l.spec.Role and
// l.spec.Generation, the launch that creates the pod, read together by workspace-init provision's
// own candidate-rotation seed (cmd/legion/workspace_init.go's rotateCandidates): a generation alone
// does not distinguish each role's own first launch of one issue, all at generation 1 — the copies
// of both in mainEnvironment are each role child's, carried by its launcher's start command, so
// workspace-init needs its own. LEGION_REMOVABLE_WORKSPACES is l.removableWorkspacesJSON, set by
// setRemovable (called from relaunch, after the daemon's candidate list is read, last, under the
// tree's launch turn), one JSON object carrying both the list and notAfter (RFC 3339: the launch
// time plus initWaitSeconds) together, so the two can never arrive apart; absent when the daemon
// found none. notAfter is what bounds how long a pod the Sandbox controller recreates on its own
// may still trust this same list, read by its own fresh workspace-fetch's start time rather than
// wall-clock time at removal (dispatch://LEGION-583, cmd/legion/workspace_init.go's
// removableWorkspacesEnv doc comment).
func (r *Runtime) initEnvironment(l launch) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "PATH", Value: imagePath},
		{Name: "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS", Value: strconv.FormatInt(r.initWaitSeconds(), 10)},
		{Name: "LEGION_ROLE", Value: string(l.spec.Role)},
		{Name: "LEGION_GENERATION", Value: strconv.FormatUint(l.spec.Generation, 10)},
	}
	if l.expectTreeVolume {
		env = append(env, corev1.EnvVar{Name: "LEGION_EXPECT_TREE_VOLUME", Value: "true"})
	}
	if l.spec.WorkspaceRecoveredFrom != "" {
		env = append(env, corev1.EnvVar{Name: "LEGION_WORKSPACE_RECOVERED_FROM", Value: l.spec.WorkspaceRecoveredFrom})
	}
	if l.removableWorkspacesJSON != "" {
		env = append(env, corev1.EnvVar{Name: "LEGION_REMOVABLE_WORKSPACES", Value: l.removableWorkspacesJSON})
	}
	return append(env, xdgEnvironment()...)
}

// initWaitSeconds bounds workspace-init provision's own wait to acquire another pod's lock on the
// shared clone (`flock --timeout`, LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS): ceil(boot timeout) ×
// (intervals + 1). Under gVisor a pod's flock never reaches another pod, so what actually keeps
// two pods from provisioning the shared clone at once is awaitTreeInitialized (relaunch.go): a
// new pod is never created while an existing tree pod is still initializing. lockTree itself
// holds the launch turn only until the new pod is in the store (relaunch.go, awaitNewPod) — well
// before that pod's own init finishes — so this wait is a safety net for whatever can still race
// around that ordering (a pod recreated outside the normal relaunch flow), not a budget this
// package expects to actually exhaust.
func (r *Runtime) initWaitSeconds() int64 {
	return int64(math.Ceil(r.bootTimeout.Seconds())) * int64(r.bootIntervals+1)
}

// ProvisionBound satisfies runtime.Runtime: workspace.FetchTimeout, the fetch's own clone bound,
// plus this same lock-wait budget, for whatever time a provisioning pod can still spend waiting on
// another pod's flock before it even starts its own clone.
func (r *Runtime) ProvisionBound() time.Duration {
	return workspace.FetchTimeout + time.Duration(r.initWaitSeconds())*time.Second
}

// mainEnvironment is the pane contract with a pod's values (decision 10): the variables every
// tmux pane is told (runtime/tmux/spawn.go, panePairs), then the operator's (runtime.kubernetes.pod),
// then the spec's own, then one `<NAME>_FILE` pointer per secret into the generation's private
// launcher directory, then one
// per providers secret into the providers mount. None of
// them repeats another: the runtime refuses a spec naming one of its own (runtimeOwned), and the
// daemon an operator's variable naming one of the runtime's or a spec's. LEGION_GRANT_FILE names
// runtime.GrantFile on the state volume, which is empty at start: the extension makes its
// directory. POD_UID is the pod's own incarnation, from the downward API. The controller is told
// LEGION_CONTROLLER=1, its pane marker, where a tree agent is told its tree and issue, and nothing a
// tree agent alone needs: a workspace, the tool paths and credential helper of a checkout, uv's
// directories on a tree volume, or the secrets broker.
func (r *Runtime) mainEnvironment(l launch, credentialHelper string) []corev1.EnvVar {
	spec := l.spec
	var env []corev1.EnvVar
	add := func(name, value string) { env = append(env, corev1.EnvVar{Name: name, Value: value}) }
	if l.controller {
		add("LEGION_CONTROLLER", "1")
	} else {
		add("LEGION_TREE", spec.Tree)
		add("LEGION_ISSUE", spec.Issue)
	}
	add("LEGION_ROLE", string(spec.Role))
	add("LEGION_GENERATION", strconv.FormatUint(spec.Generation, 10))
	add("LEGION_PROJECT", spec.Project)
	if r.daemonURL != "" {
		add("LEGION_DAEMON_URL", r.daemonURL)
	}
	add("LEGION_STATE_DIR", StateDir)
	if !l.controller {
		add("LEGION_WORKSPACE", l.workspace)
	}
	if len(r.natsURLs) > 0 {
		add("ENVOY_NATS_URL", strings.Join(r.natsURLs, ","))
	}
	if r.envoyURL != "" {
		add("ENVOY_URL", r.envoyURL)
	}
	if r.dispatchURL != "" {
		add("DISPATCH_URL", r.dispatchURL)
	}
	if !l.controller {
		add("LEGION_GH_PATH", r.tools.GH)
		add("LEGION_GIT_PATH", r.tools.Git)
		add("LEGION_JJ_PATH", r.tools.JJ)
		add("LEGION_CREDENTIAL_HELPER", credentialHelper)
	}
	legionDir := filepath.Dir(r.tools.Legion)
	add("PATH", podPath(legionDir))
	add("PI_SHELL_PREFIX", shellprefix.For(workerBin, legionDir))
	add("GIT_TERMINAL_PROMPT", "0")
	add("LEGION_GRANT_FILE", runtime.GrantFile(StateDir, spec.Claim))
	if broker := r.enrolledWith(spec.Role); broker != nil {
		add("AGENT_SECRETS_URL", broker.URL)
		add("AGENT_SECRETS_KEY_DIR", AgentSecretsKeyDir)
	}
	env = append(env, xdgEnvironment()...)
	if !l.controller {
		add("UV_PYTHON_INSTALL_DIR", uvPythonDir(spec.Issue))
		add("UV_CACHE_DIR", uvCacheDir)
		add("UV_LINK_MODE", uvLinkMode)
	}
	env = append(env, corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
	}})
	env = append(env, r.operatorEnv()...)
	for _, name := range sortedKeys(spec.Env) {
		add(name, spec.Env[name])
	}
	// The launcher writes each launch credential into its private directory before the child
	// starts (shimwire.LauncherStart.Files): no kubelet Secret propagation stands in the way of a
	// role started in a running pod.
	for _, name := range sortedKeys(l.secrets) {
		add(name+"_FILE", generationDir(l.spec.Generation)+"/"+name)
	}
	return append(env, r.providersPointers()...)
}

// launchEnvironment is mainEnvironment, with the pod's credential helper, split into its two
// carriers: the variables the kubelet resolves (the downward API, the operator's Secret
// references), set on every role container, and the plain NAME=value pairs the launcher's start
// command carries to each generation's child.
func (r *Runtime) launchEnvironment(l launch) ([]corev1.EnvVar, []string) {
	var resolved []corev1.EnvVar
	var plain []string
	for _, entry := range r.mainEnvironment(l, "!"+r.tools.Legion+" credential") {
		if entry.ValueFrom != nil {
			resolved = append(resolved, entry)
		} else {
			plain = append(plain, entry.Name+"="+entry.Value)
		}
	}
	return resolved, plain
}

// connectFlag is the flag naming the worker stream listener a role's launcher and its worker-shim
// dial.
const connectFlag = "--connect"

// handedAddress is one address the runtime hands every role process it launches from the daemon's
// configuration: name is where the process carries it, the launcher's connectFlag or one of
// mainEnvironment's variables, and value is what a process launched now carries there, "" for a
// variable the runtime leaves unset.
type handedAddress struct{ name, value string }

// handedAddresses are every address a process of role launched now carries: the worker stream
// listener its launcher dials (launcherContainers, fixed when the pod is created), then the
// daemon's API, NATS, Envoy, Dispatch and the secrets broker as the agent's environment names them
// (mainEnvironment, carried by each generation's start command, launcherCommand), the broker only
// for a role that enrolls (enrolledWith). The stream is fixed for the pod's life, so a pod a daemon
// created under another stream dials it until the pod is replaced; the other five are fixed for
// the generation's life, and the Sandbox records them per role (recordAddresses) so a daemon
// restarted under other addresses can tell (evaluate).
// TestHandedAddressesAreEveryAddressAPodCarries keeps this list equal to what those two build.
func (r *Runtime) handedAddresses(role claim.Role) []handedAddress {
	brokerURL := ""
	if broker := r.enrolledWith(role); broker != nil {
		brokerURL = broker.URL
	}
	return []handedAddress{
		{connectFlag, r.streamURL},
		{"LEGION_DAEMON_URL", r.daemonURL},
		{"ENVOY_NATS_URL", strings.Join(r.natsURLs, ",")},
		{"ENVOY_URL", r.envoyURL},
		{"DISPATCH_URL", r.dispatchURL},
		{"AGENT_SECRETS_URL", brokerURL},
	}
}

// podPath is a main container's PATH: worker-bin, then the directory of the `legion` every
// container runs, then the image's PATH, which a container's env PATH replaces, with that directory
// once — in the worker image it is the image PATH's first entry.
func podPath(legionDir string) string {
	entries := []string{workerBin, legionDir}
	for _, entry := range strings.Split(imagePath, ":") {
		if entry != legionDir {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, ":")
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

// affinity is a tree pod's placement. It refuses a node that holds a pod of another tree (Stage 4b
// decision 2): the pool's floor sizes a node for one tree, and pods carry no requests, since under
// required colocation the first pod placed decides the node and a request on a later one would
// strand it. The selector is the tree label present and not this tree's, so a pod with no tree
// label, the image probe's or the controller's, never counts. With colocate — another pod of the
// tree scheduled right now — it also requires that pod's node: the tree volume is a single-node EBS
// volume every tree pod mounts, so a pod placed on another node would fail to attach it; with no
// other pod scheduled, any node will do. The controller's pod has no tree and shares no volume, so
// it has no affinity: any Legion node will do.
func (r *Runtime) affinity(l launch, colocate bool) *corev1.Affinity {
	if l.controller {
		return nil
	}
	tree := labelValue(l.spec.Tree)
	affinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: labelTree, Operator: metav1.LabelSelectorOpExists},
				{Key: labelTree, Operator: metav1.LabelSelectorOpNotIn, Values: []string{tree}},
			}},
			TopologyKey: corev1.LabelHostname,
		}},
	}}
	if colocate {
		affinity.PodAffinity = &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelProject: r.project, labelTree: tree}},
				TopologyKey:   corev1.LabelHostname,
			}},
		}
	}
	return affinity
}

// restrictedContainer is the Pod Security "restricted" container context
// (RESTRICTED_CONTAINER_SECURITY_CONTEXT, k8s-manifests.ts).
func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
