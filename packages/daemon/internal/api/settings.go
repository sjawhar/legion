package api

import (
	"maps"
	"net/url"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Settings is the daemon's resolved legion.yaml as `GET /legion/v1/state` carries it (contract
// 35): the values the daemon runs on, defaults included (a file that omits `max_fix_attempts`
// answers 3), keyed as the file is. The route is unauthenticated, so the audience is every pod of
// the deployment and anything else that reaches the daemon's API port; the block therefore leaves
// out every credential, every source of one (a secretsd or Secret key, a key command) and every
// path of a file holding one, and carries only what every pod may already know. Of the strings it
// carries, the four address keys (`daemon_url`, `envoy_url`, `nats_urls`, `dispatch_url`) ride
// endpointOf, which masks their userinfo as `xxxxx@`; a `runtime.kubernetes.plugins` source in
// URL form rides it too (pluginSourceOf), losing its userinfo, query and fragment; and
// `runtime.kubernetes.agent_secrets.url` rides as written, since the loader refuses userinfo, a
// path, a query and a fragment on it. Every other string is a name, a path, a quantity or a
// version, never a URL. SettingsOf is an allowlist: it names each field it emits, and a
// `config.Config` field added without a place in settings_test.go's emitted or excluded table
// fails TestSettingsOfClassifiesEveryConfigField.
type Settings struct {
	Project string `json:"project"`
	Port    int    `json:"port"`
	Bind    string `json:"bind"`
	// AdvertiseHost is absent while the file sets none.
	AdvertiseHost string          `json:"advertise_host,omitempty"`
	StateDir      string          `json:"state_dir"`
	Runtime       RuntimeSettings `json:"runtime"`
	// AdmissionCap is the cap; `admission.cap` carries the same value, read from the same configuration.
	AdmissionCap     int    `json:"admission_cap"`
	DaemonURL        string `json:"daemon_url"`
	WorkerStreamPort int    `json:"worker_stream_port"`
	// OmpInvocation is absent while the file sets none; OmpLaunchPrefix is `[]` then.
	OmpInvocation   string   `json:"omp_invocation,omitempty"`
	OmpLaunchPrefix []string `json:"omp_launch_prefix"`
	// ProviderKeys are the variable names of `provider_keys`, in file order, and never the store
	// key each names: the one key whose shape differs from the file's mapping.
	ProviderKeys []string `json:"provider_keys"`
	// Instructions is the deployment instructions' path, absent while the file sets none.
	Instructions string `json:"instructions,omitempty"`
	// The durations, in whole seconds as the file takes them.
	WorkerBootTimeoutSeconds      int `json:"worker_boot_timeout_seconds"`
	WorkerRPCTimeoutSeconds       int `json:"worker_rpc_timeout_seconds"`
	WorkerStopTimeoutSeconds      int `json:"worker_stop_timeout_seconds"`
	TreeStopTimeoutSeconds        int `json:"tree_stop_timeout_seconds"`
	SlowCommandTimeoutSeconds     int `json:"slow_command_timeout_seconds"`
	ProbeIntervalSeconds          int `json:"probe_interval_seconds"`
	ControllerWakeIntervalSeconds int `json:"controller_wake_interval_seconds"`
	// The counts.
	WorkerBootRegistrationDeadlineIntervals int      `json:"worker_boot_registration_deadline_intervals"`
	LaunchFailureLimit                      int      `json:"launch_failure_limit"`
	PromptFailureLimit                      int      `json:"prompt_failure_limit"`
	PromptRetireLimit                       int      `json:"prompt_retire_limit"`
	EnvoyURL                                string   `json:"envoy_url"`
	NatsURLs                                []string `json:"nats_urls"`
	// DispatchURL is absent while the file sets none.
	DispatchURL string `json:"dispatch_url,omitempty"`
	// Projects is `{}` while the file declares none.
	Projects map[string]ProjectSettings `json:"projects"`
	Gates    GatesSettings              `json:"gates"`
	// GitHubApps is absent while the file configures none.
	GitHubApps *GitHubAppsSettings `json:"github_apps,omitempty"`
	// LingerHours and ProductionCheckHours are `linger_hours` and `production_check_hours` as the
	// file takes them, decimal numbers of hours.
	LingerHours          float64                 `json:"linger_hours"`
	ProductionCheckHours float64                 `json:"production_check_hours"`
	ReviewRoundCap       int                     `json:"review_round_cap"`
	MaxFixAttempts       int                     `json:"max_fix_attempts"`
	Controller           config.ControllerLaunch `json:"controller"`
	Capabilities         CapabilitiesSettings    `json:"capabilities"`
	// WorkerBackends is `worker_backends`, the roles that run on a backend other than Oh My Pi by
	// role, `{}` while the file names none.
	WorkerBackends map[string]string `json:"worker_backends"`
}

// RuntimeSettings is `runtime`: its name, and the kubernetes block under that runtime alone.
type RuntimeSettings struct {
	Name       string              `json:"name"`
	Kubernetes *KubernetesSettings `json:"kubernetes,omitempty"`
}

// KubernetesSettings is the settled `runtime.kubernetes` block without its credential file
// (`kubeconfig`, `context`), the session store's own fields (`session_dsn_secret`, the credential
// source, and `session_database`, the instance and login behind it) and the operator's `pod`
// pieces, which every pod reads from its own spec.
type KubernetesSettings struct {
	Namespace    string                           `json:"namespace"`
	Image        string                           `json:"image"`
	StorageClass string                           `json:"storage_class"`
	IssueVolume  string                           `json:"issue_volume"`
	Scheduling   SchedulingSettings               `json:"scheduling"`
	Resources    map[string]RoleResourcesSettings `json:"resources"`
	// SessionStore is the settled store's kind — "pvc", "postgres" or "cloudsql-iam" — and nothing
	// else of it.
	SessionStore string                `json:"session_store"`
	AgentSecrets *AgentSecretsSettings `json:"agent_secrets,omitempty"`
	// Plugins is the settled pair, whatever its source, absent while the daemon knows none.
	Plugins *PluginsSettings `json:"plugins,omitempty"`
	// Setup is the argv every role container runs before each start of its Oh My Pi, `[]` while the
	// file sets none: every pod runs it, so every pod knows it, as with `omp_launch_prefix`.
	Setup           []string                `json:"setup"`
	RepositoryCache RepositoryCacheSettings `json:"repository_cache"`
}

// SchedulingSettings is `runtime.kubernetes.scheduling`; the selector and the tolerations are `{}`
// and `[]` when the file sets none.
type SchedulingSettings struct {
	NodeSelector  map[string]string    `json:"node_selector"`
	Tolerations   []TolerationSettings `json:"tolerations"`
	PriorityClass string               `json:"priority_class,omitempty"`
}

// TolerationSettings is one `scheduling.tolerations` entry; Value is absent under operator Exists.
type TolerationSettings struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect"`
}

// RoleResourcesSettings is one role's settled reservation, every field filled.
type RoleResourcesSettings struct {
	CPU                     string `json:"cpu"`
	CPURequest              string `json:"cpu_request"`
	Memory                  string `json:"memory"`
	MemoryRequest           string `json:"memory_request"`
	EphemeralStorage        string `json:"ephemeral_storage"`
	EphemeralStorageRequest string `json:"ephemeral_storage_request"`
}

// RepositoryCacheSettings is the settled `runtime.kubernetes.repository_cache`: each repository
// cache's volume, its fetch period, how long an unmapped repository's cache is kept, and its one
// container's reservation in a role's shape.
type RepositoryCacheSettings struct {
	Volume         string                `json:"volume"`
	RefreshMinutes int                   `json:"refresh_minutes"`
	RetainHours    int                   `json:"retain_hours"`
	Resources      RoleResourcesSettings `json:"resources"`
}

// AgentSecretsSettings is `runtime.kubernetes.agent_secrets` without `token_file`, the path of the
// daemon's own login token.
type AgentSecretsSettings struct {
	URL                string `json:"url"`
	Audience           string `json:"audience"`
	TokenExpirySeconds int    `json:"token_expiry_seconds"`
}

// PluginsSettings is `runtime.kubernetes.plugins`.
type PluginsSettings struct {
	PiLegion string `json:"pi_legion"`
	PiEnvoy  string `json:"pi_envoy"`
}

// ProjectSettings is one `projects.<KEY>` entry; its repositories are Dispatch's mapping, which
// the state's `repositories` shows, never this file's.
type ProjectSettings struct {
	MergeQueueRole string           `json:"merge_queue_role,omitempty"`
	Merge          MergeSettings    `json:"merge"`
	SlackChannels  []string         `json:"slack_channels"`
	Deploys        []DeploySettings `json:"deploys"`
}

// DeploySettings is one `projects.<KEY>.deploys` entry as the file writes it: the repository as
// `<owner>/<name>`, the workflow's path and the step whose success is a deploy.
type DeploySettings struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
}

// MergeSettings is a project's `merge` policy: the by-hand expressions as written, never compiled.
type MergeSettings struct {
	ByHand []string `json:"by_hand"`
}

// GatesSettings is `gates`.
type GatesSettings struct {
	Design config.DesignGate `json:"design"`
}

// GitHubAppsSettings is `github_apps`, each App its id and installations alone: never a private
// key, nor the command or secret key it came from.
type GitHubAppsSettings struct {
	Implement GitHubAppSettings `json:"implement"`
	Review    GitHubAppSettings `json:"review"`
}

// GitHubAppSettings is one App of `github_apps`.
type GitHubAppSettings struct {
	AppID         string            `json:"app_id"`
	Installations map[string]string `json:"installations"`
}

// CapabilitiesSettings is `capabilities`: the decided capabilities with their reasons, `{}` when
// none.
type CapabilitiesSettings struct {
	Decided map[string]string `json:"decided"`
}

// SettingsOf is cfg as the state shows it: every setting of the resolved file the audience of an
// unauthenticated route may read, and no credential, credential source or credential file path
// (Settings). Of the URLs it emits, the four address keys ride endpointOf and a plugin source in
// URL form rides pluginSourceOf, so no userinfo reaches the wire; `agent_secrets.url` is the one
// URL that rides as written, which the loader has made safe by refusing userinfo, a path, a query
// and a fragment on it (internal/config/kubernetes.go, readAgentSecrets). Every slice and map it
// emits is non-nil, so the state marshals no `null` inside it.
func SettingsOf(cfg config.Config) Settings {
	settings := Settings{
		Project:                                 cfg.Project,
		Port:                                    cfg.Port,
		Bind:                                    cfg.Bind,
		AdvertiseHost:                           cfg.AdvertiseHost,
		StateDir:                                cfg.StateDir,
		Runtime:                                 RuntimeSettings{Name: cfg.Runtime.Name, Kubernetes: kubernetesSettingsOf(cfg.Runtime.Kubernetes)},
		AdmissionCap:                            cfg.AdmissionCap,
		DaemonURL:                               endpointOf(cfg.DaemonURL),
		WorkerStreamPort:                        cfg.WorkerStreamPort,
		OmpInvocation:                           cfg.OmpInvocation,
		OmpLaunchPrefix:                         stringsOf(cfg.OmpLaunchPrefix),
		ProviderKeys:                            make([]string, len(cfg.ProviderKeys)),
		Instructions:                            cfg.InstructionsPath,
		WorkerBootTimeoutSeconds:                seconds(cfg.WorkerBootTimeout),
		WorkerRPCTimeoutSeconds:                 seconds(cfg.WorkerRPCTimeout),
		WorkerStopTimeoutSeconds:                seconds(cfg.WorkerStopTimeout),
		TreeStopTimeoutSeconds:                  seconds(cfg.TreeStopTimeout),
		SlowCommandTimeoutSeconds:               seconds(cfg.SlowCommandTimeout),
		ProbeIntervalSeconds:                    seconds(cfg.ProbeInterval),
		ControllerWakeIntervalSeconds:           seconds(cfg.ControllerWakeInterval),
		WorkerBootRegistrationDeadlineIntervals: cfg.WorkerBootRegistrationDeadlineIntervals,
		LaunchFailureLimit:                      cfg.LaunchFailureLimit,
		PromptFailureLimit:                      cfg.PromptFailureLimit,
		PromptRetireLimit:                       cfg.PromptRetireLimit,
		EnvoyURL:                                endpointOf(cfg.EnvoyURL),
		NatsURLs:                                make([]string, len(cfg.NatsURLs)),
		DispatchURL:                             endpointOf(cfg.DispatchURL),
		Projects:                                make(map[string]ProjectSettings, len(cfg.Projects)),
		Gates:                                   GatesSettings{Design: cfg.Gates.Design},
		LingerHours:                             cfg.Linger.Hours(),
		ProductionCheckHours:                    cfg.ProductionCheckBound.Hours(),
		ReviewRoundCap:                          cfg.ReviewRoundCap,
		MaxFixAttempts:                          cfg.MaxFixAttempts,
		Controller:                              cfg.ControllerLaunch,
		Capabilities:                            CapabilitiesSettings{Decided: make(map[string]string, len(cfg.Capabilities.Decided))},
		WorkerBackends:                          make(map[string]string, len(cfg.WorkerBackends)),
	}
	for i, key := range cfg.ProviderKeys {
		settings.ProviderKeys[i] = key.Env
	}
	for i, raw := range cfg.NatsURLs {
		settings.NatsURLs[i] = endpointOf(raw)
	}
	for key, project := range cfg.Projects {
		entry := ProjectSettings{
			MergeQueueRole: project.MergeQueueRole,
			Merge:          MergeSettings{ByHand: make([]string, len(project.Merge.ByHand))},
			SlackChannels:  stringsOf(project.SlackChannels),
			Deploys:        make([]DeploySettings, len(project.Deploys)),
		}
		for i, pattern := range project.Merge.ByHand {
			entry.Merge.ByHand[i] = pattern.Expression
		}
		for i, deploy := range project.Deploys {
			entry.Deploys[i] = DeploySettings{Repo: deploy.Repo.String(), Workflow: deploy.Workflow, Step: deploy.Step}
		}
		settings.Projects[key] = entry
	}
	if apps := cfg.GitHubApps; apps.Implement.AppID != "" || apps.Review.AppID != "" {
		settings.GitHubApps = &GitHubAppsSettings{Implement: gitHubAppSettingsOf(apps.Implement), Review: gitHubAppSettingsOf(apps.Review)}
	}
	for name, reason := range cfg.Capabilities.Decided {
		settings.Capabilities.Decided[string(name)] = reason
	}
	for role, backend := range cfg.WorkerBackends {
		settings.WorkerBackends[string(role)] = string(backend)
	}
	return settings
}

// kubernetesSettingsOf is block as the state shows it, nil under another runtime.
func kubernetesSettingsOf(block *config.Kubernetes) *KubernetesSettings {
	if block == nil {
		return nil
	}
	settings := &KubernetesSettings{
		Namespace:    block.Namespace,
		Image:        block.Image,
		StorageClass: block.StorageClass,
		IssueVolume:  block.IssueVolume,
		Scheduling: SchedulingSettings{
			NodeSelector:  make(map[string]string, len(block.Scheduling.NodeSelector)),
			Tolerations:   make([]TolerationSettings, len(block.Scheduling.Tolerations)),
			PriorityClass: block.Scheduling.PriorityClass,
		},
		Resources:    make(map[string]RoleResourcesSettings, len(block.Resources)),
		SessionStore: string(block.SessionStore.Kind),
		Setup:        stringsOf(block.Setup),
		RepositoryCache: RepositoryCacheSettings{
			Volume:         block.RepositoryCache.Volume,
			RefreshMinutes: block.RepositoryCache.RefreshMinutes,
			RetainHours:    block.RepositoryCache.RetainHours,
			Resources:      roleResourcesSettingsOf(block.RepositoryCache.Resources),
		},
	}
	maps.Copy(settings.Scheduling.NodeSelector, block.Scheduling.NodeSelector)
	for i, toleration := range block.Scheduling.Tolerations {
		settings.Scheduling.Tolerations[i] = TolerationSettings{Key: toleration.Key, Operator: toleration.Operator, Value: toleration.Value, Effect: toleration.Effect}
	}
	for role, reservation := range block.Resources {
		settings.Resources[string(role)] = roleResourcesSettingsOf(reservation)
	}
	if broker := block.AgentSecrets; broker != nil {
		settings.AgentSecrets = &AgentSecretsSettings{URL: broker.URL, Audience: broker.Audience, TokenExpirySeconds: broker.TokenExpirySeconds}
	}
	if plugins := block.Plugins; plugins != nil {
		settings.Plugins = &PluginsSettings{PiLegion: pluginSourceOf(plugins.PiLegion), PiEnvoy: pluginSourceOf(plugins.PiEnvoy)}
	}
	return settings
}

// roleResourcesSettingsOf is one settled reservation as the state shows it.
func roleResourcesSettingsOf(reservation config.RoleResources) RoleResourcesSettings {
	return RoleResourcesSettings{
		CPU: reservation.CPU, CPURequest: reservation.CPURequest,
		Memory: reservation.Memory, MemoryRequest: reservation.MemoryRequest,
		EphemeralStorage: reservation.EphemeralStorage, EphemeralStorageRequest: reservation.EphemeralStorageRequest,
	}
}

// gitHubAppSettingsOf is app's id and installations, and nothing of its key.
func gitHubAppSettingsOf(app config.GitHubApp) GitHubAppSettings {
	settings := GitHubAppSettings{AppID: app.AppID, Installations: make(map[string]string, len(app.Installations))}
	maps.Copy(settings.Installations, app.Installations)
	return settings
}

// seconds is d in whole seconds, as the file's `*_seconds` keys take it.
func seconds(d time.Duration) int {
	return int(d / time.Second)
}

// stringsOf is a copy of values that is `[]` on the wire when values is nil.
func stringsOf(values []string) []string {
	return append([]string{}, values...)
}

// endpointOf is one address key's value as the state shows it: the scheme, the host and the path,
// with the userinfo, where the loader has NATS and Envoy credentials live (`use URL userinfo or a
// secret for credentials`), masked as `xxxxx@`. "" stays "" (`dispatch_url` may be unset); an
// address that yields no scheme and host is `xxxxx` whole, never the raw string. The result is
// composed from the parsed scheme, the userinfo mark, the host and EscapedPath, so a query or a
// fragment never reaches the wire either: the loader refuses one on each of the four address keys,
// and a plugin URL's pre-signed query (pluginSourceOf) is dropped here. runtime/sandbox's namedURL
// is the same mask for a log line and keeps the host alone; this one keeps the path because a
// setting is a value its reader acts on, where namedURL's result is only a name.
func endpointOf(address string) string {
	if address == "" {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "xxxxx"
	}
	endpoint := u.Scheme + "://"
	if u.User != nil {
		endpoint += "xxxxx@"
	}
	return endpoint + u.Host + u.EscapedPath()
}

// pluginSourceOf is one `runtime.kubernetes.plugins` source as the state shows it. The loader
// (internal/config/kubernetes.go, readPluginSource) admits an exact version, an absolute tarball
// path, or an http(s) URL stored whole, so a private or pre-signed tarball URL carries its
// credential in userinfo or in a query (`X-Amz-Signature`): the URL form, told by the loader's own
// shape test, rides endpointOf, which masks the userinfo and drops the query and fragment; a
// version and a path carry no credential and pass through unchanged, never `xxxxx`.
func pluginSourceOf(source string) string {
	if u, err := url.Parse(source); err == nil && u.IsAbs() && (u.Scheme == "http" || u.Scheme == "https") {
		return endpointOf(source)
	}
	return source
}
