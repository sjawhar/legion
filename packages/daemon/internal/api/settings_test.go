package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// settingsClassifiedTypes are config.Config and every type reachable from it: each exported field
// of each is in settingsEmitted or settingsExcluded, so a field added to any of them is classified
// before the build is green, and a type added here is read field by field.
var settingsClassifiedTypes = []reflect.Type{
	reflect.TypeFor[config.Config](),
	reflect.TypeFor[config.Runtime](),
	reflect.TypeFor[config.Kubernetes](),
	reflect.TypeFor[config.Scheduling](),
	reflect.TypeFor[config.Toleration](),
	reflect.TypeFor[config.RoleResources](),
	reflect.TypeFor[config.PodConfig](),
	reflect.TypeFor[config.AgentSecretsConfig](),
	reflect.TypeFor[config.PluginsConfig](),
	reflect.TypeFor[config.ProviderKey](),
	reflect.TypeFor[config.Project](),
	reflect.TypeFor[config.MergeRules](),
	reflect.TypeFor[config.PathPattern](),
	reflect.TypeFor[config.Gates](),
	reflect.TypeFor[config.GitHubApps](),
	reflect.TypeFor[config.GitHubApp](),
	reflect.TypeFor[config.Capabilities](),
	reflect.TypeFor[ghrepo.Repository](),
}

// settingsEmitted is every config field SettingsOf emits, by `<Type>.<Field>`, and the settings
// key that carries it.
var settingsEmitted = map[string]string{
	"Config.Project":                                 "project",
	"Config.Port":                                    "port",
	"Config.Bind":                                    "bind",
	"Config.AdvertiseHost":                           "advertise_host",
	"Config.StateDir":                                "state_dir",
	"Config.Runtime":                                 "runtime",
	"Config.AdmissionCap":                            "admission_cap",
	"Config.DaemonURL":                               "daemon_url",
	"Config.WorkerStreamPort":                        "worker_stream_port",
	"Config.OmpInvocation":                           "omp_invocation",
	"Config.OmpLaunchPrefix":                         "omp_launch_prefix",
	"Config.ProviderKeys":                            "provider_keys (the variable names alone)",
	"Config.InstructionsPath":                        "instructions",
	"Config.WorkerBootTimeout":                       "worker_boot_timeout_seconds",
	"Config.WorkerRPCTimeout":                        "worker_rpc_timeout_seconds",
	"Config.WorkerStopTimeout":                       "worker_stop_timeout_seconds",
	"Config.TreeStopTimeout":                         "tree_stop_timeout_seconds",
	"Config.SlowCommandTimeout":                      "slow_command_timeout_seconds",
	"Config.ProbeInterval":                           "probe_interval_seconds",
	"Config.ControllerWakeInterval":                  "controller_wake_interval_seconds",
	"Config.WorkerBootRegistrationDeadlineIntervals": "worker_boot_registration_deadline_intervals",
	"Config.LaunchFailureLimit":                      "launch_failure_limit",
	"Config.PromptFailureLimit":                      "prompt_failure_limit",
	"Config.PromptRetireLimit":                       "prompt_retire_limit",
	"Config.EnvoyURL":                                "envoy_url",
	"Config.NatsURLs":                                "nats_urls",
	"Config.DispatchURL":                             "dispatch_url",
	"Config.Projects":                                "projects",
	"Config.Gates":                                   "gates",
	"Config.GitHubApps":                              "github_apps",
	"Config.Linger":                                  "linger_hours",
	"Config.ReviewRoundCap":                          "review_round_cap",
	"Config.MaxFixAttempts":                          "max_fix_attempts",
	"Config.ControllerLaunch":                        "controller",
	"Config.Capabilities":                            "capabilities",
	"Runtime.Name":                                   "runtime.name",
	"Runtime.Kubernetes":                             "runtime.kubernetes",
	"Kubernetes.Namespace":                           "runtime.kubernetes.namespace",
	"Kubernetes.Image":                               "runtime.kubernetes.image",
	"Kubernetes.StorageClass":                        "runtime.kubernetes.storage_class",
	"Kubernetes.IssueVolume":                         "runtime.kubernetes.issue_volume",
	"Kubernetes.Scheduling":                          "runtime.kubernetes.scheduling",
	"Kubernetes.Resources":                           "runtime.kubernetes.resources",
	"Kubernetes.AgentSecrets":                        "runtime.kubernetes.agent_secrets",
	"Kubernetes.SessionDSNSecret":                    "runtime.kubernetes.session_store (\"postgres\" iff set, else \"pvc\"; the key itself is never emitted)",
	"Kubernetes.Plugins":                             "runtime.kubernetes.plugins",
	"Scheduling.NodeSelector":                        "runtime.kubernetes.scheduling.node_selector",
	"Scheduling.Tolerations":                         "runtime.kubernetes.scheduling.tolerations",
	"Scheduling.PriorityClass":                       "runtime.kubernetes.scheduling.priority_class",
	"Toleration.Key":                                 "runtime.kubernetes.scheduling.tolerations[].key",
	"Toleration.Operator":                            "runtime.kubernetes.scheduling.tolerations[].operator",
	"Toleration.Value":                               "runtime.kubernetes.scheduling.tolerations[].value",
	"Toleration.Effect":                              "runtime.kubernetes.scheduling.tolerations[].effect",
	"RoleResources.CPU":                              "runtime.kubernetes.resources.<role>.cpu",
	"RoleResources.CPURequest":                       "runtime.kubernetes.resources.<role>.cpu_request",
	"RoleResources.Memory":                           "runtime.kubernetes.resources.<role>.memory",
	"RoleResources.MemoryRequest":                    "runtime.kubernetes.resources.<role>.memory_request",
	"RoleResources.EphemeralStorage":                 "runtime.kubernetes.resources.<role>.ephemeral_storage",
	"RoleResources.EphemeralStorageRequest":          "runtime.kubernetes.resources.<role>.ephemeral_storage_request",
	"AgentSecretsConfig.URL":                         "runtime.kubernetes.agent_secrets.url",
	"AgentSecretsConfig.Audience":                    "runtime.kubernetes.agent_secrets.audience",
	"AgentSecretsConfig.TokenExpirySeconds":          "runtime.kubernetes.agent_secrets.token_expiry_seconds",
	"PluginsConfig.PiLegion":                         "runtime.kubernetes.plugins.pi_legion",
	"PluginsConfig.PiEnvoy":                          "runtime.kubernetes.plugins.pi_envoy",
	"ProviderKey.Env":                                "provider_keys[]",
	"Project.Repo":                                   "projects.<KEY>.repo",
	"Project.MergeQueueRole":                         "projects.<KEY>.merge_queue_role",
	"Project.Merge":                                  "projects.<KEY>.merge",
	"Project.SlackChannels":                          "projects.<KEY>.slack_channels",
	"MergeRules.ByHand":                              "projects.<KEY>.merge.by_hand",
	"PathPattern.Expression":                         "projects.<KEY>.merge.by_hand[]",
	"Gates.Design":                                   "gates.design",
	"GitHubApps.Implement":                           "github_apps.implement",
	"GitHubApps.Review":                              "github_apps.review",
	"GitHubApp.AppID":                                "github_apps.<app>.app_id",
	"GitHubApp.Installations":                        "github_apps.<app>.installations",
	"Capabilities.Decided":                           "capabilities.decided",
}

// settingsExcluded is every config field SettingsOf leaves out, by `<Type>.<Field>`, and why: a
// credential, the source of one, the path of a file holding one, the operator's pod pieces, or a
// derived value that is no setting.
var settingsExcluded = map[string]string{
	"Config.PostgresDSN":            "a credential: a DSN carries its password",
	"GitHubApp.PrivateKey":          "a credential: Config holds the PEM resolved whatever the file's source",
	"GitHubApp.PrivateKeyCommand":   "the source of a credential",
	"GitHubApp.PrivateKeySecret":    "the source of a credential",
	"ProviderKey.Secret":            "the source of a credential: a secretsd key or a providers Secret key",
	"Config.OperatorTokenFile":      "the path of a file holding a credential",
	"Config.EnvoyTokenFile":         "the path of a file holding a credential",
	"Config.NatsNkeySeedFile":       "the path of a file holding a credential",
	"Config.NatsDaemonNkeySeedFile": "the path of a file holding a credential",
	"Config.DispatchTokenFile":      "the path of a file holding a credential",
	"AgentSecretsConfig.TokenFile":  "the path of a file holding a credential: the daemon's own broker login token",
	"Kubernetes.Kubeconfig":         "the daemon's cluster credential file",
	"Kubernetes.Context":            "the context inside the daemon's cluster credential file",
	"Kubernetes.Pod":                "not a daemon setting: the operator's pod pieces, which every pod reads from its own spec and whose lists are Kubernetes API types with no legion.yaml-shaped rendering",
	"PodConfig.Env":                 "runtime.kubernetes.pod: the operator's free text, delivered to every pod's spec",
	"PodConfig.Volumes":             "runtime.kubernetes.pod: Kubernetes API types naming the operator's Secrets and ConfigMaps",
	"PodConfig.VolumeMounts":        "runtime.kubernetes.pod: Kubernetes API types",
	"PodConfig.ServiceAccount":      "runtime.kubernetes.pod: the operator's ServiceAccount, which every pod runs as and reads from its own spec",
	"Kubernetes.PluginsFromBinary":  "derived: where the settled plugins pair came from, not a setting",
	"PathPattern.Regexp":            "compiled from Expression, not a setting",
}

// Every exported field of config.Config and the types under it is named in exactly one of the two
// tables, and every table row names a field that exists: a new field fails here until someone
// decides whether an unauthenticated route may carry it.
func TestSettingsOfClassifiesEveryConfigField(t *testing.T) {
	seen := map[string]bool{}
	for _, typ := range settingsClassifiedTypes {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			path := typ.Name() + "." + field.Name
			seen[path] = true
			_, emitted := settingsEmitted[path]
			_, excluded := settingsExcluded[path]
			switch {
			case emitted && excluded:
				t.Errorf("%s is in both settingsEmitted and settingsExcluded", path)
			case !emitted && !excluded:
				t.Errorf("%s is in neither settingsEmitted nor settingsExcluded: decide whether every pod may read it, put it in one table, and have SettingsOf agree", path)
			}
		}
	}
	for name, table := range map[string]map[string]string{"settingsEmitted": settingsEmitted, "settingsExcluded": settingsExcluded} {
		for path := range table {
			if !seen[path] {
				t.Errorf("%s names %s, which is no exported field of a classified type", name, path)
			}
		}
	}
}

// settingsFixturePlanted is every value of settingsFixtureFile that is a credential, the source of
// one, the path of a file holding one, or an operator pod piece: each is in the file (so the list
// cannot go stale) and none is in the settings' bytes.
var settingsFixturePlanted = []string{
	"BEGIN", "PRIVATE KEY",
	"postgres://", "fixture-dsn-password",
	"/var/run/legion/OPERATOR_TOKEN", "/var/run/legion/ENVOY_TOKEN", "/var/run/legion/NATS_NKEY_SEED",
	"/var/run/legion/NATS_DAEMON_NKEY_SEED", "/var/run/legion/DISPATCH_TOKEN", "/var/run/secrets/agent-secrets/token",
	"/etc/legion/kubeconfig", "legion-daemon@fixture-cluster",
	"SESSION_DSN_FIXTURE",
	"ANTHROPIC_API_KEY_FIXTURE", "GEMINI_API_KEY_FIXTURE",
	"/etc/legion-operator/overlay.yml", "operator-fixture-value",
	"operator-creds", "fixture-operator-creds", "/etc/legion-operator/creds", "fixture-operator-account",
}

// settingsForbiddenKeys are the legion.yaml keys that never appear in `settings`, at any depth.
var settingsForbiddenKeys = []string{
	"postgres_dsn", "private_key", "private_key_command", "private_key_secret",
	"kubeconfig", "context", "session_dsn_secret", "pod", "env", "volumes", "volume_mounts", "service_account",
}

// The maximal kubernetes file resolves to a settings block whose top-level, runtime.kubernetes and
// github_apps.implement key sets are exactly the modelled ones, whose bytes carry none of the
// planted secrets, sources, paths or pod pieces, no credential-shaped key and no null.
func TestSettingsOfAKubernetesConfigurationNamesEveryKeyAndNoSecret(t *testing.T) {
	settings := SettingsOf(settingsFixtureConfig(t))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// (a) The top level: every key of the table, omp_invocation apart, which the loader refuses
	// under runtime kubernetes.
	wantTop := []string{
		"project", "port", "bind", "advertise_host", "state_dir", "runtime", "admission_cap", "daemon_url",
		"worker_stream_port", "omp_launch_prefix", "provider_keys", "instructions",
		"worker_boot_timeout_seconds", "worker_rpc_timeout_seconds", "worker_stop_timeout_seconds",
		"tree_stop_timeout_seconds", "slow_command_timeout_seconds", "probe_interval_seconds",
		"controller_wake_interval_seconds", "worker_boot_registration_deadline_intervals",
		"launch_failure_limit", "prompt_failure_limit", "prompt_retire_limit", "envoy_url", "nats_urls",
		"dispatch_url", "projects", "gates", "github_apps", "linger_hours", "review_round_cap",
		"max_fix_attempts", "controller", "capabilities",
	}
	top := keysOf(t, encoded)
	assertKeys(t, "settings", top, wantTop)

	// (b) runtime.kubernetes, and one App.
	runtimeKeys := keysOf(t, top["runtime"])
	assertKeys(t, "settings.runtime", runtimeKeys, []string{"name", "kubernetes"})
	assertKeys(t, "settings.runtime.kubernetes", keysOf(t, runtimeKeys["kubernetes"]), []string{
		"namespace", "image", "storage_class", "issue_volume", "scheduling", "resources", "session_store",
		"agent_secrets", "plugins",
	})
	apps := keysOf(t, top["github_apps"])
	assertKeys(t, "settings.github_apps", apps, []string{"implement", "review"})
	assertKeys(t, "settings.github_apps.implement", keysOf(t, apps["implement"]), []string{"app_id", "installations"})

	// (c) Nothing planted reaches the wire, and no key of a credential's shape does either.
	for _, planted := range settingsFixturePlanted {
		if !strings.Contains(settingsFixtureFile, planted) {
			t.Errorf("settingsFixturePlanted names %q, which settingsFixtureFile does not plant", planted)
		}
		if bytes.Contains(encoded, []byte(planted)) {
			t.Errorf("settings carry %q: %s", planted, encoded)
		}
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range everyKey(document, "settings") {
		name := key[strings.LastIndex(key, ".")+1:]
		if slices.Contains(settingsForbiddenKeys, name) || strings.HasSuffix(name, "_file") {
			t.Errorf("settings carry the key %s", key)
		}
	}

	// (d) No null anywhere: the plugin's strict reader refuses one.
	if bytes.Contains(encoded, []byte("null")) {
		t.Errorf("settings carry null: %s", encoded)
	}

	// The shapes the table changes from the file's: the variable names alone, the derived store,
	// the expressions as written, the hours as a number, the resolved reservations.
	if want := []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY"}; !slices.Equal(settings.ProviderKeys, want) {
		t.Errorf("provider_keys = %q, want %q", settings.ProviderKeys, want)
	}
	if settings.Runtime.Kubernetes.SessionStore != "postgres" {
		t.Errorf("session_store = %q, want postgres", settings.Runtime.Kubernetes.SessionStore)
	}
	if want := []string{"^deploy/", `\.sql$`}; !slices.Equal(settings.Projects["WIDGETS"].Merge.ByHand, want) {
		t.Errorf("projects.WIDGETS.merge.by_hand = %q, want %q", settings.Projects["WIDGETS"].Merge.ByHand, want)
	}
	if settings.Projects["LEGION"].Repo != "sjawhar/legion" || settings.Projects["WIDGETS"].MergeQueueRole != "merge-queue" {
		t.Errorf("projects = %+v, want LEGION's repo and WIDGETS's merge queue role", settings.Projects)
	}
	if settings.LingerHours != 1.5 || settings.WorkerStopTimeoutSeconds != 30 || settings.Controller != config.ControllerLaunchDaemon {
		t.Errorf("linger_hours %v, worker_stop_timeout_seconds %d, controller %q; want 1.5, 30, daemon", settings.LingerHours, settings.WorkerStopTimeoutSeconds, settings.Controller)
	}
	controller := settings.Runtime.Kubernetes.Resources["controller"]
	if controller.CPU != "2" || controller.CPURequest != "1" || controller.MemoryRequest != "4Gi" || controller.EphemeralStorage != "10Gi" {
		t.Errorf("resources.controller = %+v, want the file's cpu, cpu_request and memory_request with the default ephemeral storage", controller)
	}
	if len(settings.Runtime.Kubernetes.Resources) != 7 {
		t.Errorf("resources names %d roles, want the six workflow roles and the controller", len(settings.Runtime.Kubernetes.Resources))
	}
	if want := []string{"C0123ABCD", "C0123ABCE"}; !slices.Equal(settings.Projects["LEGION"].SlackChannels, want) {
		t.Errorf("projects.LEGION.slack_channels = %q, want %q", settings.Projects["LEGION"].SlackChannels, want)
	}
	if want := map[string]string{"pool-capacity": "the pool is sized by hand for the admission cap"}; !maps.Equal(settings.Capabilities.Decided, want) {
		t.Errorf("capabilities.decided = %q, want %q", settings.Capabilities.Decided, want)
	}
}

// A tmux file of the three required keys resolves to the defaults, with every optional block
// absent and every collection empty rather than null; and a key the file sets is the file's.
func TestSettingsOfAMinimalConfigurationResolvesTheDefaults(t *testing.T) {
	settings := SettingsOf(minimalSettingsConfig(t))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if settings.MaxFixAttempts != 3 || settings.WorkerBootTimeoutSeconds != 120 {
		t.Errorf("max_fix_attempts %d, worker_boot_timeout_seconds %d; want the defaults 3 and 120", settings.MaxFixAttempts, settings.WorkerBootTimeoutSeconds)
	}
	if settings.Runtime.Name != "tmux" || settings.Runtime.Kubernetes != nil {
		t.Errorf("runtime = %+v, want tmux with no kubernetes block", settings.Runtime)
	}
	if settings.GitHubApps != nil {
		t.Errorf("github_apps = %+v, want none", settings.GitHubApps)
	}
	top := keysOf(t, encoded)
	for key, want := range map[string]string{"provider_keys": "[]", "omp_launch_prefix": "[]", "nats_urls": "[]", "projects": "{}", "capabilities": `{"decided":{}}`} {
		if got := string(top[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, absent := range []string{"advertise_host", "omp_invocation", "instructions", "dispatch_url", "github_apps"} {
		if raw, present := top[absent]; present {
			t.Errorf("%s = %s, want absent", absent, raw)
		}
	}
	if bytes.Contains(encoded, []byte("null")) {
		t.Errorf("settings carry null: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("postgres://")) || bytes.Contains(encoded, []byte("fixture-dsn-password")) {
		t.Errorf("settings carry the DSN: %s", encoded)
	}

	set := SettingsOf(loadSettingsFile(t, minimalSettingsFile+"max_fix_attempts: 5\n"))
	if set.MaxFixAttempts != 5 {
		t.Errorf("max_fix_attempts = %d, want the file's 5", set.MaxFixAttempts)
	}
}

// keysOf is the members of one JSON object.
func keysOf(t *testing.T, encoded []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	return object
}

// assertKeys fails unless object's key set is exactly want.
func assertKeys(t *testing.T, name string, object map[string]json.RawMessage, want []string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(object))
	if expected := slices.Sorted(slices.Values(want)); !slices.Equal(got, expected) {
		t.Errorf("%s keys = %q, want %q", name, got, expected)
	}
}

// everyKey is every object key under document, as a dotted path from prefix.
func everyKey(document any, prefix string) []string {
	var keys []string
	switch value := document.(type) {
	case map[string]any:
		for key, member := range value {
			path := prefix + "." + key
			keys = append(keys, path)
			keys = append(keys, everyKey(member, path)...)
		}
	case []any:
		for _, member := range value {
			keys = append(keys, everyKey(member, prefix+"[]")...)
		}
	}
	return keys
}
