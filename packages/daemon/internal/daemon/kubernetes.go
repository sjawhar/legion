package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// agentSandbox is the Agent Sandbox a cluster must have installed for the runtime (LEGION-206
// Requirement 10): the Sandbox CRD and the controller's Deployment, as agent-sandbox v1.0.3 names
// them.
var agentSandbox = sandbox.InstallRef{
	CRD: "sandboxes.agents.x-k8s.io", ControllerNamespace: "agent-sandbox-system", ControllerName: "agent-sandbox-controller",
}

// workerImageTools are the worker image's own gh, git, jj, legion and agent-secrets
// (packages/daemon/docker/worker.Dockerfile: git from the distribution, gh and jj copied to
// /usr/local/bin, legion and agent-secrets under /opt/legion/bin).
var workerImageTools = sandbox.Tools{
	GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
	AgentSecrets: "/opt/legion/bin/agent-secrets",
}

// imageProbeRetry is how often the daemon tries its worker image again after an attempt that said
// nothing definitive: the daemon's backoff, bounded like `legion probe-image`'s at six attempts.
// Its transient outcomes (a pod that never finished, a kubelet failure) can repeat for a reason no
// wait changes, such as a memory limit too small for Oh My Pi, so an unbounded retry would hold a
// deterministic refusal as a boot that never ends.
var imageProbeRetry = bootprobe.Image

// sandboxReads is what Agent Sandbox needs that readBoot reads: the cluster's client and the
// runtime's Options.
type sandboxReads struct {
	client *rest.Config
	opts   sandbox.Options
}

// readSandbox is Agent Sandbox's share of readBoot (C1's translation, C3): the cluster's client from
// runtime.kubernetes' kubeconfig, and the Options every value of the configuration becomes, with
// the worker stream on tcp://<bind>:<worker_stream_port> (the address every pod's shim dials) and
// paneNatsUser, the public key of the pane NATS nkey seed's user ("" with none), which the image
// probe holds the providers Secret's seed to. None of the host's own agent machinery is read: no
// Oh My Pi invocation or plugin gate (the image probe proves the image's), no Dispatch token file (a
// pod reads its bearer from its claim's Secret), no secretsd provider keys (a pod mounts its keys
// from the providers Secret), and no host gh, git, or jj (a pod runs the image's).
func readSandbox(cfg config.Config, project, dispatchToken, paneNatsUser string, lookup func(string) (string, bool), log *slog.Logger) (sandboxReads, error) {
	k := *cfg.Runtime.Kubernetes
	rc, err := kubeClient(k)
	if err != nil {
		return sandboxReads{}, err
	}
	stream := "tcp://" + net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.WorkerStreamPort))
	opts, err := sandboxOptions(cfg, k, project, stream, dispatchToken, lookup, log)
	if err != nil {
		return sandboxReads{}, err
	}
	opts.NATSUser = paneNatsUser
	return sandboxReads{client: rc, opts: opts}, nil
}

// prepareSandbox is the runtime over readSandbox's client and Options, and the image probe.
func prepareSandbox(cfg config.Config, o overrides, reads sandboxReads, p *plan) error {
	p.stream = reads.opts.StreamURL
	if o.runtime != nil {
		p.newRuntime, p.probe = o.runtime, o.probe
		return nil
	}
	p.newRuntime = sandboxRuntime(reads.client, reads.opts, cfg.SlowCommandTimeout)
	p.probe = func(ctx context.Context, rt runtime.Runtime) error {
		sandboxed, ok := rt.(*sandbox.Runtime)
		if !ok {
			return fmt.Errorf("the image probe needs the Agent Sandbox runtime, not %T", rt)
		}
		return sandboxed.ProbeImage(ctx, sandbox.ImageProbe{
			Contract: api.DaemonAPIVersion, Budget: cfg.SlowCommandTimeout, Retry: imageProbeRetry,
			// The role prompts every pod is handed are this daemon's, inlined at each launch, so the
			// probe resolves what they name rather than the image's copy.
			RoleReferences: p.roleReferences,
		})
	}
	return nil
}

// kubeClient is the client of runtime.kubernetes: the kubeconfig's context it names, or the
// kubeconfig's current one, which must then exist (decision 4: the restricted role's kubeconfig
// sets none, so the configuration names it); with no kubeconfig, the pod's in-cluster credentials.
func kubeClient(k config.Kubernetes) (*rest.Config, error) {
	if k.Kubeconfig == "" {
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("runtime.kubernetes names no kubeconfig, so the daemon reads in-cluster credentials: %w", err)
		}
		return rc, nil
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: k.Kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: k.Context})
	if k.Context == "" {
		raw, err := loader.RawConfig()
		if err != nil {
			return nil, fmt.Errorf("read runtime.kubernetes.kubeconfig %s: %w", k.Kubeconfig, err)
		}
		if raw.CurrentContext == "" {
			return nil, fmt.Errorf("runtime.kubernetes.context is required: the kubeconfig %s sets no current context", k.Kubeconfig)
		}
	}
	rc, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("runtime.kubernetes.kubeconfig %s: %w", k.Kubeconfig, err)
	}
	return rc, nil
}

// sandboxOptions translates the configuration into the runtime's Options, all but the connection
// directory and the token source, which boot hands the factory. It refuses what the cluster would
// refuse only at the first pod: a role's request above its limit. lookup is the daemon's
// environment, which can name the NATS nkey seed (launchSecrets).
func sandboxOptions(cfg config.Config, k config.Kubernetes, project, stream, dispatchToken string, lookup func(string) (string, bool), log *slog.Logger) (sandbox.Options, error) {
	treeVolume, err := resource.ParseQuantity(k.TreeVolume)
	if err != nil {
		return sandbox.Options{}, fmt.Errorf("runtime.kubernetes.tree_volume: %w", err)
	}
	var tolerations []corev1.Toleration
	for _, t := range k.Scheduling.Tolerations {
		tolerations = append(tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	var resources map[claim.Role]corev1.ResourceRequirements
	for role, configured := range k.Resources {
		requirements, err := roleRequirements(role, configured)
		if err != nil {
			return sandbox.Options{}, err
		}
		if resources == nil {
			resources = map[claim.Role]corev1.ResourceRequirements{}
		}
		resources[role] = requirements
	}
	var agentSecrets *sandbox.AgentSecrets
	if a := k.AgentSecrets; a != nil {
		agentSecrets = &sandbox.AgentSecrets{URL: a.URL, Audience: a.Audience, TokenExpiry: time.Duration(a.TokenExpirySeconds) * time.Second}
	}
	return sandbox.Options{
		Namespace: k.Namespace, Project: project, Image: k.Image, StorageClass: k.StorageClass, TreeVolume: treeVolume,
		Scheduling: sandbox.Scheduling{NodeSelector: k.Scheduling.NodeSelector, Tolerations: tolerations, PriorityClass: k.Scheduling.PriorityClass},
		Resources:  resources,
		StreamURL:  stream,
		DaemonURL:  cfg.DaemonURL, EnvoyURL: cfg.EnvoyURL, DispatchURL: cfg.DispatchURL, DispatchToken: dispatchToken,
		NATSURLs:         cfg.NatsURLs,
		Tools:            workerImageTools,
		Pod:              sandbox.Pod(k.Pod),
		ProviderKeys:     providerSecretKeys(cfg.ProviderKeys),
		LaunchSecrets:    launchSecretNames(cfg, lookup),
		ProvidersSecrets: providersSecrets(cfg, lookup),
		BootTimeout:      cfg.WorkerBootTimeout,
		BootIntervals:    cfg.WorkerBootRegistrationDeadlineIntervals,
		TerminationGrace: cfg.WorkerStopTimeout,
		ProbeInterval:    cfg.ProbeInterval,
		AdoptTimeout:     cfg.SlowCommandTimeout,
		AgentSecrets:     agentSecrets,
		Log:              log,
	}, nil
}

// providersSecrets are the launch secrets a pod reads from the providers Secret's key of the same
// name, never from a copy in its own Secret: the NATS nkey seed, when the daemon has one. The one
// copy the cluster holds is the deployment's, in the Secret every pod already mounts; the daemon's
// own file (or variable) is where the daemon reads it, and the two must hold the same seed.
func providersSecrets(cfg config.Config, lookup func(string) (string, bool)) []string {
	if natsauth.Configured(cfg.NatsNkeySeedFile, lookup) {
		return []string{natsauth.SeedVariable}
	}
	return nil
}

// newSecretsLogin is the daemon's agent-secrets machine login as the machines' Enroller
// (AGENTC-393 Plan C): constructs the client from runtime.kubernetes.agent_secrets and starts its
// machine login on a background context at boot, logging the confirmation code exactly once —
// pod enrollment is held until a human approves it on the Dispatch credential page. The client
// itself is returned too, read-only, so the state route can show the login's current status
// (source.State, agentsecrets.Client.LoginStatus). Never part of launchSecrets, so no pod is ever
// handed the daemon's key or its won credential. Nil, nil without the block.
func newSecretsLogin(cfg config.Config, log *slog.Logger) (supervise.Enroller, *agentsecrets.Client) {
	k := cfg.Runtime.Kubernetes
	if k == nil || k.AgentSecrets == nil {
		return nil, nil
	}
	client := &agentsecrets.Client{URL: k.AgentSecrets.URL, Operator: k.AgentSecrets.Operator, HTTP: &http.Client{Timeout: 30 * time.Second}}
	operator := k.AgentSecrets.Operator
	go func() {
		code, err := client.Login(context.Background())
		if err != nil {
			log.Error("agent-secrets machine login failed", "error", err)
			return
		}
		log.Info(fmt.Sprintf(
			"agent-secrets machine login: enter code %s on the Dispatch credential page (approver: %s); pod enrollment is held until approved",
			code, operator,
		))
	}()
	return brokerEnroller{client: client}, client
}

// brokerEnroller is agentsecrets.Client as supervise.Enroller; an *agentsecrets.APIError is a
// PermanentError when its status says so, which is how the machine tells a refusal from an outage.
type brokerEnroller struct{ client *agentsecrets.Client }

func (b brokerEnroller) Enroll(ctx context.Context, e supervise.PodEnrollment) (string, error) {
	enrolled, err := b.client.Enroll(ctx, agentsecrets.PodEnrollment{
		PodUID: e.PodUID, Thumbprint: e.Thumbprint, PodToken: e.PodToken, Session: e.Session,
	})
	if err != nil {
		return "", err
	}
	return enrolled.ID, nil
}

func (b brokerEnroller) Revoke(ctx context.Context, id string) error { return b.client.Revoke(ctx, id) }

// providerSecretKeys are provider_keys as the runtime takes them: each variable Oh My Pi reads,
// to the key of the providers Secret that holds it; nil when the file names none.
func providerSecretKeys(keys []config.ProviderKey) map[string]string {
	if len(keys) == 0 {
		return nil
	}
	secretKeys := make(map[string]string, len(keys))
	for _, key := range keys {
		secretKeys[key.Env] = key.Secret
	}
	return secretKeys
}

// roleRequirements is one role's configured requests and limits as a container's, refusing a
// request above its limit: the API server refuses such a pod, but only when the role first runs.
func roleRequirements(role claim.Role, configured config.RoleResources) (corev1.ResourceRequirements, error) {
	key := "runtime.kubernetes.resources." + string(role)
	requests, err := quantities(configured.Requests, key+".requests")
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	limits, err := quantities(configured.Limits, key+".limits")
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	for name, request := range requests {
		if limit, ok := limits[name]; ok && request.Cmp(limit) > 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("%s: the %s request %s is above its limit %s, which the cluster refuses",
				key, name, request.String(), limit.String())
		}
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}, nil
}

// quantities are one requests or limits mapping, nil when it sets nothing.
func quantities(q config.Quantities, key string) (corev1.ResourceList, error) {
	var list corev1.ResourceList
	for _, part := range []struct {
		name  corev1.ResourceName
		value string
	}{
		{corev1.ResourceCPU, q.CPU}, {corev1.ResourceMemory, q.Memory}, {corev1.ResourceEphemeralStorage, q.EphemeralStorage},
	} {
		if part.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(part.value)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", key, part.name, err)
		}
		if list == nil {
			list = corev1.ResourceList{}
		}
		list[part.name] = quantity
	}
	return list, nil
}

// sandboxRuntime builds the Agent Sandbox runtime in the order its boot refusals need: Agent
// Sandbox's install check first, so a cluster without it is refused by name, then the runtime,
// whose informers run for ctx (supervision's lifetime), over the worker stream and with the
// workflow's implement App as every pod's provisioning token source.
func sandboxRuntime(rc *rest.Config, opts sandbox.Options, budget time.Duration) runtimeFactory {
	return func(ctx context.Context, conns runtime.Conns, stream string, apps appauth.Tokens) (runtime.Runtime, error) {
		checking, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if err := sandbox.CheckInstalled(checking, rc, agentSandbox); err != nil {
			return nil, err
		}
		opts.Conns, opts.StreamURL = conns, stream
		if apps != nil {
			opts.Tokens = implementTokens{apps}
		}
		return sandbox.New(ctx, rc, opts)
	}
}

// implementTokens is the workflow's implement App as a sandbox's provisioning token source: every
// pod's workspace-fetch clones with an installation token minted for the repository's owner.
type implementTokens struct{ apps appauth.Tokens }

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	if err != nil {
		return "", err
	}
	return lease.Token, nil
}
