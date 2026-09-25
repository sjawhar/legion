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
    pod:
      env: {PI_CONFIG_FILES: /etc/legion-operator/overlay.yml, CLAUDE_CODE_USE_FOUNDRY: 0, GEMINI_API_KEY_FILE: /var/run/operator/gemini}
      service_account: legion-worker
      volumes:
        - name: operator-config
          config_map: {name: legion-operator, items: [{key: models.yml, path: models.yml}, {key: overlay.yml, path: overlay.yml}]}
        - name: operator-token
          projected:
            sources:
              - service_account_token: {audience: middleman-legion, expiration_seconds: 3600, path: token}
              - secret: {name: legion-operator-ca, items: [{key: ca.crt, path: ca.crt}]}
              - config_map: {name: legion-operator-routes}
        - name: operator-creds
          secret: {name: legion-operator-creds}
      volume_mounts:
        - {volume: operator-config, mount_path: /home/legion/.omp/profiles/legion/agent/models.yml, sub_path: models.yml}
        - {volume: operator-config, mount_path: /etc/legion-operator/overlay.yml, sub_path: overlay.yml}
        - {volume: operator-token, mount_path: /var/run/operator}
        - {volume: operator-creds, mount_path: /etc/legion-operator/creds, read_only: false}
provider_keys: {ANTHROPIC_API_KEY: anthropic_api_key}
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
		Pod: PodConfig{
			Env: map[string]string{
				"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml", "CLAUDE_CODE_USE_FOUNDRY": "0",
				"GEMINI_API_KEY_FILE": "/var/run/operator/gemini",
			},
			ServiceAccount: "legion-worker",
			Volumes: []PodVolume{
				{Name: "operator-config", ConfigMap: &ObjectSource{Name: "legion-operator", Items: []KeyPath{
					{Key: "models.yml", Path: "models.yml"}, {Key: "overlay.yml", Path: "overlay.yml"},
				}}},
				{Name: "operator-token", Projected: &ProjectedSource{Sources: []Projection{
					{ServiceAccountToken: &TokenProjection{Audience: "middleman-legion", ExpirationSeconds: 3600, Path: "token"}},
					{Secret: &ObjectSource{Name: "legion-operator-ca", Items: []KeyPath{{Key: "ca.crt", Path: "ca.crt"}}}},
					{ConfigMap: &ObjectSource{Name: "legion-operator-routes"}},
				}}},
				{Name: "operator-creds", Secret: &ObjectSource{Name: "legion-operator-creds"}},
			},
			VolumeMounts: []PodMount{
				{Volume: "operator-config", MountPath: "/home/legion/.omp/profiles/legion/agent/models.yml", SubPath: "models.yml", ReadOnly: true},
				{Volume: "operator-config", MountPath: "/etc/legion-operator/overlay.yml", SubPath: "overlay.yml", ReadOnly: true},
				{Volume: "operator-token", MountPath: "/var/run/operator", ReadOnly: true},
				{Volume: "operator-creds", MountPath: "/etc/legion-operator/creds"},
			},
		},
	}}
	if cfg.Runtime.Name != want.Name || cfg.Runtime.Kubernetes == nil {
		t.Fatalf("Runtime = %+v, want %q with its block", cfg.Runtime, want.Name)
	}
	if !reflect.DeepEqual(*cfg.Runtime.Kubernetes, *want.Kubernetes) {
		t.Errorf("kubernetes block =\n%+v\nwant\n%+v", *cfg.Runtime.Kubernetes, *want.Kubernetes)
	}
	if want := []ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "anthropic_api_key"}}; !reflect.DeepEqual(cfg.ProviderKeys, want) {
		t.Errorf("ProviderKeys = %+v, want %+v: under runtime kubernetes each names a key of the providers Secret", cfg.ProviderKeys, want)
	}
}

// What a block that sets only its required members settles to: the tree volume at 20Gi, no
// requests or limits on any role (one tree per node, the pool's floor sizing the node), no
// scheduling beyond the Legion pool the runtime selects, in-cluster credentials, and nothing of the
// operator's in any pod.
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
		{"pod", block.Pod, PodConfig{}},
		{"gateway.url, trailing slash dropped", block.Gateway.URL, "https://middleman.internal.example"},
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
			name: "gateway token_expiry_seconds past the lifetime the cluster admits",
			body: kubernetesWith("token_expiry_seconds: 600", "token_expiry_seconds: 3601"),
			want: "runtime.kubernetes.gateway.token_expiry_seconds must be at most 3600 (the longest pod token the cluster's admission policy admits)",
		},
		{
			name: "gateway token_expiry_seconds not an integer",
			body: kubernetesWith("token_expiry_seconds: 600", "token_expiry_seconds: 10m"),
			want: "runtime.kubernetes.gateway.token_expiry_seconds must be an integer",
		},
		{
			name: "daemon_url empty",
			body: kubernetesWith("daemon_url: http://10.0.0.5:13370\n", "daemon_url: \"\"\n"),
			want: "daemon_url must be a valid URL",
		},
		{
			name: "daemon_url on loopback",
			body: kubernetesWith("daemon_url: http://10.0.0.5:13370", "daemon_url: http://127.0.0.1:13370"),
			want: "daemon_url http://127.0.0.1:13370 names a loopback host, which in a pod is the pod itself; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "daemon_url on localhost",
			body: kubernetesWith("daemon_url: http://10.0.0.5:13370", "daemon_url: http://localhost:13370"),
			want: "daemon_url http://localhost:13370 names a loopback host, which in a pod is the pod itself; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "envoy_url on IPv6 loopback",
			body: kubernetesWith("envoy_url: http://envoy-listener.internal.example:9020", "envoy_url: http://[::1]:9020"),
			want: "envoy_url http://[::1]:9020 names a loopback host, which in a pod is the pod itself; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "a nats_urls entry on loopback",
			body: kubernetesWith("nats_urls: [nats://nats.internal.example:4222]", "nats_urls: [nats://nats.internal.example:4222, nats://127.0.0.2:4222]"),
			want: "nats_urls nats://127.0.0.2:4222 names a loopback host, which in a pod is the pod itself; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "dispatch_url on loopback",
			body: kubernetesWith("dispatch_url: https://dispatch.internal.example", "dispatch_url: http://127.0.0.1:8080"),
			want: "dispatch_url http://127.0.0.1:8080 names a loopback host, which in a pod is the pod itself; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "daemon_url unspecified",
			body: kubernetesWith("daemon_url: http://10.0.0.5:13370", "daemon_url: http://0.0.0.0:13370"),
			want: "daemon_url http://0.0.0.0:13370 names the unspecified address, which is no host a pod can dial; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "a nats_urls entry IPv6 unspecified",
			body: kubernetesWith("nats_urls: [nats://nats.internal.example:4222]", "nats_urls: [\"nats://[::]:4222\"]"),
			want: "nats_urls nats://[::]:4222 names the unspecified address, which is no host a pod can dial; name the host pods reach it at when runtime is kubernetes",
		},
		{
			name: "bind on loopback",
			body: kubernetesWith("bind: 10.0.0.5", "bind: 127.0.0.1"),
			want: "bind 127.0.0.1 is not an address a pod can reach, and every pod's shim dials the worker stream at tcp://127.0.0.1:13371; bind the daemon host's own address when runtime is kubernetes",
		},
		{
			name: "bind unspecified",
			body: kubernetesWith("bind: 10.0.0.5", "bind: 0.0.0.0"),
			want: "bind 0.0.0.0 is not an address a pod can reach, and every pod's shim dials the worker stream at tcp://0.0.0.0:13371; bind the daemon host's own address when runtime is kubernetes",
		},
		{
			name: "bind IPv6 unspecified",
			body: kubernetesWith("bind: 10.0.0.5", "bind: \"::\""),
			want: "bind :: is not an address a pod can reach, and every pod's shim dials the worker stream at tcp://[::]:13371; bind the daemon host's own address when runtime is kubernetes",
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
			name: "the pod block is not a mapping",
			body: kubernetesFile + "    pod: [env]\n",
			want: "runtime.kubernetes.pod must be a mapping",
		},
		{
			name: "an unknown pod member",
			body: kubernetesFile + "    pod:\n      containers: []\n",
			want: "unknown key runtime.kubernetes.pod.containers",
		},
		{
			name: "pod env is not a mapping",
			body: kubernetesFile + "    pod:\n      env: [OPENAI_BASE_URL]\n",
			want: "runtime.kubernetes.pod.env must be a mapping of variable to value",
		},
		{
			name: "a pod variable that is no variable name",
			body: kubernetesFile + "    pod:\n      env: {1PASSWORD: vault}\n",
			want: `runtime.kubernetes.pod.env key "1PASSWORD" must be an environment variable name (letters, digits, and underscores, not starting with a digit)`,
		},
		{
			name: "a pod variable whose value is no string",
			body: kubernetesFile + "    pod:\n      env: {OPENAI_BASE_URL: {host: x}}\n",
			want: "runtime.kubernetes.pod.env value for OPENAI_BASE_URL must be a string",
		},
		{
			name: "a credential-shaped pod variable",
			body: kubernetesFile + "    pod:\n      env: {ANTHROPIC_API_KEY: sk-ant}\n",
			want: "runtime.kubernetes.pod.env sets ANTHROPIC_API_KEY, a credential-shaped name: a credential comes from a Secret, so mount one with runtime.kubernetes.pod.volumes or name its key in provider_keys",
		},
		{
			name: "pod volumes are not a sequence",
			body: kubernetesFile + "    pod:\n      volumes: {creds: {}}\n",
			want: "runtime.kubernetes.pod.volumes must be an array",
		},
		{
			name: "a pod volume with no source",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds}]\n",
			want: "runtime.kubernetes.pod.volumes[0] must set exactly one of secret, config_map, projected",
		},
		{
			name: "a pod volume with two sources",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}, config_map: {name: b}}]\n",
			want: "runtime.kubernetes.pod.volumes[0] must set exactly one of secret, config_map, projected",
		},
		{
			name: "a pod volume of another kind",
			body: kubernetesFile + "    pod:\n      volumes: [{name: scratch, empty_dir: {}}]\n",
			want: "unknown key runtime.kubernetes.pod.volumes[0].empty_dir",
		},
		{
			name: "a pod volume with no name",
			body: kubernetesFile + "    pod:\n      volumes: [{secret: {name: a}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].name is required",
		},
		{
			name: "a pod volume named twice",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}, {name: creds, config_map: {name: b}}]\n",
			want: "runtime.kubernetes.pod.volumes names creds twice",
		},
		{
			name: "a Secret volume with no Secret",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {items: [{key: a, path: a}]}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].secret.name is required",
		},
		{
			name: "an item with no path",
			body: kubernetesFile + "    pod:\n      volumes: [{name: operator-config, config_map: {name: legion-operator, items: [{key: models.yml}]}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].config_map.items[0].path is required",
		},
		{
			name: "a projected volume with no sources",
			body: kubernetesFile + "    pod:\n      volumes: [{name: token, projected: {sources: []}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].projected.sources must name at least one source",
		},
		{
			name: "a projected source of another kind",
			body: kubernetesFile + "    pod:\n      volumes: [{name: token, projected: {sources: [{downward_api: {}}]}}]\n",
			want: "unknown key runtime.kubernetes.pod.volumes[0].projected.sources[0].downward_api",
		},
		{
			name: "a projected source of two kinds",
			body: kubernetesFile + "    pod:\n      volumes: [{name: token, projected: {sources: [{secret: {name: a}, config_map: {name: b}}]}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].projected.sources[0] must set exactly one of service_account_token, secret, config_map",
		},
		{
			name: "a projected token with no path",
			body: kubernetesFile + "    pod:\n      volumes: [{name: token, projected: {sources: [{service_account_token: {audience: operator}}]}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].projected.sources[0].service_account_token.path is required",
		},
		{
			name: "a projected token that expires at once",
			body: kubernetesFile + "    pod:\n      volumes: [{name: token, projected: {sources: [{service_account_token: {path: token, expiration_seconds: 0}}]}}]\n",
			want: "runtime.kubernetes.pod.volumes[0].projected.sources[0].service_account_token.expiration_seconds must be a positive integer",
		},
		{
			name: "a mount naming no volume",
			body: kubernetesFile + "    pod:\n      volume_mounts: [{volume: creds, mount_path: /etc/legion-operator}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[0].volume creds names no volume of runtime.kubernetes.pod.volumes",
		},
		{
			name: "a mount with no path",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: creds}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path is required",
		},
		{
			name: "a relative mount path",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: creds, mount_path: etc/legion-operator}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path etc/legion-operator must be a clean absolute path",
		},
		{
			name: "a mount path that is not clean",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: creds, mount_path: /etc/x/../../var/run/legion/boot}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path /etc/x/../../var/run/legion/boot must be a clean absolute path",
		},
		{
			name: "two mounts at one path",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}, {name: operator-config, config_map: {name: b}}]\n" +
				"      volume_mounts: [{volume: creds, mount_path: /etc/legion-operator}, {volume: operator-config, mount_path: /etc/legion-operator}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[1].mount_path /etc/legion-operator is also runtime.kubernetes.pod.volume_mounts[0]'s",
		},
		{
			name: "a mount's read_only is no boolean",
			body: kubernetesFile + "    pod:\n      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: creds, mount_path: /etc/legion-operator, read_only: sometimes}]\n",
			want: "runtime.kubernetes.pod.volume_mounts[0].read_only must be true or false",
		},
		{
			name: "a pod account other than the gateway's",
			body: kubernetesFile + "    pod:\n      service_account: operator-worker\n",
			want: "runtime.kubernetes.pod.service_account operator-worker differs from runtime.kubernetes.gateway.service_account legion-worker: a pod runs as one account",
		},
		{
			name: "a provider key the pod's env also sets",
			body: kubernetesFile + "    pod:\n      env: {OPENAI_BASE_URL: https://gateway.internal.example}\nprovider_keys: {OPENAI_BASE_URL: openai_base_url}\n",
			want: "provider_keys names OPENAI_BASE_URL, which runtime.kubernetes.pod.env also sets: the shim refuses to export a key its own environment names",
		},
		{
			name: "a provider key whose pointer the pod's env sets",
			body: kubernetesFile + "    pod:\n      env: {GEMINI_API_KEY_FILE: /var/run/operator/gemini}\nprovider_keys: {GEMINI_API_KEY: gemini}\n",
			want: "provider_keys names GEMINI_API_KEY, whose pointer GEMINI_API_KEY_FILE runtime.kubernetes.pod.env sets: the shim would skip the key",
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
