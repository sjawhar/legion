package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podKind is which of the runtime's two pods a Sandbox runs: an issue pod (issuePod), which runs
// every workflow role of one issue on a disk its own pod owns and provisions the issue's
// workspace, or the project controller's pod (controllerPod, `controller: daemon`), which runs the
// controller alone on the volume its Sandbox owns and provisions nothing. prepare decides a
// launch's kind once, from its claim, and a stored Sandbox's kind is the one its labels name
// (podKindOf). Building a pod asks the launch's kind for every facet the two build differently;
// supervision — relaunch's check of the running pod, the launchers, Probe, Suspend and the
// address records — asks a kind only its roles.
type podKind interface {
	// roles are the pod's launcher roles, each with a launcher token and role Secret of its own.
	roles() []claim.Role
	// claimToken is the claim of a launcher of role, one of roles, on a pod of this kind of project,
	// issue being the issue key its Secrets carry (secretAnnotations).
	claimToken(project, issue string, role claim.Role) (claim.Token, error)
	// prepare resolves what the kind decides of a launch, its working directory above all,
	// refusing a spec it cannot honour.
	prepare(l *launch) error
	// labels are the pod's resource labels, on its Sandbox, pod template and Secrets, which name
	// its kind (podKindOf).
	labels(project string, l launch) map[string]string
	// disk is the pod's one disk, mounted under the fixed name issueVolume: the claim templates the
	// kind's Sandbox owns, which outlive its pods, and the source the pod mounts the disk from.
	// They are returned together so that a template and the claim name that refers to it are
	// decided in one place: the controller's one `issue` template and the claim its Sandbox makes
	// from it, or no template at all and an ephemeral volume of the pod's own for an issue.
	disk(r *Runtime, l launch) (templates []volumeClaimTemplate, source corev1.VolumeSource)
	// secretAnnotations are the annotations each of the pod's Secrets carries.
	secretAnnotations(l launch) map[string]string
	// resources are the reservation every container of the pod carries, its init containers'
	// included (Options.Resources).
	resources(r *Runtime) corev1.ResourceRequirements
	// initContainers are the pod's init containers, and initVolumes the volumes only they mount,
	// besides the pod's own volume and config home.
	initContainers(r *Runtime, l launch) []corev1.Container
	initVolumes(l launch) []corev1.Volume
	// container is the name of the pod's one container, which runs `legion launcher` for every
	// role of roles, and launcherContainer that container: its command, the mounts of every role's
	// own volumes and the pod's shared ones, and the kubelet-resolved environment (resolved).
	// mainVolumes are the volumes that container mounts beyond each role's own, the pod's volume,
	// the config home, the agent-secrets volumes, the providers and the operator's.
	container() string
	launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container
	mainVolumes() []corev1.Volume
	// agentEnv is what each agent is told of its kind, mainEnvironment's one block of its own.
	agentEnv(l launch) []corev1.EnvVar
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

// issuePod is an issue's pod: every workflow role of the issue, working in the issue's workspace
// on the issue's disk, the pod's own generic ephemeral volume, created with the pod and deleted
// with it, in one container whose PID 1 is one `legion launcher` holding a connection of each
// role's own (launcherContainer). Two init containers provision that workspace from the issue's
// repository.
type issuePod struct{}

func (issuePod) roles() []claim.Role { return claim.Roles }

func (issuePod) container() string { return runtime.IssuePodContainer }

// resources is the one issue-pod reservation.
func (issuePod) resources(r *Runtime) corev1.ResourceRequirements { return r.resources.Issue }

func (issuePod) claimToken(project, issue string, role claim.Role) (claim.Token, error) {
	return claim.NewToken(project, issue, role)
}

// prepare refuses a spec with no repository, which the workspace is provisioned from, and resolves
// the workspace's place on the issue's disk. It runs at every launch and expects nothing of that
// disk, which is created with the pod and deleted with it (INFRA-80): only a new or replacement pod
// provisions its workspace (provision, which relaunch runs only when it replaces the pod), and a
// role launched into the live pod finds the workspace the roles before it left. docs/kubernetes.md,
// "The disk dies with its pod", states which launch is which.
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

// disk has no claim template: an issue's Sandbox owns no volume, so nothing of the issue's disk
// outlives a pod (INFRA-80). The disk itself is the pod's own, a generic ephemeral volume: the
// kubelet creates its claim when the pod starts and deletes it when the pod goes, so a suspended,
// relaunched or swept issue leaves no volume behind and every pod starts on a workspace its init
// containers provision afresh. Its claim template carries no metadata at all: the pinned Sandbox
// CRD declares the template's `metadata` a bare object with no properties and no
// preserve-unknown-fields, so a label set there is pruned by the API server without a word
// (TestManifestMatchesTheSandboxCRD).
func (issuePod) disk(r *Runtime, _ launch) ([]volumeClaimTemplate, corev1.VolumeSource) {
	return nil, corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: r.diskSpec()},
	}}
}

// secretAnnotations carry the exact issue key (annotationIssue), from which the launcher resolver
// derives a role's claim (claimToken): the issue label holds the key as a label value, which past
// 63 characters is not the key.
func (issuePod) secretAnnotations(l launch) map[string]string {
	return map[string]string{annotationIssue: l.spec.Issue}
}

// initContainers are workspace-fetch and workspace-init, so the provisioning token never shares a
// process with anything an agent of the issue can write (Stage 4b Task 4b.6b): workspace-fetch
// mounts the provisioning Secret, its own TMPDIR, and the feed alone of the issue's volume (its
// feedSubPath, read-write), and fills the feed — from the repository's cache when one is ready
// (fetchEnvironment), then from GitHub for what it lacks, or from GitHub alone; workspace-init
// mounts the issue's volume, the feed over it read-only, and the config home, and does all the
// volume's work from the feed, with no credential, making the issue's clone shared with the feed
// and starting an issue with no branch at the repository's default branch the spec names. Each
// takes the pod's one reservation, which adds nothing to the pod's: the scheduler counts a pod's
// request as the larger of its main containers' sum and its largest init container.
func (k issuePod) initContainers(r *Runtime, l launch) []corev1.Container {
	legion, resources := r.tools.Legion, k.resources(r)
	return []corev1.Container{{
		Name:       fetchContainer,
		Image:      r.image,
		Command:    []string{legion, "workspace-init", "fetch", "--repo", l.spec.Repository.String(), "--feed", FeedDir},
		Env:        fetchEnvironment(l),
		WorkingDir: FeedDir,
		VolumeMounts: []corev1.VolumeMount{
			{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
			{Name: tempVolume, MountPath: initTempDir},
			{Name: issueVolume, MountPath: FeedDir, SubPath: feedSubPath},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}, {
		Name:  initContainer,
		Image: r.image,
		Command: []string{
			legion, "workspace-init", "provision", "--issue", l.spec.Issue, "--repo", l.spec.Repository.String(),
			"--default-branch", l.spec.DefaultBranch, "--root", TreeRoot,
			"--credential-helper", workspace.GitHubCredentialHelper, "--feed", FeedDir,
		},
		Env:        r.initEnvironment(l),
		WorkingDir: TreeRoot,
		VolumeMounts: []corev1.VolumeMount{
			{Name: issueVolume, MountPath: TreeRoot},
			readOnlyFeed(),
			{Name: configVolume, MountPath: xdgConfigHome},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}}
}

// readOnlyFeed is the issue volume's feed mounted read-only at FeedDir, over the volume's own mount
// at TreeRoot: what every container of an issue pod but workspace-fetch sees of the feed.
func readOnlyFeed() corev1.VolumeMount {
	return corev1.VolumeMount{Name: issueVolume, MountPath: FeedDir, SubPath: feedSubPath, ReadOnly: true}
}

// initVolumes are the provisioning material only the init containers mount: the provisioning
// Secret, with the provisioning token and the repository cache's bearer token, and
// workspace-fetch's in-memory TMPDIR. The feed is the issue volume's own (FeedDir).
func (issuePod) initVolumes(l launch) []corev1.Volume {
	return []corev1.Volume{
		{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName(l.name),
			Items: []corev1.KeyToPath{
				{Key: provisionTokenKey, Path: provisionTokenKey}, {Key: cacheTokenKey, Path: cacheTokenKey},
			},
			DefaultMode: new(int32(0o440)),
		}}},
		{Name: tempVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
}

// launcherContainer is an issue pod's one container, `roles`: `legion launcher` in multi-role mode
// (--roles-dir and one --role per role of l.roles), which reads each role's launcher token under
// its directory of RolesDir and holds a connection of its own per role, and starts each role's
// generation in namespaces of its own (`legion launcher role-init`), where the role sees its own
// gh, private, state and agent-secrets key directories at the fixed paths a role reads them at and
// none of its siblings'. So each role's own volumes mount under its directory of RolesDir, its
// launcher token and gh files read-only, and none at those fixed paths; the role homes volume
// (roleHomesVolume) at RoleHomesDir holds each role's home overlay, temporary directories and own
// config home. The pod's shared mounts are mounted once: the issue's disk, the config home
// workspace-init writes, the feed read-only (so no agent writes what workspace-fetch, the token's
// holder, reads), the providers, the broker's token, and the operator's. Every one of them that
// belongs at or under the image's home — the config home, an operator's mount there — is mounted
// under launcher.HomeMountsDir at the same relative path instead (homeMountsView), so the home
// carries no mount: each role's namespace overlays the home, which Linux refuses over a lower layer
// with mounts under it, and then binds each of them back at its own path, but the shared config
// home, whose place gets the role's own copy of it (launcher.ConfigHomeDir). The values the kubelet
// resolves are the same for every role (the downward API's POD_UID), so they are set once.
func (k issuePod) launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container {
	command := []string{r.tools.Legion, "launcher", connectFlag, r.streamURL, "--sandbox", l.name, "--roles-dir", RolesDir}
	mounts := []corev1.VolumeMount{
		{Name: issueVolume, MountPath: TreeRoot},
		{Name: configVolume, MountPath: xdgConfigHome},
		readOnlyFeed(),
	}
	for _, role := range l.roles {
		command = append(command, "--role", string(role))
		dir := roleDir(role)
		mounts = append(mounts,
			corev1.VolumeMount{Name: roleVolume("launcher", role), MountPath: dir + "/" + launcher.RoleLauncherDir, ReadOnly: true},
			corev1.VolumeMount{Name: roleVolume(ghVolume, role), MountPath: dir + "/" + launcher.RoleGHDir, ReadOnly: true},
			corev1.VolumeMount{Name: roleVolume("private", role), MountPath: dir + "/" + launcher.RolePrivateDir},
			corev1.VolumeMount{Name: roleVolume(stateVolume, role), MountPath: dir + "/" + launcher.RoleStateDir},
		)
		if r.agentSecrets != nil {
			mounts = append(mounts, corev1.VolumeMount{Name: roleVolume(agentSecretsKeyVolume, role), MountPath: dir + "/" + launcher.RoleAgentSecretsDir})
		}
	}
	mounts = append(mounts, corev1.VolumeMount{Name: roleHomesVolume, MountPath: RoleHomesDir})
	if r.agentSecrets != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: agentSecretsTokenVolume, MountPath: AgentSecretsTokenDir, ReadOnly: true})
	}
	return corev1.Container{
		Name:            k.container(),
		Image:           r.image,
		Command:         slices.Concat(command, []string{"--stop-grace", r.terminationGrace.String()}, r.sessionProxyFlags()),
		Env:             resolved,
		WorkingDir:      l.workspace,
		VolumeMounts:    homeMountsView(slices.Concat(mounts, providersMounts, r.pod.VolumeMounts)),
		Resources:       k.resources(r),
		SecurityContext: restrictedContainer(),
	}
}

// mainVolumes is the role homes volume: an emptyDir on the node's disk, charged to the pod's
// ephemeral storage as a container's own filesystem is, where each role's namespace keeps its home
// overlay's writable layer, its /tmp and /var/tmp, and its own config home (`legion launcher
// role-init`). An overlay's writable layer cannot be the container's own root filesystem, and a
// memory one would charge a role's /tmp to the pod's memory.
func (issuePod) mainVolumes() []corev1.Volume {
	return []corev1.Volume{{Name: roleHomesVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
}

// agentEnv tells each agent its tree, issue and workspace, where its gh reads its credentials, and
// uv's directories on the issue's volume.
func (issuePod) agentEnv(l launch) []corev1.EnvVar {
	return slices.Concat(
		[]corev1.EnvVar{
			{Name: "LEGION_TREE", Value: l.spec.Tree},
			{Name: "LEGION_ISSUE", Value: l.spec.Issue},
			{Name: "LEGION_WORKSPACE", Value: l.workspace},
		},
		ghAgentEnv(l),
		[]corev1.EnvVar{
			{Name: "UV_PYTHON_INSTALL_DIR", Value: uvPythonDir(l.spec.Issue)},
			{Name: "UV_CACHE_DIR", Value: uvCacheDir},
			{Name: "UV_LINK_MODE", Value: uvLinkMode},
		},
	)
}

// ghAgentEnv is the six variables that point an agent's gh at its own gh bundle (GHConfigDir), so
// its gh — the image's `legion gh`, first on PATH, which runs the real gh at LEGION_GH_PATH — and
// the clone's `legion git-credential` helper (workspace.GitHubCredentialHelper) read the token of
// the owner a command names from the owner's directory under LEGION_GH_OWNERS_DIR, and every other
// command's from GH_CONFIG_DIR, the launch's default owner's (gitHubDefault). GH_TOKEN,
// GITHUB_TOKEN and GH_HOST are set to the empty string, which gh ignores, so no value the image or
// an operator's rc file could leave in the environment outranks the files: a non-empty GH_TOKEN
// would. No other tool path or helper is told: the agent's git and jj are the image's on PATH, and
// the helper is the clone's own configuration. Every launcher role, the controller's included,
// reads its own App's gh bundle at GHConfigDir, so this is appended once for every kind.
func ghAgentEnv(l launch) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "GH_CONFIG_DIR", Value: ghconfig.OwnerDir(GHConfigDir, l.gitHubDefault)},
		{Name: "GH_TOKEN", Value: ""},
		{Name: "GITHUB_TOKEN", Value: ""},
		{Name: "GH_HOST", Value: ""},
		{Name: "LEGION_GH_OWNERS_DIR", Value: GHConfigDir},
		{Name: "LEGION_GH_PATH", Value: l.gh},
	}
}

// provision is what a new issue pod needs before it starts: it mints the provisioning token for
// the repository's owner, makes sure the repository has its cache (EnsureRepositoryCache: one a
// sweep has not made yet is made now), since a fresh feed comes from that cache alone, waits until
// the cache's Service is its own (awaitCacheService), since the fetch presents the cache's token
// at that Service's name, and writes the token to the pod's init-only provisioning Secret with the
// cache's bearer token. It waits on no other pod, the cache's included: the fetch waits for the
// cache to be ready itself, and the issue's clone lives on the pod's own disk, which no other pod
// mounts. A cache that cannot be made, or whose Service is not its own by the boot timeout, fails
// the launch, with nothing written: the fetch could fill no fresh feed without it.
func (issuePod) provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error {
	owner := l.spec.Repository.Owner()
	minting, cancel := call(ctx)
	token, err := r.tokens.Token(minting, owner)
	cancel()
	if err != nil {
		return fmt.Errorf("mint the provisioning token for %s: %w", owner, err)
	}
	cache, err := r.ensureRepositoryCache(ctx, l.spec.Repository)
	if err != nil {
		return err
	}
	if err := r.awaitCacheService(ctx, l.spec.Repository, cache); err != nil {
		return err
	}
	bearer := r.cacheBearer(cache.Name)
	if bearer == "" {
		return fmt.Errorf("the repository cache %s of %s holds no token", cache.Name, l.spec.Repository)
	}
	if err := r.upsertSecret(ctx, corev1.Secret{
		ObjectMeta: r.secretMeta(s, *l, secretName(s.Name)),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{provisionTokenKey: []byte(token), cacheTokenKey: []byte(bearer)},
	}); err != nil {
		return fmt.Errorf("write its provisioning secret: %w", err)
	}
	return nil
}

// controllerPod is the project controller's pod (`controller: daemon`): the controller's launcher
// alone, working in the root of a volume of its own, which its Sandbox owns, so the working root a
// pod leaves is the one its successor finds. It belongs to no tree and holds the review App's
// GitHub credential like any verdict-only role (appauth.AppRoleFor). Its pod runs no init
// container: it provisions no workspace, and the sessions directory that was the last thing such a
// container made on its volume is gone, its sessions now rows in the session database
// (initContainers).
type controllerPod struct{}

func (controllerPod) roles() []claim.Role { return []claim.Role{claim.RoleController} }

func (controllerPod) container() string { return runtime.ControllerPodContainer }

// resources is the controller's pod's reservation.
func (controllerPod) resources(r *Runtime) corev1.ResourceRequirements { return r.resources.Controller }

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

// disk is the one `issue` volume the controller's Sandbox owns, so the working root the controller
// runs in (TreeRoot, prepare) outlives its pods. Its template's labels are the pod's own (labels),
// which carry the project and the controller role and no tree: the controller belongs to none, and
// the daemon's restricted identity has no PVC verb, so a label put on the claim at creation is the
// label it keeps for life. The pod mounts the claim of its own Sandbox (IssueClaimName), the name
// the Agent Sandbox controller gives the claim it makes from that template.
func (controllerPod) disk(r *Runtime, l launch) ([]volumeClaimTemplate, corev1.VolumeSource) {
	templates := []volumeClaimTemplate{{
		Metadata: volumeClaimMetadata{Name: issueVolume, Labels: r.labels(l)},
		Spec:     r.diskSpec(),
	}}
	return templates, corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: IssueClaimName(l.spec.Claim),
	}}
}

// secretAnnotations are none: the controller's claim is the project's (claimToken).
func (controllerPod) secretAnnotations(launch) map[string]string { return nil }

// initContainers are none: the controller's volume needs no preparing before its launcher starts.
// The launcher works in the volume's root, which the mount itself makes, owned by the pod's group
// (fsGroup, podTemplate); the scratch directories its agent is told are made by the shim
// (podsafety.EnsureScratch); and its session is a row in the session database, which the role
// launcher looks up before it starts the child (internal/launcher), not a file on the volume.
func (controllerPod) initContainers(*Runtime, launch) []corev1.Container { return nil }

// launcherContainer is the controller's pod's one container, `controller`: `legion launcher` in
// single-role mode, with the controller's own launcher token, private directory, state directory
// and gh volume mounted at the fixed paths it reads them at, the gh volume read-only, its key
// directory and the broker's token when the pod enrolls, beside the pod's volume, the config home,
// the providers and the operator's mounts.
func (k controllerPod) launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container {
	role := claim.RoleController
	mounts := []corev1.VolumeMount{
		{Name: issueVolume, MountPath: TreeRoot},
		{Name: roleVolume("launcher", role), MountPath: LauncherDir, ReadOnly: true},
		{Name: roleVolume("private", role), MountPath: LauncherPrivateDir},
		{Name: roleVolume(stateVolume, role), MountPath: StateDir},
		{Name: configVolume, MountPath: xdgConfigHome},
		{Name: roleVolume(ghVolume, role), MountPath: GHConfigDir, ReadOnly: true},
	}
	return corev1.Container{
		Name:  k.container(),
		Image: r.image,
		Command: slices.Concat([]string{
			r.tools.Legion, "launcher", connectFlag, r.streamURL, "--token-file", LauncherDir + "/" + LauncherTokenFile,
			"--sandbox", l.name, "--role", string(role), "--private-dir", LauncherPrivateDir,
			"--stop-grace", r.terminationGrace.String(),
		}, r.sessionProxyFlags()),
		Env:             resolved,
		WorkingDir:      l.workspace,
		VolumeMounts:    slices.Concat(mounts, providersMounts, agentSecretsMounts(r.agentSecrets, role), r.pod.VolumeMounts),
		Resources:       k.resources(r),
		SecurityContext: restrictedContainer(),
	}
}

// mainVolumes are none beyond the controller's own: its launcher has no namespaces to keep homes in.
func (controllerPod) mainVolumes() []corev1.Volume { return nil }

// initVolumes are none: the controller's pod runs no init container.
func (controllerPod) initVolumes(launch) []corev1.Volume { return nil }

// agentEnv tells the agent LEGION_CONTROLLER=1, its pane marker, where its gh reads its
// credentials (ghAgentEnv), and nothing a tree agent alone needs: no tree, issue or workspace, and
// none of uv's directories on an issue's volume.
func (controllerPod) agentEnv(l launch) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "LEGION_CONTROLLER", Value: "1"}}
	return append(env, ghAgentEnv(l)...)
}

// provision readies nothing: the controller's pod provisions no workspace and mints no token.
func (controllerPod) provision(context.Context, *Runtime, *launch, *sandbox) error { return nil }
