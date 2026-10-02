package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The pod's own paths. The tree volume is mounted whole at TreeRoot in workspace-init and each
// role launcher, and sessions stay on it. Each role container has its own launcher token
// projection at LauncherDir, and its own memory-backed LauncherPrivateDir, where its launcher
// writes the generation's boot token and launch credentials, and StateDir.
const (
	TreeRoot           = "/legion"
	SessionsSubPath    = "sessions"
	LauncherDir        = "/var/run/legion/launcher"
	LauncherPrivateDir = "/var/run/legion/private"
	LauncherTokenFile  = "LAUNCHER_TOKEN"
	ProvisionDir       = "/var/run/legion/provision"
	FeedDir            = "/var/run/legion/feed"
	StateDir           = "/var/run/legion/state"
	ProvidersDir       = "/var/run/legion/providers"
)

// AgentSecretsKeyDir is the memory-backed directory the pod's agent-secrets key and enrollment id
// live in, and AgentSecretsTokenDir where its projected token for the broker's audience is
// mounted, as AgentSecretsTokenFile (AGENTC-393). Both are the worker container's alone.
const (
	AgentSecretsKeyDir    = "/var/run/legion/agent-secrets"
	AgentSecretsTokenDir  = "/var/run/legion/agent-secrets-token"
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
	podHome = "/home/legion"
	// imagePath is the image's PATH. A container's env PATH replaces the image's, so the pod's
	// PATH repeats it after its own directories.
	imagePath = "/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin"
	// defaultAgent is Oh My Pi, by the path the image installs it at (LEGION_OMP_PATH).
	defaultAgent = "/opt/omp/bin/omp"
	// legionPlugin is the packed pi-legion-envoy the image carries, whose package.json names its
	// extensions and skills.
	legionPlugin = "/opt/legion/pi-legion-envoy"
	// workerBin is where workspace-init installs the gh shim on the tree volume.
	workerBin = TreeRoot + "/worker-bin"
	// initTempDir is the workspace-fetch container's TMPDIR, an in-memory volume of its own:
	// `workspace-init fetch` keeps its one-shot credential there, off every volume another
	// container mounts.
	initTempDir = "/tmp"
)

// imageOwnedPaths are the paths in the worker image a pod runs or loads from, which an operator's
// mount there would hide: Legion's binaries, plugin, and role prompts (/opt/legion), Oh My Pi
// (/opt/omp), the profile's installed plugins, and the databases Oh My Pi keeps in the profile's
// agent directory; CheckPod adds the Tools. An operator's mount may be neither at, under, nor
// above one.
func imageOwnedPaths() []string {
	return []string{"/opt/legion", "/opt/omp", ompProfileDir + "/plugins", ompAgentDir + "/agent.db", ompAgentDir + "/models.db"}
}

// The keys of a claim's Secret that the runtime fills itself: the boot token and the provisioning
// token for every claim, and the Dispatch bearer when Dispatch is configured.
const (
	bootTokenKey      = "LEGION_BOOT_TOKEN"
	provisionTokenKey = "LEGION_PROVISION_TOKEN"
	dispatchTokenKey  = "DISPATCH_TOKEN"
)

// The labels every object of a claim carries: the Sandbox, its pod template (the only place the
// controller copies pod labels from), the root's volume claim template (copied to the PVC), and
// the claim's Secret. The informers select on the project label.
const (
	labelProject = "legion.dev/project"
	labelTree    = "legion.dev/tree"
	labelIssue   = "legion.dev/issue"
	labelRole    = "legion.dev/role"
)

// treeVolume is the root Sandbox's volume claim template, and the pod volume every tree pod mounts.
// The controller names the claim `<template>-<sandbox>` (agent-sandbox v1.0.3,
// sandbox_controller.go:1641), which is TreeClaimName.
const treeVolume = "tree"

// maxNameLength is a DNS-1123 label's length: a Sandbox name is also its pod's and, were one
// asked for, its Service's.
const maxNameLength = 63

// SandboxName is the Sandbox and pod name for an issue. The six role claims of that issue share
// it; their container and generation live in each process locator instead. A claim token always
// ends in one fixed role word, so stripping it preserves the project and issue token even where an
// issue name has hyphens.
func SandboxName(t claim.Token) string {
	name := string(t)
	for _, role := range claim.Roles {
		if strings.HasSuffix(name, "-"+string(role)) {
			name = strings.TrimSuffix(name, "-"+string(role))
			break
		}
	}
	return dnsName(name, maxNameLength)
}

// TreeClaimName is the tree volume's claim: the root Sandbox's `tree` template, as the controller
// names the claim it makes from it. Every pod of the tree mounts it by this name.
func TreeClaimName(root claim.Token) string { return treeVolume + "-" + SandboxName(root) }

// secretName is the claim's Secret, which its Sandbox owns.
func secretName(sandbox string) string { return sandbox + "-boot" }

// roleSecretName is one role's private Secret in an issue Sandbox. Its launcher token and every
// `<NAME>_FILE` credential are projected only into that role's container.
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
// sha256(value), so two long values that share the prefix still differ (the shipped k8sSlug,
// packages/daemon/src/daemon/k8s-manifests.ts:52-61).
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
// (internal/launcher). The launcher and this runtime agree on `g<generation>`.
func generationDir(generation uint64) string {
	return LauncherPrivateDir + "/g" + strconv.FormatUint(generation, 10)
}
