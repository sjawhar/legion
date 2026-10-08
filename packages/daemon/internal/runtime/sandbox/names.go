package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The pod's own paths. The issue's volume is mounted whole at TreeRoot in workspace-init and each
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
// mounted, as AgentSecretsTokenFile. Both are the worker container's alone.
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
	// envoyPlugin is the packed pi-envoy the image carries, the Envoy plugin every session loads,
	// whose package.json names its extension and skills; a pod passes it as an explicit extension
	// before legionPlugin, which claims its role through the interface pi-envoy publishes.
	envoyPlugin = "/opt/legion/pi-envoy"
	// legionPlugin is the packed pi-legion the image carries, whose package.json names its
	// extension, skills and the daemon API contract it speaks.
	legionPlugin = "/opt/legion/pi-legion"
	// workerBin is where workspace-init installs the gh shim on the issue's volume.
	workerBin = TreeRoot + "/worker-bin"
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
// token for every claim, and the Dispatch bearer when Dispatch is configured.
const (
	bootTokenKey      = "LEGION_BOOT_TOKEN"
	provisionTokenKey = "LEGION_PROVISION_TOKEN"
	dispatchTokenKey  = "DISPATCH_TOKEN"
)

// The labels every object of a claim carries: the Sandbox, its pod template (the only place the
// controller copies pod labels from), its volume claim template (copied to the PVC), and the
// claim's Secret. The informers select on the project label.
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

// maxNameLength is a DNS-1123 label's length: a Sandbox name is also its pod's and, were one
// asked for, its Service's.
const maxNameLength = 63

// SandboxName is the Sandbox and pod name for an issue, or for the project controller. The six role
// claims of an issue share it; their container and generation live in each process locator
// instead. A role claim's token always ends in one fixed role word, so cutting it (claim.Token.Cut)
// preserves the project and issue token even where an issue name has hyphens. The controller's
// token (claim.ControllerToken) ends in no workflow role and names its Sandbox whole,
// `legion-<project>-controller`, which no issue's can be: an issue's part always ends in
// `-<digits>`, and one past dnsName's length in a dash and eight hex digits.
func SandboxName(t claim.Token) string {
	pod := string(t)
	if issue, _, ok := t.Cut(); ok {
		pod = issue
	}
	return dnsName(pod, maxNameLength)
}

// launcherRoles is every role a launcher container can run: the six workflow roles of an issue
// pod, and the controller of the project controller's pod.
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

// IssueClaimName is the volume claim of the Sandbox that t's claim runs in, the issue's or the
// controller's, as the controller names the claim it makes from the Sandbox's `issue` template.
// Any role claim of the issue names it: the token is cut to the Sandbox's name as SandboxName
// cuts it.
func IssueClaimName(t claim.Token) string { return issueVolume + "-" + SandboxName(t) }

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
