package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The pod's own paths. The tree volume is mounted whole at TreeRoot in both containers, and its
// SessionsSubPath directory again at Oh My Pi's sessions directory in the main container, so a
// session the agent writes is on the volume and the init container sees it under TreeRoot. The
// claim's Secret is projected twice: its boot half at BootDir for the main container, its
// provisioning token at ProvisionDir for the init container alone. StateDir is the main
// container's in-memory LEGION_STATE_DIR. GatewayDir holds the main container's projected model
// gateway token, as GatewayDir/gatewayTokenFile.
const (
	TreeRoot        = "/legion"
	SessionsSubPath = "sessions"
	BootDir         = "/var/run/legion/boot"
	ProvisionDir    = "/var/run/legion/provision"
	StateDir        = "/var/run/legion/state"
	GatewayDir      = "/var/run/legion/gateway"
)

// gatewayTokenFile is the projected token's file under GatewayDir, which the image's Oh My Pi
// profile reads as its gateway key.
const gatewayTokenFile = "token"

// minTokenExpiry is the shortest projected service account token the API server issues.
const minTokenExpiry = 10 * time.Minute

// gatewayTokenVolume is the one projected volume a pod reaches the model gateway with, and its
// read-only mount at GatewayDir: a single serviceAccountToken source for g's audience, living
// g.TokenExpiry. Every pod that calls the gateway, a worker's and the image probe's, mounts
// exactly this.
func gatewayTokenVolume(g Gateway) (corev1.Volume, corev1.VolumeMount) {
	expiry := int64(g.TokenExpiry / time.Second)
	volume := corev1.Volume{Name: gatewayVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: g.Audience, ExpirationSeconds: &expiry, Path: gatewayTokenFile,
		}}},
	}}}
	return volume, corev1.VolumeMount{Name: gatewayVolume, MountPath: GatewayDir, ReadOnly: true}
}

// The image's own paths (packages/daemon/docker/worker.Dockerfile: ENV and the COPY lines).
const (
	// ompSessionsDir is Oh My Pi's sessions directory under the image's HOME and OMP_PROFILE:
	// <HOME>/.omp/profiles/<profile>/agent/sessions.
	ompSessionsDir = "/home/legion/.omp/profiles/legion/agent/sessions"
	// podHome is the image's HOME, which the XDG base directories sit under.
	podHome = "/home/legion"
	// imagePath is the image's PATH. A container's env PATH replaces the image's, so the pod's
	// PATH repeats it after its own directories.
	imagePath = "/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin"
	// defaultAgent is Oh My Pi, by the path the image installs it at (LEGION_OMP_PATH).
	defaultAgent = "/opt/omp/bin/omp"
	// workerBin is where workspace-init installs the gh shim on the tree volume.
	workerBin = TreeRoot + "/worker-bin"
	// initTempDir is the init container's TMPDIR, an in-memory volume: workspace-init keeps its
	// provisioning credential there, off the tree volume every pod of the tree mounts.
	initTempDir = "/tmp"
)

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
