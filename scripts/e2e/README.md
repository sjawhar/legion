# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these; `lib/` holds what
the stage scripts share: standalone helpers they run, and the files they source
([`lib/rig.sh`](#librigsh), [`lib/workflow.sh`](#libworkflowsh), [`lib/github-apps.sh`](#libgithub-appssh),
[`lib/namespace-rig.sh`](#libnamespace-rigsh), [`lib/session-db.sh`](#libsession-dbsh), [`lib/kind-sandbox.sh`](#libkind-sandboxsh), [`lib/omp-home.sh`](#libomp-homesh), [`lib/box-login.sh`](#libbox-loginsh)), which `shellcheck` follows from this directory
through `scripts/e2e/.shellcheckrc`.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state` with the 21-row capability report on it (`resource-limits` open under tmux, carrying the `capabilities.decided` line to write; `pool-capacity` present, since tmux runs no probe pod), registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
| `stage2-tmux-supervision.sh` | the Go daemon supervises real Oh My Pi sessions — the pinned build with this checkout's two plugins in an isolated profile — in its private tmux server, against a real Envoy listener and NATS: the plugin gate refuses another contract, a disabled plugin, a profile without the Envoy plugin, a missing skill, a skill only the role prompts load, and a task agent whose model role no one configured; an agent registers, holds its Envoy role and is ready; a task queued before ready runs once, its model turn through the configured route, and a retried frame starts no second turn; a killed pane resumes the same session; suspend and resume keep it; a stale hello is refused; an agent that never registers is retired at the deadline and counted; a restart re-adopts every live claim |
| `stage4a-sandbox-runtime.sh` | the Agent Sandbox runtime (`internal/runtime/sandbox`) on the production cluster, driven through the Legion daemon's restricted identity and nothing more, every check of the run keeping its sessions in a scratch Postgres of the run's own (`session_store: postgres`; the daemon requires a store that keeps sessions in a database): the Agent Sandbox install check accepts and refuses by name; the image probe Sandbox passes; a root provisions its workspace, registers, runs under gVisor and adopts its working copy's author; every container of its pod reserves the issue pod's one cpu and memory reservation with request equal to limit (the rig's overrides and the daemon's defaults both), the pod is `Guaranteed` with no affinity, and `nproc` and `MemTotal` inside it are that one reservation, which all six roles share; a second role joins the root's issue pod; suspend, resume, a same-agent refusal of a session the table lacks, a pod killed in place, and a relaunch before registration each hold; a new tree's root and child spawned at once each provision an issue pod whose Sandbox owns no claim template and whose disk is that pod's own, neither able to read the other's workspace; a fresh runtime re-adopts every live pod, and the pod it killed meanwhile comes back with a disk of a new uid and an empty marker on it, the claim resuming from the table; the orphan sweep honours its grace and takes the swept issue's pod and that pod's disk; releasing one role leaves the issue's pod and disk, and the tree's cleanup takes both; a launch that replaces an issue pod another role still runs in names the replaced pod on its Sandbox, and the running role is observed displaced and resumes into the new pod; closing an issue deletes its Sandbox with the role Secrets and leaves no disk, and the resume after it brings Sandbox, pod and disk afresh with `LEGION_WORKSPACE_RECREATED=true`; and the namespace matches its snapshot. Devbox only |
| `controller-start-tmux.sh` | the operator-launched controller on the Go daemon under tmux: `legion start --check-config` passes a real config, reporting after its OK line the two deployment capabilities a tmux file alone leaves open (secrets, resource-limits), and names the key on each broken variant, running no key command; the boot gate refuses a plugin of another contract; `legion state --config` runs no key command; `legion controller start` refuses a group-readable operator token file, claims the controller role, shows in `controllerLocator`, runs Oh My Pi interactive with the controller environment and its secret only as a file, leaves Ctrl-C to Oh My Pi, and exits with its code; `legion status` from an operator shell mints its grant with the operator bearer; a second start revokes the first's capability and grants; the controller liveness probe reads the live listener. Devbox only |
| `dispatch-user-turns.sh` | a person's direct Send or Aside from Dispatch's conversation page reaches a real Oh My Pi session — the pinned build with this checkout's Envoy plugin in an isolated profile — as that person's own user turn, the body alone, carrying its Dispatch id as the host's tag, while a BTW stays a side question; the composer completes the session's slash commands, and a skill, a prompt template, a file command, an extension command, `/session`, `/compact`, `/new` and an unknown `/foo` each run from the page as they would at the session's terminal; a frame a session forged claiming a person wrote it, a broadcast, an issue message, a Legion role notice and a session's re-send of the person's BTW through the retry route each arrive as a card; a Send the session got as a card stays one when a frame is forged for it inside the accept's minute, while the person's retry of it is their turn; the page shows each message once; after the session restarts, a replay of the Send's own envelope and a frame forged naming a Send made while it was down, over a minute old, each inject nothing, and neither does a frame a bare bus client forges for a failed Send inside its minute; and Dispatch records only the Send, the Aside and the carded Send's retry as accepted. Devbox only |
| `dispatch-session-settings.sh` | Dispatch's live view shows a real Oh My Pi session's current model and thinking level and switches them (INFRA-195), run in an issue pod: Oh My Pi in `--mode rpc` with this checkout's Envoy plugin, a real listener, `nats-server`, Postgres and Dispatch, and one held-open Chromium page. Each value the header shows matches the session's own RPC answers, its `/effort` output and its session file: before any turn; within 20 s of a change at the terminal, by a message or by a fallback; after a pick from either picker, including `auto` kept across a model pick, the level a model pick leaves (compared with a twin session), a pick during a turn, a refusal, and a pick to a stopped session that ends in the no-answer line and is refused `ACCEPT_STALE` once it resumes. A forged switch, an Envoy message and an agent's bearer change nothing. Another session, a resumed session and a new session each keep their own values. A session on the released pi-envoy shows the restart notice. A switch is stored under the person who sent it |
| `claude-backend-tmux.sh` | `worker_backends` on tmux: an implementer the daemon runs on Claude Code (`legion claude-bridge -- claude -p`) registers its Claude Code transcript as its session, takes its phase, is reminded by Claude Code's own `Stop` hook when a turn of its ends with that phase still open, completes it through the `legion` MCP tool's `handoff_complete` with a pushed `.legion/<issue>/` handoff commit, and, its pane killed, relaunches with `claude --resume` into the same session id, its earlier turns unchanged on disk and in its context. Devbox only |
| `claude-backend-sandbox.sh` | `worker_backends` under `runtime: kubernetes`, on a real cluster: the implementer's pod launches and its claim registers that pod's Claude Code transcript, under the `CLAUDE_CONFIG_DIR` the launch places on the pod's own disk, as its session, which its launcher keeps as a row of the session table; its command line, read in the implementer's own namespaces of the pod's one `roles` container, holds `claude-bridge --` and `--session-id <id>`, runs the pinned Claude Code its generation's start script installed on the pod's disk (the image carries the pin and no Claude Code), and reads its `mcp.json` from the launcher file of its own generation; Claude Code's own `Stop` hook reminds it when a turn of its ends with its phase still open; it completes that phase through the `legion` MCP tool's `handoff_complete` with a pushed `.legion/<issue>/` handoff commit; and, the pod deleted, its replacement installs its own generation's copy and runs `claude --resume <that id>` on the transcript its launcher restored from the table, its earlier bytes unchanged, and an earlier marker quoted back out of its context. Devbox only |
| `verifiers-staging-token.sh` | `dispatch` and the Envoy listener authenticate a projected service-account token the staging EKS cluster actually minted — the right audience is accepted, the other binary's audience and a missing bearer are refused, each shared token still works, half an OIDC pair and an issuer that does not answer refuse the boot, and a refused token leaves its failure class in the log and nowhere else |
| `burstable-kind.sh` | on a throwaway kind cluster with Agent Sandbox v1.0.3 and gVisor, an issue pod Legion's own loader, `sandboxOptions` and Sandbox runtime build from a `legion.yaml` giving the issue pod `issue: {memory: 32Gi, memory_request: 8Gi}` is admitted, scheduled and recorded `Burstable`, its `roles` container carrying exactly that request and limit, while one from a file with no `resources` block stays `Guaranteed`; `legion start --check-config` names the Burstable issue pod and refuses a request past its limit or that is no quantity, naming the key |
| `controller-sweep-kind.sh` | on a throwaway kind cluster with Agent Sandbox v1.0.3 and gVisor, the project controller Legion's own loader, `sandboxOptions` and boot's Sandbox runtime factory launch from a `controller: daemon` `legion.yaml` over a real Postgres store keeps its Sandbox and volume through five orphan sweeps at grace 0 told claims read before its launch, because the store holds its claim unretired, and loses both to the first sweep once the claim is retired; the same launch and sweep on a driver whose sweep reads no claim (the sweep before the fix) deletes the launched controller's Sandbox |
| `share-volumes-kind.sh` | on a throwaway kind cluster with Agent Sandbox v1.0.3 and gVisor, an issue pod Legion's own loader, `sandboxOptions` and Sandbox runtime build from a `legion.yaml` whose `runtime.kubernetes.pod` mounts an existing NFS share twice — through a PersistentVolumeClaim (read-only by default) and as an `nfs` export with `read_only: false` — reads the server's seed file through the claim, is refused a write there, and writes a file through the export that the server then holds; `legion start --check-config` refuses a volume naming two sources, and `legion probe-boot` mounts both shares and refuses a missing claim, a missing export and a server that never answers, each naming the volume and its share, and an image digest the registry does not have, naming the image |
| `merged-root-waits.sh` | the daemon keeps a tree's order, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own: a `sign_off` of a root with a child in the workflow is refused 409 `SIGNOFF_CHILD_IN_WORKFLOW` naming the child, and lands once that child is parked; the root's merge, as GitHub's own envelope, leaves it in `awaiting_merge` with its pull request `merged`, no Dispatch `done`, no suspension, its slot kept and its architect told once by a `pr-merged` notice naming the child; a restart there leaves the wait and tells nobody again; the child's implementer completes its phase in that wait, where a lingering tree refuses it 409 `TREE_LINGERING`; a pull request GitHub reports opened from the waiting root's branch leaves the merged one the root waits on recorded; and the child's own merge closes it and the root behind it, with the tree suspended, the slot freed and the root's production check open on the root's merged pull request |
| `no-required-checks.sh` | a pull request whose base branch requires no check and no workflow has no CI verdict, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own, with GitHub's settlements, push and synchronize as its own envelopes and what each base requires read by the daemon's own required-checks pass from a GitHub stand-in the driver switches: a failed check on a base that requires nothing leaves no `checksVerdict` key on `GET /legion/v1/state`, the issue in `testing` and no `checks-red`; the same failure once that base requires the check is `red`, sends the work to `implementing` and tells one `checks-red` naming it; a passing required check is `green`; and at an exhausted fix-attempt count a code push onto the held head once its base requires nothing leaves the counted attempt, `fixAttempts` unchanged, so the new head's red once the base requires the check again sends the work back with a `checks-red` rather than being held |
| `TestRealGitHubCredentialSurface` | the real `api.NewServer`'s App leases, rendered as the gh files a pane's `GH_CONFIG_DIR` holds, make the real `gh` identify as the implementer and reviewer bots, an empty `GH_CONFIG_DIR` authenticates nobody, and `git` clones the smoke repository through `gh auth git-credential` from the implement App's files alone. Devbox only |
| `TestRealGitHubReadyHoldsTheHeadToRetrosCommits` | the daemon's `POST /legion/v1/handoff/complete` with `ready: true`, over a real Postgres and as the real implement App, holds a pull request's head to its review round's approved head plus retro's commits: GitHub's compare of the two must show nothing outside `docs/solutions/` and `.legion/<issue>/`, or READY is refused `READY_UNREVIEWED_CHANGE` naming the paths. Devbox only |

## TestRealGitHubCredentialSurface

```sh
LEGION_REAL_GITHUB=1 LEGION_TEST_PG_DSN=postgres://… \
  go -C packages/daemon test -count=1 ./internal/api \
    -run '^TestRealGitHubCredentialSurface$' -v
```

The test is intentionally gated because it calls GitHub as both installed Apps: the default
pair unless the four App inputs name another's. It reads both Apps by running
[`lib/github-apps.sh`](#libgithub-appssh) itself, so it takes that lib's defaults, input checks and
key command, with no copy of its own: each unset input takes the default pair's value, and a
malformed one fails the run by name, never printing it. Before it acts, it proves both Apps
against GitHub's `GET /app` as the stage proofs do (the lib's `check_github_apps` on the very block
it uses), which needs `bun` on `PATH` (and `jq` for its log line). The ungated
`TestLiveProofAppsReadTheLib` proves the read in every `go test`.
It resolves each
App key through its `private_key_command`, mints each App's lease through the real token source,
writes the gh files the daemon would render for a pane, and drives the real `gh` and `git` against
them. The test's temporary gh directories and clone directory are removed by Go's test cleanup;
the GitHub operations are read-only except for locally cloning the smoke repository.

## TestRealGitHubReadyHoldsTheHeadToRetrosCommits

```sh
LEGION_REAL_GITHUB=1 LEGION_TEST_PG_DSN=postgres://… \
LEGION_READY_SCOPE_REPO=<owner>/<repo> LEGION_READY_SCOPE_PR=<n> LEGION_READY_SCOPE_ISSUE=<KEY> \
LEGION_READY_SCOPE_APPROVED=<approved sha> LEGION_READY_SCOPE_HEAD=<head sha> \
[LEGION_READY_SCOPE_REFUSE=<path>] \
  go -C packages/daemon test -count=1 ./internal/api \
    -run '^TestRealGitHubReadyHoldsTheHeadToRetrosCommits$' -v
```

It records the issue at `merging` with the pull request at the head and the review round's
approval of the approved sha, posts the merger's READY through the real route, and runs the check
itself (`unreviewedChange`) on the same two heads with the implement App's token, since READY
publishes a pull request already merged without reading it. With `LEGION_READY_SCOPE_REFUSE` set,
both must be refused `READY_UNREVIEWED_CHANGE` naming that path. Unset, the check must pass, and
READY must be accepted, or refused only by a check that runs after it: the head's handoffs, a
conflict or a by-hand file list. It only reads GitHub; the pull request and its
two heads are the operator's to make and close. As `TestRealGitHubCredentialSurface` does, it reads
both Apps from [`lib/github-apps.sh`](#libgithub-appssh) and checks them against `GET /app` before
it acts, which needs `bun` on `PATH` (and `jq` for its log line).


## stage1-skeleton.sh

```sh
bash scripts/e2e/stage1-skeleton.sh     # → "stage 1 e2e: PASS", exit 0
```

Needs `go`, `docker`, `jq`, `curl`, `ss` and `tmux`. It builds the binary from the checkout
(`packages/daemon/cmd/legion`), so it proves the tree you are standing in. When the first tmux on
`PATH` is a script or mise itself, as on a devbox, the run's `PATH`, and so the daemon's, starts with
the real binary that `mise which tmux` names in `packages/daemon`, the daemon's working directory
([`real_bin_path`](#librigsh) says why). Such a devbox needs tmux pinned in mise. CI's tmux is the
runner's apt binary, which the run leaves alone.

The daemon supervises its agents under tmux, and it refuses to start without what a launch needs
— tmux on `PATH`, an `operator_token_file`, and an OMP to run — and before anything else its
plugin gate holds the Oh My Pi plugin a pane would load to this daemon's contract
(`packages/daemon/internal/daemon/bootgate.go`). The run supplies all of it: its `legion.yaml`
names a 0600 operator token file in the work directory; `LEGION_OMP_PATH` points at a stub there
that answers the gate's load probe (`omp models --extension <probe> --json`) as a pane loading both
plugins does — `LEGION_PLUGIN_LOADED=yes` and, beside it, `LEGION_PLUGIN_LOADED_FROM=file://…/dist/legion.js`
inside the `@sjawhar/pi-legion` package the gate read and `LEGION_PLUGIN_ENVOY_INTERFACE=1`, then
`LEGION_ENVOY_INTERFACE=1` (one number on both lines, whatever the plugins' own version: the gate
compares the two with each other, never with a constant) and `LEGION_ENVOY_LOADED_FROM=file://…/dist/envoy.js` inside an
`@sjawhar/pi-envoy` package; `LEGION_PROMPT_AGENTS=resolved` and `LEGION_PROMPT_SKILLS=resolved`
when the gate asks for the task agents and skills the role prompts name; and
`LEGION_AGENT_MODELS=resolved` when it asks whether those agents' models resolve — and exits 1 on
anything else, because this proof launches no agent; and the daemon runs with `HOME=<work>/home`
and no `OMP_PROFILE`/`PI_PROFILE`, where
`<work>/home/.omp/plugins/node_modules/@sjawhar/pi-legion/package.json` is the checkout's
own manifest (`name`, `version`, `legion`), so it declares the contract this checkout's daemon
requires, and `@sjawhar/pi-envoy/package.json` beside it owns the file the stub says the Envoy
plugin loaded from. The real gate against a real Oh My Pi is proven where a proof installs the
plugins (`lib/install-plugin-profile.sh`); this one proves the skeleton.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_PG_DSN` | unset | the Postgres to run against. Unset, the script starts its own `postgres:16` container (`legion-e2e-pg-<pid>`) on an ephemeral loopback port and removes it on the way out — the devbox path. Set, it starts no container: that is how CI hands it the job's service. |

Everything the run takes is its own, so two runs on one box — a CI job and a devbox session, or
two sessions — neither collide nor report each other as a leftover:

- a `mktemp -d` work directory (`/tmp/legion-e2e.XXXXXXXX`) — the built binary, the two
  `legion.yaml`s, the operator token file and the OMP stub, the daemon's `home` with the plugin
  manifest, the two state documents, the refusal log. Removed when the run passes; **kept when it
  fails**, and its path printed, because those documents are the evidence.
- `XDG_STATE_HOME=<work>/xdg` — so the legions registry the run writes is its own, never the
  box's `~/.local/state/legion/legions.json`.
- a per-run project key (`E2E<pid><epoch>`) — boots are counted per project, so a fresh key is
  what makes `boots == 1` true on a store that has served other runs.
- a free daemon port picked per run by [`lib/free-port.sh`](#libfree-portsh), below the kernel's
  ephemeral range, and on the devbox path the container `legion-e2e-pg-<pid>`.

After any exit — pass, failure, or an interrupt — the `EXIT` trap removes the container and
signals the daemon; `docker ps -a --filter name=legion-e2e-pg` comes back empty and no daemon is
left holding the run's port.

### How it fails

Every step is fatal and names its reason: no port to pick ([`lib/free-port.sh`](#libfree-portsh):
`ss` missing or failing, or no room below the ephemeral range), the refusal that does not name its
host, a first tmux that is a script or mise itself while `mise which tmux` names no tmux
(`real_bin_path` refuses), a daemon that exits before it answers `/healthz` (the run stops at once
and names the daemon's exit status),
a `/healthz` that never answers, a state document that is not boot 1 with cap 4 and no
issues, a registry without exactly one entry for this run, a `firstBootAt` that moved across the
restart, a daemon that ignored a stop (SIGKILLed on the way out, so nothing holds the port) or
exited non-zero on one. `stop_daemon` reads and judges the exit status, because `daemon.Run`
returns 0 on a cancelled context — a non-zero status there is a defect, not a stop.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`). The container
  entrypoint's bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and
  a daemon that connects in that window is reset by the peer.
- The "never the password" assertion is written `grep -q … && exit 1`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail
  the run.

### In CI

On every pull request, the required `typecheck` job of `.github/workflows/pr-and-main.yaml` runs
`go vet -tags e2e ./...` (which also compiles the e2e-tagged Stage 4a harness) in
`packages/daemon`, and its required `test` job installs `tmux` — the tmux runtime's tests drive a
real tmux server and skip without one, and the daemon refuses to start without it — and the pinned
`jj` (through mise, as the pi-legion job does) — the workspace tests drive a real jj, and the daemon
resolves jj at boot and refuses to start without it — then runs `go test ./...` against its
Postgres service (`LEGION_TEST_PG_DSN`, and `BROKER_TEST_DATABASE_URL` for the real broker
that `internal/agentsecrets`'s contract tests drive, which fail there rather than skip without it),
then this script with `LEGION_E2E_PG_DSN` pointing at the same service, so the script runs no
docker of its own there.

## stage2-tmux-supervision.sh

```sh
LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/stage2-tmux-supervision.sh     # → "stage 2 e2e: PASS", exit 0, in about four minutes
LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> bash scripts/e2e/stage2-tmux-supervision.sh           # the same, for a devbox with no Hawk gateway
LEGION_E2E_MODEL_ROUTE=box-login bash scripts/e2e/stage2-tmux-supervision.sh                                                   # the same, on the operator's own Oh My Pi login
```

**Devbox only.** It runs a real Oh My Pi that calls a real model, so it needs `go`, `docker`,
`jq`, `curl`, `ss`, `tmux`, `socat`, `bun`, `mise` (with the pinned OMP build it installs if
missing), the `secrets` CLI holding `GEMINI_API_KEY_TESTS` (agent tier: no YubiKey touch), and
`LEGION_E2E_MODEL_ROUTE` (required, `gateway`, `key-file` or `box-login`), which picks how the
agents reach Anthropic, independently of which devbox this runs on:
- `gateway`: the operator's model gateway access — `LEGION_E2E_MODEL_GATEWAY_URL` naming the
  gateway's Anthropic endpoint (required in this mode;
  [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh)), `hawk-token` on `PATH`, their
  `hawk login`, and the GNOME keyring holding it unlocked (every reboot locks it; the
  `unlock-keyring` skill).
- `key-file`: `LEGION_E2E_ANTHROPIC_KEY_FILE` (required in this mode), a 0600 file holding a
  plaintext Anthropic key, for a devbox with no Hawk gateway at all
  ([`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh)).
- `box-login`: nothing more, for a devbox whose operator lets Legion's test runs use their own
  Oh My Pi login. The profile's `agent.db` is a symlink to the operator's `~/.omp/agent/agent.db`,
  so the panes use the Anthropic login stored there; no key file, gateway or `hawk-token` is
  involved, and nothing reads, prints or copies the credential
  ([`lib/install-box-login-route.sh`](#libinstall-box-login-routesh)). The panes run the
  operator's own Oh My Pi build, not `.omp-pin`: the operator's global mise pin for
  `github:sjawhar/oh-my-pi`, refused unless it carries the refresh-race fix (the build the
  operator's own sessions already run against that database); the plugin installs, the plugin
  gate's `omp plugin` calls and every pane run that one build, and the run is refused while
  `LEGION_OMP_PATH` is set, since the daemon would prefer that binary. One box-login proof runs at
  a time on the box, Stage 2 or Stage 3: the script takes the lock ([`lib/box-login.sh`](#libbox-loginsh))
  first, before it makes its work directory or starts anything. Its teardown gives each pane
  SIGTERM and up to 10 s before any SIGKILL, then removes the profile's `agent.db` link (the link
  alone) on every exit, so a kept workspace holds no live path to the operator's login.

The `secrets` CLI named here and below is secretsd, shipped from `sjawhar/forward` (the old
`sjawhar/secretsd` repository is archived): it reads its agent tier from local sops files and its
human tier through the local secretsd daemon, which asks for a YubiKey touch, and it never reads
the agent-secrets broker. `agent-secrets` is the other CLI, which reads that broker, backed by a
cloud Secret Manager. One key name can hold different values in the two stores - the App keys
these runs use are the `secrets` store's. `agent-secrets` names its store in output and every
refusal; `secrets` names none today.

CI runs the unit and integration tests, not this script; what only this
run proves is OMP's real RPC frames, `--resume`'s same-agent behaviour, the one-word
`--append-system-prompt`, the plugin's strict parse of the Go daemon's answers, the plugin gate
against an installed manifest, the provider-key path, a pane's model turn through the configured
route, and the Envoy role claim.

What it stands up, all of it the run's own:

- **Postgres**: `LEGION_E2E_PG_DSN` when set; otherwise a `postgres:16` container on tmpfs
  (`legion-e2e2-pg-<pid>`) on an ephemeral loopback port — tmpfs rather than the image's anonymous
  volume, which a box whose docker volume subsystem stalls would hang on.
- **NATS and the Envoy listener** on the host: a `nats:2.10 -js` container
  (`legion-e2e2-nats-<pid>`), and `packages/envoy`'s `cmd/listener` built into the work directory
  and started with a fresh API bearer, which reaches every pane as the 0600 file `envoy_token_file`
  names.
- **The plugins**: this checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`, each packed as
  the release packs it, installed into the OMP profile `legion-e2e2-<pid>-<epoch>` by
  `lib/install-plugin-profile.sh` (`--package pi-envoy`, then `--package pi-legion`), under the
  run's own Oh My Pi home `<work>/omp-home` ([`lib/omp-home.sh`](#libomp-homesh)). The daemons run
  with that `HOME` and with `OMP_PROFILE` naming the profile, so the gate and every pane load both,
  and the operator's `~/.omp/profiles` holds none of the run; `profile-stays-in-the-run` checks
  that last.
- **OMP**: `omp_invocation: mise x <pin> -- omp`, the pin read from
  `.omp-pin` (under `box-login`, the operator's own build that
  [`lib/install-box-login-route.sh`](#libinstall-box-login-routesh) prints). At boot the daemon asks `mise where <pin>` for the
  configured tool's executable, then runs that absolute binary under `mise x <pin>` for every boot
  probe and pane. The script deliberately keeps the ordinary daemon `PATH`, where this devbox has
  `~/.dotfiles/shims/omp` first, and checks the boot log's resolved binary, the OMP child's
  `/proc/<pid>/exe`, and every command in the pane's first-child chain. That proves the wrapper
  cannot replace the configured build while mise still supplies the tool's activation.
- **The model route**: Anthropic through the configured route, installed into the profile by
  [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (`gateway`) or
  [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (`key-file`) at
  setup, while the script's shell still holds the operator's HOME and XDG directories. Either
  route's preflight refuses loudly before any pane exists: a locked keyring by name under
  `gateway`, an unreadable or empty key file under `key-file`. Every model role of every pane is
  `anthropic/claude-opus-4-8`, and `anthropic` is the one provider a pane may use, keyed through
  `<work>/model-gateway/hawk-token` (`gateway`) or `<work>/model-gateway-key/anthropic-key`
  (`key-file`), whose log records each call; no Anthropic key reaches a pane either way — only the
  key command, and only under `gateway` through the operator's hawk login or under `key-file` by
  reading the file once per call. Under `box-login` there is no key command and no call record:
  the profile's `agent.db` links to the operator's login, and `<work>/model-gateway/box-login.route`
  records the link and the build; `model-turn-through-the-route` still checks every reply came
  from `anthropic`.
- **The provider key**: `provider_keys: {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}`, which proves the
  provider-key path; no pane's model uses it, and the profile disables the Google provider. The
  daemon resolves the secret at boot and writes it as a daemon-held 0600 file; every pane's shim
  exports it to OMP alone. The run never reads the value; it checks the length in OMP's
  environment.
- **The daemon**: `legion.yaml` with a fresh project key per run (`S2E<pid><epoch>` — a retired
  claim is never spawned again, so a reused key would fail on a store that served an earlier run),
  a free port from [`lib/free-port.sh`](#libfree-portsh) (its second daemon and the listener get
  two more, each excluding the ones already picked),
  `probe_interval_seconds: 5`, and the operator token file `legion claims` presents.
  `XDG_STATE_HOME` and `TMUX_TMPDIR` point into the work directory, so the legions registry and
  the private tmux servers are the run's.

The checks, in order, each printing what it observed (`== <check>` … `ok <check>`):

| check | what it does and requires |
| :--- | :--- |
| `gate-refuses-another-contract` | edits the installed (unpacked) `pi-legion` manifest to declare the next `daemonApiVersion`; `legion start` refuses naming both numbers; the manifest is put back byte for byte |
| `gate-refuses-a-disabled-plugin` | `omp plugin disable @sjawhar/pi-legion`; `legion start` refuses with "installed but not loaded by omp"; `omp plugin enable` |
| `gate-refuses-without-the-envoy-plugin` | `omp plugin disable @sjawhar/pi-envoy`; `legion start` refuses with "but no pi-envoy is: install the @sjawhar/pi-envoy release", naming the package to install before any skill it ships is reported missing; `omp plugin enable` |
| `gate-refuses-a-missing-skill` | the installed plugin's `dist/skills/thermonuclear-deep-review` moved aside; `legion start` refuses with "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md, roles/core/reviewer.md)", the agent definition and the reviewer's role prompt that load it; the rubric put back |
| `gate-refuses-a-skill-only-a-role-prompt-loads` | the installed plugin's `dist/skills/legion-controller` moved aside, a skill only `roles/controller-root.md` loads; `legion start` refuses with "finds no skill legion-controller (loaded by roles/controller-root.md)", which only a gate reading the daemon's own role prompts can say; the skill put back |
| `gate-refuses-an-unconfigured-model-role` | `modelRoles.oracle` removed from the isolated profile's `config.yml`; `legion start` refuses with "on its model @oracle: role oracle is not configured", naming `task agent oracle` and the prompts that dispatch it; the profile put back byte for byte |
| `architect-registers-and-is-ready` | `legion start` passes the gate (its log line, `boot gate: pi-legion speaks this daemon's contract and loads with pi-envoy`); `legion claims spawn` of a root architect whose role prompt says to reply `ready` and wait; the claim reaches `ready` and the daemon logged its registration at contract 1 |
| `envoy-role-held` | `GET /v1/roles/<claim token>` on the listener names the claim's session as holder |
| `ready-in-state` | `legion state --json` shows the issue's architect `ready`, with that session and a tmux locator |
| `task-queued-before-ready-runs-once` | a second claim spawned with a task: the spawn's answer holds the delivery unsent while the claim is launching; it ends `idle` with nothing pending, and OMP's session file holds the task once |
| `model-turn-through-the-route` | every assistant turn in that claim's session file ran on the `anthropic` provider as the profile's pinned model (`anthropic/claude-opus-4-8`), none ending in an error, and the key command served more calls than its preflight, which records none |
| `retried-frame-starts-no-second-turn` | stops the claim's shim (SIGSTOP) and delivers a task: every send goes unanswered, and the daemon sends the same delivery id again at the next sweep (`the prompt was lost to the transport` twice in its log); resumed, the shim hands OMP the first frame and answers the repeat from its record — the session file holds the task once, and no prompt failure is charged |
| `kill-pane-resumes-the-same-session` | `tmux kill-pane` on the architect: the next generation is ready in a new pane with the same session, its OMP started with `--resume=<session file>`, and the Envoy role still held |
| `suspend-keeps-the-session` | `legion claims suspend`: `suspended`, no locator, the pane gone, the session and its file kept |
| `resume-on-demand` | `legion claims resume`: `ready` at the next generation in a new pane, same session |
| `stale-generation-hello-refused` | writes a hello carrying generation 1's boot token to `<state_dir>/worker-stream.sock`: the daemon closes it with nothing written and logs `rejected hello (stale worker generation)` |
| `unregistered-agent-retired-at-the-deadline` | a second daemon whose `LEGION_OMP_PATH` stub answers the plugin gate and the boot's `config get retry.modelFallback --json`, and otherwise sleeps, with a 10 s registration deadline (5 s × 2): the claim's shim connects and its process lives, the agent never registers, and at the deadline the process is retired and one launch failure counted |
| `restart-readopts-the-live-panes` | SIGTERM, then start again: `boots` +1, both claims keep their generation, incarnation and pane, no pane opens or closes, and a task delivered after the restart runs once — over the connection the shim's reconnect hello opened |
| `omp-child-environment` | the boot log names the resolved pinned OMP binary for both probes and panes; the pane's process is `/bin/sh -c`; walking first children from it to `argv[0] == omp` finds that exact executable, never the OMP wrapper that remains first on the ordinary daemon PATH; the pane's shell and that OMP carry all four XDG base directories under `<state_dir>/home` and no `DBUS_SESSION_BUS_ADDRESS` (LEGION-206 P1, with the gateway route in place); OMP carries `GEMINI_API_KEY` (length only) that its shim does not, and no `ANTHROPIC_API_KEY`, `GEMINI_API_KEY_TESTS`, `SOPS_AGE_KEY_FILE` or `SECRETSD_CONFIG` |
| `stray-pane-reaped-after-the-grace` | opens a window marked as the daemon's (`@legion_owner`) holding no recorded pane, and an unmarked one beside it: the periodic orphan sweep (every 60 s, 120 s grace) reaps the marked one no sooner than 120 s after it opened, and keeps the unmarked window and both claims' panes |
| `stop` | `legion claims stop` of the root architect is refused — 409, "the tree's root claim ends only when its tree closes; suspend it to stop its process; no workflow issue backs its tree, so legion claims close ends it" — and moves nothing: its state, generation, and pane are what they were; `legion claims suspend` suspends the root; `legion claims stop` of a second worker, S2-3's tester, retires it and its pane is gone; no workflow issue backs S2-1, so `legion claims close` of its root closes the tree while its first worker is still live: the root and the worker are both retired and the worker's pane is gone; `legion stop` ends the daemon with exit 0 |
| `every-turn-through-the-route` | [`lib/check-model-route.sh`](#libcheck-model-routesh) over every session in the isolated profile, each subagent's included: every assistant turn was served by the `anthropic` provider, the route's; and its negative control, a copy of one captured session with a turn rewritten as Bedrock's, is refused |

Every wait is bounded and names what it waited for; a failed assertion prints
`FAIL <check>: <why>` and exits 1, and any other failing command names the check it ended. A failed
run also [notes](#libmodel-gateway-unservedsh) each agent that could have failed that check for
want of a model key, and still exits 1. The
`EXIT` trap — on a pass, a failure, or an interrupt — stops both daemons (SIGKILL after 10 s),
under `box-login` gives every process of the run SIGTERM and up to 10 s and removes the profile's
`agent.db` link, kills both private tmux servers, stops the listener, SIGKILLs any process still naming the work
directory in its command line or working directory, removes both containers and the OMP profile,
and removes the work directory when the run passed (keeping it, with `transcript.log` (the whole
run), `daemon.log`, `deadline.log`, `listener.log`, the model key command's log and each
refusal's log, when it did not). The trap still runs when whoever reads the run's output goes
first, for example a supervised launcher's own `tee` stopped with the run, and a stop follows: the
run's output goes through [`lib/transcript.sh`](#libtranscriptsh)'s `tee`, which outlives the
reader. The transcript is in the work directory, whose processes the teardown kills by path, so
the script opens it on fd 8 and calls `transcript_to /dev/fd/8`.

Three things the run had to learn about its surface:

- The shim is a Go process, which starts its child from whichever thread runs the goroutine: the
  first-child walk reads `/proc/<pid>/task/*/children`, never the main thread's list alone.
- OMP takes the session to resume as `--resume=<file>`, one argument.
- The shipped orphan sweep is the only reaper after boot (boot's own reconcile runs with grace 0),
  so the stray is opened after the restart and the check waits up to five minutes.

## stage3-devbox-workflow.sh

```sh
LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://<host>.<tailnet>.ts.net:4222 \
  bash scripts/e2e/stage3-devbox-workflow.sh     # → "stage 3 e2e: PASS", exit 0
LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> SMOKE_UPSTREAM_NATS=nats://<host>.<tailnet>.ts.net:4222 \
  bash scripts/e2e/stage3-devbox-workflow.sh     # the same, for a devbox with no Hawk gateway
LEGION_E2E_MODEL_ROUTE=box-login SMOKE_UPSTREAM_NATS=nats://<host>.<tailnet>.ts.net:4222 \
  bash scripts/e2e/stage3-devbox-workflow.sh     # the same, on the operator's own Oh My Pi login
```

**Devbox only; CI does not run this script.** It runs real Oh My Pi agents and real GitHub Apps,
and sees one disposable pull request merged into `sjawhar/legion-smoke` by the daemon's own
submission on its accepted READY: the repository allows no auto-merge, so the daemon merges it once
GitHub reports it mergeable, and the proof only reads it (`await_ready_merge`), never merging it by
hand.
The run needs `go`, `docker`, `jq`, `curl`, `ss`, `tmux`, `bun`, `mise`, `gh`, `shellcheck`, the
`secrets` CLI holding the two role App keys (the default pair unless the App inputs name
another deployment's), and
`LEGION_E2E_MODEL_ROUTE` (required, `gateway`, `key-file` or `box-login`), which picks how
the agents reach Anthropic, independently of which devbox this runs on:
- `gateway`: the operator's model gateway access — `LEGION_E2E_MODEL_GATEWAY_URL` naming the
  gateway's Anthropic endpoint (required in this mode;
  [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh)), `hawk-token` on `PATH`, their
  `hawk login`, and the GNOME keyring holding it unlocked (every reboot locks it; the
  `unlock-keyring` skill).
- `key-file`: `LEGION_E2E_ANTHROPIC_KEY_FILE` (required in this mode), a 0600 file holding a
  plaintext Anthropic key, for a devbox with no Hawk gateway at all
  ([`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh)).
- `box-login`: nothing more, for a devbox whose operator lets Legion's test runs use their own
  Oh My Pi login: the profile's `agent.db` is a symlink to the operator's, no key file, gateway or
  `hawk-token` is involved, and nothing reads the credential
  ([`lib/install-box-login-route.sh`](#libinstall-box-login-routesh)). The panes run the
  operator's own Oh My Pi build (its global mise pin, refused unless it carries the refresh-race
  fix) in place of `.omp-pin` for the plugin installs and every pane; the daemon runs without
  `LEGION_OMP_PATH` under every route (it would outrank `omp_invocation`), each daemon start must
  log that build's own `bin/omp` as its resolved binary, and a box-login run with
  `LEGION_OMP_PATH` set is refused. The script takes the one-at-a-time lock
  ([`lib/box-login.sh`](#libbox-loginsh)) first, before it makes its work or evidence directory or
  starts anything, and its teardown (the `EXIT` trap and `services-stopped`) gives every pane
  SIGTERM and up to 10 s before any SIGKILL, after the failure-path production audit, then removes
  the profile's `agent.db` link before transcripts are kept.

`SMOKE_UPSTREAM_NATS` (required) names the production Envoy
NATS the GitHub bridge subscribes on by its fully-qualified name on the operator's tailnet, the
`nats://<host>.<tailnet>.ts.net:4222` URL the operator's `envoy.json` names as `envoy.natsUrls`
(the hub's host changes when the hub moves), never a bare alias, which only a resolver's search
domain completes ([the rig-alias learning](../../docs/solutions/testing/a-rig-container-alias-that-is-momentarily-unheld-resolves-through-the-tailnet-to-production.md));
`prerequisites` refuses a run without it, and refuses a value that is not one NATS URL naming a
host with a dot. `STAGE3_PRODUCTION_ENVOY_URL` (default `http://127.0.0.1:9020`, a standing local
port-forward's own address) names the operator's Envoy listener directly instead
(`production-untouched`'s own documentation below has the check), for a devbox with no such
forward running; the operator's own `envoy.json` names the production Envoy's real address
(`envoy.url`). The script prints
neither value. The bridge connects to that upstream as the nkey user a command run from the
operator's shell resolves for it (`lib/operator-nats-seed.ts`, `envoySettings` with
`ENVOY_NATS_URL` set to that upstream): `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED`, else the
`envoy.natsSeed` of the operator's envoy.json when its `envoy.natsUrls` are exactly that upstream,
and without a credential when none names one; a file's seed written for other servers, or a file
that turns Envoy off, stops the bridge naming it. Every process the rigs start against their own no-auth NATS (the listener, the Envoy Dispatch
server, the Go daemon; stage 2 and the other local rigs unset every variable below outright) runs without
`NATS_NKEY_SEED`/`NATS_NKEY_SEED_FILE`, and the Go daemon also without the daemon seed's
`NATS_DAEMON_NKEY_SEED`/`NATS_DAEMON_NKEY_SEED_FILE`, since the Go client refuses an nkey against a server with no
users; `lib/nats-stream.ts`, which stage 4b runs against production NATS, resolves the operator's seed the same way and reads the one newest message matching each preflight subject within the caller's budget. The proof human is the devbox's ordinary `gh` — the dotfiles shim, acting as the
`sjawhar-agent` App — for its reviews, its reads, and its merge; it is never a Legion App, and the
run needs no personal access token (`GH_PUBLIC_REPO_PAT` cannot read the private smoke repository
anyway). The shim routes `gh` to that App only from the operator's own Oh My Pi session, so the run
starts there: a plain shell's `gh`, a tmux window's included, is the user's own login, a Legion
pane's is one of Legion's Apps, and a personal `GH_TOKEN` in the environment makes any session's
`gh` that token's owner. `prerequisites` refuses to start, before the run's first write to GitHub,
unless `gh` acts as `sjawhar-agent[bot]` ([`require_proof_human`](#libworkflowsh)). The daemon
resolves the two role App keys itself through `private_key_command`, both agent tier,
and `prerequisites` first runs [`read_app_logins`](#libworkflowsh), which runs the same two
commands once, in `lib/app-logins.ts`, to prove each key is its id's App (GitHub's `GET /app` must
answer that id and the role's write permission) and read each App's bot login, the login every
check that names an App compares with, before the run's first write anywhere. The agents'
model is Anthropic through the configured route:
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (`gateway`) or
[`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (`key-file`) or
[`lib/install-box-login-route.sh`](#libinstall-box-login-routesh) (`box-login`) routes the
isolated profile there in `prerequisites`, where the gateway and key-file routes' preflight refuses
loudly before any pane exists (a locked keyring by name under `gateway`, an unreadable or empty key
file under `key-file`), and the box-login route refuses a box with no operator login or an operator
build without the refresh-race fix, and the daemon is given
no `provider_keys` (the Google provider answered long workflow turns with empty responses, so the
agents are not Gemini-backed). No YubiKey touch is required under `key-file` or `box-login`, and no
key value enters the script's shell, a pane, an argv, or the transcript, under any route.

The proof human's `gh` has to stay authenticated for the whole run. A full run takes about 70
minutes, longer than the one hour a GitHub App installation token lives. The devbox shim keeps it
authenticated, because it asks for a current token on every call. A `GH_TOKEN` exported once does
not: it expires partway through, and every later `gh` call, the review-round waits and the
teardown's pull request close included, fails with `HTTP 401: Bad credentials`. A run started
outside the shim exports no `GH_TOKEN` (it outranks any config file) and instead points
`GH_CONFIG_DIR` at a private 0700 directory holding gh's `config.yml` (`version: "1"`) and a 0600
`hosts.yml` (`github.com:` with `oauth_token: <token>`, `user: x-access-token` and
`git_protocol: https`). A background loop rewrites `hosts.yml` from a newly minted
`sjawhar-agent` installation token at least every 4 minutes, since a cached mint can hand out a
token with as little as 5 minutes left. It writes a temporary file in the same directory and
renames it over the old one, so no `gh` reads a half-written file, and it stops when the run
exits. `require_proof_human` still checks that this `gh` acts as `sjawhar-agent[bot]` before the
run's first write.

The script stands up a scratch Postgres, NATS, Envoy listener, native Dispatch server, and a
subscribe-only production-Envoy GitHub bridge in one temporary directory. It installs this
checkout's two plugins in an isolated OMP profile and runs the Go daemon with a separate state
directory, private tmux server, ports from [`lib/free-port.sh`](#libfree-portsh), a root-only
Dispatch project, `admission_cap: 2`, `review_round_cap: 3`, `production_check_hours: 0.25` (the
production check's bound, 14 days by default, brought inside the run: 15 minutes, after the first
look's report, since a look open at the bound defers it by the look's own two-hour bound; the
linger keeps its default 72 h, the tree's close being Stage 4b's proof), and the root design gate
armed. Its
host-side helpers are [`lib/rig.sh`](#librigsh), and the workflow's vocabulary (Dispatch, the
daemon's state, the agents, the proof human, the handoff checks) is
[`lib/workflow.sh`](#libworkflowsh); the script defines only its runtime's reads, its scratch
services, its production guards, and its checks.

Production is off limits, and the proof checks that rather than assuming it. Every registered
pane's OMP environment must carry `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, `ENVOY_URL`, and
`ENVOY_NATS_URL` for this rig's scratch servers, and no `ANTHROPIC_API_KEY`, which would take its
turns off the configured route. A watcher checks each pane the daemon launches —
the ones it starts on its own included — as soon as its OMP process exists, before the plugin
registers and so before any turn; a mismatch kills the private tmux server and aborts naming the
pane, and the audit fails naming any launch the watcher never saw. At
the end, a read-only audit replays production Dispatch events from a baseline read at boot (the
baseline event is the replay's positive control) and fails on any event a rig session authored or
that names the rig's project key, then reads the operator's Envoy listener (`GET /v1/sessions`,
`STAGE3_PRODUCTION_ENVOY_URL`, default `http://127.0.0.1:9020`) and fails on any rig session. The
audit writes nothing. It reads the operator's Dispatch as a command run from the operator's shell
resolves it ([`lib/operator-dispatch.sh`](#liboperator-dispatchsh)), with the `DISPATCH_TOKEN` the
script read at start and took out of its environment, so nothing the run starts inherits it. A file,
URL or token that prevents Dispatch fails the baseline read, naming it; with no Dispatch
configured, there is none to audit. The scratch Dispatch runs with `HOME` and `XDG_CONFIG_HOME` in
the run's work directory, so it reads no envoy.json of the operator's.

It proves admission order and slotless children — the child is parked to `backlog` as soon as it
is seen slotless, which is all it was filed for, so it holds up no acceptance above it — then
drives a root from `todo` through an
architect's spec and gate registration, which the architect does on its own (the proof never
prompts it: its first turn is the daemon's `catch-up` notice, and the root's primary document is
the proof's one-file smoke spec), the human approval, planner, implementer pull request,
tester, reviewer, the architect's acceptance at `accepting`, retro, merger READY, the daemon's
merge of the READY head, which closes the issue (`daemon-merge`), and the implementer's production
check after the close, its first look and the bound's last look
(`production-check-bound`). It also proves three changes-requested rounds, each posted by the reviewer
pane and ended by that reviewer's completion — a review ends when its reviewer completes it, so
the round returns to implementing only once the reviewer's handoff is recorded, authored and
committed by the review App, and each review, like the final approval, names the issue branch's
head at that round's reviewer completion, the head the reviewer's own handoff push made (the order the Go
reviewer prompt gives; the proof names only the decision); in round one the proof human, a GitHub
App and so a bot account like a CI bot and none of Legion's role Apps, also opens a file-level
review thread (the run reads the thread's author back and stops, naming it, when the devbox `gh`
posted as anything else, such as the user after its App routing failed). The implementer answers
that thread in round one's correction and resolves it with its own `gh` (the pull request author's
`resolveReviewThread`), as it does every thread it answers; the round-two reviewer adjudicates it,
replying in its own words and naming its node id to the implementer as accepted (the review App
cannot resolve a thread, so the reviewer names the threads it accepts and the implementer resolves
them with plain `gh`; this one it already had), so the thread then carries the reviewer's reply and
stays resolved. No word of
a reply resolves anything. Each round names one concrete correction the spec permits (a distinct line appended to the smoke file, the one product file the
implementer's pull request changed, which the run records and requires to be exactly one; the
correction counts only in that file's patch on the pull request) and reaches testing only on
that round's own implementer handoff (the daemon's phase record must hold the implementer's
handoff for that round when the issue reaches testing, and the pushed commit carrying the handoff
that every planner, implementer, tester, and reviewer completion reported is authored and
committed by that role's own App, read from the issue's workspace). It also proves `pr-blocked`,
READY refusing after a later spec version until a human approves it
(`ready-gated-on-new-spec-version`, which ends on Dispatch carrying the human's approval forward:
after the gate-state negative control below, the proof inserts a decision block at the end of the
spec through the edits route in a stand-in architect session's name (`dispatch_agent` in
`lib/workflow.sh`), the merger's READY is refused at that version, and the proof human answers
the block, which is the whole change since the version he approved; the issue reaches
`awaiting_merge` on the carried `artifact.approved` at the version after the refused one, the
spec's newest review is pinned to that version in his name with the carry's reason and no ask, no
approval request is opened, and the proof writes no review through the reviews route between the
refusal and the release), a held
worker after its launch budget and the architect's retry relaunching it, a root whose human
decides at the design gate that no change is needed (`architect-closes-a-no-change-root`: the
architect ends its admitted tree with `close_root`, the daemon posts its reason on the issue before
it writes `done`, every status write on the issue is the daemon's, the freed slot goes to the next
waiting root, and the journal has the gate's changes request, the close, the linger, the slot
release and the architect's suspension), restart during
implementation, a pending status write while Dispatch is down, and the Go pane's
credentials (`in-agent-credentials`): in one bash tool call of a real implementer pane, `gh` — the
launcher directory's `legion gh` wrapper, over the daemon's PATH's real gh (`LEGION_GH_PATH`) —
authenticates as the implement App's bot (`role_app implementer`) from the repository owner's
directory of the claim's gh bundle, which the pane's `GH_CONFIG_DIR` names and whose root
(`LEGION_GH_OWNERS_DIR`) holds that one owner's directory alone; the shared clone's
helper is `!legion git-credential` with `credential.useHttpPath`; a `task` subagent
of that pane does the same with no bash command of the parent's first, since it inherits the same
directory, and `gh auth status` names account `x-access-token`; nothing of the pane's redeems a
grant for GitHub, no pane carries a grant file, and no agent runs `legion` from bash, so there is
no `idle-pr-read` checkpoint, which proved a per-read grant mint (LEGION-262) that no longer exists.
`notices-reach-architects-alone` reads every phase-worker session in the isolated profile (a
session's role is its newest Envoy role claim) and fails on any workflow notice delivered to one:
every notice kind is for the architect that owns its issue, on that architect's role topic, and the
run's `pr-blocked`, the reviewer's `phase-finished` at its approval (the architect's acceptance
cue) and `worker-died` are each checked in their architect's session where they are written.
The end of the issue is the new order's (LEGION-781). `final-review-cycle` first sends the
architect a `sign_off` while the issue still sits in `reviewing` (the tester's completion started
the reviewer, which waits for the proof's instruction), the acceptance's negative control: the
daemon refuses it `SIGNOFF_OUTSIDE_ACCEPTING`, read in the architect's session by the refusal's own
sentence, and the phase does not move. The reviewer's approval then moves the issue to `accepting`,
which the checkpoint reads directly: no worker starts (the implementer's claim holds no retro task,
`claim_pending_phase`), the daemon queues no status write and the board keeps `needs_review`, and
the architect holds the reviewer's `phase-finished` notice. `retro-handoff` prompts the architect to
accept: its `sign_off` moves the issue to `retro` (the daemon log's `workflow: phase changed …
from=accepting to=retro trigger=sign_off`, `phase_change_logged`), the board to `retro`, and starts
the implementer's retro. `daemon-merge` sees the daemon's merge close the issue: `awaiting_merge`
and the merged pull request write `done` (`… from=awaiting_merge to=done
trigger=pull_request_merged`, the board `done` within 60 s), the production check opens on the
merge commit at its first look, woken by the merge (`check_opened_on`), and the root's close
lingers the tree: `legion claims list --json` shows the architect, planner, tester, reviewer and
merger `suspended` and the implementer alone live (`claim_states_are`), handed the first look in
its pane. The proof sends the implementer nothing at a look: the issue is closed, and a closed
issue takes no Dispatch write (409 `ISSUE_CLOSED`), so the fixture's instructions say a check task
waits for no message and acts on the daemon's task as written — this proof has no production and
no deploy role, so the implementer leaves `Production: pending deploy of <merge commit>` on the
pull request and reports `pending`: the check stays open with `looks: 1, lookOpen: false`
(`check_look_reported`, within 600 s of the look's start), and the daemon suspends the
implementer's claim, session kept, beside the five. After the smoke main's cleanup,
`production-check-bound` waits for the bound's wake, the last look (`last_look_started`: `looks:
2`, `lastWake.kind: bound`, `last: true`, or the check already closed by an implementer that
reported it first, within 900 s of the checkpoint's start, which is what remains of the 15 minutes
after the first look and the cleanup; a bound deferred by a look still open when it came due fails
here by name), and sees the implementer, whose task names the last look, write that the check did
not run within the bound and report `pending`, and the check close as `not_run` (`check_closed`: the
state omits `check`, the issue `done`; `check_look_logged` and `check_closed_as` hold the daemon
log to the bound's look 2, `last`, and the `not_run` close) and the implementer's claim `retired`
beside the five `suspended`.
Each wait for a tree's architect to be suspended once its tree lingers (`await_tree_suspended`) is
bounded by `phase_seconds`, the proof's budget for an agent's turn: the daemon holds that
suspension until the architect's turn ends and names it no deadline, and a timeout prints the
architect's claim from `legion claims list` (its state and whether its suspension is held).
After the first look's report the proof human removes
every `.legion/` handoff and `docs/solutions/` learning from the smoke `main` through one merged
fixture pull request, and the run checks that `main` carries none: retro's last commit removes the
run's own `.legion/<issue>/` before `READY`, so a proof merge carries only its retro learnings, and
the cleanup removes those and any `.legion/` a merge from before that rule left, so the next run
starts from a base carrying no other issue's files.
The smoke `main` requires the `gate` check, which has not started when the fixture pull request is
created, and GitHub refuses a merge before it passes, so the cleanup first waits up to 600 s, named
in the transcript, for the pull request's merge state to read `CLEAN`, `UNSTABLE` or `HAS_HOOKS`
(`smoke_pr_mergeable` in `lib/workflow.sh`). A failed read is a poll that has not seen it yet, and
a timeout names the last state read. The fixture pull request's branch is
`proof/clean-main-<project, lowercased>`, and the cleanup fails, naming it, when a branch of that
name is there before it makes its own: an earlier run left that one, and the cleanup neither uses
nor removes it. To go on, delete that branch on `sjawhar/legion-smoke`, which also closes a pull
request open on it, and run again. That checkpoint, `smoke-main-clean`, is the fixture's
teardown, not the workflow under test. It begins only once every checkpoint before it has passed,
the first issue's workflow through its daemon merge, the close it made and the production check's
first look included. A failure there stops
the run like any other: the checkpoints after `smoke-main-clean`, from `production-check-bound`
through `cleanup-is-complete`, do not run, so its result is no report on them. The `EXIT` trap's
teardown follows its `FAIL smoke-main-clean` line as for any failure. The run's last line is then
`stage 3 e2e: FAIL (fixture teardown, check smoke-main-clean): every checkpoint before it passed,
…`, unless a production guard tripped during the checkpoint: the pane watcher recorded an endpoint
violation (`$evidence/pane-endpoint-violation.txt`, which aborts the cleanup's wait), or the
after-failure production audit, the run's first at this point, found rig writes or could not
confirm there were none. With either, no such line is printed and the ordinary failure stands. The
run exits non-zero either way, and the smoke `main` may still carry the leftovers the cleanup was
to remove; the trap closes the fixture pull request if it is still open and deletes its branch
(below).
From the proof's merge (Stage 4b takes it before the merger's READY, since the daemon's merge can
follow READY by seconds) until that cleanup passes, the run holds the smoke `main`: an exclusive
`flock` on `/tmp/legion-e2e-smoke-main.<owner>-<repo>.lock`, shared by every Stage 3 and Stage 4b
run on the box (`hold_smoke_main` in `lib/workflow.sh`). Another run's merge inside that window
adds the same paths and GitHub refuses it as unmergeable. A run that finds the lock
held names the holder, says every minute how long it has waited, and fails after 45 minutes
naming it. The lock is on an open descriptor, so a holder that dies or is killed frees it with no
stale lock left behind.
Each check is named in the transcript;
negative controls demonstrate that the status-actor, held-worker, re-closed-gate
and worker-notice assertions reject deliberately corrupted observations before the captured
observations pass again (the worker notice's: a copy
of one phase-worker session with a `pr-blocked` delivery appended). `ready-gated-on-new-spec-version`
opens with a live gate-state negative control for the carry: the human's whole-document upload
closes the gate, no `artifact.approved` names its version and no review is pinned to it (a
version that is nobody's answer to a decision block carries no approval), and only his own review
reopens it, which releases nothing since no READY is held yet.
Once every agent is gone, `model-turns-through-the-route` runs
[`lib/check-model-route.sh`](#libcheck-model-routesh) over every agent session in the isolated
profile, each subagent's included: it fails on any assistant turn or model selection that is not
the `anthropic` provider's, the route's, and its negative control (a copy of one captured session
with a turn rewritten as Bedrock's) is kept in the evidence as `model-route-control/`. It runs after
`services-stopped` has stopped the watcher, the daemon and the private tmux server and found no
proof process left, since an idle agent takes a turn on the next event the daemon delivers, and
before the transcripts are copied and the profile removed. It then runs over the copied
transcripts too, and fails unless they hold the same turns, sessions and subagents.

`STAGE3_FROM=held` or `STAGE3_FROM=restart` is a development aid for iterating on the later
scenarios against a fresh rig: it skips the first issue's workflow (the proof human closes that
root, which frees its admission slot as the merge's close would), `restart` also skips the held worker,
and the credential and idle-read checks read `STAGE3_PR` (default: the newest smoke pull request).
`STAGE3_UNTIL` is the other development aid. `STAGE3_UNTIL=prerequisites` stops once
`prerequisites` has passed (the tools, the inputs, the proof human, the smoke repository read and
the model route), before the rig starts anything, and removes the scratch work directory.
`STAGE3_UNTIL=rework` drives the first issue through its three review rounds, the per-round handoff
checks, and the final review, to the reviewer's approval and the issue in `accepting`, then skips
every later scenario. Such a run skips the status-actor
check, which needs the first issue's whole history, ends
`stage 3 e2e: development run from <step> finished (not the proof)` (or `until <step>`), and is
never cited as the proof; only a full run is.

Evidence survives every outcome in `STAGE3_EVIDENCE_DIR` (default a fresh
`/tmp/legion-e2e3-evidence.XXXXXXXX`, printed at exit): `transcript.log` (the whole run), every agent
transcript, the daemon, Dispatch, listener, and bridge logs, the model route's call record
(`model-gateway/`: `hawk-token.{log,calls}` under `gateway`, `anthropic-key.{log,calls}` under
`key-file` — never the key command itself, which under `key-file` lives under its own `--key-dir`,
removed on every exit — and `box-login.route` under `box-login`, which names the link, the
database path and the build, never a credential), state captures, negative-control outputs, the pane endpoint checks,
and the production audit. A passing run's last three checks stop every process, kill the private
tmux server and remove both containers (`services-stopped`), check the model route
(`model-turns-through-the-route`), and close every pull request the run still has open on the
smoke repository — its own, by branch, the daemon's `legion/<project>-*`; the proof human's fixture
pull request merged in `smoke-main-clean` — before removing the isolated OMP profile and the scratch
work directory, the agents' workspaces with it (`cleanup-is-complete`); each shows what it removed
gone.
The proof merges only the pull request its human-merge check merges, so the held-worker and outbox
proofs are open when the run reaches here. A close GitHub refuses fails the check with gh's reason;
a close whose branch delete failed is reported as closed with the reason the branch stayed.
On any exit the `EXIT` trap does the same teardown, except that a failure keeps the scratch work
directory and prints its path. For a run that did not pass, the trap also closes the run's own
pull requests, best effort: it prints each close to stderr, and a close GitHub refuses leaves that
pull request open and prints gh's reason, with a line saying some may still be open. When the run
stopped inside `smoke-main-clean`, after the cleanup made its branch and before its merge deleted
it (a merge GitHub answered with a 504, the wait that timed out), the trap also closes the fixture
pull request if it is still open, with a comment naming the run, and deletes the branch
(`close_smoke_cleanup` in `lib/workflow.sh`). It acts only on the branch the run recorded making,
by its exact name, so a run that never reached the cleanup closes nothing of it, and another run's
`proof/clean-main-<project>` is never touched. It prints the URL of each pull request it closed and
of each it could not close, with gh's reason, and names a branch it could not delete. A failed run
also [notes](#libmodel-gateway-unservedsh) each agent that could have failed the check for want of
a model key, and still exits 1. The trap still closes the run's pull requests in two further cases
([`lib/transcript.sh`](#libtranscriptsh)):
- whoever reads the run's output goes first, for example a supervised launcher's own `tee` stopped
  with the run. The run keeps going, and the transcript still gets every line;
- the transcript's disk fills. The run keeps going, and its output still reaches anyone reading.
  GNU `tee`, the devbox's, keeps the transcript only up to the point its disk filled and never
  reopens it, even once space returns; busybox `tee` picks the transcript up again once there is
  room.

## stage3-4b13b-acceptance.sh

```sh
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://<host>.<tailnet>.ts.net:4222 \
ACCEPT_PG_CONTAINER=<postgres container> ACCEPT_PG_PORT=<its host port> ACCEPT_NATS_BIN=<nats-server> \
  bash scripts/e2e/stage3-4b13b-acceptance.sh     # → "acceptance 4b.13b: PASS at <head>", exit 0
```

**Devbox only; CI does not run this script.** The live acceptance of LEGION-208 task 4b.13b. It
stands up the Stage 3 rig from the same `lib/rig.sh` and `lib/workflow.sh`, with the same inputs
(the model gateway, the production NATS, and the optional App inputs of
[`lib/github-apps.sh`](#libgithub-appssh)) and the same proof human as
[`stage3-devbox-workflow.sh`](#stage3-devbox-workflowsh) — the Hawk gateway route only, since this
script has not taken Stage 3's own `LEGION_E2E_MODEL_ROUTE` — so it too runs from the operator's
own Oh My Pi session and refuses to start in `prerequisites` when `gh` acts as anyone but
`sjawhar-agent[bot]`. It creates
no Docker container: the daemon and Dispatch take their own databases in the running Postgres
container `ACCEPT_PG_CONTAINER` names
(user `postgres`, password `ci`), and NATS is the native `ACCEPT_NATS_BIN`. Real agents drive the
task's surfaces through it: a Go prompt part a restart rewrites, the refused root stops, the
phase reminder (a planner whose turn ends with its phase open is reminded, and the tool's
`handoff_complete` closes the phase, after which no reminder comes), park and re-run, the
phase-finished notice's summary and verdict, each root's `accepting` after its reviewer's approval
and the architect's `sign_off` there (`reviewers`), the daemon posting and publishing the merger's READY
(directly, across a refused gate, with and without a merge queue holder, and refused
`READY_PACKET_TOO_LONG` on the merger's own over-cap packet through the tool; the cap's exact
boundary, `record.MessagePostLimit` one unit over, is the daemon's own test in
`internal/api/handoff_test.go`), that the merger's pane carries no grant file and ran no `legion`
from bash (`merger-pane-mints-no-grant`), and the early-merge and closed-unmerged notices, where
the merger's READY on an already-merged pull request, and a merge at `awaiting_merge`, each close
the issue (`done`). Every
proof pull request is retargeted to a scratch base before any merge (the one merged at
`awaiting_merge` to a base of its own, cut from the same main commit), so the smoke main is never
merged into, and both bases are deleted at the end. The evidence is kept in `ACCEPT_EVIDENCE_DIR`
(default the kept scratch work directory's `evidence/`), `transcript.log` (the whole run) among it.
A run that ends on its soft failures gets [notes](#libmodel-gateway-unservedsh) for its first
soft-failing check, from that check's own start. The `EXIT` trap closes every proof pull request
still open and deletes the proof branches and both bases, also when whoever reads the run's output
goes first or the transcript's disk fills, as in [stage 3](#stage3-devbox-workflowsh). The default
evidence directory is under the scratch work directory, whose processes the teardown kills by path,
so the script opens the transcript on fd 8 and calls `transcript_to /dev/fd/8`
([`lib/transcript.sh`](#libtranscriptsh)).

## claude-backend-tmux.sh

```sh
LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic \
  bash scripts/e2e/claude-backend-tmux.sh     # → "claude backend e2e: PASS", exit 0
LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> \
  bash scripts/e2e/claude-backend-tmux.sh     # the same, for a devbox with no Hawk gateway: one key file for both halves
```

The key-file route's file may come from the agent-secrets broker, which serves
`LEGION_ANTHROPIC_API_KEY` to a session without an approval wait. The key is written 0600, never
printed and never in an argv, and the subshell's `EXIT` trap removes the file however the run ends
(a failure, Ctrl-C, or a deadline that kills the shell), where a plain `rm` at the end would leave
it behind:

```sh
(
  key=$(mktemp)   # 0600 already
  trap 'rm -f "$key"' EXIT
  agent-secrets LEGION_ANTHROPIC_API_KEY -- sh -c 'umask 077; printf %s "$LEGION_ANTHROPIC_API_KEY" >"$1"' sh "$key" &&
    LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_CLAUDE_KEY_FILE=$key LEGION_E2E_ANTHROPIC_KEY_FILE=$key \
      bash scripts/e2e/claude-backend-tmux.sh
)
```

Claude Code has no box-login route: Claude Code 2.1.309 writes `~/.claude/.credentials.json` by
writing a temporary file and renaming it over the old one, without following a symlink, so a
symlinked credentials file would be replaced on its first token refresh and the operator's own
file would keep a refresh token that is already spent. This script keeps its key file and gateway.

**Devbox only; CI does not run this script.** It proves `worker_backends` on the tmux runtime: a
real Go daemon whose `legion.yaml` puts the implementer on Claude Code (`worker_backends:
{implementer: claude}`) drives one scratch Dispatch issue, a real Oh My Pi architect and planner
take it to implementing, and the implementer runs as `legion claude-bridge -- claude -p …` in its
pane. The checks:

- `claude-implementer-registers`: the implementer's claim registers its Claude Code transcript,
  `<state>/claude/<claim token>/projects/<cwd>/<session id>.jsonl`, as its session file and the
  session id as its session; its `claude` runs as a child of the run's `legion claude-bridge`,
  started with `--session-id <that id>`. The parent counts as the bridge only when its executable
  (`/proc/<pid>/exe`) is the run's `legion` and its arguments, read whole from the NUL-delimited
  `/proc/<pid>/cmdline` in the shell, are `claude-bridge`, `--` and the run's `claude`, in that
  order, after the executable's name: no other word of the command line counts, the pane's system
  prompt (past 64 KB, with newlines) included, and no pipe can end early on it. A failure prints
  the claude's ancestry.
- `stop-hook-reminds-the-open-phase`: told to answer in one line of prose and call nothing, the
  implementer ends a turn with `implementing` still open, so the pane's `legion` MCP server answers
  Claude Code's `Stop` hook with `{"decision":"block","reason":…}` (`legion_phase_hook`, the hook
  entries of `packages/claude-envoy/legion/hooks/hooks.json`, the endpoint
  `packages/claude-envoy/src/legion-phase-hook.ts`). The transcript holds that reason as one `Stop
  hook feedback` user turn carrying the reminder's own wording — "Your turn ended with your Legion
  phase still open", a fragment of `REMINDER` in `packages/pi-shared/src/phase-stall.ts`, so a later
  rewording of the rest of that paragraph does not break the proof silently — and an assistant turn
  follows that feedback, so the block carried the turn on in the same session rather than ending it.
  The run moves no `LEGION_STALL_*` number: this first reminder of a run is the hook's own answer as
  the turn settles, not a timer's, and the instruction before it resets the run of reminders, so no
  wait here rides on those four delays.
- `claude-implementer-handoff`: told through Dispatch to make the spec's one-file change, write
  and push its handoff, and call the `legion` tool's `handoff_complete`, it does: the daemon records
  the completion's commit, which the implement App authored and committed, which is on
  `sjawhar/legion-smoke` and which changes `.legion/<issue>/`, and the transcript holds the
  `mcp__legion__legion` call with a result that is not an error. No pull request is opened.
- `kill-resumes-the-same-claude-session`: its pane killed, the claim relaunches one generation on,
  registers the same session id and transcript path, runs `claude --resume <that id>`, and the
  transcript's bytes from before the kill are unchanged. The resumed session, asked to quote the
  marker its earlier instruction carried, quotes it, so the earlier turns are in its context and
  not only on disk.
- `every-launch-endpoint-checked`: every pane the daemon launched, Oh My Pi or Claude Code, was
  checked by the watcher before it registered. The watcher reads an agent's environment once,
  NUL-delimited; an environment it cannot read is not checked, never clean, and is recorded with
  why in the evidence's `pane-endpoints-unchecked.txt`, which this check's failure names.

Inputs. The App inputs ([`lib/github-apps.sh`](#libgithub-appssh), optional) name another
deployment's two role Apps; a run here sets none and takes the default pair.
`LEGION_E2E_MODEL_ROUTE` (required, as Stage 2's and Stage 3's is) picks the Oh My Pi agents'
route — the architect's and the planner's:

- `gateway`: the Hawk model gateway, as in
  [`stage3-devbox-workflow.sh`](#stage3-devbox-workflowsh), with `LEGION_E2E_MODEL_GATEWAY_URL`
  (required in this mode) and `hawk-token` on `PATH`
  ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)).
- `key-file`: `LEGION_E2E_ANTHROPIC_KEY_FILE` (required in this mode unless
  `LEGION_E2E_CLAUDE_KEY_FILE` names the file), a 0600 file holding a plaintext Anthropic key, for
  a devbox with no Hawk gateway at all
  ([`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh)). `hawk-token` is
  not required here.

`prerequisites` refuses any other value, naming it, before it writes anything.

`LEGION_E2E_CLAUDE_KEY_FILE` names a file holding the Anthropic API key the
Claude Code implementer reaches its model with. It is required under `gateway`; under `key-file`,
left unset, it falls back to `LEGION_E2E_ANTHROPIC_KEY_FILE`, so one key file serves both halves of
the run, and naming both puts the Claude Code pane on `LEGION_E2E_CLAUDE_KEY_FILE` and the Oh My Pi
agents on `LEGION_E2E_ANTHROPIC_KEY_FILE`, with both paths in the run's notes.
`prerequisites` refuses a run with neither, naming the variables, before it writes anything, and
refuses a key Anthropic's `GET /v1/models` does not accept
(the key goes by a 0600 header file, never an argv). The run copies the key once into its work
directory, 0600, and writes the implementer's claim directory (`<state>/claude/<claim token>`, which
the daemon creates only when missing) before the claim exists: a `settings.json` whose
`apiKeyHelper` is a command that prints that copy, and whose `model` is `claude-sonnet-5`. So no
pane's environment carries `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_OAUTH_TOKEN`,
`ANTHROPIC_FOUNDRY_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`, which the watcher checks for every agent
process, and the key is in no argv and no transcript. `CLAUDE_E2E_EVIDENCE_DIR` names
the evidence directory; by default it is a fresh one under `/tmp`.

The rig is Stage 3's without the GitHub ingress bridge: scratch Postgres, NATS, Envoy listener and
native Dispatch from [`lib/rig.sh`](#librigsh), the workflow's vocabulary from
[`lib/workflow.sh`](#libworkflowsh), `admission_cap: 1`, and the design gate off, so the
architect's gate registration starts planning with no approval. The daemon is started with the
real `gh`, `jj`, `tmux` and `bun` that `mise which` names first on its `PATH`, which every pane
inherits ([`real_bin_path`](#librigsh): the run's own `HOME` hides mise's global config from the
box's shims, where a pane's `jj` shim hands the call back to itself, exit 127), so all four must be
pinned in mise. `LEGION_CLAUDE_PATH` is set to the `claude` on the operator's `PATH`,
`LEGION_BUN_PATH` to that real `bun`, and `LEGION_CLAUDE_PLUGIN_DIR` to this checkout's
`packages/claude-envoy`, whose `dist/envoy-channel.js` and generated `legion/` must be built.
Legion's role Apps resolve as Stage 3's do: the daemon runs `secrets <implement App key> -- …` and
`secrets <review App key> -- …` (the key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` and
`LEGION_E2E_REVIEW_APP_KEY_NAME` give) as each App's `private_key_command`, and `prerequisites`
checks each with `secrets get <KEY> --no-request`, which prints only the key and its tier, refuses
one that does not resolve to the agent tier, and then proves each key is its id's App
([`read_app_logins`](#libworkflowsh)) before the run's first write. The proof human is the devbox's ordinary
`gh` acting as `sjawhar-agent[bot]` ([`require_proof_human`](#libworkflowsh)), so the run starts
from the operator's own Oh My Pi session. The `EXIT` trap stops every process and container,
copies every Oh My Pi session and Claude Code transcript into the evidence directory's
`transcripts/`, closes any pull request of the run's project, and deletes the issue's branch on
the smoke repository.

## claude-backend-sandbox.sh

```sh
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest of the branch's image> \
LEGION_E2E_STORAGE_CLASS=gp2 \
LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> \
LEGION_E2E_CLAUDE_MODEL=<model id> \
[LEGION_E2E_CLAUDE_BASE_URL=<gateway>/anthropic] \
LEGION_E2E_POD_REACHABILITY=aws-private-ip \
LEGION_E2E_MODEL_ROUTE=gateway \
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic \
LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> \
LEGION_E2E_DAEMON_PORT=<port> LEGION_E2E_WORKER_STREAM_PORT=<port> \
LEGION_E2E_DISPATCH_PORT=<port> LEGION_E2E_ENVOY_PORT=<port> LEGION_E2E_NATS_PORT=<port> \
LEGION_E2E_SESSION_DB_PORT=<port> \
  bash scripts/e2e/claude-backend-sandbox.sh     # → "claude backend sandbox e2e: PASS", exit 0

# A cluster whose pods cannot reach the devbox at all (GKE's shape), with no model gateway; the six
# ports are free ports on the devbox, and the runtime kubeconfig is built as below:
LEGION_E2E_OPERATOR_CONTEXT=<operator context> \
LEGION_E2E_RUNTIME_KUBECONFIG=<file> \
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest of the branch's image> \
LEGION_E2E_STORAGE_CLASS=standard-rwo \
LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> \
LEGION_E2E_CLAUDE_MODEL=<model id> \
LEGION_E2E_POD_REACHABILITY=bridge \
LEGION_E2E_MODEL_ROUTE=key-file \
LEGION_E2E_DAEMON_PORT=<port> LEGION_E2E_WORKER_STREAM_PORT=<port> \
LEGION_E2E_DISPATCH_PORT=<port> LEGION_E2E_ENVOY_PORT=<port> LEGION_E2E_NATS_PORT=<port> \
LEGION_E2E_SESSION_DB_PORT=<port> \
  bash scripts/e2e/claude-backend-sandbox.sh     # the same, through the bridge; needs docker
```

**Devbox only; CI does not run this script.** It proves `worker_backends` under the Kubernetes
runtime: the rig of [`claude-backend-tmux.sh`](#claude-backend-tmuxsh) on the cluster of
[`stage4a-sandbox-runtime.sh`](#stage4a-sandbox-runtimesh). A real Go daemon on the devbox, whose
`legion.yaml` carries `worker_backends: {implementer: claude}` and `runtime: kubernetes`, drives one
scratch Dispatch issue; a real Oh My Pi architect and planner take it to implementing in the issue's
pod, and the implementer then runs in that pod's one `roles` container, in namespaces of its own
(`legion launcher role-init`), as `legion claude-bridge -- claude -p
…` — the Claude Code its pod's start script installed at launch from the image's pin (the image
carries the pin, `/opt/legion/claude-code`, and no Claude Code), the image's
`@sjawhar/claude-envoy` as its MCP server and Legion plugin, its `mcp.json` written by the launcher
into the generation's private directory, and
its `CLAUDE_CONFIG_DIR` at `/legion/scratch/state/implementer/claude`, the role's own state home on
the pod's own disk, which dies with the pod (`docs/kubernetes.md`, "The disk dies with its pod").
Every pod of the run keeps its sessions in a scratch session database of the run's own
(`session_store: postgres`): the architect's and planner's Oh My Pi sessions are only there, and the
implementer's launcher saves its transcript there as a row keyed by the file's absolute path and
restores it from there in a replacement pod (`docs/kubernetes.md`, "Where a Claude Code session
lives across pods"). It is the first rig to run `runtime: kubernetes`, so what the tmux proof reads
from `/proc` and a pane's own files this one reads with `kubectl exec` and `kubectl get` against
that pod, and the session table with `psql` in the session database's container. The planner,
told to plan, has 2700 s to reach implementing, Stage 4b's planning budget (its plan checks, gap
analyst and plan-review rounds run on the operator route's own models), and the run prints the time
it took beside that budget. The checks, in order, which is the tmux proof's:

- `claude-implementer-registers`: the implementer's pod launches and its claim registers that pod's
  Claude Code transcript, `<CLAUDE_CONFIG_DIR>/projects/<cwd>/<session id>.jsonl`, as its session
  file and the session id as its session. `CLAUDE_CONFIG_DIR` is read back from the environment of
  the container's Claude Code (`/proc/<pid>/environ`, through `kubectl exec`), not from the pod's
  spec: the runtime sets only the kubelet-resolved variables on the container and carries every
  plain `NAME=value`, this one among them, in the launcher's start command (`launchEnvironment` in
  `packages/daemon/internal/runtime/sandbox/manifest.go`). The transcript is read in the pod and is
  not empty, and the session table already holds its row: the claim registers it only once the
  launcher's first save has committed.
- `claude-pod-runs-the-bridge`: the container's command line holds `claude-bridge --` and
  `--session-id <that id>`; the binary the bridge names, and the process under it, is the Claude
  Code this generation's start script installed,
  `/legion/scratch/state/implementer/legion-claude-code/g<generation>/node_modules/@anthropic-ai/claude-code-linux-x64/claude`
  on the pod's disk (`claudeCodeBinary`, `packages/daemon/internal/runtime/sandbox/plugins.go`),
  never `/usr/local/bin/claude`, which the pod's image does not carry; that binary, run in the pod,
  prints the version the image's pin names; and the `--mcp-config` it names is a file of this
  generation's private directory, which the launcher wrote there.
- `stop-hook-reminds-the-open-phase`: while the implementer still holds its phase — open since the
  daemon's start task, an assignment (`packages/claude-envoy/src/legion-phase-hook.ts`), and not yet
  completed — it is told to answer in one line of prose, to call nothing and to begin no line with
  WAITING, so it ends a turn with that phase open and the pod's `legion` MCP server answers Claude
  Code's `Stop` hook with `{"decision":"block","reason":…}`. The instruction is an Envoy delivery,
  which puts an open phase's run of reminders back at zero but never reopens a closed one (the
  `inbound-event` case of `packages/pi-shared/src/phase-stall.ts`): that is why this check runs
  before `claude-implementer-handoff` closes the phase, never after. The transcript holds the
  block's reason as one `Stop hook feedback` user turn carrying the reminder's own wording, "Your
  turn ended with your Legion phase still open" (a fragment of `REMINDER` in
  `packages/pi-shared/src/phase-stall.ts`, so a later rewording of the rest of that paragraph does
  not break the proof silently), and an assistant turn follows that feedback, so the block carried
  the turn on in the same session. The run moves no `LEGION_STALL_*` number.
- `claude-implementer-handoff`: told through Dispatch to make the spec's one-file change, write and
  push its handoff, and call the `legion` tool's `handoff_complete`, it does: the daemon records the
  completion's commit, which the implement App authored and committed (read with `jj` in the pod's
  own workspace), which is on `sjawhar/legion-smoke` and which changes `.legion/<issue>/`, and the
  transcript holds the `mcp__legion__legion` call with a result that is not an error. No pull
  request is opened.
- `pod-deleted-resumes-the-same-claude-session`: the transcript's size and the `sha256sum` of those
  bytes are read in the pod, and the pod is deleted, its disk and the transcript's file with it.
  Once no pod holds the deleted one's uid, and before the replacement's turn, the session table is
  read from the devbox (`docker exec … psql` in the session database's container): the
  transcript's row has a `byte_len` of at least that size, and the row's content followed by its
  parts in `start_offset` order, as Oh My Pi and `ompsessions.Read` reassemble it, begins with
  those bytes (the same `sha256sum` of its first *n* bytes). The claim relaunches at a later
  generation — its `generation` in `legion claims list`, read just before the deletion, which the
  daemon state's claim view does not carry — in a pod of another uid, registers the same session id
  and the same session file, runs `claude --resume <that id>` — the copy the replacement's own
  start script installed under the new generation's directory, the deleted generation's install
  gone with the deleted pod's disk — and the transcript its launcher restored from the table begins
  with the same bytes (the same `sha256sum`, taken in the new pod). The issue's architect and
  planner, which ran in the same pod, each run a later generation in the replacement pod (its uid)
  and register there: each is an Oh My Pi that loads its session index from the table with the
  Claude Code rows in it. The resumed session, asked to quote the marker its implementation
  instruction carried, quotes it, so the earlier turns are in its context and not only in the table.

The image. `LEGION_E2E_IMAGE` must be the branch's own published worker image, by digest: a
pull-request run of `.github/workflows/worker-image.yaml` publishes
`ghcr.io/sjawhar/legion-worker:sha-<the first 12 hex of the built commit>` and records that tag's
digest, so the image must be built from the branch under test — the one carrying Claude Code's pin
and the `@sjawhar/claude-envoy` plugin in the image — before this proof can run, and a tag is refused.
Iteration may run the same script body against a throwaway kind cluster with Agent Sandbox and
gVisor ([`lib/kind-sandbox.sh`](#libkind-sandboxsh), the pattern of the three kind proofs), which
loads the same image by digest into the node's containerd; the recorded run is the one against the
cluster.

Inputs, every one of them refused by name when unset, before the run reads a tool, a key or the
cluster. `LEGION_E2E_RUNTIME_CONTEXT` (required) and `LEGION_E2E_RUNTIME_KUBECONFIG` (default
`~/.kube/legion-daemon-production`) name the restricted identity the runtime runs as, which the
operator supplies and the run never builds or stands in for (the operator context is never it;
[The runtime identity on GKE](#the-runtime-identity-on-gke) builds one), and
`LEGION_E2E_OPERATOR_CONTEXT` (default `production`) the admin context this script's own reads of
the namespace, the pods and its teardown use. `prerequisites` holds the runtime identity to listing
the namespace's Sandboxes. `LEGION_E2E_IMAGE` (required, by digest) and
`LEGION_E2E_STORAGE_CLASS` (required: the StorageClass of the issue pod's own disk, a generic
ephemeral volume, since the cluster has no default) are the pod's. `LEGION_E2E_CLAUDE_KEY_FILE`
(required) names a file holding the
Anthropic API key the Claude Code implementer reaches its model with; the run copies it once (0600),
holds the endpoint to accepting it (`GET /v1/models`, the key in a 0600 header file and never an
argv), and creates the providers Secret the runtime names for the project with it, which
`provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY}` hands every pod — so the key travels as a
mounted Secret the shim exports into the agent's environment, and never in the pod's spec, which
refuses a credential-shaped name. `LEGION_E2E_CLAUDE_MODEL` (required) and
`LEGION_E2E_CLAUDE_BASE_URL` (optional, Anthropic's own endpoint when unset) are that lane's
non-credential `ANTHROPIC_MODEL` and `ANTHROPIC_BASE_URL` in `runtime.kubernetes.pod.env`, for a
devbox whose route to Anthropic is a gateway. `LEGION_E2E_MODEL_ROUTE` (required, `gateway` or
`key-file`) is the route the pod's *Oh My Pi* roles take, the operator route's own as Stage 4a takes
it: under `gateway`, `{pod,models}.yml` with `LEGION_E2E_MODEL_GATEWAY_URL` and
`LEGION_E2E_MODEL_GATEWAY_AUDIENCE` (both required then); under `key-file`,
`{pod,models}-key-file.yml`, which carry no projected token and read the same Secret's
`ANTHROPIC_API_KEY`. `LEGION_E2E_POD_REACHABILITY` (required, `aws-private-ip` or `bridge`) is how
the pods reach the run's services on the devbox ([Pod reachability](#pod-reachability), below), and
`LEGION_E2E_DAEMON_CONTAINER_MOUNTS` (optional, `bridge` only) the extra read-only mounts of the
daemon's container there.
The App inputs ([`lib/github-apps.sh`](#libgithub-appssh), optional) name another deployment's two
role Apps; a run here sets none and takes this deployment's own. `prerequisites` refuses a
malformed one by name, never printing it, and proves each key is its id's App
([`read_app_logins`](#libworkflowsh)) before the run's first write; the daemon's `github_apps` is
the block proved, and no process the run starts inherits an App key.
`CLAUDE_SANDBOX_E2E_EVIDENCE_DIR` names the evidence directory; by default it
is a fresh one under `/tmp`, kept and printed, holding the transcript of the run, the daemon's log,
the `legion.yaml` and pod fixture the run wrote, the implementer's Claude Code transcript as the
session table holds it, and the namespace snapshots.

The six ports are required in both modes, have no default and must differ:
`LEGION_E2E_DAEMON_PORT`, `LEGION_E2E_WORKER_STREAM_PORT`, `LEGION_E2E_DISPATCH_PORT`,
`LEGION_E2E_ENVOY_PORT`, `LEGION_E2E_NATS_PORT` and `LEGION_E2E_SESSION_DB_PORT`; `prerequisites`
refuses one that is unset, not a number, the same as another, or taken. The rig is the tmux
proof's — scratch Postgres, NATS, Envoy listener and
native Dispatch from [`lib/rig.sh`](#librigsh), the workflow's vocabulary from
[`lib/workflow.sh`](#libworkflowsh), `admission_cap: 1`, the design gate off, and the smoke
repository `sjawhar/legion-smoke`, where the implementer pushes a branch and opens no pull request —
plus the scratch session database, a second Postgres (the pinned image CI tests against, on tmpfs,
`--cpus 1`), whose password reaches docker through an owner-only env file and whose pod-facing URL,
`postgres://legion:<password>@<host>:<port>/sessions`, is written only to an owner-only file the
providers Secret's `SESSION_DSN` key is made from (`session_dsn_secret: SESSION_DSN`), neither
printed. Its services are dialled from inside a pod, so every one of them binds the address
[Pod reachability](#pod-reachability) picks: the devbox's private address under `aws-private-ip`,
on a port the devbox's security group admits from the Legion nodes
([Port admission](#port-admission)), or its loopback under `bridge`, on a port that need only be
free on the devbox. The daemon's and Dispatch's own Postgres is theirs alone and stays
on loopback. Legion's role Apps resolve as the tmux proof's do, through each App's
`private_key_command` and never this shell, and the proof human is the devbox's ordinary `gh`
acting as `sjawhar-agent[bot]` ([`require_proof_human`](#libworkflowsh)), so the run starts from the
operator's own Oh My Pi session.

Everything the run creates in namespace `legion` carries its own `legion.dev/project` label — the
run's Dispatch project lowercased, which is the daemon's own claim token for it — and
[`lib/namespace-rig.sh`](#libnamespace-rigsh) owns it: `prerequisites` refuses to start while the
namespace holds anything under that label, `snapshot` records the run's own objects before it, and
the `EXIT` trap copies the implementer's transcript out of the session table, stops the daemon and
the scratch services, removes the containers, deletes everything under that label, closes any pull
request of the run's project and deletes the issue's branch, and then compares the namespace with
its snapshot.
Under `bridge`, the bridge's own pod, Service and auth Secret carry the same label, and
`bridge_stop` deletes them by their one exact name before that comparison runs. Nothing outside
`legion` is touched.

### Pod reachability

`LEGION_E2E_POD_REACHABILITY` picks how the pods reach the run's six services on the devbox, as
Stage 4a's does ([The bridge](#the-bridge-legion_e2e_pod_reachabilitybridge)):

- **`aws-private-ip`**: the pods reach the devbox at its private address, which `prerequisites`
  reads from instance metadata, refusing the run when it is unreachable or holds no `local-ipv4`.
  The six services bind that address, each on a port the devbox's security group admits from the
  Legion nodes ([Port admission](#port-admission)). This mode needs no bridge, and the daemon runs
  on the devbox as a plain process.
- **`bridge`**: for a cluster whose pods cannot reach the devbox at all (no tailnet, no routable
  private address — GKE's shape). The six services bind the devbox's loopback, each on a port
  that need only be free on the devbox, and `begin bridge`, right after `begin snapshot`, stands up
  [`lib/bridge-tunnel.sh`](#libbridge-tunnelsh) over all six: one pod and one ClusterIP Service of
  the run's own, both named `bridge-` and the run's project label, whose Service carries one port
  per service, named `p<port>`, and whose reverse tunnel forwards each back to `127.0.0.1:<port>`
  on the devbox. The pods dial the Service's cluster-DNS name,
  `bridge-<label>.legion.svc.cluster.local`.

The daemon and the pods read one `legion.yaml`, so under `bridge` its URLs name that Service, which
the devbox cannot resolve. The daemon alone runs in a container on the devbox's own network:
`docker run --network host --add-host <that name>:127.0.0.1`, so the services it dials on loopback
and the API and worker stream it binds are the devbox's, and that name maps to loopback in the
container's `/etc/hosts` alone. Production runs this daemon in a pod, so a static binary in a
container is close to how operators run it; the mode needs docker, which both modes need anyway,
and no root. The container:

- runs the run's own `legion`, built static (`CGO_ENABLED=0`), in
  `mirror.gcr.io/library/busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e`,
  pinned by digest: the daemon needs nothing of an image, and busybox carries the `sh`, `tr` and
  `base64` each App's `private_key_command` runs ([`lib/github-apps.sh`](#libgithub-appssh));
- runs as the devbox user (`--user <uid>:<gid>`), with `--init`, whose reaper at pid 1 hands the
  daemon the TERM docker delivers and reaps whatever a key command or auth plugin leaves orphaned;
- mounts every path at its own path, so every path `legion.yaml` names works unchanged: the run's
  scratch directory read-write (the binary, `legion.yaml`, the state directory, the token files);
  read-only, the runtime kubeconfig, the auth plugin its context's user execs (if it names one), the
  App key runner (`secrets` unless `LEGION_E2E_APP_KEY_RUNNER` names another), each resolved on the
  devbox as the daemon resolves it, the devbox's `/etc/ssl/certs`, the file
  `GOOGLE_APPLICATION_CREDENTIALS` names (if it names one), each existing absolute path a passed
  variable below names (the key runner's key directory, a gcloud configuration directory), and each
  path of
  `LEGION_E2E_DAEMON_CONTAINER_MOUNTS` (optional), a colon-separated list of absolute host paths for
  what the auth plugin or the key runner reads beyond its own binary (a gcloud configuration
  directory, say), refused by name with the other inputs when an entry is not an existing absolute
  path or holds a comma;
- has `HOME` a directory of the scratch directory's (0700) and `PATH` the image's `/bin` followed by
  the two binaries' directories, and takes from the caller's environment only the variables whose
  names start with `AGENT_SECRETS_` or `CLOUDSDK_`, `GOOGLE_APPLICATION_CREDENTIALS` and
  `KUBECACHEDIR`, each by name (`-e NAME`, never a value on an argv); nothing else of the caller's
  enters it, so neither the App keys nor the NATS seeds nor `ANTHROPIC_API_KEY` can.

The docker client runs attached in the background, its output appended to the daemon's log, and is
the pid the run stops: its default `--sig-proxy` hands the container the TERM, and the `EXIT` trap
then removes the container by name (`legion-e2eccs-daemon-<pid>`, which `refuse_leftovers` reads
like the run's other containers) before the bridge and the teardown. The bridge's watchdog stops
the daemon with `docker kill --signal TERM` on that name.

`prerequisites` proves the container before the run creates anything: `getent hosts <that name>`
must resolve nothing on the devbox; the auth plugin and the key runner must each resolve to a
regular executable; and a probe container run with exactly the daemon's `docker run` arguments,
which one helper builds for both, must print the devbox user's own uid and gid, an `/etc/hosts` line
mapping the name to `127.0.0.1`, and the key runner and the auth plugin, looked up by name, at the
paths the devbox resolves them to, each of them also started once with `--help` inside it and refused
when the container's shell cannot execute it (126 or 127): busybox holds no dynamic loader and no
interpreter, so a dynamically linked binary or a script, such as a version manager's shim that
`command -v` finds first, cannot run there. Name a static runner by its path in
`LEGION_E2E_APP_KEY_RUNNER` in that case (`LEGION_E2E_APP_KEY_RUNNER=$(mise which agent-secrets)`,
say). Each observed value is printed. `getent` is required beside the
tools both modes need (`sha256sum` among them, which holds the session table's copy of the
transcript to the pod's).

### The runtime identity on GKE

The runtime runs as the Legion daemon's restricted identity, never the operator's, and that
identity stays the operator's input: on GKE it is the ServiceAccount `legion-daemon` in namespace
`legion`, which the run reads from a kubeconfig the operator builds outside the repository, owner
only (0600). Its cluster server and certificate authority are the operator context's, and its user
is a token for `legion-daemon` that outlives the run:

```sh
kubeconfig=<file>     # owner-only, outside the repository; not ~/.kube/legion-daemon-production
operator='<operator context>'
(umask 077 && : >"$kubeconfig")
server=$(kubectl config view --raw --minify --context "$operator" -o jsonpath='{.clusters[0].cluster.server}')
ca=$(kubectl config view --raw --minify --context "$operator" -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
kubectl config set-cluster legion-runtime --kubeconfig "$kubeconfig" --server "$server"
kubectl config set clusters.legion-runtime.certificate-authority-data "$ca" --kubeconfig "$kubeconfig"
token=$(kubectl --context "$operator" -n legion create token legion-daemon --duration=<longer than the run, e.g. 8h>)
kubectl config set-credentials legion-daemon --kubeconfig "$kubeconfig" --token "$token"
unset token
kubectl config set-context '<restricted context>' --kubeconfig "$kubeconfig" \
  --cluster legion-runtime --user legion-daemon --namespace legion
chmod 0600 "$kubeconfig"
kubectl --kubeconfig "$kubeconfig" --context '<restricted context>' -n legion auth can-i --list
```

The last line is the identity check: its list holds `sandboxes.agents.x-k8s.io` with `create`,
`get`, `list`, `watch`, `patch` and `delete`, and none of the operator context's own rights. Name
the file as `LEGION_E2E_RUNTIME_KUBECONFIG` and the context as `LEGION_E2E_RUNTIME_CONTEXT`, and
name the cluster's admin context as `LEGION_E2E_OPERATOR_CONTEXT` (default `production`) when it
has another name. A token that expires mid-run takes the runtime's access with it, so its duration
must outlast the whole run.

## Port admission

The devbox's security group admits TCP 13370-13375 from the Legion nodes, six ports, and nothing
wider: widening it is a change to the deployment repository's infrastructure, not either stage
script. The Legion daemon that once ran on the devbox and held 13370/13371 there has moved into
the cluster; nothing on the devbox holds them today. That makes all six ports available to a
lane's own identity, not five beside a fixed occupant — confirm with `ss` before relying on it,
since the harness's own `ports_free` and taken-port checks are the authority at run time and this
note only the architecture behind them.

The admitted range matters under `LEGION_E2E_POD_REACHABILITY=aws-private-ip` alone, where pods
dial the devbox's private address. Under `bridge` (Stage 4a's and the Claude Code sandbox proof's)
the pods dial the run's own bridge Service and never the devbox, so no security-group admission is
needed: each port need only be free on the devbox, and a bridge run takes none of the six.

Each lane needs the ports it binds itself, and the scripts need different sets, which is where
"two or three" confusion comes from:
- **Stage 4b**: three, `LEGION_E2E_DAEMON_PORT` (default `13372`), `LEGION_E2E_WORKER_STREAM_PORT`
  (default `13373`) and `LEGION_E2E_SESSION_DB_PORT` (required, no default), the port of the
  run's own scratch Postgres, which every pod of the run keeps its Oh My Pi session in: the
  daemon requires a store that keeps sessions in a database, since an issue pod's disk dies with
  the pod and a session kept on it would die too, and the run's is Postgres.
- **Stage 4a**: two, `LEGION_E2E_WORKER_STREAM_PORT` (default `13373`, the same name and default
  Stage 4b reads) and `LEGION_E2E_SESSION_DB_PORT` (required, no default: the run's own scratch
  Postgres, which every check's pods read).
- **[`claude-backend-sandbox.sh`](#claude-backend-sandboxsh)**: six, `LEGION_E2E_DAEMON_PORT`,
  `LEGION_E2E_WORKER_STREAM_PORT`, `LEGION_E2E_DISPATCH_PORT`, `LEGION_E2E_ENVOY_PORT`,
  `LEGION_E2E_NATS_PORT` and `LEGION_E2E_SESSION_DB_PORT`, all required with no default, all
  distinct: its daemon's API and worker stream, and the scratch Dispatch, Envoy listener, NATS and
  session database its pods dial, which are the devbox's own. Under `aws-private-ip` it takes all
  six admitted ports, leaving none for another lane, so it runs alone; under `bridge` the six need
  only be free on the devbox.

Both stage scripts default `LEGION_E2E_WORKER_STREAM_PORT` to `13373`, so two lanes at their
defaults — two Stage 4b's, two Stage 4a's, or one of each — collide there; set at least one
override to run two lanes together, and give each lane its own session-database port, which
neither defaults. With six admitted ports, a Stage 4a lane takes two and a Stage 4b lane three, so
three Stage 4a lanes or two Stage 4b lanes fit at once; more needs the admitted range widened. A
Claude Code sandbox run under `aws-private-ip` takes all six and names every one of them itself, so
it runs while no stage lane does.

## stage4a-sandbox-runtime.sh

```sh
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> \
LEGION_E2E_RUNTIME_IDENTITY_KIND=aws-iam-role \
LEGION_E2E_PLATFORM=eks \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
LEGION_E2E_POD_REACHABILITY=aws-private-ip \
LEGION_E2E_MODEL_ROUTE=gateway \
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic \
LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> \
LEGION_E2E_STORAGE_CLASS=gp2 \
LEGION_E2E_SESSION_DB_PORT=<port> \
LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> \
LEGION_E2E_PREVIOUS_IMAGE=<worker image of main at contract 37 or 38, before one container@sha256> \
  bash scripts/e2e/stage4a-sandbox-runtime.sh     # → "stage 4a e2e: PASS", exit 0

# A cluster whose pods cannot reach the devbox at all (GKE's shape), with no model gateway:
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> \
LEGION_E2E_RUNTIME_IDENTITY_KIND=kubernetes-service-account \
LEGION_E2E_PLATFORM=gke-autopilot \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
LEGION_E2E_POD_REACHABILITY=bridge \
LEGION_E2E_MODEL_ROUTE=key-file \
LEGION_E2E_STORAGE_CLASS=standard-rwo \
LEGION_E2E_SESSION_DB_PORT=<port> \
LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> \
LEGION_E2E_PREVIOUS_IMAGE=<worker image of main at contract 37 or 38, before one container@sha256> \
  bash scripts/e2e/stage4a-sandbox-runtime.sh     # → "stage 4a e2e: PASS", exit 0
```

**Devbox only, against a real cluster; CI compiles the harness (`go vet -tags e2e ./...`
in the Tests workflow's `typecheck` job) and does not run it.** Stage 4a's gate: `internal/runtime/sandbox` drives
Agent Sandbox pods in namespace `legion` from the devbox, the way the 4b daemon will. Four
independent inputs — never auto-detected from one another — pick the cluster's shape:
`LEGION_E2E_RUNTIME_IDENTITY_KIND` (`aws-iam-role` or `kubernetes-service-account`, how the
runtime authenticates), `LEGION_E2E_PLATFORM` (`eks` or `gke-autopilot`, which platform-specific
Go-harness relaxations apply — the gVisor cpu floor, the ephemeral-storage clamp, the
extended-duration affinity, the cgroup cpu quota in place of `nproc`), `LEGION_E2E_POD_REACHABILITY`
(`aws-private-ip`, today's shape, or `bridge`, for a cluster whose pods cannot reach the devbox at
all — GKE's shape; [below](#the-bridge-legion_e2e_pod_reachabilitybridge)) and
`LEGION_E2E_MODEL_ROUTE` (`gateway`, today's shape, or `key-file`, for a cluster with no model
gateway in front of Anthropic). The script needs `go`, `kubectl`, `curl`, `ss`,
`diff`, `docker` (the scratch session database), `jq` and `bun` (the `plugins` step, below),
`aws` only under `LEGION_E2E_RUNTIME_IDENTITY_KIND=aws-iam-role` (the runtime kubeconfig's `aws eks
get-token`), and the `secrets` CLI holding the implement App's key under the name
`LEGION_E2E_IMPLEMENT_APP_KEY_NAME` gives (agent tier: no YubiKey touch), with
`LEGION_E2E_IMPLEMENT_APP_ID` that App's id ([`lib/github-apps.sh`](#libgithub-appssh); both
optional, defaulting to the default pair, and each refused by name when it is set and
malformed). Stage 4a acts as the implement App alone, so it reads no
review App input. `prerequisites` proves the key is that id's App (`check_github_apps implement`:
GitHub's `GET /app` under the key's JWT must answer that id and `contents: write`) before the run writes anything. The
harness runs `secrets <KEY> -- sh -c 'printf %s "$<KEY>"'`: the `secrets` CLI
decrypts the key and puts it in the environment of that one `sh` child, which prints it to a pipe
the harness reads into memory. The harness decodes it there and mints the implement App's
installation token in process. The key is written to no file, appears in no argv, and reaches no
other process; only the installation token enters each claim's Secret.

The `plugins` step packs this checkout's own `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` for
`stale-plugin-install`'s own Resume through `lib/pack-plugin.sh`, as every proof that installs a
branch-built plugin does: it rewrites each package's `omp.extensions` to its built `dist` bundle
exactly as the release workflow does before publishing (`scripts/pi-plugin-prepack.sh` packs no
other manifest), runs `bun pm pack` with the prepack's build report on stderr, restores the
committed manifest on every exit of its own, and prints the tarball's path alone (the checkout
needs `bun install --frozen-lockfile` first, since the bundle resolves `@legion/*` through the
root's `node_modules`). That checkpoint's negative control packs nothing of its own: it names the
published `@sjawhar/pi-legion@9.6.1` by exact version, which the daemon-rendered install script
resolves to `@sjawhar/pi-legion@9.6.1` for `bun add` and bun fetches from the registry inside the
pod.

Every pod carries the operator route's pod fixture for the chosen `LEGION_E2E_MODEL_ROUTE`, read
through the daemon's own loader (`config.ReadPodFile`): ServiceAccount `legion-worker`, and a
ConfigMap holding the route's `models.yml` and `overlay.yml` (every role Legion's prompts reach,
`enabledModels` holding each session to anthropic, and each provider a pod could reach without the
route disabled). Legion holds none of it. Before the harness runs, the script creates the run's own
copy of that ConfigMap as the operator, `legion-operator-route-<project>`, labelled with the run's
project, so another run in the namespace can neither see nor delete this one's route.

- **`gateway`** ([`pod.yml`](../../deploy/kubernetes/operator-route/pod.yml),
  [`models.yml`](../../deploy/kubernetes/operator-route/models.yml)): one projected token for the
  model gateway's audience; `models.yml`'s `baseUrl` gets `LEGION_E2E_MODEL_GATEWAY_URL` in place
  of its `${MODEL_BASE_URL}` placeholder, and `pod.yml`'s token audience gets
  `LEGION_E2E_MODEL_GATEWAY_AUDIENCE` in place of its `${MODEL_TOKEN_AUDIENCE}` placeholder, so
  `operator-token` checks the token against it.
- **`key-file`** ([`pod-key-file.yml`](../../deploy/kubernetes/operator-route/pod-key-file.yml),
  [`models-key-file.yml`](../../deploy/kubernetes/operator-route/models-key-file.yml)): no
  projected token at all (`operator-token`'s ServiceAccount, automount and no-API-token checks
  still run and pass; its one projected-token check is skipped, since there is nothing for it to
  read); `models.yml`'s `baseUrl` is Anthropic's own endpoint directly, keyed by
  `ANTHROPIC_API_KEY` read back from a `provider_keys` entry the shim exported into the agent's
  environment. Neither
  `LEGION_E2E_MODEL_GATEWAY_URL` nor `LEGION_E2E_MODEL_GATEWAY_AUDIENCE` is read in this mode. No
  check anywhere in the harness makes a real model round trip (the pods run a stub agent, decision
  7; Legion's own image probe makes none either), so the script generates `ANTHROPIC_API_KEY` as a
  local, never-printed placeholder — this proves the key-file plumbing (`provider_keys`, the shim's
  export, `models.yml`'s command reading it back), never a working Anthropic credential.

It also creates the run's providers Secret, `legion-<project>-providers`, with one key (`stage4a`,
a random value no model route reads) that the harness's `provider_keys` hands every agent as
`STAGE4A_PROVIDER_KEY`, so every pod and the probe run with the providers Secret mounted, as a
deployment with `provider_keys` does; a second key, `stage4a_sessions`, the scratch session
database's URL, which every runtime of the run projects (`session_dsn_secret`), since the run's
store is `session_store: postgres`, whose key that is (the daemon requires a store that keeps
sessions in a database, and the run's scratch Postgres is one); and, under
`LEGION_E2E_MODEL_ROUTE=key-file`, the placeholder `ANTHROPIC_API_KEY` key above.

The script starts that session database itself: a throwaway Postgres (the pinned image CI tests
against) in
a docker container on the devbox, bound to the devbox's private address under
`LEGION_E2E_POD_REACHABILITY=aws-private-ip` or to loopback behind the bridge under `=bridge`, at
`LEGION_E2E_SESSION_DB_PORT`, which pods reach as they reach the worker stream, and removes it on
every exit. Its password is the run's own, reaches docker through an owner-only env file, and the
URL (with the host every pod dials, not the devbox's own bind address) is written only to an
owner-only file the harness and the providers Secret read; neither is printed. It is what a role
carries across a pod now that a pod's disk dies with it: the harness records each claim's session
there as `legion sessions import` copies one, and every resume of the run names that recorded
path, which the role launcher looks up in the table and never on a disk.

### The bridge (`LEGION_E2E_POD_REACHABILITY=bridge`)

GKE's pods have no route to the devbox at all: no tailnet bridge, no routable private address
(measured live, 2026-10-17: the devbox's tailnet address timed out from a `legion` pod, which
could reach only in-cluster Services). [`lib/bridge-tunnel.sh`](#libbridge-tunnelsh) stands in for
that reachability with a reverse tunnel, entirely of the run's own. It serves two callers: this
stage, over two ports (the worker stream's and the session database's), and
[`claude-backend-sandbox.sh`](#claude-backend-sandboxsh), over six (its daemon's API and worker
stream, the scratch Dispatch, Envoy listener, NATS and session database;
[Pod reachability](#pod-reachability)). Here:

1. **`begin bridge`**, right after `begin snapshot` (so a leaked bridge object shows up in
   `namespace-clean`'s before/after diff even if its own teardown fails) and before
   `begin session-db`: the operator context creates one pod and one ClusterIP Service, both named
   `bridge-<project>` and labelled `legion.dev/project=<project>`, running
   `mirror.gcr.io/jpillora/chisel` (pinned by digest) as a reverse-tunnel server — non-root,
   read-only root filesystem, every capability dropped, tolerating the `legion` pool's taint
   without requiring it, and carrying the same eviction-protection annotation pair every Legion
   issue pod does (`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`,
   `karpenter.sh/do-not-disrupt: "true"`) — without them the bridge pod, alone on its own node, is
   fair game for GKE's node consolidation mid-run. Only the chisel server's *data* ports, the
   ones the caller names, are on the Service, each a Service port named `p<port>` (here the worker
   stream's and the session database's); its control port is never exposed by the Service or the
   cluster network, reachable only through `kubectl port-forward` under the operator context.
2. The devbox holds a `kubectl port-forward` to the bridge pod's control port, and a `chisel`
   client — built from pinned source (`go install github.com/jpillora/chisel@v1.11.8`) into the
   run's scratch directory, never installed system-wide — asks the server, over that port-forward,
   to open one reverse listener per named port, each proxying back to 127.0.0.1 on the devbox,
   where the caller's services already bind (here the worker stream listener and the scratch
   Postgres). The harness waits for every listener's `tun: proxy#R:<port>=>` line in the bridge
   pod's log before any pod under test is started.
3. **A dropped `kubectl port-forward` is recovered, not fatal**: the watchdog that polls both
   processes every 5s restarts a dead port-forward itself, on the same local port, so the chisel
   client's own bounded reconnect (`--max-retry-count 30 --max-retry-interval 2s`, finite rather
   than chisel's default of forever) can pick the tunnel back up without reconfiguring anything.
   Only two things still send the script itself `SIGTERM`, failing the run loudly: a port-forward
   that cannot be restarted within 60s of dropping, or the chisel client exiting because its own
   retry budget is exhausted. On either, the watchdog first calls the caller's `kill_harness`,
   which stops what the run drives (here the Go test process; in the Claude Code sandbox proof, its
   daemon).
4. Pods dial `tcp://bridge-<project>.legion.svc.cluster.local:<port>` for the worker stream and the
   same host at the session-database port — the advertised host
   (`LEGION_E2E_STREAM_ADVERTISE_HOST`, internal to the harness) the Go runtime hands every launch,
   distinct from the host its own listener binds (`LEGION_E2E_STREAM_HOST`, loopback in this mode).
5. **Teardown** (`bridge_stop`, the first step of `teardown_sequence`, the one function both a
   passing run's own flow and `cleanup`'s fallback call, so it always runs before `namespace_clean`
   takes its snapshot): stops the watchdog, the chisel client and the port-forward, then deletes
   the bridge's auth Secret, pod and Service by their one exact name — never by broader
   selection, the same discipline [`lib/namespace-rig.sh`](#libnamespace-rigsh) holds every other
   object the run creates to.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_RUNTIME_CONTEXT` | required | the kubeconfig context of the Legion daemon's restricted identity |
| `LEGION_E2E_RUNTIME_KUBECONFIG` | `~/.kube/legion-daemon-production` | the kubeconfig file holding that context, kept apart from the devbox's own |
| `LEGION_E2E_RUNTIME_IDENTITY_KIND` | required | `aws-iam-role` (an assumed IAM role mapped to RBAC group `legion-daemon`, EKS's shape) or `kubernetes-service-account` (a plain `system:serviceaccount:<namespace>:legion-daemon` with no such mapping, GKE's shape); `identity`'s assertions and a `network path` failure's diagnostics branch on it |
| `LEGION_E2E_PLATFORM` | required | `eks` or `gke-autopilot`: the cluster's own platform, independent of identity and pod reachability. Every platform-specific relaxation in the Go harness (the gVisor cpu floor, the ephemeral-storage clamp, the extended-duration-pods affinity, the cgroup cpu quota check in place of `nproc`) keys off this field alone |
| `LEGION_E2E_OPERATOR_CONTEXT` | `production` | the devbox's admin context, for operator steps only |
| `LEGION_E2E_IMAGE` | required | the worker image under test, by digest: a `worker-image.yaml` run on the branch under test |
| `LEGION_E2E_POD_REACHABILITY` | required | `aws-private-ip` (the devbox's private address, directly routable) or `bridge` (a reverse tunnel through a scratch pod+Service of the run's own, for a cluster whose pods cannot reach the devbox at all; [above](#the-bridge-legion_e2e_pod_reachabilitybridge)) |
| `LEGION_E2E_MODEL_ROUTE` | required | `gateway` (a model gateway, today's shape) or `key-file` (no gateway: `ANTHROPIC_API_KEY` reaches the agent as a provider key) |
| `LEGION_E2E_STORAGE_CLASS` | required | the issue volumes' StorageClass — AWS's `gp2`, GKE's `standard-rwo`, or whatever else the cluster under test defaults to; the cluster has no default of its own to fall back on |
| `LEGION_E2E_MODEL_GATEWAY_URL` | required under `LEGION_E2E_MODEL_ROUTE=gateway`, unused under `key-file` | the model gateway's Anthropic endpoint, the `baseUrl` the run's copy of the fixture's `models.yml` names; checked by [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh) |
| `LEGION_E2E_MODEL_GATEWAY_AUDIENCE` | required under `LEGION_E2E_MODEL_ROUTE=gateway`, unused under `key-file` | the audience the model gateway accepts on a worker's projected ServiceAccount token, put in place of the `${MODEL_TOKEN_AUDIENCE}` placeholder in the run's copy of the operator route's `pod.yml`; checked by [`lib/model-gateway-audience.sh`](#libmodel-gateway-audiencesh) |
| `LEGION_E2E_WORKER_STREAM_PORT` | `13373` | the port the harness binds for the worker stream; the same name and default Stage 4b reads for its own worker-stream port, so two runs — two Stage 4a's, or a Stage 4a and a Stage 4b — collide on it by default and need distinct values to run at once |
| `LEGION_E2E_SESSION_DB_PORT` | required | a port the scratch session database listens on, reachable everywhere the worker-stream port is (under `aws-private-ip`, one the devbox's security group admits from the Legion nodes; under `bridge`, forwarded through the same tunnel); refused when it is not a number, is the worker stream's port, or is taken |
| `LEGION_E2E_REPO_SECOND_OWNER` | required | `<owner>/<name>`, a repository under an owner other than `sjawhar` on which both Legion Apps are installed, where `second-owner` provisions an issue; refused when unset, not `<owner>/<name>`, or under `sjawhar`. Nothing is pushed to it |
| `LEGION_E2E_PREVIOUS_IMAGE` | required | a worker image, by digest, built from a commit of `main` at daemon API contract 37 (the pod-owned disk, whose harness records each claim's session for the fixture) or 38 (the blocked issues, the same harness) and before one container (its issue pods run six role containers) that this checkout contains; refused when not pinned by digest, when its `org.opencontainers.image.revision` is a commit `origin/main` or this checkout does not contain, when that commit already runs one container, or when that commit's `DaemonAPIVersion` is neither 37 nor 38. `previous-layout` builds that commit's own harness and runs it on the image |
| `LEGION_E2E_IMPLEMENT_APP_ID` | the default pair's implement App | the App's numeric id, which GitHub's `GET /app` must answer for its key ([`lib/github-apps.sh`](#libgithub-appssh)) |
| `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` | the default pair's implement App key | the agent-tier secret-store key the `secrets` CLI resolves to that App's private key; `prerequisites` proves it is that id's App, and the harness mints the App's installation tokens from it |
| `STAGE4A_FROM` | unset | a development entry point: any check after `identity` except `stale-incarnation`, which rides `kill-role-init`'s relaunch; the harness refuses any other name at `identity`, before it creates anything. `identity` always runs; the checks before the entry point are skipped, and each later check first puts the claims it needs where the full run would have left them, through the same runtime calls. The run ends `stage 4a e2e: every check from <check> passed — a development run, never the proof`, and is never cited as the proof |
| `STAGE4A_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e4a-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `transcript.log` (the whole run), `runtime.log` (the runtime's and the listener's JSON log lines), and the two namespace snapshots |
| `LEGION_E2E_AGENT_SECRETS_URL` | unset (the `secrets-*` checks report `SKIPPED-BLOCKED`) | the agent-secrets broker the run enrolls pods with — the **production** broker (Plan D, the broker design's AWS deployment plan), never a development slot (below) |
| `LEGION_E2E_AGENT_SECRETS_OPERATOR` | unset | the email of the person who approves the run's credential request for `LEGION_E2E_APPROVAL` (the `secrets-approval-ask` check prints `STAGE4A: approve credential request <record> for LEGION_E2E_APPROVAL on the Dispatch credential page as <operator>`). The stage is devbox-attended for that one approval: someone clicks Approve during the run (polled up to 10 minutes; a timeout, denial, or expiry fails that check). The run's own broker login asks no one: it logs in as the daemon does, with a token for `LEGION_E2E_AGENT_SECRETS_ACCOUNT` |
| `LEGION_E2E_AGENT_SECRETS_ACCOUNT` | unset | `<namespace>/<name>` of a ServiceAccount of the proof's own that the production broker's `BROKER_SERVICE_LOGINS` lets log in as `legion-daemon` and that no pod runs as, never the production daemon's: each account holds one launcher credential, which every login moves to its own key, so a run logging in as a running daemon's account would take that daemon's credential, and the two would trade it for the whole run. The script refuses an account any pod in its namespace runs as, naming the pods. It mints an hour-long token for the account with the operator context (`kubectl create token … --audience agent-secrets --duration 1h`; the broker refuses a longer one) into an owner-only file it never prints, and mints it again every half hour while the run lasts; the harness logs in with that token, reading the file afresh at each login, as a daemon off the cluster does; no person approves it. With this or any other `LEGION_E2E_AGENT_SECRETS_*` input unset, the harness starts no login and every `secrets-*` check reports `SKIPPED-BLOCKED`; a refused login blocks them with the broker's answer, never fails the stage |
| `LEGION_E2E_AGENT_SECRETS_AUTO_SHA256` | unset | the `sha256sum` of the dummy value Sami seeded into the production broker's secret store for `LEGION_E2E_AUTO` (`owner=shared`, `tier=agent`: every session and pod gets it automatically) — the harness never sees the value itself, only its hash |
| `LEGION_E2E_AGENT_SECRETS_BIN` | `$work/agent-secrets` (built by the script; not read from the environment) | the checkout's `agent-secrets` CLI (`packages/envoy/cmd/agent-secrets`), run directly from the devbox for the `secrets-old-uid-and-revocation` check's before/after-revocation reads |

**Why the production broker, with dummy secrets, and not a development slot.** A dispatch-project
development slot cannot host this proof, for three reasons Plan D established: its broker verifies
tokens of the *staging* cluster's issuer, and the pods this harness launches carry the production
cluster's; it lives in the staging VPC and cannot reach production Dispatch; and it may not run a
Dispatch of its own (the GitHub App's credentials cannot cross accounts), so nobody can approve the
run's credential request there. Instead the proof runs
against the **production broker** with two throwaway secrets in its namespace, tagged
(`LEGION_E2E_AUTO`: `owner=shared`, `tier=agent`, granted automatically to every pod;
`LEGION_E2E_APPROVAL`: `owner=shared`, `tier=human`, so a request for it is decided by anyone
signed in to Dispatch, the operator included — a pod has no operator, and the broker's policy
names no issue assignee; Sami's values seeded after Plan D's apply) —
"before any real secret moves" is exactly this state, and it is what spec Acceptance 2's "a live
worker pod on a development slot" means here.

Two identities, so the runtime is proven under exactly the RBAC it ships with. The runtime and
the harness's own reads use the restricted one; the admin context only runs what an operator does
beside the daemon — `kubectl exec`, PVC phases, the pod uid cross-checks, a Secret's boot token
(hashed in process, never printed), the namespace list, and, on a network-path failure, the
diagnostic that identity kind calls for: under `aws-iam-role`, the node the pod ran on and its
EC2NodeClass's security groups (Karpenter's CRD, meaningless off AWS); under
`kubernetes-service-account` (GKE), the reverse-tunnel bridge's own processes and pod log instead
— [`lib/bridge-tunnel.sh`](#libbridge-tunnelsh)'s `chisel` client and `kubectl port-forward`,
since there is no node mapping to read. Every evidence line names which one observed it
(`[runtime]`, `[operator]`, or `[harness]` for the listener and its resolver).

The harness hosts the worker stream itself, bound at `LEGION_E2E_STREAM_HOST` (the devbox's
private address from instance metadata under `aws-private-ip`, or loopback under `bridge`) and
advertised to pods at `LEGION_E2E_STREAM_ADVERTISE_HOST` (the same address under
`aws-private-ip`; the bridge's ClusterIP Service DNS name under `bridge`) and port
`LEGION_E2E_WORKER_STREAM_PORT` (default 13373) — within the devbox's admitted range
([Port admission](#port-admission)) under `aws-private-ip`; the bridge's own tunnel in `bridge`
mode, so no devbox port needs admitting from the cluster — and refuses
to start while anything holds it, naming the holder. Its resolver accepts
only each claim's current generation and records every hello with the claim, the generation, and
the hash of the token presented. The pods run a stub agent under the real Go shim: it appends its
pod's uid to a marker file on the pod's own disk and sleeps, so the runtime's whole path runs
with no model and no provider key. That marker proves a role's identity within one pod and
nothing across pods — the disk dies with the pod, so a replacement starts with an empty one — and
continuity across a pod is read from the session table instead. The runtime's settings:
a 5-minute boot timeout, 3 registration intervals, a 15-second termination grace, a 10-second
probe interval, storage class `LEGION_E2E_STORAGE_CLASS` (AWS's `gp2`, GKE's `standard-rwo`), a 20Gi issue volume, the daemon's default reservations with
the issue pod's overridden field by field (`liveIssueOverride`: 2 CPU and 4 GiB, request equal to
limit, and a 10Gi ephemeral-storage limit and request), as a deployment's
`runtime.kubernetes.resources.issue` does, the controller's pod at the defaults, and no scheduling
beyond the Legion pool the runtime selects: no pod asks for or keeps off another pod's node.

The checks, in order, each printing what it observed and then `CHECK <name>: PASS`:

| check | what it does and requires |
| :--- | :--- |
| `identity` | refuses to start unless the runtime context is set and authenticates as someone other than the operator; under `LEGION_E2E_RUNTIME_IDENTITY_KIND=aws-iam-role` a SelfSubjectReview shows the assumed `…legion-daemon` role in group `legion-daemon`, and under `=kubernetes-service-account` the bare `system:serviceaccount:<namespace>:legion-daemon` identity with no such mapping; `list secrets -n legion` is 403; a SelfSubjectRulesReview (`can-i --list`) in every namespace finds no grant beyond the plan's and the repository cache's Service reads (`services` list, watch in `legion`); access reviews, which reach the cluster's webhook authorizer that a rules review cannot enumerate, deny every kind of impersonation, `serviceaccounts/token`, pod create and exec, secret list and create, PVC get, nodes, RBAC create/update/patch/escalate/bind, and Sandboxes outside `legion`, beside three positive controls, one of them `watch services` in `legion` |
| `installed` | `CheckInstalled` with production's `InstallRef` passes under the `resourceNames` grants |
| `boot-refusal-negative` | `CheckInstalled` naming `legion-no-such-controller` refuses, naming that Deployment and the 403 the `resourceNames` grant answers, without blaming the CRD |
| `image-probe` | `ProbeImage` on the image under test, with the daemon's own probe command (`--role-references` with the checkout's role prompts' references, and `--provider-env-dir`, the run having a provider key), its probe pod carrying the operator's pod, passes; its log confirms `daemon-api-version` equal to the daemon's contract, `agent-models=resolved` (every task agent the prompts dispatch resolved its model under the operator's pod) and `capabilities=checked` (every image-site capability of the list every worker is held to present, the table printed before the OK line), and the report carries the `model-fallback=<on|off>` mark the pod read; the probe pod, read while the probe runs, carries one container whose cpu and memory request equals its limit and equals the probe's own reservation (`probeReservation()`, 250m and 1Gi over a 5Gi disk bound and a 1Gi disk request, the one the daemon's `imageProbe` hands it and no issue pod's share), `status.qosClass` `Guaranteed` and no affinity, and the same pod with its cpu limit raised past its request is refused by the reservation rule naming the container (the negative control); and the probe Sandbox is deleted |
| `image-probe-negative` | with `modelRoles.oracle` removed from the run's ConfigMap, `ProbeImage` on the same image is refused naming `task agent oracle` and `role oracle is not configured`; the ConfigMap is restored before the check ends, so every later check boots on it |
| `image-probe-capability-negative` | on a second runtime built from the run's Options with `PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium` added to the operator's pod env (the shared runtime's pod untouched), `ProbeImage` on the same image is refused naming `capability browser is missing`: the probe runs the browser the variable names as Oh My Pi would, so a wrong value in an operator's pod env is caught at boot; the probe Sandbox is deleted |
| `image-probe-missing-volume-negative` | on a second runtime built from the run's Options with a volume `operator-missing`, from ConfigMap `<operator ConfigMap>-missing`, which does not exist, added to the operator's pod volumes and mounted at `/etc/legion-operator-missing` (the shared runtime's pod untouched), `ProbeImage` on the same image is refused naming `cannot mount volume "operator-missing"`, `ConfigMap "<name>"` and the kubelet's `configmap "<name>" not found`, within 45 s of the probe pod's `PodScheduled` transition (two 15 s event rechecks and slack for the poll's tick and the kubelet's first volume sync; the noted durations show how soon after scheduling it came) and with no retry (no `never completed within its retry budget`): a FailedMount for an operator volume whose object is missing is read on the first event read after the kubelet records it, not waited out to the budget; the probe Sandbox is deleted |
| `root-ready` | Spawn of the root: its Sandbox Ready, the returned locator's `podUid`, container (`roles`) and process incarnation, the init log (`pods/log`) carrying `workspace-init: /legion/workspaces/sjawhar/legion-smoke/s4a-1 on legion/S4A-1`, and a hello registered at generation 1 with that generation's token. The root is the run's first issue pod, so the run's repository has no cache before it: its launch makes the cache Sandbox (`legion-<project>-cache-<owner>-<repo>-<hash>`, recorded for the teardown first), and the root's fetch log ends `workspace-init fetch: <n> bytes from the repository cache http://<cache>.<namespace>.svc:8418 in <d>, its history verified in <d>; <m> bytes from GitHub for the delta (<k> refs)`, the address the runtime names, `<m>` under 1 MiB and no clone from GitHub |
| `gvisor` | `uname -r` in the root pod is gVisor's emulated kernel (`…-gvisor`), not the node's, and the pod's `runtimeClassName` is `gvisor` |
| `resources` | the root pod's one `roles` container and its two init containers each reserve the issue pod's one reservation, `runtime.kubernetes.resources.issue` as the rig sets it (`liveIssueOverride`: 2 CPU, 4Gi of memory, and ephemeral-storage 10Gi as both request and limit), cpu and memory with request equal to limit, so the pod is `Guaranteed`; it carries no affinity but GKE Autopilot's own extended-duration one under `LEGION_E2E_PLATFORM=gke-autopilot`; inside the pod `nproc` is max(2, ⌈cpu⌉) and `/proc/meminfo`'s `MemTotal` is within 3 % of the `roles` container's memory limit — on AWS, as gVisor sizes the sandbox from the pod's own cgroup; on GKE, from the pod's cgroup cpu quota, read directly (`/sys/fs/cgroup/cpu/cpu.cfs_quota_us` and `cpu.cfs_period_us`), since `nproc` there reflects gVisor's own host-vCPU presentation instead |
| `operator-token` | the root pod runs as the fixture's ServiceAccount with `automountServiceAccountToken: false` and no API server token, in every mode; under `LEGION_E2E_MODEL_ROUTE=gateway` the fixture's one projected token, at its mount, is also checked — a JWT for the fixture's audience, subject the pod's ServiceAccount, and exactly the fixture's lifetime, read into the harness's memory and only its claims printed. Under `=key-file`, only that one projected-token check is skipped: `pod-key-file.yml` mounts no projected token, there being no gateway to authenticate to, and the check still passes |
| `pod-baseline` | the stub agent the root's shim started (read from `/proc/<pid>/environ`) has `PI_CONFIG_FILES` = the pod baseline's turn-scoping overlay on the state volume, then the operator's; `PI_CONFIG_DIR=.omp`, `OMP_SESSION_STORAGE=sql` with `OMP_SESSION_SQL_DSN_FILE` naming the session database's URL file (no pod keeps a session on its disk, which dies with it), `XDG_STATE_HOME=/legion/scratch/state/architect` (the root claim's role: the state home is the one directory of the issue's scratch a role has to itself, so Oh My Pi's browser broker lock, an abstract unix socket named from a path under the state root, is one name per container of the pod rather than one across it), `TMPDIR=/legion/scratch/tmp` (the issue's own temporary directory, the one every role of the pod is told: the runtime names it, so an agent falling back to the image's `/tmp` is the regression this value catches), and the operator's other variables; neither `OTEL_SDK_DISABLED` nor `PI_AUTO_QA` (no baseline sets them); `/legion/scratch/state/architect/omp/profiles/legion` is a directory in the root's container (the shim makes it before Oh My Pi starts; Oh My Pi reads the state home only where it already exists); `/legion/scratch/tmp`, the agent's `TMPDIR`, is a directory in the root's container too, made by the shim before Oh My Pi started, since nothing else creates the temporary directory a role is told; the shim itself (the agent's parent `legion worker-shim`; the container's PID 1 is its `legion launcher`) has the operator's `PI_CONFIG_FILES` alone; the overlay is mode `444`; and the image's `omp config get`, run under the agent's environment in a repository whose `.omp/config.yml` sets both, reads `compaction.remoteEndpoint` as the repository set it and `bash.autoBackground.enabled` as `false`, and with an operator overlay setting the endpoint named after the agent's overlays reads the operator's endpoint |
| `setup-ran` | the shared runtime's `runtime.kubernetes.setup` runs before every successful role stub agent: each record in the issue marker precedes that role's own `<role>:<pod uid>/<generation>` record; it ran in the agent's resolved working directory, as the pod's configured uid, and its parent pid became the agent through the wrapper's later `exec`. Every git the setup runs starts under `env -i` with an allowlist alone: the role's `PATH` and `LEGION_GH_OWNERS_DIR` (the one variable `legion git-credential` reads), its `GIT_CONFIG_COUNT/KEY/VALUE` entries, an empty temporary `HOME`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0` and `LC_ALL=C`, so no `GIT_CONFIG`, `GIT_DIR`, askpass, token, locale or other variable of the role's environment reaches it. `git ls-remote https://github.com/sjawhar/legion-smoke HEAD` succeeds from a fresh empty directory (`mktemp -d`, recorded as `git_from` and removed when the setup exits) outside the issue clone, with `GIT_CEILING_DIRECTORIES` at that directory's parent, so a `.git` above it (an operator's volume at `/tmp`, or one an earlier generation's agent wrote) cannot rewrite or authenticate either call. The same ls-remote with every command-scope helper value emptied, the negative control, must be refused for want of a credential: git's own `fatal: could not read Username for '<url>': terminal prompts disabled` (or `Password`, when it already has a username), as the whole of its stderr, since a remote's error message reaches stderr after git's own `fatal: remote error:` line and could otherwise supply that text. Since legion-smoke is private, that refusal is what proves the success came from the role's helper. The record names the control `ok`, `refused` or `failed`, and for `failed` its exit status and first stderr line with control characters removed and any GitHub token, JWT or URL user info replaced by `***`. The check refuses a `git_from` that is not absolute or lies in the setup's directory, and one still present in the pod after the setup exited. A control that succeeded fails the check as a repository that needs no helper, and one that failed any other way (no DNS, a GitHub error, its 60-second timeout) fails it as `the negative control failed for another reason: <that line>`, so an outage reads as an outage. From the agent's directory, the issue clone, the setup reads every `credential.*.helper` key with `git config -z`, so a newline in a value cannot forge a record, in git's read order, as git runs them for one github.com request. An empty `credential.helper` or `credential.https://github.com.helper` resets the list; any other helper key, which git applies to github.com under several URL forms, counts as an entry. It classifies the list as `expected` when it is exactly one command-scope `!legion git-credential`, `other` otherwise, and records only the classification, the nonempty-helper count, the list's length after the last reset and the record count, never helper text or a token. Stage 4a builds `sandbox.Options` directly, so the `runtime.kubernetes.setup` configuration-to-Options boundary is held by `TestKubernetesSetupReachesTheSandboxRuntime` |
| `setup-negative` | a second runtime built from the run's Options but with setup `[/bin/sh, -c, exit 7]` launches throwaway issue `S4A-8`. Its shim hello registers before it spawns any child; then the role is observed `Gone` with exit code 7, the agent's stub marker is absent, and the `roles` container's log carries, behind the role's prefix, `legion setup: runtime.kubernetes.setup exited 7; not starting Oh My Pi`. The check deletes the throwaway Sandbox, and its pod's own disk goes with its pod |
| `provider-key` | the stub agent's environment (`/proc/<pid>/environ`) carries `STAGE4A_PROVIDER_KEY` equal to the run's providers Secret's `stage4a` key, read back through the admin context and never printed; the shim's (the agent's parent) carries no such variable |
| `adopt-working-copy` | `AdoptWorkingCopy` with the implement App's bot identity; `jj log -r @ -T author` in `$LEGION_WORKSPACE` shows it |
| `worker-shares-issue-pod` | a second role of the issue starts in the root issue's existing pod: the same `podUid`, the locator's container `roles` for both roles, and the root's init-container identities unchanged (no init container runs again) |
| `secrets-two-roles-enrolled` | the root and worker share one actual pod UID but each hello enrolls under its own role-generation slot; each role has a private 0600 `key.pem` and enrollment id and the two ids differ, while one projected `agent-secrets` audience token remains shared by the pod |
| `secrets-automatic-grant` | the root's `agent-secrets LEGION_E2E_AUTO -- …` runs with the automatic secret's value (proven by its sha256 against the operator's `LEGION_E2E_AGENT_SECRETS_AUTO_SHA256`, never the value itself); the root's and the worker's separate `request`s each grant, with two distinct grant ids |
| `secrets-cross-pod-negative` | the worker cannot touch the root's request or grant — reading the request (GET /v1/requests/{id}) or revoking the grant (POST /v1/grants/{id}/revoke) is 403 `NOT_YOURS`, and the root's grant still works after the attempt — nor use its own key beside the root's enrollment id (401 `PROOF_INVALID`: the proof's thumbprint is not the enrollment's) |
| `secrets-copied-token-negative` | the root pod's projected token presented for another pod UID is refused 403 `POD_IDENTITY_MISMATCH`; sharing that token inside its one issue pod does not prove a different pod |
| `secrets-self-enroll-negative` | from inside the root's pod, an enrollment attempt without the daemon's launcher proof is refused and the pod's enrollment (`agent-secrets self`) is unchanged after |
| `secrets-approval-ask` | a request for the approval-gated secret comes back pending with a credential-request record id (ruling 16: the secret's owner and tier pick the approver at request time, naming no issue); the harness prints `STAGE4A: approve credential request <record id> for LEGION_E2E_APPROVAL on the Dispatch credential page as <operator>` and polls for up to 10 minutes until the operator's real approval settles it granted (success), denied, or expired (both failures) — always attended, with no cancel-and-cleanup path |
| `suspend` | Suspend of the worker: when it returns the runtime's watch no longer holds that claim and its old locator probes `Gone`; the shared Sandbox stays `Running`, the pod UID and root role stay alive, nothing answers to the Sandbox-owned claim name an issue's volume had before this release (`issue-<sandbox>`), and the pod's own disk (`<pod>-issue`) is `Bound` and owned by that pod — a role's stop takes neither the pod nor its disk |
| `role-isolation` | a second role starts as a process of its own namespaces in the same `roles` container. Entered as its agent sees the pod (`nsenter -t <its role-init> -U -m -p -i`, then the shim's `-U`), it reads its own gh bundle at `/var/run/legion/gh`, finds `/var/run/legion/roles` empty (no sibling's gh, private or agent-secrets directory, and no launcher token, its own neither), and its `/proc` lists only its own role-init (PID 1), shim and agent, not the root's; it stops while the root stays Alive. That a role cannot signal a sibling is proven by `internal/launcher`'s role-init tests (`TestRoleInit*`, "signal-outside"), not live |
| `resume` | Resume of the first worker: a new process incarnation and hello generation in the unchanged shared pod; the marker holds exactly the old and new process incarnations |
| `stale-plugin-install` | a Resume the harness marks as installing a stale-plugin pair (LEGION-164), naming the checkout's own packed `@sjawhar/pi-legion` and `@sjawhar/pi-envoy` tarballs `kubectl cp`'d into the worker's pod: the pod's uid and creation time are unchanged and its Sandbox is `Running`, the installed pair under `legion-plugins/g<generation>` matches the packed versions and the daemon's contract, and the shim's own `/proc/<pid>/cmdline` (the one process of the launch chain the agent's own execs never touch) carries `--extension` for both installed roots in place of the image's. Negative control: naming the published `@sjawhar/pi-legion@9.6.1` by exact version — no tarball of its own, installed from the registry inside the pod, so this proves the version→package mapping (LEGION-164: a bare version reaches `bun add` as `@sjawhar/pi-legion@9.6.1`, never a bare dist-tag) end to end too — a second Resume installs it (contract 23) but the wrapper's own check ends the child before `exec`, the runtime observes it `Gone`, the pod is still the same uid, and the first generation's own install directory is gone, replaced by the second's |
| `same-agent-negative` | a Resume naming a session the table lacks is refused by the role launcher before any child runs (`resume session <path>: the session table holds no such session`), observed as gone; the shared pod remains available, then a correct resume writes the second marker and never the refused process. A file on the pod's disk decides nothing either way: the launcher never looks there |
| `kill-role-init` | the operator SIGKILLs the worker's role-init from the `roles` container (the container's PID 1, the launcher, serves every role, so it is never killed): the runtime reports `Gone` from the launcher's report of that generation's exit (killed by a signal), no process of the worker's PID namespace is left, the root stays Alive in the same pod, and `Resume(prev=dead)` starts a new generation in that pod with the Sandbox generation unchanged; the pod's own disk is the same claim at the same uid — only one role's processes died, so neither the pod nor its disk went |
| `stale-incarnation` | across that relaunch, every observation carrying the new process is alive or uncertain, the gone carried the old process, and `Suspend(old)` leaves the Sandbox `Running` on the same pod |
| `secrets-old-uid-and-revocation` | a separate issue pod is deleted and resumed to a new pod UID. Its copied old key fails after revocation, and a replacement's identity under the old UID is refused 403 `POD_IDENTITY_MISMATCH` |
| `respawn-before-register` | a role spawned on a token the resolver withholds, stopped before any hello, starts again in its existing issue pod at generation 2 and registers only that second token |
| `independent-provision` | a new tree's root and its child spawned at once each get an issue pod of their own: each init log names its own workspace and the branch it started from (`workspace-init: <dir> on legion/<issue> from <default branch>`), each Sandbox owns no volume claim template at all, each pod's own disk is `<pod>-issue`, Bound and owned by that pod, and the two disks' uids differ, each pod's `/legion/repos/github.com/<owner>` holds the clone alone with no lock beside it, each clone's `jj workspace list` names its own issue's workspace and not the other's, the workspace's `@-` is the repository's default-branch head as GitHub names it (`default_branch`, which the harness reads once with the implement App's token), never the literal `main` (`workspace @- <sha> = default_branch <name> @ <sha>`), and each clone passes `git fsck --connectivity-only`. Nothing orders the two provisions: each works on its own disk. Negative control: each pod's `ls` of the other issue's workspace path answers `No such file or directory`, while the same path exists in that issue's own pod. Which node each pod lands on is not checked: no pod asks for or keeps off another's node |
| `second-owner` | an issue on `LEGION_E2E_REPO_SECOND_OWNER`, a repository of another owner, gets an issue pod whose init log names its workspace from that repository's default branch as GitHub names it, whose launch makes that repository's own cache (recorded for the teardown first) and whose fetch log streams its feed from it alone, and whose agent is told `LEGION_GH_OWNERS_DIR=/var/run/legion/gh` (listing both owners), `GH_CONFIG_DIR` its own owner's directory and `LEGION_GH_PATH=/usr/local/bin/gh`. Under the agent's own environment its gh, the image's `legion gh` wrapper, answers `gh api repos/sjawhar/legion-smoke --jq .full_name` from sjawhar's read-only directory; `git ls-remote` of sjawhar/legion-smoke through the clone's `legion git-credential` helper answers its `HEAD`, and `git push` there of `HEAD:refs/heads/legion/s4a-7-must-not-land-<run>` (the run's claim project, so a push that ever landed fails only its own run) is refused `403`. Negative controls: the real gh with the issue owner's own directory answers `404` on sjawhar/legion-smoke, which is private to its owner, so the wrapper's pick is what reads it; the operator's gh finds no branch `legion/s4a-7-must-not-land-<run>` on sjawhar/legion-smoke. Nothing is pushed to the second owner's repository |
| `repository-cache` | the cache the root's launch made is Ready in the runtime's stores at its Service, `http://<cache>.<namespace>.svc:8418`, which the cluster holds as the controller made it: controlled by `Sandbox/<cache>`, selecting `agents.x-k8s.io/sandbox-name-hash` at the hash of the cache's name, and headless (`clusterIP: None`) with no `externalName`, the shape the launch checked; inside the cache's pod, `curl localhost:8418/healthz` with no token answers `ready: true` for the repository, with bytes and a fetch time no older than its refresh and two minutes, at the commit GitHub named for the default branch at that fetch: the branch's head now, or, when another run has merged into the shared repository since, a commit on the branch's first-parent line whose next commit landed after the cache's fetch (less ten seconds), logged; and `/tar` answers 401 naming nothing without its token and 200 with the token its container holds, handed to curl on its stdin (`-H @-`), never in a process's argv |
| `cache-second-pod` | another issue pod of the run's repository (`cache-b`, on an issue key of its run's own, `S4A-<n>` with `<n>` the FNV-1a hash of the run's project), launched while the cache is Ready, logs the same stream from the cache, `<m>` under 1 MiB; its clone has no `.git/objects/info/alternates` and `git fsck --connectivity-only` passes using the clone's own objects |
| `cache-lost-volume` | with the cache warm, the harness first deletes the `cache-b` branches killed runs left (cache-b's shape, at the harness's own commit, committed more than 4 hours ago, longer than any run lives since the script stops the harness at `-test.timeout 150m`; a run by hand with a longer timeout must not share the repository with other runs), then pushes `legion/S4A-<n>`, `cache-b`'s branch, as a commit of its own on the default branch's head, adding `.legion-e2e/S4A-<n>.txt` (GitHub's git data API; deleted when the harness ends, and only while it still names that commit, so the run's teardown never deletes a branch the run did not push), checks that commit is neither the default branch's nor the head the cache serves, and checks inside the cache's pod that its clone lacks it (`git cat-file -e`); then, with no runtime running, deletes `cache-b`'s Sandbox, which takes its pod and, with the pod, the disk its workspace was on — what every stop of an issue now leaves. A fresh runtime relaunches `cache-b` into a new Sandbox and a new pod with a disk of its own, whose fetch log streams the cache again and, while the cache still lacks the commit, brings more than no bytes from GitHub for the delta, whose init log reads `workspace-init: <dir> on legion/S4A-<n> from <default branch>`, whose workspace's `jj log -r @-` is the pushed commit, and whose `.legion-e2e/S4A-<n>.txt` holds the pushed line: the branch is where the work survived |
| `re-adopt` | the listener and runtime closed, a separate issue pod deleted while none runs, then a fresh listener and `sandbox.New` with `ReconcileOrphans(known)`: the living claims are alive with their recorded processes and unchanged pods and Sandbox generations, the deleted issue is gone with its recorded process, every living shim says hello again with its current token, and the claim is then resumed into the replacement pod from the session the table holds — a new pod uid, a disk of the same name at a new uid owned by that new pod, and a marker on it holding the resumed incarnation alone, the killed pod's disk and marker having gone with it |
| `orphan-sweep` | a running separate issue Sandbox left out of `known` survives a sweep with a 1-hour grace, and a sweep with a 1-second grace while its tree's lifecycle is open; once its tree's cleanup is recorded confirmed and the runtime and listener are replaced, a 1-second sweep deletes it, and the swept issue's disk goes with it (`kubectl get pvc <pod>-issue`: none) — the Sandbox's delete takes its pod, and the pod takes its own disk. The suspended role and every known issue Sandbox survive |
| `cleanup-reaps-the-issue` | the two halves of what a stop leaves, on the run's first issue. Release of one worker ends only its role process: the shared issue Sandbox stays `Running`, the root role stays Alive, and the pod and its own disk stay as they were. Then `CleanupTree` on that tree — the daemon's durable whole-issue effect, which this direct runtime harness calls itself, having no store or outbox — deletes every issue Sandbox of the tree and hands none over: the Sandbox is `NotFound`, and the disk the issue's work was on is gone with its pod |
| `displaced` | a launch that replaces an issue pod another role still runs in (LEGION-729): its own issue `S4A-6`, whose architect runs when the operator deletes the implementer's launcher Secret (`<sandbox>-implementer-boot`), an operator's fault, as `kill-role-init`'s SIGKILL is. The implementer's Spawn then finds the pod no longer binds its launcher and replaces it under the restricted identity: a new pod uid, the same Sandbox uid `Running`, and the Sandbox annotated `legion.dev/displaced-pod` with the replaced pod's uid, by the merge patch that set it `Suspended`, which the cluster's admission admits for the daemon's identity. The architect's end is observed at its recorded locator as `Gone` or `NotRecordedProcess` with `Displaced` set, the verdict the supervisor resumes charging nothing; the architect's Resume, from the session the table holds, starts in the implementer's new pod, registers with its new generation's token and probes Alive. The pod that went took its disk with it, so the new pod's is a claim of its own — the same name, owned by the new pod, a new uid — and the marker on it holds the resumed incarnation alone, never the replaced one. Both roles are then suspended, the new pod kept |
| `previous-layout` | with this runtime stopped, the previous release's own Stage 4a harness (built from `git archive` of `LEGION_E2E_PREVIOUS_IMAGE`'s revision (a commit of main at contract 37, the pod-owned disk, or 38, the blocked issues, before one container) with `packages/daemon/internal/runtime/sandbox/testdata/previouslayout/fixture_test.go.txt` added) launches issue `S4A-10`'s architect and tester on `LEGION_E2E_PREVIOUS_IMAGE`, one pod of six role containers, each claim's marker at the root of that pod's own disk and its session recorded in the run's session table; their locators are taken over as store migration 0054 rewrites them (container `roles`) and this runtime starts: each role's first observation is `StaleAddress` naming the six containers, the architect's Resume replaces the pod with one `roles` container, the tester's old process reads `Displaced` (uncharged) and resumes into the same new pod, each from the session the table holds, and each role is charged exactly one new generation, registers once at it, and its marker on the new pod's own disk holds the resumed incarnation alone, a later one than the previous incarnation the fixture recorded — the old pod's disk, and its line, went with that pod |
| `postgres-resume` | the run keeps every session in the scratch database, and a new tree's root spawned under that runtime registers with `OMP_SESSION_STORAGE=sql`, `OMP_SESSION_SQL_DSN_FILE=/var/run/legion/providers/OMP_SESSION_SQL_DSN` and `LEGION_WORKSPACE_RECREATED=false` in its agent's environment, and never the URL. The harness writes this one role's session into the table itself, as `legion sessions import` copies one whose disk is gone, at a path the pod's disk holds no file at, dated an hour before the pod's workspace was created. Suspended, the root's Resume naming a path the table lacks is refused by its launcher with `the session table holds no such session` and registers nothing; its Resume from the copied path registers that generation's token, so the launcher found the session in the table through the pod's URL file, and the agent's environment (`LEGION_WORKSPACE_RECREATED=true`) says the workspace was recreated since the session was last written |
| `issue-suspend-resume` | the whole of the change on an issue of its own: its role runs, its session is in the table, and `SuspendIssue` closes it as the daemon's durable close effect does — the Sandbox `Suspended`, its pod waited out, then the Sandbox deleted with the six role Secrets it owned. Nothing of the issue is left: `get sandbox` is `NotFound` and `get pvc <pod>-issue` answers none. The resume that follows is a launch from nothing: a new Sandbox, a new pod, and that pod's own disk under the same name with a new uid and the new pod as its owner, the agent carrying on from the table with `LEGION_WORKSPACE_RECREATED=true` and a marker holding its resumed incarnation alone |
| `namespace-clean` | the script's last step, after the teardown and outside the harness: the run's own Sandboxes, Secrets, PVCs, pods and ConfigMaps are exactly the snapshot taken before the run: those labelled with its project, and those with no project label whose name carries the run's label, as written or as a claim token carries it, which the teardown's label selector would leave behind. Another system's objects in the shared namespace, labelled or not, are not the run's and are not compared |

Everything the run creates carries the project label `s4a-<UTC timestamp>-<4 hex>`, and the
claim tokens carry the same value without its dashes. The harness appends each Sandbox's name to
a record before the Sandbox can exist. On any exit the `EXIT` trap runs
[`lib/namespace-rig.sh`](#libnamespace-rigsh)'s teardown: it refuses to act on a project without
the `s4a-` prefix, deletes every recorded Sandbox by its exact name and then the Sandboxes
labelled with that exact project (never by label existence), the run's ConfigMap by the same
label and its providers Secret by name, waits for the owned Secrets, pods and PVCs to follow,
deletes by the same exact label any Secret or PVC still left after 90
listings, and runs `namespace-clean` when the harness did not get to it. Nothing outside `legion`
is touched.

What the run had to learn about production (AWS, `LEGION_E2E_RUNTIME_IDENTITY_KIND=aws-iam-role`):

- **Nothing ties a pod to a node.** Every issue pod owns its volume and carries no affinity, so the
  scheduler bin-packs it wherever the pool has room for its reservation, and Karpenter adds a node
  under the `legion` NodePool's floor, `karpenter.k8s.aws/instance-cpu` Gt 3 (set in the cluster's
  own infrastructure code), when none has. Under the tree-volume layout before LEGION-632 every pod
  of a tree had to join the node of the tree's first pod, and a `c7a.medium`'s 8 pod slots, 7 of
  them daemonsets, left no room for a second: that is what the floor was set for, and it now only
  decides the smallest node the pool adds.
- gVisor on production reports `4.19.0-gvisor` from `uname -r`.
- The worker image has no `kill` binary; the exec runs the shell's builtin.

What the run had to learn about GKE (`LEGION_E2E_RUNTIME_IDENTITY_KIND=kubernetes-service-account`,
a production GKE cluster the daemon deploys into), each confirmed against a real, already-running
production issue pod as well as this run's own:

- **gVisor floors a lone small Sandbox's cpu to 500m.** A single-container Sandbox (the image
  probe, 250m configured) is admitted with its container's cpu request and limit both raised to
  500m, request and limit together so it stays Guaranteed; an issue pod, whose one `roles`
  container already reserves well past that, is admitted at exactly its configured cpu,
  unchanged. The harness floors only the probe's expectation to match
  (`gvisorCPUFloor`, `live_lifecycle_test.go`).
- **Every container's ephemeral-storage limit is clamped to its request, and GKE Autopilot caps a
  whole pod's summed requests at 10Gi regardless.** Confirmed live on a bare debug pod and on every
  container of a real production issue pod: each admitted with request = limit = its configured
  request, whatever higher limit was configured; a gVisor-sandboxed pod's ephemeral storage is
  never burstable here, so the harness checks the request's value in place of the configured limit
  (`gkeEphemeralStorageClamped`) for the image probe, whose request is the daemon's own fixed
  value. The issue pod's reservation, the rig's `liveIssueOverride`, sets ephemeral-storage 10Gi
  as both request and limit, so the clamp changes nothing there. GKE Autopilot's Warden admission
  webhook refuses to schedule a pod whose summed ephemeral-storage request passes 10Gi (naming
  `autogke-pod-limit-constraints`, "higher than the Autopilot maximum of 10Gi"; seen live when the
  six role containers of the earlier layout each asked for their own limit, about 110Gi in all).
  With every role in one `roles` container, that cap is the issue pod's one reservation itself:
  `runtime.kubernetes.resources.issue.ephemeral_storage` and its request, at most 10Gi on GKE
  Autopilot, shared by all six roles.
- **GKE Autopilot adds its own NodeAffinity to every issue pod.** Any pod carrying
  `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` (every issue pod's annotation; the
  image probe omits it) is given a `cloud.google.com/extended-duration-pods` NodeAffinity on
  admission, protecting it from disruption for up to seven days. It is GKE's own addition, not a
  pod or anti-affinity the pod spec asks for, and still admits the pod to any node whose pool
  carries that label — the reservation rule tolerates exactly this one shape, never any other
  affinity (`isGKEExtendedDurationAffinity`).
- **`nproc` inside a gVisor sandbox reflects a host-vCPU presentation, not the pod's own cgroup
  quota.** Two debug pods summing to 2.5 and to exactly 3.0 cores (one container each at 1 and 2
  cores) both reported `nproc=2` on an 8-vCPU node with 7.91 allocatable — far more than either
  sum, ruling out node capacity — a known gap (kubernetes/kubernetes#91665). The pod's cgroup cpu
  quota, read from every container's identically-presented `/sys/fs/cgroup/cpu/cpu.cfs_quota_us`
  and `cpu.cfs_period_us` (one gVisor sandbox covers the whole pod), tracked the configured sum
  exactly in both debug pods and is what `resources` now asserts on GKE; `nproc` is logged, never
  asserted, there. AWS keeps the original `nproc` assertion, proven reliable there.
- **The harness's own direct Postgres connection needs the bind host, not the advertise host.**
  the harness's `writeSession`, which records every claim's session and which `postgres-resume`
  calls once more of its own, dials the scratch database directly from the devbox; under
  `bridge` mode the pod-facing advertise host is the bridge's cluster-DNS name, which the devbox
  itself cannot resolve. A second DSN file, at the bind host (loopback under `bridge`), is written
  for the harness alone; pods and the providers Secret still get the original.
- **This script's own `set -e` treats `kill`, `wait` and `kubectl get` succeeding at exactly what
  they were asked to do as a fatal error, if nothing guards them.** The actual cause of every
  "every `CHECK` passed, then the script exits before teardown runs at all" failure this lane hit
  (confirmed by `strace -f -e trace=kill,tkill,tgkill,execve`, after a GKE API-consistency theory,
  this devbox's own tooling, a watchdog race, and a shared process group were each ruled out by
  direct evidence first): three bare statements in `bridge_stop` — `wait PID` for a process it had
  just killed (which reports 128+15=143, bash's own encoding for "killed by that signal"), a bare
  `kubectl get` on an object that has just, correctly, become `NotFound`, and `kill PID` on a
  process already dead (an empty-but-expected race between the watchdog's own exit and
  `bridge_stop`'s cleanup) — each returns a nonzero status at exactly the moment its own job
  finishes successfully, and `set -e` ends the whole script right there, having nothing to do with
  an external signal at all. Every one of those three needs its own `|| true`; a guard around only
  some of them still fails the same way, intermittently, since which one fires first depends on
  timing. Confirmed in isolation (not just live) with a six-line repro before the final fix: a bare
  `kill $pid; wait $pid` reliably exits 143 printing nothing after it; the same two lines each
  followed by `|| true` run to completion.
- **A dropped `kubectl port-forward` does not have to end the run.** Two full-length runs died
  outright to a transient drop during this lane's work. `kubectl port-forward` never retries
  itself, but nothing about a drop invalidates the chisel client's tunnel state (its auth, its
  reverse listeners) if the port-forward comes back on the same local port — so the bridge's
  watchdog now restarts a dropped port-forward itself (confirmed live: killing the process mid-run
  let it recover and the run continue), only failing the run if a restart can't land within 60s,
  or the chisel client's own bounded reconnect (`--max-retry-count 30 --max-retry-interval 2s`,
  not chisel's default of forever) gives up first.
- **A lone pod on its own node is fair game for node consolidation.** A bridge pod without the
  eviction-protection annotations every issue pod carries ran fine for ten minutes, then vanished
  outright mid-run (not a graceful shutdown: its chisel client's log shows the connection refused,
  not closed) — consistent with GKE reclaiming the underutilized Node-Auto-Provisioned node it was
  alone on. The watchdog's port-forward recovery is the right answer to a dropped connection, never
  to the pod itself being gone (every restart attempt correctly failed `NotFound` once it was); the
  actual fix is the same `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` and
  `karpenter.sh/do-not-disrupt: "true"` pair every issue pod already carries, now on the bridge pod
  too.
- **This cluster's `autopilot-default-resources-mutator` admission webhook adjusts the bridge
  pod's cpu on create**, and bringing up the fresh Node-Auto-Provisioned node it needs can take
  close to the original 120s `Ready` wait budget; raised to 240s.

## stage4b-sandbox-tree.sh

Stage 4b's gate for the Go coordinator: the Go daemon drives real issue trees on the Agent Sandbox
runtime. It runs in the production cluster's namespace `legion`, against production Dispatch, the
production Envoy listener and production NATS, in the disposable Dispatch project LEGSMOKE and the
smoke repository `sjawhar/legion-smoke`. The daemon runs on the devbox as the Legion daemon's
restricted identity, with `bind: 0.0.0.0`, as a daemon that runs as a pod binds, and its pods dial
its worker stream at `advertise_host`, the devbox's private address, and keep their Oh My Pi
sessions in a scratch Postgres the run starts beside it (`session_store: postgres`: the daemon
requires a store that keeps sessions in a database, since an issue pod's disk dies with the pod). Tree 1 runs the whole workflow
to `done`; while its planner holds, the driver files two children under it, each in an issue pod
and on a disk of its own, closes one as `done` and parks the other in backlog. Two more children
are still in the workflow when tree 1's pull request merges, so the merge is recorded and the root
waits in `awaiting_merge` until the proof human has parked both; the last park closes it. Its
`legion.yaml` names `runtime.kubernetes.plugins` with the latest published `@sjawhar/pi-legion` and
`@sjawhar/pi-envoy`, resolved at run time: the branch-built daemon links no release plugin pair, so
without it the run would boot in the degraded mode (`plugins: none` from `--check-config`) instead
of the pair a stale-plugin relaunch would install, even though this run's own claims register at
the branch daemon's own contract and so never go stale enough to trigger that install. It sets
`linger_hours: 0.5` (tree 1's linger runs from the close its last child's park brought on, and
holds its production check's first look and suspension, the smoke main's cleanup and the check's
bound; `close` then waits for the expiry) and
`production_check_hours: 0.25` (the check's bound, 14 days by default, brought inside the run: 15
minutes, after the first look's report, since a look open at the bound defers it by the look's own
two-hour bound).

```sh
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
  LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> \
  LEGION_E2E_DISPATCH_URL=https://<dispatch> LEGION_E2E_ENVOY_URL=http://<listener>:<port> LEGION_E2E_NATS_URL=nats://<nats>:4222 \
  LEGION_E2E_DISPATCH_TOKEN_FILE=<0600 file> LEGION_E2E_ENVOY_TOKEN_SECRET_ID=<secret id> \
  LEGION_E2E_SESSION_DB_PORT=<port> \
  bash scripts/e2e/stage4b-sandbox-tree.sh        # → "stage 4b e2e: PASS", exit 0
STAGE4B_UNTIL=<checkpoint> …                      # a development run: stops after that checkpoint, never PASS
STAGE4B_DESIGN_GATE=root-issues STAGE4B_UNTIL=spec-posted …   # the design gate, armed, on tree 1 alone
LEGION_E2E_POD_FILE=<block.json> STAGE4B_UNTIL=deployment-pod-block …   # a deployment's own worker pod block, before it ships
LEGION_E2E_PROJECT=S4B2 LEGION_E2E_DAEMON_PORT=13370 LEGION_E2E_WORKER_STREAM_PORT=13371 LEGION_E2E_SESSION_DB_PORT=<port> \
  bash scripts/e2e/stage4b-sandbox-tree.sh        # a second lane beside a default-identity run; S4B2 is created if missing
```

The repository names no production service. `LEGION_E2E_MODEL_GATEWAY_URL` is the model gateway's
Anthropic endpoint (checked by [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh)), which the
run writes into its copy of the operator fixture's `models.yml` and the controller's profile.
`LEGION_E2E_DISPATCH_URL`, `LEGION_E2E_ENVOY_URL` and `LEGION_E2E_NATS_URL` are production
Dispatch, the production Envoy listener and production NATS, by the operator's fully-qualified names
for them. `prerequisites` refuses a value that is unset, names a bare alias, or carries a path,
naming the variable and never its value.

`LEGION_E2E_MODEL_GATEWAY_AUDIENCE` is the audience the model gateway accepts on a worker's projected
ServiceAccount token. The run puts it in place of the placeholder in its copy of the operator route's
`pod.yml`, which the daemon loads, and `pod-shape` holds every pod to exactly that one token.
`LEGION_E2E_DISPATCH_TOKEN_FILE` names a file only its owner can read (no group or other permission
bits) holding a bearer of the Dispatch agents' client: the one an agent session's own Dispatch
configuration resolves, `DISPATCH_TOKEN_FILE`, then `DISPATCH_TOKEN`, then `dispatch.token` in
the envoy.json the session reads (`packages/envoy-client/src/dispatch-config.ts`). Dispatch answers
that bearer as an agent session, the actor its HTTP routes and its document websocket require.
`LEGION_E2E_ENVOY_TOKEN_SECRET_ID` is the Secrets Manager id of the production Envoy listener's API
token, which `prerequisites` reads with the devbox admin role. Both bearers land in 0600 files under
the run's scratch directory. `prerequisites` refuses the token file when the variable is unset, or
names something that is not a readable regular file, whose group or others have any permission bit
(read through a symlink, from the file it names), or that holds only whitespace; each refusal names
the variable and the path, never what the file holds. It refuses the other three when they are
unset or malformed (the audience through
[`lib/model-gateway-audience.sh`](#libmodel-gateway-audiencesh)), naming the variable and never its
value. `preflight` reads Dispatch's `whoami` with the run's bearer and requires an agent session;
the same read with an invalid bearer, its negative control, must be refused 401.

The App inputs ([`lib/github-apps.sh`](#libgithub-appssh), optional) name another deployment's two
role Apps; `prerequisites` proves each key is its id's App
([`read_app_logins`](#libworkflowsh)), default or input, before `ensure_project`, the run's first
write anywhere. The daemon's `github_apps` is the block it proved.

`STAGE4B_UNTIL` must name a checkpoint below; any other value is refused. `STAGE4B_SKIP_CONTROLLER=1`,
refused without `STAGE4B_UNTIL`, runs none of the checks of `controller`, `full-agent-negative` or `controller-walk` and only takes
tree 3 out, printing `CHECK <checkpoint>: SKIPPED (…)` for each of the three,
so a development run can reach a later checkpoint while a defect of the controller's own is unfixed; it also skips the GitHub credential check in `controller` and the one in `daemon-controller-liveness`'s phase 1, so a run with it proves neither controller's gh. `STAGE4B_EVIDENCE_DIR`
keeps the evidence (default: a fresh `/tmp` directory, printed at the end): the transcript, the
daemon log, `run.json` (source revision, image and plugin), the pod watch, each checked pod's spec,
every agent transcript (the pods' under `transcripts/pods/`, the operator's controller's under
`transcripts/controller/`, and under `transcripts/daemon-controller/<pod>/` the controller the daemon
launched in `daemon-controller-liveness`, whose sessions are also among the pods'), the interest
samples, the audit files and the negative controls. The pods' transcripts are copied from the
session database before the teardown removes it, each at a name derived from its session key, so
no key places a file outside `transcripts/pods/`; that directory holds the pods' sessions alone, so
no key a pod writes can take a name the harness writes beside it (`controller`, `daemon-controller`).
A key the copy refuses (its name empty, `.` or `..`, shared with another key, or resolving outside
the directory) or cannot read is named, never its bytes, and skipped while every other session is
still copied, and any key left out fails the teardown check `transcripts`, as does a failure to copy
the operator's controller's sessions out of the run's profile before the teardown removes it. The
directory a copy writes in is the harness's own, never a path an agent's value chooses: the
reviewer's subagents' go under `review-pair/subagents/`, a name the harness fixes, and the launched
controller's under `transcripts/daemon-controller/<pod>/`, named for the pod the daemon launched it
in, as the daemon's claim locator records it. The session file an agent records only selects which
sessions to copy; the reviewer's stem is kept as data in `review-pair/session-stem`, so a reader
can tell which session the pair came from. The copy still refuses, by name and never cleaned, a
directory name that is not a plain one of `[A-Za-z0-9._-]`, is `.` or `..`, or does not resolve
directly inside `review-pair/` or `transcripts/daemon-controller/`, and writes nothing of that copy:
a second fence behind the harness's own names. What the
run built is printed by [`lib/built-from.sh`](#libbuilt-fromsh). Its verdict is one line, just before the evidence line:
`stage 4b e2e: PASS`; `stage 4b e2e: FAIL (check <check>)`, after [notes](#libmodel-gateway-unservedsh)
on whether the controller could have failed that check for want of a model key; or
`stage 4b e2e: BLOCKED (check <check>)` when the checkpoint could not run and the teardown checks
(`transcripts`, `namespace-clean`, `production-audit`) passed, which makes the run no verdict on the change while
the checkpoints before it stand, and gets no notes. Capacity is such a stop. No pod asks for or
keeps off a node: each issue pod carries its own disk and the issue pod's one cpu and memory
reservation, shared by its six roles
(docs/kubernetes.md "Issue sizing: one reservation per pod"), so `preflight` counts the `legion`
pool's room in pods of that reservation under the run's resources (`lib/stage4b-room.jq`) and ends
BLOCKED `capacity: …` when it cannot place the run's four pods at once: tree 1's root and its two
children beside tree 2's root. Its room is what each Ready, schedulable pool node, neither tainted
`karpenter.sh/disrupted` nor being deleted, has free beside every non-terminal pod placed on it, of
any namespace, plus the pods the pool's cpu and memory limits still leave room for beside what it
already runs (a pool with no limit bounds nothing). A wait that needs a tree's pods placed names that tree (or the
reachability pod) as its subject. If it times out while one of its subject's pods is Pending
Unschedulable, and Karpenter's newest event of any reason for that pod's uid, no older than its
Unschedulable transition, is its FailedScheduling finding every instance type over the pool's
limits (a newer Nominated or other Karpenter event supersedes it), it ends BLOCKED `capacity: …`,
naming the pod with its evidence: the limit event's time and message, the pod's PodScheduled
condition, and the default scheduler's newest FailedScheduling message for it. A wrong
classification can only read BLOCKED, never PASS, and the line shows it. A wait with no subject, a starved pod outside the subject, any other timeout, and
any check that fails outright stay FAIL. A failed teardown check outranks every reason
the run stopped: it prints its own `CHECK <name>: FAIL` line (the audit's names the run's writes and
subscriptions outside LEGSMOKE, as the `production-audit` checkpoint does), and the verdict is
`stage 4b e2e: FAIL (check <teardown check>, in the teardown after check <check>)` whenever the
checkpoint that stopped the run did not fail itself: a `STAGE4B_UNTIL` run's last checkpoint, a
blocked checkpoint, a signal. `done` cleans the smoke `main` after tree 1's merge, which closed it,
while the implementer works its production check's first look (`clean_smoke_main`,
[stage 3's cleanup](#stage3-devbox-workflowsh), with its bounded
wait). That cleanup is the fixture's teardown, not the workflow under test. It stays inside `done`,
so a `STAGE4B_UNTIL=done` run still cleans the smoke `main`, and the driver marks it
(`smoke_main_cleaning`) for the `EXIT` trap. When `done` fails there, the verdict is
`stage 4b e2e: FAIL (fixture teardown, in check done): every checkpoint before done passed, …`, after
the same notes, unless a teardown check failed or a production guard recorded a violation
(`$evidence/pane-endpoint-violation.txt`). Then it is the ordinary `stage 4b e2e: FAIL (check done)`.
Either way the rest of `done` and the checkpoints after it do not run. Every verdict but the pass
exits non-zero: 1, or the stopping signal's 129, 130 or 143 when the teardown was clean.

`STAGE4B_DESIGN_GATE=root-issues`, refused without a `STAGE4B_UNTIL` of `spec-posted` or a checkpoint
before it, arms the design gate (`gates.design: root-issues`) and files tree 1 alone, since each
admitted root's architect requests approval on its own. Tree 1's document leaves one choice (where
the smoke file goes) to the human. `admitted-issue-cap` prints `SKIPPED`, and `spec-posted` waits up
to 12 hours for a human to answer the architect's decision block and approve the spec in Dispatch.
It then fails unless the architect's approval request at the approved version carries a summary
after `Approve spec.md (version N)?`, a human answered at least one of the spec's decision blocks,
and no approval request on the spec was early by either of two rules
([`lib/design-gate-verdict.jq`](lib/design-gate-verdict.jq), tested by `bun test scripts/e2e/lib`).
The version rule judges every hand-back: the version its event records in `requested_version` must
hold none of the spec's blocks open, read from the version itself by the block ids of the spec's
block asks, because Dispatch indexes a block as an ask only when it settles the document, after
the edit that wrote it. An approval row follows versions in place, so `ask.opened` records its
first hand-back and each `ask.handed_back` a later one; an `ask.edited` only rewords the request.
The answer-time rule judges every hand-back against the human's first turn on it (an answer to its
request, or a human's reply in the request's thread before the request is handed back again): no
block raised before that turn may be answered after the hand-back. That catches a request made
while the choice was still prose, or sent in parallel with the edit that wrote the block. A block
raised after the human's turn takes up what they said, so the flows the dispatch skill and
`legion-architect` prescribe pass: the human replies in the request's thread, or answers Request
changes; the revision raises a block; the human answers it; and the architect hands the request
back, or requests approval again, which opens a new request since Request changes answered the old
one. It keeps the issue's asks as `<issue>-asks.json`, approval events as
`<issue>-events.json`, each requested version as `<issue>-spec-v<N>.json`, and the verdict as
`<issue>-gate-verdict.json`.

`LEGION_E2E_POD_FILE` names a JSON file holding a deployment's own worker pod block, the
`runtime.kubernetes.pod` value its configuration renders (`service_account`, `env`, `volumes`,
`volume_mounts`, in the shape `packages/daemon/internal/config/kubernetes.go` reads). The run copies
it once, at prerequisites, as `jq -cS .` into its work directory and records the copy's sha256; every
later step reads that copy, so a pass ties to one block. The run's `legion.yaml` carries the block
whole, as one JSON line, in place of the operator fixture's `pod`, so its every pod (the boot
probe's included) is built from it and nothing in it is rewritten: every object it names, its
ServiceAccount, Secrets and ConfigMaps, must exist in the namespace. It is refused unless
`STAGE4B_UNTIL=deployment-pod-block`, since the checkpoints after that one rely on the operator
fixture's pod, and refused when the file is not a JSON object of that shape. Without it,
`deployment-pod-block` prints `SKIPPED`.

This is how a deployment proves a change to its pod block (a new projected token, a mounted
ConfigMap, a variable) on the real daemon before the change ships: the daemon boots on it, admits a
root, and launches the root's issue pod from it. A pass proves that and no more:
- Run it from the release tag the deployment pins and with `LEGION_E2E_IMAGE` set to the worker
  image it pins: the daemon's boot probe, its launch and the pod's shape are that release's.
- The rest of `runtime.kubernetes` (namespace, pool, scheduling, the controller and the deployment's
  other keys) is Stage 4b's own, never the deployment's, so pair the run with `legion start
  --check-config` on the deployment's whole `legion.yaml` at that release.
- A Secret or ConfigMap volume is compared by its kind and object name, not its `items`.
- A projected token is checked as carried, with its audience, expiry and path, never as accepted by
  the service it is for.
- The pod is an issue pod; the daemon's own controller pod, under `controller: daemon`, is not
  launched or checked.

Three roots are set todo under `admission_cap: 2`:
- Tree 1 runs the whole workflow with real agents to `done`, through one changes-requested review
  round in a thread the reviewer opens and the architect's acceptance at `accepting`; the daemon's
  merge is recorded while the rest of its tree still runs, the last of that tree's parks closes it,
  its implementer runs the production check after the close (the first look, then
  the bound's last look) while the tree lingers, and the tree closes at the linger's expiry. While its planner holds, the driver
  files two children under it, each in an issue pod and on a disk of its own: child 1 is closed
  as `done` and released, child 2 parked in backlog, which deletes its Sandbox as a close does —
  no Sandbox, no pod and so no disk. Children 3 and 4, filed just before `done`, are still in the
  workflow when the pull request merges: the daemon records the merge, leaves the root in
  `awaiting_merge` with nothing suspended, no linger, its slot and its roles kept, and tells its
  architect once, naming the pull request and both children. The proof human then parks them one
  at a time, each park leaving the child no Sandbox, no pod and no disk; child 3's park moves
  nothing of the root, and child 4's closes it, as the merge of a finished tree's root does.
- Tree 2 runs through its planner beside tree 1, in an issue pod of its own, carrying the
  repository-configuration fixture (the loading paths' markers, two skills, a Go, a TypeScript and
  a Python root, and a `.omp/config.yml`), whose tools its planner uses in its checkout
  (`repository-tools`), supplies the capability report every session sends at its start
  (`full-agent`), and is then moved to backlog.
- Tree 3 is admitted when tree 2 leaves the line. It supplies the held phase the controller
  checkpoint needs and, held, the planner whose workspace turns a capability off and on again
  (`full-agent-negative`), and is taken out from an operator shell (`controller-walk`).
- Tree 4 is admitted when tree 3 has been taken out. It supplies the deaths of a worker whose task
  is outstanding, and is taken out the same way.

**Two checkpoints are gone with INFRA-229**, `mid-turn-carry` and `carried-child`, with the setups
that left a child mid-run and mid-turn under a closing root. Each needed a root to close while a
child of its tree was still in the workflow, and no merge does that any more: the merge is
recorded and the root waits. The one close that still lingers a tree children and all is a
person's own in the Dispatch dashboard, which this driver cannot make — every write it sends
production Dispatch names a session as its author (`lib/workflow.sh`), the daemon sets a live
root's status back when the session writing it holds no claim in the tree, and the operator's
`legion status` writes only `todo`, `backlog` or `icebox`, never the `done` a hand-over needs. A
finished root's hand-over of a child keeps its cover in the daemon's own tests
(`packages/daemon/internal/admit/finished_root_test.go`).

**One run at a time per identity.** `LEGION_E2E_PROJECT` (default `LEGSMOKE`, a Dispatch project
key: `[A-Z][A-Z0-9]{1,9}`, exactly Dispatch's own `projectKeyPattern`
(`packages/envoy/internal/dispatch/api/server.go`), checked in `prerequisites`) names a run's
identity; everything else derives from it. `run_label`, the `legion.dev/project` label every
object in the namespace carries, is the daemon's own `ProjectToken(project)`
(`packages/daemon/internal/claim/token.go`) — exactly `project` lowercased, since the validated
shape holds no character `ProjectToken` would drop. The lock is today's single fixed path when
`run_label` is the default `legsmoke`, else a path named for `run_label`, so a non-default
project never shares today's lock, its leftover checks, or its durable consumers with a default
run. `LEGION_E2E_DAEMON_PORT` (default `13372`), `LEGION_E2E_WORKER_STREAM_PORT` (default
`13373`, the same name Stage 4a reads for its worker stream) and `LEGION_E2E_SESSION_DB_PORT`
(required, no default: the run's own scratch session database, which every pod of it dials) are
the only other inputs, each within the
devbox's admitted range ([Port admission](#port-admission)); the run's daemon may swap which of
its API and worker-stream ports takes which (`address-moved-stream-new-pod`), never a third.
Left at their defaults, a run collides with another default run exactly as it always has; set all
four to distinct values and two runs proceed at once, on the same devbox, against the
same production cluster. `LEGION_E2E_PROJECT` need not already exist as a Dispatch project:
`prerequisites` creates it before the lock when it is missing (`ensure_project`, `lib/workflow.sh`),
a proof-human write like every other, over the agents' bearer and naming the proof human's session
as its actor.
- The run takes its own lock, derived from the project (above).
- It owns its identity's shared objects only after four checks pass: that lock, both its ports
  free, no leftover `run_label` object in the namespace, and no `legion-go-<project>-` consumer on
  the stream.
- A run refused at any of the four removes nothing.
- Each daemon start checks both its ports again, since another process can take one after the
  prerequisites; once the daemon answers `/healthz`, both ports must be held by that daemon's
  process, or the boot fails naming the holder. A `/healthz` answer alone could come from another
  daemon on the same address.

**Two lanes need separate checkouts, not just distinct identities above.** The controller
checkpoint packs both of this checkout's plugins, `@sjawhar/pi-envoy` (`packages/pi-envoy`) and
`@sjawhar/pi-legion` (`packages/pi-legion`) ([`lib/pack-plugin.sh`](#libpack-pluginsh)), each
rewriting its own `package.json` in place for the few seconds its pack takes, under its own flock
on that one file: pi-envoy's pack and pi-legion's pack take separate locks, each serializing
two lanes' packs of that one plugin against each other (whichever holds it waits its turn, as
`release.yaml` and every other branch-built install already rely on); neither protects a lane's
own [`lib/built-from.sh`](#libbuilt-fromsh) — called in `prerequisites` and again before the
build — whose `jj diff`/`jj log` read the shared working copy directly, with no lock of their
own. A second lane's pack landing mid-read reads as this lane's own tree having changed, failing
`prerequisites` or the build's own revision check for a reason that has nothing to do with the
change under test. Run concurrent lanes from separate checkouts (`jj workspace add`, or separate
clones) of the same commit.

**Two lanes can race the legion pool's own capacity preflight, and that is a finding, not a bug
to fix here.** `prerequisites` reads the legion NodePool's current room (`pool_pod_room`) before
either lane has placed a pod, and refuses, BLOCKED, only if that one read already can't fit
`run_pods_at_once` pods (4: the cap's two roots and tree 1's two children). Two lanes starting
close together can each read room enough for itself alone — the read is not a reservation — so a
true shortfall surfaces only once both are actually scheduling. That is never silent: every
pod-placement wait is wrapped (`on_tree`/`on_issue`), so a wait stalled on the pool's own limits
while its subject pod is still Pending, or after its claim has given up, ends its checkpoint
`BLOCKED: capacity: …`, naming the pod the scheduler could not place — the same outcome a single
lane gets today when the pool is simply full. A pod that starves and is placed before its own
claim's launch budget runs out, with the wait still timing out for some other reason, reads FAIL:
neither path has evidence of a starvation that never reached either threshold.
Running two lanes at once therefore needs either enough spare legion-pool capacity for both (8
pods' worth of reservations between them) or tolerance for the slower lane landing `BLOCKED` on
capacity; widening the pool is infrastructure, not this change.

**What the run touches in production**, all of it removed by the `EXIT`/`INT`/`TERM`/`HUP` trap of
the run that owns it. A signal to the whole process group does not stop the removal:
- a closed pane, a Ctrl-C, or `timeout`'s TERM;
- the transcript's `tee` ignores those signals and SIGPIPE;
- whoever reads the run's output can go first, for example a supervised launcher's own `tee` stopped
  with the run. A run whose reader goes and no signal follows runs to its own end, and holds the
  lock until then;
- the transcript's disk can fill. With or without its reader, the run keeps going and its teardown
  still runs in full, and its output reaches anyone still reading. GNU `tee`, the devbox's, keeps
  the transcript only up to the point its disk filled and never reopens it, even once space
  returns; busybox `tee` picks the transcript up again once there is room
  ([`lib/transcript.sh`](#libtranscriptsh));
- the teardown ignores a second signal and SIGPIPE;
- the teardown writes to the transcript even when the signal interrupted a command whose output
  went to `/dev/null`;
- a teardown command that fails prints `cleanup warning: line N exited S`, never a check's `FAIL`
  line, and leaves the exit status the checks set.
- **Production NATS, stream `ENVOY_NOTIFICATIONS`**: the daemon's two durable consumers. They are
  named by the Dispatch key: `legion-go-<project>-dispatch` (`notifications.dispatch.issue.>`) and
  `legion-go-<project>-github` (`notifications.github.sjawhar.legion-smoke.>`) — `LEGSMOKE` by default.
  - The teardown deletes both by exact name and prints `deleted` or `absent` for each.
  - `hygiene` fails if either remains, and the preflight refuses to start while either exists.
  - A SIGKILLed driver runs no trap, and the next run of the same identity's preflight names what it left; a different identity's run never sees it.
- **Production Dispatch, project `LEGSMOKE` by default**: three root issues per run. The preflight moves stale
  todo issues to backlog, roots and the children a failed run left todo alike. The proof human's writes use the agents' bearer and name the session
  `legion-e2e4b-proof-human-<pid>` as their actor, which production Dispatch requires; it holds no
  claim, so the daemon sets back its status write on a live root as it does any outside session's.
  The run takes trees out with `legion status` from the operator shell, the daemon's own write.
- **`sjawhar/legion-smoke`**: the fixture branch `legion/<tree 2>`, tree 1's pull request, which
  the daemon merges on its accepted READY while the proof only reads it (`await_ready_merge`), and
  `done`'s cleanup pull request on `proof/clean-main-<run_label>` (`legsmoke` by default), which the proof human merges. The
  teardown closes any pull request the run left open, such as one from a
  run that stopped before the merge, and deletes each tree's branch `legion/<tree>`. It also closes
  the cleanup pull request if it is still open, with a comment naming the run, and deletes its
  branch, when the run stopped after `done` made that branch and before the merge deleted it (a
  merge GitHub answered with a 504, the wait that timed out, an interrupt;
  [`close_smoke_cleanup`](#libworkflowsh)), and prints the URL of a pull request it could not close.
  Every run's cleanup branch is named for its own identity, so the teardown acts only on the branch the
  run recorded making: a run that never reached the cleanup touches none, an earlier run's
  included, and `done` fails, naming the branch, when one is there before it makes its own, which it
  then leaves. To go on, delete that branch on `sjawhar/legion-smoke`, which also closes a pull
  request open on it, and run again. Every one of
  these writes is Stage 3's proof human's: the devbox `gh`, and for the fixture push the git
  credential helper the same routing installs. So the driver runs from the operator's own Oh My Pi
  session, and `prerequisites` refuses to start, before it takes the lock, when `gh` acts as anyone
  but `sjawhar-agent[bot]` ([`require_proof_human`](#libworkflowsh)).
- **Namespace `legion`**: the run's Sandboxes, pods, Secrets and PVCs, its control pods, and its
  copy of the operator fixture's ConfigMap, `legion-operator-route-legsmoke`, all labelled
  `legsmoke`. [`lib/namespace-rig.sh`](#libnamespace-rigsh)'s teardown and
  `namespace-clean` hold the namespace to its snapshot.

`production-audit` checks the run's own writes. Its window opens, to the nanosecond, just before
the daemon starts. It lists every production issue outside LEGSMOKE updated since then, and keeps
those whose events name one of the run's writers: its agents' sessions, `legion-daemon:LEGSMOKE`,
and the proof human. Event times are compared as instants, not strings.

It also audits the Envoy interests the run's sessions held, which are sampled every 5 s and at
every checkpoint while the daemon runs. The run fails when:
- a registered session has no sample the listener answered;
- the listener left one session's samples unanswered 3 times in a row. That leaves a gap of at
  least 20 s against the sampler's 5 s. One or two failures in a row are a blip, kept in
  `interests-outcomes.txt`, and their count is in the checkpoint's note;
- a listener restart outlasted its bound. While a release restarts the listener it answers
  `503 {"error":"service starting"}`, interleaved with answers from the task it replaces. Those
  503s count toward no run of unanswered samples, inside a restart episode: it opens at a
  session's first such 503, takes every such 503 within 300 s of it, and ends at the session's
  next answered sample. The run fails when a session does not answer again within 300 s of the
  episode's start, or has a second episode within 600 s of the first. Any other error, and a 503
  with another body, counts as unanswered. Each episode's start, end and sample count is in
  `listener-restarts.json`, and the checkpoint's note names the listener release whose run spans
  them, when one does;
- any sampled topic falls outside the run. The run's topics name LEGSMOKE, its subject space `notifications.legion.legsmoke.`, its repository's GitHub subjects (`notifications.github.sjawhar.legion-smoke.`, where an agent follows its own pull request), operator-close's tree, a `legion-legsmoke-` role, or the session itself.

Four controls show the audit can fail. The verdict is given a synthetic outside issue and must
refuse it. The interest filter is given the run's samples plus one outside topic and must catch
it. The unanswered-sample rule passes two failures in a row and fails three, passes a restart
answered within its bound, and fails a 310 s restart and three 503s of another body. The
collector, on the run's real
window, is given one actor that did write outside LEGSMOKE, and must find that actor's events. An
event is dated by Dispatch's `created_at`; one without it stops the audit, never counts as older.

| checkpoint | what it holds |
| :--- | :--- |
| `prerequisites` | the tools, the restricted context and the image by digest; the run's project exists in production Dispatch, created when missing (201, or 409 `PROJECT_EXISTS` when it already exists; any other answer fails the check); the devbox `gh` acts as the proof human, `sjawhar-agent[bot]`; the lock and the two ports; nothing left in the namespace (Sandboxes, pods, PVCs, ConfigMaps) or on NATS from another run; only then does the run own the shared objects |
| `preflight` | the runtime identity is the daemon's restricted IAM role and cannot list Secrets; the Sandbox CRD and the `legion` NodePool's instance-cpu floor; the pool's room for the run's four pods at once (tree 1's root and its two children, tree 2's root), counted in issue pods of the run's `issue` reservation (`lib/stage4b-room.jq`): what each Ready, schedulable, undisrupted pool node has free beside every non-terminal pod on it, of any namespace, plus the pods the pool's cpu and memory limits still leave room for, below which the run ends BLOCKED `capacity: …`; whether the NodePool's instance-cpu and instance-memory bounds let two of the run's pods share a node, which decides what `issue-independence` asserts of their nodes; the run's Dispatch bearer reads `whoami` as an agent session, and an invalid bearer is refused 401; LEGSMOKE has no todo root; the stream carries both halves of intake; a throwaway pod on the Legion pool reaches Dispatch, the listener, the gateway and NATS, each within three tries 5 s apart (a fresh node's first outbound connection can fail while it settles), and a service that never answers fails the check with every try's error |
| `pod-watch` | the namespace snapshot; the pod, node-event and node-memory watches start, and the Secret-value check (`lib/secret-leaks.ts`). The pod and node-event watches last the whole run: kubectl's own watch ends when the API server closes it at its watch timeout, so each lists, watches from that resourceVersion, resumes from the last version it saw when a watch ends, and lists again on 410 Gone, noting each in the transcript. Each watch asks the server to end it within 300 s, so a loop a killed driver left stops within five minutes; a watch that delivered nothing is resumed after a pause, and a line that does not parse ends that watch unrecorded |
| `boot` | the build's source is the one prerequisites recorded; `legion start --check-config` passes the `runtime: kubernetes` config, whose `pod` is the operator fixture's ([`deploy/kubernetes/operator-route`](../../deploy/kubernetes/operator-route/pod.yml)) with its ConfigMap renamed to the run's copy, whose `issue_volume` is 20Gi, and whose `resources` block sets the issue pod's cpu and memory alone (`issue: { cpu: 2950m, memory: 16Gi }`, `resources_block`), its ephemeral-storage limit and request the daemon's defaults (80Gi and 6Gi), so the issue pod's reservation comes from both paths the loader has, the disk bound's included; the operator creates that ConfigMap from the fixture's `models.yml` and `overlay.yml`; the audit window opens and the interest sampler starts; the daemon boots, and the image probe passes (its first attempt's timeline is kept). The daemon's capability report (`legion state --json`'s `.capabilities`, kept as `capabilities-at-boot.json`), once the probe's verdict is in it (waited for: no image row `unchecked`): every image row (`eval-js`, `eval-python`, `browser`, `lsp`, `codegraph`, `skills`, `toolchain`) is `present`, codegraph included; every live row (`subagents`, `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) is `unchecked` with a detail starting `no session has reported yet (`, since the live rows render from the live sessions' reports (LEGION-663) and boot's own empty Postgres holds none yet — the first comes with the first admitted root's architect; and the open rows are `secrets`, and `pool-capacity` when the pool scaled for the probe (the pool's to decide, not the configuration's: the row is open when the image probe pod was Unschedulable before a node came up for it, which a run that starts on an empty pool meets), since the proof's `legion.yaml` configures no `runtime.kubernetes.agent_secrets` and decides nothing, while `model-fallback` and `resource-limits` read `present` (the fixture overlay's `retry.modelFallback` and the issue and controller pods' reservations); `pool-capacity`'s row is recorded either way. A row that reads otherwise fails by name with its status and detail |
| `deployment-pod-block` | only with `LEGION_E2E_POD_FILE` (otherwise `SKIPPED`): one root of the run's project admitted, its architect registered, and its issue pod, launched from the run's copy of the deployment's block (its sha256 noted at prerequisites), carrying the whole block ([`lib/deployment-pod-block-verdict.jq`](lib/deployment-pod-block-verdict.jq), tested by `bun test scripts/e2e/lib`): the block's ServiceAccount; every volume, of the block's kind and object name (a Secret's or ConfigMap's `items` are not compared), each projected token's audience, expiry (3600 s when the block names none) and path; every mount, with its `sub_path` and `read_only` (read-only unless the block says `false`), in the pod's one `roles` container, at the block's path, or, for a path at or under the image's home `/home/legion`, at the same path under `/var/run/legion/home`, where that container holds the home's volumes for each role's namespaces to bind back; and every env name and value on the architect's process holding the most of them, read in the architect's own namespaces, where the launcher's start command puts the block's environment, since the `roles` container's spec carries only `POD_UID`. The verdict names what is missing or wrong and never prints a value. When the block sets `KUBECONFIG`, kubectl in the architect's namespaces on that file answers `auth whoami` as `system:serviceaccount:<namespace>:<the block's ServiceAccount>`, and its `auth can-i --list` in the namespace is kept as `deployment-block-can-i.txt`, for the deployment to judge its own policy. A token is checked as carried, never as accepted by its service; the rest of `runtime.kubernetes` is the run's own and the daemon's controller pod is not launched, so a deployment pairs this with `legion start --check-config` on its whole file at the release and image it pins (the paragraph on `LEGION_E2E_POD_FILE` above). The teardown sets the root `done` once the daemon has stopped, since the live daemon sets back a status an outside session writes on a root it runs |
| `admitted-issue-cap` | the three roots: two admitted and one waiting, in rank order |
| `spec-posted` | each admitted architect, prompted by nothing but the daemon's `catch-up` notice, posts its spec and registers the gate; with `gates.design: off` the daemon moves the tree to planning |
| `advertise-host` | the daemon's API and worker-stream listeners hold every interface (`ss -Hltn`); every pod the watch has seen ready has its one `roles` container's launcher dialing the worker stream at `advertise_host` (a pod-shape rule, so the shape watcher and `pod-shape` hold every later pod to it too); each admitted root's architect registered from such a pod. The daemon's loader refuses the proof's file with `advertise_host` dropped (`bind 0.0.0.0 is not an address a pod can reach`) and with `bind: 127.0.0.1` beside it (`bind 127.0.0.1 is loopback, …`). Negative control: a recorded pod whose launcher's `--connect` names the bind address departs from the pod shape |
| `issue-independence` | while tree 1's planner holds for the driver, the driver files two children of tree 1 in Dispatch and sets them todo, with no `legion` label (the daemon enters a child of a live tree itself); each child's planner registers from an issue pod of its own, and its planner claim records tree 1 as its tree (`legion claims list --json`, the claims carrying `tree`; the state view's issue entry carries none). The root and its two children then run at once in three pods, each Running with no affinity; each pod has a disk of its own, `<pod>-issue`, Bound and owned by that pod, and the three are three; and no pod can read another issue's workspace, each way between the root's pod and child 1's (`ls` of the other's workspace path answers `No such file or directory`, while the same path exists in its own pod). Their nodes are recorded (`issue-independence-pods.txt`); two or more are asserted only when preflight read the NodePool's instance bounds as holding no two of the run's pods, otherwise the count is noted. Tree 1's planner is then told to plan. Tree 2's planner has held for the driver: a tree 2 that left planning before the driver sent it anything fails here by name. The proof's instructions tell every phase worker that the daemon's task line (`Continue <title>. Issue: <key>. Phase: <phase>.`) is not the driver's instruction. The memory hog, the pod watch's negative control, runs on the root's node and is OOMKilled by its own 64Mi limit |
| `child-release` | while tree 1 keeps running: the proof human sets child 1 `done`, and its Sandbox and pod are gone (`kubectl get sandboxes,pods -l legion.dev/issue=<child 1>` lists nothing) and so is the disk that pod owned, the daemon logged `sandbox runtime: deleted the closed issue's Sandbox and the role Secrets it owned` for it exactly once and no lost volume, every claim of child 1 is `retired` with no session, and the root's Sandbox stays Running, its pod's own disk Bound and its pod the same uid. The proof human then sets child 2 `backlog`, and within 600 s it has no Sandbox, no pod and no disk (`kubectl get sandboxes,pods -l legion.dev/issue=<child 2>` lists nothing, and `<pod>-issue` answers none): a park reaches the same durable suspend effect as a close, which deletes the Sandbox with the six role Secrets it owned once the pod is gone, so nothing of the parked child is kept and its next `todo` makes the Sandbox afresh. Each of these waits reads the disk inside it, since a pod's own claim is reaped a moment after the pod object goes |
| `child-reentered` | resume by rule, live: child 2's planner, parked by `child-release`, is `suspended` with `suspendedBy: daemon`; the proof human sets child 2 `todo` again, its planning start resumes that planner on the session it kept with no `suspendedBy`, the daemon logs `supervise: resuming by rule` for it exactly once naming the daemon's suspension, and the planner's session then holds two planning tasks, the first run's and the re-entered run's. The proof human parks child 2 again: within 900 s it has no Sandbox, no pod and no disk, and its planner is suspended by the daemon. The control, on the same claim and start path: `legion claims suspend` of the parked planner records `suspendedBy: operator`, and child 2 is set `todo` once more; its planning start waits (`outbox row waits` naming the suspension that holds until the operator resumes the claim), the planner stays suspended by the operator at the same generation for 60 s, and no `supervise: resuming by rule` line names it. `legion claims resume` relaunches it and the waiting start gives it the run's planning task, its session's third. Child 2 is parked a last time: within 900 s no Sandbox, no pod and no disk, its planner suspended by the daemon — what the session keeps is the table's, not the disk's |
| `repository-configuration` | tree 2's workspace carries the fixture (`.omp/extensions/fixture.ts` and its `AGENTS.md`); the markers each loading path writes are recorded, for `repository-tools` to judge, and the agent's argv runs with discovery on and the image's two explicit plugin roots, never `--no-extensions`. The fixture's two MCP servers are one bun stdio MCP server (`.omp/mcp-fixture.ts`: newline-delimited JSON-RPC answering `initialize`, `ping`, `tools/list` with `fixture_echo` and `tools/call`), configured under two names, which writes the marker its argument names whenever a client starts it; an MCP marker may be written by the session's own connection or by the capability probe's second client |
| `repository-tools` | what a worker in a repository checkout has, proved in tree 2's planner while it still waits (LEGION-578's integration check, criteria 1 and 2), each read from a tool's result in its session and held to the call it answered (`lib/stage4b-tools.jq`, over the session fetched once and kept as `repository-tools-session.jsonl`; the results are kept as `repository-tools-results.json`), never from the model's words. The read tool returned both skills at `skill://fixture-omp-skill` and `skill://fixture-claude-skill`, each carrying the run's nonce. The lsp tool's status lists `gopls`, `typescript-language-server` and `pyright` for the checkout, and its hovers return each root's own signature (`func Greet`, `function greet`, `def greet`). A role shim of tree 2's pod logged the CodeGraph warm-up's success line (`built the index: codegraph init took <duration>, done <duration> after launch`; one of the pod's six shims wins the warm-up lease and builds, the others skip), read from the log of the pod's one `roles` container with `kubectl logs -c roles --timestamps`, each line behind its role's `<role>: ` prefix, and kept, with every role's warm-up line, as `codegraph-warm-up.txt`; the role whose shim built and the seconds from the pod's creation to the line are recorded (`warm_up_built_elapsed`, tested by `bun test scripts/e2e/lib`); `codegraph status --json` then reads `initialized: true` and `index.state: "complete"`; the codegraph tool's affected names `greet_test.go` for `fixture/greet.go`, its impact of `Greet` names the caller (`GreetWorld` or `caller.go`), and its control, the impact of `NoSuchSymbol`, errs or names none of them. The fixture's `omp-extension`, `omp-tool`, `mcp-root` and `mcp-omp` markers are all present, and the planner's argv has no `--no-extensions`. Five settings each read twice by `omp config get` in the workspace, under the agent process's own environment (`/proc/<pid>/environ`, which carries `PI_CONFIG_FILES`) and with no environment but `PATH` and `HOME` (no overlay: Oh My Pi's own default under the repository's `.omp/config.yml`), asserting the relation between the two reads and never a literal default, since defaults move between Oh My Pi pins: `compaction.remoteEndpoint`, `memory.backend` and `dev.autoqa`, which nothing sets, read the same both ways; `images.urls.enabled` reads `true` both ways (the repository's `.omp/config.yml`); `retry.modelFallback` reads `true` under the agent's environment and the repository's `false` with none (the operator's overlay), so the overlay reaches the agent through its environment alone. Then the planner is told to plan |
| `full-agent` | every live session measured the six live capability rows (`subagents`, `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) at its boot and reported them with its ready (LEGION-663). With tree 1's and tree 2's roles live and tree 2's finished planner still the process of its first boot, `legion state --json` renders all six `present` (waited for: the newest session's report may be seconds away), each row's detail naming the pod of a live claim; tree 2's planner's claim view carries `capabilities` with all six under `ok`, `open` empty and `incarnation` equal to its locator's; the daemon log's `capabilities: session reported` line for that incarnation carries `rows` whose `mcp` detail names `fixture-omp-mcp` and `fixture-root-mcp` and whose `repository-extensions` detail names `fixture.ts`; and no `capability <live row> is open` line has been logged. The rows, the view and the report line are kept as `full-agent.json` |
| `finished-idle-planner-death` | tree 2's completed planner is idle with no pending task while its implementer owns `implementing`; the driver records its session, process incarnation and pod UID and ends only the planner's generation, once (`end_claim_process kill`: in the pod's one `roles` container, the role-init of the planner's generation is found by its argv and its worker-shim's process group gets SIGKILL; the launcher, every role's PID 1, runs on). The kill is seen landing on the planner alone: that generation's role-init leaves the container while the pod's uid, the `roles` container's id and restart count, and every other role's role-init (role, generation, pid) stay as they were, any change aborting the run by name. The daemon relaunches the planner on the same session at a new process generation, in the same pod by uid, registered and ready or idle with no task pending and no new planning-task delivery, while tree 2 remains `implementing`, unheld and with its implementer live. A relaunch registers and reaches Ready in seconds, and Ready resets the launch-failure budget, so the driver cannot exhaust it; the exhausted budget's failure, its one `worker-died` and the unheld phase are `packages/daemon/internal/daemon/outbox_lifecycle_test.go`'s (`TestAFinishedWorkerThatDiesIsRelaunchedAndItsFailureHoldsNothing`). `issue-cap-moves` then removes tree 2 normally |
| `issue-cap-moves` | the proof human's `backlog` on tree 2's live root is set back: the next status write is `legion-daemon:LEGSMOKE`'s (a control re-attributing it must fail), tree 2's architect receives a `status-reasserted` notice naming the proof human, and tree 2 keeps its slot; `legion status … backlog` then frees the slot, tree 3 is admitted, and tree 2's pods are gone |
| `tree-moved` | before tree 1's implementer completes, `legion claims suspend` suspends tree 1's architect (`suspendedBy: operator`); the implementer's completion takes tree 1 to `testing`, and its `phase-finished` notice waits for the architect (`outbox row waits`, because the architect holds no role or because the row waits behind an earlier notice held for it), resuming nothing: the architect stays suspended by the operator at the same generation for 120 s. No rule acts on a tree architect, so the rule passing over an operator's suspension is `child-reentered`'s control. `legion claims resume` then relaunches it with no `suspendedBy`, the held notice arrives in its session, and the daemon logged exactly two `api: operator request` lines for the architect, the suspend's route and then the resume's. With real agents throughout; the tester's adoption leaves a new empty change and keeps the implementer's author |
| `ci-red-takeover` | CI settling red while tree 1's tester is in a turn takes the phase back without its completion, and interrupts that turn before the implementer starts. The control: the tester runs `/usr/bin/sleep 20 && date … > /tmp/stage4b-takeover-control`, which nothing interrupts, and the file is written. The witness: the tester runs the same chain with `/usr/bin/sleep 1207` into `/tmp/stage4b-takeover-witness`, its claim `working` and the sleep in its pod's process list; the proof human commits `.fail-me` to the pull request's branch, the smoke repository's `fail-on-demand` check fails on it, and tree 1 returns to `implementing`. The tester then goes idle on its first session in its first pod, its sleep gone and the witness never written. The daemon log's lines of the takeover (kept as `ci-red-takeover-log.jsonl`) hold the tester's turn over, the start going on only after it, and the implementer's CI-red task reaching its session after it too; the tester's turn interrupted, which the log can carry or not (`takeover_interrupted` is optional since 829b4f94), must come no later than it being over when the line exists. A control that puts the task at the row's own interrupt time, or, with none logged, at the timestamp taken just before the `.fail-me` commit (edb4eb62, 80d0c82b), must fail. The implementer, told to, deletes `.fail-me`, tree 1 returns to `testing`, and the tester, handed testing again on its first session in its first pod, has still written no witness |
| `tree-reviewed` | tree 1 runs planner, implementer, tester, reviewer, the architect's acceptance and retro to merging with real agents; the tester's adoption leaves a new empty change and keeps the implementer's author. Once both thermonuclear dispatches have an outcome, the reviewer's session, its subagents' sessions and each dispatch are kept under `review-pair/`. The reviewer first submits `COMMENT` and completes without a decision: there is no approval, the issue stays in `reviewing`, and the architect receives a completion-written `review-stuck` notice naming a commit of the pull request (`notice-review-stuck.jsonl`). The driver tells the architect only to handle that notice, naming no topic, head or decision. A message in the reviewer's session must carry the architect's `reply_role` and arrive after completion; earlier round reports cannot count. These messages are kept as `architect-ask.jsonl`; an ask before the driver's steer is noted as the stronger pass. Once asked, the reviewer requests changes with exactly one inline thread and a review naming its handoff head. The implementer appends the requested line, answers the thread and resolves it with its own `gh`; the tester verifies it. Before the re-review the proof bot (the proof human, a GitHub App that is none of Legion's role Apps) opens a second thread, and the reviewer is told to reply on it and name its node id to the implementer as accepted over Envoy, which resolves it with its own `gh`. The re-review's decision is the reviewer's own; a second request for changes fails the proof. Both threads are recorded at approval (the reviewer's also after the correction; the bot's may still be open there, since the approval does not wait for the implementer's resolution). The approval moves the issue to `accepting`, read directly: no worker starts (the implementer's claim holds no retro task, `claim_pending_phase`; its state alone says nothing, since it may still be resolving the bot's thread), the daemon queues no status write and Dispatch keeps `needs_review`, and the architect holds the reviewer's `phase-finished` notice, its acceptance cue. The driver prompts the architect to accept: its `sign_off` moves the issue to `retro` (the daemon log's `workflow: phase changed … from=accepting to=retro trigger=sign_off`, `phase_change_logged`), Dispatch to `retro`, and starts the implementer's retro, which takes the issue to merging |
| `review-thread` | threads are resolved by id, by the pull request's author, on no magic word (LEGION-631): after the correction round the reviewer's thread read `isResolved: true` with the implementer's reply as its newest submitted comment (the pull request author's `resolveReviewThread`, run by the implementer's own `gh` before it completed); at the approval it still read resolved with the reviewer's own reply newest; the bot's thread, which the reviewer answered and named by node id to the implementer over Envoy (the review App cannot resolve one), reads resolved with the reviewer's reply (`bot-thread-resolved.json`; the implementer, live after its phase, resolved it with its own `gh`, so the approval need not have waited for it), and the implementer's session holds the reviewer's delivery naming the id (`bot-thread-named-to-implementer.jsonl`); `POST /legion/v1/threads/resolve` on the daemon answers 404 (`threads-resolve-404.json`): no daemon route resolves a thread (acceptance 4). Controls: the implementer's record left unresolved, and the reviewer's record with a later implementer reply appended, both fail; the bot's record left unresolved, or stripped of the reviewer's reply, fails |
| `completion-closed` | each phase worker of tree 1 — planner, implementer, tester, reviewer — stays live once its phase ends: it goes idle on the session and in the pod (by uid) it first registered with, and its live session answers every `handoff_complete` call of the legion tool, holds one success for each assignment it completed, and records the phase stall `closed` after that call, with no phase reminder after its last success: the 4b.13b acceptance's stall check (`stage3-4b13b-acceptance.sh`'s `phase-stall-follow-up`), which a stop inside the call fails (LEGION-283). The sessions and each verdict are kept as `completion-<role>.jsonl` and `completion-<role>-verdict.json`. Its control: the planner's session cut at its `handoff_complete` call, the transcript such a stop leaves, is refused |
| `resident-answer` | tree 1's finished planner, asked a question while tree 1 is in merging, answers it in the session and pod it first registered with; nothing relaunches it, and tree 1 stays in merging |
| `full-agent-tools` | the third of LEGION-663's `full-agent` family (`full-agent`: the rows reported; `full-agent-negative`: a failing report; here: the tools exercised): every role of tree 1 is a full agent on a running pod (LEGION-578's integration check, criteria 1, 3, 4 and 5), each in its own session and all at once, every answer read from a tool's result (`lib/stage4b-tools.jq`), never from the model's words. The architect, planner, implementer, tester and reviewer each ran `eval` in Python (`<nonce> py 42`) and in JavaScript (`<nonce> js 42`), drove the browser from `eval` to a page they wrote (`title: full-agent-tools <nonce>`), got an `https://` hit from `web_search`, ran the toolchain in `bash` (`go version`, `bun`, `Python 3`, `curl`, `gh version`, `git version` in one chain), edited that page (`/tmp/full-agent-tools-<role>.html`) and read it back (`edited <nonce>`), and printed their GitHub App login with plain `gh api graphql` (`viewer.login`; REST `/user` is refused for an installation token): the implement App's bot login for the implementer, the review App's for every other role (`role_app`, `lib/workflow.sh`, each login read from GitHub by `read_app_logins`), with the other App in no bash result. Each dispatched a `task` subagent, `FullAgentGh`, whose result carries the same login, and whose own session beside its parent's holds that login in a bash result of its own, no bash result with the other App, and an accepted yield. The merger wrote, edited and read its page (`/tmp/full-agent-tools-merger.html`) and printed the implement App's login, never the reviewer's App, and is left waiting for `done`'s READY. The control: `notebook`, which nothing asked for, has no result in any session. Nothing relaunched the four finished workers, idle again in their first processes, and tree 1 is still in merging. Each instruction as sent, each role's session from the delivery on (`<role>.jsonl`, fetched once and the bytes every assertion on that role judged) and each subagent session (`<role>-<id>.jsonl`) are kept under `full-agent-tools/` |
| `review-pair` | the reviewer dispatched `thermonuclear-deep-review` and `thermonuclear-code-quality` by name, and one run of each completed. A run completes by the task-result block the reviewer received, whether by async delivery, a `wait`, or a `read` of its `proc://` job, saying `completed`. With no block, the subagent's own session beside the reviewer's must end in an accepted yield. Every turn of that session runs on the fixture overlay's `review` target: the task executor runs a subagent on its parent's model, silently, when the subagent's own does not resolve. A refusal (`Unknown agent`, `No model selected`) in a task result or in a run that did not complete fails with its text. tree-reviewed keeps the reviewer's session and the subagents' sessions as the pair settles, reading the session database, not the daemon |
| `first-turns` | every role on tree 1 completed a first turn in its pod |
| `token-rotation` | a pod's projected operator token (`/var/run/operator/token`, 3600 s, renewed by the kubelet at 80 %) is renewed: the token in the file was issued (its `iat`) after the pod started, in the same pod by uid. An exec that does not answer is never a token. A model turn after the renewal still runs on a model the operator fixture's `overlay.yml` gives a role |
| `github-credential` | `gh` as the role's App inside the pod, and isolation inside one pod: in tree 1's issue pod, the implementer's `gh` is the image's wrapper `/opt/legion/bin/gh` (`legion gh`), which, handed the agent's `LEGION_GH_PATH=/usr/local/bin/gh` and `LEGION_GH_OWNERS_DIR=/var/run/legion/gh` since an exec carries none of the agent's variables, authenticates as the implement App's bot (`role_app implementer`, the login `read_app_logins` read from GitHub) from the gh files of the repository owner's directory its `GH_CONFIG_DIR` (`/var/run/legion/gh/<owner>`, the read-only `gh-implementer` projection of its role Secret, which its role-init binds there) names, and `gh auth status` there names account `x-access-token`; its Oh My Pi environment carries `GH_CONFIG_DIR`, `LEGION_GH_OWNERS_DIR`, `LEGION_GH_PATH`, an empty `GH_TOKEN`, none of the old tool pins, no grant file (`LEGION_GRANT_FILE`, `LEGION_GRANT`), no App login variable and a PATH with no `worker-bin`; a `task` subagent of that session answers the same login with no `legion` command of the parent's first, since it inherits the directory, and the implementer's own bash runs `jj undo --help` and reports jj's first help line, nothing refusing it (acceptance 4c); in the same pod the reviewer's `gh` is the review App's bot from a different `hosts.yml`, and each role's bundle root holds owners' directories and the kubelet's entries alone. Isolation in the pod's one `roles` container, each read in the role's own namespaces (entered as its agent sees the pod: `nsenter -t <its role-init> -U -m -p -i`, then its shim's `-U`): the container holds all six roles' directories under `/var/run/legion/roles` (the control that a cover, not an absent mount, hides each sibling's), while in the implementer's and in the reviewer's namespaces that directory is empty, `/proc` lists one worker-shim, its own, and the `hosts.yml` at its `GH_CONFIG_DIR` hashes to its own `/var/run/legion/roles/<role>/gh` copy in the container and differs from the other's; the pod shares no process namespace; the daemon logged `github credential written` for every role of the pod and names no token route; GitHub's record of each commit attributes the planner's handoff push to the review App's bot and the implementer's head to the implement App's; the planner's handoff-only push ends its message with two empty lines and `skip-checks: true` (the shape GitHub honours; one empty line started both workflows in the rule's first live run) and has no check run, the implementer's code head carries no CI-skipping line and has check runs, and the daemon recorded each pushed head as that role's completion (acceptance 4a); the negative control is the same `gh` with `GH_CONFIG_DIR=/nonexistent`, which fails naming `GH_TOKEN`; a seventh holder of a gh directory, the controller, both operator-launched and daemon-launched, is proven in the `controller` and `daemon-controller-liveness` rows below |
| `github-credential-refresh` | the hour boundary, on the architect (its pod the longest-lived, read first at issue-independence): the daemon's lease chain for the sandbox and role (its `written` line, then every `sandbox runtime: github credential refreshed`) is judged pair by pair — each refresh made with the review App, at least fifteen minutes before the expiry of the lease it replaced, for a lease expiring later — however many cycles have elapsed by the time the checkpoint runs; a refresh after the read at issue-independence is awaited, in the same pod by uid, and the architect's `hosts.yml` then changed in place within the kubelet's sync (the propagation delay is noted); its plain `gh` authenticates as the review App's bot on the new file; once the lease the read belonged to has expired, the token read then answers `401` while the refreshed one answers the review App's bot (both presented raw to the real gh, mise's, past the devbox's gh wrapper, which refuses an inherited `GH_TOKEN`; every other gh call of the proof is the proof human's); the tokens live in the proof's shell alone |
| `idle-resident` | every finished worker of tree 1 (planner, implementer, tester, reviewer) still runs in the pod it first registered with, that pod Running; the root's one Sandbox is Running and its pod's own disk Bound; child 2, parked in backlog, has no Sandbox and no disk, and so no pod; and child 1, done, has neither a Sandbox nor a disk — each disk read by its pod's name, since an issue's disk is its pod's own ephemeral volume and carries no label |
| `kill-role-resume` | once the merger runs, the driver ends only its generation (`end_claim_process kill`, as `finished-idle-planner-death` does; the launcher, every role's PID 1, is never killed), and the daemon resumes the merger's session at a new process generation in the same pod; the `roles` container's id and restart count stay as they were, and so does every other live claim of the pod (its token, generation and incarnation, from `legion claims`) |
| `fence` | a whole-pod deletion is never adopted. The merger resumes in a new pod UID; its former private boot token is refused, and the daemon logs `worker-stream: rejected hello (stale worker generation)` |
| `daemon-relaunch-count` | the daemon resumed the merger after both the end of its generation and the whole-pod replacement |
| `restart-mid-tree` | a daemon restart at the same addresses re-adopts the merger's pod and session, and relaunches no role: 75 s after the merger is re-adopted (past the probe interval in which every role, once its launcher has redialled, is judged on its Sandbox's address record), every claim that ran a process and still runs one has its generation and incarnation unchanged, its issue pod's uid as the operator reads it is the one `legion claims` records, tree 1's claims all still run, and the daemon logged no stale-address replacement. It is the two address-moved checkpoints' negative control |
| `address-moved-env-same-pod` | the daemon restarts handing `envoy_url` with its host upper-cased, the same Envoy listener, and nothing else moved. Each of tree 1's claims that ran a process runs exactly its next generation (one launch, none refused), registered and ready again, **in the same issue pod** (its uid unchanged), whose launcher still dials the unmoved worker stream for each of its roles; the generation's Oh My Pi is told the new `ENVOY_URL` spelling and the run's other services, and runs `--resume=` the claim's unchanged session file; `budgets.launchFailures` is 0 and its deaths unchanged; and the daemon logged one stale-address replacement of it, whose detail names `ENVOY_URL` alone (compared, never printed, since it names a production host), and no failed launch. Every other claim leaves the generation it ran, no claim leaves its issue pod, and from one read of `legion claims` every claim that runs a process is on the addresses the daemon hands now. The respelling stays for the rest of the run, and `scrub` hides the Envoy host in either case |
| `address-moved-stream-new-pod` | the daemon restarts with its API and worker-stream ports swapped (13373 and 13372, the rigs' pair), so the pod's launcher dials, for every role, a worker stream, and every role generation was told a `LEGION_DAEMON_URL`, the daemon no longer serves. Each of tree 1's claims that ran a process runs exactly its next generation, registered and ready again, **in a new issue pod**, one per issue shared by all its roles, whose launcher dials the new stream for each of them; the generation is told the new API and the run's services and resumes the claim's unchanged session file; `budgets.launchFailures` is 0 and its deaths unchanged; and the daemon logged one stale-address replacement of it, whose detail names `--connect` alone, from the old stream to the new, and no failed launch. Every other claim leaves the incarnation it ran, and from one read of `legion claims` every claim that runs a process is on the new addresses. `record_stream`'s last two records are the old stream then the new, the shape watcher has checked every pod the claims run in since the move, and every pod the watch has seen ready dials the stream served when it was created. The ports stay swapped for the rest of the run |
| `controller` | `legion controller start` registers with the Sandbox daemon, and the controller's first turn starts itself: its session's first user message is the start message the pi-legion extension sends once its claim succeeds (LEGION_CONTROLLER_START_MESSAGE, cmd/legion/controller.go), before it opens the live wake subscription a tick could race it on, and the model answers it, with nothing typed into its pane. Tree 3's planner is told to plan, and each launch of its implementer is killed once its agent is ready or in a turn with its task outstanding, so every death is charged whatever a relaunch's boot takes; the proof's instructions have an implementer run `sleep 300` on its task line (and on a new task after a missed end) before it replies WAITING, so the end lands inside the turn even when the kill takes a minute on a loaded host, where a resumed session's short turn would otherwise end first and the daemon would rightly charge nothing. A kill that lands after the task's turn has ended is charged nothing, since the turn's end retired the task: the driver reads each death's charge from the daemon log, gives such a relaunch a new task with `legion claims deliver` (a Dispatch message sets no delivery of the daemon's), and kills again, at most twice per hold loop (`hold_rearm_limit`), failing on a third miss; a miss counts toward no hold ([`lib/stage4b-hold.test.ts`](lib/stage4b-hold.test.ts)). Tree 3 is held by one of its implementer claim's budgets, `launch failures ran out` or `deaths with work outstanding ran out`, and any other hold fails. The reason must be the budget the daemon's own counters show at the bound (its `supervise: claim failed` line) and the one its implementer's last relaunch death leads to: charged as a death with work outstanding, or not. A death charged for a relaunch that never registered fails, since it could have had no work. The transcript names each relaunch's end, registration, death and charge. The notice reaching the controller is the checkpoint's point; the budgets' own rules are held by `packages/daemon/internal/supervise/budgets_test.go`: tree 3's held notice reaches the controller's session, which is under the run's own home, and `~/.omp/profiles` holds none of the controller's profile ([`lib/omp-home.sh`](#libomp-homesh)). The daemon's tick (`controller_wake_interval_seconds: 60`) reaches that session too, and every tick delivery in it so far — this window is before `full-agent-negative`, the first checkpoint that opens a live row — carries the daemon's capability report as its payload's `openCapabilities`, rendered by pi-envoy into the card's `message` block (`openCapabilities[1]: secrets`; [`lib/controller-ticks.jq`](lib/controller-ticks.jq)), and each tick's list is judged on its own: as a set it must be `["secrets"]` or `["secrets","pool-capacity"]`, the two lists the proof's deployment can leave open. Each tick alone, since every `legion start` between `boot` and here re-probes the image and `pool-capacity` is that probe's own verdict, so one session's ticks can carry either; any other name, a live row included, fails with the tick quoted. The ticks are kept as `controller-ticks.json`, and `boot`'s set is in the note for context; the daemon that reported the gap keeps running, which every later checkpoint shows. Tree 3 stays held, its planner live, for `full-agent-negative`; `controller-walk` takes it out. The operator-launched controller, started with `LEGION_GH_PATH` at mise's `gh` (the devbox's `gh` wrapper replaces `GH_CONFIG_DIR` in an agent session), holds its gh bundle under `$work/controller-state/gh`; read from the repository owner's directory there with mise's `gh`, it answers tree 1's pull request's state (`OPEN`, `MERGED` or `CLOSED`) and the review App's bot as viewer, with `GH_CONFIG_DIR=/nonexistent` failing, and its stderr names the review App and the bundle's owners, and `github-credential.log` exists |
| `full-agent-negative` | a live row a session's check fails reads `open`, named by the session's pod, until that process is gone, and the daemon runs the session all the same (dispatch://LEGION-663: run, and report). Tree 3, held, is the one live tree without a pull request while a controller runs: the driver writes `.omp/config.yml` (`web_search: enabled: false`) in tree 3's workspace in the planner's namespaces, ends the planner's generation once (`end_claim_process kill`, as `finished-idle-planner-death` does), and waits for the relaunch to be ready or idle at a new incarnation with its claim view's `capabilities.incarnation` equal to its locator's. Then the `web-search` row of `legion state --json` is `open` with one entry of its detail (the daemon joins failing sessions with `; `) reading `pod <sandbox>/roles (planner, <tree 3>): web_search is not a registered tool` (an issue pod's roles all run in its one `roles` container) (`web_search_open_for`, run on the row the daemon's renderer writes in `lib/stage4b-capabilities.test.ts`); the view lists `web-search` under `capabilities.open`; the daemon log holds a `capability web-search is open` warning carrying the new incarnation; the controller's session receives a tick whose `openCapabilities` names `web-search`; and the planner, so reported, answers a nonce. The file removed and the planner's generation ended once more, the relaunch's report reads `web-search` `present` again, and the view lists it under `capabilities.ok`. The open and healed rows and views are kept as `full-agent-negative.json`; the teardown removes the file, best effort, should the run end while it is in place |
| `controller-walk` | `legion status … backlog` from the operator shell moves tree 3, Dispatch shows it, and tree 3's pods are gone. With tree 3 out and a slot free, the controller's slot wakes reach it: its Oh My Pi was launched with the daemon's ``Design gate policy: `gates.design: off`.`` line; an unlabelled issue set to `todo` puts `todo on <KEY>` in its session, and the daemon's tick (`controller_wake_interval_seconds: 60`) `tick on LEGSMOKE`; a minute later that issue is still unlabelled and unrecorded, since the controller starts no work a person did not hand over (the proof's scope says so too), and it is set `done`; tree 3's root issue is claimed by its architect's session, and its events carry the `issue.claimed` |
| `daily-report` | the controller's daily report, which the proof's instructions exempt from their wait for a targeted message and fit to the run: an issue titled `Legion daily report (<run directory>)` appears in LEGSMOKE, parked in icebox and without the `legion` label, holding the controller session's message, which names tree 1 and the free slots within 2,000 characters; in the controller's session, the call that posted it comes on a turn a `tick on LEGSMOKE` delivery started once the start turn had ended, never in its start turn. That call is the controller's first `dispatch message --issue <report>` run through bash, made any way Oh My Pi offers: the `bash` tool, a write to its `xd://bash` device, or `eval` code calling `tool.bash(...)` ([`lib/omp-tool-calls.jq`](lib/omp-tool-calls.jq), which `stage3-4b13b-acceptance.sh`'s merger check reads too; `lib/report-after-tick.test.ts`). `production-audit` then holds that the issue, like every write, is in LEGSMOKE |
| `daemon-controller-liveness` | under `controller: daemon` the daemon's liveness sweep writes the two lines a log monitor on the daemon counts, with the text and on the cadence it writes them under `controller: operator` ([`lib/controller-liveness-verdict.jq`](lib/controller-liveness-verdict.jq), tested by `bun test scripts/e2e/lib`, which also holds the daemon's source to both texts and the machine's to its launch lines). The operator's controller quits, and the listener answers 404 for the controller role. The daemon restarts with `controller: daemon` and `runtime.kubernetes.resources.controller` sized to run: the controller it launches registers, holds the role and is ready, `controllerLocator` names its session, and for 270 s (two boot timeouts and 30 s) the daemon logs neither line, the negative control (`controller-liveness-up.json`); its gh, from the repository owner's directory of the pod's gh bundle `/var/run/legion/gh`, answers tree 1's pull request's state and the review App's bot as viewer, that directory holds `config.yml` and `hosts.yml` alone and the bundle's root the kubelet's `..data` (a `subPath` mount, which never refreshes, has none) and, beside the owners' directories, only the kubelet's other atomic-writer entries, and the real gh with `GH_CONFIG_DIR=/nonexistent` fails. It restarts with the controller's CPU request at 100000, more than any node holds, re-adopts the running controller, and the driver deletes its pod. The relaunch's Running patch makes a pod whose role launcher never connects, so each launch onto it fails at the boot timeout (`supervise: launch failed`) with no process recorded. The daemon logs `controller liveness: the controller role has no live holder; the controller is gone` on at least 3 sweeps, naming the dead session, and `controller not registered; run legion controller start only under controller: operator; …` at least twice, 120 s or more apart, with `mode: daemon`, `registered: true` and a claim state that is not a live controller's, one of them after the first failed launch. The controller's claim launches on no pod but the deleted one (a relaunch can catch that pod while it terminates, return it and see it die), and the pod watch shows each pod requesting 100000 CPU, the pods the relaunch made, Unschedulable and never scheduled; the deleted pod and the one the Agent Sandbox controller recreates from the old template request 250m and are not judged (`controller-liveness-down.json`). It restarts with `controller: operator`: the boot retires the controller's claim, its Sandbox and volume go, its registration ends (`controllerLocator` empty), and the sweep logs `controller not registered; run legion controller start`, exactly, with `mode: operator` (`controller-liveness-operator.json`). The rest of the run keeps `controller: operator` with no controller registered |
| `deaths-with-work` | tree 4, admitted once tree 3 has left: its planner, killed once mid-turn, is sent its task again, told the turn was interrupted, and finishes planning; its implementer, killed after each ready with its task outstanding, is failed after 3 deaths in a row (`budgets.deaths` 3, `supervise: claim failed` because "deaths with work outstanding ran out"), tree 4 is held and nothing relaunches it; `legion status … backlog` then takes tree 4 out. A kill of either that lands after its task's turn has ended is given a new task and repeated, as in `controller`, and restarts the implementer's run of deaths |
| `children-at-the-merge` | the driver files children 3 and 4 under tree 1 and sets each todo, with no `legion` label; each one's planner registers from an issue pod of its own and holds in planning, its planner claim records tree 1 (`legion claims list --json`), the state view shows it at generation 1 in planning, and its pod's own disk (`<pod>-issue`, owned by the pod) is Bound. They are filed after every checkpoint that judges each live claim of tree 1 (the restart and both address moves), and once child 1 is released and child 2 parked, so they raise no peak the room check sizes; they run beside the root, no more of tree 1's pods at once than `issue-independence`'s |
| `done` | before the merger's READY, since the daemon's merge of the READY head can follow it by seconds: the smoke main's lock is taken (`hold_smoke_main`). Then the merger's READY and the daemon's merge of its head — the proof only reads the pull request until it merges (`await_ready_merge`) and never merges it by hand; the note names the merge commit. **The merge does not close tree 1**, since children 3 and 4 are still in the workflow (INFRA-229): the daemon records it (`legion state`: the root's `pullRequest.state` `merged` while its phase stays `awaiting_merge`) and logs `workflow: a merged root's close waits for the rest of its tree` with `waitingFor` naming both children in order; its architect gets one `pr-merged` notice naming the pull request and both; the admission slot is still the root's, no production check is open, the Dispatch board still reads `retro`, no `workflow: tree lingers` line names tree 1, and no claim of tree 1 is suspended. The proof human then parks the children one at a time (`backlog`, each leaving the workflow with its planner suspended, and within 600 s with no Sandbox, no pod and no disk: the park deletes the child's Sandbox as a close does, and the disk goes with the pod, read inside the same wait, `child_parked`): child 3's park leaves the root in `awaiting_merge`, and child 4's — the last member of the tree to leave — closes it, as the merge of a finished tree's root does. `awaiting_merge` → `done` within 900 s (the daemon log's `workflow: phase changed … from=awaiting_merge to=done trigger=pull_request_merged` and, once, `workflow: a merged root closes once its last child left the workflow`; Dispatch `done` within 120 s), the production check opens on the merge commit at its first look, woken by the merge (`check_opened_on`: `looks: 1`, `lookOpen: true`, `lastWake.kind: merge`), and the close hands the implementer that look in the still-live pod. The proof sends it nothing there: the issue is closed, and a closed issue takes no Dispatch write (409 `ISSUE_CLOSED`), so the fixture's instructions say a check task waits for no targeted message and acts on the daemon's task as written — this proof has no production and no deploy role, so the implementer leaves `Production: pending deploy of <merge commit>` on the pull request and reports `pending` (`check_look_reported`, within 600 s of the look's start: `looks: 1, lookOpen: false`, the check still open); the smoke main's cleanup (`clean_smoke_main`) runs while it works. The pending report suspends the implementer's claim, its session kept in the session database, and deletes the root's Sandbox as any stop of an issue does: within 600 s the root has no Sandbox, no pod and no disk (`kubectl get sandboxes,pods -l legion.dev/issue=<tree 1>` lists nothing, and the disk that pod owned answers none, read inside the same wait); `legion claims list --json` then shows all six claims `suspended` (`claim_states_are`). The close suspends every role of tree 1, architect to merger, within 1800 s, since the daemon's suspension of a role in a turn waits for that turn to end, and a wait that runs out names each role still unsuspended with whether its suspension is held for a turn; tree 1's events carry its architect's `issue.claimed`, and done leaves its root issue unclaimed. The READY packet names the pull request's head, and `reviewThreads` shows every thread resolved, the reviewer's and the bot's included (`review-threads-at-ready.json`): the end state the implementer's `gh` left, its own threads and the bot's the reviewer named to it, since the merger resolves nothing and READY reads no thread state. Control: the same record with the reviewer's thread unresolved fails |
| `production-check-bound` | the check's bound (`production_check_hours: 0.25`, 15 minutes after the close) wakes its last look: `last_look_started` reads `looks: 2`, `lookOpen: true`, `lastWake.kind: bound`, `last: true` (or the check already closed by an implementer that reported it first) within 900 s of the checkpoint's start (what remains of the 15 minutes after `done`; a bound deferred by a look still open when it came due, by the look's own two-hour bound, fails here by name rather than hanging), the root's Sandbox, made afresh by the wake, reads `Running` and a pod of tree 1 is back within 900 s (the relaunch took 1 to 3 minutes in acceptance runs), and the implementer is resumed on its kept session, in a pod whose disk is that pod's own. Its task names the last look and its rule (the proof sends the closed issue nothing), so it writes on the pull request that the check did not run because production did not carry the merge within the bound and reports `pending`: the check closes as `not_run` within 600 s (`check_closed`: the state omits `check`, the issue `done`; `check_look_logged` finds `workflow: production check look started` with `look: 2`, `cause: bound`, `last: true` and `check_closed_as` finds `workflow: production check closed` with `outcome: not_run` in the daemon log), the implementer's claim is `retired` beside the five `suspended` claims (`claim_states_are`), and within 600 s the root again has no Sandbox, no pod and no disk, the disk read inside the same wait. It runs before `node-release`, which needs tree 1's pods gone for good, and before the tree's close at its linger expiry, which waits while a member's check is open |
| `node-release` | tree 1 lingers (`linger_hours: 0.5`, from the close its last child's park brought on): the root and its parked children 2, 3 and 4 each have no Sandbox and no disk, and so no pod, read first — each stop deleted the member's Sandbox once its pod was gone, the children's at their parks and the root's at its production check's close; then every node any pod of tree 1 ran on (read from the pod watch: the root's, the children's, the replacements the fence and the stream move made, the pod the production check's last look brought back) is released — the node is gone after the pool's consolidation, or, when another pod keeps it, Pending or Running, the node stays carrying no pod of tree 1: a pod of another project (a production daemon running beside the run), the run's repository cache (`legion.dev/cache=true`, no tree's, alive as long as the daemon), or a pod of another tree of the run; a pod of tree 1, or of the run with no tree label other than the cache, holds the wait, one that has Succeeded or Failed counts for nothing, and a pod with no project label is no Legion pod (`node_pods_judged`, lib's `node-released.test.ts`). The note says which it saw where, and a timeout lists what the node still held with each pod's project and tree label. The linger's expiry is read from the daemon's `workflow: tree lingers … until` line for tree 1: while it still runs after the waits, the four are read again with no Sandbox and no disk; once it has passed, what is left of tree 1 is the close's to judge (`close`), and the note says so |
| `close` | at linger expiry (30 minutes from the close that followed tree 1's merge; the close waits while a member's production check is open, which `production-check-bound` saw closed) no Sandbox or pod carries tree 1's label any more, and none of the pods of the root and children 2, 3 and 4 has a disk. The cleanup finds little left to delete: each member's own stop already took its Sandbox, and the disk its pod owned went with that pod — child 1's at its `done`, child 2's at its parks, those of children 3 and 4 at the parks that closed the root, the root's at the linger's suspension and again when its production check closed. Every member of the tree left the workflow before the close, so none is handed over |
| `re-admission` | tree 1 set todo again: its architect comes back in a pod of its own, on that pod's own disk, Bound and owned by it, resumed from the session the database kept. Nothing reports a lost volume — the daemon's log holds no `volume was lost` line anywhere in the run, that verdict having gone with the lost-disk machinery — and the architect's workspace holds no `.legion/<tree 1>/workspace-recovered.json`, the marker having gone with it; what tells the agent its workspace is new is the launcher's own `LEGION_WORKSPACE_RECREATED` |
| `operator-close` | `legion claims close` on the Sandbox runtime: the close of re-admitted tree 1's live root is refused 409, and its claims, Sandboxes, pods and those pods' disks are unchanged; `legion status … backlog` then takes tree 1 out; an operator-spawned tree closes with its worker live, the root and the worker are retired, and the tree's Sandboxes, pods and the disk of each of those pods are gone |
| `pod-shape` | every Sandbox pod whose one `roles` container the pod watch saw ready is judged from the spec the watch recorded for it, deleted pods included: gVisor, the operator's ServiceAccount and one projected token, the run's route ConfigMap mounted as every role's profile `models.yml`, the pool, Pod Security restricted, its launcher's `--connect` naming `advertise_host` at the worker-stream port the daemon served when the pod was created (each daemon's from its start, in `worker-streams.jsonl`; a move waits out the second the stopped daemon could have created a pod in, since a pod's `creationTimestamp` counts whole seconds; the run assumes, without checking, that the devbox's clock, which dates each start, and the API server's agree within that second, and that by then the Agent Sandbox controller, which creates each pod after the daemon writes its Sandbox, has created the pod of every Sandbox the stopped daemon wrote), six role-private Secrets, split provisioning, and the run's reservations: every container, the init containers included, carries cpu and memory as both request and limit and ephemeral-storage as a request no greater than its limit (the node-disk bound between pods of unrelated trees), all four the issue pod's one reservation: the `boot` row's `issue: { cpu: 2950m, memory: 16Gi }` override, the ephemeral-storage limit and request the daemon's defaults; each pod is `Guaranteed`, and none carries an affinity (`lib/stage4b-pods.jq` `reservation_problems`). No pod command, args or environment carries a value any provisioning or role-private Secret held at any point in the run (`lib/secret-leaks.ts`), and every such Secret was seen. The watch is complete: every pod UID the shape watcher read, the driver ended, or the daemon launched is in it. Negative controls: a recorded pod with another runtime class, one whose `roles` container is limited past its request and reads `Burstable`, one pinned to its tree's node by a pod affinity, and a pod the watch never recorded |
| `pod-watch-verdict` | the pod watch is complete (as at `pod-shape`); no pod of the run was Evicted or had a container OOMKilled, and every claim process the daemon found dead (`supervise: process died`) was one the driver ended — there is no lost-volume exception any more, since no launch of this release reports one. The memory hog was OOMKilled. Synthetic OOMKilled and process-died controls both fail |
| `hygiene` | the daemon stopped, the teardown ran, and the run's consumers are gone; `namespace-clean` follows it |
| `namespace-clean` | the run's own Sandboxes, Secrets, PVCs, pods and ConfigMaps are exactly the snapshot taken before the run: those labelled with its project, and those with no project label whose name carries the run's label, as written or as a claim token carries it, which the teardown's label selector would leave behind. Another system's objects in the shared namespace, labelled or not, are not the run's and are not compared |
| `production-audit` | no write by the run outside LEGSMOKE and no interest outside it; the verdict refuses a synthetic outside issue, and the collector finds a real outside writer's events |

## controller-start-tmux.sh

Task 4b.5's acceptance for `legion controller start`, on tmux. The Go daemon runs on a real Postgres
with this checkout's two plugins in an isolated OMP profile, against a real Envoy listener and NATS, and
the command runs in real tmux panes, as an operator would. The pinned Oh My Pi is launched through
mise. The run needs no model route: the controller's one model turn, which the pi-legion
extension starts from LEGION_CONTROLLER_START_MESSAGE once its claim succeeds, fails against the
profile's default, a static-key provider that listens nowhere, and no check reads its answer. The
profile names the roles the task agents use and that default, which is all the boot gate's
agent-model check resolves.

```bash
bash scripts/e2e/controller-start-tmux.sh
```

Each check prints `== <name>`, what it observed, and `ok <name>`. The first check that fails ends the
run non-zero and names itself. On any exit the run removes its scratch directory, the isolated
profile, both tmux servers, and its Postgres and NATS containers, also when whoever reads the run's
output goes first and a stop follows: the run's output goes through
[`lib/transcript.sh`](#libtranscriptsh)'s `tee`, which outlives the reader.
`CONTROLLER_START_EVIDENCE_DIR` keeps `transcript.log` (the whole run), the daemon and listener
logs and, under `checks/`, each check's own output (the controllers' stderr and exit codes, the
refusal, the route answers, the prober's log) and both controller panes as they were at exit, so a
failed run keeps what failed; it defaults to a fresh `/tmp` directory, which is printed. No secret
is written there.

| check | what it holds, and 4b.5's acceptance item |
| :--- | :--- |
| `check-config-passes` | `legion start --check-config` passes the run's tmux config: its first line is the `Config OK` line, and the lines after it are exactly the two deployment capabilities a tmux file alone leaves open (`capability secrets is open: …` then `capability resource-limits is open: …`, each ending with its `capabilities.decided.<name>: "<reason>"` line), a report that keeps the exit 0 (LEGION-578). 4b.5's "`--check-config` passes on the proof's config" is the full-tree driver's, whose config is `runtime: kubernetes`: `stage4b-sandbox-tree.sh`'s `boot` checkpoint runs it before the daemon starts |
| `check-config-names-each-broken-key` | an unknown key, a zero `admission_cap`, a bad `envoy_url`, and a Dispatch URL without its token file are each refused by name, and neither App's `private_key_command` runs (names the key on each broken variant) |
| `gate-refuses-the-previous-contract` | the boot gate refuses the plugin with its manifest set to the contract before the checkout's, naming both contracts (the plugin at the bumped contract refuses the previous one by name) |
| `daemon-serves` | the daemon answers `/healthz` with no `controllerLocator` yet |
| `state-config-runs-no-key-command` | `legion state --config` on a config whose key command would fail still reads the state, and the command never runs (`legion state --config` runs no key command) |
| `operator-token-file-others-can-read-is-refused` | a 0640 operator token file is refused by path and mode, the daemon mints nothing, and no state directory is written (a group-readable operator token file is refused naming it) |
| `controller-claims-the-role` | the controller registers, `GET /legion/v1/state` shows `controllerLocator` (`runtime: tmux`, `external: true`), the Envoy role `legion-<project>-controller` names its session, and its Oh My Pi has the controller environment, its secret only as a 0600 file, interactive (claims the controller role, shows `controllerLocator`); with no GitHub App configured, its stderr says the controller's gh acts as nobody, and `$ctl_state/gh` is an empty 0700 directory with no credential log |
| `ctrl-c-reaches-omp-not-the-cli` | one Ctrl-C leaves both `legion controller start` and Oh My Pi running (decision 3: Oh My Pi owns the terminal) |
| `status-from-an-operator-shell` | `legion status --operator-token-file` mints a grant with the operator bearer and reaches the status route; the rig has no Dispatch, so the route answers 500 naming it. A wrong bearer is refused 403 (decision 3's operator path; the status write itself is the full-tree proof's) |
| `second-start-revokes-the-first` | a second start takes the role and the locator, the daemon logs mints 1 and 2, the first capability's registration is refused 403, and a grant minted before the second start, which redeemed then (500 `DISPATCH_UNAVAILABLE`, past the grant check), is refused 403 `GRANT_UNAVAILABLE` after it (a second start revokes the first's capability) |
| `liveness-probe-against-the-listener` | `controller.Prober` on the live listener calls the second session alive and the first gone |
| `exit-code-is-oh-my-pis` | Ctrl-D quits Oh My Pi cleanly and the command exits 0, as Oh My Pi did. A non-zero code is carried through too; the stub-omp unit test (`cmd/legion/controller_test.go`, exit 3) holds that |

## dispatch-user-turns.sh

LEGION-394's acceptance: a person's direct Send or Aside from Dispatch's conversation page is the
session's own user turn, and everything else keeps its Envoy card. One real session — the pinned
Oh My Pi (`.omp-pin`, run as a mise tool spec) with this checkout's Envoy plugin in an isolated
profile, launched with `controller-start-tmux.sh`'s `operator_env` line and `OMP_SKIP_SETUP=1` (a
fresh profile's first launch otherwise sits in the first-run setup wizard), its cwd under `/tmp` —
registers with a real Envoy listener and NATS. Dispatch, built from the checkout with NATS on and
its trusted identity header, serves the SPA this checkout builds, and Playwright drives the
conversation page as the person the header names. The session's model turns go through the model
gateway on the operator's own hawk login ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)).

The rig serves the checkout's built `packages/dispatch/web/dist` live. Do not run `bun run build:web`
or `bun run e2e` in this checkout while a run is live: either rewrites that directory under the
Dispatch process and can leave its page with stale asset names.
The profile also holds what the slash-command checks send, each named with the run's token: a
skill (`skills/<name>/SKILL.md`), a prompt template (`prompts/<name>.md`), a file command
(`commands/<name>.md`), an extension (`extensions/<name>.ts`) registering a command that writes its
arguments to `checks/extension-command-args` and appending the role, `customType` and `tag` of every
`message_start` the host hands extensions to `checks/message-starts.jsonl`, and
`compaction.keepRecentTokens: 200` appended to the `config.yml` the model route wrote, so the run's
one `/compact` has history to summarize. Each slash-command check waits until the session is idle
before it sends, since the page refuses a send while a turn runs: a stopped answer with no message
or custom message after it and no background job the model started still owed its result.

```bash
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/dispatch-user-turns.sh     # → "dispatch user turns e2e: PASS", exit 0
```

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_MODEL_GATEWAY_URL` | required | the model gateway's Anthropic endpoint; checked by [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh) |
| `DISPATCH_USER_TURNS_OMP` | the pinned build, `.omp-pin`'s mise tool spec (`github:sjawhar/oh-my-pi@<version>`) | another Oh My Pi, as a mise tool spec |
| `DISPATCH_USER_TURNS_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e-user-turns-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `logs/` (the listener, Dispatch and the SPA build), `checks/` (the page's screenshots, the session's pane at exit, `extension-command-args` and `message-starts.jsonl`) and `session.jsonl`, the session's transcript |

Each check prints `== <name>`, what it observed, and `ok <name>`. The first check that fails ends the
run non-zero and names itself. On any exit the run removes its scratch directory, the isolated
profile under the HOME it gives Oh My Pi, its tmux server, its listener and Dispatch, and its
Postgres and NATS containers; a run that finds another's leftovers refuses to start
([`lib/leftovers.sh`](#libleftoverssh)).

| check | what it holds |
| :--- | :--- |
| `session-registers` | the session registers with the listener from its cwd and advertises `aside`, `btw` and `steer` |
| `send-is-the-persons-own-turn` | the page's composer opens on Send and offers Send, Aside and BTW; a Send typed there is one user message in the session's transcript that is exactly the body, and no card; the session answers it |
| `aside-is-the-persons-own-turn` | the same for an Aside |
| `btw-is-a-side-question` | a BTW is answered in Dispatch and is no user message |
| `a-session-forging-a-person-gets-a-card` | holding the listener token and a Dispatch bearer, as any session does, the driver stores a session's message on an issue and publishes a frame naming it that claims a person wrote it, on no issue: a card, no user message |
| `a-broadcast-gets-a-card` | a person's broadcast to the session: a card, no user message |
| `an-issue-message-gets-a-card` | a person's issue message targeted at the session: a card, no user message |
| `a-legion-notice-gets-a-card` | the Go daemon's `phase-finished` notice on a role topic the session holds: a card, no user message |
| `a-session-re-sending-a-persons-btw-gets-a-card` | as any bearer may, the driver retries the person's BTW to the session as a steer (`POST /api/v1/messages/{id}/deliveries` with a session actor), an attempt whose `requested_by` is that session: it arrives as a card, no user message (the refusal itself is `message_accept_test.go`'s) |
| `a-carded-send-and-a-frame-forged-for-it-inside-the-minute-get-cards` | with the session's Dispatch token file made wrong, the person's Send arrives as a card, since the session cannot accept it; with the token restored, a frame forged with the listener token names that attempt within 45 s of the Send, while Dispatch would still accept it: the session's own record of the attempts it delivered keeps it a card, with neither the Send's text nor the forged text a user message, and Dispatch records no acceptance |
| `a-persons-retry-of-a-carded-send-is-their-turn` | the person retries that Send as an Aside (`POST /api/v1/messages/{id}/deliveries`, attempt 2), an attempt the session never delivered: one user message that is exactly the body, and Dispatch records attempt 2 accepted |
| `the-composer-lists-the-sessions-commands` | typed key by key in the composer, `/rig` lists the rig's template, file command and extension command, `/skill:rig` its skill, and `/sess`, `/comp` and `/new` the built-ins `session`, `compact` and `new`, `new` alone marked terminal only; nothing is sent |
| `a-forged-command-list-changes-nothing-on-the-page` | with a page open, a bare bus client publishes five malformed `commands` frames on `agentstream.<session>.frames`, each at a sequence number far above the session's own (an unknown `source`, `commands` not a list, an empty name, a whitespace name, `v` 2): the page's list stays the session's and no error screen shows; a well-formed list published after them replaces it, so the frames reached the page; a fresh page lists the session's own again |
| `a-skill-runs-as-the-skill` | `/skill:<name> …` picked from the list and sent: one `skill-prompt` custom message tagged with the Dispatch message id, one `message_start` for it that the rig's extension saw (the host event Legion's phase-stall check reads), no user message holding the raw text, and the session answers with the skill's code word |
| `a-template-and-a-file-command-run-as-their-expansions` | each picked and sent with an argument: one user message tagged with its Dispatch id, holding the expansion with the argument substituted rather than the raw `/name`, and the session answers it |
| `an-extension-command-runs-its-handler` | the extension's command picked and sent with arguments: its handler wrote exactly those arguments to `checks/extension-command-args`, and no user message was submitted |
| `session-replies-with-its-id` | `/session`: the first line of the session's Dispatch reply to it is `Session: <session id>` |
| `compact-compacts-and-replies` | `/compact`, sent once, after the checks above gave the session history: the reply starts `Compaction complete`, and the transcript gains a `compaction` entry |
| `new-is-terminal-only` | `/new`: the reply says it runs only in the session's own terminal and nothing was sent, and the listener lists the same session with the same user messages |
| `an-unknown-command-is-the-persons-words` | a Send starting `/foo<run>`, which no command names: one user message, the body alone, tagged with its Dispatch id, and the session answers it |
| `the-send-and-aside-carry-their-dispatch-ids` | the Send's and the Aside's user messages each carry their Dispatch message id as their `tag` in the session's transcript |
| `the-page-shows-each-message-once` | a fresh page, opened while the session's stream still holds the turns it tagged (the ring lives in the process, so the restart below empties it): the replay the page is served carries the Send, the Aside and the retried Send as user messages tagged with their Dispatch message ids, beside their stored copies, and the page shows each person's message once |
| `the-session-stops` | the Send's own envelope is read back from the notification stream; Oh My Pi is killed and the listener drops the session |
| `a-send-while-the-session-is-down-fails` | the person sends the stopped session a Send, which Dispatch records as one failed attempt naming it |
| `a-replay-after-restart-gets-a-card` | once Oh My Pi is continued (`--continue`, the same session), the Send's own envelope is sent again: a card, and the Send is still one user message |
| `a-frame-forged-after-the-restart-naming-an-old-send-gets-a-card` | once the Send made while the session was down, which Dispatch recorded as failed, is more than a minute old, a frame forged with the listener token names it as a person's steer: it arrives as a card, with neither its stored nor its forged text a user message |
| `a-frame-forged-for-a-failed-send-inside-the-minute-gets-a-card` | the listener drops the session's registration (`DELETE /v1/sessions/{id}`), so the person's Send to it fails with no frame sent, and within 45 s a bare bus client publishes a frame naming that failed attempt straight onto the session's agent subject: it arrives as a card, with neither text a user message |
| `dispatch-records-only-the-turns-the-session-took` | Dispatch's thread read, which the Agents page reads, records the Send and the Aside accepted at attempt 1 and the carded Send at its retry, attempt 2, and never the BTW, the Send made while the session was down or the Send made while the listener listed no session |

## dispatch-session-settings.sh

INFRA-195's local-stack proof. Dispatch's live view shows a real Oh My Pi session's current model
and thinking level and switches them from its header. The stack is real: Oh My Pi with this
checkout's Envoy plugin, an Envoy listener, NATS, Postgres, Dispatch built from the checkout, and
its page driven in Chromium. The rig never takes the page as its own oracle. Each value the header
shows is compared with what the session reports without the page: RPC `get_state` (the model) and
`get_available_models`, the `Thinking:` and `Available:` lines an `/effort` prompt prints (RPC
`command_output`), the session file's `model_change`, `thinking_level_change`, card and user-message
entries, and Dispatch's stored messages, read with the person's header.

```bash
LEGION_TEST_PG_DSN='postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable' \
  bash scripts/e2e/dispatch-session-settings.sh     # → "dispatch session settings e2e: PASS in <n>s", exit 0
```

The script does only the setup and the cleanup. The driver,
[`lib/dispatch-session-settings.ts`](lib/dispatch-session-settings.ts), owns every Oh My Pi
process and one held-open page, signed in as `alice`, and runs the checks in order, each its own
function. The parts it drives are its siblings in `lib/`:
[`session-settings-omp.ts`](lib/session-settings-omp.ts) (Oh My Pi in `--mode rpc`),
[`session-settings-services.ts`](lib/session-settings-services.ts) (Dispatch, the listener and the
bus), [`session-settings-browser.ts`](lib/session-settings-browser.ts) (the held-open page),
[`session-settings-session-file.ts`](lib/session-settings-session-file.ts) (the session file's
entries) and [`session-settings-common.ts`](lib/session-settings-common.ts) (the rig's setup, the
transcript and the waits).

It differs from [`dispatch-user-turns.sh`](#dispatch-user-turnssh) in these ways:

- **It runs in an issue pod.** It needs no docker, tmux, mise or root. It uses `go`, `bun`, `jq`,
  `curl`, `tar` and `python3`.
- **Oh My Pi runs in `--mode rpc`, as a Legion pane runs it, not as the TUI in tmux.** The command
  is `omp --extension <pi-envoy> --mode rpc`, with the plugin packed by
  [`lib/pack-plugin.sh`](#libpack-pluginsh) and unpacked as the worker image unpacks
  `/opt/legion/pi-envoy`. The driver reads and writes the RPC protocol on the process's stdin and
  stdout. The profile is an isolated `PI_CODING_AGENT_DIR` under a `HOME` of the run's own. That
  `HOME` links `.omp/natives` to the caller's cache, as [`lib/omp-home.sh`](#libomp-homesh) does.
  The Oh My Pi is `LEGION_TEST_OMP`, not a mise tool spec.
- **The model turns use the caller's own provider keys, not the Hawk gateway.** The script takes
  `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` out of its environment before it starts anything, so
  no build, install, service or log of the run inherits them, and gives them back to the driver
  alone, which takes them off its own environment at once and passes them to each Oh My Pi it
  starts and to no other process (Chromium included). It sets no key of its own, and prints only
  each key's length. A key is needed only for a model of its provider.
- **Postgres is a loopback server the caller names**, in `LEGION_TEST_PG_DSN`, as for
  [`merged-root-waits.sh`](#merged-root-waitssh). The rig creates a database of its own in it and
  drops it on any exit. In a pod, start one without root as
  `docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md`
  says.
- **NATS is a pinned `nats-server`**: `LEGION_E2E_NATS_BIN`, or v2.10.22, which the rig installs
  once with `go install`. In an issue pod it installs into `/legion/scratch/bin`, on the issue's
  volume, and otherwise into the run's own directory. The server starts with JetStream on a port
  the kernel picks, as `merged-root-waits.sh` starts it.
- **No variable of the caller's shell named `DISPATCH_*`, `ENVOY_*`, `NATS_*`, `OMP*` or `PI_*`
  reaches any process of the run.** A Legion pane's own Dispatch, Envoy and profile stay out of it.

**The model list `picker-refusal` picks from is injected, not reported by the session.** The check makes the session
refuse a model because it has no key for that model's provider. Under the run's keys, the session
lists no such model, so its header cannot offer one. A bare NATS client publishes a `settings` frame
on `agentstream.<session>.frames`. The frame's `seq` is above the session's own, and it lists the
session's own models plus `google/gemini-2.5-flash`, a catalog model of a provider the run has no
key for. The person picks that model. The refusal comes from the real session, and the page
shows it. The check then reloads the page, so that the session's next replay puts its real frame
back. Any process that can reach NATS can publish this frame. That is the frames subject's existing
exposure, the one `dispatch-user-turns.sh`'s `a-forged-command-list-changes-nothing-on-the-page`
drives.

The default models are the cheapest reasoning models of each provider that the pinned host's
`get_available_models` lists under a pod's keys: `anthropic/claude-haiku-5-5` and
`openai/gpt-5-nano`. Their `/effort` lists differ, and the preflight refuses two models whose lists
are the same. `anthropic/claude-haiku-5-5` offers `off, auto, low, medium, high, xhigh, max`, and
`openai/gpt-5-nano` offers `off, auto, minimal, low, medium, high`. Before any check, the preflight
runs a one-word prompt on each model, in a session with no plugin.

The rig serves the checkout's built `packages/dispatch/web/dist` live. Do not run
`bun run build:web` or `bun run e2e` in this checkout while a run is live, for the reason
`dispatch-user-turns.sh` gives.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_TEST_PG_DSN` | required | a loopback PostgreSQL with pgvector; the run makes and drops its own database there |
| `LEGION_E2E_NATS_BIN` | the pinned `nats-server` v2.10.22, installed as above | another `nats-server` |
| `LEGION_TEST_OMP` | the `omp` on `PATH`, else the worker image's `/opt/omp/bin/omp` | the Oh My Pi to run. The rig refuses a host whose first replay has no `commands` frame, because that host cannot run typed input from Dispatch |
| `SESSION_SETTINGS_MODELS` | `anthropic/claude-haiku-5-5 openai/gpt-5-nano` | two `provider/id` models the caller's keys serve. The first is the profile's default model (every `modelRoles` entry) and the fallback target. Two Anthropic models run every check but `fallback`: Oh My Pi skips a same-provider fallback candidate after a turn with another Anthropic model's signed thinking, and only retries (dispatch://INFRA-309) |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` | required for a model of that provider | passed through to Oh My Pi only |
| `SESSION_SETTINGS_CHROMIUM` | `/usr/bin/chromium` | the Chromium Playwright launches (`executablePath`); a pod has no Playwright browser of its own |
| `SESSION_SETTINGS_EVIDENCE_DIR` | a fresh directory under `TMPDIR` | kept and printed: `transcript.log`; `checks/`, one screenshot per check; `logs/`, with the builds, the services, each Oh My Pi's stderr and RPC events (`omp-<label>.rpc.jsonl`), the Oh My Pi logs (`logs/omp/`) and the profile's `config.yml` and `models.yml`; and `sessions/`, each session file |

The profile's `config.yml` sets `defaultThinkingLevel: low` and every `modelRoles` entry to the
first model, so no side request (a title, a summary) goes to another model. It also sets
`retry.fallbackChains` (the key the fork's `session/retry-fallback-chains.ts` reads) from
`rig-broken/broken-model` to the first model. `models.yml` adds the provider `rig-broken`: a
literal key and one model, `broken-model`, whose `baseUrl` is a loopback port that a `python3`
process of the run binds and never listens on. Every connection to that port is refused.

Each check prints `== <name>`, what it observed, and `ok <name>`. The first check that fails ends
the run non-zero and names itself. Check 2 of the plan is three checks here, so a passing run
prints 20 `ok` lines. On any exit, the script stops what it started by the pid it started: the
`nats-server`, the listener, Dispatch, the port holder and every Oh My Pi in `$work/omp.pids`.
It stops an Oh My Pi only while that pid's command line still names the run's work directory. It
also drops the database and removes the work directory.

| check | what it holds |
| :--- | :--- |
| `header-before-any-turn` | before any prompt, the header shows `get_state`'s model and the level on `/effort`'s `Thinking:` line. The model picker offers exactly what `get_available_models` lists, and the thinking picker offers `/effort`'s `Available:` list, in its order. No turn has run in the session file or on the page |
| `terminal-set-model` | RPC `set_model` to the second model, then back to the first. The same open page shows each within 20 s of the RPC answer, `get_state` agrees, and the session file gains no user message |
| `terminal-set-thinking` | RPC `set_thinking_level medium`. The page shows it within 20 s, `/effort` reports it, and no user message is added |
| `terminal-typed-effort` | an RPC `prompt` of `/effort high`, the host's own typed route. It prints `Thinking set to high.`, the page shows `high` within 20 s, and no user message is added |
| `message-switch` | the composer sends `/model <second>` as a Send. The header shows it within 20 s, and the session replies `Model set to <second>.` |
| `picker-model` | a pick of the first model. Dispatch stores `/model <first>` authored by `alice`, the thread shows it as hers, and the session's reply `Model set to <first>.` shows on the switch line and in the thread. `get_state` and the header agree, and the header shows the level `/effort` reports. A composer prompt then runs, and its turn's `model` is the first model in the session file and on the page's turn label |
| `picker-auto-kept` | a pick of `auto`, then a pick of the second model. `/effort` still reports `auto`, and the header shows `auto` |
| `picker-level-after-model` | a concrete level of the second model that the first model lacks (`minimal` by default), then a pick of the first model. The level the header shows, and the one `/effort` reports, equals what `/effort` reports in a twin session after a person's `/model <first>` Send. The twin is a second session of the same profile, brought to the same model and level through RPC |
| `picker-effort` | a pick of a concrete level (`medium`). `/effort` reports it, and the header shows it |
| `picker-during-turn` | while a prompt runs `sleep 45` in the bash tool, a pick of the second model. The session answers within 30 s, while `get_state` still says the turn is streaming. The run's request that was already running is on the first model, and each request started after the answer is on the second. The next composer prompt runs on the second model |
| `picker-refusal` | the injected list described above, then a pick of `google/gemini-2.5-flash`. The switch line reads `The session answered: Failed to set model: No API key for google/gemini-2.5-flash`. `get_state`, `/effort` and the header are unchanged. A reload brings back the session's own list |
| `forged-switch-changes-nothing` | two sends to the session. First, a frame forged with the listener token, in `dispatch-user-turns.sh`'s forged-frame shape: a person-shaped steer naming a message Dispatch never stored, with the body `/model <the other model>`. Second, an Envoy message `/effort <another level>` from a second client registered with the listener. The session file records the forged attempt as handled (`envoy-dispatch-handled-attempt`), and both arrive as cards. `get_state` and `/effort` are unchanged. The check prints the values before, the values after, and what each would read had a switch been applied, which is its negative control |
| `agent-token-refused` | `POST /api/v1/agents/<id>/messages` with the body `/model <the other model>` and the session's Dispatch bearer gets 403 `HUMAN_ONLY`. Dispatch stores nothing new, and the session is unchanged |
| `other-session-unchanged` | a second session of the same profile, started before every switch, still reports the model and level it started with (`get_state`, `/effort`) |
| `no-answer` | the session's Oh My Pi gets `SIGSTOP`, and then a model is picked. Dispatch's send does not fail: its attempt stays `pending` or `sent`. After 30 s the switch line reads the no-answer text, the header keeps the values it had, and the pickers come back. The process gets `SIGCONT` 65 s after the message was stored, so the late frame arrives as a card and a handled attempt. The session's log records Dispatch's refusal (`attempt 1 was requested more than a minute ago`). The same accept, asked again with the session's bearer, answers 409 `ACCEPT_STALE`. `get_state` is unchanged, and Dispatch records no acceptance |
| `resume-keeps-switch` | one page, held open from before the first relaunch to the last step, and three steps. **The switch stays (A2):** the process ends and is started again with `--resume <session file>`, which opens the same session; `get_state` and `/effort` show the model and level picked last. **Numbering:** once the resumed process has registered, a bare NATS client subscribes to `agentstream.<session>.control` and waits up to 30 s for the first control message the relay sends there (its `watch`, about every 10 s while a page is open), then 1 s more; RPC `set_thinking_level` to another level then shows on the held page within 20 s with no reload, and the level set back shows too. **Arming:** the process ends again and is started with `--resume <session file> --model <the other model>`; `get_state` shows that model, and with no RPC change and no reload the held page shows it within 20 s of the registration. The screenshot is the held page then. RPC `set_model` (and `set_thinking_level`, when the level moved) puts the session back on the model and level picked last, and the reloaded header shows them. A held-page step that times out reloads the page, to tell a report the page missed from one never made, and fails naming its step |
| `new-session-defaults` | a new session of the same profile reports the profile's default model and `low`, through RPC and on its own live view |
| `fallback` | a pick of `rig-broken/broken-model`, then a composer prompt. That request fails, and the session file gains a `model_change` with role `fallback` to the first model. The header shows the first model within 20 s of that entry, with no further user message. The check prints how each request of the run ended, including the retry on the fallback model |
| `older-plugin-notice` | a session on `@sjawhar/pi-envoy` 10.24.0, a release before the settings frame (pinned in the script, since the newest release reports its settings once this one ships), installed with `bun add` into the run's directory, runs one prompt. Its live view shows that turn's model, no picker, and the restart notice |
| `attribution` | `bob` sends a switch with `X-Dispatch-User: bob`, the header's own body (`/model <id>` as a steer). Dispatch stores it as `bob`'s, the session answers it, and `alice`'s held-open page shows `bob` beside it in the thread |

## verifiers-staging-token.sh

```sh
bash scripts/e2e/verifiers-staging-token.sh     # → "verifiers e2e: PASS", exit 0
```

Needs `go`, `docker`, `kubectl`, `jq`, `curl`, `ss` and `base64`, and credentials for the `staging`
kube context — it mints two real tokens with `kubectl -n legion create token default --audience
{dispatch,envoy} --duration 10m` (the TokenRequest API) and reads the issuer from the cluster's own
`/.well-known/openid-configuration`. That is why it is a devbox gate and not a CI job: GitHub's
runners have no cluster to mint from. It builds both binaries from the checkout, so it proves the
tree you are standing in.

| input | default | meaning |
| :--- | :--- | :--- |
| `VERIFIERS_E2E_KUBE_CONTEXT` | `staging` | the cluster to read the issuer from and mint against |
| `VERIFIERS_E2E_NAMESPACE` | `legion` | the namespace whose `default` service account the tokens are minted for |

What the run touches, and nothing else:

- `/tmp/verifiers-e2e` — the two built binaries, the boot logs, the refusal logs, the last response
  body, and the scratch `HOME` the binaries run under, so Dispatch's signing key never lands in the
  box's `~/.local/share/dispatch`. Recreated from empty each run.
- the containers `verifiers-e2e-pg` (`postgres:16`) and `verifiers-e2e-nats` (`nats:2.10 -js`), both
  on ephemeral loopback ports.
- two ports from [`lib/free-port.sh`](#libfree-portsh), one for `dispatch` and one for the
  listener, the second excluding the first.

After any exit — pass, failure, or an interrupt — the `EXIT` trap signals both binaries and removes
both containers; `docker ps -a --filter name=verifiers-e2e` comes back empty.

### No raw token is printed

A token lives in a shell variable, reaches curl through a config document on stdin — never an
argument, since `/proc/<pid>/cmdline` is world-readable, and never a file — and everything the
script prints that it did not compose itself goes through `redact`, which replaces any JWT-shaped
run with `<redacted-jwt>`. The two minted tokens are reported by their decoded `iss`, `aud`, `sub`
and `exp`. The last step asserts that neither binary's log contains `eyJ`.

### How it fails

Every step is fatal and names its reason: a cluster that will not mint, a discovery document with
no `https` issuer, a token whose `iss`, `aud` or `sub` is not the one asked for, a container that
never becomes ready, a binary that exits during boot (reported at once from its dead pid, with its
log, rather than after the whole wait), a boot log that does not name the issuer, any of the eight
calls answering the wrong status, a body that is not the actor the verifier should have produced, a
refusal whose class is missing from the log — or present in the 401 body, which would tell an
unauthenticated caller which credential the listener is configured for.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`). The container entrypoint's
  bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and a client that
  connects in that window is reset by the peer.
- The listener answers `/healthz` 200 with `{"status":"starting"}` before NATS is up, and every
  `/v1` route is 503 until then, so the wait is for the healthy status and not for the port.
- The "never prints a token" assertions are written `grep -q … && fail`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail the
  run.

## multi-repo-acceptance.sh

The observer for the multi-repository live acceptance: a read-only CLI the operator runs by hand,
one observation at a time, as a tree advances on the deployment's own repositories. It never starts
a daemon, Dispatch, or any agent of its own, and it writes nothing remote: every admission, push,
pull request and Dispatch write the acceptance checklist names is the operator's and the agents'
own.

**Every judgment reads a structured source**: the daemon's state JSON (`GET /legion/v1/state`),
Dispatch's own JSON API (`GET /api/v1/issues/<key>` and `/events` — the same routes the dashboard
reads, never `dispatch read`'s human prose, which truncates and has no stable grammar to parse),
GitHub's API (`gh api … --jq`), and `kubectl get`/`logs`/`exec` as the operator, with `exec` run as
the role itself (below), never as the bare pod shell, which carries none of a role's variables. The
one exception is a daemon message whose exact format the daemon itself produces: those are matched
as the complete literal sentence, with the run's own repository names substituted in, from a
session actor named `legion-daemon:<PROJECT>` and carrying the daemon's outbox marker, never a
loose substring over any author's messages. Dispatch takes a bearer session's actor id from the
request body, so that id is the message's claimed author: whoever holds the project's Dispatch
bearer can post the same text. Such a note shows what Dispatch holds, never who posted it. Two such
sentences are quoted below with their file:line.

```sh
export LEGION_E2E_REPO_T=<owner>/<toolchain repo> LEGION_E2E_REPO_C=<owner>/<consumer repo>
export LEGION_E2E_PIN_FILE=<path inside C that pins T's commit>
export LEGION_E2E_REPO_X=<owner>/<negative control repo> LEGION_E2E_REPO_O=<second owner>/<repo>
export LEGION_E2E_DAEMON_URL=http://127.0.0.1:<port> DISPATCH_URL=<dispatch API root> DISPATCH_TOKEN_FILE=<0600 file>
export MULTIREPO_EVIDENCE_DIR=<dir kept for the PR body>
bash scripts/e2e/multi-repo-acceptance.sh record-head "$LEGION_E2E_REPO_T"
bash scripts/e2e/multi-repo-acceptance.sh children <parent KEY>
bash scripts/e2e/multi-repo-acceptance.sh branch-base <T child KEY>
bash scripts/e2e/multi-repo-acceptance.sh pin "$LEGION_E2E_REPO_C" <T merge sha>
bash scripts/e2e/multi-repo-acceptance.sh done <KEY>...
bash scripts/e2e/multi-repo-acceptance.sh second-owner <O child KEY> <pod> [role]
bash scripts/e2e/multi-repo-acceptance.sh cache <issue KEY> <pod> [container]
bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x before <T child KEY>        # before the operator opens the pull request in X
bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x after <run id> <T child KEY>   # after it closes
bash scripts/e2e/multi-repo-acceptance.sh negative no-repo <KEY>
bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit before <child KEY>        # before the operator's edit
bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit after <run id> <child KEY>   # after it
```

Every input above is required and is an operator value, never a literal in the script: an absent
or malformed observation name or argument count is refused with `usage:` and exit 2 before any
environment variable, tool, or network read, and so is an issue key that is not shaped
`PROJECT-123` (each goes into an evidence file name and an API path); then it refuses naming the
first required variable that is unset, refuses a value that is not an `owner/name` repository
shape, refuses two of `LEGION_E2E_REPO_{T,C,X,O}` naming the same repository
(case-insensitively), refuses `LEGION_E2E_REPO_T` and `LEGION_E2E_REPO_O` sharing an owner (the
second-owner observation needs two), and refuses a `LEGION_E2E_DAEMON_URL` whose `GET /healthz`
does not answer `{"status":"ok"}`. `gh`, `jq` and `curl` must be on `PATH`; `second-owner` and
`cache` also need `kubectl`. `second-owner` also needs `python3` on the pod's own `PATH`, never the
operator's: the worker image the `roles` container runs is checked for it at boot
(`internal/capabilities/image.go`), so it is never an operator dependency to install. There is no
`dispatch` CLI dependency at all: Dispatch reads go straight to its own JSON API with the bearer
`DISPATCH_TOKEN_FILE` holds. That bearer never appears in any process's argv
(`/proc/<pid>/cmdline` is world-readable): it reaches `curl` through a `-K` config file
(`mktemp`, `chmod 600`, one `header = "Authorization: Bearer <token>"` line). An `INT` or `TERM`
on the whole process group (a terminal's Ctrl-C) or on the script's own pid alone removes that
file and a raw-fetch temp file mid-write and stops the script with the shell's conventional
130/143: `raw_fetch` arms its own local traps around each fetch, since most calls run inside a
`$(…)` subshell, where a variable a signal handler reads in the parent is never set, and restores
the script's standing handlers before returning. The script never touches a shared session-state
root.

State and Dispatch reads go through `state_raw`/`dispatch_issue_raw`/`dispatch_events_raw`, each
taking the calling observation's own name: every evidence file is `<observation>-state.raw`,
`<observation>-dispatch-issue-<key>.raw` or `<observation>-dispatch-events-<key>.raw`, never a
source-only name two observations in one run could share and overwrite. `before` and `after` are
themselves two different observation names (`negative-pr-in-x-before`/`-after`,
`negative-repo-edit-before`/`-after`), so they never share raw evidence either, and `children`
reads state exactly once, before any child's judgment, reusing that one read (and its one raw
file) for every child, so the evidence is the state every child was judged against. Each read
writes the external response straight to its own file (never through `$(…)`, which strips
trailing bytes; the temp-file-then-`mv` write is itself checked for a failed rename), and every jq
path below reads that file directly, so the evidence a PR body quotes is exactly the bytes the
observation judged.

Each observation prints `CHECK <name>: PASS` or `CHECK <name>: FAIL` (a FAIL exits non-zero) and
writes its own summary file (`<name>.txt`) beside the raw reads it judged — keep
`MULTIREPO_EVIDENCE_DIR` and quote both in the PR body:

| observation | judges |
| :--- | :--- |
| `record-head <T>` | T's default-branch head now (both nonempty, the head a full commit id), for `branch-base` to compare the first child commit's parent against; refuses a repository argument that is not T |
| `children <parent>` | Dispatch's own `GET /api/v1/issues/<parent>` for `.children[].key`, then each child's `.resolved_repo` against one `state_raw` read (made once, before any child's judgment, and reused for every child) against `.issues[<key>].repo`: one child resolves to T and one to C, in both, case-insensitively; a non-2xx Dispatch answer for any child fails the whole observation |
| `branch-base <T child>` | `legion/<child>`'s first commit's first parent on GitHub equals the stored `record-head`, both required to be full commit ids |
| `pin <C> <T merge sha>` | `LEGION_E2E_PIN_FILE` on C's default branch pins the sha: the one run of 40 or more hex digits the file holds, compared whole and case-insensitively, is the sha, so the sha in a comment beside a pin that names another commit, or inside a longer hex string, fails; refuses an empty or non-full-length sha argument and a repository argument that is not C; the file's own base64-decoded bytes go straight to `evidence/pin.txt` by redirection |
| `done <KEY>...` | Dispatch's own `.status` field for every named issue reads `done`; a non-2xx Dispatch answer fails the observation |
| `second-owner <O child> <pod> [role]` | `.issues[<child>].pullRequest.repo` is O and its phase is `done` (the phase a merge closes an issue into); `gh api repos/<O>/pulls/<n> --jq .merged` is `true` (the daemon's `done` alone is never merge proof); the pod's `legion.dev/issue` label matches the issue. The credential evidence is `lib/as-role.py`'s, streamed over `kubectl exec -i` into the pod's `roles` container and from there into the named role's own namespaces (the role's `legion launcher role-init`'s user, mount, PID and IPC namespaces, then its worker-shim's user namespace; `[role]` defaults to `implementer`), where it runs as `python3 -I -` (`-I`: no working directory and no user site on `sys.path`, since both are agent-writable). It resolves the one `worker-shim` — a process whose `exe` is `/opt/legion/bin/legion`, whose own parent is PID 1 (in the role's PID namespace, its role-init) and whose argv[1] is `worker-shim` (a test points the scan at a fake tree with `MULTIREPO_PROC_ROOT`, refused without `MULTIREPO_TEST_SEAM=1`) — then that shim's one child whose argv equals the shim's own argv after its `--` or, behind a plugin-reinstall launch's install wrapper (`/bin/sh -c <script> legion-plugins <agent argv>`, whose final `exec "$@"` replaces its image with the agent, `internal/runtime/sandbox/plugins.go`), the argv after the wrapper's four leading elements. A broker-enrolled pod's shim also parents `agent-secrets renew` and briefly `codegraph`, so parentage alone is ambiguous; zero or several candidates are refused by name, every candidate listed. The agent's `/proc/<pid>/environ` is read for two things. It is compared with what the image tells an agent: `LEGION_GH_OWNERS_DIR` must be `/var/run/legion/gh`, `GH_CONFIG_DIR` its owner's directory under it and `LEGION_GH_PATH` `/usr/local/bin/gh`; any difference is a FAIL. And the proxy variables come from it. Nothing else is taken from it, since the agent's own code (the workspace's `.omp/extensions`, its profile plugins) runs in that process and can rewrite its environment in place: `legion git-credential get` and `gh` run by absolute path under `/opt/legion/bin`, root-owned in the image, with `PATH`, `LEGION_GH_OWNERS_DIR` and `LEGION_GH_PATH` set to the image's constants and `HOME=/nonexistent` (TLS is verified against the root-owned CA store, so a proxy can drop a request and cannot answer one; a test lays fakes out under `MULTIREPO_TRUSTED_ROOT`, same marker). `gh api repos/<T> --jq .full_name` must say T. Then, for O and for T, GitHub's answer to the request a `git push` opens with, `GET https://github.com/<repo>.git/info/refs?service=git-receive-pack`, sent with the credential that helper prints for that repository: for O, `200` and an `application/x-git-receive-pack-advertisement` (the positive control: network, helper and token are sound); for T, `403` with a one-line body that is GitHub's whole refusal, `Write access to repository not granted.` or `Permission to <T>.git denied to <who>.`, which shows the credential the helper hands out for T cannot push T. A `200` for T, a `404`, no answer, another repository's refusal or any other body is a FAIL. No git runs: git reads `~/.netrc`, `~/.gitconfig`, the repository's own `.git/config`, any scratch repository's and the `GIT_*` variables, all of which the agent's uid can write while the check runs; the request is Python's `urllib`, which reads none of them. What `as-role.py` prints is one JSON object, kept as `second-owner-probe.raw`; it never holds an argument's or an environment variable's value or a token. The agent runs with `--append-system-prompt <the composed prompt, the deployment's own instructions>`, so its cmdline appears only as `argv0`, `argc` and a sha256 of the whole `/proc/<pid>/cmdline`. The check trusts the root-owned image (`/opt/legion/bin`, the CA store, `python3` on the exec's `PATH`); it does not defend against an agent that can kill its own shim or ptrace the prober, which no check run beside it in the same container can. |
| `cache <issue> <pod> [container]` | the named pod's `legion.dev/issue` label matches the issue (refusing an unrelated pod outright), its fetch container's log (`kubectl logs`, default container `workspace-fetch`) carries the producer's exact, anchored cache-stream-with-delta success line (`packages/daemon/cmd/legion/workspace_fetch.go:83`, the form `workspace_fetch_test.go:572` pins — a whole-line match, not a loose substring), and `.repositories.mapped[] | select(.repo==<repo>) | .cache.ready` is `true` |
| `negative pr-in-x before\|after <run id>? <T child>` | a two-call baseline protocol (below). `before` requires the child already recorded on T or C, and captures GitHub's own highest pull-request number in X at that moment — the cursor `after` compares against, never a wall-clock timestamp, which skew between the operator and GitHub could fool. `after` PASSes only when GitHub shows a closed pull request on `legion/<child>` in X numbered above that cursor, **and** the daemon's own side is unaffected: the child is still recorded on the baseline's repository, and its pull request never names X |
| `negative no-repo <KEY>` | `.issues \| has(<KEY>)` is `false` (refusing outright, never vacuously passing, when `.issues` is not even an object), Dispatch's own `.resolved_repo` is still `null`, and the **latest** repository note among the messages claiming the daemon's actor (`{kind:"session", id:"legion-daemon:<PROJECT>"}`) is the exact no-repository admission refusal, word for word, with the daemon's outbox marker — `"Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.\n\n<!-- legion-outbox:<id> -->"` (`packages/daemon/internal/workflow/repository.go:76`, `packages/daemon/internal/record/outbox.go:71-73,104-106`). Dispatch takes a bearer session's actor id from the request body (`packages/envoy/internal/dispatch/api/server.go`, `bearerSessionActor`), so the note shows what Dispatch holds, never who posted it; what shows the daemon declined the issue is its own state read. |
| `negative repo-edit before\|after <run id>? <child>` | a two-call baseline protocol (below). `before` captures the daemon's recorded repository and refuses outright if Dispatch already resolves the issue to X. `after` requires: the daemon's record unchanged; Dispatch's own `.repo` for the issue now reading X; an `issue.updated` event whose `.seq` is above the baseline's own last-seen `.seq` with `.payload.repo` equal to X (Dispatch's own sequence cursor, never a wall-clock comparison, and never merely "any `issue.updated`", which the daemon's own routine status writes also emit); and the daemon's exact kept-repository message, word for word, with its claimed actor id and outbox marker (as for `negative no-repo`), built from Dispatch's own resolved spelling of X (`dispatch_repo_now`, read back from the API) rather than the operator's `LEGION_E2E_REPO_X` spelling, since the daemon's real message echoes whatever case Dispatch itself stored — `"Legion keeps <repo> as this issue's repository; the change to <X> in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for <X> instead."` (`packages/daemon/internal/workflow/repository.go:94`) |

**The two-call baseline protocol** (`negative pr-in-x`, `negative repo-edit`): `before` mints a
fresh run id, captures the baseline under `$MULTIREPO_EVIDENCE_DIR/runs/<run id>/baseline.json`
(the directory made with a plain `mkdir`, the file written with `set -o noclobber` — both fail
loudly rather than overwrite, so two overlapping `before` calls for the same issue can never
collide), binds it to the project, the daemon URL and the issue's own generation, and prints
`BASELINE <name>: captured` — never `PASS`, since capturing a baseline is not an acceptance
judgment. `after` takes that exact run id back (rejecting one that is not a bare id — no `/`, no
`..`) and refuses a missing, foreign (another issue, operation, project or daemon URL than the
baseline's, or a generation that moved since — a hand-over or re-admission mid-control), or stale
(more than 24 hours old) baseline before judging anything.

`scripts/e2e/lib/multi-repo-acceptance.test.ts` pins the script's refusals and every jq path above
against two `Bun.serve` stubs (the daemon's `/legion/v1/state` and Dispatch's own
`/api/v1/issues/*` JSON API) and fake `gh`/`kubectl` on an isolated `PATH`;
`scripts/e2e/lib/multi-repo-negative.test.ts` pins the three `negative` observations and
`scripts/e2e/lib/multi-repo-second-owner.test.ts` pins `second-owner`; the three share
`scripts/e2e/lib/multi-repo-acceptance-harness.ts`. For `second-owner`, the fake `kubectl exec`
runs `lib/as-role.py` locally against a fake, deterministic `/proc`-shaped tree built per test
(`fakeProcTree` in `multi-repo-proc-tree.ts`, wired in through `MULTIREPO_PROC_ROOT` and the
`MULTIREPO_TEST_SEAM` marker it requires): scanning the host's own `/proc` would pick a sibling
session's process on a shared devbox. The image's `legion`, `gh` and gh directory are fakes laid
out under `MULTIREPO_TRUSTED_ROOT` (same marker), and GitHub is a stub (`MULTIREPO_GITHUB_URL`,
same marker) that answers the receive-pack advertisement request per repository and token. The
tree builds the shapes a real process tree cannot reliably reproduce on demand: no worker-shim,
two, a shim whose parent is not PID 1 or whose
`exe` is not `/opt/legion/bin/legion`, two children whose argv both match the shim's, and a
broker-enrolled shim's `agent-secrets renew` and `codegraph` children beside the agent. One test
spawns a real `sh -c 'exec "$@"' legion-plugins sleep 999` and copies that process's own post-exec
`/proc/<pid>/cmdline` into the tree, so the install-wrapper match is checked against what the
kernel produces. The regressions pin each way an observation could print PASS without the fact it
names: a failed Dispatch read that prints the bearer; repository names that merely contain T's; a
merged-PR claim the phase alone cannot prove; a push-access answer for T that is a `200`, a `404`,
no answer, another repository's refusal or a multi-line body, and one for O that is not an
advertisement (a missing positive control); an unanswered credential helper; a `~/.netrc`,
`~/.gitconfig` or `GIT_*` variable the agent plants (git never runs, so none can reach the
answer); a role process that cannot be told apart from its neighbors; evidence that holds an
argument, an environment value or a token (checked at a 130 KB prompt and across every evidence
file); a `hashlib.py` in the exec's working directory or a `.pth` in the user site; a pin file that
mentions the sha beside another pin; a default branch with a `?` in it; an issue key shaped like a
path; a baseline that is missing, foreign, stale or shared; two observations in one run whose raw
evidence would overwrite each other; a SIGTERM or a group SIGINT mid-fetch; a cache fetch slower
than a minute; and an operator's spelling of X that differs in case from Dispatch's. The live
acceptance on the deployment's own repositories is the operator's; this suite does not run it.

## burstable-kind.sh

```sh
LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/burstable-kind.sh     # → "burstable kind e2e: PASS in <n>s", exit 0
```

Runs on an amd64 (x86_64) Linux host only: the gVisor release it installs and the static test binary
it builds are amd64. Needs `docker`, `go`, `kubectl`, `curl`, `zstd`, a `tar` that supports `--zstd`
(GNU tar 1.31 or later) and `kind` (`LEGION_E2E_KIND` names the binary when it is not on `PATH`).
Its cluster is [`lib/kind-sandbox.sh`](#libkind-sandboxsh)'s, `legion-burstable-<pid>`, whose
kubeconfig is a file in the run's work directory: every `kubectl` call names that file and the
cluster's `kind-…` context, and the Go driver refuses any other context, so the caller's kubeconfig
and its current context are never read. The cluster is deleted on any exit.

| check | what it holds |
| :--- | :--- |
| `cluster`, `agent-sandbox`, `gvisor` | the lib's: the kind cluster, its one node labelled `legion.dev/pool=legion`, the `legion` namespace and the `standard` storage class; Agent Sandbox v1.0.3's release manifest, pinned by sha256, applied and its controller rolled out; gVisor's dated release, pinned by sha512, installed into the node's containerd as the `runsc` handler with a `gvisor` RuntimeClass, and a `runtimeClassName: gvisor` pod's `dmesg` says `Starting gVisor` |
| `image` | the worker image, pulled into the node's containerd by digest |
| `launch` | `TestABurstableIssuePodRendersLive` (`internal/daemon/burstable_live_test.go`, `-tags e2e`), built as a static test binary and run in a container on the kind network, writes two `legion.yaml` files, reads each with the daemon's loader, translates it with `sandboxOptions`, runs the cluster check, and launches one issue's root through the Sandbox runtime's `Spawn` on a worker stream bound to that container's address (pods reach the kind network's containers, while a host firewall can keep them from the host itself); the launch makes the repository's cache first, as every issue pod's does; it holds the `roles` container to the issue pod's request and limit (3 CPU, 8Gi under 32Gi, 6Gi under 80Gi for the first file; 3 CPU, 19Gi, 6Gi under 80Gi for the second) and the pod to its QoS class |
| `qos-class` | `kubectl get pod <pod> -o jsonpath='{.status.qosClass}'` reads `Burstable` for the file with `issue: {memory: 32Gi, memory_request: 8Gi}` and `Guaranteed` for the file with no `resources` block; each pod has a `roles` container, is `PodScheduled=True` under `runtimeClassName: gvisor`, and its manifest is kept as evidence |
| `check-config` | `legion start --check-config` passes both files, printing `resources: Burstable (a request below the limit): the issue pod; every other pod is Guaranteed` for the first and no `resources:` line for the second, and refuses `memory_request: 40Gi` (`runtime.kubernetes.resources.issue.memory_request 40Gi exceeds memory 32Gi`) and `memory_request: lots` (`… must be a positive Kubernetes quantity …`) with exit 1 |

What the proof stands in for lies outside the reservation path: each role's agent is a sleep stub,
so nothing dials a model, Dispatch or NATS (their addresses in the files are documentation
placeholders); the store answers as the daemon's does for an issue's first launch; the mapping
names one public repository, which GitHub clones for its cache without asking for the placeholder
provisioning token; and the roles' gh files hold a placeholder token. The pod's reservation, its
admission, scheduling and QoS class, Agent Sandbox's pod and gVisor are the real ones.
`burstable_kind_test.go`, untagged, builds both files through the loader and each runtime through
`sandboxOptions` and `sandbox.New` on every `go test`, so a contract change that refuses either
fails CI, not the next live run. `BURSTABLE_EVIDENCE_DIR` (default a fresh `/tmp` directory, kept
and printed) keeps the transcript, both `legion.yaml` files and each pod's manifest as the API
server holds it.

## controller-sweep-kind.sh

```sh
LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/controller-sweep-kind.sh     # → "controller sweep kind e2e: PASS in <n>s", exit 0
```

Runs on an amd64 (x86_64) Linux host only, with the tools `burstable-kind.sh` needs; its cluster is
[`lib/kind-sandbox.sh`](#libkind-sandboxsh)'s, `legion-controller-sweep-<pid>`, deleted on any exit
with the Postgres container the run starts beside it on the kind network.

| check | what it holds |
| :--- | :--- |
| `drivers` | two static test binaries of `TestTheControllerOrphanSweepLive` (`internal/daemon/controller_sweep_live_test.go`, `-tags e2e`): the checkout's, and the negative control's, built from a scratch copy of the checkout whose `controllerOwned` answers false before it reads the store, so its sweep judges the controller's Sandbox by the known set alone |
| `cluster`, `agent-sandbox`, `gvisor` | as `burstable-kind.sh`'s, through the lib |
| `image` | the worker image, pulled into the node's containerd by digest |
| `store` | the CI's pinned pgvector Postgres on the kind network, migrated by the driver |
| `sweep` | the checkout's driver, in a container on the kind network, reads the claims, stores the controller's claim `launching`, and launches the controller through the runtime's `Spawn` (`controller-launch`: its Sandbox, its `gvisor` pod and its volume claim on the API server); five `ReconcileOrphans` at grace 0 told the claims read before the launch each leave the Sandbox, its uid unchanged and not terminating, and its volume claim (`sweep-keeps-launched-controller`); with the claim stored `retired`, a sweep deletes the Sandbox, and the API server holds neither it nor its volume claim (`retired-controller-swept`); the `legion` namespace then holds no Sandbox or volume claim |
| `negative-control` | the negative control's driver, on the same store and cluster, makes the same launch and sweep, and a sweep at grace 0 deletes the Sandbox of the controller whose claim the store holds `launching` (`reverted-sweep-deletes-launched-controller`) |

What the proof stands in for lies outside the sweep: the controller's agent is a sleep stub, so
nothing dials a model, Dispatch or NATS (their addresses in the file are documentation
placeholders); its App tokens are placeholders; and no repository is mapped, so its gh files name
no owner. The claim the supervisor would store is written to the store by the driver, as the
supervisor writes it before a launch begins. The store, the runtime's sweep and launch, Agent
Sandbox's controller, its foreground deletion of the volume claim, and gVisor are the real ones.
`controller_sweep_kind_test.go`, untagged, builds the proof's `legion.yaml` through the loader and
its runtime through boot's factory and `sandbox.New` on every `go test`, so a contract change that
refuses either fails CI, not the next live run. `CONTROLLER_SWEEP_EVIDENCE_DIR` (default a fresh
`/tmp` directory, kept and printed) keeps the transcript and, on a failure, what the cluster held.

## share-volumes-kind.sh

```sh
LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/share-volumes-kind.sh     # → "share volumes kind e2e: PASS in <n>s", exit 0
```

Runs on an amd64 (x86_64) Linux host only, with the tools `burstable-kind.sh` needs; its cluster is
[`lib/kind-sandbox.sh`](#libkind-sandboxsh)'s, `legion-share-volumes-<pid>`, deleted on any exit.
The kind node's own `mount.nfs` mounts the share, as a GKE node's does.

| check | what it holds |
| :--- | :--- |
| `binaries` | a static test binary of `TestShareVolumesMountInAnIssuePodLive` (`internal/daemon/share_volumes_live_test.go`, `-tags e2e`) and a static `legion` |
| `cluster`, `agent-sandbox`, `gvisor` | as `burstable-kind.sh`'s, through the lib |
| `images` | the worker image and the NFS server's (SIG Storage's `nfs-provisioner`, pinned by digest), pulled into the node's containerd |
| `nfs-server` | NFS-Ganesha in the `nfs` namespace, behind a Service, exporting `/team` (seeded with one file naming the run) and `/scratch`; the node mounts `/team` and reads the seed; a PersistentVolume of `/team` and the claim `team-share` in `legion`, `Bound` |
| `launch` | the driver, in a container on the kind network, writes four `legion.yaml` files, holds each to the pod block the loader reads from it, and launches the `shares` file's issue root through the runtime's `Spawn`; its pod carries the claim and the export as volumes, both mounted in its one `roles` container — the claim read-only, the export writable — and in no init container, under `runtimeClassName: gvisor` |
| `pod-shares` | in the pod's `roles` container, whose kernel is gVisor's: `cat /mnt/team/seed.txt` reads the server's seed through the claim, a write to `/mnt/team` is refused `Read-only file system`, and a file written to `/mnt/scratch` is what the server's `/export/scratch` then holds |
| `check-config` | `legion start --check-config` passes the `shares` file and refuses a volume naming a claim and an export (`… must set exactly one of secret, config_map, projected, persistent_volume_claim, nfs`), exit 1 |
| `probe-mounts-shares` | `legion probe-boot` on the `shares` file: the probe pod mounts both shares and its container runs, so the refusal, exit 1, is the image's own verdict on the run's agents' models, which have no route here, and names no share (`failed its probe: pod <name> Failed`, never `cannot mount` or `over its mounts`) |
| `probe-silent-server` | `legion probe-boot` refuses, exit 1, within its first attempt, the file whose export's server is `192.0.2.10` (RFC 5737's documentation range, where nothing answers): `… gives it from NFS export 192.0.2.10:/scratch: MountVolume.SetUp failed … mount failed: … Connection timed out …`, the kubelet's first report, which already stands for mount.nfs's own two minutes of retries |
| `probe-missing-claim`, `probe-missing-export` | `legion probe-boot`, in a container on the kind network, refuses, exit 1, the file naming a claim that does not exist (`cannot mount volume "team", which runtime.kubernetes.pod.volumes gives it from PersistentVolumeClaim "absent-share": … persistentvolumeclaim "absent-share" not found`) and the file naming an export the server lacks (`… gives it from NFS export <server>:/absent: MountVolume.SetUp failed … mount failed: …`) |
| `probe-missing-image` | `legion probe-boot` refuses, exit 1, within its first attempt and with no transient retry logged, the `shares` file with its image's digest swapped for an all-zero one the repository does not have: `worker image sha256:0…0 failed its probe: the registry does not have image <repository>@sha256:0…0: … rpc error: code = NotFound desc = failed to pull and unpack image … : not found`, once the pod's `Failed` events show the kubelet met that answer on two pulls |

What the proof stands in for lies outside the volume path, as `burstable-kind.sh`'s does: the
agents are sleep stubs, the store answers for an issue's first launch, the repository is public,
and the roles' gh files hold a placeholder token. The loader, the runtime, the image probe, Agent
Sandbox's pod, gVisor, the node's NFS client and the NFS server are the real ones.
`share_volumes_kind_test.go`, untagged, builds all four files through the loader and each runtime
through `sandboxOptions` and `sandbox.New` on every `go test`. `SHARE_VOLUMES_EVIDENCE_DIR`
(default a fresh `/tmp` directory, kept and printed) keeps the transcript, the four `legion.yaml`
files and the missing-image copy of the `shares` one, the issue pod's manifest and each
`probe-boot`'s output.

## merged-root-waits.sh

```sh
LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
  [LEGION_E2E_NATS_BIN=<nats-server>] bash scripts/e2e/merged-root-waits.sh     # → "merged root waits e2e: PASS in <n>s", exit 0
```

Runs on any Linux host with `go` — no Docker, no cluster. `LEGION_TEST_PG_DSN` is required and
names the PostgreSQL the daemon migrates and runs on; the script starts none, so CI's service, a
devbox container and a pod's own embedded PostgreSQL all satisfy it. The broker is the run's own:
the `nats-server` `LEGION_E2E_NATS_BIN` names, or the pinned `v2.10.22` the script installs with
`go install` into a GOBIN of its own, started with JetStream on a port the kernel picks for it
(`-p -1`, read back from its log) and stopped on any exit. The four checks below are
[`lib/live-daemon-proof.sh`](#liblive-daemon-proofsh)'s, the run `no-required-checks.sh` makes too.

| check | what it holds |
| :--- | :--- |
| `prerequisites` | `LEGION_TEST_PG_DSN` is set (the DSN itself is not printed), `go` and `awk` are on PATH, and a `nats-server` is named or installed |
| `broker` | that `nats-server`, with JetStream, listening on 127.0.0.1 and ready |
| `driver` | a test binary of `TestAMergedRootsCloseWaitsForTheRestOfItsTreeLive` (`internal/daemon/merged_root_waits_live_test.go`, `-tags e2e`) |
| `proof` | the seven steps below, each printing what it drove and what it read back between its own `== <name>` and `ok <name>` |

The driver boots the whole daemon through `run`, the function `legion start` calls, over that
PostgreSQL and that broker, and seeds one tree: a root in `accepting` on a reviewed, open pull
request, holding its tree's slot, with one child implementing beneath it on an open pull request
of its own. Every fact it drives enters by a transport production has — a GitHub or Dispatch
envelope on the `ENVOY_NOTIFICATIONS` stream, or one of the daemon's own HTTP routes — and every
assertion is a read of `GET /legion/v1/state`, of what the daemon posted to the Dispatch stand-in
or published to the Envoy listener stand-in, or of what the fake worker runtime was asked to
launch and suspend. Step 7 also reads the store once, for the daemon's `processed_events` row of
the envelope it drove, so that its window starts with that event applied; no route shows it, since
the workflow only logs the event it leaves. The steps run in the order each one's state follows
from the last, so the plan's step 6 is the control of its step 1 and runs beside it, and the
restart runs where the wait it must not disturb stands:

| step | driven by | read back as |
| :--- | :--- | :--- |
| `step1-the-acceptance-waits-for-the-child` | the architect's `POST /legion/v1/signoff`, with a grant its registered session bought | 409 `SIGNOFF_CHILD_IN_WORKFLOW` naming the child and `park_child`; the root still in `accepting` |
| `step6-the-acceptance-lands-once-the-child-is-out` | the child parked to `backlog` by its own Dispatch event, the way the refusal names, then the same sign-off | the park read back on the state, then 200 and the root in `retro` |
| `step2-the-merged-root-waits-for-its-child` | the root's merge, as a `notifications.github.…` envelope | the root still `awaiting_merge`, its `pullRequest.state` now `merged`, its Dispatch status not `done`, its slot still held, no production check, no suspension asked of the runtime, no status write taken by the Dispatch stand-in, and exactly one `pr-merged` notice on the architect's role topic naming the pull request and the child |
| `step5-a-restart-leaves-the-wait-and-tells-no-one-again` | a restart of the daemon at that state | over three seconds, the root still `awaiting_merge` on its merged pull request and still exactly one `pr-merged` notice: the boot's sweep leaves the wait and repeats no notice |
| `step3-the-waiting-roots-child-completes-its-phase` | the child's implementer completion over `POST /legion/v1/handoff/complete` | 200, the child in `testing`, and the root still waiting — where a lingering tree refuses the completion 409 `TREE_LINGERING` |
| `step7-a-pull-request-opened-from-the-waiting-roots-branch-leaves-its-merge` | GitHub's `opened` of another pull request of the run's own from the root's branch `legion/<root>`, as a `notifications.github.…` envelope | once the daemon's `processed_events` row shows it took the envelope, over three seconds, the root still `awaiting_merge` with `pullRequest.number` its own merged pull request's and `pullRequest.state` `merged`: the opened is not recorded over the merge the root waits on |
| `step4-the-childs-merge-closes-the-root` | the child's own merge envelope | the child `done`; the root `done` on Dispatch too, its slot freed, its production check open on the root's own merged pull request (the one step 7 left standing), its architect suspended with its session kept (the linger's own doing, which the state carries no deadline for), and still the one `pr-merged` notice |

What the proof stands in for lies outside the two rules: the broker is a native `nats-server`
rather than the testcontainers one the daemon's other NATS tests start, Dispatch is this package's
own stand-in grown into the board the daemon writes to and reads its issues from, Envoy's listener
is the package's `noticeListener`, GitHub answers that it knows no pull request so each disarm
finishes, the two Apps' tokens are placeholders, and no agent runs: each claim is launched by the
package's fake runtime and brought to ready over the daemon's own worker stream and HTTP routes.
The records that put the tree at each step are seeded through the store, as every daemon test
seeds its fixture, and each seed is printed as one. The configuration load, the store and its
migrations, the HTTP API, the claim supervision, `openWorkflow`, the JetStream consumers and the
outbox lanes are the real ones. `MERGED_ROOT_WAITS_EVIDENCE_DIR` (default a fresh directory under
`TMPDIR`, kept and printed) keeps the transcript, the `nats-server`'s log and the daemon's own,
which the driver writes through `LEGION_E2E_DAEMON_LOG`.

## no-required-checks.sh

```sh
LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
  [LEGION_E2E_NATS_BIN=<nats-server>] bash scripts/e2e/no-required-checks.sh     # → "no required checks e2e: PASS in <n>s", exit 0
```

Runs on any Linux host with `go` — no Docker, no cluster. `LEGION_TEST_PG_DSN` is required and
names the PostgreSQL the daemon migrates and runs on; the script starts none, so CI's service, a
devbox container and a pod's own embedded PostgreSQL all satisfy it. The broker is the run's own:
the `nats-server` `LEGION_E2E_NATS_BIN` names, or the pinned `v2.10.22` the script installs with
`go install` into a GOBIN of its own, started with JetStream on a port the kernel picks for it
(`-p -1`, read back from its log) and stopped on any exit. A run takes about seven minutes: the
daemon reads what each base branch requires at boot and every two minutes after
(`requiredChecksInterval`), and the proof waits for three of those reads. The four checks below
are [`lib/live-daemon-proof.sh`](#liblive-daemon-proofsh)'s, the run `merged-root-waits.sh` makes
too.

| check | what it holds |
| :--- | :--- |
| `prerequisites` | `LEGION_TEST_PG_DSN` is set (the DSN itself is not printed), `go` and `awk` are on PATH, and a `nats-server` is named or installed |
| `broker` | that `nats-server`, with JetStream, listening on 127.0.0.1 and ready |
| `driver` | a test binary of `TestABaseThatRequiresNothingGivesNoVerdictLive` (`internal/daemon/no_required_checks_live_test.go`, `-tags e2e`) |
| `proof` | the four steps below, each printing what it drove and what it read back between its own `== <name>` and `ok <name>` |

The driver boots the whole daemon through `run`, the function `legion start` calls, over that
PostgreSQL and that broker, with `max_fix_attempts` 1, and seeds three roots, each holding its
tree's slot on an open pull request at its first head, each on a base branch of its own so each
has its own required set: one in `testing` on a base that requires nothing, its set not yet read;
one in `reviewing` on a base that requires `ci`; and one in `testing` on a base that requires
`ci`, its one fix attempt counted at its head. Every fact enters by a transport production has: CI
settlements, the push and the synchronize as `notifications.github.…` envelopes on the
`ENVOY_NOTIFICATIONS` stream, and what each base requires as the daemon's own required-checks pass
reads it from the GitHub stand-in — the pull request (its base, head and `mergeable: true`), its
reviews (none), the base's rulesets (`/rules/branches/<base>`, switched between a lone
`pull_request` rule and a `required_status_checks` rule for `ci`) and the base branch itself,
unprotected, every read answered 2xx, since a failed read keeps the set last read. Every assertion
is a read of `GET /legion/v1/state` — raw, where a step holds that `checksVerdict` is absent — or
of the notices the daemon published to the Envoy listener stand-in. The store is read only to know
the daemon took what a step drove, which no route shows: the settlement it recorded
(`checked_head`), the set its pass read (`required`, polled for up to three minutes after each
switch), and the `processed_events` rows of the push and the synchronize.

| step | driven by | read back as |
| :--- | :--- | :--- |
| `nothing-required-failed-check` | CI's settlement of the first issue's head with `ci` failed, on a base that requires nothing | once the settlement and the boot's read of the empty set are recorded, `issues[<key>].pullRequest` has no `checksVerdict` key; over three seconds the issue stays in `testing` and its architect is told no `checks-red` |
| `the-same-failure-once-required-is-red` | the stand-in switched to make that base require `ci`, read by the daemon's next pass | `checksVerdict` `red`, the issue in `implementing`, and one `checks-red` reading `CI is red at a1: ci` — the control that step 1's absent verdict was the empty set's, not an unread set or a lost settlement |
| `a-passing-required-check-is-green` | CI's settlement of the second issue's head with `ci` passed, on a base that requires it | `checksVerdict` `green`, the issue still in `reviewing` |
| `a-push-onto-a-base-that-requires-nothing-leaves-the-attempt` | CI's settlement of the third issue's counted head `b1` with `ci` failed; the base switched to require nothing; the implementer's push of `b2` onto `b1` (a code path, pushed by the implement App) and GitHub's synchronize to `b2`; the base switched back to require `ci`; CI's settlement of `b2` with `ci` failed | the held red: `testing`, `checksVerdict` `red`, `fixAttempts` 1 and one `pr-blocked`; then no `checksVerdict` key and still `testing`; then head `b2` with `fixAttempts` still 1; then, at `b2`'s red, `implementing`, `checksVerdict` `red`, `fixAttempts` 1, one `checks-red` reading `CI is red at b2: ci` and still the one `pr-blocked` — where a push that stayed in the counted attempt leaves `b2`'s red held in `testing` |

What the proof stands in for lies outside the rule: the broker is a native `nats-server` rather
than the testcontainers one the daemon's other NATS tests start, GitHub is a REST stand-in that
also answers the repository a launch reads and a GraphQL root that knows no pull request, Dispatch
is the board both whole-daemon drivers share (`liveDispatch`, in
`internal/daemon/live_proof_test.go`), Envoy's listener is the package's `noticeListener`, the two
Apps' tokens are placeholders, and no agent runs: a send-back's implementer is launched by the
package's fake runtime, which provisions its own workspace so the daemon clones nothing, and never
registers. The records are seeded through the store, as every daemon test seeds its fixture, and
each seed is printed as one. The configuration, the store and its migrations, the HTTP API,
`openWorkflow` and its required-checks pass, the JetStream consumers and the outbox lanes are the
real ones. `NO_REQUIRED_CHECKS_EVIDENCE_DIR` (default a fresh directory under `TMPDIR`, kept and
printed) keeps the transcript, the `nats-server`'s log and the daemon's own, which the driver
writes through `LEGION_E2E_DAEMON_LOG`.

## lib/free-port.sh

Prints one TCP port that nothing listens on, for a stage proof to hand a binary that binds it
later. Stage 1, Stage 2, Stage 3 (through [`lib/rig.sh`](#librigsh)'s `pick_port`) and
`verifiers-staging-token.sh` take their ports from it.

```sh
port=$(bash scripts/e2e/lib/free-port.sh)                  # → 20000 ≤ port < first ephemeral port
second=$(bash scripts/e2e/lib/free-port.sh "$port")        # never $port
third=$(bash scripts/e2e/lib/free-port.sh "$port" "$second")
```

The port comes from 20000 up to, not including, the first port of
`/proc/sys/net/ipv4/ip_local_port_range` (32768 on a default kernel). The kernel autobinds every
outbound socket from that range, so between the pick and the bind, any connection the run opens
(go build's module fetches, a Postgres dial, a curl) could take a port inside it. The bind would
then fail with `address already in use`. The kernel never autobinds a port below the range.
`ss -ltnH` rules out a port something already listens on. Each argument is a port the caller has
already picked and not yet bound, and the helper never returns one of them, so several picks in
one run stay distinct.

### How it fails

It exits non-zero, printing the reason on stderr and nothing on stdout, when:

- `ss` is missing or fails (`ss: command not found`, or `ss`'s own error). An unanswered busy
  check is never read as a free port.
- the ephemeral range starts at 21000 or below (`the ephemeral range starts at <n>, leaving no
  room above 20000`).
- 50 draws find no free port (`no free port found below <n>`).

## lib/built-from.sh

Prints what a stage proof ran, so a run can be tied to a commit after its scratch directory, and
the binary in it, are gone. Stage 1, Stage 2, Stage 3, Stage 4a and Stage 4b print it through their own
`note`.

```sh
bash scripts/e2e/lib/built-from.sh "$root" "$work/legion"
# source: <commit> on <parent>                     (jj: the working copy and its parent; git: HEAD)
# legion: sha256 <hash>, stamped <commit> modified=<true|false>
```

When the working copy has changes, as in a negative control, which runs a base with the new
script copied in, the source line says so. The next line gives the sha256 of the diff, followed by
its `--stat`, so the run shows what it held and not only that something changed.
- Under jj the diff is `jj diff --git`.
- Under git it is `git diff HEAD`, taken against a copy of the index in which every untracked file
  is added as intent-to-add. The copied-in script is therefore in it, and the checkout's own index
  is never written. Without jj, or outside a jj workspace, it reads git, which is
what CI's checkout is.

Each binary is tied to that source by the stamp the Go toolchain writes at build time. The stamp has
two fields: `vcs.revision`, the commit the checkout's git HEAD named, and `vcs.modified`, whether the
tree differed from it. Under jj, HEAD is the working copy's first parent. The helper fails, naming
both sides, when a binary:
- carries no stamp (`-buildvcs=false`, or built outside a checkout);
- was stamped with a commit that is not the source's;
- has a modified flag that disagrees with the tree now.

The stamp does not hash a changed tree, so an edit made after the build to a tree that was already
changed goes unseen. Every caller runs the helper right after its build.

## lib/pack-plugin.sh

Packs one of this checkout's two plugins, `@sjawhar/pi-envoy` (`packages/pi-envoy`) or
`@sjawhar/pi-legion` (`packages/pi-legion`), the way the release packs it, and prints the tarball's
path: what `npm pack` ships (`package.json` `files`: `dist/` with the one bundle and the prepack's
`dist/skills`, the Legion plugin's `agents/`, and the packed manifest). Every script that installs a
branch-built plugin packs through it: [`lib/install-plugin-profile.sh`](#libinstall-plugin-profilesh),
which `controller-start-tmux.sh`, `stage2-tmux-supervision.sh`, `stage3-devbox-workflow.sh`,
`stage3-4b13b-acceptance.sh`, `stage4b-sandbox-tree.sh`, `dispatch-user-turns.sh` and the
skill-scenario rig (`packages/pi-legion/scripts/skill-scenarios/rig.sh`) call. The worker image packs
on its own, as the release does: `packages/daemon/docker/worker.Dockerfile`'s two plugin `RUN`s each
rewrite `omp.extensions` with `jq` and run `bun pm pack`, and the prepack
(`scripts/pi-plugin-prepack.sh`) refuses to pack any other `omp.extensions`, which holds all of
them to the same manifest.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
envoy=$(scripts/e2e/lib/pack-plugin.sh pi-envoy "$work/pack/pi-envoy")
legion=$(scripts/e2e/lib/pack-plugin.sh pi-legion "$work/pack/pi-legion")
```

The first argument is the package's directory under `packages/`, `pi-envoy` or `pi-legion`; any
other is refused, since the other workspace packages are not plugins. `<out dir>` is created when
missing, and refused inside the checkout (jj would snapshot the tarball) or when it already holds a
`.tgz`. Stdout is exactly one line, the tarball's path; every step's own output goes to stderr.

The steps are the release's, run in the checkout — a copy of the package cannot build, because the
prepack copies the repository's `skills/` and the bundle resolves `@legion/*` through the root's
`node_modules`:

1. save the package's `package.json` and arm an `EXIT` trap that copies it back byte-identical
   (`.github/workflows/release.yaml`'s `pi_envoy_build` and `pi_legion_build` jobs rewrite the
   manifest in "Point extensions at the packed bundle" and discard their tree, so only this script
   restores);
2. rewrite `omp.extensions` to `["dist/<entry>.js"]` with `jq`, where `extensions/<entry>.ts` is
   the committed manifest's one entry (the same step, and the `jq '.omp.extensions = …'` line of
   `packages/daemon/docker/worker.Dockerfile`'s plugin `RUN` for that package);
3. `bun pm pack --destination <out dir>`, whose `prepack` builds `dist/` (the release's "Pack
   extension" step, `scripts/pi-plugin-prepack.sh`); the bundle inlines `package.json`, so it is
   built while the manifest names the packed bundle, as the release builds it;
4. copy the saved manifest back and check it byte for byte (this script's own need: the checkout
   is reused, where the release's build job tree is discarded).

Each step cites its source by what it runs, never by line number: the lines move with every edit
above them. The release's version bump (its "Set release version" step) is not a step: the tarball
carries the checkout's own version. The saved manifest is written to the run's `mktemp -d`
directory, never beside `package.json`, so an interrupted run strands no `tmp.json` in the checkout.

The manifest is rewritten only for as long as the pack takes. The trap copies it back on every other
way out — a failed step, `SIGHUP`/`SIGINT`/`SIGTERM` (each routed through `exit`) — so a pack that
dies halfway never leaves the rewrite for jj to snapshot. The script ignores SIGPIPE, so this holds
when whoever read its stderr has gone too: bash's `Terminated` notice for an interrupted pack would
otherwise kill it before the trap ran. It keeps the run's status; if the copy back itself fails, it
says where the saved bytes are, leaves them there, and exits non-zero. Afterwards `jj status` is as
it was before the run: `dist/` is gitignored, and nothing else is written inside the checkout.

Runs in one checkout take turns at each package's manifest from the save to the copy back, under a
`flock` on the manifest itself (rewritten and restored in place, so the lock's inode lasts the
whole window); a run that has to wait says so on stderr. Without the lock, a run that starts while
another has the manifest rewritten saves that rewrite as its "before" and puts it back at its own
exit: both runs exit 0 and jj snapshots the rewritten `package.json`. Two stage proofs in one
checkout, or a stage proof and the skill-scenario rig, can pack at the same time, and the lock takes
them in turn; the two packages' packs never wait on each other.

## lib/install-plugin-profile.sh

Installs one of this checkout's two plugins, `@sjawhar/pi-envoy` or `@sjawhar/pi-legion`, into a
named OMP profile, packed the way the release packs it, so a stage proof runs the branch-built
plugins and the user's own profiles are never touched. A Legion pane needs both — the daemon's boot
gate refuses `pi-legion` without `pi-envoy` — so every stage calls it twice, with a `--dest` of its
own each time.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
. scripts/e2e/lib/omp-home.sh && make_omp_home "$work/omp-home"
envoy_manifest=$(scripts/e2e/lib/install-plugin-profile.sh --package pi-envoy --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/pi-envoy")
manifest=$(scripts/e2e/lib/install-plugin-profile.sh --package pi-legion --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/pi-legion")
# → $work/omp-home/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-envoy/package.json
# → $work/omp-home/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-legion/package.json
```

| flag | meaning |
| :--- | :--- |
| `--package <pi-envoy\|pi-legion>` | the plugin to install, by its directory under `packages/`; any other value is refused, since the other workspace packages are not plugins. The listing below is checked by that package's npm name. |
| `--profile <name>` | the OMP profile to install into (`OMP_PROFILE=<name>`). Refused when OMP would read it as its default profile — empty, all whitespace, or `default` — since that is the profile every plain `omp` uses. Any other name goes to OMP as given, and OMP refuses one it cannot use. |
| `--home <dir>` | the `HOME` Oh My Pi runs under for the profile, which is then `<dir>/.omp/profiles/<name>`: the run's own home from [`make_omp_home`](#libomp-homesh). Refused when it is the caller's own `HOME`, when `HOME` is unset or empty (it could not be compared), and when `<dir>/.omp` does not exist. |
| `--dest <dir>` | where the tarball is unpacked. `omp plugin install` links this directory into the profile rather than copying it, so it **is** the installed plugin and must outlive the run. Refused inside the checkout (jj would snapshot it, symlinks resolved first) and when it exists and is not an empty directory (an unpack over an earlier build would keep that build's stale files). |
| `--omp <mise tool spec>` | optional: the Oh My Pi build steps 2 and 3 run (`github:sjawhar/oh-my-pi@<version>`), `.omp-pin`'s by default. Stage 2 and Stage 3 pass the build their daemon and panes run, so no other build opens the profile; under `LEGION_E2E_MODEL_ROUTE=box-login` that profile's `agent.db` is the operator's login database ([`lib/install-box-login-route.sh`](#libinstall-box-login-routesh)). Refused when empty. |

The first four flags are required; each refusal names its flag and exits 2. Stdout is exactly one line, the
installed manifest's path as `HOME=<home> OMP_PROFILE=<name> omp plugin list --json` reports the plugin; for
`pi-legion` that is the manifest the daemon's contract gate reads under the same profile
(`pluginManifestPath`, `packages/daemon/internal/daemon/bootgate.go`). Every step's own output
goes to stderr.

The plugin is packed by [`lib/pack-plugin.sh`](#libpack-pluginsh), the release's pack steps run in the
checkout, into the run's `mktemp -d` directory (never beside `package.json`, so an interrupted run
strands no `.tgz` in the checkout). Then:

1. unpack the tarball into `<dir>` (`worker.Dockerfile`'s `mkdir -p /out/<package>` and
   `tar xzf ./*.tgz -C /out/<package> --strip-components=1`);
2. `OMP_PROFILE=<name> omp plugin install <dir>` (`worker.Dockerfile`'s
   `omp plugin install /opt/legion/<package>`);
3. `OMP_PROFILE=<name> omp plugin list --json` must show the package, by its npm name, at the
   tarball's version, enabled, and resolving to `<dir>`.

Steps 2 and 3 run the Oh My Pi `--omp` names, else the one the daemon pins (`.omp-pin`), under
`HOME=<home>`, from `<dir>`, as the daemon runs it: the build's own `bin/omp` by its absolute path
(`mise where <pin>`) under `mise x <pin> --`, never an `omp` looked up on the caller's `PATH`. An
operator's wrapper there (the devbox's `~/.dotfiles/shims/omp`) reads its own files from `HOME`,
which is the run's, and its own setup could rewrite the profile, whose `agent.db` is the operator's
login database under the box-login route.

Each step cites its source by what it runs, never by line number: the lines move with every edit
above them.

The script creates the profile and `<dir>` and removes neither; the caller does, with its work
directory, which holds both (the profile holds `plugins/` — the link and `omp-plugins.lock.json` —
and OMP's `logs/`).

### The natives download

OMP's native module (`pi_natives.linux-x64-modern.node` or `…-baseline.node`, the one the CPU
selects, 111 MiB at the pin; builds before 18.11.0 wrote every variant they embed, about twice that) lives in
`$HOME/.omp/natives/<omp version>/` — `$XDG_DATA_HOME/omp/natives/` when `$XDG_DATA_HOME/omp`
exists — and every profile under that `HOME` shares it (`getNativesDir`,
`@oh-my-pi/pi-natives/native/loader-state.js`); a profile has no natives of its own. OMP writes it
on its first run under a `HOME` that has not run this OMP version, and in this script that run is
`omp plugin install`. The run's own home links `.omp/natives` to the operator's
([`lib/omp-home.sh`](#libomp-homesh)), so a fresh CI runner, a container, or a newly bumped OMP pin
pays that once, into the operator's cache; on a box where the pinned OMP has already run,
a fresh profile pays nothing. Measured on the
devbox with OMP 18.2.2: build, pack, install and verify took 2 s into a new profile, the profile got
no `natives/` directory, and `~/.omp/natives/18.2.2/` was untouched.

## lib/model-gateway-url.sh

Prints `LEGION_E2E_MODEL_GATEWAY_URL`, the model gateway's Anthropic endpoint, once it is one a
stage proof can use. [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (Stage 2 and
Stage 3) and Stage 4a read the variable through it. The repository carries no default: the operator
sets it to the `baseUrl` of the `anthropic` provider in their own gateway route
(`~/.omp/agent/models.yml`).

```sh
gateway=$(bash scripts/e2e/lib/model-gateway-url.sh)
```

The URL is written into an Oh My Pi `models.yml` as one plain YAML scalar, so the helper exits 1
when the variable is unset, holds a character other than letters, digits and `:/._~-`, ends in a
colon (which YAML reads as a mapping key), or is not an `https://` URL. Its refusal names the
variable on stderr and never prints the value, which names production infrastructure; the scripts
that use it print that the route comes from the variable, not the URL.

## lib/model-gateway-audience.sh

Prints `LEGION_E2E_MODEL_GATEWAY_AUDIENCE`, the audience the model gateway accepts on a worker's
projected ServiceAccount token, once it is one a stage proof can use. Stage 4a and Stage 4b read the
variable through it and put it in place of the `${MODEL_TOKEN_AUDIENCE}` placeholder in the run's
copy of the operator route's `pod.yml`. The repository carries no default: the operator sets it to the
audience their own gateway verifies.

```sh
gateway_audience=$(bash scripts/e2e/lib/model-gateway-audience.sh)
```

The audience is written inside a double-quoted YAML string, so the helper exits 1 when the variable
is unset or holds a character other than letters, digits and `._:/-` (a quote or a backslash would
break the pod the daemon loads). Its refusal names the variable on stderr and never prints the value.

## lib/install-model-gateway.sh

Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's
own hawk login, the route a devbox's agent sessions use when it is configured for the gateway (`~/.omp/agent/models.yml`:
`X-Api-Key: !hawk-token`), so the tmux stage proofs' panes reach Anthropic with no provider key.
Stage 2 and Stage 3 run it under the operator's own `HOME`, before they move any XDG directory of
their own.

```sh
export LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic
key_command=$(bash scripts/e2e/lib/install-model-gateway.sh --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/model-gateway" --cache-dir "$work/model-gateway-cache")
# → $work/model-gateway/hawk-token
```

`LEGION_E2E_MODEL_GATEWAY_URL` is required: the gateway's Anthropic endpoint, the one
`hawk-token`'s default `HAWK_API_URL` mints keys for, read through
[`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh).

| flag | meaning |
| :--- | :--- |
| `--profile <name>` | the OMP profile to route. Refused when OMP would read it as its default profile (empty, all whitespace, or `default`), and when its `agent/models.yml` or `agent/config.yml` already exists. |
| `--home <dir>` | the `HOME` Oh My Pi runs under for the profile, whose files then go under `<dir>/.omp/profiles/<name>/agent`: the run's own home from [`make_omp_home`](#libomp-homesh). Refused when it is the caller's own `HOME`, when `HOME` is unset or empty (it could not be compared), and when `<dir>/.omp` does not exist. |
| `--dest <dir>` | where the key command and its log are written; created `0700`. Refused when it exists and is not an empty directory, and when its path holds a character other than letters, digits, `/`, `.`, `_` or `-`, since it is written into YAML as one `!command` word. |
| `--cache-dir <dir>` | where the key command keeps the key it minted; created `0700`. Refused when it exists and is not an empty directory, so a key left there is never served. A private directory of the run, never its evidence: the key is a live gateway credential. |

It writes `<dir>/hawk-token`, the key command: `hawk-token` (resolved on `PATH`) run under the
caller's `HOME`, `DBUS_SESSION_BUS_ADDRESS` and XDG base directories (a variable the caller has unset
is unset for it), for that one command. It appends one line per invocation, one per mint, one per
call that got no key and why, and `hawk-token`'s own stderr to `<dir>/hawk-token.log`, and one line
per call and its outcome to `<dir>/hawk-token.calls`
([`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh) reads it); stdout carries the key
alone. The profile's `agent/models.yml` points the `anthropic` provider at
`LEGION_E2E_MODEL_GATEWAY_URL` with `apiKey` and `X-Api-Key` both `!exec <dir>/hawk-token` (Oh My
Pi runs a `!command` through `/bin/sh -c`, and `exec` makes the key command the process it started,
so the call's clock starts when Oh My Pi started it), and its `agent/config.yml` pins every model
role, the roles Legion's task agents name included, to `anthropic/claude-opus-4-8`, sets
`enabledModels: [anthropic/*]`, and disables `amazon-bedrock`, `bedrock-mantle`, `google`,
`ollama`, `llama.cpp` and `lm-studio`. Stdout is the key command's path.

A pane cannot run `hawk-token` itself, which is why the command, and only it, gets the operator's
environment. Measured in a Go pane at `f1749048` whose profile named `!hawk-token` directly, by
running `hawk-token` under that OMP process's exact environment: it fails on mise (`No version is
set for shim: uv`), because the pane's `XDG_CONFIG_HOME` is the daemon's own (LEGION-206 P1) and
mise's global config is not there; with the operator's XDG directories it fails on the login (`no
usable hawk login`), because the pane carries no `DBUS_SESSION_BUS_ADDRESS`, the only address the
keyring client reads (`jeepney/bus.py`, `find_session_bus`); with both it mints. A pane's `HOME` is
the run's own ([`lib/omp-home.sh`](#libomp-homesh)), where neither mise's global config nor the hawk
login is, so the command gets the operator's `HOME` too. Panes run as the
operator's uid and can reach `/run/user/<uid>/bus` anyway, so the command gains nothing a pane
lacks, and every pane's environment stays as it is.

Oh My Pi falls back from a failing provider without a word, so the profile leaves it nowhere to
fall. In that same pane, OMP logged `model-config: !command value resolution failed` and answered
from `amazon-bedrock/us.anthropic.claude-opus-4-8` on the devbox's instance role, and an earlier
Stage 3 retro's scout subagent ran on Bedrock's `openai.gpt-oss-120b`. `enabledModels` holds each
session's own model to `anthropic`: a pane whose key command fails refuses to start (`No model
available matching enabledModels (anthropic/*) with usable credentials`), and the claim fails its
launch budget. Subagents and retries choose from every enabled provider rather than that list
(OMP's `resolveModelOverride` reads `getAvailable()`), so every provider a pane can use without the
gateway is disabled: in a pane's environment with `GEMINI_API_KEY` set, `omp models` lists
`amazon-bedrock`, `bedrock-mantle`, `google` and `anthropic` when the file sets only the default
role, and `anthropic` alone with the file as written; the three local servers are ones OMP uses
with no key. Every role is the one model because the gateway answers `claude-haiku-4-5`, the
model OMP gave that Stage 3 scout once Bedrock failed it, with `404 model not found`.

Its first mint is the preflight, before any pane exists. It exits 1 naming the cause when
`hawk-token`, `flock` or `timeout` is not on `PATH`, when `DBUS_SESSION_BUS_ADDRESS` is unset, when
`lib/model-gateway-url.sh` refuses `LEGION_E2E_MODEL_GATEWAY_URL`, when the keyring is locked
(`the operator's keyring is locked, so hawk-token cannot read the hawk login: unlock it (the
unlock-keyring skill) and rerun`), when the key command gets no key (quoting its own reason: the
outcome and `hawk-token`'s last stderr line), and when `hawk-token` prints anything but one JWT;
an argument refusal exits 2. The installer runs that one call with
`--preflight`, which exempts it from the key command's deadline (below): on a machine where
`hawk-token` has never run, its first-run build takes about a minute in the foreground, and it
happens here rather than inside a pane's ten-second `!command`. The key is never printed.

The key command mints once and keeps the key in `<cache-dir>/hawk-token.key` (`0600`) until
300 seconds before its JWT `exp`, or for 300 seconds when the key has none. Each `hawk-token` run
reads the hawk login from the keyring over the session bus, and the devbox's keyring daemon died
serving such a read at 09:33Z on 2026-09-24, relocking the keyring mid-run. A Stage 3 run
invoked the command 29 times, once per pane launch plus the preflight, and each was a mint before
the cache. Every call inside the window gets the kept key. A key the gateway refuses before then is
not re-minted: the proof's model turns fail, loudly, which is right for a proof. (A call's parent
is a helper the Oh My Pi process starts for each call, on 18.2.9 a child of the agent's own `omp`,
so the record names the agent by the Legion pane's role and generation instead.)

One call mints at a time: a wave of agents that starts as the kept key expires calls the command at
once, and concurrent mints on a loaded devbox run past `hawk-token`'s 9000 ms budget. A call that
finds no kept key takes an `flock` on `<cache-dir>/hawk-token.key.lock`, looks at the cache again,
and mints only when the key is still missing; every other call waits on the lock and serves the key
the holder kept. The mint runs with the lock's descriptor closed, since `hawk-token` can start a
detached refresh that would otherwise hold the lock for minutes.

Oh My Pi kills a `!command` 10 s after it starts it, so every call but the preflight gives up at
9500 ms of a clock that starts with its own process (under load bash can take half a second to
reach its first line), whether it is waiting on the lock or minting under `timeout`, and logs why.
A waiter that takes the lock with under 2000 ms left starts no mint: the fastest mint measured on
the devbox took 2006 ms, and each attempt is another keyring read. A waiter's wait is bounded by
one mint, since it started no earlier than the call it waits on, so a wave served by a mint that
succeeds is served inside every caller's ten seconds.

Every agent's call appends one tab-separated line to `<dir>/hawk-token.calls`, and to the caller's
`MODEL_GATEWAY_CALLS_FILE` when its environment names one: the time, the caller's pid, the
directory Oh My Pi ran it in, the agent, the outcome, and a detail. The fields, and the values the
agent and outcome take, are stated once, in the header of
[`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh), the file that reads them. The
installer's preflight is no agent's and is left out; a call that gets no key also says why on
stderr, which is where the installer reads the preflight's reason. The key command tells a spent
budget from a refused login only by `hawk-token`'s own sentence (`in <spent> ms of a <budget> ms
budget`, spent at least the budget), since the wrapper exits 1 either way; a `hawk-token` that
words it otherwise has a spent budget recorded as `failed`, never as a starve. A harness that gives
each agent its own file can judge one run from that run's directory alone.

The script creates the profile's two files, `<dir>` and `<cache-dir>`, and removes none of them; the
caller does, with its work directory and `<cache-dir>`.

## lib/install-key-file-model-route.sh

Routes a named OMP profile's model turns straight to Anthropic's own endpoint, keyed from a local
file: the key-file alternative to [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)'s
Hawk route, for a devbox that has no Hawk gateway at all. Mirrors the model-route pattern
[`deploy/kubernetes/operator-route/models-key-file.yml`](../../deploy/kubernetes/operator-route/models-key-file.yml)
gives Stage 4a's pods: an operator-supplied key, read fresh on every call, never minted and never
written into the tree.

```sh
scripts/e2e/lib/install-key-file-model-route.sh --profile <name> --home <dir> --dest <dir> --key-dir <dir> --key-file <file>
# → <key-dir>/anthropic-key
```

`--key-file` names a file holding the plaintext Anthropic key alone (one line; a trailing newline
is read past), refused unless it is a regular file only its owner can read (no group or other
permission bit) — checked once, here, before any pane exists, never read beyond that check, so a
key a caller rotates mid-run takes effect on the very next call with no restart.

`--dest` and `--key-dir` must be different directories (refused otherwise), and the caller gives
them different lifetimes. `--dest` gets only the call record (`<dest>/anthropic-key.log`,
`<dest>/anthropic-key.calls`), which carries no secret and is what a stage proof keeps as evidence
after every outcome. `--key-dir` gets the key command itself
(`<key-dir>/anthropic-key`): a plain file read, with `--key-file`'s resolved path baked into it, so
anyone holding that script can read the key for as long as `--key-file`'s path still holds one —
the caller must give `--key-dir` a path its own cleanup removes unconditionally, on every exit, not
the evidence directory or a workspace a failing run keeps for debugging, the same rule
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)'s `--cache-dir` already follows for
its own minted token file. Unlike `hawk-token` there is nothing to mint and no login to lose to a
relocked keyring, so there is no cache and no budget: every call is a plain, fast file read,
`served` or `failed` (there is no `timeout` or `killed` here, since nothing is minted or waited
on). It appends one tab-separated line per call to `<dest>/anthropic-key.calls`, and to the
caller's `MODEL_GATEWAY_CALLS_FILE` when its environment names one, in exactly the record shape
[`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh) reads — the installer's preflight
(`--preflight`) is no agent's and is left out, the same rule `hawk-token`'s own key command
follows. The profile's `agent/models.yml` points the `anthropic` provider at
`https://api.anthropic.com` with `apiKey` and `X-Api-Key` both `!exec <key-dir>/anthropic-key`, and
its `agent/config.yml` pins every model role to `anthropic/claude-opus-4-8` and disables every
other provider a pane could reach without the key file, the same shape
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) writes.

The script creates the profile's two directories, `<dest>` and `<key-dir>`, and removes neither;
the caller does, with its work directory, `<key-dir>` unconditionally and `<dest>` only when it
keeps no evidence.

## lib/install-box-login-route.sh

Routes a named OMP profile's model turns to Anthropic on the operator's own Oh My Pi login, the
one the devbox's own sessions use: the box-login alternative to
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) and
[`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh), for a devbox whose
operator lets Legion's test runs use that login. No key is minted, read or copied, and no gateway
is involved.

```sh
scripts/e2e/lib/install-box-login-route.sh --profile <name> --home <dir> --dest <dir> --operator-home <dir>
# → github:sjawhar/oh-my-pi@<the operator's version>
```

Oh My Pi keeps its stored logins in `<agentDir>/agent.db`, and a named profile's `agentDir` is
`<home>/.omp/profiles/<name>/agent`, with no setting that moves the database. The script makes the
profile's `agent.db` a symlink to `<operator-home>/.omp/agent/agent.db`; SQLite opens the database
through the link and writes its `-wal` and `-shm` beside the real file, so every pane shares the
operator's one database, never a copy. The script checks only that the operator's `agent.db` is a
regular file, then links to it: it never opens, reads, copies or hashes it, and runs no `omp`
command that touches a login. `--operator-home` is the operator's real home, which the stage proof
captures before it runs anything under the run's own `HOME`; the script never takes it from its own
`$HOME`.

The profile's `agent/config.yml` is the key-file route's (every model role
`anthropic/claude-opus-4-8`, `enabledModels` that one model, every other provider a pane could
reach disabled) plus `memory.backend: "off"`, so the run writes no memory into the operator's
database. No `models.yml` is written, so no `apiKey` or `X-Api-Key` overrides the stored login.

**The operator's build.** Processes sharing one `agent.db` refresh its OAuth token safely only on a
build with the refresh-race fix, first released as `18.12.2-sami.20261026-004924`; on a build
without it one process can spend the shared refresh token while another still holds it, which
disables the login for every session on the box. So the panes run the operator's own build, the one
the operator's sessions already run against that database, not `.omp-pin` (which also feeds the
worker image and CI and is not moved for this). The script reads it with
`cd / && HOME=<operator-home> mise ls --global --json`: from the filesystem root, so neither this
checkout's nor the caller's directory's mise config can answer in place of the operator's global
one. It takes the one active `github:sjawhar/oh-my-pi` entry's `version` and prints the tool spec on
stdout, which the stage proof writes into `omp_invocation`.

Each refusal goes to stderr and writes nothing into the profile. Exit 2 (arguments): a missing
flag or a flag with no value (`--<flag> needs a value: …`), OMP's default profile, a `--profile` that is not one plain path segment (`--profile '<name>'
is not one plain name …`: a letter or digit, then letters, digits, `.`, `_` or `-`, and no `..`), an
agent directory whose canonical path is not `<home>/.omp/profiles/<name>/agent` (`… resolves to <path>,
outside …`: a symlinked directory on the way), `--home` equal to `HOME` or `--operator-home` or without `.omp`, an
existing `agent/models.yml` or `agent/config.yml`, a profile `agent.db` that exists and is not the
symlink it would make (`… exists and is not the symlink to <db>; this installs a fresh profile's
model route only, and never replaces a profile's own database`), and a `--dest` that is not empty.
Exit 1 (the box):

- `<db> is not a regular file: the operator has no Oh My Pi login on this box to share (log in with the operator's own omp first)`, or `<db> is a symlink; …`;
- `the operator's global mise config pins no single active github:sjawhar/oh-my-pi, …`;
- `the operator's github:sjawhar/oh-my-pi is <v>, not <base>-sami.YYYYMMDD-HHMMSS, so it cannot be compared with the refresh-race fix (18.12.2-sami.20261026-004924)`;
- `the operator's github:sjawhar/oh-my-pi is <v>, base <b>; only base 18.12.2 is known here, …`: another base version is refused, never guessed at;
- `the operator's github:sjawhar/oh-my-pi is <v>, older than 18.12.2-sami.20261026-004924, the first build with the refresh-race fix; …`.

It writes `<dest>/box-login.route`, the link, the database's path and the build, for the evidence;
it holds no secret. Claude Code has no such route
([`claude-backend-tmux.sh`](#claude-backend-tmuxsh) says why).

## lib/box-login.sh

What a `LEGION_E2E_MODEL_ROUTE=box-login` run needs beyond the route: one run at a time, a teardown
that lets a pane finish a token refresh, and no link to the operator's login left behind. Sourced,
never run; Stage 2 and Stage 3 source it first.

```sh
. "$root/scripts/e2e/lib/box-login.sh"
take_box_login_lock || { echo "FAIL setup: another box-login proof is running on this box (the holder is named above)" >&2; exit 1; }
…
stop_box_login_processes $(run_processes)   # in the teardown
unlink_box_login_db "$profile_agent"
```

`take_box_login_lock` takes a non-blocking `flock` on one fixed file per operator uid,
`/tmp/legion-e2e-box-login.<uid>.lock` (`box_login_lock_file`), whatever `XDG_RUNTIME_DIR` or
`TMPDIR` says, so every box-login run of that operator meets the same lock. The stages take it
before they make a work or evidence directory. The lock stays on a file descriptor of the calling
shell for its whole life, and of every process it starts that keeps the descriptor, so a pane a run
leaves behind still holds it. A taken lock fails at once, naming the holder the file records:
`another box-login proof holds <file>: pid <pid> (<command>); one box-login proof runs at a time`,
or that the pid has exited and a process it started still holds the lock. Every pane of a box-login
run shares the operator's login database; one run at a time keeps the load on that login bounded
and each run's evidence its own.

`stop_box_login_processes PID…` sends SIGTERM to every pid, waits at most `box_login_stop_grace`
(10) seconds for all of them to exit, then SIGKILLs only those still alive, so a pane in the middle
of a token refresh can write the rotated token back before it goes. `unlink_box_login_db
AGENT_DIR` removes `AGENT_DIR/agent.db` when it is a symlink, the link itself and never what it
names; the stages call it once the run's processes are gone, since a process that opened the
database again after the link went would make a fresh database of its own in the profile.

## lib/model-gateway-unserved.sh

Says whether the key command [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) or
[`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) wrote
left an agent without a key, and why. Oh My Pi answers every call that gets no key the same way
(`No API key found for anthropic.`, exit 1, and for `omp -p` on 18.2.9 no session file at all),
whatever the cause, so a run cannot say for itself that it never got a model turn.

```sh
bash scripts/e2e/lib/model-gateway-unserved.sh --record "$run/model-gateway-calls"                                # a scorer: one agent run
bash scripts/e2e/lib/model-gateway-unserved.sh --notes "$evidence/model-gateway" "$check_started" "$check"  # a failed stage proof's EXIT trap
bash scripts/e2e/lib/model-gateway-unserved.sh --fresh "$evidence"                                         # a stage proof's setup
```

An agent is a working directory and an agent field together, so a relaunched pane, with its new
generation, is a new agent. An agent's last call decides whether it went without a key: Oh My Pi
retries a failed key command 30 s later and after a 401.

`--record` scores one agent run from the `MODEL_GATEWAY_CALLS_FILE` a harness named in that
agent's environment: a file of the run's own, which does not exist before the run, so every line
in it is that agent's. The skill-scenario rig runs one `omp -p` per run, which keeps its key for
the life of the process, so a starved run has no model turn and the record agrees with the
outcome. It exits 0 when the last call was served or the file holds none (no file included), `75`
(`STARVED, not scored`: rerun it) when that call ran out of time or had its mint killed, and `77`
(`KEY FAILED, not scored`) when the mint failed otherwise, which a rerun does not fix. A run from a
checkout whose key command predates the record carries the same signal without the reason: no
session file at all.

`--notes` is a stage proof's diagnostic: the `EXIT` trap runs it once it has decided the run
failed, guarded so that it never changes the exit status, and not after a hangup, an interrupt or
a termination (129, 130, 143), which stopped the run rather than failed it; a child a signal killed
is a failure and still gets them. It prints, after the check's own failure, each agent that could
have failed the check for want of a key: when it last got none, from where, and why.

- **An agent whose last call got no key is still without one.** It is listed whenever that call
  came, marked when it came before the check began: it had no key when the check asked it.
- **An agent served again after its last starve recovered.** It is listed, with when it was first
  served again, when that starve came at or after `<since>` less 30 s. The 30 s is the wait Oh My
  Pi keeps before it runs a failed key command again (`COMMAND_FAILURE_RETRY_MS`, 30 s in the Oh My
  Pi `.omp-pin` names): a request inside it fails with no key and runs no command, so it leaves
  no line, and a check's first calls can fail on a starve from just before the check began.
- **An agent served again before that window** held a key through the whole check, and is left out.

A person then sees whether starvation could explain the failure. A run-wide "not scored" would not
be honest: in Stage 4b the key command's one caller is the operator's controller, while every pod
uses its projected token, so a worker's failure cannot come from a starved controller.

`--fresh` is the refusal Stage 3, the 4b.13b acceptance and Stage 4b make first, before they write
anything into their evidence directory or set any trap, when the operator's evidence directory
exists and is not empty, or cannot be listed: whatever it holds is an earlier run's, whether or not
that run got as far as its key command, and this run must neither overwrite it nor read it as its
own. An absent directory, or an empty one it can list (every default), passes. Stage 4b prints its
verdict line in that refusal, since no trap exists yet to print it.

Every form exits 2 on an argument refusal (a `--record` in a directory that does not exist
included), and `--record` and `--notes` 1 on a record line they cannot read.

## lib/check-model-route.sh

Proves a tmux stage proof's agents reached the model only through its configured route: every
agent turn the isolated OMP profile recorded, each subagent's included, was served by the
`anthropic` provider, the one [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (the
Hawk gateway), [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (a
local key file) or [`lib/install-box-login-route.sh`](#libinstall-box-login-routesh) (the
operator's own login) routes and leaves enabled. Stage 2 runs it last, after `stop`. Stage 3 runs it once
every agent process has stopped and before it copies the transcripts, then again over the copies,
which must hold the same turns, sessions and subagents.

```sh
bash scripts/e2e/lib/check-model-route.sh --sessions "$work/omp-home/.omp/profiles/<profile>/agent/sessions" --control "$work/model-route-control"
# → 74 assistant turns in 5 agent sessions (0 of them subagents'), every one on the anthropic provider; negative control: … refused
```

Each session is a JSONL file under `--sessions`; a subagent's is the `<AgentName>.jsonl` in its
parent session's own directory. Every assistant turn must record `message.provider` `anthropic`,
and every `model_change` a model under `anthropic/`; a line that does not parse is skipped, since
a live agent may be mid-write. It exits 1 naming each session off the route with what it recorded,
and when no session, or no assistant turn, exists. Run over the transcripts of a Stage 3 run from
before either model route, it names the retro scout's Bedrock turn:

```text
check-model-route: agent sessions off the anthropic route:
…/RetroFreshEyes.jsonl: model change to amazon-bedrock/openai.gpt-oss-120b
…/RetroFreshEyes.jsonl: turn on amazon-bedrock/openai.gpt-oss-120b
```

Every run also carries its own negative control: the first session holding a turn is copied into
`--control` with that one turn rewritten as `amazon-bedrock/us.anthropic.claude-opus-4-8`, and the
same check must refuse the copy, or the run exits 1 (`the negative control passed`). An argument
refusal exits 2.

## lib/transcript.sh

The stage proof's transcript. Stage 2, Stage 3, Stage 4a, Stage 4b, `stage3-4b13b-acceptance.sh`
and `controller-start-tmux.sh` source it before their first output.

```sh
. "$root/scripts/e2e/lib/transcript.sh"     # sourced, never run
transcript_to "$evidence/transcript.log"
```

`transcript_to FILE` sends the calling shell's stdout and stderr to `FILE` as well as to whatever
read stdout before. It does this through a `tee` that ignores the signals a driver traps and
SIGPIPE.

- A signal to the process group, a reader that goes first (a supervised launcher's own `tee`,
  stopped with the run), or a full disk under `FILE` each cost `tee` at most the outputs they break.
  The driver and its cleanup never fail on a write. Without it, a group TERM after the reader had
  gone would end a driver with a TERM trap before its `EXIT` trap ran: bash writes its `Terminated`
  notice for the interrupted command to the dead pipe first.
- GNU `tee` stops once every output has failed and never reopens one. `/dev/null`, an output that
  never fails, is what keeps it draining once both the reader and `FILE` have failed.
- busybox `tee` keeps writing every output and reports errors at EOF, so it picks `FILE` up again
  once its disk has room. It rejects `-p`, which is why the trap, not `-p`, carries SIGPIPE.
- The `tee` is one of the run's processes: its argv names `FILE`, and its working directory is the
  caller's at the call. [`lib/rig.sh`](#librigsh)'s `run_processes` matches `$work` in either, so a
  caller keeps both outside `$work`, or opens `FILE` on a descriptor and passes `/dev/fd/N`, as
  Stage 2 and `stage3-4b13b-acceptance.sh` do with their transcript under `$work`.

## lib/rig.sh

The host-side helpers a stage proof sources for its rig: bounded waits, ports, the processes the
run starts, and their teardown. Stage 1, Stage 3, Stage 4b, the 4b.13b acceptance and the
controller, Dispatch user-turn and Claude Code backend proofs source it, as do the docs site's
broker and operator-walkthrough recorders and pi-legion's skill-scenario rig.

```sh
. "$root/scripts/e2e/lib/rig.sh"     # sourced, never run
```

The caller sets `root` (the checkout), `work` (the run's scratch directory: every process whose
working directory or command line names it is the run's), `evidence` (each service started by
`start_process` logs to `$evidence/logs/<name>.log`) and `timeout_hook` (empty, or a function a
timed-out wait runs before it fails), and defines `note` and `fail`, which exits. Stage 1 calls
only `real_bin_path`, so it sets `root` and `work` and defines `fail` alone.

| function | does |
| :--- | :--- |
| `until_true SECONDS WHAT CMD…` | runs CMD every half second until it succeeds. After SECONDS of wall time, each poll's own run included, it runs `timeout_hook` and fails naming WHAT. It says what it waits for on entry and every 60 s. A poll that never returns would hold the wait, so callers bound their remote calls. A guard that writes `$evidence/pane-endpoint-violation.txt` makes the next wait abort, naming the violation |
| `pick_port VAR` | assigns VAR a port from [`lib/free-port.sh`](#libfree-portsh) that no earlier pick of the run returned. It assigns in place: a command substitution would run it in a subshell and lose the run's set of picks |
| `start_process NAME CMD…` | starts CMD in the background, appending to NAME's log, and sets `NAME_pid` |
| `log_size NAME` | the size of NAME's log, the offset `await_start` reads a start's own lines from |
| `await_start NAME PID OFFSET SECONDS WHAT CMD…` | waits, bounded, for CMD to succeed while PID lives. It returns 2 when the service exited on `address already in use` after OFFSET, the one race a pick before the bind cannot close, so the caller picks again; any other exit fails naming the log |
| `stop_pid PID` | TERM, then KILL after 10 s; best effort, so a failed cleanup never hides the check that failed. An empty PID does nothing; a PID that cannot name one process of the run (0 however spelled, so any value with a leading zero, 1, a negative or non-numeric value, the calling shell or its process group's leader) is refused with a printed line, since `kill 0` signals the caller's whole process group |
| `stop_tree PID` | stops a background loop with every process under it: it freezes the loop, stops each child the same way, then kills the loop, so the `kubectl`, `jq` or `sleep` the loop was waiting on goes with it. It signals a pid only while its parent is the calling shell, so a pid the watcher left and another process reused is never hit, and refuses the same pids `stop_pid` does: `kill -STOP 0` would freeze the whole driver and a later `kill -KILL 0` end it. Stage 3's and Stage 4b's watchers are stopped with it |
| `run_processes` | prints every pid whose working directory or command line names `$work` |
| `real_bin_path DEST NAME…` | symlinks `DEST/NAME` to the real `NAME` binary `mise which NAME` resolves in the caller's own environment, for every `NAME`, and prints `DEST`: a devbox's own `jj` resolves through a shim chain that a pane's relocated `HOME` (`make_omp_home`) breaks, since mise has no pin for a directory whose global config a relocated `HOME` hides — `jj`'s own shim hands the call straight back to itself, exit 127. tmux also breaks for the daemon itself, which runs it with the pane environment's allow-list alone: under the daemon's tmux environment, which moves the XDG directories under the state directory (a relocated `HOME` has the same effect), a shimmed tmux run from a directory under the operator's real `HOME` finds that `HOME`'s mise config as a parent directory's config with no trust record, and exits 1, which the daemon's boot reads as a failed scrub of its private server's environment, not as no server, so boot refuses. Every `NAME` must resolve through the caller's own mise configuration; it refuses naming the tool if `mise which` fails. Stage 3 prepends `real_bin_path "$work/realbin" gh jj tmux` to the daemon's own PATH (`start_daemon`), which every pane inherits (`paneEnvAllowList`); Stage 1 prepends `real_bin_path "$work/realbin" tmux` to its daemon's PATH when its first tmux is a script or mise itself, and keeps a real tmux binary, such as CI's apt tmux, otherwise; Stage 2, the 4b.13b acceptance and the Claude Code backend proof, which do not call it, put a link to `mise which tmux` on their daemons' PATH the same way |

## lib/omp-home.sh

The run's own `HOME` for Oh My Pi, so a stage proof's isolated profile lives in its work directory
and the operator's `~/.omp/profiles` is never written or deleted. Stage 2, Stage 3, the 4b.13b
acceptance, Stage 4b's controller and the controller proof source it.

```sh
. "$root/scripts/e2e/lib/omp-home.sh"     # sourced, never run
make_omp_home "$work/omp-home"
```

Oh My Pi places a profile from the home directory alone: `os.homedir()/.omp/profiles/<name>`
(`getProfileConfigRoot`, `@oh-my-pi/pi-utils` `dirs.ts`). `XDG_DATA_HOME` moves only a named
profile's data, and only once `$XDG_DATA_HOME/omp/profiles/<name>` exists, so the profile's
`agent/models.yml` and `config.yml` would stay in the operator's home; `PI_CONFIG_DIR` reaches no
pane, since neither daemon's pane allow-list carries it. So the driver runs every Oh My Pi process
of the run under `HOME=<dir>`: the daemon, and through the pane allow-list every pane, the
controller, and each `omp` the libs run (`--home`).

`make_omp_home DIR` creates `DIR/.omp` and links `DIR/.omp/natives` to the operator's native module
cache ([The natives download](#the-natives-download)), so the run downloads nothing and removing
`DIR` removes the link, never the cache. A second call on the same `DIR` replaces the link rather
than writing one inside the cache through it (`ln -sfT`), and a real directory at `DIR/.omp/natives`
fails the call. It first exports `MISE_DATA_DIR` and `XDG_CONFIG_HOME` as
the operator's own, the paths each already resolves to, so what the daemon itself runs under `DIR`
finds what it did under the operator's `HOME`: `mise` the same pinned Oh My Pi, `secrets` its
secretsd config (the daemon resolves `provider_keys` with it), gh and jj their config. Panes are
unaffected, since the daemon moves their four XDG base directories under `<state_dir>/home`
(LEGION-206 P1).

jj reads a leading `~/` in a config value against `HOME`, which is `DIR` once the daemon runs. An
operator's `signing.key = "~/.ssh/<key>.pub"` would then name a key inside `DIR`, where there is
none. With `signing.behavior = "own"`, the first commit of every workspace clone the daemon makes is
authored by the operator, so it fails to sign, and no pane ever starts: the daemon log shows each
provision failing with `Couldn't load public key <dir>/.ssh/...`, and the run fails
`panes-pinned-to-the-rig-before-any-agent-turn` waiting for the architect to register. So
`make_omp_home` pins every `~/` value of the operator's jj config, in
`DIR/.jjconfig-operator-paths.toml`, to the path it names under the operator's `HOME`. It exports
`JJ_CONFIG` as the operator's own config files (`jj config path --user`, which names jj's default
files when `JJ_CONFIG` is unset), with that overlay last, so the daemon's jj signs as the
operator's does. A second call leaves its own overlay out when it reads the operator's config. A
`~/` value it cannot pin, inside an array or a table or under a quoted key, fails the call, naming
the key. Panes commit as their App's identity (`JJ_USER` and `JJ_EMAIL`), which `"own"` never
signs.

A proof's panes and controller therefore see none of the operator's `~/.gitconfig`, `~/.claude`,
`~/.codex` or `~/.aws`, which is what a Sandbox pod sees. The gateway key command alone runs under
the operator's `HOME` ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)).

## lib/leftovers.sh

A driver that dies with no chance to clean up (SIGKILL, a crashed host) leaves its daemon, listener,
agents and containers running. `refuse_leftovers PREFIX` is how the next run of the same stage
notices: Stage 2 (`legion-e2e2`), Stage 3 (`legion-e2e3`), Stage 4b (`legion-e2e4b`) and the
controller proof (`legion-e2e-controller`) run it before they start anything.

```sh
. "$root/scripts/e2e/lib/leftovers.sh"     # sourced, never run
refuse_leftovers legion-e2e3
```

Everything a run starts carries its driver's pid: each container is named `<prefix>-<role>-<pid>`,
and every process of the run holds its work directory, `/tmp/<prefix>.<pid>.<random>`, in its argv,
its working directory (an agent) or its environment (a bridge handed the run's state directories).
The check fails, through the caller's `fail`, naming each container and process whose driver is no
longer alive and the command that removes it (`docker rm -f -v <name>`, `kill <pid>`). A driver
counts as alive only while its pid is a live, non-zombie process that started no later than the
container or process it would own, since pids are reused. Only processes in the caller's own pid
namespace are read, since a pid means nothing across namespaces (an agent box's run is its own).
What a live run started is left alone, since Stages 2 and 3 take no lock and two lanes may run one
at once.

## lib/operator-dispatch.sh

The operator's Dispatch, for a script run from the operator's shell. Stage 3 and
`stage3-4b13b-acceptance.sh` read their production audit's server and bearer through it, and
`packages/pi-envoy/scripts/smoke-btw.sh` the Dispatch it asks as an agent.

```sh
. "$root/scripts/e2e/lib/operator-dispatch.sh"     # sourced, never run
operator_dispatch     # sets operator_dispatch_file, _url, _token and _notice
operator_dispatch_hold_token     # moves DISPATCH_TOKEN into operator_token, out of the environment
production_baseline     # sets prod_dispatch_url, prod_token, prod_baseline (`none` without Dispatch)
```

The two Stage 3 scripts call `operator_dispatch_hold_token` once at start, so nothing a run starts
inherits the operator's production bearer, and `production_baseline` to open their audit's window:
it resolves the operator's Dispatch with the held bearer and reads the newest event id of the
issues updated in the last 24 hours, through a 0600 header file under the caller's `$work`
(`prod_header_file`).

`operator_dispatch` runs `bun scripts/e2e/lib/operator-dispatch.ts <bearer file>`, which resolves
Dispatch with `@legion/envoy-client` as every command does, so no script keeps its own copy of the which-file or
the same-server rule: the envoy.json `envoyConfigPath("command")` chooses (`ENVOY_CONFIG_FILE`,
which the operator's own session sets, then a named omp profile's `PI_CODING_AGENT_DIR`,
`CLAUDE_CONFIG_DIR` only under `CLAUDE_CODE_CHILD_SESSION=1` (a Claude Code shell), and
`OPENCODE_CONFIG_DIR`, then `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`), the working
directory's `.envoy/envoy.json`, and `dispatchSettings` over the environment as it is at
the call (`DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, `DISPATCH_TOKEN`; a file's `dispatch.token` goes
only with the server written beside it, and a `!` token runs). With Dispatch on it sets the
envoy.json's path, the URL and the bearer; when nothing configures Dispatch or a file turns it off,
the URL and bearer are empty and `operator_dispatch_notice` says why. A relative path the rule would
use, a file that cannot be read, or a URL or token that does not resolve fails the call through the
caller's `fail`, naming it and never the token. A checkout's `.opencode/envoy.json`, which is not
read, is named on stderr with its rename while the working directory has no `.envoy/envoy.json`.
The TypeScript entry never prints the bearer, so an agent that runs it directly puts none in its
transcript: it prints `{"file","url"}` and writes the bearer to `<bearer file>`, which it creates
owner-only (0600) and never over a file already there, and `operator_dispatch` passes a path in a
private directory it makes with `mktemp -d`, reads the bearer from there and removes the directory.
The bearer then stays in the caller's unexported variable, so it reaches no argv and no process's
environment. The entry imports the checkout's `packages/envoy-client`, so the checkout's
dependencies must be installed (`bun install`).

## lib/workflow.sh

The vocabulary of a stage proof that drives Dispatch issues through the Go daemon's workflow with
real agents: Dispatch, the daemon's state, each phase's worker, the smoke repository's pull request
under the proof human, the handoffs the daemon accepted, the notices, and the negative controls.
Stage 3, Stage 4b, the 4b.13b acceptance and the Claude Code backend proof source it after
[`lib/rig.sh`](#librigsh).

```sh
. "$root/scripts/e2e/lib/workflow.sh"     # sourced, never run
```

The caller sets `work` (holding `legion`, `legion.yaml`, `operator-token` (the bearer `claims_list`
presents to `legion claims list`), `dispatch-auth-header` and
`dispatch-human-header`, the last two written by the caller or by `write_dispatch_header`),
`evidence`, `project` (the Dispatch project; the daemon writes as `legion-daemon:<project>`),
`repo`, `dispatch_base`, `dispatch_actor` (the session every proof-human write names as its actor,
or empty), `dispatch_token_file` (the file holding the agents' Dispatch bearer, which
`write_dispatch_header` reads: Stage 4b's `LEGION_E2E_DISPATCH_TOKEN_FILE`), `pg_container` (the
daemon's Postgres), and, once they exist, `pr_number` and `smoke_file` (the one product file the
pull request's first implementation changed). It defines `note`, `pass` and `fail`, `scrub` (the
filter that hides the run's private hosts in what it prints, which only a caller of
`ensure_project` needs), and the runtime seam, the only reads that depend on where an agent runs:

| seam | the caller's definition |
| :--- | :--- |
| `assert_claim_endpoints ISSUE ROLE` | fails the check when the claim's process could reach a service outside the rig; every instruction runs it first |
| `claim_session_text ISSUE ROLE` | prints the claim's session file, and fails when there is none |
| `workspace_jj ISSUE ARGS…` | runs `jj ARGS…` in the issue's workspace |

`new_issue TITLE [PARENT] [SPEC]` creates each issue a proof drives. A root carries the Dispatch
label `legion`, which hands it to the Go daemon (the daemon admits no root without it), and `SPEC`
as its primary document, `smoke_spec` when `SPEC` is omitted. A child carries neither, since it runs
under its root's tree.

`write_dispatch_header` writes the two header files from `dispatch_token_file`; Stage 4b's
`prerequisites` calls it first, before the lock, and `read_bearers` calls it again for the run
proper. `ensure_project` creates `project` as a proof-human write, naming `dispatch_actor`: 201 and
409 `PROJECT_EXISTS` both leave it ready, any other status fails the check naming the status and
the body, and a failed connection fails it in one line naming curl's error.
`dispatch_human METHOD PATH [BODY]` is every proof-human write, over `dispatch-human-header`;
`dispatch_agent METHOD PATH BODY` is a write in an agent's name over `dispatch-auth-header`, the
agents' bearer, whose body gains the stand-in session `stage3-architect-stand-in` as its `actor`
(a bearer caller must name one; the stand-in holds no claim), for a proof step whose author must be
a session, such as the decision block Stage 3 inserts into the spec as its architect would.
`lib/ensure-project.test.ts` drives `write_dispatch_header`, `ensure_project` and `dispatch_agent`
against a fake `curl`.

`require_proof_human` is the proof human's precondition, which every stage proof that writes to
GitHub as the proof human runs in `prerequisites` before its first `gh` call. It asks the devbox
`gh` which account it acts as, through GraphQL's `viewer` with `GH_REPO` naming `repo` (the owner
the dotfiles shim routes by): an App installation token answers `viewer` with the App's bot login,
where REST's `GET /user` refuses one (403, "Resource not accessible by integration"). Unless the
answer is `sjawhar-agent[bot]` it fails the check in one line: the account it found ("no account"
when `gh` gave none, and why when it gave no answer within the probe's 60 s), the requirement (the
operator's own Oh My Pi session, not a Legion pane, with no personal `GH_TOKEN` in its
environment), and whatever `gh` wrote to stderr, where the shim names an inherited `GH_TOKEN`. Each
harness's GitHub teardown (Stage 3's `close_unpassed_run_pull_requests`, 4b.13b's `github_cleanup`,
Stage 4b's `remove_run_branches`) returns without a `gh` call unless the check passed.
`lib/proof-human.test.ts` drives the check and Stage 3's teardown against a fake `gh`
(`bun test scripts/e2e/lib`, which CI runs).

`read_app_logins` proves the run's two role Apps against GitHub and reads the bot login of each,
as GitHub names it today, and every check that names an App compares with it: no script carries an
App's name or id, so an App renamed on GitHub changes what the run reads and no line of it, and a
name another App takes over later is never mistaken for Legion's. It runs
[`check_github_apps`](#libgithub-appssh) over the run's `$work/github-apps.yaml`, whose
`lib/app-logins.ts` signs a JWT with each App's key from its `github_apps.<role>.private_key_command`
(run with `sh -c`, as the daemon runs it, the key held in that process alone) and reads the App's id
and slug from `GET /app`, so the login is `<slug>[bot]`, the one the daemon reads at boot for the
identity each role commits, pushes and reviews as. The two Apps resolving to one login fail it, as
the daemon refuses at boot, and so does a read of either App whose answer names another App id,
lacks the write permission the role's work needs, or gives no slug, the run failing with the
reason the helper names by the App's role and id: a GitHub
that never answered, an answer that is not 2xx (GitHub's 401 for a key that is not the id's App),
an answer naming another id (both ids named, never the key), one without the role's permission
(which refuses the two Apps configured the other way round), or one with no slug. A stage proof
calls it once in `prerequisites`, after `require_app_inputs` and `write_github_apps` and before its
first write anywhere and any check that needs a login. `role_app ROLE` then prints the login of the App the role
acts as (the implement App's for the implementer and the merger, the review App's for every other
role, the controller's included), as REST and git name it, and `role_app_slug ROLE` the bare slug
GraphQL names the bot by; either fails, saying so, when `read_app_logins` has not run.
`lib/app-logins.test.ts` runs the helper, and `read_app_logins` and the input checks over it,
against a stand-in GitHub, and `lib/reviewer-thread.test.ts` drives `reviewer_thread` with the
logins set.

`clean_smoke_main` is Stage 3's `smoke-main-clean` and the cleanup inside Stage 4b's `done`: one
fixture pull request, on the branch `proof/clean-main-<project, lowercased>`, that removes the
handoffs and learnings proof merges leave on the smoke `main`. Stage 3's project is a fresh key per
run, but Stage 4b's is `LEGSMOKE` in every run, so the branch's name alone cannot tell the run's own
from one an earlier run left. The run records the branch as its own (`smoke_cleanup_branch`) once it
has read that no such branch exists (`smoke_branch_state`, through `matching-refs`, which answers an
absent branch with an empty list), just before it makes it, and empties the record once the merge
has deleted it; it fails, naming the branch, when one is already there. `close_smoke_cleanup` is
both stages' teardown of a run that stopped in between (Stage 3's `close_unpassed_run_pull_requests`,
Stage 4b's `remove_run_branches`): it closes each open pull request whose branch is exactly the
recorded one, with a comment naming the run's script, project and pid, deletes the branch, prints
one line per pull request (its URL, and gh's reason when the close failed) and for the branch, and
returns 1 when the pull request may still be open or the branch remains. Without a record it makes
no `gh` call. `lib/smoke-cleanup.test.ts` drives both teardowns against a fake `gh`.

Every wait for an issue to reach one phase is `wait_for_phase ISSUE PHASE [SECONDS]`: `phase_seconds`
(600 s, the proof's budget for an agent's turn), unless
the phase's worker runs a whole loop and the caller passes its own bound: a correction round, the
retro, and planning, whose planner runs its gap analyst and up to three plan-review rounds before
its handoff (Stage 4b's `plan_seconds`, 2700 s). `round_correction_pushed ROUND` accepts the round's
line only as an addition in `smoke_file`'s patch, never in a notes file or in a `.legion/` handoff
that quotes it.

`request_changes_as_reviewer ISSUE ROUND BODY [NOTE]` is a scripted changes-requested round: it
tells the reviewer pane its decision and the review's text, quoted, which the reviewer ends with
the Legion footer naming its own session, as its role ends every review. The daemon sets aside a
review-App review whose footer does not name the session recorded on the reviewer's claim, and that
session is recorded at the reviewer's first completion, so a round from the second on that told the
reviewer to post a body verbatim, footer-less, would decide nothing and leave the issue in
`reviewing`. `NOTE` is a further step the round asks of the reviewer, never part of the review. The
round waits for `reviewer_requested_changes_from ROUND SESSION`: the review App's ROUND-th
changes-requested review whose last footer names the session the daemon's state gives the reviewer,
read as the daemon reads it. `lib/reviewer-footer.test.ts` drives both against a fake `gh`.

The handoff checks read the daemon's phase record (the `phases` table joined to `issues`), not the
ids of the facts it processed, so they hold whatever format a handoff event id takes. A role's
`handoff_commit` is the commit its `handoff_complete` reported at its last completion the daemon
accepted (for a file-backed phase the pushed commit carrying the handoff, which the `legion` tool
finds in the pane and refuses unpushed, so a push before the completion is the role's rule; for
retro and READY the commit the workspace stands on), and the daemon empties it
when a transition starts that role on a new phase (`clearHandoff`,
`packages/daemon/internal/workflow/effects.go`); the implementer's `rounds` counts its returns
to implementing. Read right after the transition a completion caused, a role's non-empty
`handoff_commit` is that phase's own: `assert_round_handoff ISSUE ROUND` (testing reached on the
implementer's completion of that round; implementing moves to testing only once that handoff is
recorded), `assert_handoff_committer ISSUE ROLE PHASE ROUND` (that commit is authored and committed
by the role's own App, read with `workspace_jj`), and `retro_reported` (the implementer's handoff while
the issue is merging or awaiting merge).

The production check after the close is no phase: a look's report is a `handoff_complete` with a
verdict and no commit, moves no phase and writes no handoff, so it is read from `legion state
--json`'s `.issues[KEY].check` (`repo`, `pullRequest`, `mergeCommit`, `openedAt`, `looks`,
`lookOpen`, `lastWake{kind, detail, at}`, `nextWakeAt`, `last`), present while the check is open
and omitted once it closed. `check_open ISSUE` (the check is open), `check_opened_on ISSUE MERGE`
(open on the merge commit `MERGE` at its first look, woken by the merge), `check_look_open ISSUE N
[KIND]` (look `N` is open, woken by `KIND` when named; `bound` also requires `last`),
`check_look_reported ISSUE N` (look `N` reported and the check still open, which only a `pending`
before the last look leaves: a pass or fail closes it), `check_closed ISSUE` (the state omits the
check and the issue is `done`), `last_look_started ISSUE` (look 2 open, woken by the bound and
`last`, or the check closed since, so a read after the fact is not failed by an implementer that
reported the last look first), `check_look_logged ISSUE N CAUSE` (the daemon log's `workflow:
production check look started` for look `N` on `CAUSE`, `last` for the bound's, the record of a
look the state no longer shows) and `check_closed_as ISSUE OUTCOME` (the daemon log's `workflow:
production check closed` for the issue with `recorded`, `defect` or `not_run`) are each a predicate
a wait polls or an assertion reads. Beside them, `claims_list` reads `legion claims list --json` over the operator
bearer, `issue_claim_states ISSUE` prints each claim's role and state, `claim_pending_phase ISSUE
ROLE` the phase of the task the claim holds (how `accepting` is shown to start no worker: the
implementer holds no retro task), `claim_states_are ISSUE IMPLEMENTER OTHERS` holds the issue's six
claims to the implementer in one of the states `IMPLEMENTER` names and every other claim in one of
`OTHERS` (live at a look, `suspended` between looks, `retired` once the check closed, beside five
`suspended`), and `phase_change_logged ISSUE FROM TO TRIGGER` finds the daemon log's `workflow: phase
changed` line for a move (`sign_off`, `pull_request_merged`).

## lib/github-apps.sh

Legion's two role Apps as a live proof takes them, sourced by `lib/workflow.sh` (so by every stage
proof that sources it) and by Stage 4a directly. Each App is an id and a secret-store key name,
which two optional operator inputs give:

| input | what it names |
| :--- | :--- |
| `LEGION_E2E_IMPLEMENT_APP_ID`, `LEGION_E2E_REVIEW_APP_ID` | the App's numeric id |
| `LEGION_E2E_IMPLEMENT_APP_KEY_NAME`, `LEGION_E2E_REVIEW_APP_KEY_NAME` | the secret-store key the key runner resolves to the App's private key (base64-encoded PEM) |
| `LEGION_E2E_APP_KEY_RUNNER` | the key runner, one for both Apps: a command, by name or path, that runs `<runner> <NAME> -- <command>` with the key `NAME` holds in that command's environment alone. `secrets` by default; a store's own runner otherwise (`agent-secrets` reads the broker with no local fallback). Legion names no store |

Each App input defaults to the pair the live proofs have always used, which the lib holds in one place
(`default_<role>_app_id`, `default_<role>_app_key_name`), so a run here sets none of them and acts
as it always has, and a deployment with its own Apps sets all four.

`require_app_inputs ROLE...` reads each role's pair, taking the default for an input that is
unset, and fails naming the input that is set but malformed: an id that is not a number of at most
12 digits, or a key name that is not letters, digits and underscores of at most 64 (so it is safe
in a shell command and as a variable name, and a pasted key is refused by its length). A refusal
never prints the value. With both roles read it also refuses one id or one key
name for both Apps. `write_github_apps ROLE...` writes `$work/github-apps.yaml`, the `github_apps`
block a run's `legion.yaml` includes as it is: each App's `app_id` and a `private_key_command` that
runs `<runner> <name> -- sh -c …` to decode the store's Node-style base64 into the PEM on stdout. It
refuses, writing nothing, a role the last `require_app_inputs` did not read.
`check_github_apps ROLE...` runs [`lib/app-logins.ts`](#libworkflowsh) `--roles` over that file and
fails the check unless GitHub's `GET /app`, under a JWT each key signs with its id as issuer,
answers that id and the write permission the role's work needs (the implement role's `contents`,
the review role's `checks`): a key of another App is refused by GitHub's 401, an answer naming
another id is refused naming both ids, and the two Apps configured the other way round are refused
by the permission, since the implement App holds `checks: read` alone. The key never enters the
script's shell, an argv or a file. A run
refuses before its first write anywhere: Stage 3, 4b.13b, the Claude Code backend and Stage 4b
check both Apps (`read_app_logins`), Stage 4a, which acts as the implement App alone, checks that
one. `TestRealGitHubCredentialSurface` and `TestRealGitHubReadyHoldsTheHeadToRetrosCommits` source
this lib and run its `require_app_inputs`, `app_key_command` and `check_github_apps` (through
`liveProofApps` and `proveLiveProofApps` in `packages/daemon/internal/api/real_github_test.go`;
`proveLiveProofApps` checks the very block the proofs act with), so they hold no copy of the pair,
its checks or its key command (the key runner included), and they prove both Apps as the stage
proofs do before they act. Stage 4a hands its Go harness the key runner beside the implement App's pair.
The checks run in the C locale whatever the caller's, so a UTF-8 locale does not admit fullwidth
digits or accented letters. `require_app_inputs` also fills `app_key_unsets`, the
`env` argument list (`-u NAME` …) every proof passes to a process it starts, which strips each
role's key name, the default pair's two, and every exported variable whose name holds
`PRIVATE_KEY`, the proof human's App key included. `lib/app-logins.test.ts` drives the three
functions under a stage script's shell options against a stand-in GitHub: an unset input taking its
default, a malformed one refused by name with its value unprinted, `write_github_apps` refusing an
unread role, `app_key_unsets` leaving no App key in a child's environment, an answer naming another
App id refused naming both ids, a role's missing write permission refused, and a match accepted.

## lib/namespace-rig.sh

The namespace rig of a stage proof that runs pods in a shared cluster namespace. Stage 4a, Stage 4b
and [`claude-backend-sandbox.sh`](#claude-backend-sandboxsh) source it.

```sh
. "$root/scripts/e2e/lib/namespace-rig.sh"     # sourced, never run
```

The caller sets `operator` (the admin kubectl context), `namespace`, `run_label` (the run's
`legion.dev/project` label, named apart from `lib/workflow.sh`'s `project`, the Dispatch project
key), and either `label_prefix` (the prefix every run label of the proof carries, Stage 4a's
`s4a-`) or `label_exact` (the one fixed label the proof owns while it holds its run lock, Stage
4b's `legsmoke`), `record` (a file with one Sandbox name per line), `work`, `evidence`, and `torn_down` and
`compared` empty; it defines `begin`, `note`, `pass` and `fail`, which exits.

| function | does |
| :--- | :--- |
| `op ARGS…` | `kubectl --context $operator -n $namespace ARGS…`, bounded at 300 s (`timeout --foreground`, so a Ctrl-C still reaches kubectl) |
| `snapshot FILE` | writes the run's own Sandboxes, Secrets, PVCs, pods, ConfigMaps and Services (`kinds`), sorted: those labelled with its project, and those with no project label that `run_named` counts as the run's. It fails when either listing does |
| `run_named LIST` | prints the lines of a `-o name` listing whose object name carries the run's label as written, or as a claim token carries it (every character outside `a-z0-9` dropped): every object the run's daemon and harness create is named from one or the other |
| `teardown` | runs once and never fails. It refuses a label without `label_prefix`, or other than `label_exact`, so a mistyped label cannot select another run's objects; deletes every recorded Sandbox by name, then the Sandboxes and ConfigMaps labelled with that exact project; then lists the project's objects every 2 s, up to 150 listings, until none is left. Secrets and PVCs the Sandboxes' own deletion has not taken by the 90th listing are deleted by that exact label once, on the first listing from then on that answers; three failed listings in a row end the wait, naming the context and its error |
| `namespace_clean` | the check `namespace-clean`: a fresh snapshot, written to `$evidence/namespace-after.txt`, must equal `$evidence/namespace-before.txt` |

## lib/session-db.sh

The reads a stage proof makes of its run's scratch session database, where every pod of the run
keeps its sessions (`session_store: postgres`): Oh My Pi's table `omp_session_files` and its parts
`omp_session_files_parts`, keyed by the path the session's file would have had. Stage 4b and
[`claude-backend-sandbox.sh`](#claude-backend-sandboxsh) source it; each starts the database's
container itself.

```sh
. "$root/scripts/e2e/lib/session-db.sh"     # sourced, never run
```

The caller sets `session_db_container`, the container running the database (owner `legion`,
database `sessions`). Every read goes through that container, with no client on the devbox.

| function | does |
| :--- | :--- |
| `session_psql NAME VALUE` | runs the SQL on stdin with `psql -A -t` as `legion`, VALUE the psql variable NAME, which the SQL quotes as `:'NAME'`, so no value is spliced into it; an SQL error is its exit status |
| `session_text PATH` | prints the session the table holds at PATH whole, the row's content and then its parts in `start_offset` order, from one statement; a session the table does not hold, or a read that fails, fails naming PATH |
| `session_byte_len PATH` | prints the row's `byte_len` at PATH; an answer that is no number (no such row), or a read that fails, fails naming PATH |

## lib/bridge-tunnel.sh

The reverse-tunnel bridge a stage proof uses when its pods cannot reach the devbox at all
(`LEGION_E2E_POD_REACHABILITY=bridge`, GKE's shape). Two callers source it, each after
[`lib/namespace-rig.sh`](#libnamespace-rigsh): Stage 4a, which bridges two ports (the worker stream
and the session database), and [`claude-backend-sandbox.sh`](#claude-backend-sandboxsh), which
bridges six (its daemon's API and worker stream, the scratch Dispatch, Envoy listener, NATS and
session database).

```sh
. "$root/scripts/e2e/lib/bridge-tunnel.sh"     # sourced, never run
```

The caller sets `root` (the checkout, for the pinned chisel client's `go install`), `operator` (the
admin kubectl context, the only one this file uses), `work` (the run's scratch directory),
`bridge_name` (the run's deterministic bridge pod/Service name, set before `bridge_start` runs, so
the cluster-DNS name pods dial is known from it alone) and `run_label`, beside `lib/namespace-rig.sh`'s
own (`namespace`, and `op`, which every read and write here goes through) and `begin`, `note`,
`fail` and `pass`. It defines `kill_harness`, which stops whatever the run drives: the watchdog
calls it before it sends the script `SIGTERM`, and runs under the caller's `errexit`, so an
undefined `kill_harness` would end the watchdog before it signals. This file defines
`bridge_start` and `bridge_stop`.

| function | does |
| :--- | :--- |
| `bridge_start PORT…` | for one or more ports (a call naming none fails), creates a run-labelled Secret (`$bridge_name-auth`, a random `AUTH` value), then a chisel server Pod (`--host 127.0.0.1`, reading `AUTH` from the Secret through `secretKeyRef`, never argv) and a ClusterIP Service naming it, with one Service port per named port, named `p<port>`, both `create`d so no `last-applied-configuration` annotation ever carries the auth; forwards the devbox's `kubectl port-forward` to the pod's control port, starts a chisel client with `AUTH` in its own environment opening one reverse listener per named port, each to `127.0.0.1:<port>` on the devbox, waits for every one in the bridge pod's log, and starts the watchdog that restarts a dropped port-forward (up to 60 s) and, when it cannot or the client's own bounded reconnect budget is exhausted, calls `kill_harness` and fails the run |
| `bridge_stop` | runs once and never fails: stops the watchdog and the chisel client, kills the port-forward (both the one it is tracking and the current global one, so a restart in flight is never orphaned), and deletes the Secret, the Pod and the Service by their one exact name |

## lib/kind-sandbox.sh

A throwaway kind cluster running Agent Sandbox's controller under gVisor, for a live proof of the
Sandbox runtime on an amd64 Linux host. `burstable-kind.sh`, `controller-sweep-kind.sh` and `share-volumes-kind.sh` source it.

```sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"     # sourced, never run
```

The caller defines `note` and `fail`, which exits, and calls `kind_sandbox_teardown` from its `EXIT`
trap. `LEGION_E2E_KIND` names the kind binary when it is not on `PATH`.

| function | does |
| :--- | :--- |
| `kind_sandbox_create NAME WORK` | creates the cluster `NAME`, its kubeconfig a file of `WORK` (`kind_kubeconfig`) and its context `kind-NAME` (`kind_context`); labels its one node (`kind_node`) `legion.dev/pool=legion`, the pool the runtime places every pod on; creates the `legion` namespace and checks kind's `standard` storage class |
| `kind_k ARGS…` | `kubectl` against that kubeconfig and context alone, so the caller's kubeconfig and its current context are never read |
| `kind_sandbox_install_agent_sandbox WORK` | applies Agent Sandbox v1.0.3's release manifest, pinned by sha256, and waits for its controller's rollout |
| `kind_sandbox_install_gvisor WORK` | installs gVisor's dated release, pinned by sha512, into the node's containerd as the `runsc` handler with a `gvisor` RuntimeClass, and fails unless a `runtimeClassName: gvisor` pod's `dmesg` says `Starting gVisor` |
| `kind_sandbox_load_image IMAGE` | pulls `IMAGE`, which must be named by digest, into the node's containerd, so a pod's start never waits on a registry |
| `kind_sandbox_internal_kubeconfig FILE` | writes the kubeconfig a container on the kind network reads, whose server is the node's address on that network |
| `kind_sandbox_teardown STATUS EVIDENCE` | when `STATUS` is not 0, writes the cluster's pods, the `legion` namespace's Sandboxes, volume claims, events and container logs to `EVIDENCE/cluster-at-failure.txt`; then deletes the cluster. It does nothing before `kind_sandbox_create` named a cluster, or once the cluster is gone |

## lib/live-daemon-proof.sh

The run a whole-daemon live proof makes around its driver. `merged-root-waits.sh` and
`no-required-checks.sh` differ only in what their driver proves, so each sets its own values and
makes this run.

```sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"     # sourced, never run
live_daemon_proof LABEL SLUG TEST EVIDENCE         # the script's last command
```

The caller sets `set -euo pipefail` and `root`, the checkout. `live_daemon_proof` starts the
transcript ([`lib/transcript.sh`](#libtranscriptsh)) in the evidence directory, then runs the four
checks each script's section lists — `prerequisites`, `broker`, `driver` and `proof` — each
printing `== <check>`, what it observed and `ok <check> (<s>s)`, and the first that fails
`FAIL <check>: <why>`. On any exit it stops the `nats-server`, removes the work directory and
prints `LABEL e2e: PASS in <n>s (evidence: <dir>)`, or
`LABEL e2e: FAIL at <check> after <n>s (evidence: <dir>)` on stderr.

| argument | is |
| :--- | :--- |
| `LABEL` | the run's name in its last line: `merged root waits`, `no required checks` |
| `SLUG` | the name of the run's own files under `TMPDIR` (or `/tmp` when it is unset): the work directory `legion-e2e-SLUG.<pid>.*`, which holds the broker's store and the driver binary `SLUG.test`, and the evidence directory `legion-e2e-SLUG-evidence.*` when `EVIDENCE` is empty |
| `TEST` | the one test the driver runs (`-test.run '^TEST$'`); `proof` fails unless the driver reports `--- PASS: TEST`, so a driver that skipped, or a pattern that matched nothing, is no pass |
| `EVIDENCE` | the evidence directory the script's own variable names (`MERGED_ROOT_WAITS_EVIDENCE_DIR`, `NO_REQUIRED_CHECKS_EVIDENCE_DIR`), or empty for a fresh one; kept and printed, it holds `transcript.log`, the broker's `nats.log`, the daemon's own `daemon.log`, which the driver writes through `LEGION_E2E_DAEMON_LOG`, and the driver's `proof.log` |

The two drivers share their Go harness the same way: `internal/daemon/live_proof_test.go`
(`-tags e2e`) holds the broker and log variables (`LEGION_E2E_NATS_URL`, `LEGION_E2E_DAEMON_LOG`);
the configuration the daemon boots with (`newLiveProof` reads `LEGION_E2E_NATS_URL`, skips when it
is unset, and builds the workflow configuration around it); the stream the daemon's intake
consumes; the Dispatch board stand-in (`liveDispatch`); the store; the seeding of a tree into the
next free admission slots (`admit`); and the envelope, `processed_events` and notice reads and
writes a step makes, the notices read by the issue whose architect they tell (`architectNotices`).

## lib/secret-leaks.ts

Stage 4b's check that no pod carried a value of its Sandbox's Secret in a container's command, args
or environment. A pod's `-boot` Secret holds the next generation's token after each relaunch, and the
pod watch records no Secret value, so a check made from a later read would judge the wrong values or
none. The helper watches the run's Secrets from `pod-watch` on and keeps every value each one held
in memory only. It never prints or writes a value.

```sh
bun scripts/e2e/lib/secret-leaks.ts <context> <namespace> <label-selector> <pod-watch> <verdict> &
kill -TERM $!    # judges the recorded pods and writes <verdict>, then exits 0
```

On SIGTERM it reads `<pod-watch>` (one watch event a line) and writes one JSON line to `<verdict>`:
`pods`, `secrets` and `values` are counts, `leaks` names each pod whose worker ran ready and one of
whose containers carries a value `<pod>-boot` held, and `unseen` names each such pod whose `-boot`
Secret the watch never saw. Each entry is `{uid, pod, secret}`. `unreadable` counts the lines of
`<pod-watch>` before its last that do not parse; the last may be one the watch is still writing, and
is skipped. Each watch asks the server to end it within 300 s and resumes from the last
resourceVersion; a watch that delivered nothing is resumed after a pause, and a line that does not
parse ends that watch. On 410 Gone it lists the Secrets again, so a value a Secret held only between
the last version seen and that list is not seen; every value a list or a watch event shows is. It
exits 2 when it cannot start.

## lib/stage4b-tools.jq

What a tool returned in a role's Oh My Pi session, for a Stage 4b checkpoint to prove a tool ran
rather than that the model said it did. `assistant_said ISSUE ROLE TEXT` reads the claim's assistant
turns, the reply text and each tool call's arguments, so it proves what the model wrote; the
session's `toolResult` entries are what the tools answered. The tool helpers take SESSION, the
session file a checkpoint fetched once (`claim_session_text`) and keeps as evidence, so one fetch is
what every assertion judges and the bytes kept are the bytes judged: `tool_ran SESSION TOOL` holds
when one call of TOOL in the session returned without error, `tool_result_said SESSION TOOL TEXT`
when such a result's text carries TEXT, literally and case-sensitively, and `tool_result_answered
SESSION TOOL ARGS TEXT` when such a result's call carried ARGS too, judged on the argument that
carries the call's target and never on the model's `i` intent, which Oh My Pi puts in every tool's
arguments (an intent could name a command the call never ran, `gh api graphql` in the intent of a
`gh pr view`, or `skill://fixture-omp-skill` on a read of the skill's file path): for `bash`, ARGS
must be in the call's `command`; for `read`, in its `path`; for every other tool, in its arguments
object with `i` removed, as JSON text, which is the device's content for a write to `xd://<name>`
(the hover of one file among the lsp tool's hovers, `NoSuchSymbol` among the codegraph calls, an
edit's `input`, since Oh My Pi's edit has no path argument). A bash call dispatched through
`eval`, or through a write to `xd://bash` whose content is no JSON object, has no `command`, so a
bound bash assertion does not see it, while `tool_result_said SESSION bash TEXT` still counts it.
A result is TOOL's by the call it answers, never by the `toolName` it records: the model calls a
tool by its name, by a `write` to its device `xd://<name>`, or by `eval` code calling
`tool.<name>(…)`, and a write to the device records `toolName: "write"`, the device's name only in
the paired call's `arguments.path`. The library pairs each result with its call by `toolCallId`,
judges the call with `lib/omp-tool-calls.jq`'s `calls`, the one rule every live proof uses for "the
agent called this tool", and keeps the call's arguments (`call_arguments`, as JSON text) on the
result. An error result (`isError` true; a `bash` whose command exited 1 after printing what was
wanted) never counts, and a result whose call the session no longer holds is no tool's.
`tool_results(name)` (each `{toolCallId, isError, text, arguments}`), `tool_result_texts(name)`
(the non-error texts, for a checkpoint's note to quote), `tool_result_quote($lead; $max)` (one
result on one line, for a fail line to quote), `arguments_carry(name; $args)` (whether a result's
call carried `$args`: its `command` for bash, its `path` for read, its arguments without `i` as
JSON text otherwise), `tool_ran(name)`, `tool_result_said(name; $text)` and
`tool_result_answered(name; $args; $text)` take the session's text read with `jq -R -s` and are
loaded with `jq -L scripts/e2e/lib 'include "stage4b-tools"; …'` (the script's `session_jq SESSION
JQ-ARGS…`).

`lib/stage4b-tools.test.ts` (`bun test scripts/e2e/lib`, which CI runs) lifts the three helpers from
the script by name and runs them over `lib/testdata/stage4b-tools-session.jsonl`, a session an
implementer pod wrote: `eval` returned `42`; the `lsp` device's two writes are `lsp`'s results and a
`write`'s alike, each carrying its call's `action`; the Go version is in the session only in a `bash`
that exited 1 and the browser's refusal only in an `eval` that errored, and each is refused;
`notebook`, which never ran, is the never-ran control; a word the model wrote in a call's arguments,
which `assistant_said` accepts, is refused as a tool's answer; the hover's text is no answer of
the status call; a composed `bash` call whose intent names `gh api graphql` beside a `gh pr view`
command is refused for `gh api graphql`, where the fixture's real `gh api graphql` call, which
answered the implement App's bot login as it was when the session was recorded, is accepted; a composed `read` of
`.omp/skills/fixture-omp-skill/SKILL.md` whose intent names `skill://fixture-omp-skill` is refused
for the `skill://`, where a read whose `path` is the `skill://` is accepted; a composed `lsp` hover
called by name on `greet.ts` whose intent names `greet.go` is refused for `greet.go` and accepted
for `greet.ts`, and a `codegraph` call whose intent alone names `NoSuchSymbol` is not the control
(`codegraph_control`'s selector) where one whose `query` names it is; and a `bash` call dispatched
through `eval`, which has no `command`, is counted by `tool_result_said` and not by a bound
`tool_result_answered`.
