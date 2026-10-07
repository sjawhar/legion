package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podKind is which of the runtime's two pods a Sandbox runs: an issue pod (issuePod), which runs
// every workflow role of one issue on its tree's volume and provisions the issue's workspace, or the
// project controller's pod (controllerPod, `controller: daemon`), which runs the controller alone on
// a volume of its own and provisions nothing. prepare decides a launch's kind once, from its claim,
// and a stored Sandbox's kind is the one its labels name (podKindOf). Building a pod asks the
// launch's kind for every facet the two build differently; supervision — relaunch's check of the
// running pod, the launchers, Probe, Suspend and the address records — asks a kind only its roles.
type podKind interface {
	// roles are the pod's launcher roles, one container each.
	roles() []claim.Role
	// claimToken is the claim of a launcher of role, one of roles, on a pod of this kind of project,
	// issue being the issue key its Secrets carry (secretAnnotations).
	claimToken(project, issue string, role claim.Role) (claim.Token, error)
	// prepare resolves what the kind decides of a launch, its workspace and the volume its pod
	// mounts, refusing a spec it cannot honour.
	prepare(l *launch) error
	// labels are the pod's resource labels, on its Sandbox, pod template, volume claim template and
	// Secrets, which name its kind (podKindOf).
	labels(project string, l launch) map[string]string
	// secretAnnotations are the annotations each of the pod's Secrets carries.
	secretAnnotations(l launch) map[string]string
	// initContainers are the pod's init containers, and initVolumes the volumes only they mount,
	// besides the pod's own volume and config home.
	initContainers(r *Runtime, l launch) []corev1.Container
	initVolumes(l launch) []corev1.Volume
	// agentEnv is what each agent is told of its kind, mainEnvironment's one block of its own.
	agentEnv(r *Runtime, l launch, credentialHelper string) []corev1.EnvVar
	// colocate is whether a new pod must share a node with another pod scheduled now, and affinity
	// the placement that follows.
	colocate(r *Runtime, l launch) bool
	affinity(r *Runtime, l launch, colocate bool) *corev1.Affinity
	// readyNewPod readies what a new pod needs before relaunch writes its launcher Secrets and sets
	// its Sandbox, s, running, and returns the release of what it holds meanwhile, which relaunch
	// calls once it is done with the new pod. On an error it holds nothing: it has given back
	// whatever it took, and returns no release.
	readyNewPod(ctx context.Context, r *Runtime, l *launch, s *sandbox) (func(), error)
}

// podKindOf is the kind a Sandbox's labels name: an issue pod's carry legion.dev/issue, the project
// controller's legion.dev/role=controller and no tree. Any other labelling names no kind and is
// refused, so a pod is never judged against a role list its labels do not state.
func podKindOf(labels map[string]string) (podKind, error) {
	switch {
	case labels[labelIssue] != "":
		return issuePod{}, nil
	case labels[labelRole] == string(claim.RoleController) && labels[labelTree] == "":
		return controllerPod{}, nil
	}
	return nil, fmt.Errorf("its labels name neither an issue nor the project controller (%s=%q, %s=%q, %s=%q)",
		labelIssue, labels[labelIssue], labelTree, labels[labelTree], labelRole, labels[labelRole])
}

// issuePod is an issue's pod: every workflow role of the issue, one launcher each, working in the
// issue's workspace on its tree's volume, which the tree's root Sandbox owns. Two init containers
// provision that workspace from the issue's repository, and the pod is placed beside its tree's
// other pods and off every other tree's node.
type issuePod struct{}

func (issuePod) roles() []claim.Role { return claim.Roles }

func (issuePod) claimToken(project, issue string, role claim.Role) (claim.Token, error) {
	return claim.NewToken(project, issue, role)
}

// prepare refuses a spec with no repository, which the workspace is provisioned from, and resolves
// the workspace's place on the tree volume and the tree's root claim, whose Sandbox owns that
// volume; the root's own launch owns it. A resume expects the tree volume to hold what it left.
func (issuePod) prepare(l *launch) error {
	spec := l.spec
	if spec.Repository.IsZero() {
		return errors.New("no repository: a pod's init container provisions the issue's workspace from one")
	}
	working, err := workspace.Location(TreeRoot, spec.Repository, spec.Issue)
	if err != nil {
		return err
	}
	root, err := claim.NewToken(spec.Project, spec.Tree, claim.RoleArchitect)
	if err != nil {
		return fmt.Errorf("the tree's root claim: %w", err)
	}
	l.workspace, l.volume, l.ownsVolume = working.Dir, root, claim.IsTreeArchitect(spec.Role, spec.Issue, spec.Tree)
	l.expectTreeVolume = l.resumeFile != ""
	return nil
}

// labels carry the pod's tree and issue and no role: a role belongs to a process locator, not to a
// shared issue pod.
func (issuePod) labels(project string, l launch) map[string]string {
	return map[string]string{
		labelProject: project,
		labelTree:    labelValue(l.spec.Tree),
		labelIssue:   labelValue(l.spec.Issue),
	}
}

// secretAnnotations carry the exact issue key (annotationIssue), from which the launcher resolver
// derives a role's claim (claimToken): the issue label holds the key as a label value, which past
// 63 characters is not the key.
func (issuePod) secretAnnotations(l launch) map[string]string {
	return map[string]string{annotationIssue: l.spec.Issue}
}

// initContainers are workspace-fetch and workspace-init, so the provisioning token never shares a
// process with anything a tree agent can write (Stage 4b Task 4b.6b): workspace-fetch mounts the
// provisioning Secret, its own TMPDIR, and the feed, and clones the repository from GitHub into the
// feed; workspace-init mounts the tree volume, the feed read-only, and the config home, and does all
// the tree volume's work from the feed, with no credential. Each takes the resources of the role
// whose launch creates the pod.
func (issuePod) initContainers(r *Runtime, l launch) []corev1.Container {
	legion, resources := r.tools.Legion, r.resources[l.spec.Role]
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

// initVolumes are the provisioning material only the init containers mount: the provisioning
// Secret, workspace-fetch's in-memory TMPDIR, and the feed it fills for workspace-init.
func (issuePod) initVolumes(l launch) []corev1.Volume {
	return []corev1.Volume{
		{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName(l.name), Items: []corev1.KeyToPath{{Key: provisionTokenKey, Path: provisionTokenKey}},
			DefaultMode: new(int32(0o440)),
		}}},
		{Name: feedVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: tempVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
}

// agentEnv tells each agent its tree, issue and workspace, the tool paths and credential helper of
// its checkout, and uv's directories on the tree volume.
func (issuePod) agentEnv(r *Runtime, l launch, credentialHelper string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "LEGION_TREE", Value: l.spec.Tree},
		{Name: "LEGION_ISSUE", Value: l.spec.Issue},
		{Name: "LEGION_WORKSPACE", Value: l.workspace},
		{Name: "LEGION_GH_PATH", Value: r.tools.GH},
		{Name: "LEGION_GIT_PATH", Value: r.tools.Git},
		{Name: "LEGION_JJ_PATH", Value: r.tools.JJ},
		{Name: "LEGION_CREDENTIAL_HELPER", Value: credentialHelper},
		{Name: "UV_PYTHON_INSTALL_DIR", Value: uvPythonDir(l.spec.Issue)},
		{Name: "UV_CACHE_DIR", Value: uvCacheDir},
		{Name: "UV_LINK_MODE", Value: uvLinkMode},
	}
}

// colocate is whether another pod of the tree is scheduled now (treePodScheduled).
func (issuePod) colocate(r *Runtime, l launch) bool { return r.treePodScheduled(l) }

// affinity refuses a node that holds a pod of another tree (Stage 4b decision 2): the pool's floor
// sizes a node for one tree, and pods carry no requests, since under required colocation the first
// pod placed decides the node and a request on a later one would strand it. The selector is the
// tree label present and not this tree's, so a pod with no tree label, the image probe's or the
// controller's, never counts. With colocate it also requires the scheduled pod's node: the tree
// volume is a single-node EBS volume every tree pod mounts, so a pod placed on another node would
// fail to attach it; with no other pod scheduled, any node will do.
func (issuePod) affinity(r *Runtime, l launch, colocate bool) *corev1.Affinity {
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

// readyNewPod takes the tree's launch turn and readies the pod under it (provision), then hands the
// turn's release back, so relaunch holds the turn until its new pod is in the store, where the next
// relaunch's wait sees it. A launch that cannot be readied gives the turn back itself and returns
// the error: the next launch of the tree is never left waiting on a turn nobody holds.
func (pod issuePod) readyNewPod(ctx context.Context, r *Runtime, l *launch, s *sandbox) (func(), error) {
	unlock, err := r.lockTree(ctx, l.spec.Tree)
	if err != nil {
		return nil, fmt.Errorf("take its tree's launch turn: %w", err)
	}
	if err := pod.provision(ctx, r, l, s); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// provision is what a new issue pod needs before it starts, run under the tree's launch turn: it
// waits until no other pod of the tree is initializing (awaitTreeInitialized), reads whether
// workspace-init must find the tree volume holding retained sessions, lists the tree's removable
// workspaces, mints the provisioning token for the repository's owner, and writes it to the pod's
// init-only provisioning Secret.
func (issuePod) provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error {
	if err := r.awaitTreeInitialized(ctx, *l); err != nil {
		return fmt.Errorf("wait for its tree's other pods to finish initializing: %w", err)
	}
	if !l.expectTreeVolume && l.spec.WorkspaceRecoveredFrom == "" {
		sessions, err := r.store.TreeHasSessions(ctx, r.project, l.spec.Tree)
		if err != nil {
			return fmt.Errorf("read its tree's retained sessions: %w", err)
		}
		l.expectTreeVolume = sessions
	}
	if r.removable != nil {
		// Computed now, under the tree's launch turn, after every other pod of the tree has finished
		// initializing: the latest moment before this pod's own manifest is written, so a sibling
		// that became live in the time this launch spent waiting is not judged by a list that was
		// already stale when this launch started (dispatch://LEGION-583). Only a launch that creates
		// the issue pod reaches here: a role started in a running issue pod runs no init container,
		// so it has no list to carry. That guarantee is this relaunch's own, though: a pod the
		// Sandbox controller recreates on its own (eviction, node drain, a hand deletion) runs
		// workspace-init from this same pod template, list included, without ever passing through
		// here again. workspace-init closes that case itself: it refuses to act on this list unless
		// its own fetch (fetchStartedFile) started no later than the notAfter stamped below, this
		// launch's time plus initWaitSeconds (workspace_init.go's own removableWorkspacesEnv doc
		// comment), since a recreated pod's fetch starts hours later, whatever its own clone then
		// takes.
		//
		// removableWorkspaces states the candidate rule from the daemon's own claim store;
		// withoutLiveTreePods below checks the pod itself, a second guarantee on different evidence:
		// it cannot tell a claim whose fail persisted StateFailed despite its own suspendProcess
		// erroring from one truly gone, so a candidate can still have a live, non-terminal pod of
		// this tree right now.
		candidates, err := r.removable(ctx, l.spec.Tree, l.spec.Issue)
		if err != nil {
			return fmt.Errorf("compute its tree's removable workspaces: %w", err)
		}
		notAfter := r.now().Add(time.Duration(r.initWaitSeconds()) * time.Second)
		if err := l.setRemovable(r.withoutLiveTreePods(*l, candidates), notAfter); err != nil {
			return fmt.Errorf("build its removable-workspaces list: %w", err)
		}
	}
	owner := l.spec.Repository.Owner()
	minting, cancel := call(ctx)
	token, err := r.tokens.Token(minting, owner)
	cancel()
	if err != nil {
		return fmt.Errorf("mint the provisioning token for %s: %w", owner, err)
	}
	if err := r.upsertSecret(ctx, corev1.Secret{
		ObjectMeta: r.secretMeta(s, *l, secretName(s.Name)),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{provisionTokenKey: []byte(token)},
	}); err != nil {
		return fmt.Errorf("write its provisioning secret: %w", err)
	}
	return nil
}

// controllerPod is the project controller's pod (`controller: daemon`): the controller's launcher
// alone, working in the root of a volume of its own, which its Sandbox owns, so a relaunch resumes
// its session. It belongs to no tree and holds no repository credential: it provisions no
// workspace, takes no tree's launch turn, is placed on any Legion node, and its one init container
// makes its sessions directory and holds a resume to its session.
type controllerPod struct{}

func (controllerPod) roles() []claim.Role { return []claim.Role{claim.RoleController} }

func (controllerPod) claimToken(project, _ string, _ claim.Role) (claim.Token, error) {
	return claim.ControllerToken(project), nil
}

// prepare works in the root of the controller's own volume.
func (controllerPod) prepare(l *launch) error {
	l.workspace, l.volume, l.ownsVolume = TreeRoot, l.spec.Claim, true
	return nil
}

// labels carry the controller role and no tree or issue: a tree pod's anti-affinity refuses a node
// holding a pod with any other tree label, and the controller belongs to no tree.
func (controllerPod) labels(project string, _ launch) map[string]string {
	return map[string]string{labelProject: project, labelRole: string(claim.RoleController)}
}

// secretAnnotations are none: the controller's claim is the project's (claimToken).
func (controllerPod) secretAnnotations(launch) map[string]string { return nil }

// initContainers are one workspace-init, which mounts the controller's volume alone: `workspace-init
// controller` provisions nothing and waits on no lock. It is told the image's PATH and, when the
// pod is created to resume the controller, the session it must find, as the volume holds it, so a
// lost volume brings up a fresh controller before any launcher starts. It takes the controller's
// resources.
func (controllerPod) initContainers(r *Runtime, l launch) []corev1.Container {
	env := []corev1.EnvVar{{Name: "PATH", Value: imagePath}}
	if l.resumeFile != "" {
		onVolume := TreeRoot + "/" + SessionsSubPath + strings.TrimPrefix(l.resumeFile, ompSessionsDir)
		env = append(env, corev1.EnvVar{Name: "LEGION_RESUME_SESSION_FILE", Value: onVolume})
	}
	return []corev1.Container{{
		Name:            initContainer,
		Image:           r.image,
		Command:         []string{r.tools.Legion, "workspace-init", "controller", "--root", TreeRoot},
		Env:             env,
		WorkingDir:      TreeRoot,
		VolumeMounts:    []corev1.VolumeMount{{Name: treeVolume, MountPath: TreeRoot}},
		Resources:       r.resources[l.spec.Role],
		SecurityContext: restrictedContainer(),
	}}
}

// initVolumes are none: its init container mounts only its volume.
func (controllerPod) initVolumes(launch) []corev1.Volume { return nil }

// agentEnv tells the agent LEGION_CONTROLLER=1, its pane marker, and nothing a tree agent alone
// needs: no tree, issue or workspace, no tool paths or credential helper of a checkout, and none of
// uv's directories on a tree volume.
func (controllerPod) agentEnv(*Runtime, launch, string) []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "LEGION_CONTROLLER", Value: "1"}}
}

// colocate is never: the controller shares no volume with any other pod.
func (controllerPod) colocate(*Runtime, launch) bool { return false }

// affinity is none: any Legion node will do.
func (controllerPod) affinity(*Runtime, launch, bool) *corev1.Affinity { return nil }

// readyNewPod readies nothing: the controller's pod belongs to no tree and provisions nothing.
func (controllerPod) readyNewPod(context.Context, *Runtime, *launch, *sandbox) (func(), error) {
	return func() {}, nil
}
