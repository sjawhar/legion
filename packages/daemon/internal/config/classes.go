package config

import (
	"slices"
	"strings"
)

// Class is when a change of one setting takes effect, which is what the daemon can do with the new
// value while it runs.
type Class string

const (
	// ClassDecision is a value read per decision: the change takes effect at once, with nothing
	// rebuilt.
	ClassDecision Class = "decision"
	// ClassRebuild is a value a client, a listener or a connection holds: the change takes effect
	// once that holder is built again, which Row.Holder names.
	ClassRebuild Class = "rebuild"
	// ClassLaunch is a value a pod's or a pane's own launch reads: the change reaches a role at its
	// next launch. Row.Holder names the holder that must be built again first, because the holder
	// is what the next launch reads the value from.
	ClassLaunch Class = "launch"
	// ClassFixed is a value nothing can change under a running daemon: the route refuses the path
	// by name, with Row.Why.
	ClassFixed Class = "fixed"
)

// Outcome is what one applied change means for the caller that asked for it.
type Outcome string

const (
	OutcomeAtOnce     Outcome = "at-once"
	OutcomeRebuilt    Outcome = "rebuilt"
	OutcomeNextLaunch Outcome = "next-launch"
)

// Outcome is the class's outcome. A fixed path is refused rather than applied, so it has none.
func (c Class) Outcome() Outcome {
	switch c {
	case ClassDecision:
		return OutcomeAtOnce
	case ClassRebuild:
		return OutcomeRebuilt
	case ClassLaunch:
		return OutcomeNextLaunch
	}
	return ""
}

// The holders a rebuild names. Each is one thing the daemon builds from the configuration and can
// build again without stopping the work it is doing: a short stable string, because an operator
// reads it in a log line and the daemon's rebuild observer switches on it.
const (
	// HolderNATS is the NATS connection and its durable consumers.
	HolderNATS = "nats"
	// HolderDispatchClient is the Dispatch client every workflow call goes through.
	HolderDispatchClient = "dispatch-client"
	// HolderEnvoyPublisher is the Envoy notify publisher.
	HolderEnvoyPublisher = "envoy-publisher"
	// HolderGitHubApps is the GitHub App credential manager.
	HolderGitHubApps = "github-apps"
	// HolderAPIServer is the settings the API server holds; its socket is never rebound.
	HolderAPIServer = "api-server"
	// HolderWorkerStream is the worker stream listener's held settings; its socket is never
	// rebound and no worker connection drops.
	HolderWorkerStream = "worker-stream"
	// HolderControllerKeeper is the controller keeper and the controller tick.
	HolderControllerKeeper = "controller-keeper"
	// HolderRuntime is the process runtime, Sandbox or tmux, which holds everything a launch reads.
	HolderRuntime = "runtime"
	// HolderLaunchSpecs is the launch specs the supervisor builds each claim's process from.
	HolderLaunchSpecs = "launch-specs"
	// HolderRepositoryMapping is the project's repository mapping, which holds the deployment's
	// repository equivalence (`repository_renames`) it compares two names with.
	HolderRepositoryMapping = "repository-mapping"
)

// Row is one settable unit of `legion.yaml`, keyed by the Go field it is read into.
type Row struct {
	// Field is the field the value lands in, `<Type>.<Field>`, exactly as
	// api/settings_test.go's settingsEmitted writes it. It is the key the census test holds the
	// table to, so a field added to Config with no row fails that test.
	Field string
	// Path is the dotted path `legion.yaml` writes the unit as. A segment in angle brackets is a
	// name the file chooses (`projects.<KEY>.merge`), and a `[]` suffix one entry of a sequence
	// (`runtime.kubernetes.scheduling.tolerations[].key`); ClassOf matches a concrete path against
	// both. Two rows share a path where one key fills more than one field.
	Path string
	// Class is when a change of the unit takes effect.
	Class Class
	// Holder is what must be built again for the change to reach anything, "" when nothing holds
	// the value. Required on a rebuild row; a launch row names the holder the next launch reads
	// the value from. It is the one holder an operator is told was rebuilt (Change.Holder); a
	// value more than one thing holds names the rest in Also.
	Holder string
	// Also are the other holders a change of this row must build again, beside Holder: each is
	// built again in the daemon's registration order, and none is named in the answer the caller
	// reads, which says what the change did and not how many things the daemon holds it in.
	Also []string
	// Why is why a fixed path is refused, as the refusal says it; "" for every other class.
	Why string
	// Block is whether the field is a block the diff descends into rather than a value it
	// compares: the paths that change are the block's own members'. Path is still the block's own
	// key, which ClassOf answers for a caller that sets the whole block.
	Block bool
	// Secret is whether the value is a credential, the source of one, the path of a file holding
	// one, or an address whose userinfo or query may carry one. Such a value renders as
	// "(withheld)" in a Change, never as itself: the judgement is api.SettingsOf's, which leaves
	// each of them out of the unauthenticated state (api/settings.go, api/settings_test.go's
	// settingsExcluded).
	Secret bool
}

// The reasons the fixed paths are refused, named here because the loader's own refusals quote
// them and a test holds each to its row.
const (
	whyProject = "the project token names every claim, every record in the store and the daemon's own registry entry: a daemon that changed it would own none of the work it is running"
	whyAddress = "the API's listening socket is bound at boot, and LEGION_LISTEN_FD may hand the daemon one it never bound; there is nothing to rebuild and no way to rebind under the requests it is serving"
	whyStream  = "the worker stream's listening socket is bound at boot and every running shim holds a connection to it; rebinding it would cut every worker off"
	whyState   = "every running claim names files under it — its prompt, its secrets, its session and its workspace — and nothing moves them"
	whyStore   = "the store's pool is connected at boot and holds every record the daemon is working from"
	whyRuntime = "the processes the daemon supervises are that runtime's, panes of its own tmux server or pods of its Sandboxes, and no running process moves to the other"
	whyNamespc = "every Sandbox, pod, volume, Service and Secret the daemon works with lives in it: a daemon that changed it would see none of the pods it is supervising"
	whyTakeOve = "it is read once at boot, when the daemon adopts the claims of the tmux server it takes over; a later change has nothing left to adopt"
)

// Caller is which arm asked for a change through the settings route: what that caller may set.
//
// It is the rule of INFRA-1077's spec, "Which settings a projected token may change": the
// operator bearer and the daemon's own file may change every key except those fixed for the
// process, and a projected ServiceAccount token — Dispatch's path, whose caller is any
// deployment-scoped Dispatch user, a person or an agent — only a key whose value is plain data
// and is no `gates` key.
type Caller string

const (
	// CallerOperator is the operator bearer, and the boot's own restore of what the store kept:
	// every path the loader takes, which is every row but one fixed for the process.
	CallerOperator Caller = "operator"
	// CallerProjected is a caller holding a projected ServiceAccount token: only a row whose
	// value is plain data (Row.Plain) and that is no `gates` key, which is refused by name
	// whatever its row says (Caller.Admits). Every other path is refused by name too.
	CallerProjected Caller = "projected"
)

// The two reasons a projected ServiceAccount token is refused a path, by what the value is. Both
// are quoted in a Refusal, so a caller reads which of the two it met.
const (
	whyNotData = "a projected ServiceAccount token changes only the settings the daemon reads at each decision, and this one names a command to run, a credential, the file holding one, or an address the daemon connects to"
	whyNotRead = "a projected ServiceAccount token changes only the settings the daemon reads at each decision, and this one is held by a client, a listener, a launch, or the rule of who may call this route"
)

// gatesKey is `gates`, the one block a projected ServiceAccount token may not set, by name: the
// block itself and every key under it.
const gatesKey = "gates"

// Admits is whether this caller may set path, and why not when it may not. A path the class
// table does not hold is admitted here and refused by Apply as an unknown setting, so no caller
// learns from this answer alone which paths the daemon has.
//
// `gates` is named here rather than read off its row. Its rows are decision rows and stay so —
// the operator bearer and the daemon's own file change them as they change any other, and the
// API server and the launch specs hold their copies as before — but a gate is the rule that
// says a person decides, and a caller holding a projected token is not the party that may
// switch that rule off. It is refused with the reason the arm already gives for a key it may
// not set, since the design gate is held by the launch specs and the API server too.
func (c Caller) Admits(path string) (bool, string) {
	row, known := ClassOf(path)
	if !known || c != CallerProjected {
		return true, ""
	}
	if path == gatesKey || strings.HasPrefix(path, gatesKey+".") {
		return false, whyNotRead
	}
	if row.Plain() {
		return true, ""
	}
	if row.Secret {
		return false, whyNotData
	}
	return false, whyNotRead
}

// Plain is whether this row's value is plain data: a value the daemon reads at each decision and
// hands to nothing else — no command it runs, no credential, no file holding one, no address it
// connects to or listens on, and none of the rules of who may call the settings route.
//
// It is a property of the row and no second list: the class says the daemon reads the value as
// it decides rather than handing it to a client, a listener, a launch or a command, which leaves
// out every command, address, listener and `settings_route` key, each of them held; and Secret
// says the value is a credential, the source of one, the path of a file holding one, or an
// address that may carry one. Today the rows it admits are `admission_cap`, the supervision
// limits and timeouts (`launch_failure_limit`, `prompt_failure_limit`, `prompt_retire_limit`,
// `worker_boot_timeout_seconds`, `worker_boot_registration_deadline_intervals`,
// `tree_stop_timeout_seconds`), the workflow's own bounds (`linger_hours`,
// `production_check_hours`, `pod_update_retask_hours`, `pod_update_notice_hours`,
// `review_round_cap`, `max_fix_attempts`), `capabilities.decided` and `gates.design`.
//
// It is no longer the whole of what a projected token may set: `gates.design` is plain data and
// that caller is refused it by name (Caller.Admits).
func (r Row) Plain() bool { return r.Class == ClassDecision && !r.Secret }

// Classes is every settable unit of `legion.yaml`, one row per field the loader reads it into.
//
// The class is decided from what the daemon does with the value, read from its readers: where a
// field is read in more than one place the strictest class wins (fixed over rebuild over launch
// over decision) and the row says so. Holder is a second axis: it names what the daemon must build
// again for the new value to reach anything, which a launch row has too, since a launch reads the
// value from the runtime that launches it.
//
// A field of a nested block carries that block's whole path, so the table is flat: the diff walks
// Config and reads each field's path here, descending into a Block row rather than comparing it.
var Classes = []Row{
	// The identity and the sockets: boot bound them and the work in flight is tied to them.
	{Field: "Config.Project", Path: "project", Class: ClassFixed, Why: whyProject},
	{Field: "Config.Port", Path: "port", Class: ClassFixed, Why: whyAddress},
	{Field: "Config.Bind", Path: "bind", Class: ClassFixed, Why: whyAddress},
	{Field: "Config.WorkerStreamPort", Path: "worker_stream_port", Class: ClassFixed, Why: whyStream},
	{Field: "Config.StateDir", Path: "state_dir", Class: ClassFixed, Why: whyState},
	{Field: "Config.PostgresDSN", Path: "postgres_dsn", Class: ClassFixed, Why: whyStore, Secret: true},

	// The runtime selector and its two blocks. `runtime` is one key the loader splits into the
	// selector and the block it names, so Config.Runtime and Runtime.Name share its path: the
	// first is the block the diff descends into, the second the selector's own value.
	{Field: "Config.Runtime", Path: "runtime", Class: ClassFixed, Why: whyRuntime, Block: true},
	{Field: "Runtime.Name", Path: "runtime", Class: ClassFixed, Why: whyRuntime},
	{Field: "Runtime.Kubernetes", Path: "runtime.kubernetes", Class: ClassFixed, Why: whyNamespc, Block: true},
	{Field: "Runtime.Tmux", Path: "runtime.tmux", Class: ClassFixed, Why: whyTakeOve, Block: true},

	// Read per decision: admission reads the cap at every admission, and the supervisor its
	// budgets and its boot deadline at every launch it is deciding about.
	{Field: "Config.AdmissionCap", Path: "admission_cap", Class: ClassDecision},
	{Field: "Config.LaunchFailureLimit", Path: "launch_failure_limit", Class: ClassDecision},
	{Field: "Config.PromptFailureLimit", Path: "prompt_failure_limit", Class: ClassDecision},
	{Field: "Config.PromptRetireLimit", Path: "prompt_retire_limit", Class: ClassDecision},
	{Field: "Config.WorkerBootTimeout", Path: "worker_boot_timeout_seconds", Class: ClassDecision},
	{Field: "Config.WorkerBootRegistrationDeadlineIntervals", Path: "worker_boot_registration_deadline_intervals", Class: ClassDecision},
	// No holder reads it today: the whole of its behaviour is the value the state carries.
	{Field: "Config.TreeStopTimeout", Path: "tree_stop_timeout_seconds", Class: ClassDecision},

	// The workflow's own decisions, each read as the decision is made.
	{Field: "Config.Linger", Path: "linger_hours", Class: ClassDecision},
	{Field: "Config.ProductionCheckBound", Path: "production_check_hours", Class: ClassDecision},
	{Field: "Config.PodUpdateRetask", Path: "pod_update_retask_hours", Class: ClassDecision},
	{Field: "Config.PodUpdateNotice", Path: "pod_update_notice_hours", Class: ClassDecision},
	{Field: "Config.ReviewRoundCap", Path: "review_round_cap", Class: ClassDecision},
	{Field: "Config.MaxFixAttempts", Path: "max_fix_attempts", Class: ClassDecision},
	// The capability report is built from the configuration at every read of the state.
	{Field: "Config.Capabilities", Path: "capabilities", Class: ClassDecision, Block: true},
	{Field: "Capabilities.Decided", Path: "capabilities.decided", Class: ClassDecision},
	// The design gate is read per decision; the API server and the launch specs each hold a copy
	// of it, which their own rebuild refreshes, so the change takes effect at once where it
	// decides. A row names the one holder an operator is told was rebuilt (Change.Holder), which
	// is why this one names the API server alone.
	{Field: "Config.Gates", Path: "gates", Class: ClassDecision, Holder: HolderAPIServer, Block: true},
	{Field: "Gates.Design", Path: "gates.design", Class: ClassDecision, Holder: HolderAPIServer},

	// Held by a client, a listener or a connection.
	{Field: "Config.WorkerRPCTimeout", Path: "worker_rpc_timeout_seconds", Class: ClassRebuild, Holder: HolderWorkerStream},
	// Also supervise.Timeouts.Stop, read per decision; the runtime holds the stop grace it passes
	// each shim, so the stricter class is the runtime's rebuild.
	{Field: "Config.WorkerStopTimeout", Path: "worker_stop_timeout_seconds", Class: ClassRebuild, Holder: HolderRuntime},
	// The runtime's sweep period and its adopt timeout: both are its options, settled when it is
	// built. ProbeInterval is supervise.Timeouts.Probe too, and the stricter class wins.
	{Field: "Config.ProbeInterval", Path: "probe_interval_seconds", Class: ClassRebuild, Holder: HolderRuntime},
	{Field: "Config.SlowCommandTimeout", Path: "slow_command_timeout_seconds", Class: ClassRebuild, Holder: HolderRuntime},
	{Field: "Config.ControllerWakeInterval", Path: "controller_wake_interval_seconds", Class: ClassRebuild, Holder: HolderControllerKeeper},
	{Field: "Config.ControllerLaunch", Path: "controller", Class: ClassRebuild, Holder: HolderControllerKeeper},
	{Field: "Config.EnvoyURL", Path: "envoy_url", Class: ClassRebuild, Holder: HolderEnvoyPublisher, Secret: true},
	{Field: "Config.EnvoyTokenFile", Path: "envoy_token_file", Class: ClassRebuild, Holder: HolderEnvoyPublisher, Secret: true},
	{Field: "Config.NatsURLs", Path: "nats_urls", Class: ClassRebuild, Holder: HolderNATS, Secret: true},
	{Field: "Config.NatsNkeySeedFile", Path: "nats_nkey_seed_file", Class: ClassRebuild, Holder: HolderNATS, Secret: true},
	{Field: "Config.NatsDaemonNkeySeedFile", Path: "nats_daemon_nkey_seed_file", Class: ClassRebuild, Holder: HolderNATS, Secret: true},
	{Field: "Config.DispatchURL", Path: "dispatch_url", Class: ClassRebuild, Holder: HolderDispatchClient, Secret: true},
	{Field: "Config.DispatchTokenFile", Path: "dispatch_token_file", Class: ClassRebuild, Holder: HolderDispatchClient, Secret: true},
	// The bearer the operator routes compare against: the API server holds its hash.
	{Field: "Config.OperatorTokenFile", Path: "operator_token_file", Class: ClassRebuild, Holder: HolderAPIServer, Secret: true},
	// The settings route's own admission rule: the API server holds the verifier it checks a
	// projected ServiceAccount token with, and a change of any of the three builds that verifier
	// again (internal/daemon's apiHolder). Building one reads nothing — the issuer is discovered
	// at the first token it verifies — so the rebuild costs the running daemon no network read.
	{Field: "Config.SettingsRoute", Path: "settings_route", Class: ClassRebuild, Holder: HolderAPIServer, Block: true},
	{Field: "SettingsRoute.OIDCIssuer", Path: "settings_route.oidc_issuer", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "SettingsRoute.Audience", Path: "settings_route.audience", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "SettingsRoute.ServiceAccounts", Path: "settings_route.service_accounts", Class: ClassRebuild, Holder: HolderAPIServer},
	// The repository equivalence every comparison of two repository names reads. Four things
	// hold it: the API server's settings, the project's repository mapping, the intake
	// consumers' own decoding of a GitHub fact, and the gh bundles the runtime's launches hand
	// each role. The operator is told the API server was rebuilt, and the other three are built
	// again with it, so no reader is left comparing names by the boot's value.
	{Field: "Config.RepositoryRenames", Path: "repository_renames", Class: ClassRebuild, Holder: HolderAPIServer,
		Also: []string{HolderRepositoryMapping, HolderNATS, HolderRuntime}},
	// The projects block: the API server holds each project's merge rules, and the supervisor's
	// launch specs copy its merge rules and its Slack channels, which reach a role at its next
	// launch; the stricter class is the server's rebuild.
	{Field: "Config.Projects", Path: "projects", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Project.MergeQueueRole", Path: "projects.<KEY>.merge_queue_role", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Project.Merge", Path: "projects.<KEY>.merge", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Project.SlackChannels", Path: "projects.<KEY>.slack_channels", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Project.Deploys", Path: "projects.<KEY>.deploys", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Deploy.Repo", Path: "projects.<KEY>.deploys[].repo", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Deploy.Workflow", Path: "projects.<KEY>.deploys[].workflow", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "Deploy.Step", Path: "projects.<KEY>.deploys[].step", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "MergeRules.ByHand", Path: "projects.<KEY>.merge.by_hand", Class: ClassRebuild, Holder: HolderAPIServer},
	{Field: "PathPattern.Expression", Path: "projects.<KEY>.merge.by_hand[]", Class: ClassRebuild, Holder: HolderAPIServer},
	// Each App's credential: the manager mints every role's token from it.
	{Field: "Config.GitHubApps", Path: "github_apps", Class: ClassRebuild, Holder: HolderGitHubApps, Block: true},
	{Field: "GitHubApps.Implement", Path: "github_apps.implement", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},
	{Field: "GitHubApps.Review", Path: "github_apps.review", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},
	{Field: "GitHubApp.AppID", Path: "github_apps.<app>.app_id", Class: ClassRebuild, Holder: HolderGitHubApps},
	{Field: "GitHubApp.Installations", Path: "github_apps.<app>.installations", Class: ClassRebuild, Holder: HolderGitHubApps},
	{Field: "GitHubApp.PrivateKey", Path: "github_apps.<app>.private_key", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},
	{Field: "GitHubApp.PrivateKeyCommand", Path: "github_apps.<app>.private_key_command", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},
	{Field: "GitHubApp.PrivateKeySecret", Path: "github_apps.<app>.private_key_secret", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},
	// The fourth source of an App's credential, for a deployment whose keys only a secrets broker
	// holds: the manager runs it for a token, so a change of it rebuilds the manager as the three
	// key sources do.
	{Field: "GitHubApp.TokenCommand", Path: "github_apps.<app>.token_command", Class: ClassRebuild, Holder: HolderGitHubApps, Secret: true},

	// Read when a pod or a pane launches: the runtime and the launch specs hold them, and the
	// change reaches a role at its next launch.
	{Field: "Config.DaemonURL", Path: "daemon_url", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "Config.AdvertiseHost", Path: "advertise_host", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Config.OmpInvocation", Path: "omp_invocation", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Config.OmpLaunchPrefix", Path: "omp_launch_prefix", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Config.RoleConfigFiles", Path: "role_config_files", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Config.WorkerBackends", Path: "worker_backends", Class: ClassLaunch, Holder: HolderLaunchSpecs},
	{Field: "Config.InstructionsPath", Path: "instructions", Class: ClassLaunch, Holder: HolderLaunchSpecs},
	// The provider credentials themselves. One `provider_keys` entry is `<NAME>: <source>`: its
	// name and its source land in three fields of one entry, so the three share the entry's path.
	// Boot reads each source once, as the daemon, into a file of its own the runtime hands the
	// launch, so a change reaches a role at its next launch.
	{Field: "Config.ProviderKeys", Path: "provider_keys", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "ProviderKey.Env", Path: "provider_keys.<NAME>", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "ProviderKey.Secret", Path: "provider_keys.<NAME>", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "ProviderKey.File", Path: "provider_keys.<NAME>", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},

	// The kubernetes block. The runtime holds all of it — it caches each pod's shape when it is
	// built — so every change of it rebuilds the runtime; what the operator is told is whether the
	// value does anything before the next pod is built.
	{Field: "Kubernetes.Namespace", Path: "runtime.kubernetes.namespace", Class: ClassFixed, Why: whyNamespc},
	{Field: "Kubernetes.Image", Path: "runtime.kubernetes.image", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.StorageClass", Path: "runtime.kubernetes.storage_class", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.IssueVolume", Path: "runtime.kubernetes.issue_volume", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.SessionStore", Path: "runtime.kubernetes.session_store", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "Kubernetes.Setup", Path: "runtime.kubernetes.setup", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.TestPostgres", Path: "runtime.kubernetes.test_postgres", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.Scheduling", Path: "runtime.kubernetes.scheduling", Class: ClassLaunch, Holder: HolderRuntime, Block: true},
	{Field: "Scheduling.NodeSelector", Path: "runtime.kubernetes.scheduling.node_selector", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Scheduling.Tolerations", Path: "runtime.kubernetes.scheduling.tolerations", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Scheduling.PriorityClass", Path: "runtime.kubernetes.scheduling.priority_class", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Toleration.Key", Path: "runtime.kubernetes.scheduling.tolerations[].key", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Toleration.Operator", Path: "runtime.kubernetes.scheduling.tolerations[].operator", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Toleration.Value", Path: "runtime.kubernetes.scheduling.tolerations[].value", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Toleration.Effect", Path: "runtime.kubernetes.scheduling.tolerations[].effect", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.Resources", Path: "runtime.kubernetes.resources", Class: ClassLaunch, Holder: HolderRuntime, Block: true},
	{Field: "PodResources.Issue", Path: "runtime.kubernetes.resources.issue", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "PodResources.Controller", Path: "runtime.kubernetes.resources.controller", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.CPU", Path: "runtime.kubernetes.resources.<pod>.cpu", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.CPURequest", Path: "runtime.kubernetes.resources.<pod>.cpu_request", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.Memory", Path: "runtime.kubernetes.resources.<pod>.memory", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.MemoryRequest", Path: "runtime.kubernetes.resources.<pod>.memory_request", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.EphemeralStorage", Path: "runtime.kubernetes.resources.<pod>.ephemeral_storage", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Reservation.EphemeralStorageRequest", Path: "runtime.kubernetes.resources.<pod>.ephemeral_storage_request", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Kubernetes.Pod", Path: "runtime.kubernetes.pod", Class: ClassLaunch, Holder: HolderRuntime, Block: true},
	{Field: "PodConfig.Env", Path: "runtime.kubernetes.pod.env", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "PodConfig.Volumes", Path: "runtime.kubernetes.pod.volumes", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "PodConfig.VolumeMounts", Path: "runtime.kubernetes.pod.volume_mounts", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "PodConfig.ServiceAccount", Path: "runtime.kubernetes.pod.service_account", Class: ClassLaunch, Holder: HolderRuntime},
	// The broker a pod is enrolled with at its launch; token_file is the daemon's own login token.
	{Field: "Kubernetes.AgentSecrets", Path: "runtime.kubernetes.agent_secrets", Class: ClassLaunch, Holder: HolderRuntime, Block: true},
	{Field: "AgentSecretsConfig.URL", Path: "runtime.kubernetes.agent_secrets.url", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "AgentSecretsConfig.Audience", Path: "runtime.kubernetes.agent_secrets.audience", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "AgentSecretsConfig.TokenExpirySeconds", Path: "runtime.kubernetes.agent_secrets.token_expiry_seconds", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "AgentSecretsConfig.TokenFile", Path: "runtime.kubernetes.agent_secrets.token_file", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	// The plugin pair a stale-plugin relaunch installs into a kept pod.
	{Field: "Kubernetes.Plugins", Path: "runtime.kubernetes.plugins", Class: ClassLaunch, Holder: HolderRuntime, Block: true},
	{Field: "PluginsConfig.PiLegion", Path: "runtime.kubernetes.plugins.pi_legion", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "PluginsConfig.PiEnvoy", Path: "runtime.kubernetes.plugins.pi_envoy", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	// The daemon's own cluster credential, and the repository caches it keeps: pods of the
	// runtime's own, replaced when the runtime is built again, never role launches.
	{Field: "Kubernetes.Kubeconfig", Path: "runtime.kubernetes.kubeconfig", Class: ClassRebuild, Holder: HolderRuntime, Secret: true},
	{Field: "Kubernetes.Context", Path: "runtime.kubernetes.context", Class: ClassRebuild, Holder: HolderRuntime, Secret: true},
	{Field: "Kubernetes.RepositoryCache", Path: "runtime.kubernetes.repository_cache", Class: ClassRebuild, Holder: HolderRuntime, Block: true},
	{Field: "RepositoryCacheConfig.Volume", Path: "runtime.kubernetes.repository_cache.volume", Class: ClassRebuild, Holder: HolderRuntime},
	{Field: "RepositoryCacheConfig.RefreshMinutes", Path: "runtime.kubernetes.repository_cache.refresh_minutes", Class: ClassRebuild, Holder: HolderRuntime},
	{Field: "RepositoryCacheConfig.RetainHours", Path: "runtime.kubernetes.repository_cache.retain_hours", Class: ClassRebuild, Holder: HolderRuntime},
	{Field: "RepositoryCacheConfig.Resources", Path: "runtime.kubernetes.repository_cache.resources", Class: ClassRebuild, Holder: HolderRuntime},

	// The tmux block: what every pane's launch reads, and the take-over boot read once.
	{Field: "Tmux.PaneEnv", Path: "runtime.tmux.pane_env", Class: ClassLaunch, Holder: HolderRuntime, Secret: true},
	{Field: "Tmux.PiConfigFiles", Path: "runtime.tmux.pi_config_files", Class: ClassLaunch, Holder: HolderRuntime},
	{Field: "Tmux.TakeOver", Path: "runtime.tmux.take_over", Class: ClassFixed, Why: whyTakeOve},
}

// rows indexes Classes by field. The table is a literal, so the index is built once.
var rows = func() map[string]Row {
	index := make(map[string]Row, len(Classes))
	for _, row := range Classes {
		index[row.Field] = row
	}
	return index
}()

// rowOf is the row the field `<Type>.<Field>` is read into.
func rowOf(field string) (Row, bool) {
	row, ok := rows[field]
	return row, ok
}

// Paths is every dotted path the table holds, each once, sorted. Two fields of one key share a
// path, so it is shorter than the table.
func Paths() []string {
	paths := make([]string, 0, len(Classes))
	for _, row := range Classes {
		paths = append(paths, row.Path)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// ClassOf is the row a dotted path belongs to: the row whose path it is, or the first row whose
// path is the template it matches — a `<name>` segment matching any name the file chose, and a
// `name[]` segment any entry of that sequence. Rows that share a path agree on their class, so the
// first is the answer.
func ClassOf(path string) (Row, bool) {
	for _, row := range Classes {
		if row.Path == path {
			return row, true
		}
	}
	for _, row := range Classes {
		if matchesTemplate(row.Path, path) {
			return row, true
		}
	}
	return Row{}, false
}

// matchesTemplate is whether a concrete dotted path is one of the paths a template names.
func matchesTemplate(template, path string) bool {
	want, got := strings.Split(template, "."), strings.Split(path, ".")
	if len(want) != len(got) {
		return false
	}
	for i, segment := range want {
		switch {
		case segment == got[i]:
		case strings.HasPrefix(segment, "<") && strings.HasSuffix(segment, ">"):
			// A name the file chose, which may not be empty and names one level.
			if got[i] == "" {
				return false
			}
		case strings.HasSuffix(segment, "[]"):
			// One entry of a sequence, written `name[0]`.
			name := strings.TrimSuffix(segment, "[]")
			index, ok := strings.CutPrefix(got[i], name+"[")
			if !ok || !strings.HasSuffix(index, "]") || len(index) < 2 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
