package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podKind is which of the runtime's two pods a Sandbox runs: an issue pod (issuePod), which runs
// every workflow role of one issue on the issue's own volume and provisions the issue's workspace,
// or the project controller's pod (controllerPod, `controller: daemon`), which runs the controller
// alone on a volume of its own and provisions nothing. prepare decides a launch's kind once, from
// its claim, and a stored Sandbox's kind is the one its labels name (podKindOf). Building a pod
// asks the launch's kind for every facet the two build differently; supervision — relaunch's check
// of the running pod, the launchers, Probe, Suspend and the address records — asks a kind only its
// roles.
type podKind interface {
	// roles are the pod's launcher roles, one container each.
	roles() []claim.Role
	// claimToken is the claim of a launcher of role, one of roles, on a pod of this kind of project,
	// issue being the issue key its Secrets carry (secretAnnotations).
	claimToken(project, issue string, role claim.Role) (claim.Token, error)
	// prepare resolves what the kind decides of a launch, its workspace and whether its volume
	// must already hold what it left, refusing a spec it cannot honour.
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
	// provision readies what a new pod needs before relaunch writes its launcher Secrets and sets
	// its Sandbox, s, running, under nothing but the pod's own launch turn: each pod works on a
	// volume of its own, so no pod's provisioning waits on another's.
	provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error
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
// issue's workspace on the issue's own volume, which its Sandbox owns. Two init containers
// provision that workspace from the issue's repository.
type issuePod struct{}

func (issuePod) roles() []claim.Role { return claim.Roles }

func (issuePod) claimToken(project, issue string, role claim.Role) (claim.Token, error) {
	return claim.NewToken(project, issue, role)
}

// prepare refuses a spec with no repository, which the workspace is provisioned from, and resolves
// the workspace's place on the issue's volume. A resume expects the volume to hold what it left.
func (issuePod) prepare(l *launch) error {
	spec := l.spec
	if spec.Repository.IsZero() {
		return errors.New("no repository: a pod's init container provisions the issue's workspace from one")
	}
	working, err := workspace.Location(TreeRoot, spec.Repository, spec.Issue)
	if err != nil {
		return err
	}
	l.workspace = working.Dir
	l.expectVolume = l.resumeFile != ""
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
// process with anything an agent of the issue can write (Stage 4b Task 4b.6b): workspace-fetch
// mounts the provisioning Secret, its own TMPDIR, and the feed, and clones the repository from
// GitHub into the feed; workspace-init mounts the issue's volume, the feed read-only, and the
// config home, and does all the volume's work from the feed, with no credential. Each takes the
// resources of the role whose launch creates the pod.
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
			{Name: issueVolume, MountPath: TreeRoot},
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
// its checkout, and uv's directories on the issue's volume.
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

// provision is what a new issue pod needs before it starts: it reads whether workspace-init must
// find the issue's volume holding retained sessions, mints the provisioning token for the
// repository's owner, and writes it to the pod's init-only provisioning Secret. It waits on no
// other pod: the issue's clone lives on the issue's own volume, which no other pod mounts.
func (issuePod) provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error {
	if !l.expectVolume && l.spec.WorkspaceRecoveredFrom == "" {
		sessions, err := r.store.IssueHasSessions(ctx, r.project, l.spec.Issue)
		if err != nil {
			return fmt.Errorf("read its issue's retained sessions: %w", err)
		}
		l.expectVolume = sessions
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
// workspace, and its one init container makes its sessions directory and holds a resume to its
// session.
type controllerPod struct{}

func (controllerPod) roles() []claim.Role { return []claim.Role{claim.RoleController} }

func (controllerPod) claimToken(project, _ string, _ claim.Role) (claim.Token, error) {
	return claim.ControllerToken(project), nil
}

// prepare works in the root of the controller's own volume.
func (controllerPod) prepare(l *launch) error {
	l.workspace = TreeRoot
	return nil
}

// labels carry the controller role and no tree or issue: the controller belongs to no tree, so no
// tree's cleanup lists its Sandbox (CleanupTree), and the labels name its kind (podKindOf).
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
		VolumeMounts:    []corev1.VolumeMount{{Name: issueVolume, MountPath: TreeRoot}},
		Resources:       r.resources[l.spec.Role],
		SecurityContext: restrictedContainer(),
	}}
}

// initVolumes are none: its init container mounts only its volume.
func (controllerPod) initVolumes(launch) []corev1.Volume { return nil }

// agentEnv tells the agent LEGION_CONTROLLER=1, its pane marker, and nothing a tree agent alone
// needs: no tree, issue or workspace, no tool paths or credential helper of a checkout, and none of
// uv's directories on an issue's volume.
func (controllerPod) agentEnv(*Runtime, launch, string) []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "LEGION_CONTROLLER", Value: "1"}}
}

// provision readies nothing: the controller's pod provisions no workspace and mints no token.
func (controllerPod) provision(context.Context, *Runtime, *launch, *sandbox) error { return nil }
