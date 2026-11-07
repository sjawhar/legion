package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// goldenPath is the cross-language pin: Go writes the wire shape, and
// `packages/contracts/src/legion-api.test.ts` parses it through the zod schema the plugin reads
// with and holds the parse equal to the file, so a Go field with no zod line is named there. Go
// is the source of truth; the fixture is how the schema is held to it.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// settingsFixtureFile is the maximal kubernetes legion.yaml the state golden and the settings tests
// share: every key the loader models is set, and every value a credential, its source or its file
// could be is a planted fixture a test can look for in the wire bytes (a PEM that is no key, a DSN
// password that opens nothing, `*_file` paths, the kubeconfig, the session DSN secret's key, each
// provider key's store key, the pod's env values, volume, Secret and ServiceAccount names, userinfo
// on `envoy_url` and the first `nats_urls` entry, which the settings mask, and userinfo and a query
// on `plugins.pi_envoy`'s tarball URL, which the settings mask and drop while `pi_legion` pins the
// version form). Every path the settings emit (`state_dir`, `instructions`) is a fixed absolute
// literal, so the golden does not churn with the temporary directory the file is written under.
// Hosts are placeholders.
const settingsFixtureFile = `project: LEGION
port: 13370
bind: 0.0.0.0
advertise_host: legion-daemon.internal.example
postgres_dsn: postgres://legion:fixture-dsn-password@postgres.internal.example:5432/legion
state_dir: /var/lib/legion
admission_cap: 4
daemon_url: http://legion-daemon.internal.example:13370
worker_stream_port: 13371
provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY_FIXTURE, GEMINI_API_KEY: GEMINI_API_KEY_FIXTURE}
instructions: /etc/legion/deployment-instructions.md
worker_boot_timeout_seconds: 180
worker_boot_registration_deadline_intervals: 2
worker_rpc_timeout_seconds: 8
worker_stop_timeout_seconds: 30
tree_stop_timeout_seconds: 90
slow_command_timeout_seconds: 600
probe_interval_seconds: 20
controller_wake_interval_seconds: 1800
launch_failure_limit: 4
prompt_failure_limit: 5
prompt_retire_limit: 3
worker_wakes_per_minute: 12
worker_wake_burst: 2
operator_token_file: /var/run/legion/OPERATOR_TOKEN
envoy_url: http://legion:fixture-envoy-password@envoy-listener.internal.example:9020
nats_urls: [nats://legion:fixture-nats-password@nats.internal.example:4222, nats://nats-2.internal.example:4222]
envoy_token_file: /var/run/legion/ENVOY_TOKEN
nats_nkey_seed_file: /var/run/legion/NATS_NKEY_SEED
nats_daemon_nkey_seed_file: /var/run/legion/NATS_DAEMON_NKEY_SEED
dispatch_url: https://dispatch.internal.example
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  LEGION:
    slack_channels: [C0123ABCD, C0123ABCE]
    deploys:
      - {repo: sjawhar/legion, workflow: .github/workflows/release.yaml, step: Apply}
  WIDGETS:
    merge_queue_role: merge-queue
    merge:
      by_hand: ['^deploy/', '\.sql$']
    production_check: off
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {sjawhar: "2001", acme: "2002"}
  review:
    app_id: "1002"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {sjawhar: "2003"}
linger_hours: 1.5
production_check_hours: 240
pod_update_max_open_rounds: 6
pod_update_rounds_per_minute: 2
review_round_cap: 4
max_fix_attempts: 5
controller: daemon
worker_backends: {tester: omp}
repository_renames:
  - {from: acme/widgets-legacy, to: acme/widgets}
capabilities:
  decided:
    pool-capacity: "the pool is sized by hand for the admission cap"
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp3
    issue_volume: 30Gi
    kubeconfig: /etc/legion/kubeconfig
    context: legion-daemon@fixture-cluster
    session_store: postgres
    session_dsn_secret: SESSION_DSN_FIXTURE
    scheduling:
      node_selector: {node.kubernetes.io/instance-type: m7i.xlarge}
      tolerations:
        - {key: dedicated, operator: Equal, value: legion, effect: NoSchedule}
        - {key: spot, operator: Exists, effect: NoExecute}
      priority_class: legion-workers
    resources:
      issue: {cpu: 4, memory: 24Gi}
      controller: {cpu: 2, cpu_request: 1, memory: 6Gi, memory_request: 4Gi}
    pod:
      env: {PI_CONFIG_FILES: /etc/legion-operator/overlay.yml, OPERATOR_FIXTURE_FLAG: operator-fixture-value}
      service_account: fixture-operator-account
      volumes:
        - name: operator-creds
          secret: {name: fixture-operator-creds}
      volume_mounts:
        - {volume: operator-creds, mount_path: /etc/legion-operator/creds}
    agent_secrets:
      url: https://agent-secrets.internal.example
      token_file: /var/run/secrets/agent-secrets/token
      audience: fixture-broker-audience
      token_expiry_seconds: 1800
    plugins:
      pi_legion: 10.4.0
      pi_envoy: https://legion:fixture-plugin-password@plugins.internal.example/pi-envoy-10.4.0.tgz?token=fixture-plugin-token
    setup: [/bin/sh, -c, 'echo fixture-setup']
    skills_repository: acme/agent-skills
    repository_cache:
      volume: 40Gi
      refresh_minutes: 15
      retain_hours: 48
      resources: {cpu: 500m, memory: 1Gi}
`

// minimalSettingsFile is the smallest tmux legion.yaml that loads: every other setting is a
// default, which is what the Stage 3 and operator-claim goldens and the defaults test show.
const minimalSettingsFile = `project: DEMO
postgres_dsn: postgres://legion:fixture-dsn-password@127.0.0.1:5432/legion
state_dir: /var/lib/legion
`

// loadSettingsFile writes body as a legion.yaml under t.TempDir() and loads it as the daemon does,
// reading no environment, so the resolved values are the file's and the defaults alone.
func loadSettingsFile(t *testing.T, body string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// settingsFixtureConfig is settingsFixtureFile loaded.
func settingsFixtureConfig(t *testing.T) config.Config {
	t.Helper()
	return loadSettingsFile(t, settingsFixtureFile)
}

// minimalSettingsConfig is minimalSettingsFile loaded.
func minimalSettingsConfig(t *testing.T) config.Config {
	t.Helper()
	return loadSettingsFile(t, minimalSettingsFile)
}

// goldenSettingSources is where each setting of the fixture's daemon came from, written by the
// daemon's own provenance (SettingProvenance): every settable path is the file this boot read,
// and the one path an operator changed through the settings route names the route, who asked and
// when. The clock is pinned here, since a fixture must encode the same bytes at every run.
func goldenSettingSources() map[string]SettingSource {
	changed := time.Date(2026, 9, 22, 9, 40, 18, 0, time.UTC)
	sources := NewSettingProvenance(time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC))
	sources.now = func() time.Time { return changed }
	sources.Observe(nil, []config.Change{{
		Path: "admission_cap", Old: "2", New: "3",
		Class: config.ClassDecision, Outcome: config.OutcomeAtOnce,
		Source: config.SourceRoute, By: "ana@example.com",
	}})
	return sources.Sources()
}

// populatedState carries every optional the state shape has, so the fixture pins the full shape
// rather than Stage 1's empty record.
func populatedState(t *testing.T) State {
	t.Helper()
	approvedVersion := 6
	commit := "3f9c1a7e5b2d84c06f1a9e3b7d5c2a08e4f6b1d9"
	requiresChecks, requiresNothing := true, false
	waitedSince := time.Date(2026, 9, 22, 8, 5, 0, 0, time.UTC)
	answeredAt := time.Date(2026, 9, 22, 9, 21, 0, 0, time.UTC)
	state := State{
		Daemon: DaemonInfo{
			Project:       "legion",
			Commit:        &commit,
			SchemaVersion: 1,
			Boots:         4,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		SettingSources: goldenSettingSources(),
		Admission: Admission{
			Cap:     2,
			Active:  []string{"LEGION-208", "LEGION-210"},
			Waiting: []string{"LEGION-211"},
			// The roots of the live trees, which the slots no longer name: LEGION-207 closed, and
			// each of the other three roots a tree of its own.
			Trees: []string{"LEGION-208", "LEGION-210", "LEGION-212"},
			// LEGION-211 is the waiting root admission skips, so it is in `waiting` too;
			// LEGION-209 is a child of LEGION-208's tree the workflow holds at `admitted`, which
			// no waiting line carries. Both are listed, by issue key, with what each waits on —
			// LEGION-213 among them a key the daemon has no record of, which counts as unfinished.
			Blocked: []BlockedIssue{
				{Issue: "LEGION-209", BlockedBy: []string{"LEGION-210", "LEGION-213"}},
				{Issue: "LEGION-211", BlockedBy: []string{"LEGION-210"}},
			},
		},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:           "LEGION-208",
				Generation:    3,
				Phase:         phase.Implementing,
				Status:        "in_progress",
				Tree:          "LEGION-208",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Architect: &ClaimView{
					Session: "ses_architect_208",
					State:   "ready",
					Locator: &runtime.Locator{
						Runtime:     runtime.RuntimeTmux,
						Claim:       "legion-legion-legion-208-architect",
						Incarnation: "31881:9442101",
						Tmux:        &runtime.TmuxLocator{Window: "@4", Pane: "%12"},
					},
				},
				Workers: map[claim.Role]PhaseView{
					claim.RolePlanner: {
						Claim: ClaimView{
							Session: "ses_planner_208",
							State:   "suspended",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-planner",
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/2",
								Sandbox: &runtime.SandboxLocator{
									Namespace: "legion", Name: "legion-legion-legion-208", PodUID: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
									Container: "roles", Generation: 2,
								},
							},
							// The pod's update round, which every claim of that pod shares: the daemon
							// replaces the pod only once each has answered. This one has, and reported
							// the head it pushed.
							UpdateRound: "pod:7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
							UpdateHead:  "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
						},
						HandoffRecord: &HandoffRecordView{ID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 1},
						Rounds:        0,
					},
					claim.RoleImplementer: {
						Claim: ClaimView{
							Session: "ses_implementer_208",
							State:   "working",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-implementer",
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
								Sandbox: &runtime.SandboxLocator{
									Namespace: "legion", Name: "legion-legion-legion-208", PodUID: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
									Container: "roles", Generation: 5,
								},
							},
							// The session's capability report (LEGION-663), from the process the locator
							// names: two rows it proved and one it did not, with the fact its check found.
							Capabilities: &CapabilityReportView{
								MeasuredAt:  time.Date(2026, 9, 22, 9, 17, 3, 0, time.UTC),
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
								OK:          []string{"subagents", "dispatch-envoy-tools"},
								Open:        []CapabilityGap{{Name: "github", Detail: "gh api user: HTTP 401: Bad credentials"}},
							},
							// The same round, on the worker that has not answered it yet: no head is
							// reported until a worker answers.
							UpdateRound: "pod:7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
						},
						HandoffCommit: "",
						Rounds:        2,
					},
				},
				PullRequest: &PullRequestView{
					Repo:           "acme/widgets",
					Number:         1247,
					Head:           "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29",
					State:          "open",
					ChecksVerdict:  "pending",
					ReviewDecision: "changes_requested",
					// Its base branch requires a check, so the pending verdict is a CI wait.
					RequiresChecks: &requiresChecks,
					FixAttempts:    1,
				},
				DesignGate: &GateView{
					ArtifactID:      "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
					CurrentVersion:  7,
					ApprovedVersion: &approvedVersion,
				},
				Slot: &SlotView{
					Index:      0,
					AdmittedAt: time.Date(2026, 9, 21, 18, 42, 11, 0, time.UTC),
				},
			},
			"LEGION-210": {
				Key:           "LEGION-210",
				Generation:    1,
				Phase:         phase.Held,
				Status:        "in_progress",
				Tree:          "LEGION-210",
				Repo:          "zeta/tools",
				DefaultBranch: "release/v7.1",
				HoldReason:    "escalated",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer_210", State: "failed"}, Rounds: 1},
				},
			},
			// LEGION-212's architect asked a person a question and handed the turn back, which gave
			// its slot up; the person has answered the question.
			"LEGION-212": {
				Key:           "LEGION-212",
				Generation:    1,
				Phase:         phase.AwaitingPerson,
				Status:        "in_progress",
				Tree:          "LEGION-212",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
				WaitingOn: &WaitingOnView{
					Kind:       "ask",
					Ask:        "9a8fd348-55d5-4d72-a793-3cd037f75f13",
					Since:      &waitedSince,
					AnsweredAt: &answeredAt,
					AnsweredBy: record.AnsweredByAsk,
				},
			},
			// LEGION-207 merged and closed; its production check opened before this release, so the
			// migration left the implementer on it (20261105231916_production_check_role_and_wake),
			// and it is open at its second look, woken by the Dispatch issue.deployed that release
			// still read, with that implementer working the look in the kept pod.
			"LEGION-207": {
				Key:           "LEGION-207",
				Generation:    2,
				Phase:         phase.Done,
				Status:        "done",
				Tree:          "LEGION-207",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer_207", State: "working"}, HandoffCommit: "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40", Rounds: 1},
				},
				PullRequest: &PullRequestView{
					Repo:           "acme/widgets",
					Number:         1239,
					Head:           "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40",
					State:          "merged",
					ReviewDecision: "approved",
					// A base branch that requires nothing, or one whose rules the daemon has not
					// read: either way it knows of no required check there.
					RequiresChecks: &requiresNothing,
				},
				// A check of this release records the project's `production_check` role, the tester;
				// this one opened before it and keeps the implementer, which is the migrated case
				// the release must keep working. Look 2 is open, so no agent's wake stands and the
				// next look is the check's bound.
				Check: &CheckView{
					Repo:        "acme/widgets",
					PullRequest: 1239,
					MergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
					OpenedAt:    time.Date(2026, 9, 22, 8, 40, 12, 0, time.UTC),
					Role:        string(claim.RoleImplementer),
					Looks:       2,
					LookOpen:    true,
					LastWake:    &CheckWakeView{Kind: "issue.deployed", Detail: "run 38230245571 completed 2026-09-22T09:10:44Z", At: time.Date(2026, 9, 22, 9, 10, 51, 0, time.UTC)},
					NextWakeAt:  time.Date(2026, 10, 6, 8, 40, 12, 0, time.UTC),
				},
			},
		},
		// LEGION-208's lifecycle lane is running row 801; the supervise row queued after it came due
		// two and a half minutes ago and waits behind it (record.Store.HeldRows).
		HeldOutboxRows: []HeldOutboxRow{{ID: 802, Kind: "supervise", Issue: "LEGION-208", WaitedSeconds: 150, WaitsOn: 801}},
		// LEGION-210's implementer failed while implementing: its issue is held from that phase,
		// and nothing retries the claim until its architect retries the phase. LEGION-211's
		// implementer is the other kind, a released pod whose wake never brought it back: who
		// suspended it is read on the operator claim routes, and the entry here carries the same
		// three keys as every other, which is what a session on the contract before this one
		// parses.
		StrandedClaims: []StrandedClaim{
			{Issue: "LEGION-210", Role: claim.RoleImplementer, State: "failed"},
			{Issue: "LEGION-211", Role: claim.RoleImplementer, State: "suspended"},
		},
		// A wave going out at the pace its operator set, with one wake of each lane still held:
		// LEGION-207's implementer, working its production check in the pod the daemon keeps, waits
		// for the update task its plugin round queues it; LEGION-208's architect, already running,
		// waits for the catch-up a child's re-admission wrote it in place of a launch, which waits
		// on its outbox row rather than in a machine; and LEGION-208's planner, whose pod answered
		// its update round, waits for the launch that brings it up on the pod's replacement. The
		// two machine wakes are over a process that still runs, which the queue holds like any
		// other, and the three are one list in claim order whichever lane holds each.
		WakesWaiting: []WakeWaiting{
			{Claim: "legion-legion-legion-207-implementer", Issue: "LEGION-207", Role: claim.RoleImplementer,
				Kind: "plugin-task", DueAt: time.Date(2026, 9, 22, 9, 17, 56, 0, time.UTC)},
			{Claim: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: claim.RoleArchitect,
				Kind: "catch-up", DueAt: time.Date(2026, 9, 22, 9, 18, 3, 0, time.UTC)},
			{Claim: "legion-legion-legion-208-planner", Issue: "LEGION-208", Role: claim.RolePlanner,
				Kind: "launch", DueAt: time.Date(2026, 9, 22, 9, 18, 11, 0, time.UTC)},
		},
		ControllerLocator: &ControllerLocator{
			Runtime:      "kubernetes",
			External:     true,
			SessionID:    "ses_controller",
			RegisteredAt: time.Date(2026, 9, 22, 9, 16, 40, 0, time.UTC),
		},
		AgentSecretsLogin: &AgentSecretsLoginView{State: "issued"},
		// A kubernetes deployment whose image passed the probe, with the broker gap decided and
		// one role unreserved, and no session's report yet, so the fixture carries a decided row and
		// an open row beside the present, unchecked and withheld ones, rendered by the report
		// itself: a wording change there rewrites the fixture rather than leaving the pinned words
		// stale.
		Capabilities: CapabilityStatesOf(capabilities.Deployment{
			Runtime: "kubernetes", Probed: true, ModelFallback: "on",
			Decided:        map[capabilities.Name]string{capabilities.Secrets: "pods are enrolled with the secrets broker once dispatch://LEGION-205 lands; until then no pod reads a secret"},
			UnreservedPods: []capabilities.Pod{capabilities.PodIssue},
		}.Report()),
		// The project maps three repositories of two owners; the review App is not installed on one,
		// which the daemon leaves out. acme/widgets' cache is Ready and its last health read
		// answered, zeta/tools' could not be read, and acme/legacy, mapped no longer, keeps its cache
		// until its retention ends.
		Repositories: RepositoriesView{
			Owners: []string{"acme", "zeta"},
			Mapped: []MappedRepositoryView{
				{Repo: "acme/widgets", Default: true, Cache: &CacheView{
					Repo: "acme/widgets", Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
					Head: "34a7ac80e9c3389ffbc1bbeece371ccacc043992", LastFetchAt: time.Date(2026, 10, 16, 19, 20, 30, 0, time.UTC), Bytes: 6612345678,
				}},
				{Repo: "zeta/tools", Cache: &CacheView{
					Repo: "zeta/tools", Sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e", State: "failing",
					Reason: "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)",
				}},
			},
			Refused: []RefusedRepositoryView{{Repo: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets"}},
			// Dispatch's GitHub App is not installed on zeta/tools, which is served all the same.
			WithoutWebhookApp: []WebhookAppGapView{{Repo: "zeta/tools", Reason: "Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events and nothing redelivers them; the pass heals its pull requests every 2 minutes"}},
			Caches: []CacheView{{
				Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "starting", RetainedUntil: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
			}},
		},
		// The resolved maximal kubernetes file, so the fixture pins every key `settings` can carry.
		Settings: SettingsOf(settingsFixtureConfig(t)),
		// The three durable consumers the daemon opens, in the order Lag answers them
		// (intake.Consumers.Lag): the Dispatch consumer is caught up; the GitHub consumer has 412
		// messages waiting and has handled none for over a minute; the listener-wake consumer's
		// info read ran out of the bound.
		Consumers: ConsumerLagViewsOf([]intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: time.Date(2026, 9, 22, 9, 17, 50, 0, time.UTC)},
			{
				Name: "legion-go-LEGION-github", Waiting: 412, OldestAt: time.Date(2026, 9, 22, 9, 5, 34, 0, time.UTC), OldestAge: 754 * time.Second,
				LastHandledAt: time.Date(2026, 9, 22, 9, 16, 41, 0, time.UTC), Stalled: true,
			},
			{Name: "legion-go-LEGION-wake", Unavailable: "read the consumer's info: context deadline exceeded"},
		}),
	}
	// LEGION-208's tree was drawn into the memory group, so its root and LEGION-209, a child of
	// that tree, both show the draw as internal/projection fills them, while every other issue of
	// the fixture belongs to a tree no draw has reached.
	root := state.Issues["LEGION-208"]
	root.MemoryGroup = string(memorygroup.GroupMemory)
	root.MemoryDrawMode = string(config.MemoryRandom)
	root.MemoryDrawnAt = &memoryDrawnAt
	state.Issues["LEGION-208"] = root
	state.Issues["LEGION-209"] = Issue{
		Key:            "LEGION-209",
		Generation:     1,
		Phase:          phase.Admitted,
		Status:         "in_progress",
		Repo:           "acme/widgets",
		DefaultBranch:  "main",
		Workers:        map[claim.Role]PhaseView{},
		MemoryGroup:    string(memorygroup.GroupMemory),
		MemoryDrawMode: string(config.MemoryRandom),
		MemoryDrawnAt:  &memoryDrawnAt,
	}
	return state
}

func TestStateGolden(t *testing.T) {
	golden(t, "state.json", populatedState(t))
}

// Every plugin parses the state with the schema of the contract it was built at, and a
// Kubernetes deployment relaunches no stale session (INFRA-945 spec R10), so a session on the
// contract before this one reads this daemon's state for as long as its pod lives: a key added
// to a session-facing body refuses the whole of it at every plugin whose answer side predates
// INFRA-1048, and is dropped at every plugin after it, which loses the field rather than the
// state. `suspendedBy` is the key this change was tempted to add, and it belongs to the
// operator claim routes alone; the guard beside `legion-api.test.ts`'s schema tests holds the
// same property on the schema side.
func TestNoSessionFacingBodyCarriesSuspendedBy(t *testing.T) {
	// The body where the key does live, so a guard over nothing cannot pass for a clean one.
	operator, err := json.Marshal(OperatorClaims{Claims: []OperatorClaim{
		{Issue: "LEGION-211", Role: claim.RoleImplementer, State: "suspended", SuspendedBy: "released"},
	}})
	if err != nil {
		t.Fatalf("marshal the operator claim list: %v", err)
	}
	if !bytes.Contains(operator, []byte("suspendedBy")) {
		t.Fatalf("the operator claim list does not carry suspendedBy either: %s", operator)
	}
	encoded, err := json.Marshal(populatedState(t))
	if err != nil {
		t.Fatalf("marshal the state: %v", err)
	}
	if bytes.Contains(encoded, []byte("suspendedBy")) {
		t.Fatalf("the state body carries suspendedBy: %s", encoded)
	}
}

func TestStateStage3Golden(t *testing.T) {
	// A build that is not a release stamps no commit, and a daemon whose settings nothing has
	// changed reports the provenance of none: both are written all the same, as every caller at
	// settingSourcesContract reads them (State.SettingSources, DaemonInfo.Commit).
	unreleased := ""
	golden(t, "state-stage3.json", State{
		Daemon: DaemonInfo{
			Project:       "LEGION",
			Commit:        &unreleased,
			SchemaVersion: 3,
			Boots:         5,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 17, 30, 0, 0, time.UTC),
		},
		SettingSources: map[string]SettingSource{},
		Admission: Admission{
			Cap: 2, Active: []string{"LEGION-208"}, Waiting: []string{"LEGION-209"},
			Trees: []string{"LEGION-208"},
		},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:           "LEGION-208",
				Generation:    4,
				Phase:         phase.Reviewing,
				Status:        "needs_review",
				Tree:          "LEGION-208",
				Repo:          "sjawhar/legion-smoke",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{{
			Issue:     "LEGION-208",
			Payload:   json.RawMessage(`{"status":"needs_review"}`),
			Attempts:  2,
			NextAt:    time.Date(2026, 9, 22, 17, 32, 0, 0, time.UTC),
			LastError: "Dispatch unavailable",
		}},
		Settings: SettingsOf(minimalSettingsConfig(t)),
	})
}

// An operator's claim on an issue the workflow does not record is listed for the claim's sake,
// and says so rather than leaving its phase and status empty: an empty phase is no value of the
// phase enum, so it refuses the whole document, over a row that is a legitimate state.
// internal/projection's own test holds the producer to these values.
func TestStateOperatorClaimGolden(t *testing.T) {
	unreleased := ""
	golden(t, "state-operator-claim.json", State{
		Daemon: DaemonInfo{
			Project:       "AC91849371",
			Commit:        &unreleased,
			SchemaVersion: 3,
			Boots:         1,
			FirstBootAt:   time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC),
		},
		SettingSources: map[string]SettingSource{},
		Admission:      Admission{Cap: 2, Active: []string{}, Waiting: []string{}},
		Issues: map[string]Issue{
			"AC91849371-900": {
				Key:     "AC91849371-900",
				Phase:   Unrecorded,
				Status:  string(Unrecorded),
				Workers: map[claim.Role]PhaseView{},
				Architect: &ClaimView{
					Session: "01a0d71d-aace-7448-a0d4-e401d736438f",
					State:   "idle",
					Locator: &runtime.Locator{
						Runtime:     "tmux",
						Claim:       "legion-ac91849371-ac91849371-900-architect",
						Incarnation: "1618075:268131172",
						Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
					},
				},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{},
		Settings:            SettingsOf(minimalSettingsConfig(t)),
	})
}

// golden pins one response's wire shape: Go writes it (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it through the schema of the response it is
// and holds the parse equal to the file. A fixture that differs from what Go now writes is stale.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')

	path := goldenPath(name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/api/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}

// The register response is what the plugin reads its claim, its secret and the task agents its
// role's prompts dispatch from.
func TestRegisterResponseGolden(t *testing.T) {
	golden(t, "register.json", claim.RegisterResponse{
		ClaimToken:   "legion-legion-legion-208-architect",
		Tree:         "LEGION-208",
		Issue:        "LEGION-208",
		Role:         claim.RoleArchitect,
		Generation:   3,
		Secret:       "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		PromptAgents: []string{"deep-worker", "oracle", "plan-gap-analyst", "plan-reviewer", "thermonuclear-code-quality", "thermonuclear-deep-review"},
	})
}

// The ready request is what the plugin posts once its session can be prompted, and again carrying
// the capability report the session measured at an idle tick of the 5 s interval it already runs
// (LEGION-663, INFRA-927): the one request body of the claim routes the contract pins, since the
// report is the one with a shape of its own.
func TestReadyRequestGolden(t *testing.T) {
	golden(t, "ready.json", claim.ReadyRequest{
		ClaimToken: "legion-legion-legion-208-implementer",
		SessionID:  "ses_implementer_208",
		Secret:     "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		Generation: 5,
		Capabilities: &claim.CapabilityReport{
			MeasuredAt: time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC),
			ElapsedMs:  1200,
			Rows: []claim.CapabilityRow{
				{Name: "subagents", OK: true, Detail: "6 task agents discovered: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality, thermonuclear-deep-review"},
				{Name: "dispatch-envoy-tools", OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
				{Name: "github", OK: false, Detail: "gh api user: HTTP 401: Bad credentials"},
			},
		},
	})
}

// The ready-for-update request is the worker's answer to its pod's update task: the claim it
// holds, the generation it answers for, the round the task it read named — the one round the
// answer counts for — and the head of `legion/<issue>@origin` it reports, which the daemon
// records and verifies nothing of.
func TestReadyForUpdateRequestGolden(t *testing.T) {
	golden(t, "ready-for-update.json", claim.ReadyForUpdateRequest{
		ClaimToken: "legion-legion-legion-208-implementer",
		SessionID:  "ses_implementer_208",
		Secret:     "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		Generation: 5,
		Round:      "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
		Head:       "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35",
	})
}

// The release records a pod reads of its deployment's idle release, built through
// releaseRecordsBody from the store's own rows, so the fixture is what the route writes rather
// than a literal. The store's `Project` is no field of the wire — the route answers one
// project's records — so the rows carry only what is served.
//
// Two rows of each list, because one row of each pins less than the shape:
//   - the releases are a release that suspended two roles and saved a bundle, and one whose
//     stops suspended no claim and whose workspace held nothing unpushed (recordPodRelease
//     counts only the claims this release's own stops suspended, and the store reads an empty
//     `roles` column back as none): `roles` is `[]` and never `null`, and `bundleBytes` is 0
//     rather than absent;
//   - the resumes are a wake whose pod was Ready and whose role took its first turn, which is
//     the row that fills both optional stamps, and a wake of a pod with no release row recorded
//     for it (`release` is 0) that went ten minutes with no turn, so it is `stuck` and its
//     `firstTurnAt` is left out until something observes it;
//   - the holds are a background-work streak over eleven passes with the kinds its session
//     reported in `detail`, and a teardown's overtaken hold, which reads a hold and not the view
//     behind it and so carries no detail at all (outbox.recordPodHold).
func TestReleaseRecordsResponseGolden(t *testing.T) {
	released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	golden(t, "release-records.json", releaseRecordsBody(store.PodReleaseRecords{
		Releases: []store.PodRelease{
			{ID: 101, Issue: "LEGION-208", Generation: 3, Roles: []string{"implementer", "tester"},
				At: released, BundleBytes: 4096},
			{ID: 102, Issue: "LEGION-209", Generation: 1, At: released.Add(15 * time.Minute)},
		},
		Resumes: []store.PodResume{
			{ID: 201, Release: 101, Issue: "LEGION-208", Role: "implementer", Kind: "delivery",
				At: released.Add(30 * time.Minute), PodReadyAt: released.Add(31 * time.Minute),
				FirstTurnAt: released.Add(32 * time.Minute)},
			{ID: 202, Issue: "LEGION-207", Role: "architect", Kind: "backstop",
				At: released.Add(45 * time.Minute), PodReadyAt: released.Add(46 * time.Minute), Stuck: true},
		},
		Holds: []store.PodHold{
			{ID: 301, Issue: "LEGION-210", Reason: "background_work", Detail: "subagent,async-job",
				FirstAt: released.Add(time.Hour), At: released.Add(70 * time.Minute), Passes: 11},
			{ID: 302, Issue: "LEGION-211", Reason: "overtaken",
				FirstAt: released.Add(65 * time.Minute), At: released.Add(65 * time.Minute), Passes: 1},
		},
	}))
}

// A session's report shows on its claim as the names it proved and the gaps it did not, each side
// in the report's order, and both sides are arrays on the wire even when empty: a session that
// proved every row has `"open": []`, not `null`, under the plugin's reader.
func TestCapabilityReportViewPartitionsTheRowsAndNeverMarshalsNull(t *testing.T) {
	measured := time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC)
	report := capabilities.Report{
		Claim: "legion-legion-legion-208-implementer", Generation: 5, MeasuredAt: measured,
		Locator: runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: "legion-legion-legion-208-implementer", Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5"},
		Rows: []capabilities.Row{
			{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered"},
			{Name: capabilities.WebSearch, Detail: "web_search is not an active tool"},
			{Name: capabilities.MCP, OK: true, Detail: "no MCP server is configured"},
			{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}
	want := &CapabilityReportView{
		MeasuredAt:  measured,
		Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
		OK:          []string{"subagents", "mcp"},
		Open: []CapabilityGap{
			{Name: "web-search", Detail: "web_search is not an active tool"},
			{Name: "github", Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}
	if got := CapabilityReportViewOf(report); !reflect.DeepEqual(got, want) {
		t.Fatalf("CapabilityReportViewOf = %#v, want %#v", got, want)
	}

	allOK := CapabilityReportViewOf(capabilities.Report{Rows: []capabilities.Row{{Name: capabilities.Subagents, OK: true}}})
	encoded, err := json.Marshal(allOK)
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"open":[]`)) {
		t.Fatalf("view = %s, want an empty open array", encoded)
	}
	noneOK := CapabilityReportViewOf(capabilities.Report{Rows: []capabilities.Row{{Name: capabilities.Subagents, Detail: "no task agent discovered"}}})
	if encoded, err = json.Marshal(noneOK); err != nil {
		t.Fatalf("marshal the view: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"ok":[]`)) {
		t.Fatalf("view = %s, want an empty ok array", encoded)
	}
}

// A session that registered with the controller capability learns the project's controller token
// and role, the capability's generation, and its registration's secret — no tree and no issue.
func TestControllerRegisterResponseGolden(t *testing.T) {
	golden(t, "register-controller.json", ControllerRegisterResponse{
		ClaimToken: "legion-legion-controller",
		Role:       claim.RoleController,
		Generation: 2,
		Secret:     "Q29udHJvbGxlZEJ5TGVnaW9u",
	})
}

// What `legion controller start` fetches with the operator's bearer.
func TestControllerSecretResponseGolden(t *testing.T) {
	golden(t, "controller-secret.json", ControllerSecretResponse{Secret: "Q2FwYWJpbGl0eUZvclRoZUNvbnRyb2xsZXI", DesignGate: config.DesignGateRootIssues})
}

// What `legion controller start` fetches for the review App's read-only tokens, one per owner,
// each rendered as the two files `gh` reads.
func TestControllerCredentialResponseGolden(t *testing.T) {
	expiresAt := time.Date(2026, 9, 22, 10, 15, 2, 0, time.UTC)
	acme := ghconfig.Render("ghs_placeholder_acme_token", "review", expiresAt)
	sjawhar := ghconfig.Render("ghs_placeholder_sjawhar_token", "review", expiresAt)
	golden(t, "controller-github-credential.json", ControllerCredentialResponse{
		Owners: map[string]ControllerOwnerCredential{
			"acme":    {Hosts: acme.Hosts, Config: acme.Config},
			"sjawhar": {Hosts: sjawhar.Hosts, Config: sjawhar.Config},
		},
		Default:   "acme",
		App:       "review",
		ExpiresAt: expiresAt.UTC().Format(timeFormat),
	})
}

// Every refusal a route answers is one sentence under `error`.
func TestErrorResponseGolden(t *testing.T) {
	golden(t, "error.json", claim.SameAgentRefusal)
}

// What `PUT /legion/v1/operator/settings` answers, and what Dispatch relays unchanged: one
// verdict per dotted path the request named, under `settings`. Every kind of verdict is here —
// a value read at the next decision, one a holder was rebuilt for, one a launch reads, a change
// the daemon was already running on, and a path refused by name.
func TestSettingsChangeResponseGolden(t *testing.T) {
	golden(t, "settings-change.json", SettingsChangeResponse{Settings: map[string]SettingResult{
		"admission_cap":            {Applied: string(config.OutcomeAtOnce), Old: "4", New: "6"},
		"envoy_url":                {Applied: string(config.OutcomeRebuilt), Holder: config.HolderEnvoyPublisher, Old: "http://envoy.internal:9020", New: "http://envoy.internal:9021"},
		"runtime.kubernetes.image": {Applied: string(config.OutcomeNextLaunch), Holder: config.HolderRuntime, Old: "(unset)", New: "ghcr.io/example/worker@sha256:placeholder"},
		"review_round_cap":         {Applied: string(config.OutcomeAtOnce), Old: "3", New: "3", Unchanged: true},
		"postgres_dsn":             {Refused: "the store's pool is connected at boot and holds every record the daemon is working from"},
	}})
}

// operatorClaim carries every optional an operator's view of a claim has.
func operatorClaim() OperatorClaim {
	delivered := time.Date(2026, 9, 22, 9, 31, 4, 0, time.UTC)
	return OperatorClaim{
		Token:       "legion-legion-legion-209-implementer",
		Tree:        "LEGION-208",
		Issue:       "LEGION-209",
		Role:        claim.RoleImplementer,
		Generation:  2,
		State:       "working",
		Session:     "ses_implementer_209",
		SessionFile: "/srv/legion/state/home/.omp/agent/sessions/ses_implementer_209.jsonl",
		Locator: &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       "legion-legion-legion-209-implementer",
			Incarnation: "40217:9551230",
			Tmux:        &runtime.TmuxLocator{Window: "@7", Pane: "%23"},
		},
		Budgets:         BudgetsView{LaunchFailures: 1, Deaths: 1, PromptFailures: 0, PromptRetires: 0},
		UncertainStreak: 0,
		Pending: &DeliveryView{
			ID:          "OVXA3ZC6BPIKTOQHKXHRZ7TYVB",
			Task:        "Implement the plan in .legion/plan.json.",
			QueuedAt:    time.Date(2026, 9, 22, 9, 30, 58, 0, time.UTC),
			DeliveredAt: &delivered,
			ConfirmedAt: &delivered,
			Interrupted: true,
		},
		SuspensionHeld: true,
		// The claim's own plugin round: its process speaks a plugin contract this daemon does not,
		// and it is replaced only once its worker has pushed its work and answered, which it has.
		UpdateRound: "plugin:43",
		UpdateHead:  "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
	}
}

// What the spawn, deliver, suspend, resume, and stop routes answer.
func TestOperatorClaimGolden(t *testing.T) {
	golden(t, "operator-claim.json", operatorClaim())
}

func TestForgetSessionOperatorClaimGolden(t *testing.T) {
	golden(t, "operator-forget-session.json", OperatorClaim{
		Token:                "legion-legion-legion-208-architect",
		Tree:                 "LEGION-208",
		Issue:                "LEGION-208",
		Role:                 claim.RoleArchitect,
		Generation:           4,
		State:                "failed",
		ForgottenSession:     "ses_architect",
		ForgottenSessionFile: "/sessions/ses_architect.jsonl",
		Budgets:              BudgetsView{LaunchFailures: 3},
		UncertainStreak:      0,
		PluginContract:       38,
	})
}

// What the list route answers: a queued claim carries no locator and no delivery yet. The
// pod-update record is left out of the value this marshals, so the fixture is also the proof
// that OperatorClaims.MarshalJSON writes the zero record for a nil field and the wire carries
// the object either way.
func TestOperatorClaimsGolden(t *testing.T) {
	golden(t, "operator-claims.json", OperatorClaims{Claims: []OperatorClaim{
		{
			Token:      "legion-legion-legion-208-architect",
			Tree:       "LEGION-208",
			Issue:      "LEGION-208",
			Role:       claim.RoleArchitect,
			Generation: 0,
			State:      "queued",
		},
		operatorClaim(),
	}})
}

// An issue the daemon has admitted but not yet given a worker still has to answer the plugin's
// reader: `workers` is an object, never `null`.
func TestIssueWithoutWorkersMarshalsAnEmptyObject(t *testing.T) {
	encoded, err := json.Marshal(Issue{Key: "LEGION-209", Phase: phase.Held})
	if err != nil {
		t.Fatalf("marshal issue: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"workers":{}`)) {
		t.Fatalf("issue = %s, want an empty workers object", encoded)
	}
}

// Every Task 3.10 route has a fixture. The TypeScript Go-daemon client parses these exact
// responses, so adding a route cannot quietly leave its wire shape undocumented.
func TestTask310RouteGoldens(t *testing.T) {
	golden(t, "grant.json", GrantResponse{
		GrantID: "grant-for-one-command", ExpiresAt: "2026-09-23T12:01:00Z",
	})
	golden(t, "handoff-complete.json", HandoffCompleteResponse{Note: "pull request #312 is already merged, so READY was published without reading its head's handoffs"})
	golden(t, "issue-status.json", IssueStatusResponse{})
	golden(t, "gate-register.json", GateRegisterResponse{})
	golden(t, "wave-release.json", WaveReleaseResponse{Released: []string{"LEGION-209"}})
	golden(t, "phase-backward.json", PhaseBackwardResponse{})
	golden(t, "phase-retry.json", PhaseRetryResponse{})
	golden(t, "signoff.json", SignOffResponse{})
	golden(t, "root-close.json", EmptyResponse{})
	golden(t, "child-park.json", EmptyResponse{})
	golden(t, "child-rerun.json", EmptyResponse{})
}
