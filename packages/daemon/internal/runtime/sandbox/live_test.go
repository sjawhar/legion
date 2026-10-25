//go:build e2e

// The Stage 4a live proof (LEGION-208 Stage 4 plan, Task 4.7): this package's runtime driven on a
// real cluster through the Legion daemon's own restricted identity, with the devbox's admin
// identity only for what an operator does beside it — exec, PVC reads, a Secret's hash, the
// namespace list. scripts/e2e/stage4a-sandbox-runtime.sh builds it with `-tags e2e`, runs it, and
// owns the run's inputs, its teardown, and the namespace comparison; see scripts/e2e/README.md.
//
// Each check prints what it observed, each line naming the identity that observed it, and then
// `CHECK <name>: PASS`. The first check that does not hold prints `CHECK <name>: FAIL: <why>` and
// ends the run. The agent every pod runs is a stub (decision 7): the Go shim bridges it as it would
// Oh My Pi, and it appends its own role, pod uid and generation to a marker file on the issue pod's
// own disk, and sleeps. That disk is the pod's own ephemeral volume and dies with it (INFRA-80), so
// the marker proves a role's identity within one pod and nothing across pods. Within the pod it is
// shared: SandboxName (see names.go) is one name for all six of an issue's role claims, so one issue
// pod's resident roles each append their own launches to the same file, and a check reads only its
// own claim's lines back out of it (roleIncarnations); a pod replacement leaves a fresh, empty one.
// Continuity across a pod replacement is the session table's instead. Every runtime of the run keeps
// sessions in the scratch Postgres the script started (session_store postgres, liveSessionsSecretKey),
// the harness records each claim's session there as `legion sessions import` copies one
// (recordSession), and every resume names that recorded path, which the role launcher looks up in the
// table — never on the disk — before any child runs.
//
// This file is the rig: the run's inputs, the boot-token registry, the observation and log
// records, the runtime's lifecycle, and the launches and reads every check shares. The checks
// live beside it: live_install_test.go (identity and install), live_lifecycle_test.go (the claims'
// lifecycle), live_setup_test.go (runtime.kubernetes.setup), and live_app_test.go (the implement
// App the provisioning token comes from).
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// liveCheck is one check of the harness: the name it prints, what it runs, and — when non-nil —
// whether this run's inputs support it at all (SKIPPED-BLOCKED, not a failure).
type liveCheck struct {
	name    string
	run     func(*liveRig) error
	blocked func(*liveRig) string
}

// liveChecks are the harness's checks, in the order they run: the one list of them, which a
// development run's entry point is checked against. namespace-clean, the last check of the proof,
// is the script's: it compares the namespace after the teardown, outside the harness.
var liveChecks = []liveCheck{
	{"identity", (*liveRig).checkIdentity, nil},
	{"installed", (*liveRig).checkInstalled, nil},
	{"boot-refusal-negative", (*liveRig).checkBootRefusal, nil},
	{"image-probe", (*liveRig).checkImageProbe, nil},
	{"image-probe-negative", (*liveRig).checkImageProbeRefusal, nil},
	{"image-probe-capability-negative", (*liveRig).checkImageProbeCapabilityRefusal, nil},
	{"image-probe-missing-volume-negative", (*liveRig).checkImageProbeMissingVolumeRefusal, nil},
	{"root-ready", (*liveRig).checkRootReady, nil},
	{"gvisor", (*liveRig).checkGVisor, nil},
	{"resources", (*liveRig).checkResources, nil},
	{"operator-token", (*liveRig).checkOperatorToken, nil},
	{"pod-baseline", (*liveRig).checkPodBaseline, nil},
	{"setup-ran", (*liveRig).checkSetupRan, nil},
	{"setup-negative", (*liveRig).checkSetupNegative, nil},
	{"adopt-working-copy", (*liveRig).checkAdoptWorkingCopy, nil},
	{"worker-shares-issue-pod", (*liveRig).checkWorkerSharesIssuePod, nil},
	{"secrets-two-roles-enrolled", (*liveRig).checkSecretsTwoRolesEnrolled, secretsBlocked},
	{"secrets-automatic-grant", (*liveRig).checkSecretsAutomaticGrant, secretsBlocked},
	{"secrets-cross-pod-negative", (*liveRig).checkSecretsCrossPodNegative, secretsBlocked},
	{"secrets-copied-token-negative", (*liveRig).checkSecretsCopiedTokenNegative, secretsBlocked},
	{"secrets-self-enroll-negative", (*liveRig).checkSecretsSelfEnrollNegative, secretsBlocked},
	{"secrets-approval-ask", (*liveRig).checkSecretsApprovalAsk, secretsBlocked},
	{"suspend", (*liveRig).checkSuspend, nil},
	{"role-container-isolation", (*liveRig).checkRoleContainerIsolation, nil},
	{"resume", (*liveRig).checkResume, nil},
	{"stale-plugin-install", (*liveRig).checkStalePluginInstall, stalePluginBlocked},
	{"same-agent-negative", (*liveRig).checkSameAgentNegative, nil},
	{"kill-launcher", (*liveRig).checkKillLauncher, nil},
	{"stale-incarnation", (*liveRig).checkStaleIncarnation, nil},
	{"secrets-old-uid-and-revocation", (*liveRig).checkSecretsOldUIDAndRevocation, secretsBlocked},
	{"respawn-before-register", (*liveRig).checkRespawnBeforeRegister, nil},
	{"independent-provision", (*liveRig).checkIndependentProvision, nil},
	{"second-owner", (*liveRig).checkSecondOwner, nil},
	{"repository-cache", (*liveRig).checkRepositoryCache, nil},
	{"cache-second-pod", (*liveRig).checkCacheSecondPod, nil},
	{"cache-lost-volume", (*liveRig).checkCacheLostVolume, nil},
	{"re-adopt", (*liveRig).checkReAdopt, nil},
	{"orphan-sweep", (*liveRig).checkOrphanSweep, nil},
	{"cleanup-reaps-the-issue", (*liveRig).checkCleanupReapsTheIssue, nil},
	{"displaced", (*liveRig).checkDisplaced, nil},
	{"postgres-resume", (*liveRig).checkPostgresResume, nil},
	{"issue-suspend-resume", (*liveRig).checkIssueSuspendResume, nil},
}

// The runtime's settings for the run: the boot timeout covers a Karpenter node coming up and the
// worker image's pull, which is what a first pod on an empty pool waits for.
const (
	liveBootTimeout   = 5 * time.Minute
	liveGrace         = 15 * time.Second
	liveProbeInterval = 10 * time.Second
	liveAdoptTimeout  = time.Minute
	// liveRunningLimit bounds a launch's wait for its pod to run and its Sandbox to be Ready.
	liveRunningLimit = 15 * time.Minute
	// liveGoneLimit bounds a wait for a pod or an object to be deleted.
	liveGoneLimit = 5 * time.Minute
	// liveSettle is how long a check keeps listening for observations it must not see.
	liveSettle = 2*liveProbeInterval + 5*time.Second
)

// liveIssueVolume is the issue volume's size, the daemon configuration's default. The run sets no
// scheduling beyond the Legion pool the runtime selects: no pod asks for another pod's node or
// keeps off one (every issue pod owns its volume, LEGION-632), and every pod reserves what
// liveResources gives its roles, so the scheduler, and Karpenter under the `legion` NodePool's
// floor (karpenter.k8s.aws/instance-cpu Gt 3, set in the deployment repository), place it where
// the pool has room.
var liveIssueVolume = resource.MustParse("20Gi")

// liveRepositoryCache is the daemon's default repository cache (config.DefaultRepositoryCache), as
// sandboxOptions translates it.
func liveRepositoryCache() RepositoryCache {
	return RepositoryCache{
		Volume: resource.MustParse("20Gi"), Refresh: 10 * time.Minute, Retain: 168 * time.Hour,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
		},
	}
}

// mappedRepositories is the run's mapping as the runtime reads it.
func (r *liveRig) mappedRepositories() []ghrepo.Repository {
	r.mappedMu.Lock()
	defer r.mappedMu.Unlock()
	return slices.Clone(r.mapped)
}

// mapRepository adds repo to the run's mapping, as Dispatch's mapping names a repository before
// any issue of it launches.
func (r *liveRig) mapRepository(repo ghrepo.Repository) {
	r.mappedMu.Lock()
	defer r.mappedMu.Unlock()
	if !slices.Contains(r.mapped, repo) {
		r.mapped = append(r.mapped, repo)
	}
}

// liveOverrides are the roles whose reservation the rig sets itself, as a deployment's
// `runtime.kubernetes.resources.<role>` does, field by field: the implementer's and tester's cpu
// and memory, the tester's ephemeral-storage limit AND request beside them (both explicitly
// configured and equal — see liveResources' own doc comment for why only one role gets this), the
// reviewer's memory alone (its cpu stays the default). The architect, planner and merger keep
// config.DefaultResources(), so the root's pod carries a reservation from each path — an
// override, a default, and one with a field of each — and the `resources` check tells them apart,
// the disk bound's two paths included. The stub agent sleeps, so the overrides size the pods down
// from the defaults' 3 CPU and 19 GiB to 2.5 CPU and 6 GiB, which nproc and MemTotal in the pod
// are then checked against.
var liveOverrides = map[claim.Role]config.RoleResources{
	claim.RoleImplementer: {CPU: "500m", Memory: "1Gi"},
	claim.RoleTester:      {CPU: "500m", Memory: "1Gi", EphemeralStorage: "2Gi", EphemeralStorageRequest: "2Gi"},
	claim.RoleReviewer:    {Memory: "1Gi"},
}

// liveResources are the reservations the rig hands the runtime (Options.Resources): the daemon's
// defaults as it translates them (reservations, manifest_test.go) with liveOverrides applied field
// by field, as resolveKubernetes settles a file's `resources` block. Cpu and memory are each the
// request and the limit alike; the ephemeral-storage limit and request are set apart except where
// liveOverrides gives both explicitly (the tester, above) — every other role keeps
// config.DefaultResources()'s own split, EphemeralStorageRequest 1Gi under whatever limit the role
// (default or liveOverrides) configures.
//
// GKE admits a container's ephemeral-storage limit equal to its request, discarding whatever
// higher limit was configured (confirmed live, 2026-10-17, on a bare debug pod and every role
// container of a real production issue pod); `checkResources`' own ephemeralStorageClamped
// parameter asserts that admitted reality rather than the originally-configured limit, for every
// role whose request and limit differ. Raising a role's own ephemeral_storage_request to match its
// limit — this rig's own tester override — defeats that clamp for that one role alone, proving the
// mechanism an operator would reach for works correctly. It cannot be the fix for every role at
// once, and that is its own, separate, structural finding, confirmed live the same day: equalizing
// every role's request to its own configured limit (architect/planner/reviewer/merger at 10Gi,
// implementer at 20Gi, tester at 30Gi, the two workspace-init containers already at 10Gi each) sums
// to roughly 110Gi requested across the root pod's eight containers, and GKE Autopilot's Warden
// admission webhook refused to schedule the pod at all: "Total ephemeral-storage requested by
// containers … is higher than the Autopilot maximum of 10Gi" (autogke-pod-limit-constraints).
// Because Legion runs every role of one issue in containers of a single pod, that 10Gi is a shared
// budget across all six roles together, not a per-role one — config.DefaultResources()'s 10-30Gi
// per-role bounds, meant as the per-role disk ceiling, cannot be realized simultaneously for more
// than a small fraction of the roles on GKE Autopilot as the runtime is architected today, whatever
// an operator configures: a gap for the daemon's resource model or its pod layout to resolve, not
// something a deployment's `legion.yaml` can configure around, and not something this harness can
// fix from a test rig.
func (r *liveRig) liveResources() map[claim.Role]corev1.ResourceRequirements {
	resources := reservations()
	for role, override := range liveOverrides {
		requirements := resources[role]
		for name, value := range map[corev1.ResourceName]string{corev1.ResourceCPU: override.CPU, corev1.ResourceMemory: override.Memory} {
			if value == "" {
				continue
			}
			quantity := resource.MustParse(value)
			requirements.Requests[name] = quantity
			requirements.Limits[name] = quantity.DeepCopy()
		}
		if override.EphemeralStorage != "" {
			requirements.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(override.EphemeralStorage)
		}
		if override.EphemeralStorageRequest != "" {
			requirements.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(override.EphemeralStorageRequest)
		}
		resources[role] = requirements
	}
	return resources
}

// overridden reports whether role's reservation field name comes from liveOverrides rather than
// the defaults: for ephemeral-storage, whether either side of the bound does.
func overridden(role claim.Role, name corev1.ResourceName) bool {
	override, ok := liveOverrides[role]
	if !ok {
		return false
	}
	switch name {
	case corev1.ResourceCPU:
		return override.CPU != ""
	case corev1.ResourceEphemeralStorage:
		return override.EphemeralStorage != "" || override.EphemeralStorageRequest != ""
	}
	return override.Memory != ""
}

// The run's one provider key: the variable its agents' Oh My Pi gets, and the key of the providers
// Secret (ProvidersSecretName) the script creates for the run, holding a value no model route reads.
const (
	liveProviderKey        = "STAGE4A_PROVIDER_KEY"
	liveProvidersSecretKey = "stage4a"
	// liveAnthropicKeyVariable is both the providers Secret's key and the variable Oh My Pi reads
	// it as, under LEGION_E2E_MODEL_ROUTE=key-file (runtimeOptions): mirroring the live GKE route's
	// shape, where provider_keys names ANTHROPIC_API_KEY directly rather than a gateway.
	liveAnthropicKeyVariable = "ANTHROPIC_API_KEY"
)

// fixtureConfigMap is the ConfigMap the operator fixture's pod.yml names; each run creates its own
// copy, named for its project (LEGION_E2E_OPERATOR_CONFIGMAP), since Stage 4b's driver shares the
// namespace and a common name would let one run delete another's route.
const fixtureConfigMap = "legion-operator-route"

// The marker is the runtime locator's process incarnation: an unchanged pod UID plus the launch
// generation, prefixed with the role that launched it since the marker is one issue pod's, shared
// by every resident role (SandboxName). A role restart keeps the pod UID but must append a new
// incarnation. It lives on the pod's own disk, so it says nothing about any earlier pod: what a
// role carries across a pod replacement is its session, which the table holds (recordSession).
var stubAgent = []string{"/bin/sh", "-c", `printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER" && exec sleep infinity`, "stage4a-stub"}

// liveEnv is what the script hands the harness.
type liveEnv struct {
	runtimeKubeconfig, runtimeContext, operatorContext string
	namespace, project, claimProject, image            string
	repo                                               ghrepo.Repository
	// podReachability is LEGION_E2E_POD_REACHABILITY: "aws-private-ip" or "bridge" — how pods reach
	// the devbox, which networkPathFailure's diagnostic branches on. streamHost is where the
	// harness's own listener binds: the devbox's private address directly reachable from pods
	// (aws-private-ip), or loopback behind the reverse-tunnel bridge every pod instead dials by
	// streamAdvertiseHost (bridge). Every pod's shim is handed streamAdvertiseHost, never
	// streamHost, when the two differ.
	podReachability                             string
	streamHost, streamAdvertiseHost, streamPort string
	appID, appKeyName                           string
	// storageClass is the issue volumes' StorageClass: AWS's gp2, GKE's standard-rwo, or whatever
	// else the cluster under test defaults issue volumes to. No default of its own: the plan
	// behind LEGION_E2E_STORAGE_CLASS is the operator's to pick, never this harness's to guess.
	storageClass string
	// modelRoute is LEGION_E2E_MODEL_ROUTE: "gateway" (the operator route's pod.yml/models.yml,
	// a projected ServiceAccount token authenticating to a model gateway) or "key-file" (no
	// gateway: ANTHROPIC_API_KEY reaches the agent as a provider key the providers Secret holds,
	// mirroring the live GKE route's shape). readLiveEnv refuses any other value.
	modelRoute string
	// platform is LEGION_E2E_PLATFORM: "gke-autopilot" or "eks", the cluster's own platform,
	// independent of runtimeIdentityKind (how the runtime authenticates) and podReachability (how
	// pods reach the devbox) — a cluster could in principle combine any of these differently. Every
	// platform-specific relaxation in this file (gke(), the cpu floor, the ephemeral-storage
	// equalization, the extended-duration-pods affinity, the cgroup-quota-over-nproc choice) keys
	// off this field alone, never off identity or reachability, which happened to coincide with it
	// on every cluster this harness has run against so far but are not the same fact. readLiveEnv
	// refuses any other value.
	platform string
	// runtimeIdentityKind is LEGION_E2E_RUNTIME_IDENTITY_KIND: "aws-iam-role" (an assumed IAM role
	// mapped to the legion-daemon RBAC group, EKS's shape) or "kubernetes-service-account" (a
	// plain system:serviceaccount:<namespace>:legion-daemon identity, GKE's shape with no IAM
	// mapping involved). checkIdentity's assertions branch on it; readLiveEnv refuses any other
	// value.
	runtimeIdentityKind string
	// repoSecondOwner is LEGION_E2E_REPO_SECOND_OWNER: a repository under another owner than
	// repo's, both Apps installed on it, where second-owner provisions an issue.
	repoSecondOwner    ghrepo.Repository
	record, work, from string
	// operatorPodFile is the run's copy of deploy/kubernetes/operator-route/pod.yml, its token
	// audience filled in, the operator's pod every launch carries; operatorConfigMap is the run's
	// copy of the ConfigMap it names.
	operatorPodFile, operatorConfigMap string
	// The agent-secrets checks' inputs: the production broker, the email of the person who
	// approves the run's credential request for LEGION_E2E_APPROVAL, the sha256 of the automatic
	// rule's dummy value, the checkout's agent-secrets binary, and the token file the script mints
	// for LEGION_E2E_AGENT_SECRETS_ACCOUNT, which the harness logs in with as the daemon does, no
	// person approving it. Every field here is read with os.Getenv, unlike the rest of liveEnv: unset
	// is a blocked run of the secrets-* checks, never a refusal to start (secretsBlocked).
	agentSecretsURL, agentSecretsOperator, agentSecretsAutoSHA, agentSecretsBin, agentSecretsTokenFile string
	// The scratch Postgres the script started, which every pod of the run keeps its sessions in:
	// the file holding its URL at streamAdvertiseHost, which the script also wrote into the
	// providers Secret under liveSessionsSecretKey for every pod to dial as it dials the worker
	// stream, and the address it listens on there. sessionDSNLocalFile holds the same database's
	// URL at streamHost instead — loopback under bridge, where the script's own docker container
	// actually binds, reachable from the devbox the harness runs on even though
	// streamAdvertiseHost (the bridge's cluster-DNS name) is not — for the harness's own direct
	// connection in writeSession, never handed to a pod.
	sessionDSNFile, sessionDBAddress, sessionDSNLocalFile string
	// The stale-plugin-install checkpoint's own inputs: the devbox paths of this checkout's packed
	// pi-legion and pi-envoy and their versions. Its negative control names the published
	// pi-legion@9.6.1 by exact version — no devbox path of its own — so bun resolves it from the
	// registry inside the pod. Read with os.Getenv, unlike the rest of liveEnv: unset skips the
	// checkpoint with a clear message, never a refusal to start, so an older invocation of the
	// script still runs every checkpoint it knows (stalePluginBlocked).
	piLegionTarball, piEnvoyTarball, piLegionVersion, piEnvoyVersion string
}

func readLiveEnv(t *testing.T) liveEnv {
	t.Helper()
	get := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			fmt.Printf("CHECK identity: FAIL: %s is unset; the harness refuses to start (scripts/e2e/stage4a-sandbox-runtime.sh sets it)\n", name)
			t.FailNow()
		}
		return value
	}
	env := liveEnv{
		runtimeKubeconfig:     get("LEGION_E2E_RUNTIME_KUBECONFIG"),
		runtimeContext:        get("LEGION_E2E_RUNTIME_CONTEXT"),
		operatorContext:       get("LEGION_E2E_OPERATOR_CONTEXT"),
		namespace:             get("LEGION_E2E_NAMESPACE"),
		project:               get("LEGION_E2E_PROJECT"),
		image:                 get("LEGION_E2E_IMAGE"),
		podReachability:       get("LEGION_E2E_POD_REACHABILITY"),
		streamHost:            get("LEGION_E2E_STREAM_HOST"),
		streamAdvertiseHost:   get("LEGION_E2E_STREAM_ADVERTISE_HOST"),
		streamPort:            get("LEGION_E2E_STREAM_PORT"),
		storageClass:          get("LEGION_E2E_STORAGE_CLASS"),
		modelRoute:            get("LEGION_E2E_MODEL_ROUTE"),
		platform:              get("LEGION_E2E_PLATFORM"),
		runtimeIdentityKind:   get("LEGION_E2E_RUNTIME_IDENTITY_KIND"),
		appID:                 get("LEGION_E2E_IMPLEMENT_APP_ID"),
		appKeyName:            get("LEGION_E2E_IMPLEMENT_APP_KEY"),
		record:                get("LEGION_E2E_RECORD"),
		work:                  get("LEGION_E2E_WORK"),
		from:                  os.Getenv("LEGION_E2E_FROM"),
		operatorPodFile:       get("LEGION_E2E_OPERATOR_POD"),
		operatorConfigMap:     get("LEGION_E2E_OPERATOR_CONFIGMAP"),
		agentSecretsURL:       os.Getenv("LEGION_E2E_AGENT_SECRETS_URL"),
		agentSecretsOperator:  os.Getenv("LEGION_E2E_AGENT_SECRETS_OPERATOR"),
		agentSecretsAutoSHA:   os.Getenv("LEGION_E2E_AGENT_SECRETS_AUTO_SHA256"),
		agentSecretsBin:       os.Getenv("LEGION_E2E_AGENT_SECRETS_BIN"),
		agentSecretsTokenFile: os.Getenv("LEGION_E2E_AGENT_SECRETS_TOKEN_FILE"),
		sessionDSNFile:        get("LEGION_E2E_SESSION_DSN_FILE"),
		sessionDBAddress:      get("LEGION_E2E_SESSION_DB_ADDRESS"),
		sessionDSNLocalFile:   get("LEGION_E2E_SESSION_DSN_LOCAL_FILE"),
		piLegionTarball:       os.Getenv("LEGION_E2E_PI_LEGION_TARBALL"),
		piEnvoyTarball:        os.Getenv("LEGION_E2E_PI_ENVOY_TARBALL"),
		piLegionVersion:       os.Getenv("LEGION_E2E_PI_LEGION_VERSION"),
		piEnvoyVersion:        os.Getenv("LEGION_E2E_PI_ENVOY_VERSION"),
	}
	repo, err := ghrepo.Parse("LEGION_E2E_REPO", get("LEGION_E2E_REPO"))
	if err != nil {
		fmt.Printf("CHECK identity: FAIL: %v\n", err)
		t.FailNow()
	}
	env.repo = repo
	if env.modelRoute != "gateway" && env.modelRoute != "key-file" {
		fmt.Printf("CHECK identity: FAIL: LEGION_E2E_MODEL_ROUTE must be gateway or key-file, not %q\n", env.modelRoute)
		t.FailNow()
	}
	if env.runtimeIdentityKind != "aws-iam-role" && env.runtimeIdentityKind != "kubernetes-service-account" {
		fmt.Printf("CHECK identity: FAIL: LEGION_E2E_RUNTIME_IDENTITY_KIND must be aws-iam-role or kubernetes-service-account, not %q\n", env.runtimeIdentityKind)
		t.FailNow()
	}
	if env.platform != "gke-autopilot" && env.platform != "eks" {
		fmt.Printf("CHECK identity: FAIL: LEGION_E2E_PLATFORM must be gke-autopilot or eks, not %q\n", env.platform)
		t.FailNow()
	}
	if env.podReachability != "aws-private-ip" && env.podReachability != "bridge" {
		fmt.Printf("CHECK identity: FAIL: LEGION_E2E_POD_REACHABILITY must be aws-private-ip or bridge, not %q\n", env.podReachability)
		t.FailNow()
	}
	if env.repoSecondOwner, err = ghrepo.Parse("LEGION_E2E_REPO_SECOND_OWNER", get("LEGION_E2E_REPO_SECOND_OWNER")); err != nil {
		fmt.Printf("CHECK identity: FAIL: %v\n", err)
		t.FailNow()
	}
	if strings.EqualFold(env.repoSecondOwner.Owner(), repo.Owner()) {
		fmt.Printf("CHECK identity: FAIL: LEGION_E2E_REPO_SECOND_OWNER %s is under %s, LEGION_E2E_REPO's owner\n", env.repoSecondOwner, repo.Owner())
		t.FailNow()
	}
	if !strings.HasPrefix(env.project, "s4a-") {
		fmt.Printf("CHECK identity: FAIL: the run project %q lacks the reserved prefix s4a-\n", env.project)
		t.FailNow()
	}
	// The claim token's project is a project token, [a-z0-9]+; the label keeps the dashes.
	env.claimProject = strings.ReplaceAll(env.project, "-", "")
	entry := func(check liveCheck) bool { return check.name == env.from }
	if env.from != "" && (env.from == "identity" || env.from == "stale-incarnation" || !slices.ContainsFunc(liveChecks, entry)) {
		fmt.Printf("CHECK identity: FAIL: STAGE4A_FROM=%s is not an entry point (any check but identity and stale-incarnation, which rides kill-pod)\n", env.from)
		t.FailNow()
	}
	return env
}

// readOperatorPod reads the operator fixture through the daemon's own loader, with its ConfigMap named
// for this run.
func readOperatorPod(env liveEnv) (Pod, error) {
	pod, err := config.ReadPodFile(env.operatorPodFile)
	if err != nil {
		return Pod{}, err
	}
	renamed := 0
	for _, volume := range pod.Volumes {
		if volume.ConfigMap != nil && volume.ConfigMap.Name == fixtureConfigMap {
			volume.ConfigMap.Name = env.operatorConfigMap
			renamed++
		}
	}
	if renamed != 1 {
		return Pod{}, fmt.Errorf("%s names ConfigMap %s in %d volumes, want one", env.operatorPodFile, fixtureConfigMap, renamed)
	}
	return Pod(pod), nil
}

// claimState is where a claim stands, as the daemon's machine would record it.
type claimState int

const (
	stateNone claimState = iota
	stateRunning
	stateSuspended
	stateDead
	stateReleased
)

// liveClaim is one claim of the run and what the harness recorded for it.
type liveClaim struct {
	token             claim.Token
	tree, issue, name string
	role              claim.Role
	// repo is the claim's repository when it is not LEGION_E2E_REPO (second-owner's).
	repo   ghrepo.Repository
	marker string
	// session is the claim's recorded session, the path every resume of it names. No pod ever
	// holds a file there — the disk it would have sat on dies with the pod — so the role launcher
	// finds it in the session table alone; recorded says the harness has put it there.
	session   string
	recorded  bool
	state     claimState
	gen       uint64
	bootToken string
	// loc is the running process; last is the latest locator, kept as Resume's hint once the
	// process stopped.
	loc, last *runtime.Locator
}

// minted is one boot token the harness minted, for one generation of one claim.
type minted struct {
	claim claim.Token
	gen   uint64
	hash  string
	armed bool
}

// registration is a hello the listener registered: the claim and generation of the Hello event,
// the hash of the token the resolver accepted for it, and — when the shim's hello carried one —
// the pod's agent-secrets session identity.
type registration struct {
	claim    claim.Token
	gen      uint64
	hash     string
	at       time.Time
	identity *stream.AgentSecretsIdentity
}

// registry is the harness's store of boot tokens, which outlives every listener and runtime of
// the run as the daemon's database does. Its resolver accepts only each claim's current,
// armed generation, and records every hello it judges.
type registry struct {
	mu       sync.Mutex
	byToken  map[string]minted
	current  map[claim.Token]uint64
	pending  map[claim.Token]string
	accepted []registration
	refused  map[claim.Token]int
	changed  chan struct{}
}

func newRegistry() *registry {
	return &registry{
		byToken: map[string]minted{}, current: map[claim.Token]uint64{}, pending: map[claim.Token]string{},
		refused: map[claim.Token]int{}, changed: make(chan struct{}),
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mint makes the claim's next generation current. An unarmed token is refused as unknown, so a
// pod on it can never register.
func (g *registry) mint(c claim.Token, armed bool) (string, uint64) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(raw)
	g.mu.Lock()
	defer g.mu.Unlock()
	gen := g.current[c] + 1
	g.current[c] = gen
	g.byToken[token] = minted{claim: c, gen: gen, hash: tokenHash(token), armed: armed}
	return token, gen
}

func (g *registry) resolve(bootToken string) (claim.Token, uint64, bool, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.byToken[bootToken]
	switch {
	case !ok:
		return "", 0, false, false, nil
	case !m.armed:
		g.refused[m.claim]++
		return "", 0, false, false, nil
	case m.gen != g.current[m.claim]:
		g.refused[m.claim]++
		return m.claim, m.gen, true, true, nil
	}
	g.pending[m.claim] = m.hash
	return m.claim, m.gen, false, true, nil
}

func (g *registry) hello(event stream.Hello) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accepted = append(g.accepted, registration{
		claim: event.Claim, gen: event.Generation, hash: g.pending[event.Claim], at: time.Now(), identity: event.AgentSecrets,
	})
	close(g.changed)
	g.changed = make(chan struct{})
}

// await is the claim's registration at gen after since, within limit.
func (g *registry) await(c claim.Token, gen uint64, since time.Time, limit time.Duration) (registration, bool) {
	deadline := time.After(limit)
	for {
		g.mu.Lock()
		for _, reg := range g.accepted {
			if reg.claim == c && reg.gen == gen && !reg.at.Before(since) {
				g.mu.Unlock()
				return reg, true
			}
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return registration{}, false
		}
	}
}

func (g *registry) registrations(c claim.Token) []registration {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []registration
	for _, reg := range g.accepted {
		if reg.claim == c {
			out = append(out, reg)
		}
	}
	return out
}

func (g *registry) refusals(c claim.Token) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[c]
}

// observed is one observation the runtime delivered, numbered in delivery order across every
// runtime instance of the run.
type observed struct {
	runtime.Observation
	seq int
}

type observations struct {
	mu      sync.Mutex
	list    []observed
	changed chan struct{}
}

func (o *observations) add(obs runtime.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, observed{Observation: obs, seq: len(o.list) + 1})
	close(o.changed)
	o.changed = make(chan struct{})
}

// mark is the sequence number of the latest observation: what came after it has a larger one.
func (o *observations) mark() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.list)
}

func (o *observations) since(after int) []observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.list[after:])
}

// await is the first observation after `after` that match accepts, within limit.
func (o *observations) await(after int, limit time.Duration, match func(runtime.Observation) bool) (observed, bool) {
	deadline := time.After(limit)
	for {
		o.mu.Lock()
		for _, obs := range o.list[after:] {
			if match(obs.Observation) {
				o.mu.Unlock()
				return obs, true
			}
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return observed{}, false
		}
	}
}

// logRecorder keeps every record the runtime and the listener log, in memory for the checks
// that read them and as JSON lines in the run's evidence.
type logRecorder struct {
	mu      sync.Mutex
	json    slog.Handler
	records []map[string]string
}

type recordingHandler struct {
	rec   *logRecorder
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordingHandler{rec: h.rec, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h recordingHandler) Handle(ctx context.Context, record slog.Record) error {
	fields := map[string]string{"msg": record.Message, "level": record.Level.String()}
	add := func(a slog.Attr) bool { fields[a.Key] = a.Value.String(); return true }
	for _, a := range h.attrs {
		add(a)
	}
	record.Attrs(add)
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	h.rec.records = append(h.rec.records, fields)
	return h.rec.json.WithAttrs(h.attrs).Handle(ctx, record)
}

func (l *logRecorder) find(msg string) (map[string]string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i]["msg"] == msg {
			return l.records[i], true
		}
	}
	return nil, false
}

// liveEnrollment is one pod's agent-secrets enrollment as the harness (playing the daemon's
// machine) holds it: the broker's id, and the pod incarnation it was enrolled for — so a claim
// relaunched since (suspend/resume, kill/relaunch) is recognized as needing a fresh enrollment.
type liveEnrollment struct {
	id, incarnation string
}

// liveRig is the run: the two identities, the registry, and the current runtime instance.
type liveRig struct {
	t      *testing.T
	env    liveEnv
	ctx    context.Context
	cancel context.CancelFunc

	rc   *rest.Config // the runtime identity, as the runtime gets it
	kube kubernetes.Interface
	dyn  dynamic.Interface

	tokens   implementTokens
	identity runtime.GitIdentity
	// defaultBranch is LEGION_E2E_REPO's default branch as GitHub names it (resolveApp), which
	// every launch names and independent-provision holds each workspace to; secondDefaultBranch is
	// LEGION_E2E_REPO_SECOND_OWNER's, which second-owner's launch names.
	defaultBranch, secondDefaultBranch string
	prompt                             string
	// pod is the operator's pod every launch carries: the fixture, as the daemon reads it.
	pod  Pod
	reg  *registry
	obs  *observations
	logs *logRecorder
	log  *slog.Logger
	// trees is the durable state every runtime of the run reads (Options.Store): every tree live
	// until a check records its cleanup confirmed.
	trees *fakeStore
	// mapped is the repositories the run maps (Options.Mapped): each added just before its first
	// issue pod launches (root-ready, second-owner), so no sweep makes a cache the run's first
	// launch of that repository is to make.
	mappedMu sync.Mutex
	mapped   []ghrepo.Repository

	// The current runtime instance and its listener; stop ends both.
	rt      *Runtime
	ln      *stream.Listener
	stop    context.CancelFunc
	stopped sync.WaitGroup

	// claims are the run's claims in newLiveRig's order, each tree's root before its other claims.
	claims []*liveClaim
	// What kill-pod recorded for stale-incarnation.
	killed struct {
		old, fresh runtime.Locator
		gone       observed
	}

	// secrets is the broker client, nil when this run's inputs leave it unconfigured or the token
	// login started in newLiveRig was refused (secretsBlocked). secretsBlockReason is empty exactly
	// when secrets is set: it holds that refusal for secretsBlocked to report through the same
	// SKIPPED-BLOCKED path a missing env var takes, since a login outcome discovered at rig setup has
	// nowhere else to surface before any check runs.
	// enrollments is the harness's own enrollment of each claim's running pod, as the daemon's
	// machine holds it; grants and requests are what checkSecretsAutomaticGrant records for
	// checkSecretsCrossPodNegative to attack.
	secrets            *agentsecrets.Client
	secretsBlockReason string
	enrollments        map[claim.Token]liveEnrollment
	grants             map[claim.Token]string
	requests           map[claim.Token]string
	// sessionStore is the providers Secret's key every runtime of the run keeps its sessions in
	// (Options.SessionDSNKey, runtime.kubernetes.session_store postgres): liveSessionsSecretKey,
	// the scratch database's URL the script wrote there, from the rig's first runtime on. The
	// daemon loads no other store, so there is no run of this harness without one.
	sessionStore string
}

func TestStage4aSandboxRuntimeLive(t *testing.T) {
	env := readLiveEnv(t)
	r := newLiveRig(t, env)
	defer r.close()

	skipping := env.from != ""
	for _, check := range liveChecks {
		if skipping && check.name == env.from {
			skipping = false
		}
		if skipping && check.name != "identity" {
			fmt.Printf("CHECK %s: SKIPPED (development run from %s)\n", check.name, env.from)
			continue
		}
		r.run(check)
	}
	fmt.Println("stage 4a harness: every check passed")
}

func (r *liveRig) run(check liveCheck) {
	if check.blocked != nil {
		if reason := check.blocked(r); reason != "" {
			fmt.Printf("CHECK %s: SKIPPED-BLOCKED: %s\n", check.name, reason)
			return
		}
	}
	fmt.Printf("== %s\n", check.name)
	started := time.Now()
	if err := check.run(r); err != nil {
		fmt.Printf("CHECK %s: FAIL: %v\n", check.name, err)
		r.t.FailNow()
	}
	fmt.Printf("CHECK %s: PASS (%s)\n", check.name, time.Since(started).Round(time.Second))
}

// note prints one piece of evidence, naming the identity that observed it: runtime (the Legion
// daemon's restricted identity, through the runtime or the harness's own client under it),
// operator (the admin context, through kubectl), or harness (the listener and its resolver).
func note(who, format string, args ...any) {
	fmt.Printf("   [%s] %s\n", who, fmt.Sprintf(format, args...))
}

func newLiveRig(t *testing.T, env liveEnv) *liveRig {
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRig{
		t: t, env: env, ctx: ctx, cancel: cancel, reg: newRegistry(), obs: &observations{changed: make(chan struct{})}, trees: newFakeStore(),
		enrollments: map[claim.Token]liveEnrollment{}, grants: map[claim.Token]string{}, sessionStore: liveSessionsSecretKey,
	}
	fail := func(format string, args ...any) {
		fmt.Printf("CHECK identity: FAIL: "+format+"\n", args...)
		t.FailNow()
	}
	logFile, err := os.OpenFile(filepath.Join(env.work, "runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail("%v", err)
	}
	t.Cleanup(func() { logFile.Close() })
	r.logs = &logRecorder{json: slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})}
	r.log = slog.New(recordingHandler{rec: r.logs})

	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: env.runtimeKubeconfig}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: env.runtimeContext}).ClientConfig()
	if err != nil {
		fail("the runtime context %s in %s: %v", env.runtimeContext, env.runtimeKubeconfig, err)
	}
	r.rc = rc
	// The harness's own reads under the runtime identity ask the rules of every namespace, so
	// they run faster than client-go's default rate; the runtime paces its own (clientQPS).
	own := rest.CopyConfig(rc)
	own.QPS, own.Burst = 50, 100
	if r.kube, err = kubernetes.NewForConfig(own); err != nil {
		fail("%v", err)
	}
	if r.dyn, err = dynamic.NewForConfig(own); err != nil {
		fail("%v", err)
	}
	if agentSecretsBlockReason(env.agentSecretsURL, env.agentSecretsOperator, env.agentSecretsAutoSHA, env.agentSecretsBin, env.agentSecretsTokenFile) == "" {
		// The run logs in as the daemon does, with the ServiceAccount token the script minted; no
		// person approves it, and the client keeps logging in for as long as the run lasts.
		client := &agentsecrets.Client{URL: env.agentSecretsURL, TokenFile: env.agentSecretsTokenFile, Log: r.log}
		if err := client.Start(r.ctx); err != nil {
			r.secretsBlockReason = fmt.Sprintf("the token login was refused: %v", err)
		} else {
			r.secrets = client
		}
	}

	if r.pod, err = readOperatorPod(env); err != nil {
		fail("%v", err)
	}
	note("operator", "pod from %s: service account %s, %d volumes, %d mounts, ConfigMap %s",
		env.operatorPodFile, r.pod.ServiceAccount, len(r.pod.Volumes), len(r.pod.VolumeMounts), env.operatorConfigMap)

	prompt := filepath.Join(env.work, "role-prompt.md")
	if err := os.WriteFile(prompt, []byte("The Stage 4a stub agent; no model reads this.\n"), 0o600); err != nil {
		fail("%v", err)
	}
	r.prompt = prompt

	cacheIssue := cacheRunIssue(env.project)
	for _, c := range []struct {
		name, tree, issue string
		role              claim.Role
	}{
		{"root", "S4A-1", "S4A-1", claim.RoleArchitect},
		{"worker", "S4A-1", "S4A-1", claim.RoleImplementer},
		{"second", "S4A-1", "S4A-1", claim.RoleTester},
		{"fresh", "S4A-1", "S4A-1", claim.RoleReviewer},
		// orphan is a separate issue pod: its unrecorded Sandbox can be swept or deleted without
		// taking the root issue's resident roles with it. Its role is its own tree's architect, the
		// claim a tree's first launch is in the daemon.
		{"orphan", "S4A-4", "S4A-4", claim.RoleArchitect},
		// sessions is postgres-resume's own tree: the one claim whose session the check writes into
		// the table itself, dated before its pod's workspace, rather than letting the rig record it.
		{"sessions", "S4A-5", "S4A-5", claim.RoleArchitect},
		// root2 and child2 are two issues of one tree, each in a Sandbox and on a volume of its own
		// (independent-provision).
		{"root2", "S4A-2", "S4A-2", claim.RoleArchitect},
		{"child2", "S4A-2", "S4A-3", claim.RolePlanner},
		// displaced and displacer are two roles of one more issue pod, which the displacer's launch
		// replaces while the displaced role still runs in it (displaced).
		{"displaced", "S4A-6", "S4A-6", claim.RoleArchitect},
		{"displacer", "S4A-6", "S4A-6", claim.RoleImplementer},
		// cache-b is one more issue pod on the run's repository, launched while its cache is Ready
		// (cache-second-pod) and again after its volume is lost (cache-lost-volume). Its issue is
		// its run's own (cacheRunIssue), since cache-lost-volume pushes the issue's branch onto the
		// repository every run shares.
		{"cache-b", cacheIssue, cacheIssue, claim.RoleArchitect},
		// second-owner is an issue pod on the repository of the run's other owner (second-owner).
		{"second-owner", "S4A-7", "S4A-7", claim.RoleImplementer},
		// setup-negative is the throwaway issue the setup-negative check launches on a second
		// runtime whose setup exits 7, then deletes.
		{"setup-negative", "S4A-8", "S4A-8", claim.RoleArchitect},
		// closed is the issue whose close takes its whole pod, disk included, and whose next launch
		// brings a disk of its own while the agent carries on from the table (issue-suspend-resume).
		{"closed", "S4A-9", "S4A-9", claim.RoleArchitect},
	} {
		token, err := claim.NewToken(env.claimProject, c.issue, c.role)
		if err != nil {
			fail("%v", err)
		}
		r.claims = append(r.claims, &liveClaim{
			token: token, tree: c.tree, issue: c.issue, role: c.role, name: c.name,
			// The marker sits at the root of the issue's volume: TreeRoot is the one path every
			// role container of the pod mounts read-write (manifest.go's role mounts), so the six
			// roles of an issue share the one file, and it is the pod's own ephemeral disk, so it
			// dies with the pod — the property this file's header claims. No pod mounts
			// ompSessionsDir any more, which is why the marker is not there.
			marker:  TreeRoot + "/" + SandboxName(token) + ".marker",
			session: ompSessionsDir + "/" + SandboxName(token) + "-" + string(c.role) + ".jsonl",
		})
	}
	r.claim("second-owner").repo = env.repoSecondOwner
	return r
}

func (r *liveRig) claim(name string) *liveClaim {
	i := slices.IndexFunc(r.claims, func(c *liveClaim) bool { return c.name == name })
	return r.claims[i]
}

func (r *liveRig) close() {
	r.stopRuntime()
	r.cancel()
}

// sameLocator compares two locators by what they name; the struct holds pointers.
func sameLocator(a, b runtime.Locator) bool {
	return a.Runtime == b.Runtime && a.Claim == b.Claim && a.Incarnation == b.Incarnation &&
		(a.Sandbox == nil) == (b.Sandbox == nil) && (a.Sandbox == nil || *a.Sandbox == *b.Sandbox)
}

// runtimeOptions are the Options every runtime of the run is built from, as a daemon boot builds
// them: the operator's pod, the run's provider key, the stub agent, the shared setup command, and
// the worker stream at address, through ln.
func (r *liveRig) runtimeOptions(address string, ln *stream.Listener) Options {
	providerKeys := map[string]string{liveProviderKey: liveProvidersSecretKey}
	if r.env.modelRoute == "key-file" {
		// Mirrors the live GKE route's shape: ANTHROPIC_API_KEY is a providers-Secret key of its
		// own name, which the shim exports into the agent's environment alongside
		// STAGE4A_PROVIDER_KEY, and the run's key-file models.yml reads it back from there.
		providerKeys[liveAnthropicKeyVariable] = liveAnthropicKeyVariable
	}
	opts := Options{
		Namespace: r.env.namespace, Project: r.env.project, Image: r.env.image, StorageClass: r.env.storageClass, IssueVolume: liveIssueVolume,
		Resources: r.liveResources(),
		StreamURL: address,
		Tools: Tools{
			GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets",
		},
		// Stage4a drives Runtime directly to prove Kubernetes mechanics; the daemon's store is the
		// in-memory one, which outlives each runtime as Postgres outlives a daemon restart.
		Store:                 r.trees,
		Pod:                   r.pod,
		ProviderKeys:          providerKeys,
		Agent:                 stubAgent,
		Setup:                 liveSetup(r.env.repo),
		BootTimeout:           liveBootTimeout,
		RegistrationIntervals: 3,
		BusyBackoff:           250 * time.Millisecond,
		TerminationGrace:      liveGrace,
		ProbeInterval:         liveProbeInterval,
		AdoptTimeout:          liveAdoptTimeout,
		Tokens:                r.tokens,
		GitHubCredential:      r.tokens.Credential,
		Conns:                 ln,
		Log:                   r.log,
		SessionDSNKey:         r.sessionStore,
		RepositoryCache:       liveRepositoryCache(),
		Mapped:                r.mappedRepositories,
	}
	if r.env.agentSecretsURL != "" {
		opts.AgentSecrets = &AgentSecrets{URL: r.env.agentSecretsURL, Audience: "agent-secrets", TokenExpiry: time.Hour}
	}
	return opts
}

// streamBindAddress is where the harness's own listener binds: the devbox's private address
// directly in AWS mode, or loopback behind the reverse-tunnel bridge in GKE mode
// (LEGION_E2E_POD_REACHABILITY=bridge), since nothing outside the devbox needs to reach this
// address directly there — the bridge's pod and Service do that instead.
func (r *liveRig) streamBindAddress() string {
	return "tcp://" + r.env.streamHost + ":" + r.env.streamPort
}

// streamAddress is the worker stream's address every pod's shim dials: the devbox's private
// address directly in AWS mode, or the reverse-tunnel bridge's in-cluster Service in GKE mode.
// Every runtime of the run is built on it.
func (r *liveRig) streamAddress() string {
	return "tcp://" + r.env.streamAdvertiseHost + ":" + r.env.streamPort
}

// startRuntime binds a fresh listener on streamBindAddress, builds a fresh runtime whose pods are
// handed streamAddress to dial, and registers the runtime's launcher acceptor, as a daemon boot
// does (sandboxRuntime), with an Observe feeding the run's observation record.
func (r *liveRig) startRuntime() error {
	if r.tokens.apps == nil {
		if err := r.resolveApp(); err != nil {
			return err
		}
	}
	ctx, stop := context.WithCancel(r.ctx)
	bind := r.streamBindAddress()
	ln, err := stream.Listen(ctx, bind, r.reg.resolve, stream.Options{RPCTimeout: 30 * time.Second, Log: r.log})
	if err != nil {
		stop()
		holder, _ := exec.Command("ss", "-Hltnp", "sport = :"+r.env.streamPort).CombinedOutput()
		return fmt.Errorf("the worker stream cannot bind %s: %v; the port's holder: %s", bind, err, strings.TrimSpace(string(holder)))
	}
	opts := r.runtimeOptions(r.streamAddress(), ln)
	rt, err := New(ctx, r.rc, opts)
	if err != nil {
		stop()
		return err
	}
	ln.SetLauncherResolver(rt.LauncherResolver())
	observe, err := rt.Observe(ctx)
	if err != nil {
		stop()
		return err
	}
	r.rt, r.ln, r.stop = rt, ln, stop
	r.stopped.Add(2)
	go func() {
		defer r.stopped.Done()
		for obs := range observe {
			r.obs.add(obs)
		}
	}()
	go func() {
		defer r.stopped.Done()
		for event := range ln.Events() {
			if hello, ok := event.(stream.Hello); ok {
				r.reg.hello(hello)
			}
		}
	}()
	return nil
}

// startRuntimeOnce starts the runtime unless one is running.
func (r *liveRig) startRuntimeOnce() error {
	if r.rt != nil {
		return nil
	}
	return r.startRuntime()
}

// stopRuntime ends the current runtime and its listener, as a daemon exit does: the informers,
// the Observe, every connection.
func (r *liveRig) stopRuntime() {
	if r.stop == nil {
		return
	}
	r.stop()
	r.stopped.Wait()
	r.rt, r.ln, r.stop = nil, nil, nil
}

// recordSandbox appends a Sandbox name to the script's record before the Sandbox can exist, so
// the teardown deletes it by its exact name whatever happens next.
func (r *liveRig) recordSandbox(name string) error {
	f, err := os.OpenFile(r.env.record, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, name)
	return err
}

// ---- operator steps ----------------------------------------------------------------------------

// kubectl runs one operator step in the namespace through the admin context. A single retry
// absorbs the one failure mode confirmed transient here (live, 2026-10-17): the operator's
// gke-gcloud-auth-plugin hitting a one-off "internal_failure" refreshing its token mid-run, this
// devbox's own intermittent reach to Google's OAuth endpoint, not a cluster or RBAC problem —
// every other kubectl failure (a real RBAC refusal, a missing object, a context error) is
// returned as before, on the first try.
func (r *liveRig) kubectl(args ...string) (string, error) {
	run := func() (string, error) {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
		defer cancel()
		full := append([]string{"--context", r.env.operatorContext, "-n", r.env.namespace}, args...)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "kubectl", full...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return stdout.String(), fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
	out, err := run()
	if err != nil && strings.Contains(err.Error(), "gke-gcloud-auth-plugin") {
		time.Sleep(2 * time.Second)
		out, err = run()
	}
	return out, err
}

// exec runs a command in a claim's main container, through the admin context.
func (r *liveRig) exec(c *liveClaim, command ...string) (string, error) {
	out, err := r.kubectl(append([]string{"exec", SandboxName(c.token), "-c", string(c.role), "--"}, command...)...)
	return strings.TrimSpace(out), err
}

// issueDiskName is the name Kubernetes gives the claim of an issue pod's own disk: the pod's name
// and the volume's (issueVolume), the pod being the claim's Sandbox name (SandboxName).
func issueDiskName(t claim.Token) string { return SandboxName(t) + "-" + issueVolume }

// liveDisk is one issue pod's disk as the operator reads it. Nothing is selected by a label here:
// the disk is a generic ephemeral volume declared in the pod's own template (issuePod.diskSource),
// and the pinned Sandbox CRD declares that template's metadata a bare object, so the API server
// prunes any label set there — an issue's disk carries none, and is found by its name alone. owner
// is the claim's controlling owner as `<kind>/<name>`: a Pod, never the Sandbox that owned the
// issue's volume before INFRA-80.
type liveDisk struct {
	name, phase, uid, owner string
}

// issueDisk reads c's issue pod's disk; found is false when there is none, which is what a pod
// that has gone leaves behind, its disk deleted with it. The restricted runtime identity has no
// PVC verb, so this is an operator step.
func (r *liveRig) issueDisk(c *liveClaim) (disk liveDisk, found bool, err error) {
	out, err := r.kubectl("get", "pvc", issueDiskName(c.token), "--ignore-not-found",
		"-o", `jsonpath={.metadata.name} {.status.phase} {.metadata.uid} {.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}`)
	if err != nil {
		return liveDisk{}, false, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return liveDisk{}, false, nil
	}
	if len(fields) != 4 {
		return liveDisk{}, false, fmt.Errorf("the operator read %s as %q, want its name, phase, uid and owner", issueDiskName(c.token), strings.TrimSpace(out))
	}
	return liveDisk{name: fields[0], phase: fields[1], uid: fields[2], owner: fields[3]}, true, nil
}

// assertPodOwnedDisk holds c's disk to what an issue pod's disk is now: Bound, and owned by that
// pod itself, so it is created with the pod and deleted with it. It returns the disk, whose uid a
// check comparing a pod replacement's disk with the replaced one's keeps.
func (r *liveRig) assertPodOwnedDisk(c *liveClaim) (liveDisk, error) {
	disk, found, err := r.issueDisk(c)
	if err != nil {
		return liveDisk{}, err
	}
	if !found {
		return liveDisk{}, fmt.Errorf("the issue pod %s has no disk %s", SandboxName(c.token), issueDiskName(c.token))
	}
	pod, err := r.getPod(SandboxName(c.token))
	if err != nil {
		return liveDisk{}, err
	}
	if disk.phase != "Bound" || disk.owner != "Pod/"+pod.Name {
		return liveDisk{}, fmt.Errorf("the disk %s is %s owned by %s, want Bound owned by its own pod %s", disk.name, disk.phase, disk.owner, "Pod/"+pod.Name)
	}
	return disk, nil
}

// awaitDiskGone waits until c's issue pod's disk is gone, which is what a pod's own ephemeral
// volume does when the pod goes: the kubelet's garbage collection follows the pod's ownership.
func (r *liveRig) awaitDiskGone(c *liveClaim) error {
	return r.poll(liveGoneLimit, "the disk "+issueDiskName(c.token)+" of issue "+c.issue+" to go with its pod", func() (bool, error) {
		_, found, err := r.issueDisk(c)
		return !found, err
	})
}

// markerLines reads the marker with `test ! -e || cat`, not a bare `cat`: a marker a poll reads
// before the stub agent has written to it does not exist yet, and a bare `cat`'s non-zero exit
// would reach the caller as an error — a poll returns on the first error, never retrying — rather
// than "not yet" (awaitRunning's own poll, the first launch of an issue: root at root-ready, root2
// and child2 at independent-provision, an orphan's first launch).
func (r *liveRig) markerLines(c *liveClaim) ([]string, error) {
	out, err := r.exec(c, "sh", "-c", `test ! -e "$1" || cat -- "$1"`, "sh", c.marker)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// roleIncarnations is markerLines filtered to one role's own "role:incarnation" entries, with the
// role prefix stripped back to the bare incarnation: the marker is the issue pod's (SandboxName),
// shared by every resident role that has started in it, so its raw lines also hold every sibling
// role's own launches. They are that pod's alone — the disk the marker sits on died with any pod
// before it — so a check across a pod replacement reads the session table, never this.
func roleIncarnations(lines []string, role claim.Role) []string {
	prefix := string(role) + ":"
	var own []string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			own = append(own, rest)
		}
	}
	return own
}

// recordSession puts c's recorded session in the session table, as `legion sessions import` copies
// one (ompsessions.Import), the first time it is asked and nothing after: the stub agent is no Oh
// My Pi and writes no session of its own, so the harness records it where the daemon's would be, as
// the machine recording a claim's session file does. It is called once c's pod runs (awaitRunning),
// so the row is written after that pod provisioned its workspace and before any later pod's: a
// resume in the same pod is told its workspace was not recreated, and a resume in a replacement pod
// is (launcher.WorkspaceRecreatedVariable, from Import's own mtime).
func (r *liveRig) recordSession(c *liveClaim) error {
	if c.recorded {
		return nil
	}
	content := `{"type":"session","version":3,"id":"` + string(c.token) + `"}` + "\n"
	if err := r.writeSession(c.session, content, time.Now()); err != nil {
		return fmt.Errorf("record %s's session %s in the table: %w", c.name, c.session, err)
	}
	c.recorded = true
	return nil
}

// writeSession puts content in the scratch database's session table at path, last written at
// written, as `legion sessions import` copies a session file (ompsessions.Import), through the
// URL file the script wrote for the harness's own direct use (sessionDSNLocalFile, dialable from
// the devbox this process runs on — never sessionDSNFile, the pod-advertised address a bridge-mode
// devbox cannot resolve; liveEnv's doc has the reasoning).
func (r *liveRig) writeSession(path, content string, written time.Time) error {
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	conn, err := ompsessions.Connect(ctx, r.env.sessionDSNLocalFile)
	if err != nil {
		return fmt.Errorf("the scratch database (LEGION_E2E_SESSION_DSN_LOCAL_FILE=%s): %w", r.env.sessionDSNLocalFile, err)
	}
	defer conn.Close(context.Background())
	outcome, err := ompsessions.Import(ctx, conn, path, []byte(content), written)
	if err != nil {
		return err
	}
	if outcome != ompsessions.Copied {
		return fmt.Errorf("the session table already held %s (%s), want a fresh copy", path, outcome)
	}
	return nil
}

// ---- reads under the runtime identity ----------------------------------------------------------

func (r *liveRig) getSandbox(name string) (*sandbox, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	u, err := r.dyn.Resource(sandboxGVR).Namespace(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}

func (r *liveRig) getPod(name string) (*corev1.Pod, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	return r.kube.CoreV1().Pods(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *liveRig) initLog(name string) (string, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	raw, err := r.kube.CoreV1().Pods(r.env.namespace).GetLogs(name, &corev1.PodLogOptions{Container: initContainer}).DoRaw(ctx)
	return string(raw), err
}

// poll re-checks cond every two seconds until it holds, fails, or limit passes.
func (r *liveRig) poll(limit time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := cond()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", limit, what)
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ---- launching, as the machine does -----------------------------------------------------------

// spec is the claim's launch: on its repository (claimRepository) at that repository's default
// branch.
func (r *liveRig) spec(c *liveClaim, resume string) runtime.SpawnSpec {
	repo, branch := r.claimRepository(c), r.defaultBranch
	if !c.repo.IsZero() {
		branch = r.secondDefaultBranch
	}
	return runtime.SpawnSpec{
		Claim: c.token, Project: r.env.claimProject, Tree: c.tree, Issue: c.issue, Role: c.role,
		Generation: c.gen, BootToken: c.bootToken, Env: map[string]string{"LEGION_E2E_MARKER": c.marker},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{r.prompt}}, Repository: repo, DefaultBranch: branch,
		ResumeSessionFile: resume,
	}
}

// claimRepository is c's repository: LEGION_E2E_REPO, or, for a claim on another repository
// (liveClaim.repo), LEGION_E2E_REPO_SECOND_OWNER.
func (r *liveRig) claimRepository(c *liveClaim) ghrepo.Repository {
	if !c.repo.IsZero() {
		return c.repo
	}
	return r.env.repo
}

// spawn is a Spawn of the claim's next generation; armed false mints a token no hello registers.
func (r *liveRig) spawn(c *liveClaim, armed bool) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	if err := r.recordSandbox(SandboxName(c.token)); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, armed)
	loc, err := r.rt.Spawn(r.ctx, r.spec(c, ""))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// resume is a Resume of the claim's next generation from file, with its last locator as prev.
func (r *liveRig) resume(c *liveClaim, file string) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	loc, err := r.rt.Resume(r.ctx, c.last, r.spec(c, file))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// resumeWithPlugins is resume with spec.Plugins set, for a launch that installs a stale-plugin
// pair into a kept pod (stale-plugin-install) rather than loading the image's own copies.
func (r *liveRig) resumeWithPlugins(c *liveClaim, file string, plugins *runtime.PluginInstall) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	spec := r.spec(c, file)
	spec.Plugins = plugins
	loc, err := r.rt.Resume(r.ctx, c.last, spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// awaitRunning waits for the claim's current pod to run with its Sandbox Ready, then for its
// shim's hello at the current generation, then for the stub agent's own marker line and setup
// record, and records the claim's session in the table (recordSession) once all of it holds. The
// hello only proves the shim dialed in before spawning its child (shim.go:204/317);
// checkSetupRecord holds the setup line before the agent line and its cwd, uid and parent pid for
// every successful role launch. A pod that runs and never says hello within one boot interval is a
// network-path failure, named as one.
func (r *liveRig) awaitRunning(c *liveClaim, since time.Time) (registration, error) {
	name := SandboxName(c.token)
	var pod *corev1.Pod
	err := r.poll(liveRunningLimit, "pod "+name+" to run with its Sandbox Ready", func() (bool, error) {
		p, err := r.getPod(name)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if string(p.UID) != c.loc.Sandbox.PodUID {
			return false, fmt.Errorf("pod %s is uid %s, not the pod uid %s the launch returned in process %s", name, p.UID, c.loc.Sandbox.PodUID, c.loc.Incarnation)
		}
		if terminal(p) {
			log, _ := r.initLog(name)
			return false, fmt.Errorf("pod %s (uid %s) ended %s before it ran: %s; init log: %s", name, p.UID, p.Status.Phase, containerStates(p), strings.TrimSpace(log))
		}
		s, err := r.getSandbox(name)
		if err != nil {
			return false, err
		}
		ready := s.condition(conditionReady)
		pod = p
		return p.Status.Phase == corev1.PodRunning && ready != nil && ready.Status == metav1.ConditionTrue, nil
	})
	if err != nil {
		return registration{}, err
	}
	reg, ok := r.reg.await(c.token, c.gen, since, liveBootTimeout)
	if !ok {
		return registration{}, r.networkPathFailure(pod)
	}
	wrote := string(c.role) + ":" + c.loc.Incarnation
	if err := r.poll(liveBootTimeout, "marker "+c.marker+" to hold "+wrote, func() (bool, error) {
		lines, err := r.markerLines(c)
		if err != nil {
			return false, err
		}
		return slices.Contains(lines, wrote), nil
	}); err != nil {
		return registration{}, err
	}
	if err := r.checkSetupRecord(c); err != nil {
		return registration{}, err
	}
	if err := r.recordSession(c); err != nil {
		return registration{}, err
	}
	return reg, nil
}

// awaitHelloAgain is c's shim saying hello, since the runtime and listener were replaced at since,
// with its current generation's boot token: a live claim reconnecting to a new runtime.
func (r *liveRig) awaitHelloAgain(c *liveClaim, since time.Time) (registration, error) {
	reg, ok := r.reg.await(c.token, c.gen, since, 2*time.Minute)
	if !ok || reg.hash != tokenHash(c.bootToken) {
		return reg, fmt.Errorf("%s's shim did not say hello again with its generation-%d token", c.name, c.gen)
	}
	return reg, nil
}

// networkPathFailure names what a pod that runs but never says hello points at: the address and
// port it dials, and, under LEGION_E2E_POD_REACHABILITY=aws-private-ip, the security groups of the
// node it runs on (from the node's EC2NodeClass, read as the operator) — Karpenter's EC2NodeClass
// CRD is an AWS concept, meaningless off it. Under bridge, there is no node-level diagnostic to
// read (the pod reaches a Service, not the devbox's own address), so the message instead points at
// the reverse-tunnel bridge (scripts/e2e/lib/bridge-tunnel.sh), whose chisel client or kubectl
// port-forward dying is what a network-path failure there actually means. This keys off
// podReachability, not runtimeIdentityKind or platform: the diagnostic is about how the pod was
// told to reach the devbox, which is independent of how the runtime authenticates or which
// platform the cluster runs.
func (r *liveRig) networkPathFailure(pod *corev1.Pod) error {
	if r.env.podReachability != "aws-private-ip" {
		return fmt.Errorf("network path: pod %s (uid %s) runs on node %s but no hello reached tcp://%s:%s within %s; check that the reverse-tunnel bridge is still up (its chisel client and kubectl port-forward processes, and the bridge pod's log, scripts/e2e/lib/bridge-tunnel.sh)",
			pod.Name, pod.UID, pod.Spec.NodeName, r.env.streamAdvertiseHost, r.env.streamPort, liveBootTimeout)
	}
	nodeClass, err := r.kubectl("get", "node", pod.Spec.NodeName, "-o", `jsonpath={.metadata.labels.karpenter\.k8s\.aws/ec2nodeclass}`)
	groups := "(unknown: " + fmt.Sprint(err) + ")"
	if err == nil {
		out, err := r.kubectl("get", "ec2nodeclass", strings.TrimSpace(nodeClass), "-o", "jsonpath={.status.securityGroups[*].id}")
		groups = strings.TrimSpace(out)
		if err != nil {
			groups = "(unknown: " + err.Error() + ")"
		}
	}
	return fmt.Errorf("network path: pod %s (uid %s) runs on node %s but no hello reached tcp://%s:%s within %s; the node's security groups are [%s], and the devbox's security group must admit %s from them",
		pod.Name, pod.UID, pod.Spec.NodeName, r.env.streamAdvertiseHost, r.env.streamPort, liveBootTimeout, groups, r.env.streamPort)
}

// suspend is a role-process stop. It leaves the issue pod in place; its launcher reports the
// child exit, so Runtime.Suspend returns only after that role's process is gone.
func (r *liveRig) suspend(c *liveClaim) error {
	if err := r.rt.Suspend(r.ctx, *c.loc); err != nil {
		return err
	}
	c.loc, c.state = nil, stateSuspended
	return nil
}

// ensureRunning and ensureSuspended put a claim where a check starts from: nothing to do in a full
// run, where the earlier checks left it there, and the same runtime calls in a development run. A
// claim that has run before comes back from its recorded session, which the table holds whatever
// became of the pod that first ran it.
func (r *liveRig) ensureRunning(c *liveClaim) error {
	if c.state == stateRunning {
		return nil
	}
	since := time.Now()
	var err error
	if c.state == stateNone {
		_, err = r.spawn(c, true)
	} else {
		_, err = r.resume(c, c.session)
	}
	if err == nil {
		_, err = r.awaitRunning(c, since)
	}
	if err != nil {
		return fmt.Errorf("setup: %s running: %w", c.name, err)
	}
	note("runtime", "setup: %s running at generation %d", c.name, c.gen)
	return nil
}

func (r *liveRig) ensureSuspended(c *liveClaim) error {
	if c.state == stateSuspended {
		return nil
	}
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.suspend(c); err != nil {
		return fmt.Errorf("setup: suspend %s: %w", c.name, err)
	}
	note("runtime", "setup: %s suspended", c.name)
	return nil
}

func containerStates(p *corev1.Pod) string {
	var parts []string
	for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, s := range statuses {
			switch {
			case s.State.Terminated != nil:
				parts = append(parts, fmt.Sprintf("%s terminated %s exit %d", s.Name, s.State.Terminated.Reason, s.State.Terminated.ExitCode))
			case s.State.Waiting != nil:
				parts = append(parts, fmt.Sprintf("%s waiting %s", s.Name, s.State.Waiting.Reason))
			case s.State.Running != nil:
				parts = append(parts, s.Name+" running")
			}
		}
	}
	return strings.Join(parts, "; ")
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// live are the claims with a running process, in the roster's order.
func (r *liveRig) live() []*liveClaim {
	var out []*liveClaim
	for _, c := range r.claims {
		if c.state == stateRunning {
			out = append(out, c)
		}
	}
	return out
}

// known is every claim the daemon has not retired, but for skip.
func (r *liveRig) known(skip *liveClaim) []runtime.Known {
	var out []runtime.Known
	for _, c := range r.claims {
		if c == skip || c.state == stateNone || c.state == stateReleased {
			continue
		}
		out = append(out, runtime.Known{Claim: c.token, Locator: c.loc})
	}
	return out
}
