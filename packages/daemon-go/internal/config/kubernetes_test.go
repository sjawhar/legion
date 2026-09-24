package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

const workerImage = "ghcr.io/sjawhar/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// kubernetesFile is the smallest file `runtime: kubernetes` loads: every key a pod needs to reach
// the daemon, Envoy, NATS, Dispatch, and the model, and the required members of the block. The App
// keys are commands that fail, so a passing LoadForValidation also shows it never ran them. The
// block comes last, so a test appends a member of it by appending a four-space-indented line.
const kubernetesFile = `project: LEGSMOKE
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
nats_urls: [nats://nats.internal.example:4222]
envoy_token_file: /var/run/legion/ENVOY_TOKEN
operator_token_file: /var/run/legion/OPERATOR_TOKEN
dispatch_url: https://dispatch.internal.example
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  LEGSMOKE: { repo: sjawhar/legion-smoke }
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: ` + workerImage + `
    storage_class: gp2
    gateway:
      url: https://middleman.internal.example/
      audience: middleman-legion
      service_account: legion-worker
      token_expiry_seconds: 600
`

// kubernetesWith is kubernetesFile with one line replaced; it panics when the line is not there,
// so a table row cannot pass by editing nothing.
func kubernetesWith(line, replacement string) string {
	if !strings.Contains(kubernetesFile, line) {
		panic("kubernetesFile has no line " + line)
	}
	return strings.Replace(kubernetesFile, line, replacement, 1)
}

func TestLoadForValidationSettlesEveryMemberOfTheKubernetesBlock(t *testing.T) {
	path := writeConfigFile(t, kubernetesFile+`    tree_volume: 30Gi
    kubeconfig: kube/legion-daemon
    context: legion-daemon@production
    session_store: pvc
    scheduling:
      node_selector: {karpenter.k8s.aws/instance-family: m7i, zone: 4}
      tolerations:
        - {key: dedicated, operator: Equal, value: legion, effect: NoSchedule}
        - {key: spot, operator: Exists, effect: NoExecute}
      priority_class: legion-workers
    resources:
      implementer:
        requests: {cpu: 500m, memory: 2Gi}
        limits: {memory: 6Gi, ephemeral_storage: 30Gi}
      tester:
        limits: {cpu: 4}
`)

	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}

	want := Runtime{Name: "kubernetes", Kubernetes: &Kubernetes{
		Namespace:    "legion",
		Image:        workerImage,
		StorageClass: "gp2",
		TreeVolume:   "30Gi",
		Kubeconfig:   filepath.Join(filepath.Dir(path), "kube/legion-daemon"),
		Context:      "legion-daemon@production",
		Scheduling: Scheduling{
			NodeSelector: map[string]string{"karpenter.k8s.aws/instance-family": "m7i", "zone": "4"},
			Tolerations: []Toleration{
				{Key: "dedicated", Operator: "Equal", Value: "legion", Effect: "NoSchedule"},
				{Key: "spot", Operator: "Exists", Effect: "NoExecute"},
			},
			PriorityClass: "legion-workers",
		},
		Resources: map[claim.Role]RoleResources{
			claim.RoleImplementer: {
				Requests: Quantities{CPU: "500m", Memory: "2Gi"},
				Limits:   Quantities{Memory: "6Gi", EphemeralStorage: "30Gi"},
			},
			claim.RoleTester: {Limits: Quantities{CPU: "4"}},
		},
		Gateway: Gateway{
			URL:            "https://middleman.internal.example",
			Audience:       "middleman-legion",
			ServiceAccount: "legion-worker",
			TokenExpiry:    600 * time.Second,
		},
	}}
	if cfg.Runtime.Name != want.Name || cfg.Runtime.Kubernetes == nil {
		t.Fatalf("Runtime = %+v, want %q with its block", cfg.Runtime, want.Name)
	}
	if !reflect.DeepEqual(*cfg.Runtime.Kubernetes, *want.Kubernetes) {
		t.Errorf("kubernetes block =\n%+v\nwant\n%+v", *cfg.Runtime.Kubernetes, *want.Kubernetes)
	}
}

// What a block that sets only its required members settles to: the tree volume at 20Gi, no
// requests or limits on any role (one tree per node, the pool's floor sizing the node), no
// scheduling beyond the Legion pool the runtime selects, and in-cluster credentials.
func TestLoadForValidationDefaultsTheKubernetesBlock(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	block := cfg.Runtime.Kubernetes
	if block == nil {
		t.Fatalf("Runtime = %+v, want the kubernetes block", cfg.Runtime)
	}
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"tree_volume", block.TreeVolume, "20Gi"},
		{"resources", block.Resources, map[claim.Role]RoleResources(nil)},
		{"scheduling", block.Scheduling, Scheduling{}},
		{"kubeconfig", block.Kubeconfig, ""},
		{"context", block.Context, ""},
		{"gateway.url, trailing slash dropped", block.Gateway.URL, "https://middleman.internal.example"},
		{"gateway.token_expiry_seconds", block.Gateway.TokenExpiry, 600 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Errorf("%s = %#v, want %#v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestLoadForValidationRefusesUnderKubernetes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "the kubernetes scalar with no block",
			body: kubernetesWith(kubernetesFile[strings.Index(kubernetesFile, "runtime:"):], "runtime: kubernetes\n"),
			want: "runtime.kubernetes is required when runtime is kubernetes",
		},
		{
			name: "the block is not a mapping",
			body: kubernetesWith(kubernetesFile[strings.Index(kubernetesFile, "runtime:"):], "runtime: {kubernetes: legion}\n"),
			want: "runtime.kubernetes must be a mapping",
		},
		{
			name: "an unknown member",
			body: kubernetesFile + "    frobnicate: 1\n",
			want: "unknown key runtime.kubernetes.frobnicate",
		},
		{
			name: "a member named twice",
			body: kubernetesFile + "    namespace: other\n",
			want: "runtime.kubernetes names namespace twice",
		},
		{
			name: "namespace absent",
			body: kubernetesWith("    namespace: legion\n", ""),
			want: "runtime.kubernetes.namespace is required",
		},
		{
			name: "namespace blank",
			body: kubernetesWith("    namespace: legion\n", "    namespace: \"\"\n"),
			want: "runtime.kubernetes.namespace must not be empty",
		},
		{
			name: "image absent",
			body: kubernetesWith("    image: "+workerImage+"\n", ""),
			want: "runtime.kubernetes.image is required",
		},
		{
			name: "image by tag",
			body: kubernetesWith(workerImage, "ghcr.io/sjawhar/legion-worker:latest"),
			want: "runtime.kubernetes.image must be pinned by digest (@sha256:…)",
		},
		{
			name: "image with a short digest",
			body: kubernetesWith(workerImage, "ghcr.io/sjawhar/legion-worker@sha256:0"),
			want: "runtime.kubernetes.image must be pinned by digest (@sha256:…)",
		},
		{
			name: "storage_class absent",
			body: kubernetesWith("    storage_class: gp2\n", ""),
			want: "runtime.kubernetes.storage_class is required: the cluster has no default storage class for the tree volume",
		},
		{
			name: "tree_volume not a quantity",
			body: kubernetesFile + "    tree_volume: twenty gigs\n",
			want: "runtime.kubernetes.tree_volume must be a positive Kubernetes quantity (e.g. 20Gi or 500m)",
		},
		{
			// The runtime reads a zero volume as "use the default", so zero would be a silent 20Gi.
			name: "tree_volume zero",
			body: kubernetesFile + "    tree_volume: 0\n",
			want: "runtime.kubernetes.tree_volume must be a positive Kubernetes quantity (e.g. 20Gi or 500m)",
		},
		{
			name: "kubeconfig blank",
			body: kubernetesFile + "    kubeconfig: \"\"\n",
			want: "runtime.kubernetes.kubeconfig must not be empty",
		},
		{
			name: "context blank",
			body: kubernetesFile + "    kubeconfig: /home/legion/.kube/config\n    context: \" \"\n",
			want: "runtime.kubernetes.context must not be empty",
		},
		{
			name: "context without a kubeconfig",
			body: kubernetesFile + "    context: legion-daemon@production\n",
			want: "runtime.kubernetes.context names a kubeconfig context, so it requires runtime.kubernetes.kubeconfig",
		},
		{
			name: "scheduling not a mapping",
			body: kubernetesFile + "    scheduling: [legion]\n",
			want: "runtime.kubernetes.scheduling must be a mapping",
		},
		{
			name: "an unknown scheduling member",
			body: kubernetesFile + "    scheduling: {affinity: {}}\n",
			want: "unknown key runtime.kubernetes.scheduling.affinity",
		},
		{
			name: "node_selector not a mapping",
			body: kubernetesFile + "    scheduling: {node_selector: [legion]}\n",
			want: "runtime.kubernetes.scheduling.node_selector must be a mapping of strings",
		},
		{
			name: "node_selector setting the Legion pool",
			body: kubernetesFile + "    scheduling: {node_selector: {legion.dev/pool: other}}\n",
			want: "runtime.kubernetes.scheduling.node_selector must not set legion.dev/pool: the runtime selects the Legion pool itself, and legion-sandbox-pods requires its value",
		},
		{
			name: "tolerations not an array",
			body: kubernetesFile + "    scheduling: {tolerations: {key: spot}}\n",
			want: "runtime.kubernetes.scheduling.tolerations must be an array",
		},
		{
			name: "a toleration that is not a mapping",
			body: kubernetesFile + "    scheduling: {tolerations: [spot]}\n",
			want: "runtime.kubernetes.scheduling.tolerations[0] must be a mapping",
		},
		{
			name: "a toleration with an unknown member",
			body: kubernetesFile + "    scheduling: {tolerations: [{key: spot, operator: Exists, effect: NoSchedule, tolerationSeconds: 5}]}\n",
			want: "unknown key runtime.kubernetes.scheduling.tolerations[0].tolerationSeconds",
		},
		{
			name: "a toleration with no key",
			body: kubernetesFile + "    scheduling: {tolerations: [{operator: Exists, effect: NoSchedule}]}\n",
			want: "runtime.kubernetes.scheduling.tolerations[0].key must not be empty",
		},
		{
			name: "a toleration with an unknown operator",
			body: kubernetesFile + "    scheduling: {tolerations: [{key: spot, operator: In, effect: NoSchedule}]}\n",
			want: "runtime.kubernetes.scheduling.tolerations[0].operator must be Equal or Exists",
		},
		{
			name: "a toleration with an unknown effect",
			body: kubernetesFile + "    scheduling: {tolerations: [{key: spot, operator: Exists, effect: Evict}]}\n",
			want: "runtime.kubernetes.scheduling.tolerations[0].effect must be NoSchedule, NoExecute, or PreferNoSchedule",
		},
		{
			name: "a toleration with a value under Exists",
			body: kubernetesFile + "    scheduling: {tolerations: [{key: spot, operator: Exists, value: yes, effect: NoSchedule}]}\n",
			want: "runtime.kubernetes.scheduling.tolerations[0].value must be omitted with operator Exists",
		},
		{
			name: "priority_class blank",
			body: kubernetesFile + "    scheduling: {priority_class: \"\"}\n",
			want: "runtime.kubernetes.scheduling.priority_class must not be empty",
		},
		{
			name: "resources not a mapping",
			body: kubernetesFile + "    resources: [planner]\n",
			want: "runtime.kubernetes.resources must be a mapping of role to requests and limits",
		},
		{
			// The TypeScript daemon's resource profiles are not roles.
			name: "resources keyed by a profile",
			body: kubernetesFile + "    resources: {small: {requests: {cpu: 500m}}}\n",
			want: `runtime.kubernetes.resources key "small" must be a role (architect, planner, implementer, tester, reviewer, merger)`,
		},
		{
			name: "a role named twice",
			body: kubernetesFile + "    resources: {planner: {}, planner: {}}\n",
			want: "runtime.kubernetes.resources names planner twice",
		},
		{
			name: "a role's resources not a mapping",
			body: kubernetesFile + "    resources: {planner: 500m}\n",
			want: "runtime.kubernetes.resources.planner must be a mapping",
		},
		{
			name: "a role's resources with an unknown member",
			body: kubernetesFile + "    resources: {planner: {claims: {}}}\n",
			want: "unknown key runtime.kubernetes.resources.planner.claims",
		},
		{
			name: "requests not a mapping",
			body: kubernetesFile + "    resources: {planner: {requests: 500m}}\n",
			want: "runtime.kubernetes.resources.planner.requests must be a mapping",
		},
		{
			name: "an unknown resource",
			body: kubernetesFile + "    resources: {planner: {limits: {nvidia.com/gpu: 1}}}\n",
			want: "unknown key runtime.kubernetes.resources.planner.limits.nvidia.com/gpu",
		},
		{
			name: "a quantity that is not one",
			body: kubernetesFile + "    resources: {planner: {requests: {memory: lots}}}\n",
			want: "runtime.kubernetes.resources.planner.requests.memory must be a positive Kubernetes quantity (e.g. 20Gi or 500m)",
		},
		{
			name: "a negative quantity",
			body: kubernetesFile + "    resources: {planner: {limits: {cpu: -1}}}\n",
			want: "runtime.kubernetes.resources.planner.limits.cpu must be a positive Kubernetes quantity (e.g. 20Gi or 500m)",
		},
		{
			name: "the TypeScript role_profiles",
			body: kubernetesFile + "    role_profiles: {tester: large}\n",
			want: "unknown key runtime.kubernetes.role_profiles: each role's requests and limits are set under runtime.kubernetes.resources, and a role absent there gets none",
		},
		{
			name: "session_store postgres",
			body: kubernetesFile + "    session_store: postgres\n",
			want: "runtime.kubernetes.session_store postgres is not supported until Stage 6: a pod's session lives on the tree volume (pvc)",
		},
		{
			name: "session_store neither pvc nor postgres",
			body: kubernetesFile + "    session_store: s3\n",
			want: "runtime.kubernetes.session_store must be 'pvc' or 'postgres'",
		},
		{
			name: "session_dsn_secret with the volume store",
			body: kubernetesFile + "    session_dsn_secret: SESSION_DSN\n",
			want: "runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is pvc; remove it",
		},
		{
			name: "gateway absent",
			body: kubernetesWith(kubernetesFile[strings.Index(kubernetesFile, "    gateway:"):], ""),
			want: "runtime.kubernetes.gateway is required: a pod reaches the model only through the gateway, with its projected service account token",
		},
		{
			name: "gateway not a mapping",
			body: kubernetesWith(kubernetesFile[strings.Index(kubernetesFile, "    gateway:"):], "    gateway: https://middleman.internal.example\n"),
			want: "runtime.kubernetes.gateway must be a mapping",
		},
		{
			name: "an unknown gateway member",
			body: kubernetesWith("      audience: middleman-legion\n", "      audience: middleman-legion\n      api_key: sk-nope\n"),
			want: "unknown key runtime.kubernetes.gateway.api_key",
		},
		{
			name: "gateway url absent",
			body: kubernetesWith("      url: https://middleman.internal.example/\n", ""),
			want: "runtime.kubernetes.gateway.url is required",
		},
		{
			name: "gateway url not a URL",
			body: kubernetesWith("https://middleman.internal.example/", "middleman"),
			want: "runtime.kubernetes.gateway.url must be a valid URL",
		},
		{
			name: "gateway url with a query string",
			body: kubernetesWith("https://middleman.internal.example/", "https://middleman.internal.example/?x=1"),
			want: "runtime.kubernetes.gateway.url must not include a query string or fragment",
		},
		{
			name: "gateway audience absent",
			body: kubernetesWith("      audience: middleman-legion\n", ""),
			want: "runtime.kubernetes.gateway.audience is required",
		},
		{
			name: "gateway service_account absent",
			body: kubernetesWith("      service_account: legion-worker\n", ""),
			want: "runtime.kubernetes.gateway.service_account is required",
		},
		{
			name: "gateway service_account blank",
			body: kubernetesWith("      service_account: legion-worker\n", "      service_account: \"\"\n"),
			want: "runtime.kubernetes.gateway.service_account must not be empty",
		},
		{
			name: "gateway token_expiry_seconds absent",
			body: kubernetesWith("      token_expiry_seconds: 600\n", ""),
			want: "runtime.kubernetes.gateway.token_expiry_seconds is required",
		},
		{
			name: "gateway token_expiry_seconds below the kubelet's minimum",
			body: kubernetesWith("token_expiry_seconds: 600", "token_expiry_seconds: 599"),
			want: "runtime.kubernetes.gateway.token_expiry_seconds must be at least 600 (the kubelet's minimum)",
		},
		{
			name: "gateway token_expiry_seconds past the timer bound",
			body: kubernetesWith("token_expiry_seconds: 600", "token_expiry_seconds: 2147484"),
			want: "runtime.kubernetes.gateway.token_expiry_seconds must be at most 2147483",
		},
		{
			name: "gateway token_expiry_seconds not an integer",
			body: kubernetesWith("token_expiry_seconds: 600", "token_expiry_seconds: 10m"),
			want: "runtime.kubernetes.gateway.token_expiry_seconds must be an integer",
		},
		{
			name: "daemon_url absent",
			body: kubernetesWith("daemon_url: http://10.0.0.5:13370\n", ""),
			want: "daemon_url is required when runtime is kubernetes: a pod cannot reach the daemon's loopback address",
		},
		{
			name: "envoy_url absent",
			body: kubernetesWith("envoy_url: http://envoy-listener.internal.example:9020\n", ""),
			want: "envoy_url is required when runtime is kubernetes: a pod cannot reach the loopback listener it defaults to",
		},
		{
			name: "nats_urls absent",
			body: kubernetesWith("nats_urls: [nats://nats.internal.example:4222]\n", ""),
			want: "nats_urls is required when runtime is kubernetes: every pod's Envoy client connects to NATS",
		},
		{
			name: "nats_urls empty",
			body: kubernetesWith("nats_urls: [nats://nats.internal.example:4222]\n", "nats_urls: []\n"),
			want: "nats_urls is required when runtime is kubernetes: every pod's Envoy client connects to NATS",
		},
		{
			name: "envoy_token_file absent",
			body: kubernetesWith("envoy_token_file: /var/run/legion/ENVOY_TOKEN\n", ""),
			want: "envoy_token_file is required when runtime is kubernetes: every pod receives the Envoy bearer",
		},
		{
			name: "operator_token_file absent",
			body: kubernetesWith("operator_token_file: /var/run/legion/OPERATOR_TOKEN\n", ""),
			want: "operator_token_file is required when runtime is kubernetes: the daemon cannot launch the controller there; legion controller start presents this token",
		},
		{
			name: "dispatch_url absent",
			body: kubernetesWith("dispatch_url: https://dispatch.internal.example\n", ""),
			want: "dispatch_url is required when runtime is kubernetes: the workflow is what launches every pod",
		},
		{
			name: "github_apps absent",
			body: kubernetesWith(`github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
`, ""),
			want: "github_apps is required when runtime is kubernetes: every pod's workspace is cloned with the implement App's token",
		},
		{
			name: "projects absent",
			body: kubernetesWith("projects:\n  LEGSMOKE: { repo: sjawhar/legion-smoke }\n", ""),
			want: "projects is required when runtime is kubernetes: every pod's workspace is its project's repository",
		},
		{
			name: "omp_invocation set",
			body: kubernetesFile + "omp_invocation: mise x github:acme/omp@1 -- omp\n",
			want: "omp_invocation is not used when runtime is kubernetes: every pod runs the worker image's Oh My Pi; remove omp_invocation",
		},
		{
			name: "omp_launch_prefix set",
			body: kubernetesFile + "omp_launch_prefix: [env, OMP_PROFILE=legion, --]\n",
			want: "omp_launch_prefix is not used when runtime is kubernetes: every pod runs the worker image's Oh My Pi; remove omp_launch_prefix",
		},
		{
			name: "provider_keys set",
			body: kubernetesFile + "provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY}\n",
			want: "provider_keys is not used when runtime is kubernetes: a pod reaches the model through runtime.kubernetes.gateway, and no provider key reaches a pod; remove provider_keys",
		},
		{
			name: "provider_keys set to an empty mapping",
			body: kubernetesFile + "provider_keys: {}\n",
			want: "provider_keys is not used when runtime is kubernetes: a pod reaches the model through runtime.kubernetes.gateway, and no provider key reaches a pod; remove provider_keys",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			if err == nil {
				t.Fatalf("LoadForValidation succeeded, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("LoadForValidation error =\n%q\nwant\n%q", err.Error(), tc.want)
			}
		})
	}
}

// The Stage 4b proof's configuration (the LEGION-208 Stage 4b plan, decisions 1, 2, 4, 5, 6, 7):
// the daemon on the devbox's private address driving production's namespace legion through the
// restricted role's kubeconfig and context, against production Dispatch, the Envoy listener, and
// NATS, with the gate off, a linger of 0.3 hours, and pods reaching the model through middleman
// with a 600-second token. No node selector and no requests: the pool's floor sizes the node.
func TestLoadForValidationAcceptsTheStage4bProofConfig(t *testing.T) {
	path := writeConfigFile(t, `project: LEGSMOKE
port: 13370
worker_stream_port: 13371
bind: 10.1.20.30
daemon_url: http://10.1.20.30:13370
postgres_dsn: postgres://legion:secret@127.0.0.1:5432/legion?sslmode=disable
state_dir: /tmp/stage4b/state
operator_token_file: /tmp/stage4b/operator-token
envoy_url: http://envoy-listener.internal.trajectorylabs.com:9020
envoy_token_file: /tmp/stage4b/envoy-token
nats_urls:
  - nats://nats.internal.trajectorylabs.com:4222
dispatch_url: https://dispatch.internal.trajectorylabs.com
dispatch_token_file: /tmp/stage4b/dispatch-token
projects:
  LEGSMOKE: { repo: sjawhar/legion-smoke }
gates:
  design: off
admission_cap: 2
review_round_cap: 3
linger_hours: 0.3
instructions: /tmp/stage4b/instructions.md
github_apps:
  implement:
    app_id: "3202636"
    private_key_command: "secrets LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -- sh -c 'printf %s \"$LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64\" | base64 -d'"
  review:
    app_id: "3202653"
    private_key_command: "secrets GH_REVIEW_APP_PRIVATE_KEY_B64 -- sh -c 'printf %s \"$GH_REVIEW_APP_PRIVATE_KEY_B64\" | base64 -d'"
runtime:
  kubernetes:
    namespace: legion
    image: `+workerImage+`
    storage_class: gp2
    kubeconfig: /home/ubuntu/.kube/legion-daemon-production
    context: legion-daemon@production
    gateway:
      url: https://middleman.hawk.internal.trajectorylabs.com
      audience: middleman-legion
      service_account: legion-worker
      token_expiry_seconds: 600
`)

	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Runtime.Name != "kubernetes" || cfg.Runtime.Kubernetes == nil {
		t.Fatalf("Runtime = %+v, want the kubernetes block", cfg.Runtime)
	}
	if cfg.Linger != 18*time.Minute || cfg.Gates.Design != DesignGateOff || cfg.AdmissionCap != 2 {
		t.Errorf("Linger=%s Gates=%+v AdmissionCap=%d, want 18m, off, 2", cfg.Linger, cfg.Gates, cfg.AdmissionCap)
	}
	if block := cfg.Runtime.Kubernetes; block.Context != "legion-daemon@production" || block.Resources != nil || block.Scheduling.NodeSelector != nil {
		t.Errorf("kubernetes block = %+v, want the restricted context, no requests, and no node selector", *block)
	}
}
