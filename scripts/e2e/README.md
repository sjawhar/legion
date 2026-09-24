# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these; `lib/` holds what
the stage scripts share: standalone helpers they run, and the files they source
([`lib/rig.sh`](#librigsh), [`lib/workflow.sh`](#libworkflowsh),
[`lib/namespace-rig.sh`](#libnamespace-rigsh)), which `shellcheck` follows from this directory
through `scripts/e2e/.shellcheckrc`.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state`, registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
| `stage2-tmux-supervision.sh` | the Go daemon supervises real Oh My Pi sessions — the pinned build with this checkout's plugin in an isolated profile — in its private tmux server, against a real Envoy listener and NATS: the plugin gate refuses another contract and a disabled plugin; an agent registers, holds its Envoy role and is ready; a task queued before ready runs once, its model turn through the Hawk model gateway, and a retried frame starts no second turn; a killed pane resumes the same session; suspend and resume keep it; a stale hello is refused; an agent that never registers is retired at the deadline and counted; a restart re-adopts every live pane; an orphan is reaped after the grace; the OMP process's environment is the isolated one. Devbox only |
| `stage4a-sandbox-runtime.sh` | the Agent Sandbox runtime (`internal/runtime/sandbox`) on the production cluster, driven through the Legion daemon's restricted identity and nothing more: the Agent Sandbox install check accepts and refuses by name; the image probe Sandbox passes; a root provisions its workspace, registers, runs under gVisor and adopts its working copy's author; workers join the root's node, and schedule anywhere when no tree pod is scheduled; suspend, resume, a same-agent refusal, a pod killed in place, a relaunch before registration, and two concurrent provisions each hold; a fresh runtime re-adopts every live pod; the orphan sweep honours its grace; releasing the tree leaves nothing, and the namespace matches its snapshot. Devbox only |
| `verifiers-staging-token.sh` | `dispatch` and the Envoy listener authenticate a projected service-account token the staging EKS cluster actually minted — the right audience is accepted, the other binary's audience and a missing bearer are refused, each shared token still works, half an OIDC pair and an issuer that does not answer refuse the boot, and a refused token leaves its failure class in the log and nowhere else |
| `TestRealGitHubCredentialSurface` | the real `api.NewServer` and built `legion` binary use the implementer and reviewer Apps to identify as their bots, list the smoke repository's pull requests, refuse a merge before GitHub receives it, and clone the smoke repository through `legion credential` alone. Devbox only |

## TestRealGitHubCredentialSurface

```sh
LEGION_REAL_GITHUB=1 LEGION_TEST_PG_DSN=postgres://… \
  go -C packages/daemon-go test -count=1 ./internal/api \
    -run '^TestRealGitHubCredentialSurface$' -v
```

The test is intentionally gated because it calls GitHub as both installed Apps. It resolves each
App key through its `private_key_command`, registers the implementer and reviewer claims through
the actual API, and drives the binary it builds from this checkout. The test's temporary grant
files, built binary, and clone directory are removed by Go's test cleanup; the GitHub operations
are read-only except for locally cloning the smoke repository.


## stage1-skeleton.sh

```sh
bash scripts/e2e/stage1-skeleton.sh     # → "stage 1 e2e: PASS", exit 0
```

Needs `go`, `docker`, `jq`, `curl`, `ss` and `tmux`. It builds the binary from the checkout
(`packages/daemon-go/cmd/legion`), so it proves the tree you are standing in.

The daemon supervises its agents under tmux, and it refuses to start without what a launch needs
— tmux on `PATH`, an `operator_token_file`, and an OMP to run — and before anything else its
plugin gate holds the Oh My Pi plugin a pane would load to this daemon's contract
(`packages/daemon-go/internal/daemon/bootgate.go`). The run supplies all of it: its `legion.yaml`
names a 0600 operator token file in the work directory; `LEGION_OMP_PATH` points at a stub there
that answers the gate's load probe (`omp models --extension <probe> --json`) as a loaded plugin does
— `LEGION_PLUGIN_LOADED=yes` and, beside it, `LEGION_PLUGIN_LOADED_FROM=file://…/dist/legion.js`
inside the package the gate read — and exits 1 on anything else, because this proof launches no
agent; and the daemon runs with `HOME=<work>/home` and no `OMP_PROFILE`/`PI_PROFILE`, where
`<work>/home/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json` is the checkout's
own manifest (`name`, `version`, `legion`), so it declares the contract this checkout's daemon
requires. The real gate against a real Oh My Pi is proven where a proof installs the plugin
(`lib/install-plugin-profile.sh`); this one proves the skeleton.

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
  box's `~/.local/state/legion/legions-go.json`.
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
host, a `/healthz` that never answers, a state document that is not boot 1 with cap 4 and no
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

The `daemon-go` job in `.github/workflows/envoy-and-contracts.yaml` runs `go vet -tags e2e ./...` (which also compiles the e2e-tagged Stage 4a harness) in
`packages/daemon-go`, installs `tmux` — the tmux runtime's tests drive a real tmux server and skip
without one, and the daemon refuses to start without it — and the pinned `jj` (through mise, as the
pi-envoy job does) — the workspace tests drive a real jj, and the daemon resolves jj at boot and
refuses to start without it — then `go test ./...` against its
`postgres:16` service (`LEGION_TEST_PG_DSN`), then this script with `LEGION_E2E_PG_DSN` pointing at
the same service, so the script runs no docker of its own there. The job is gated on the
workflow's `changes` filter (`daemon_go`: `packages/daemon-go/**`, `go.work`, `scripts/e2e/**`,
`packages/pi-envoy/**`, `packages/contracts/fixtures/**`, and the native Dispatch server under
`packages/envoy/internal/dispatch/**` and `packages/envoy/cmd/dispatch/**`).

## stage2-tmux-supervision.sh

```sh
bash scripts/e2e/stage2-tmux-supervision.sh     # → "stage 2 e2e: PASS", exit 0, in about four minutes
```

**Devbox only.** It runs a real Oh My Pi that calls a real model, so it needs `go`, `docker`,
`jq`, `curl`, `ss`, `tmux`, `socat`, `bun`, `mise` (with the pinned OMP build it installs if
missing), the `secrets` CLI holding `GEMINI_API_KEY_TESTS` (agent tier: no YubiKey touch), and
the operator's model gateway access: `hawk-token` on `PATH`, their `hawk login`, and the GNOME
keyring holding it unlocked (every reboot locks it; the `unlock-keyring` skill). CI runs the unit
and integration tests, not this script; what only this run proves is OMP's real RPC frames,
`--resume`'s same-agent behaviour, the one-word `--append-system-prompt`, the plugin's strict
parse of the Go daemon's answers, the plugin gate against an installed manifest, the provider-key
path, a pane's model turn through the gateway, and the Envoy role claim.

What it stands up, all of it the run's own:

- **Postgres**: `LEGION_E2E_PG_DSN` when set; otherwise a `postgres:16` container on tmpfs
  (`legion-e2e2-pg-<pid>`) on an ephemeral loopback port — tmpfs rather than the image's anonymous
  volume, which a box whose docker volume subsystem stalls would hang on.
- **NATS and the Envoy listener**, as `scripts/kind-smoke/up.sh` runs them on the host: a
  `nats:2.10 -js` container (`legion-e2e2-nats-<pid>`), and `packages/envoy`'s `cmd/listener`
  built into the work directory and started with a fresh API bearer, which reaches every pane as
  the 0600 file `envoy_token_file` names.
- **The plugin**: this checkout's `pi-legion-envoy`, packed as the release packs it, installed into
  the OMP profile `legion-e2e2-<pid>-<epoch>` by `lib/install-plugin-profile.sh`. The daemons run
  with `OMP_PROFILE` naming it, so the gate and every pane load it.
- **OMP**: `omp_invocation: mise x <pin> -- omp`, the pin read from
  `packages/daemon/src/daemon/omp-pin.ts`. At boot the daemon asks `mise where <pin>` for the
  configured tool's executable, then runs that absolute binary under `mise x <pin>` for every boot
  probe and pane. The script deliberately keeps the ordinary daemon `PATH`, where this devbox has
  `~/.dotfiles/shims/omp` first, and checks the boot log's resolved binary, the OMP child's
  `/proc/<pid>/exe`, and every command in the pane's first-child chain. That proves the wrapper
  cannot replace the configured build while mise still supplies the tool's activation.
- **The model route**: Anthropic through the Hawk model gateway, installed into the profile by
  [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) at setup, while the script's shell
  still holds the operator's XDG directories. Its preflight mint refuses a locked keyring by name.
  Every model role of every pane is `anthropic/claude-opus-4-8`, and `anthropic` is the one
  provider a pane may use, keyed by the operator's hawk login through
  `<work>/model-gateway/hawk-token`, whose log records each mint; no Anthropic key reaches a pane.
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
| `gate-refuses-another-contract` | edits the installed (unpacked) manifest to declare the next `goDaemonApiVersion`; `legion start` refuses naming both numbers; the manifest is put back byte for byte |
| `gate-refuses-a-disabled-plugin` | `omp plugin disable`; `legion start` refuses with "installed but not loaded by omp"; `omp plugin enable` |
| `architect-registers-and-is-ready` | `legion start` passes the gate (its log line); `legion claims spawn` of a root architect whose role prompt says to reply `ready` and wait; the claim reaches `ready` and the daemon logged its registration at contract 1 |
| `envoy-role-held` | `GET /v1/roles/<claim token>` on the listener names the claim's session as holder |
| `ready-in-state` | `legion state --json` shows the issue's architect `ready`, with that session and a tmux locator |
| `task-queued-before-ready-runs-once` | a second claim spawned with a task: the spawn's answer holds the delivery unsent while the claim is launching; it ends `idle` with nothing pending, and OMP's session file holds the task once |
| `model-turn-through-the-gateway` | every assistant turn in that claim's session file ran on the `anthropic` provider as the profile's pinned model (`anthropic/claude-opus-4-8`), none ending in an error, and the key command minted more than its preflight |
| `retried-frame-starts-no-second-turn` | stops the claim's shim (SIGSTOP) and delivers a task: every send goes unanswered, and the daemon sends the same delivery id again at the next sweep (`the prompt was lost to the transport` twice in its log); resumed, the shim hands OMP the first frame and answers the repeat from its record — the session file holds the task once, and no prompt failure is charged |
| `kill-pane-resumes-the-same-session` | `tmux kill-pane` on the architect: the next generation is ready in a new pane with the same session, its OMP started with `--resume=<session file>`, and the Envoy role still held |
| `suspend-keeps-the-session` | `legion claims suspend`: `suspended`, no locator, the pane gone, the session and its file kept |
| `resume-on-demand` | `legion claims resume`: `ready` at the next generation in a new pane, same session |
| `stale-generation-hello-refused` | writes a hello carrying generation 1's boot token to `<state_dir>/worker-stream.sock`: the daemon closes it with nothing written and logs `rejected hello (stale worker generation)` |
| `unregistered-agent-retired-at-the-deadline` | a second daemon whose `LEGION_OMP_PATH` stub answers the plugin gate and otherwise sleeps, with a 10 s registration deadline (5 s × 2): the claim's shim connects and its process lives, the agent never registers, and at the deadline the process is retired and one launch failure counted |
| `restart-readopts-the-live-panes` | SIGTERM, then start again: `boots` +1, both claims keep their generation, incarnation and pane, no pane opens or closes, and a task delivered after the restart runs once — over the connection the shim's reconnect hello opened |
| `omp-child-environment` | the boot log names the resolved pinned OMP binary for both probes and panes; the pane's process is `/bin/sh -c`; walking first children from it to `argv[0] == omp` finds that exact executable, never the OMP wrapper that remains first on the ordinary daemon PATH; the pane's shell and that OMP carry all four XDG base directories under `<state_dir>/home` and no `DBUS_SESSION_BUS_ADDRESS` (LEGION-206 P1, with the gateway route in place); OMP carries `GEMINI_API_KEY` (length only) that its shim does not, and no `ANTHROPIC_API_KEY`, `GEMINI_API_KEY_TESTS`, `SOPS_AGE_KEY_FILE` or `SECRETSD_CONFIG` |
| `stray-pane-reaped-after-the-grace` | opens a window marked as the daemon's (`@legion_owner`) holding no recorded pane, and an unmarked one beside it: the periodic orphan sweep (every 60 s, 120 s grace) reaps the marked one no sooner than 120 s after it opened, and keeps the unmarked window and both claims' panes |
| `stop` | `legion claims stop` retires both claims; `legion stop` ends the daemon with exit 0 |
| `every-turn-through-the-gateway` | [`lib/check-model-route.sh`](#libcheck-model-routesh) over every session in the isolated profile, each subagent's included: every assistant turn was served by the `anthropic` provider, the gateway's; and its negative control, a copy of one captured session with a turn rewritten as Bedrock's, is refused |

Every wait is bounded and names what it waited for; a failed assertion prints
`FAIL <check>: <why>` and exits 1, and any other failing command names the check it ended. The
`EXIT` trap — on a pass, a failure, or an interrupt — stops both daemons (SIGKILL after 10 s),
kills both private tmux servers, stops the listener, SIGKILLs any process still naming the work
directory in its command line or working directory, removes both containers and the OMP profile,
and removes the work directory when the run passed (keeping it, with `daemon.log`,
`deadline.log`, `listener.log`, `model-gateway/hawk-token.log` and each refusal's log, when it did
not).

Three things the run had to learn about its surface:

- The shim is a Go process, which starts its child from whichever thread runs the goroutine: the
  first-child walk reads `/proc/<pid>/task/*/children`, never the main thread's list alone.
- OMP takes the session to resume as `--resume=<file>`, one argument.
- The shipped orphan sweep is the only reaper after boot (boot's own reconcile runs with grace 0),
  so the stray is opened after the restart and the check waits up to five minutes.

## stage3-devbox-workflow.sh

```sh
bash scripts/e2e/stage3-devbox-workflow.sh     # → "stage 3 e2e: PASS", exit 0
```

**Devbox only; CI does not run this script.** It runs real Oh My Pi agents and real GitHub Apps,
then squash-merges one disposable pull request as the proof human into `sjawhar/legion-smoke`.
The run needs `go`, `docker`, `jq`, `curl`, `ss`, `tmux`, `bun`, `mise`, `gh`, `shellcheck`, the
`secrets` CLI, and the operator's model gateway access: `hawk-token` on `PATH`, their `hawk login`,
and the GNOME keyring holding it unlocked (every reboot locks it; the `unlock-keyring` skill). The
proof human is the devbox's ordinary `gh` — the dotfiles shim, acting as the
`sjawhar-agent` App — for its reviews, its reads, and its merge; it is never a Legion App, and the
run needs no personal access token (`GH_PUBLIC_REPO_PAT` cannot read the private smoke repository
anyway). The daemon resolves `LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` and
`GH_REVIEW_APP_PRIVATE_KEY_B64` itself through `private_key_command`, both agent tier. The agents'
model is Anthropic through the Hawk model gateway, the route every devbox agent session uses:
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) routes the isolated profile there in
`prerequisites`, where its preflight mint refuses a locked keyring by name, and the daemon is given
no `provider_keys` (the Google provider answered long workflow turns with empty responses, so the
agents are not Gemini-backed). No YubiKey touch is required, and no key value enters the script's
shell, a pane, an argv, or the transcript.

The script stands up a scratch Postgres, NATS, Envoy listener, native Dispatch server, and a
subscribe-only production-Envoy GitHub bridge in one temporary directory. It installs this
checkout's plugin in an isolated OMP profile and runs the Go daemon with a separate state
directory, private tmux server, ports from [`lib/free-port.sh`](#libfree-portsh), a root-only
Dispatch project, `admission_cap: 2`, `review_round_cap: 3`, and the root design gate armed. Its
host-side helpers are [`lib/rig.sh`](#librigsh), and the workflow's vocabulary (Dispatch, the
daemon's state, the agents, the proof human, the handoff checks) is
[`lib/workflow.sh`](#libworkflowsh); the script defines only its runtime's reads, its scratch
services, its production guards, and its checks.

Production is off limits, and the proof checks that rather than assuming it. Every registered
pane's OMP environment must carry `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, `ENVOY_URL`, and
`ENVOY_NATS_URL` for this rig's scratch servers, and no `ANTHROPIC_API_KEY`, which would take its
turns off the gateway route. A watcher checks each pane the daemon launches —
the ones it starts on its own included — as soon as its OMP process exists, before the plugin
registers and so before any turn; a mismatch kills the private tmux server and aborts naming the
pane, and the audit fails naming any launch the watcher never saw. At
the end, a read-only audit replays production Dispatch events from a baseline read at boot (the
baseline event is the replay's positive control) and fails on any event a rig session authored or
that names the rig's project key, then reads the operator's Envoy listener (`GET /v1/sessions`,
`STAGE3_PRODUCTION_ENVOY_URL`, default `http://127.0.0.1:9020`) and fails on any rig session. The
audit uses the Dispatch token already in `~/.config/opencode/envoy.json` and writes nothing.

It proves admission order and slotless children, then drives a root from `todo` through an
architect's spec and gate registration, the human approval, planner, implementer pull request,
tester, reviewer, retro, merger READY, the ordinary human squash merge, production check, and
architect sign-off. It also proves three changes-requested rounds, each naming one concrete
correction the spec permits (a distinct line appended to the smoke file, the one product file the
implementer's pull request changed, which the run records and requires to be exactly one; the
correction counts only in that file's patch on the pull request) and reaching testing only on
that round's own implementer handoff (the daemon's phase record must hold the implementer's
handoff for that round when the issue reaches testing, and the commit carrying every planner,
implementer, and tester handoff is authored and committed by that role's own App, read from the
issue's workspace),
and `pr-blocked`, READY refusing after a later spec version until a human approves it, a held
worker after its launch budget and the architect's retry relaunching it, restart during
implementation, a pending status write while Dispatch is down, and the Go pane's
credentials: in one bash tool call of a real implementer pane, plain `gh` resolves
`<state_dir>/worker-bin/gh`, two chained `legion gh` calls authenticate as `legion-implementer[bot]`
on the command's one grant, and `gh pr merge` is refused. After the sign-off the proof human removes
every `.legion/` handoff and `docs/solutions/` learning from the smoke `main` through one merged
fixture pull request, and the run checks that `main` carries none: the Go daemon has no clean-head
loop before Stage 7, so the proof's reviewer approves a head that still carries `.legion/`, and
without the cleanup each merge would leave the next run a base carrying another issue's handoffs.
Each check is named in the transcript;
three negative controls demonstrate that the status-actor, held-worker, and re-closed-gate
assertions reject deliberately corrupted observations before the captured observations pass again.
Once every agent is gone, `model-turns-through-the-gateway` runs
[`lib/check-model-route.sh`](#libcheck-model-routesh) over every agent session in the isolated
profile, each subagent's included: it fails on any assistant turn or model selection that is not
the `anthropic` provider's, the gateway's, and its negative control (a copy of one captured session
with a turn rewritten as Bedrock's) is kept in the evidence as `model-route-control/`. It runs after
`services-stopped` has stopped the watcher, the daemon and the private tmux server and found no
proof process left, since an idle agent takes a turn on the next event the daemon delivers, and
before the transcripts are copied and the profile removed. It then runs over the copied
transcripts too, and fails unless they hold the same turns, sessions and subagents.

`STAGE3_FROM=held` or `STAGE3_FROM=restart` is a development aid for iterating on the later
scenarios against a fresh rig: it skips the first issue's workflow (the proof human closes that
root, which frees its admission slot as its sign-off would), `restart` also skips the held worker,
and the credential check reads `STAGE3_PR` (default: the newest smoke pull request).
`STAGE3_UNTIL=rework` is the other development aid: it drives the first issue through its three
review rounds, the per-round handoff checks, and the final review, then skips every later scenario.
Such a run skips the status-actor check, which needs the first issue's whole history, ends
`stage 3 e2e: development run from <step> finished (not the proof)` (or `until rework`), and is
never cited as the proof; only a full run is.

Evidence survives every outcome in `STAGE3_EVIDENCE_DIR` (default a fresh
`/tmp/legion-e2e3-evidence.XXXXXXXX`, printed at exit): every agent transcript, the daemon,
Dispatch, listener, and bridge logs, the model key command and its log of every mint
(`model-gateway/`), state captures, negative-control outputs, the pane endpoint checks, and the
production audit. A passing run's last three checks stop every process, kill the private tmux
server and remove both containers (`services-stopped`), check the model route
(`model-turns-through-the-gateway`), and remove the isolated OMP profile and the scratch work
directory, the agents' workspaces with it (`cleanup-is-complete`); each shows what it removed gone.
On any exit the `EXIT` trap does the same teardown, except that a failure keeps the scratch work
directory and prints its path.

## stage4a-sandbox-runtime.sh

```sh
LEGION_E2E_RUNTIME_CONTEXT=legion-daemon@production \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
  bash scripts/e2e/stage4a-sandbox-runtime.sh     # → "stage 4a e2e: PASS", exit 0
```

**Devbox only, against the production cluster; CI compiles the harness (`go vet -tags e2e ./...`
in the `daemon-go` job) and does not run it.** Stage 4a's gate: `internal/runtime/sandbox` drives
Agent Sandbox pods in namespace `legion` from the devbox, the way the 4b daemon will. The script
needs `go`, `kubectl`, `aws` (the runtime kubeconfig's `aws eks get-token`), `curl`, `ss`,
`diff`, and the `secrets` CLI holding `LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` (agent tier: no
YubiKey touch). The harness runs `secrets <KEY> -- sh -c 'printf %s "$<KEY>"'`: the `secrets` CLI
decrypts the key and puts it in the environment of that one `sh` child, which prints it to a pipe
the harness reads into memory. The harness decodes it there and mints the implement App's
installation token in process. The key is written to no file, appears in no argv, and reaches no
other process; only the installation token enters each claim's Secret.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_RUNTIME_CONTEXT` | required | the kubeconfig context of the Legion daemon's restricted identity (`legion-daemon@production`: the IAM role `production-legion-daemon`, group `legion-daemon`) |
| `LEGION_E2E_RUNTIME_KUBECONFIG` | `~/.kube/legion-daemon-production` | the kubeconfig file holding that context, kept apart from the devbox's own |
| `LEGION_E2E_OPERATOR_CONTEXT` | `production` | the devbox's admin context, for operator steps only |
| `LEGION_E2E_IMAGE` | required | the worker image under test, by digest: a `worker-image.yaml` run on the branch under test |
| `STAGE4A_FROM` | unset | a development entry point: any check after `identity` except `stale-incarnation`, which rides `kill-pod`'s relaunch. `identity` always runs; the checks before the entry point are skipped, and each later check first puts the claims it needs where the full run would have left them, through the same runtime calls. The run ends `stage 4a e2e: every check from <check> passed — a development run, never the proof`, and is never cited as the proof |
| `STAGE4A_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e4a-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `transcript.log` (the whole run), `runtime.log` (the runtime's and the listener's JSON log lines), the two namespace snapshots, and the probe's pass cache |

Two identities, so the runtime is proven under exactly the RBAC it ships with. The runtime and
the harness's own reads use the restricted one; the admin context only runs what an operator does
beside the daemon — `kubectl exec`, PVC phases, the pod uid cross-checks, a Secret's boot token
(hashed in process, never printed), node and EC2NodeClass reads for a network failure, and the
namespace list. Every evidence line names which one observed it (`[runtime]`, `[operator]`, or
`[harness]` for the listener and its resolver).

The harness hosts the worker stream itself, on the devbox's private address (from instance
metadata) and port 13371 — the port the devbox's security group admits from Legion nodes, never
`0.0.0.0` — and refuses to start while anything holds it, naming the holder. Its resolver accepts
only each claim's current generation and records every hello with the claim, the generation, and
the hash of the token presented. The pods run a stub agent under the real Go shim: it appends its
pod's uid to a marker file in the tree volume's sessions directory, the file a resume names, and
sleeps, so the runtime's whole path runs with no model and no provider key. The runtime's settings:
a 5-minute boot timeout, 3 registration intervals, a 15-second termination grace, a 10-second
probe interval, storage class `gp2`, no resource requests, and the node selector
`karpenter.k8s.aws/instance-cpu: "4"`, which gives every tree a node that fits it (below).

The checks, in order, each printing what it observed and then `CHECK <name>: PASS`:

| check | what it does and requires |
| :--- | :--- |
| `identity` | refuses to start unless the runtime context is set and authenticates as someone other than the operator; a SelfSubjectReview shows the assumed `…legion-daemon` role in group `legion-daemon`; `list secrets -n legion` is 403; a SelfSubjectRulesReview (`can-i --list`) in every namespace finds no grant beyond the plan's; access reviews, which reach EKS's webhook authorizer that a rules review cannot enumerate, deny every kind of impersonation, `serviceaccounts/token`, pod create and exec, secret list and create, PVC get, nodes, RBAC create/update/patch/escalate/bind, and Sandboxes outside `legion`, beside two positive controls |
| `installed` | `CheckInstalled` with production's `InstallRef` passes under the `resourceNames` grants |
| `boot-refusal-negative` | `CheckInstalled` naming `legion-no-such-controller` refuses, naming that Deployment and the 403 the `resourceNames` grant answers, without blaming the CRD |
| `image-probe` | `ProbeImage` on the image under test passes, its log confirms `go-daemon-api-version` equal to the daemon's contract, and the probe Sandbox is deleted |
| `root-ready` | Spawn of the root: its Sandbox Ready, the returned incarnation the pod's uid, the init log (`pods/log`) carrying `workspace-init: /legion/workspaces/sjawhar/legion-smoke/s4a-1 on legion/S4A-1`, and a hello registered at generation 1 with that generation's token |
| `gvisor` | `uname -r` in the root pod is gVisor's emulated kernel (`…-gvisor`), not the node's, and the pod's `runtimeClassName` is `gvisor` |
| `adopt-working-copy` | `AdoptWorkingCopy` with the implement App's bot identity; `jj log -r @ -T author` in `$LEGION_WORKSPACE` shows it |
| `worker-colocated` | a worker spawned while the root runs requires the tree's node (podAffinity on `legion.dev/tree`, topology `kubernetes.io/hostname`) and runs there |
| `suspend` | Suspend of the worker: when it returns the runtime's watch no longer holds the claim; Sandbox `Suspended`, pod gone, tree PVC `Bound`, `Probe(recorded)` gone; over the settle window Observe delivers no observation of the worker evaluated after Suspend returned (an observation's `At` is stamped as its evaluation ends, and Observe re-reads the recorded incarnation before it sends) |
| `no-affinity` | with the root suspended and no tree pod scheduled, a second worker carries no affinity, runs, and mounts the tree PVC; suspended, the resumed root carries none either |
| `resume` | Resume of the first worker: the affinity is back, a new incarnation, a hello at the next generation with its token, and the marker holds exactly the old and new pod uids |
| `same-agent-negative` | a Resume naming a session file the volume lacks: the init container refuses (`Refusing to start S4A-1 fresh`), observed as gone with the init log; resumed correctly, the marker holds two agents and never the refused pod |
| `kill-pod` | `kubectl exec … sh -c 'kill 1'` on the worker: gone with the old uid and the main container's exit code; `Resume(prev=dead)` relaunches through `Suspended` (the Sandbox's generation moves by exactly two) |
| `stale-incarnation` | across that relaunch, every observation carrying the new uid is alive or uncertain, the gone carried the old uid, and `Suspend(old)` leaves the Sandbox `Running` on the same pod |
| `respawn-before-register` | a claim spawned on a token the resolver withholds, suspended before any hello, spawns again over its Sandbox: a new uid, the Secret's boot token rotated to generation 2's, and generation 2 registered |
| `concurrent-provision` | a new tree's root and a child worker spawned at once: both provision their workspace, the two `workspace-init` runs do not overlap (the runtime serializes them; `flock` does not reach across gVisor pods), and the volume holds one clone, with both jj workspaces, that passes `git fsck --connectivity-only` |
| `re-adopt` | the listener and runtime closed, one worker killed while none runs, then a fresh listener and `sandbox.New` with `ReconcileOrphans(known)`: the living claims are alive with their recorded incarnations and unchanged pods and Sandbox generations, the killed one is gone with its recorded uid, and every living shim says hello again with its current token |
| `orphan-sweep` | a running claim left out of `known` survives a sweep with a 1-hour grace and is deleted by one with a 1-second grace; the suspended claim's Sandbox and every known one survive both |
| `release-tree` | Release of every claim, the suspended one with a nil locator: no Sandbox, `-boot` Secret, pod, or tree PVC of the run is left |
| `namespace-clean` | the script's last step, after the teardown and outside the harness: the namespace's Sandboxes, Secrets, PVCs and pods that carry the run's project label or none are exactly the snapshot taken before the run |

Everything the run creates carries the project label `s4a-<UTC timestamp>-<4 hex>`, and the
claim tokens carry the same value without its dashes. The harness appends each Sandbox's name to
a record before the Sandbox can exist. On any exit the `EXIT` trap runs
[`lib/namespace-rig.sh`](#libnamespace-rigsh)'s teardown: it refuses to act on a project without
the `s4a-` prefix, deletes every recorded Sandbox by its exact name and then the Sandboxes
labelled with that exact project (never by label existence), waits for the owned Secrets, pods
and PVCs to follow, deletes by the same exact label any Secret or PVC still left after 90
listings, and runs `namespace-clean` when the harness did not get to it. Nothing outside `legion`
is touched.

What the run had to learn about production:

- **A tree needs a node that fits it.** Every pod of a tree requires the node of the tree's first
  scheduled pod, since the tree volume is a single-node EBS volume. With nothing more, Karpenter
  puts that pod on a `c7a.medium`, whose 8 pod slots its 7 daemonsets all but fill, so no second
  pod of the tree can ever join it. A CPU request on the root does not fix it: when a child is
  placed first, as the concurrent launch showed, the root must join the child's node, and there a
  2-CPU root beside another tree's root stayed Pending on `Insufficient cpu`. The node selector
  on Karpenter's `karpenter.k8s.aws/instance-cpu` label keeps every Legion pod on a 4-vCPU node
  with 58 slots and requests nothing. The 4b daemon must carry it as
  `runtime.kubernetes.scheduling.node_selector` until the `legion` NodePool in agent-c has the same
  floor.
- gVisor on production reports `4.19.0-gvisor` from `uname -r`.
- The worker image has no `kill` binary; the exec runs the shell's builtin.

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

## lib/install-plugin-profile.sh

Installs this checkout's `@sjawhar/pi-legion-envoy` into a named OMP profile, packed the way the
release packs it, so a stage proof or a boot-gate test runs the branch-built plugin and the user's
default profile is never touched.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
manifest=$(scripts/e2e/lib/install-plugin-profile.sh --profile legion-e2e-$$ --dest "$(mktemp -d)")
# → ~/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json
```

| flag | meaning |
| :--- | :--- |
| `--profile <name>` | the OMP profile to install into (`OMP_PROFILE=<name>`). Refused when OMP would read it as its default profile — empty, all whitespace, or `default` — since that is the profile every plain `omp` uses. Any other name goes to OMP as given, and OMP refuses one it cannot use. |
| `--dest <dir>` | where the tarball is unpacked. `omp plugin install` links this directory into the profile rather than copying it, so it **is** the installed plugin and must outlive the run. Refused inside the checkout (jj would snapshot it, symlinks resolved first) and when it exists and is not an empty directory (an unpack over an earlier build would keep that build's stale files). |

Both flags are required; each refusal names its flag and exits 2. Stdout is exactly one line, the
installed manifest's path as `OMP_PROFILE=<name> omp plugin list --json` reports the plugin; that is
the manifest both daemons' contract gates read under the same profile — the TypeScript daemon's
(`getPluginsNodeModules()`, `packages/daemon/src/daemon/boot-probes.ts`) and the Go daemon's
(`pluginManifestPath`, `packages/daemon-go/internal/daemon/bootgate.go`). Every step's own output
goes to stderr.

The steps are the release's, run in the checkout — a copy of `packages/pi-envoy` cannot build,
because `prepack.sh` copies `../../skills` and the bundle resolves `@legion/*` through the root's
`node_modules`:

1. save `packages/pi-envoy/package.json` and arm an `EXIT` trap that copies it back byte-identical
   (`.github/workflows/release.yaml:345`);
2. rewrite `omp.extensions` to `["dist/envoy.js","dist/legion.js"]` with `jq` (`release.yaml:346-348`,
   `packages/daemon/docker/worker.Dockerfile:66-67`);
3. `bun pm pack`, whose `prepack` builds `dist/` (`release.yaml:350-353`, `packages/pi-envoy/scripts/prepack.sh`);
4. copy the saved manifest back and check it byte for byte (`release.yaml:365-370`);
5. unpack the tarball into `<dir>` (`worker.Dockerfile:62-64, :69-70`);
6. `OMP_PROFILE=<name> omp plugin install <dir>` (`worker.Dockerfile:184`);
7. `OMP_PROFILE=<name> omp plugin list --json` must show the plugin at the checkout's version,
   enabled, and resolving to `<dir>`.

The release's version bump (`release.yaml:328-333`) is not a step: the profile gets the checkout's
own version. The packed manifest and the tarball are written to the run's `mktemp -d` directory,
never beside `package.json`, so an interrupted run strands no `tmp.json` or `.tgz` in the checkout.

The manifest is rewritten only for as long as the pack takes. The trap copies it back on every other
way out — a failed step, `SIGHUP`/`SIGINT`/`SIGTERM` (each routed through `exit`) — so a pack that
dies halfway never leaves the rewrite for jj to snapshot. It keeps the run's status; if the copy back
itself fails, it says where the saved bytes are, leaves them there, and exits non-zero. Afterwards
`jj status` is as it was before the run: `dist/` is gitignored, and nothing else is written inside
the checkout.

Runs in one checkout take turns from the save to the copy back, under a `flock` on the manifest
itself (rewritten and restored in place, so the lock's inode lasts the whole window); a run that
has to wait says so on stderr. Without the lock, a run that starts while another has the manifest
rewritten saves that rewrite as its "before" and puts it back at its own exit: both runs exit 0 and
jj snapshots the rewritten `package.json`. `go test ./...` runs package test binaries in parallel,
so two callers at once is the expected case.

The script creates the profile and `<dir>` and removes neither; the caller does, with
`rm -rf ~/.omp/profiles/<name> <dir>` (the profile holds `plugins/` — the link and
`omp-plugins.lock.json` — and OMP's `logs/`).

### The natives download

OMP's native modules (`pi_natives.linux-x64-{baseline,modern}.node`, ~350 MB) live in
`$HOME/.omp/natives/<omp version>/` — `$XDG_DATA_HOME/omp/natives/` when `$XDG_DATA_HOME/omp`
exists — and every profile under that `HOME` shares them (`getNativesDir`,
`@oh-my-pi/pi-natives/native/loader-state.js`); a profile has no natives of its own. OMP writes them
on its first run under a `HOME` that has not run this OMP version, and in this script that run is
`omp plugin install`. So a fresh CI runner, a container, or a newly bumped OMP pin pays ~350 MB there,
once; on a box where the pinned OMP has already run, a fresh profile pays nothing. Measured on the
devbox with OMP 18.2.2: build, pack, install and verify took 2 s into a new profile, the profile got
no `natives/` directory, and `~/.omp/natives/18.2.2/` was untouched.

## lib/install-model-gateway.sh

Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's
own hawk login, the route every devbox agent session uses (`~/.omp/agent/models.yml`:
`X-Api-Key: !hawk-token`), so the tmux stage proofs' panes reach Anthropic with no provider key.
Stage 2 and Stage 3 run it before they move any XDG directory of their own.

```sh
key_command=$(bash scripts/e2e/lib/install-model-gateway.sh --profile legion-e2e-$$ --dest "$work/model-gateway" --cache-dir "$work/model-gateway-cache")
# → $work/model-gateway/hawk-token
```

| flag | meaning |
| :--- | :--- |
| `--profile <name>` | the OMP profile to route. Refused when OMP would read it as its default profile (empty, all whitespace, or `default`), and when its `agent/models.yml` or `agent/config.yml` already exists. |
| `--dest <dir>` | where the key command and its log are written; created `0700`. Refused when it exists and is not an empty directory, and when its path holds a character other than letters, digits, `/`, `.`, `_` or `-`, since it is written into YAML as one `!command` word. |
| `--cache-dir <dir>` | where the key command keeps the key it minted; created `0700`. Refused when it exists and is not an empty directory, so a key left there is never served. A private directory of the run, never its evidence: the key is a live gateway credential. |

It writes `<dir>/hawk-token`, the key command: `hawk-token` (resolved on `PATH`) run under the
caller's `DBUS_SESSION_BUS_ADDRESS` and XDG base directories (a variable the caller has unset is
unset for it), for that one command. It appends one line per invocation, one per mint, and
`hawk-token`'s own stderr to `<dir>/hawk-token.log`; stdout carries the key alone. The profile's `agent/models.yml`
points the `anthropic` provider at `https://middleman.hawk.internal.trajectorylabs.com/anthropic`
with `apiKey` and `X-Api-Key` both `!<dir>/hawk-token`, and its `agent/config.yml` pins every
model role (`default`, `smol`, `slow`, `vision`, `plan`, `commit`, `tiny`, `task`, `advisor`) to
`anthropic/claude-opus-4-8`, sets `enabledModels: [anthropic/*]`, and disables `amazon-bedrock`,
`bedrock-mantle`, `google`, `ollama`, `llama.cpp` and `lm-studio`. Stdout is the key command's path.

A pane cannot run `hawk-token` itself, which is why the command, and only it, gets the operator's
environment. Measured in a Go pane at `f1749048` whose profile named `!hawk-token` directly, by
running `hawk-token` under that OMP process's exact environment: it fails on mise (`No version is
set for shim: uv`), because the pane's `XDG_CONFIG_HOME` is the daemon's own (LEGION-206 P1) and
mise's global config is not there; with the operator's XDG directories it fails on the login (`no
usable hawk login`), because the pane carries no `DBUS_SESSION_BUS_ADDRESS`, the only address the
keyring client reads (`jeepney/bus.py`, `find_session_bus`); with both it mints. Panes run as the
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
`hawk-token` is not on `PATH`, when `DBUS_SESSION_BUS_ADDRESS` is unset, when the keyring is locked
(`the operator's keyring is locked, so hawk-token cannot read the hawk login: unlock it (the
unlock-keyring skill) and rerun`), and when `hawk-token` prints anything but one JWT (quoting the
last line of its stderr); an argument refusal exits 2. The mint also runs `hawk-token`'s own periodic
self-refresh, which can take longer than OMP's ten-second budget for a `!command`, before any pane
needs a key rather than inside one. The key is never printed.

The key command mints once and keeps the key in `<cache-dir>/hawk-token.key` (`0600`) until
300 seconds before its JWT `exp`, or for 300 seconds when the key has none. Each `hawk-token` run
reads the hawk login from the keyring over the session bus, and the devbox's keyring daemon died
serving such a read at 09:33Z on 2026-09-24, relocking the keyring mid-run. A Stage 3 run
invoked the command 29 times, once per pane launch plus the preflight, and each was a mint before
the cache. Every call inside the window gets the kept key. A key the gateway refuses before then is
not re-minted: the proof's model turns fail, loudly, which is right for a proof. (The command cannot
tell Oh My Pi's retry after a 401 from a first call: OMP runs it through `/bin/sh -c`, so each call
has a fresh parent process.)

The script creates the profile's two files, `<dir>` and `<cache-dir>`, and removes none of them; the
caller does, with `rm -rf ~/.omp/profiles/<name> <dir> <cache-dir>`.

## lib/check-model-route.sh

Proves a tmux stage proof's agents reached the model only through the gateway: every agent turn
the isolated OMP profile recorded, each subagent's included, was served by the `anthropic`
provider, the one [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) routes to the
gateway and leaves enabled. Stage 2 runs it last, after `stop`. Stage 3 runs it once every agent
process has stopped and before it copies the transcripts, then again over the copies, which must
hold the same turns, sessions and subagents.

```sh
bash scripts/e2e/lib/check-model-route.sh --sessions ~/.omp/profiles/<profile>/agent/sessions --control "$work/model-route-control"
# → 74 assistant turns in 5 agent sessions (0 of them subagents'), every one on the anthropic provider (the gateway); negative control: … refused
```

Each session is a JSONL file under `--sessions`; a subagent's is the `<AgentName>.jsonl` in its
parent session's own directory. Every assistant turn must record `message.provider` `anthropic`,
and every `model_change` a model under `anthropic/`; a line that does not parse is skipped, since
a live agent may be mid-write. It exits 1 naming each session off the route with what it recorded,
and when no session, or no assistant turn, exists. Run over the transcripts of a Stage 3 run from
before the gateway route, it names the retro scout's Bedrock turn:

```text
check-model-route: agent sessions off the anthropic route (the gateway):
…/RetroFreshEyes.jsonl: model change to amazon-bedrock/openai.gpt-oss-120b
…/RetroFreshEyes.jsonl: turn on amazon-bedrock/openai.gpt-oss-120b
```

Every run also carries its own negative control: the first session holding a turn is copied into
`--control` with that one turn rewritten as `amazon-bedrock/us.anthropic.claude-opus-4-8`, and the
same check must refuse the copy, or the run exits 1 (`the negative control passed`). An argument
refusal exits 2.

## lib/rig.sh

The host-side helpers a stage proof sources for its rig: bounded waits, ports, the processes the
run starts, and their teardown. Stage 3 sources it.

```sh
. "$root/scripts/e2e/lib/rig.sh"     # sourced, never run
```

The caller sets `root` (the checkout), `work` (the run's scratch directory: every process whose
working directory or command line names it is the run's), `evidence` (each service started by
`start_process` logs to `$evidence/logs/<name>.log`) and `timeout_hook` (empty, or a function a
timed-out wait runs before it fails), and defines `note` and `fail`, which exits.

| function | does |
| :--- | :--- |
| `until_true SECONDS WHAT CMD…` | runs CMD every half second until it succeeds; after SECONDS it runs `timeout_hook` and fails naming WHAT. A guard that writes `$evidence/pane-endpoint-violation.txt` makes the next wait abort, naming the violation |
| `pick_port VAR` | assigns VAR a port from [`lib/free-port.sh`](#libfree-portsh) that no earlier pick of the run returned. It assigns in place: a command substitution would run it in a subshell and lose the run's set of picks |
| `start_process NAME CMD…` | starts CMD in the background, appending to NAME's log, and sets `NAME_pid` |
| `log_size NAME` | the size of NAME's log, the offset `await_start` reads a start's own lines from |
| `await_start NAME PID OFFSET SECONDS WHAT CMD…` | waits, bounded, for CMD to succeed while PID lives. It returns 2 when the service exited on `address already in use` after OFFSET, the one race a pick before the bind cannot close, so the caller picks again; any other exit fails naming the log |
| `stop_pid PID` | TERM, then KILL after 10 s; best effort, so a failed cleanup never hides the check that failed |
| `run_processes` | prints every pid whose working directory or command line names `$work` |

## lib/workflow.sh

The vocabulary of a stage proof that drives Dispatch issues through the Go daemon's workflow with
real agents: Dispatch, the daemon's state, each phase's worker, the smoke repository's pull request
under the proof human, the handoffs the daemon accepted, the notices, and the negative controls.
Stage 3 sources it after [`lib/rig.sh`](#librigsh).

```sh
. "$root/scripts/e2e/lib/workflow.sh"     # sourced, never run
```

The caller sets `work` (holding `dispatch-token`, `legion.yaml` and `operator-token`),
`evidence`, `project` (the Dispatch project; the daemon writes as `legion-daemon:<project>`),
`repo`, `port_dispatch`, `port_daemon`, `pg_container` (the daemon's Postgres), and, once they
exist, `pr_number` and `smoke_file` (the one product file the pull request's first implementation
changed). It defines `note`, `pass` and `fail`, and the runtime seam, the only reads that depend on
where an agent runs:

| seam | the caller's definition |
| :--- | :--- |
| `assert_claim_endpoints ISSUE ROLE` | fails the check when the claim's process could reach a service outside the rig; every instruction runs it first |
| `claim_session_text ISSUE ROLE` | prints the claim's session file, and fails when there is none |
| `workspace_jj ISSUE ARGS…` | runs `jj ARGS…` in the issue's workspace |

Every wait for an issue to reach one phase is `wait_for_phase ISSUE PHASE [SECONDS]`: 600 s, unless
the phase's worker runs a whole loop (a correction round, the retro) and the caller passes its own
bound. `round_correction_pushed ROUND` accepts the round's line only as an addition in
`smoke_file`'s patch, never in a notes file or in a `.legion/` handoff that quotes it.

The handoff checks read the daemon's phase record (the `phases` table joined to `issues`), not the
ids of the facts it processed, so they hold whatever format a handoff event id takes. A role's
`handoff_commit` is the commit its last accepted completion reported, and the daemon empties it
when a transition starts that role on a new phase (`clearHandoff`,
`packages/daemon-go/internal/workflow/effects.go`); the implementer's `rounds` counts its returns
to implementing. Read right after the transition a completion caused, a role's non-empty
`handoff_commit` is that phase's own: `assert_round_handoff ISSUE ROUND` (testing reached on the
implementer's completion of that round; implementing moves to testing only once that handoff is
recorded), `assert_handoff_committer ISSUE ROLE PHASE ROUND` (that commit is authored and committed
by the role's own App, read with `workspace_jj`), `retro_reported` (the implementer's handoff while
the issue is merging or awaiting merge), and `production_check_reported` (the implementer's handoff
in production check or done).

## lib/namespace-rig.sh

The namespace rig of a stage proof that runs pods in a shared cluster namespace. Stage 4a sources
it.

```sh
. "$root/scripts/e2e/lib/namespace-rig.sh"     # sourced, never run
```

The caller sets `operator` (the admin kubectl context), `namespace`, `project` (the run's
`legion.dev/project` label), `project_prefix` (the prefix every run project of the proof carries,
`s4a-`), `record` (a file with one Sandbox name per line), `work`, `evidence`, and `torn_down` and
`compared` empty; it defines `begin`, `note`, `pass` and `fail`, which exits.

| function | does |
| :--- | :--- |
| `op ARGS…` | `kubectl --context $operator -n $namespace ARGS…` |
| `snapshot FILE` | writes the namespace's Sandboxes, Secrets, PVCs and pods that carry the run's project label or none, sorted |
| `teardown` | runs once and never fails. It refuses a project without `project_prefix`, so a mistyped project cannot select another run's objects; deletes every recorded Sandbox by name, then the Sandboxes labelled with that exact project; then lists the project's objects every 2 s, up to 150 listings, until none is left. Secrets and PVCs the Sandboxes' own deletion has not taken by the 90th listing are deleted by that exact label once, on the first listing from then on that answers; three failed listings in a row end the wait, naming the context and its error |
| `namespace_clean` | the check `namespace-clean`: a fresh snapshot, written to `$evidence/namespace-after.txt`, must equal `$evidence/namespace-before.txt` |
