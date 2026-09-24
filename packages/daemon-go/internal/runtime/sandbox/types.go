package sandbox

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Scheduling is where Legion's pods may run, beyond the Legion pool every one of them selects.
type Scheduling struct {
	// NodeSelector is added to {legion.dev/pool: legion}; it may not set legion.dev/pool itself.
	NodeSelector map[string]string
	// Tolerations are appended to the Legion pool's own toleration.
	Tolerations   []corev1.Toleration
	PriorityClass string
}

// Tools are absolute paths inside the worker image. GH, Git, and JJ reach the agent as
// LEGION_GH_PATH, LEGION_GIT_PATH, and LEGION_JJ_PATH; Legion is the Go `legion` every container
// runs (/opt/legion/go/bin/legion), whose directory also leads the pod's PATH.
type Tools struct{ GH, Git, JJ, Legion string }

// Gateway is the model gateway every pod reaches the models through (LEGION-208 Stage 4b plan,
// decision 1: LEGION-199's design A′). A pod runs as ServiceAccount, and the kubelet projects it a
// token for Audience, rotated before TokenExpiry passes, which Oh My Pi presents to URL as its key:
// no provider key reaches a pod.
type Gateway struct {
	URL, Audience, ServiceAccount string
	// TokenExpiry is the projected token's lifetime, at least minTokenExpiry; the kubelet rotates
	// the token at 80% of it.
	TokenExpiry time.Duration
}

// ProvisionTokens mints the installation token workspace-init clones with, for a repository owner.
// The daemon's is appauth; a harness's may be a token file read on every call.
type ProvisionTokens interface {
	Token(ctx context.Context, owner string) (string, error)
}

// Options is what a Runtime is built from. Every field without a stated default is required.
type Options struct {
	// Namespace is where every Sandbox, pod, and Secret of the runtime lives; Project is the
	// value of the legion.dev/project label on every one of them, and the informers select on it.
	Namespace, Project string
	// Image is the worker image, pinned by digest: New refuses one without "@sha256:".
	Image string
	// StorageClass is the tree volume's class. Required: production has no default class.
	StorageClass string
	// TreeVolume is the tree volume's size, positive; the daemon's configuration supplies its
	// default (runtime.kubernetes.tree_volume, 20Gi).
	TreeVolume resource.Quantity
	Scheduling Scheduling
	// Resources are each role's container requests and limits; a role absent here gets none.
	Resources map[claim.Role]corev1.ResourceRequirements
	// StreamURL is the worker stream listener every pod's shim dials, tcp://host:port.
	StreamURL string
	// DaemonURL, EnvoyURL, and DispatchURL are the pane values LEGION_DAEMON_URL, ENVOY_URL, and
	// DISPATCH_URL; an empty one is left unset.
	DaemonURL, EnvoyURL, DispatchURL string
	// DispatchToken is the Dispatch bearer every claim's Secret carries as DISPATCH_TOKEN, and its
	// main container reads through DISPATCH_TOKEN_FILE. It is configured exactly when DispatchURL
	// is.
	DispatchToken string
	// NATSURLs are ENVOY_NATS_URL, comma-joined; none leaves it unset.
	NATSURLs []string
	Tools    Tools
	Gateway  Gateway
	// Agent is the command the shim wraps, before the Oh My Pi arguments the runtime appends
	// (`--resume`, `--mode rpc`, `--append-system-prompt`); Oh My Pi itself when nil.
	Agent []string
	// BootTimeout bounds each wait of a relaunch, and is how long a pod may stay unscheduled
	// before it counts as gone (worker_boot_timeout_seconds).
	BootTimeout time.Duration
	// BootIntervals is the registration deadline in boot intervals; the init container's lock
	// wait is sized from it (worker_boot_registration_deadline_intervals).
	BootIntervals int
	// TerminationGrace is the pods' terminationGracePeriodSeconds, and how long Suspend and Release
	// wait for a process to end itself after its shutdown frame (worker_stop_timeout_seconds).
	TerminationGrace time.Duration
	// ProbeInterval is how often every watched claim is evaluated again, beyond the evaluation
	// each change to its Sandbox or pod triggers (probe_interval_seconds).
	ProbeInterval time.Duration
	// AdoptTimeout bounds a working-copy adoption (slow_command_timeout_seconds).
	AdoptTimeout time.Duration
	Tokens       ProvisionTokens
	// Conns is the worker stream listener: Suspend's and Release's shutdown frames, and
	// AdoptWorkingCopy, go through it.
	Conns runtime.Conns
	// Now stamps observations and ages pods and orphans; time.Now when nil.
	Now func() time.Time
	// Log receives what the runtime decides without being asked; slog.Default() when nil.
	Log *slog.Logger
}

// InstallRef names the pieces of Agent Sandbox a cluster must have: the Sandbox CRD, and the
// controller's Deployment. Production's are sandboxes.agents.x-k8s.io and
// agent-sandbox-system/agent-sandbox-controller.
type InstallRef struct{ CRD, ControllerNamespace, ControllerName string }

// sandboxGVR is the Sandbox resource, agents.x-k8s.io/v1beta1 at agent-sandbox v1.0.3.
var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// The operating modes a Sandbox has (sandbox_types.go, SandboxOperatingMode). The CRD defaults an
// absent mode to Running.
const (
	modeRunning   = "Running"
	modeSuspended = "Suspended"
)

// The Sandbox conditions the mapping reads, and the Ready reasons that make it uncertain
// (sandbox_types.go:21-89).
const (
	conditionReady     = "Ready"
	conditionSuspended = "Suspended"
	conditionFinished  = "Finished"

	reasonMultiplePods    = "MultiplePods"
	reasonReconcilerError = "ReconcilerError"
)

// sandbox is the part of an agents.x-k8s.io/v1beta1 Sandbox that Legion writes or reads, with the
// upstream JSON names (api/v1beta1/sandbox_types.go at v1.0.3). Nothing else is declared: the
// schema test walks every emitted field against the vendored CRD, which is the only place a
// misspelled or unknown one would show — the API server prunes it without a word.
type sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              sandboxSpec   `json:"spec"`
	Status            sandboxStatus `json:"status,omitzero"`
}

type sandboxSpec struct {
	PodTemplate          podTemplate           `json:"podTemplate"`
	VolumeClaimTemplates []volumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	OperatingMode        string                `json:"operatingMode,omitempty"`
}

// podTemplate is upstream's PodTemplate: labels and annotations, and a core PodSpec.
type podTemplate struct {
	Metadata podMetadata    `json:"metadata"`
	Spec     corev1.PodSpec `json:"spec"`
}

type podMetadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type volumeClaimTemplate struct {
	Metadata volumeClaimMetadata              `json:"metadata"`
	Spec     corev1.PersistentVolumeClaimSpec `json:"spec"`
}

type volumeClaimMetadata struct {
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type sandboxStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// mode is the Sandbox's operating mode, with the CRD's default applied.
func (s *sandbox) mode() string {
	if s.Spec.OperatingMode == "" {
		return modeRunning
	}
	return s.Spec.OperatingMode
}

// condition is the Sandbox's condition of that type, when the controller wrote it for the
// Sandbox's current generation. Every operatingMode patch bumps the generation, so a condition
// left over from the previous pod is never read as this one's (B5).
func (s *sandbox) condition(kind string) *metav1.Condition {
	if c := meta.FindStatusCondition(s.Status.Conditions, kind); c != nil && c.ObservedGeneration == s.Generation {
		return c
	}
	return nil
}
