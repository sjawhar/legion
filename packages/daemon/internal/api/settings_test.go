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
)

// settingsConfigTypes is config.Config and every struct type of its package reachable from it
// through exported fields, pointers, slices, arrays and map keys and elements, each once: the
// types whose exported fields TestSettingsOfClassifiesEveryConfigField holds to settingsEmitted and
// settingsExcluded. Because the set is derived, a nested struct a new field introduces is read
// field by field before the build is green, with no list to extend. A type of another package (a
// Kubernetes API type, time.Duration, a role, a backend, a compiled regexp) is a leaf: its fields
// are no legion.yaml keys.
func settingsConfigTypes() []reflect.Type {
	root := reflect.TypeFor[config.Config]()
	var types []reflect.Type
	var walk func(typ reflect.Type)
	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Map:
			walk(typ.Key())
			walk(typ.Elem())
		case reflect.Struct:
			if typ.PkgPath() != root.PkgPath() || slices.Contains(types, typ) {
				return
			}
			types = append(types, typ)
			for i := range typ.NumField() {
				if field := typ.Field(i); field.IsExported() {
					walk(field.Type)
				}
			}
		}
	}
	walk(root)
	return types
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
	"Config.ProductionCheckBound":                    "production_check_hours",
	"Config.ReviewRoundCap":                          "review_round_cap",
	"Config.MaxFixAttempts":                          "max_fix_attempts",
	"Config.ControllerLaunch":                        "controller",
	"Config.Capabilities":                            "capabilities",
	"Config.WorkerBackends":                          "worker_backends",
	"Runtime.Name":                                   "runtime.name",
	"Runtime.Kubernetes":                             "runtime.kubernetes",
	"Runtime.Tmux":                                   "runtime.tmux (its session store's kind alone: session_store, files or postgres)",
	"Kubernetes.Namespace":                           "runtime.kubernetes.namespace",
	"Kubernetes.Image":                               "runtime.kubernetes.image",
	"Kubernetes.StorageClass":                        "runtime.kubernetes.storage_class",
	"Kubernetes.IssueVolume":                         "runtime.kubernetes.issue_volume",
	"Kubernetes.Scheduling":                          "runtime.kubernetes.scheduling",
	"Kubernetes.Resources":                           "runtime.kubernetes.resources",
	"Kubernetes.AgentSecrets":                        "runtime.kubernetes.agent_secrets",
	"Kubernetes.SessionStore":                        "runtime.kubernetes.session_store (the kind alone: neither the Secret key nor the instance and login behind it is emitted)",
	"Kubernetes.Plugins":                             "runtime.kubernetes.plugins",
	"Kubernetes.Setup":                               "runtime.kubernetes.setup",
	"Kubernetes.RepositoryCache":                     "runtime.kubernetes.repository_cache",
	"RepositoryCacheConfig.Volume":                   "runtime.kubernetes.repository_cache.volume",
	"RepositoryCacheConfig.RefreshMinutes":           "runtime.kubernetes.repository_cache.refresh_minutes",
	"RepositoryCacheConfig.RetainHours":              "runtime.kubernetes.repository_cache.retain_hours",
	"RepositoryCacheConfig.Resources":                "runtime.kubernetes.repository_cache.resources",
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
	"Project.MergeQueueRole":                         "projects.<KEY>.merge_queue_role",
	"Project.Merge":                                  "projects.<KEY>.merge",
	"Project.SlackChannels":                          "projects.<KEY>.slack_channels",
	"Project.Deploys":                                "projects.<KEY>.deploys",
	"Deploy.Repo":                                    "projects.<KEY>.deploys[].repo",
	"Deploy.Workflow":                                "projects.<KEY>.deploys[].workflow",
	"Deploy.Step":                                    "projects.<KEY>.deploys[].step",
	"MergeRules.ByHand":                              "projects.<KEY>.merge.by_hand",
	"PathPattern.Expression":                         "projects.<KEY>.merge.by_hand[]",
	"Gates.Design":                                   "gates.design",
	"GitHubApps.Implement":                           "github_apps.implement",
	"GitHubApps.Review":                              "github_apps.review",
	"GitHubApp.AppID":                                "github_apps.<app>.app_id",
	"GitHubApp.Installations":                        "github_apps.<app>.installations",
	"Capabilities.Decided":                           "capabilities.decided",
	"Config.Answers":                                 "answers",
	"Answers.Providers":                              "answers.providers",
	"Answers.Prices":                                 "answers.prices",
	"Answers.Bounds":                                 "answers.bounds",
	"AnswerEndpoint.BaseURL":                         "answers.providers.<provider>.base_url",
	"AnswerEndpoint.Credential":                      "answers.providers.<provider>.api_key_source (the key the file names, api_key_file or api_key_command, and nothing of the key, its command or its file)",
	"AnswerPrice.Input":                              "answers.prices.<model>.input",
	"AnswerPrice.Output":                             "answers.prices.<model>.output",
	"AnswerPrice.CacheWrite":                         "answers.prices.<model>.cache_write",
	"AnswerPrice.CacheRead":                          "answers.prices.<model>.cache_read",
	"AnswerBounds.Answer":                            "answers.bounds.answer_seconds",
	"AnswerBounds.ModelCalls":                        "answers.bounds.model_calls",
	"AnswerBounds.OutputTokens":                      "answers.bounds.output_tokens",
	"AnswerBounds.GHCall":                            "answers.bounds.gh_seconds",
	"AnswerBounds.GHOutputChars":                     "answers.bounds.gh_output_chars",
	"AnswerBounds.GHCalls":                           "answers.bounds.gh_calls",
	"AnswerBounds.Concurrent":                        "answers.bounds.concurrent",
	"AnswerBounds.DailyUSD":                          "answers.bounds.daily_usd",
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
	"PodConfig.Volumes":             "runtime.kubernetes.pod: Kubernetes API types naming the operator's Secrets, ConfigMaps, claims and NFS exports",
	"PodConfig.VolumeMounts":        "runtime.kubernetes.pod: Kubernetes API types",
	"PodConfig.ServiceAccount":      "runtime.kubernetes.pod: the operator's ServiceAccount, which every pod runs as and reads from its own spec",
	"Kubernetes.PluginsFromBinary":  "derived: where the settled plugins pair came from, not a setting",
	"PathPattern.Regexp":            "compiled from Expression, not a setting",
	"AnswerCredential.File":         "the path of a file holding a credential: a model provider's key",
	"AnswerCredential.Command":      "the source of a credential: the command that prints a model provider's key",
	"Answers.ConversationStore":     "the daemon's own connection to the conversation store: the path of a file holding its URL, or the login the daemon signs in as",
	"ConversationStore.DSNFile":     "the path of a file holding a credential: the conversation store's URL, password and all",
	"ConversationStore.Database":    "the instance and IAM login behind the daemon's conversation store, left out as runtime.kubernetes.session_database is",
	"Tmux.SessionDSNFile":           "the path of a file holding a credential: the tmux session database's URL, password and all",
}

// Every exported field of every struct the walk reaches (settingsConfigTypes) is named in exactly
// one of the two tables, and every table row names a field the walk reaches: a new field, on a
// type the walk already reads or on a nested type the field itself introduces, fails here until
// someone decides whether an unauthenticated route may carry it.
func TestSettingsOfClassifiesEveryConfigField(t *testing.T) {
	seen := map[string]bool{}
	for _, typ := range settingsConfigTypes() {
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
				t.Errorf("%s names %s, which is no exported field of a struct reachable from config.Config", name, path)
			}
		}
	}
}

// settingsFixturePlanted is every value of settingsFixtureFile that is a credential, the source of
// one, the path of a file holding one, the userinfo of an address, or an operator pod piece: each
// is in the file (so the list cannot go stale) and none is in the settings' bytes.
var settingsFixturePlanted = []string{
	"BEGIN", "PRIVATE KEY",
	"postgres://", "fixture-dsn-password",
	"legion:fixture-", "fixture-envoy-password", "fixture-nats-password",
	"legion:fixture-plugin-", "fixture-plugin-password", "fixture-plugin-token",
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
	"kubeconfig", "context", "session_dsn_secret", "session_database", "pod", "env", "volumes", "volume_mounts", "service_account",
	"api_key_file", "api_key_command", "conversation_store", "dsn_file", "user",
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
		"dispatch_url", "projects", "gates", "github_apps", "linger_hours", "production_check_hours", "review_round_cap",
		"max_fix_attempts", "controller", "capabilities", "worker_backends",
	}
	top := keysOf(t, encoded)
	assertKeys(t, "settings", top, wantTop)

	// (b) runtime.kubernetes, and one App.
	runtimeKeys := keysOf(t, top["runtime"])
	assertKeys(t, "settings.runtime", runtimeKeys, []string{"name", "kubernetes"})
	kubernetesKeys := keysOf(t, runtimeKeys["kubernetes"])
	assertKeys(t, "settings.runtime.kubernetes", kubernetesKeys, []string{
		"namespace", "image", "storage_class", "issue_volume", "scheduling", "resources", "session_store",
		"agent_secrets", "plugins", "setup", "repository_cache",
	})
	assertKeys(t, "settings.runtime.kubernetes.repository_cache", keysOf(t, kubernetesKeys["repository_cache"]), []string{
		"volume", "refresh_minutes", "retain_hours", "resources",
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
	if settings.Projects["WIDGETS"].MergeQueueRole != "merge-queue" {
		t.Errorf("projects = %+v, want WIDGETS's merge queue role", settings.Projects)
	}
	if settings.ProductionCheckHours != 240 {
		t.Errorf("production_check_hours = %v, want the file's 240", settings.ProductionCheckHours)
	}
	if want := map[string]string{"tester": "omp"}; !maps.Equal(settings.WorkerBackends, want) {
		t.Errorf("worker_backends = %q, want %q", settings.WorkerBackends, want)
	}
	if want := []string{"/bin/sh", "-c", "echo fixture-setup"}; !slices.Equal(settings.Runtime.Kubernetes.Setup, want) {
		t.Errorf("runtime.kubernetes.setup = %q, want %q", settings.Runtime.Kubernetes.Setup, want)
	}
	cache := settings.Runtime.Kubernetes.RepositoryCache
	if cache.Volume != "40Gi" || cache.RefreshMinutes != 15 || cache.RetainHours != 48 || cache.Resources.CPU != "500m" || cache.Resources.CPURequest != "500m" || cache.Resources.Memory != "1Gi" || cache.Resources.EphemeralStorage != "2Gi" {
		t.Errorf("runtime.kubernetes.repository_cache = %+v, want the file's volume, refresh, retention, cpu and memory with the default ephemeral storage", cache)
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
	if want := []DeploySettings{{Repo: "sjawhar/legion", Workflow: ".github/workflows/release.yaml", Step: "Apply"}}; !slices.Equal(settings.Projects["LEGION"].Deploys, want) {
		t.Errorf("projects.LEGION.deploys = %+v, want %+v", settings.Projects["LEGION"].Deploys, want)
	}
	if deploys := settings.Projects["WIDGETS"].Deploys; deploys == nil || len(deploys) != 0 {
		t.Errorf("projects.WIDGETS.deploys = %#v, want the empty list a project with no deploy shows", deploys)
	}
	if want := map[string]string{"pool-capacity": "the pool is sized by hand for the admission cap"}; !maps.Equal(settings.Capabilities.Decided, want) {
		t.Errorf("capabilities.decided = %q, want %q", settings.Capabilities.Decided, want)
	}
	// The two addresses the file gives userinfo ride masked; the one it gives none rides as written.
	if want := "http://xxxxx@envoy-listener.internal.example:9020"; settings.EnvoyURL != want {
		t.Errorf("envoy_url = %q, want %q", settings.EnvoyURL, want)
	}
	if want := []string{"nats://xxxxx@nats.internal.example:4222", "nats://nats-2.internal.example:4222"}; !slices.Equal(settings.NatsURLs, want) {
		t.Errorf("nats_urls = %q, want %q: the first entry's userinfo masked, the second unchanged", settings.NatsURLs, want)
	}
	// The plugin source in URL form rides with its userinfo masked and its query dropped; the one in
	// version form rides as written.
	want := PluginsSettings{PiLegion: "10.4.0", PiEnvoy: "https://xxxxx@plugins.internal.example/pi-envoy-10.4.0.tgz"}
	if plugins := settings.Runtime.Kubernetes.Plugins; plugins == nil || *plugins != want {
		t.Errorf("runtime.kubernetes.plugins = %+v, want %+v", plugins, want)
	}
}

// settingsAnswersBlock is an `answers` block for settingsFixtureFile's postgres session store,
// planting each credential source and file path the block can hold, and userinfo on one base URL.
// The state golden's fixture stays without it, so the golden pins the shape a deployment with the
// loop off serves.
const settingsAnswersBlock = `answers:
  providers:
    anthropic: {base_url: "https://legion:fixture-gateway-password@gateway.internal.example/anthropic/", api_key_file: /var/run/legion/ANTHROPIC_MODEL_KEY}
    openai: {base_url: https://gateway.internal.example/openai, api_key_command: cat /var/run/legion/fixture-openai-key-command}
  prices:
    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}
    openai/gpt-6.1: {input: 1.25, output: 10, cache_write: 0, cache_read: 0.125}
  conversation_store: {dsn_file: /var/run/legion/CONVERSATIONS_DSN}
  bounds: {model_calls: 3, daily_usd: 12.5}
`

// An `answers` block reaches the settings as each provider's base URL, its userinfo masked, and
// the name of its key's source; the prices; and every bound, the defaults filled in. No key, key
// command, key file, conversation store file or credential-shaped key reaches the wire.
func TestSettingsOfAnAnswersBlockCarriesNoCredential(t *testing.T) {
	settings := SettingsOf(loadSettingsFile(t, settingsFixtureFile+settingsAnswersBlock))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	answers := keysOf(t, keysOf(t, encoded)["answers"])
	assertKeys(t, "settings.answers", answers, []string{"providers", "prices", "bounds"})
	assertKeys(t, "settings.answers.providers.anthropic", keysOf(t, keysOf(t, answers["providers"])["anthropic"]), []string{"base_url", "api_key_source"})

	want := &AnswersSettings{
		Providers: map[string]AnswerProviderSettings{
			"anthropic": {BaseURL: "https://xxxxx@gateway.internal.example/anthropic", APIKeySource: "api_key_file"},
			"openai":    {BaseURL: "https://gateway.internal.example/openai", APIKeySource: "api_key_command"},
		},
		Prices: map[string]AnswerPriceSettings{
			"anthropic/claude-opus-5-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5},
			"openai/gpt-6.1":            {Input: 1.25, Output: 10, CacheWrite: 0, CacheRead: 0.125},
		},
		Bounds: AnswerBoundsSettings{
			AnswerSeconds: 180, ModelCalls: 3, OutputTokens: 4000, GHSeconds: 30, GHOutputChars: 60000, GHCalls: 20,
			Concurrent: 4, DailyUSD: 12.5,
		},
	}
	if !reflect.DeepEqual(settings.Answers, want) {
		t.Errorf("answers = %+v, want %+v", settings.Answers, want)
	}

	for _, planted := range []string{
		"fixture-gateway-password", "/var/run/legion/ANTHROPIC_MODEL_KEY", "fixture-openai-key-command",
		"/var/run/legion/CONVERSATIONS_DSN",
	} {
		if !strings.Contains(settingsAnswersBlock, planted) {
			t.Errorf("the test names %q, which settingsAnswersBlock does not plant", planted)
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
}

// endpointOf keeps an address's scheme, host and path and masks its userinfo, a user alone or a
// user and password, as xxxxx@; "" stays "", and an address that yields no scheme and host is xxxxx
// whole.
func TestEndpointOfMasksUserinfoAndKeepsThePath(t *testing.T) {
	for _, tc := range []struct{ address, want string }{
		{"http://envoy-listener.internal.example:9020", "http://envoy-listener.internal.example:9020"},
		{"http://legion@envoy-listener.internal.example:9020", "http://xxxxx@envoy-listener.internal.example:9020"},
		{"http://legion:fixture-envoy-password@envoy-listener.internal.example:9020", "http://xxxxx@envoy-listener.internal.example:9020"},
		{"https://dispatch.internal.example/legion", "https://dispatch.internal.example/legion"},
		{"https://legion:fixture-dispatch-password@dispatch.internal.example/legion", "https://xxxxx@dispatch.internal.example/legion"},
		{"nats://legion:fixture-nats-password@nats.internal.example:4222", "nats://xxxxx@nats.internal.example:4222"},
		{"", ""},
		{"not a url", "xxxxx"},
		{"//host-only", "xxxxx"},
	} {
		if got := endpointOf(tc.address); got != tc.want {
			t.Errorf("endpointOf(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
}

// pluginSourceOf leaves a version and an absolute tarball path as written and rides the URL form
// through endpointOf, masking its userinfo as xxxxx@ and dropping its query and fragment.
func TestPluginSourceOfMasksOnlyTheURLForm(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"10.4.0", "10.4.0"},
		{"/opt/plugins/pi-legion-10.4.0.tgz", "/opt/plugins/pi-legion-10.4.0.tgz"},
		{"https://plugins.internal.example/pi-legion-10.4.0.tgz", "https://plugins.internal.example/pi-legion-10.4.0.tgz"},
		{"https://legion:secret@plugins.internal.example/pi-legion-10.4.0.tgz?X-Amz-Signature=abc#frag", "https://xxxxx@plugins.internal.example/pi-legion-10.4.0.tgz"},
		{"http://plugins.internal.example/x.tgz?token=t", "http://plugins.internal.example/x.tgz"},
	} {
		if got := pluginSourceOf(tc.source); got != tc.want {
			t.Errorf("pluginSourceOf(%q) = %q, want %q", tc.source, got, tc.want)
		}
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
	if settings.Runtime.Name != "tmux" || settings.Runtime.Kubernetes != nil || settings.Runtime.Tmux != nil {
		t.Errorf("runtime = %+v, want tmux with no kubernetes block and no tmux block", settings.Runtime)
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
	for _, absent := range []string{"advertise_host", "omp_invocation", "instructions", "dispatch_url", "github_apps", "answers"} {
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

// A tmux file whose panes keep their sessions in Postgres shows runtime.tmux.session_store and
// nothing else of the block: never the path of the file holding the session database's URL.
func TestSettingsOfATmuxSessionDatabaseShowsTheStoreAndNotItsFile(t *testing.T) {
	const dsnFile = "/var/run/legion/TMUX_SESSION_DSN_FIXTURE"
	settings := SettingsOf(loadSettingsFile(t, minimalSettingsFile+"runtime: {tmux: {session_store: postgres, session_dsn_file: "+dsnFile+"}}\n"))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	runtimeKeys := keysOf(t, keysOf(t, encoded)["runtime"])
	assertKeys(t, "settings.runtime", runtimeKeys, []string{"name", "tmux"})
	assertKeys(t, "settings.runtime.tmux", keysOf(t, runtimeKeys["tmux"]), []string{"session_store"})
	if settings.Runtime.Tmux == nil || settings.Runtime.Tmux.SessionStore != "postgres" {
		t.Errorf("runtime.tmux = %+v, want session_store postgres", settings.Runtime.Tmux)
	}
	if bytes.Contains(encoded, []byte("TMUX_SESSION_DSN_FIXTURE")) {
		t.Errorf("settings carry the session database's URL file: %s", encoded)
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
