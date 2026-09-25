package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The pod's own paths. The tree volume is mounted whole at TreeRoot in the workspace-init and main
// containers, and its SessionsSubPath directory again at Oh My Pi's sessions directory in the main
// container, so a session the agent writes is on the volume and workspace-init sees it under
// TreeRoot. The claim's Secret is projected twice: its boot half at BootDir for the main
// container, its provisioning token at ProvisionDir for the workspace-fetch container alone.
// FeedDir is the feed workspace-fetch fills and workspace-init reads. StateDir is the main
// container's in-memory LEGION_STATE_DIR. ProvidersDir is where the main container mounts the
// configured keys of the providers Secret (ProvidersSecretName), one file per variable Oh My Pi
// reads, which the shim exports into Oh My Pi's environment alone (--provider-env-dir).
const (
	TreeRoot        = "/legion"
	SessionsSubPath = "sessions"
	BootDir         = "/var/run/legion/boot"
	ProvisionDir    = "/var/run/legion/provision"
	FeedDir         = "/var/run/legion/feed"
	StateDir        = "/var/run/legion/state"
	ProvidersDir    = "/var/run/legion/providers"
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
	imagePath = "/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin"
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

// SandboxName is the Sandbox a claim runs in, and the pod behind it: the claim token as a DNS-1123
// name, and past 63 characters a readable prefix and an 8-hex hash of the whole token. It is the
// same for every generation of the claim, since a generation is a new boot token and a relaunch,
// never a new Sandbox.
func SandboxName(t claim.Token) string { return dnsName(string(t), maxNameLength) }

// TreeClaimName is the tree volume's claim: the root Sandbox's `tree` template, as the controller
// names the claim it makes from it. Every pod of the tree mounts it by this name.
func TreeClaimName(root claim.Token) string { return treeVolume + "-" + SandboxName(root) }

// secretName is the claim's Secret, which its Sandbox owns.
func secretName(sandbox string) string { return sandbox + "-boot" }

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
