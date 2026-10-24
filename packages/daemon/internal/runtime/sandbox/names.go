package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	corev1 "k8s.io/api/core/v1"
)

// The pod's own paths. The issue's volume is mounted whole at TreeRoot in workspace-init and each
// role launcher, and sessions stay on it. The pod's feed is the volume's feedSubPath directory,
// mounted at FeedDir: read-write in workspace-fetch, which mounts nothing else of the volume, and
// read-only over the whole volume's mount in every other container. The clone owns its object-store
// entries, and no agent writes what the token-holding container reads. On a volume carried over from
// an earlier release, whose agents could write the whole volume, feedSubPath may be a link to another
// directory of it, which the kubelet mounts in its place; agents may still change what the next fetch
// reads there, through that directory's own path or the link itself, which is no mount point in their
// containers. There the fetch's own check of the feed (workspace.Fetch takes a feed only in the shape
// it writes), run before any git and in an init container no agent runs beside, keeps the
// token-holding container from running what they wrote.
// LauncherDir, LauncherPrivateDir, StateDir, GHConfigDir and AgentSecretsKeyDir are where a
// launcher role's process finds its own launcher token projection, memory-backed private directory
// (where its launcher writes each generation's boot token and launch credentials), state directory,
// gh bundle and agent-secrets key directory. The controller's pod mounts them there in its one
// container. An issue pod's one container runs every workflow role, so it mounts each role's under
// RolesDir (roleDir) instead, and each role's namespace (`legion launcher role-init`) covers
// RolesDir and binds its own back at these paths, all but the launcher token, which no role's
// process sees. GHConfigDir is the role's gh bundle root (LEGION_GH_OWNERS_DIR): the read-only
// projection of its role Secret's gh files, one directory per GitHub owner of the project
// (GitHubHostsKey(owner) as <owner>/ghconfig.HostsFile, GitHubConfigKey(owner) as
// <owner>/ghconfig.ConfigFile), whose default owner's directory (GitHubDefaultKey) is its
// GH_CONFIG_DIR, and which its gh (the image's `legion gh`) and the clone's `legion git-credential`
// helper read the token of the owner a command names from. The kubelet rewrites a whole-volume
// Secret projection in place when the Secret changes, and a bind of it sees the rewrite, so the
// refresher's Secret update is what the next gh runs with. CacheRoot is where a repository cache's
// pod mounts its own volume. The
// issue's scratch is `/legion/scratch` (scratchRoot) on its volume, and the roles of the issue
// share it: one temporary directory, one cache home, one data home and one bun install cache,
// `/legion/scratch/{tmp,cache,data,bun/install/cache}`, every role told the same four
// (roleEnvironment). The one directory of it that is a role's own is its state home,
// `/legion/scratch/state/<role>` (roleStateHome), which keeps one role's browser broker lock from
// blocking its siblings'. The go command's two caches are the issue's too, one pair every role of
// it is told, `/legion/go/build` and `/legion/go/mod` (goBuildCache, goModCache) under
// `/legion/go`, a top-level directory of the volume beside the workspaces, the repositories,
// SessionsSubPath, feedSubPath and uv's own (uvPythonRoot, uvCacheDir). The pod's ephemeral
// storage bounds none of them.
const (
	TreeRoot           = "/legion"
	SessionsSubPath    = "sessions"
	FeedDir            = TreeRoot + "/" + feedSubPath
	CacheRoot          = "/legion/cache"
	LauncherDir        = launcher.FixedRoot + "/" + launcher.RoleLauncherDir
	LauncherPrivateDir = launcher.FixedRoot + "/" + launcher.RolePrivateDir
	LauncherTokenFile  = launcher.LauncherTokenFile
	GHConfigDir        = launcher.FixedRoot + "/" + launcher.RoleGHDir
	ProvisionDir       = launcher.FixedRoot + "/provision"
	StateDir           = launcher.FixedRoot + "/" + launcher.RoleStateDir
	ProvidersDir       = launcher.FixedRoot + "/providers"
)

// RolesDir is where an issue pod's one container mounts every workflow role's own volumes, one
// directory per role (roleDir), and the role homes volume at RoleHomesDir. `legion launcher
// --roles-dir` reads each role's launcher token there, and `legion launcher role-init` covers the
// whole directory inside the role's namespaces and binds the role's own gh, private, state and
// agent-secrets directories back at GHConfigDir, LauncherPrivateDir, StateDir and
// AgentSecretsKeyDir, and makes the role's home overlay and temporary directories under its
// directory of RoleHomesDir. The names under it are internal/launcher's layout (launcher.RoleGHDir
// and its siblings), which both sides read.
const (
	RolesDir     = launcher.FixedRoot + "/roles"
	RoleHomesDir = RolesDir + "/" + launcher.RoleHomesDir
	// HomeMountsDir is where an issue pod's roles container mounts every volume that belongs at
	// or under the image's home (homeMountsView), and where `legion launcher role-init` finds
	// each to bind it back at its own path under the role's home overlay.
	HomeMountsDir = launcher.HomeMountsDir
)

// roleDir is role's own directory of an issue pod's one container, under RolesDir: its launcher
// token and gh projections of its role Secret, read-only, and its memory-backed private, state and
// agent-secrets key directories, the last only in a pod enrolled with the secrets broker, each one
// volume's mount at that name of launcher's layout.
func roleDir(role claim.Role) string { return RolesDir + "/" + string(role) }

// homeMountsView is mounts as an issue pod's roles container carries them: every mount at or
// under the image's home (podHome) at the same path relative to HomeMountsDir instead
// (containerHomePath), so the home itself carries no mount, and every other mount as it is.
// `legion launcher role-init` binds each back at its own path under the role's home overlay.
func homeMountsView(mounts []corev1.VolumeMount) []corev1.VolumeMount {
	out := slices.Clone(mounts)
	for i := range out {
		out[i].MountPath = containerHomePath(out[i].MountPath)
	}
	return out
}

// containerHomePath is where an issue pod's roles container, outside every role's namespaces,
// holds what a role sees at path: a path at or under the image's home is under HomeMountsDir,
// and any other path is the same in both views.
func containerHomePath(path string) string {
	if path == podHome {
		return HomeMountsDir
	}
	if rest, ok := strings.CutPrefix(path, podHome+"/"); ok {
		return HomeMountsDir + "/" + rest
	}
	return path
}

// feedSubPath is the issue volume's directory that holds the pod's feed (FeedDir). The kubelet
// makes it on first use, so a volume provisioned before the feed moved onto it is relaunched as it
// is: nothing of its tree moves.
const feedSubPath = "feed"

// AgentSecretsKeyDir is the memory-backed directory a role's agent-secrets key and enrollment id
// live in, and AgentSecretsTokenDir where the pod's projected token for the broker's audience is
// mounted, as AgentSecretsTokenFile. The key directory is each role's own; the token is shared.
const (
	AgentSecretsKeyDir    = launcher.FixedRoot + "/" + launcher.RoleAgentSecretsDir
	AgentSecretsTokenDir  = launcher.FixedRoot + "/agent-secrets-token"
	AgentSecretsTokenFile = "token"
)

// The image's own paths (packages/daemon/docker/worker.Dockerfile: ENV and the COPY lines).
const (
	// ompProfileDir is the image's Oh My Pi profile, <HOME>/.omp/profiles/<OMP_PROFILE>, whose
	// plugins/ holds the plugin the image installed; ompAgentDir is the profile's agent directory,
	// where Oh My Pi keeps its databases, and ompSessionsDir the sessions directory in it.
	ompProfileDir  = podHome + "/.omp/profiles/legion"
	ompAgentDir    = ompProfileDir + "/agent"
	ompSessionsDir = ompAgentDir + "/sessions"
	// podHome is the image's HOME, which the XDG base directories sit under.
	podHome = launcher.Home
	// imagePath is the image's PATH. A container's env PATH replaces the image's, so the pod's
	// PATH repeats it after its own directories.
	imagePath = "/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin"
	// defaultAgent is Oh My Pi, by the path the image installs it at (LEGION_OMP_PATH).
	defaultAgent = "/opt/omp/bin/omp"
	// envoyPlugin is the packed pi-envoy the image carries, the Envoy plugin every session loads,
	// whose package.json names its extension and skills; a pod passes it as an explicit extension
	// before legionPlugin, which claims its role through the interface pi-envoy publishes.
	envoyPlugin = "/opt/legion/pi-envoy"
	// legionPlugin is the packed pi-legion the image carries, whose package.json names its
	// extension, skills and the daemon API contract it speaks.
	legionPlugin = "/opt/legion/pi-legion"
	// initTempDir is the workspace-fetch container's TMPDIR, an in-memory volume of its own:
	// `workspace-init fetch` keeps its one-shot credential there, off every volume another
	// container mounts.
	initTempDir = "/tmp"
)

// imageOwnedPaths are the paths in the worker image a pod runs or loads from, which an operator's
// mount there would hide: Legion's binaries and plugin (/opt/legion), Oh My Pi
// (/opt/omp), the profile's installed plugins, and the databases Oh My Pi keeps in the profile's
// agent directory; CheckPod adds the Tools. An operator's mount may be neither at, under, nor
// above one.
func imageOwnedPaths() []string {
	return []string{"/opt/legion", "/opt/omp", ompProfileDir + "/plugins", ompAgentDir + "/agent.db", ompAgentDir + "/models.db"}
}

// The keys of a claim's Secret that the runtime fills itself: the boot token and the provisioning
// token for every claim, the Dispatch bearer when Dispatch is configured, and, in an issue pod's
// provisioning Secret and its repository cache's Secret, the cache's bearer token.
const (
	bootTokenKey      = "LEGION_BOOT_TOKEN"
	provisionTokenKey = "LEGION_PROVISION_TOKEN"
	dispatchTokenKey  = "DISPATCH_TOKEN"
	cacheTokenKey     = "LEGION_REPOSITORY_CACHE_TOKEN"
)

// The keys of a role's Secret that carry its GitHub credential beside the launcher token: each
// owner's rendered hosts.yml and config.yml (ghconfig.Bundle), and the bundle's default owner,
// written for every launcher role: every workflow role of an issue pod and the controller's.
// credentialFromSecret hashes the launcher token alone, so the refresher rewrites the gh keys
// without changing the launcher's binding.
const GitHubDefaultKey = "github-default"

// GitHubHostsKey is the role Secret's key holding owner's hosts.yml.
func GitHubHostsKey(owner string) string { return "github-hosts-" + owner }

// GitHubConfigKey is the role Secret's key holding owner's config.yml.
func GitHubConfigKey(owner string) string { return "github-config-" + owner }

// The labels every object of a claim carries: the Sandbox, its pod template (the only place the
// controller copies pod labels from), the claim's Secret, and, less the tree (claimLabels), its
// volume claim template (copied to the PVC). The informers select on the project label.
const (
	labelProject = "legion.dev/project"
	labelTree    = "legion.dev/tree"
	labelIssue   = "legion.dev/issue"
	labelRole    = "legion.dev/role"
)

// issueVolume is every Sandbox's volume claim template, the issue's or the controller's, and the
// pod volume its pod mounts. The controller names the claim `<template>-<sandbox>` (agent-sandbox
// v1.0.3, sandbox_controller.go:1641), which is IssueClaimName.
const issueVolume = "issue"

// A repository cache's identity and parts. labelCache marks every object of a cache — its Sandbox,
// pod template, pod and Secret — with the one value cacheMarker, for listing and passing over;
// it names no repository, which a label value cannot hold (`/`) and a DNS-safe form of it cannot
// be read back from. The repository is the annotation annotationCacheRepo alone, exactly as GitHub
// names it, on the Sandbox, its pod template and its Secret (cacheRepository). The sweep marks a
// cache whose repository left the mapping with annotationCacheUnmappedAt. cacheVolume is a cache
// Sandbox's one claim template and cacheContainer its pod's one container.
const (
	labelCache                = "legion.dev/cache"
	cacheMarker               = "true"
	annotationCacheRepo       = "legion.dev/cache-repo"
	annotationCacheUnmappedAt = "legion.dev/cache-unmapped-at"
	cacheVolume               = "cache"
	cacheContainer            = "cache"
)

// maxNameLength is a DNS-1123 label's length: a Sandbox name is also its pod's and, were one
// asked for, its Service's.
const maxNameLength = 63

// SandboxName is the Sandbox and pod name for an issue, or for the project controller. The six role
// claims of an issue share it, and its one container; their generations live in each process
// locator instead. A role claim's token always ends in one fixed role word, so cutting it
// (claim.Token.Cut) preserves the project and issue token even where an issue name has hyphens.
// The controller's token (claim.ControllerToken) ends in no workflow role and names its Sandbox
// whole, `legion-<project>-controller`, which no issue's can be: an issue's part always ends in
// `-<digits>`, and one past dnsName's length in a dash and eight hex digits.
func SandboxName(t claim.Token) string {
	pod := string(t)
	if issue, _, ok := t.Cut(); ok {
		pod = issue
	}
	return dnsName(pod, maxNameLength)
}

// launcherRoles is every role a launcher can run: the six workflow roles of an issue pod, and the
// controller of the project controller's pod. Each has a role Secret of its own in its pod.
var launcherRoles = append(slices.Clone(claim.Roles), claim.RoleController)

// issueSandboxName is SandboxName of issue's role claims, from the issue key. project need not
// already be a claim.ProjectToken (the caller's own Options.Project, the legion.dev/project
// label's value, may carry characters one drops); it goes through ProjectToken here before
// naming a claim.
func issueSandboxName(project, issue string) (string, error) {
	normalized, err := claim.ProjectToken(project)
	if err != nil {
		return "", err
	}
	token, err := claim.NewToken(normalized, issue, claim.RoleArchitect)
	if err != nil {
		return "", err
	}
	return SandboxName(token), nil
}

// CacheSandboxName is the Sandbox and pod name of repo's cache in project: a DNS label derived from
// the project and the repository, always ending in a dash and the first eight hex digits of the
// sha256 of the repository's lowercased name, so two repositories whose slugs coincide
// (acme/widgets-x and acme-widgets/x) still get two names; at most 63 characters. It is never
// parsed back: the repository is the annotation's (cacheRepository).
func CacheSandboxName(project string, repo ghrepo.Repository) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo.String())))
	return dnsName("legion-"+project+"-cache-"+repo.Owner()+"-"+repo.Name(), maxNameLength-1-hashLength) + "-" + hex.EncodeToString(sum[:])[:hashLength]
}

// cacheSecretName is a cache's Secret, which its Sandbox owns: the implement App's token for the
// repository's owner, and the cache's bearer token its clients present.
func cacheSecretName(sandbox string) string { return sandbox + "-token" }

// cacheClaimName is a cache's volume claim, as the controller names the claim it makes from the
// Sandbox's cacheVolume template.
func cacheClaimName(sandbox string) string { return cacheVolume + "-" + sandbox }

// IssueClaimName is the volume claim of the Sandbox that t's claim runs in, the issue's or the
// controller's, as the controller names the claim it makes from the Sandbox's `issue` template.
// Any role claim of the issue names it: the token is cut to the Sandbox's name as SandboxName
// cuts it.
func IssueClaimName(t claim.Token) string { return issueVolume + "-" + SandboxName(t) }

// secretName is the claim's Secret, which its Sandbox owns.
func secretName(sandbox string) string { return sandbox + "-boot" }

// roleSecretName is one role's private Secret in its Sandbox: its launcher token and gh files,
// which only its launcher, and only its role's namespace, read.
func roleSecretName(sandbox string, role claim.Role) string {
	return sandbox + "-" + string(role) + "-boot"
}

func roleVolume(prefix string, role claim.Role) string { return prefix + "-" + string(role) }

// ProvidersSecretName is the Secret the operator keeps the provider keys in, the TypeScript
// runtime's legion-<project>-providers (k8s-manifests.ts, providersSecretName), project being the
// project token (claim.ProjectToken: lowercase letters and digits). Every pod mounts from it exactly
// the keys provider_keys names.
func ProvidersSecretName(project string) string { return "legion-" + project + "-providers" }

var (
	notDNS     = regexp.MustCompile(`[^a-z0-9-]`)
	dashes     = regexp.MustCompile(`-+`)
	hashLength = 8
)

// dnsName lowercases value, turns every character outside [a-z0-9-] into a dash, collapses dash
// runs, and trims dashes at either end; past max it keeps a prefix and appends a dash and 8 hex of
// sha256(value), so two long values that share the prefix still differ.
func dnsName(value string, max int) string {
	slug := strings.Trim(dashes.ReplaceAllString(notDNS.ReplaceAllString(strings.ToLower(value), "-"), "-"), "-")
	if len(slug) <= max {
		return slug
	}
	sum := sha256.Sum256([]byte(value))
	return slug[:max-1-hashLength] + "-" + hex.EncodeToString(sum[:])[:hashLength]
}

// labelValue is an issue key as a label value: itself up to a label value's 63 characters, and
// dnsName past them (k8s-manifests.ts:75-79).
func labelValue(key string) string {
	if len(key) <= maxNameLength {
		return key
	}
	return dnsName(key, maxNameLength)
}

// generationDir is where a role's launcher writes one generation's credentials: a fresh
// owner-only directory under its private directory, gone when the generation ends
// (internal/launcher, shimwire.GenerationDir).
func generationDir(generation uint64) string {
	return LauncherPrivateDir + "/" + shimwire.GenerationDir(generation)
}
