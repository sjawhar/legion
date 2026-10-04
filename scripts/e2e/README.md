# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these; `lib/` holds what
the stage scripts share: standalone helpers they run, and the files they source
([`lib/rig.sh`](#librigsh), [`lib/workflow.sh`](#libworkflowsh),
[`lib/namespace-rig.sh`](#libnamespace-rigsh), [`lib/omp-home.sh`](#libomp-homesh)), which `shellcheck` follows from this directory
through `scripts/e2e/.shellcheckrc`.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state`, registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
| `stage2-tmux-supervision.sh` | the Go daemon supervises real Oh My Pi sessions — the pinned build with this checkout's plugin in an isolated profile — in its private tmux server, against a real Envoy listener and NATS: the plugin gate refuses another contract, a disabled plugin, a missing skill, a skill only the role prompts load, and a task agent whose model role no one configured; an agent registers, holds its Envoy role and is ready; a task queued before ready runs once, its model turn through the Hawk model gateway, and a retried frame starts no second turn; a killed pane resumes the same session; suspend and resume keep it; a stale hello is refused; an agent that never registers is retired at the deadline and counted; a restart re-adopts every live pane; an orphan is reaped after the grace; the OMP process's environment is the isolated one. Devbox only |
| `stage4a-sandbox-runtime.sh` | the Agent Sandbox runtime (`internal/runtime/sandbox`) on the production cluster, driven through the Legion daemon's restricted identity and nothing more: the Agent Sandbox install check accepts and refuses by name; the image probe Sandbox passes; a root provisions its workspace, registers, runs under gVisor and adopts its working copy's author; workers join the root's node, and schedule anywhere when no tree pod is scheduled; suspend, resume, a same-agent refusal, a pod killed in place, a relaunch before registration, and two concurrent provisions each hold; a fresh runtime re-adopts every live pod; the orphan sweep honours its grace; releasing the tree leaves nothing, and the namespace matches its snapshot. Devbox only |
| `controller-start-tmux.sh` | the operator-launched controller on the Go daemon under tmux: `legion start --check-config` passes a real config and names the key on each broken variant, running no key command; the boot gate refuses a plugin of another contract; `legion state --config` runs no key command; `legion controller start` refuses a group-readable operator token file, claims the controller role, shows in `controllerLocator`, runs Oh My Pi interactive with the controller environment and its secret only as a file, leaves Ctrl-C to Oh My Pi, and exits with its code; `legion status` from an operator shell mints its grant with the operator bearer; a second start revokes the first's capability and grants; the controller liveness probe reads the live listener. Devbox only |
| `dispatch-user-turns.sh` | a person's direct Send or Aside from Dispatch's conversation page reaches a real Oh My Pi session — the pinned build with this checkout's plugin in an isolated profile — as that person's own user turn, the body alone, while a BTW stays a side question; a frame a session forged claiming a person wrote it, a broadcast, an issue message, a Legion role notice and a session's re-send of the person's BTW through the retry route each arrive as a card; a Send the session got as a card stays one when a frame is forged for it inside the accept's minute, while the person's retry of it is their turn; the page shows each message once; after the session restarts, a replay of the Send's own envelope and a frame forged naming a Send made while it was down, over a minute old, each inject nothing, and neither does a frame a bare bus client forges for a failed Send inside its minute; and Dispatch records only the Send, the Aside and the carded Send's retry as accepted. Devbox only |
| `verifiers-staging-token.sh` | `dispatch` and the Envoy listener authenticate a projected service-account token the staging EKS cluster actually minted — the right audience is accepted, the other binary's audience and a missing bearer are refused, each shared token still works, half an OIDC pair and an issuer that does not answer refuse the boot, and a refused token leaves its failure class in the log and nowhere else |
| `TestRealGitHubCredentialSurface` | the real `api.NewServer` and built `legion` binary use the implementer and reviewer Apps to identify as their bots, list the smoke repository's pull requests, refuse a merge before GitHub receives it, and clone the smoke repository through `legion credential` alone. Devbox only |

## TestRealGitHubCredentialSurface

```sh
LEGION_REAL_GITHUB=1 LEGION_TEST_PG_DSN=postgres://… \
  go -C packages/daemon test -count=1 ./internal/api \
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
(`packages/daemon/cmd/legion`), so it proves the tree you are standing in.

The daemon supervises its agents under tmux, and it refuses to start without what a launch needs
— tmux on `PATH`, an `operator_token_file`, and an OMP to run — and before anything else its
plugin gate holds the Oh My Pi plugin a pane would load to this daemon's contract
(`packages/daemon/internal/daemon/bootgate.go`). The run supplies all of it: its `legion.yaml`
names a 0600 operator token file in the work directory; `LEGION_OMP_PATH` points at a stub there
that answers the gate's load probe (`omp models --extension <probe> --json`) as a loaded plugin does
— `LEGION_PLUGIN_LOADED=yes` and, beside it, `LEGION_PLUGIN_LOADED_FROM=file://…/dist/legion.js`
inside the package the gate read; `LEGION_PROMPT_AGENTS=resolved` and `LEGION_PROMPT_SKILLS=resolved`
when the gate asks for the task agents and skills the role prompts name; and
`LEGION_AGENT_MODELS=resolved` when it asks whether those agents' models resolve — and exits 1 on
anything else, because this proof launches no agent; and the daemon runs with `HOME=<work>/home`
and no `OMP_PROFILE`/`PI_PROFILE`, where
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

On every pull request, the required `typecheck` job of `.github/workflows/pr-and-main.yaml` runs
`go vet -tags e2e ./...` (which also compiles the e2e-tagged Stage 4a harness) in
`packages/daemon`, and its required `test` job installs `tmux` — the tmux runtime's tests drive a
real tmux server and skip without one, and the daemon refuses to start without it — and the pinned
`jj` (through mise, as the pi-envoy job does) — the workspace tests drive a real jj, and the daemon
resolves jj at boot and refuses to start without it — then runs `go test ./...` against its
`postgres:16` service (`LEGION_TEST_PG_DSN`), then this script with `LEGION_E2E_PG_DSN` pointing at
the same service, so the script runs no docker of its own there.

## stage2-tmux-supervision.sh

```sh
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/stage2-tmux-supervision.sh     # → "stage 2 e2e: PASS", exit 0, in about four minutes
```

**Devbox only.** It runs a real Oh My Pi that calls a real model, so it needs `go`, `docker`,
`jq`, `curl`, `ss`, `tmux`, `socat`, `bun`, `mise` (with the pinned OMP build it installs if
missing), the `secrets` CLI holding `GEMINI_API_KEY_TESTS` (agent tier: no YubiKey touch), and
the operator's model gateway access: `LEGION_E2E_MODEL_GATEWAY_URL` naming the gateway's Anthropic
endpoint (required; [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh)), `hawk-token` on
`PATH`, their `hawk login`, and the GNOME keyring holding it unlocked (every reboot locks it; the
`unlock-keyring` skill). CI runs the unit and integration tests, not this script; what only this
run proves is OMP's real RPC frames, `--resume`'s same-agent behaviour, the one-word
`--append-system-prompt`, the plugin's strict parse of the Go daemon's answers, the plugin gate
against an installed manifest, the provider-key path, a pane's model turn through the gateway, and
the Envoy role claim.

What it stands up, all of it the run's own:

- **Postgres**: `LEGION_E2E_PG_DSN` when set; otherwise a `postgres:16` container on tmpfs
  (`legion-e2e2-pg-<pid>`) on an ephemeral loopback port — tmpfs rather than the image's anonymous
  volume, which a box whose docker volume subsystem stalls would hang on.
- **NATS and the Envoy listener** on the host: a `nats:2.10 -js` container
  (`legion-e2e2-nats-<pid>`), and `packages/envoy`'s `cmd/listener` built into the work directory
  and started with a fresh API bearer, which reaches every pane as the 0600 file `envoy_token_file`
  names.
- **The plugin**: this checkout's `pi-legion-envoy`, packed as the release packs it, installed into
  the OMP profile `legion-e2e2-<pid>-<epoch>` by `lib/install-plugin-profile.sh`, under the run's
  own Oh My Pi home `<work>/omp-home` ([`lib/omp-home.sh`](#libomp-homesh)). The daemons run with
  that `HOME` and with `OMP_PROFILE` naming the profile, so the gate and every pane load it, and the
  operator's `~/.omp/profiles` holds none of the run; `profile-stays-in-the-run` checks that last.
- **OMP**: `omp_invocation: mise x <pin> -- omp`, the pin read from
  `.omp-pin`. At boot the daemon asks `mise where <pin>` for the
  configured tool's executable, then runs that absolute binary under `mise x <pin>` for every boot
  probe and pane. The script deliberately keeps the ordinary daemon `PATH`, where this devbox has
  `~/.dotfiles/shims/omp` first, and checks the boot log's resolved binary, the OMP child's
  `/proc/<pid>/exe`, and every command in the pane's first-child chain. That proves the wrapper
  cannot replace the configured build while mise still supplies the tool's activation.
- **The model route**: Anthropic through the Hawk model gateway, installed into the profile by
  [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) at setup, while the script's shell
  still holds the operator's HOME and XDG directories. Its preflight mint refuses a locked keyring by name.
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
| `gate-refuses-another-contract` | edits the installed (unpacked) manifest to declare the next `daemonApiVersion`; `legion start` refuses naming both numbers; the manifest is put back byte for byte |
| `gate-refuses-a-disabled-plugin` | `omp plugin disable`; `legion start` refuses with "installed but not loaded by omp"; `omp plugin enable` |
| `gate-refuses-a-missing-skill` | the installed plugin's `dist/skills/thermonuclear-deep-review` moved aside; `legion start` refuses with "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md, roles/core/reviewer.md)", the agent definition and the reviewer's role prompt that load it; the rubric put back |
| `gate-refuses-a-skill-only-a-role-prompt-loads` | the installed plugin's `dist/skills/legion-controller` moved aside, a skill only `roles/controller-root.md` loads; `legion start` refuses with "finds no skill legion-controller (loaded by roles/controller-root.md)", which only a gate reading the daemon's own role prompts can say; the skill put back |
| `gate-refuses-an-unconfigured-model-role` | `modelRoles.oracle` removed from the isolated profile's `config.yml`; `legion start` refuses with "on its model @oracle: role oracle is not configured", naming `task agent oracle` and the prompts that dispatch it; the profile put back byte for byte |
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
| `stop` | `legion claims stop` of the root architect is refused — 409, "the tree's root claim ends only when its tree closes; suspend it to stop its process; no workflow issue backs its tree, so legion claims close ends it" — and moves nothing: its state, generation, and pane are what they were; `legion claims suspend` suspends the root; `legion claims stop` of a second worker, S2-3's tester, retires it and its pane is gone; no workflow issue backs S2-1, so `legion claims close` of its root closes the tree while its first worker is still live: the root and the worker are both retired and the worker's pane is gone; `legion stop` ends the daemon with exit 0 |
| `every-turn-through-the-gateway` | [`lib/check-model-route.sh`](#libcheck-model-routesh) over every session in the isolated profile, each subagent's included: every assistant turn was served by the `anthropic` provider, the gateway's; and its negative control, a copy of one captured session with a turn rewritten as Bedrock's, is refused |

Every wait is bounded and names what it waited for; a failed assertion prints
`FAIL <check>: <why>` and exits 1, and any other failing command names the check it ended. A failed
run also [notes](#libmodel-gateway-unservedsh) each agent that could have failed that check for
want of a model key, and still exits 1. The
`EXIT` trap — on a pass, a failure, or an interrupt — stops both daemons (SIGKILL after 10 s),
kills both private tmux servers, stops the listener, SIGKILLs any process still naming the work
directory in its command line or working directory, removes both containers and the OMP profile,
and removes the work directory when the run passed (keeping it, with `transcript.log` (the whole
run), `daemon.log`, `deadline.log`, `listener.log`, `model-gateway/hawk-token.log` and each
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
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://envoy-nats.<tailnet>.ts.net:4222 \
  bash scripts/e2e/stage3-devbox-workflow.sh     # → "stage 3 e2e: PASS", exit 0
```

**Devbox only; CI does not run this script.** It runs real Oh My Pi agents and real GitHub Apps,
then squash-merges one disposable pull request as the proof human into `sjawhar/legion-smoke`.
The run needs `go`, `docker`, `jq`, `curl`, `ss`, `tmux`, `bun`, `mise`, `gh`, `shellcheck`, the
`secrets` CLI, and the operator's model gateway access: `LEGION_E2E_MODEL_GATEWAY_URL` naming the
gateway's Anthropic endpoint (required; [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh)),
`hawk-token` on `PATH`, their `hawk login`, and the GNOME keyring holding it unlocked (every reboot
locks it; the `unlock-keyring` skill). `SMOKE_UPSTREAM_NATS` (required) names the production Envoy
NATS the GitHub bridge subscribes on by its fully-qualified name on the operator's tailnet
(`nats://envoy-nats.<tailnet>.ts.net:4222`), never a bare alias, which only a resolver's search
domain completes ([the rig-alias learning](../../docs/solutions/testing/a-rig-container-alias-that-is-momentarily-unheld-resolves-through-the-tailnet-to-production.md));
`prerequisites` refuses a run without
either, and refuses an upstream that is not one NATS URL naming a host with a dot. The script prints
neither value. The bridge connects to that upstream as the nkey user `NATS_NKEY_SEED_FILE` or
`NATS_NKEY_SEED` in the operator's environment names, and without a credential when neither is set. Every process the rigs start against their own no-auth NATS (the listener, the Envoy Dispatch
server, the Go daemon; stage 2 and the other local rigs unset every variable below outright) runs without
`NATS_NKEY_SEED`/`NATS_NKEY_SEED_FILE`, and the Go daemon also without the daemon seed's
`NATS_DAEMON_NKEY_SEED`/`NATS_DAEMON_NKEY_SEED_FILE`, since the Go client refuses an nkey against a server with no
users; `lib/nats-stream.ts`, which stage 4b runs against production NATS, keeps the operator's seed. The proof human is the devbox's ordinary `gh` — the dotfiles shim, acting as the
`sjawhar-agent` App — for its reviews, its reads, and its merge; it is never a Legion App, and the
run needs no personal access token (`GH_PUBLIC_REPO_PAT` cannot read the private smoke repository
anyway). The shim routes `gh` to that App only from the operator's own Oh My Pi session, so the run
starts there: a plain shell's `gh`, a tmux window's included, is the user's own login, a Legion
pane's is one of Legion's Apps, and a personal `GH_TOKEN` in the environment makes any session's
`gh` that token's owner. `prerequisites` refuses to start, before the run's first write to GitHub,
unless `gh` acts as `sjawhar-agent[bot]` ([`require_proof_human`](#libworkflowsh)). The daemon resolves `LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` and
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
architect's spec and gate registration, which the architect does on its own (the proof never
prompts it: its first turn is the daemon's `catch-up` notice, and the root's primary document is
the proof's one-file smoke spec), the human approval, planner, implementer pull request,
tester, reviewer, retro, merger READY, the ordinary human squash merge, production check, and
architect sign-off. It also proves three changes-requested rounds, each posted by the reviewer
pane and ended by that reviewer's completion — a review ends when its reviewer completes it, so
the round returns to implementing only once the reviewer's handoff is recorded, authored and
committed by the review App, and each review, like the final approval, names the commit carrying
that round's reviewer handoff, the head the reviewer's own handoff push made (the order the Go
reviewer prompt gives; the proof names only the decision); in round one the proof human, a GitHub
App and so a bot account like a CI bot and none of Legion's role Apps, also opens a file-level
review thread (the run reads the thread's author back and stops, naming it, when the devbox `gh`
posted as anything else, such as the user after its App routing failed). The implementer answers
that thread in round one's correction, and the thread must stay open, since the pull request
author's reply closes nothing. The round-two reviewer accepts it with `Accepted:`, and round two's
correction must leave it resolved by the implementer's `legion threads resolve`, carrying that
acceptance. Each round names one concrete correction the spec permits (a distinct line appended to the smoke file, the one product file the
implementer's pull request changed, which the run records and requires to be exactly one; the
correction counts only in that file's patch on the pull request) and reaches testing only on
that round's own implementer handoff (the daemon's phase record must hold the implementer's
handoff for that round when the issue reaches testing, and the commit carrying every planner,
implementer, tester, and reviewer handoff is authored and committed by that role's own App, read
from the issue's workspace). It also proves `pr-blocked`, READY refusing after a later spec version until a human approves it, a held
worker after its launch budget and the architect's retry relaunching it, a root whose human
decides at the design gate that no change is needed (`architect-closes-a-no-change-root`: the
architect ends its admitted tree with `close_root`, the daemon posts its reason on the issue before
it writes `done`, every status write on the issue is the daemon's, the freed slot goes to the next
waiting root, and the journal has the gate's changes request, the close, the linger, the slot
release and the architect's suspension), restart during
implementation, a pending status write while Dispatch is down, and the Go pane's
credentials: in one bash tool call of a real implementer pane, plain `gh` resolves
`<state_dir>/worker-bin/gh`, two chained `legion gh` calls authenticate as `legion-implementer[bot]`
on the command's one grant, and `gh pr merge` is refused. Then `idle-pr-read` has the implementer
and the root architect each read the pull request through Oh My Pi's `read` tool (`pr://`), with
no bash command first, once their last grant is over 60 s old — a grant's lifetime — and requires
the read's own `Created:` line back, judged from the transcript: Oh My Pi serves the read by
running `gh` with the environment it copied at its start, so this passes only when the pane named
`LEGION_GRANT_FILE` from that start and the plugin minted a grant for the read itself (LEGION-262).
`notices-reach-architects-alone` reads every phase-worker session in the isolated profile (a
session's role is its newest Envoy role claim) and fails on any workflow notice delivered to one:
every notice kind is for the architect that owns its issue, on that architect's role topic, and the
run's `pr-blocked`, production-check `phase-finished` and `worker-died` are each checked in their
architect's session where they are written.
After the sign-off the proof human removes
every `.legion/` handoff and `docs/solutions/` learning from the smoke `main` through one merged
fixture pull request, and the run checks that `main` carries none: the Go daemon has no clean-head
loop before Stage 7, so the reviewer, as its Go prompt says and with no approval sent by the proof, approves a head that still carries `.legion/`, and
without the cleanup each merge would leave the next run a base carrying another issue's handoffs.
From the proof's merge until that cleanup passes, the run holds the smoke `main`: an exclusive
`flock` on `/tmp/legion-e2e-smoke-main.<owner>-<repo>.lock`, shared by every Stage 3 and Stage 4b
run on the box (`hold_smoke_main` in `lib/workflow.sh`). Another run's merge inside that window
adds the same `.legion/` paths and GitHub refuses it as unmergeable. A run that finds the lock
held names the holder, says every minute how long it has waited, and fails after 45 minutes
naming it. The lock is on an open descriptor, so a holder that dies or is killed frees it with no
stale lock left behind.
Each check is named in the transcript;
seven negative controls demonstrate that the status-actor, held-worker, re-closed-gate, idle-read
and worker-notice assertions reject deliberately corrupted observations before the captured
observations pass again (the idle read's three: its result refused as it was before LEGION-262, the
read inside its previous grant's lifetime, and a bash command before it; the worker notice's: a copy
of one phase-worker session with a `pr-blocked` delivery appended).
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
and the credential and idle-read checks read `STAGE3_PR` (default: the newest smoke pull request).
`STAGE3_UNTIL` is the other development aid. `STAGE3_UNTIL=prerequisites` stops once
`prerequisites` has passed (the tools, the inputs, the proof human, the smoke repository read and
the model route), before the rig starts anything, and removes the scratch work directory.
`STAGE3_UNTIL=rework` drives the first issue through its three review rounds, the per-round handoff
checks, and the final review, then skips every later scenario. Such a run skips the status-actor
check, which needs the first issue's whole history, ends
`stage 3 e2e: development run from <step> finished (not the proof)` (or `until <step>`), and is
never cited as the proof; only a full run is.

Evidence survives every outcome in `STAGE3_EVIDENCE_DIR` (default a fresh
`/tmp/legion-e2e3-evidence.XXXXXXXX`, printed at exit): `transcript.log` (the whole run), every agent
transcript, the daemon, Dispatch, listener, and bridge logs, the model key command and its log of
every mint (`model-gateway/`), state captures, negative-control outputs, the pane endpoint checks,
and the production audit. A passing run's last three checks stop every process, kill the private
tmux server and remove both containers (`services-stopped`), check the model route
(`model-turns-through-the-gateway`), and close every pull request the run still has open on the
smoke repository — its own, by branch: the daemon's `legion/<project>-*` and the proof human's
`proof/clean-main-<project, lowercased>` — before removing the isolated OMP profile and the scratch
work directory, the agents' workspaces with it (`cleanup-is-complete`); each shows what it removed
gone.
The proof merges only the pull request its human-merge check merges, so the held-worker and outbox
proofs are open when the run reaches here. A close GitHub refuses fails the check with gh's reason;
a close whose branch delete failed is reported as closed with the reason the branch stayed.
On any exit the `EXIT` trap does the same teardown, except that a failure keeps the scratch work
directory and prints its path. For a run that did not pass, the trap also closes the run's own
pull requests, best effort: it prints each close to stderr, and a close GitHub refuses leaves that
pull request open and prints gh's reason, with a line saying some may still be open. A failed run
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
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://envoy-nats.<tailnet>.ts.net:4222 \
ACCEPT_PG_CONTAINER=<postgres container> ACCEPT_PG_PORT=<its host port> ACCEPT_NATS_BIN=<nats-server> \
  bash scripts/e2e/stage3-4b13b-acceptance.sh     # → "acceptance 4b.13b: PASS at <head>", exit 0
```

**Devbox only; CI does not run this script.** The live acceptance of LEGION-208 task 4b.13b. It
stands up the Stage 3 rig from the same `lib/rig.sh` and `lib/workflow.sh`, with the same two
required inputs and the same model route and proof human as
[`stage3-devbox-workflow.sh`](#stage3-devbox-workflowsh), so it too runs from the operator's own Oh My
Pi session and refuses to start in `prerequisites` when `gh` acts as anyone but `sjawhar-agent[bot]`. It creates
no Docker container: the daemon and Dispatch take their own databases in the running Postgres
container `ACCEPT_PG_CONTAINER` names
(user `postgres`, password `ci`), and NATS is the native `ACCEPT_NATS_BIN`. Real agents drive the
task's surfaces through it: a Go prompt part a restart rewrites, the refused root stops, the pane's
refusal of `legion handoff complete` from a shell in every pane kind, park and re-run, the
phase-finished notice's summary and verdict, the daemon posting and publishing the merger's READY
(directly, across a refused gate, with and without a merge queue holder, and refused at
`record.MessagePostLimit` one unit over), and the early-merge and closed-unmerged notices. Every
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

## stage4a-sandbox-runtime.sh

```sh
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic \
LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> \
  bash scripts/e2e/stage4a-sandbox-runtime.sh     # → "stage 4a e2e: PASS", exit 0
```

**Devbox only, against the production cluster; CI compiles the harness (`go vet -tags e2e ./...`
in the Tests workflow's `typecheck` job) and does not run it.** Stage 4a's gate: `internal/runtime/sandbox` drives
Agent Sandbox pods in namespace `legion` from the devbox, the way the 4b daemon will. The script
needs `go`, `kubectl`, `aws` (the runtime kubeconfig's `aws eks get-token`), `curl`, `ss`,
`diff`, and the `secrets` CLI holding `LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` (agent tier: no
YubiKey touch). The harness runs `secrets <KEY> -- sh -c 'printf %s "$<KEY>"'`: the `secrets` CLI
decrypts the key and puts it in the environment of that one `sh` child, which prints it to a pipe
the harness reads into memory. The harness decodes it there and mints the implement App's
installation token in process. The key is written to no file, appears in no argv, and reaches no
other process; only the installation token enters each claim's Secret.

Every pod carries the operator route's pod,
[`deploy/kubernetes/operator-route/pod.yml`](../../deploy/kubernetes/operator-route/pod.yml), read through the daemon's
own loader (`config.ReadPodFile`): ServiceAccount `legion-worker`, one projected token for
the model gateway's audience, and a ConfigMap holding the route's `models.yml` (anthropic through
the operator's model gateway, `LEGION_E2E_MODEL_GATEWAY_URL`, keyed by that token) and
`overlay.yml` (every role Legion's prompts reach, `enabledModels` holding each session to
anthropic, and each provider a pod could reach without the gateway disabled). Legion holds
none of it. Before the harness runs, the script creates the run's own copy of that ConfigMap as the
operator, `legion-operator-route-<project>`, labelled with the run's project, its `models.yml` with
`LEGION_E2E_MODEL_GATEWAY_URL` put in place of the route's `${MODEL_BASE_URL}`
placeholder; the harness points the pods at it, so another run in the namespace can neither see nor
delete this one's route. The harness loads the run's own copy of the route's `pod.yml`, with
`LEGION_E2E_MODEL_GATEWAY_AUDIENCE` put in place of its `${MODEL_TOKEN_AUDIENCE}` placeholder, so
the audience is the operator's and `operator-token` checks the token against it. It
also creates the run's providers Secret,
`legion-<project>-providers`, with one key (`stage4a`, a random value no model route reads) that the
harness's `provider_keys` hands every agent as `STAGE4A_PROVIDER_KEY`, so every pod and the probe run
with the providers Secret mounted, as a deployment with `provider_keys` does.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_RUNTIME_CONTEXT` | required | the kubeconfig context of the Legion daemon's restricted identity (an IAM role mapped to group `legion-daemon`) |
| `LEGION_E2E_RUNTIME_KUBECONFIG` | `~/.kube/legion-daemon-production` | the kubeconfig file holding that context, kept apart from the devbox's own |
| `LEGION_E2E_OPERATOR_CONTEXT` | `production` | the devbox's admin context, for operator steps only |
| `LEGION_E2E_IMAGE` | required | the worker image under test, by digest: a `worker-image.yaml` run on the branch under test |
| `LEGION_E2E_MODEL_GATEWAY_URL` | required | the model gateway's Anthropic endpoint, the `baseUrl` the run's copy of the fixture's `models.yml` names; checked by [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh) |
| `LEGION_E2E_MODEL_GATEWAY_AUDIENCE` | required | the audience the model gateway accepts on a worker's projected ServiceAccount token, put in place of the `${MODEL_TOKEN_AUDIENCE}` placeholder in the run's copy of the operator route's `pod.yml`; checked by [`lib/model-gateway-audience.sh`](#libmodel-gateway-audiencesh) |
| `STAGE4A_FROM` | unset | a development entry point: any check after `identity` except `stale-incarnation`, which rides `kill-pod`'s relaunch; the harness refuses any other name at `identity`, before it creates anything. `identity` always runs; the checks before the entry point are skipped, and each later check first puts the claims it needs where the full run would have left them, through the same runtime calls. The run ends `stage 4a e2e: every check from <check> passed — a development run, never the proof`, and is never cited as the proof |
| `STAGE4A_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e4a-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `transcript.log` (the whole run), `runtime.log` (the runtime's and the listener's JSON log lines), and the two namespace snapshots |
| `LEGION_E2E_AGENT_SECRETS_URL` | unset (the `secrets-*` checks report `SKIPPED-BLOCKED`) | the agent-secrets broker the run enrolls pods with — the **production** broker (Plan D, the broker design's AWS deployment plan), never a development slot (below) |
| `LEGION_E2E_AGENT_SECRETS_OPERATOR` | unset | the email of the person this run's machine login is approved by — the harness starts a `legion-daemon` machine login and prints `STAGE4A: approve machine login code XXXX-XXXX on the Dispatch credential page as <operator>`, the stage is devbox-attended so the operator enters the code and clicks Approve, signed in to Dispatch as that person, during the run (polled up to 10 minutes; a timeout, denial, or expiry blocks the `secrets-*` checks with that reason, never fails the stage); distinct from the daemon's own production credential |
| `LEGION_E2E_AGENT_SECRETS_AUTO_SHA256` | unset | the `sha256sum` of the dummy value Sami seeded into the production broker's secret store for rule `LEGION_E2E_AUTO` (pod, automatic, inject) — the harness never sees the value itself, only its hash |
| `LEGION_E2E_AGENT_SECRETS_BIN` | `$work/agent-secrets` (built by the script; not read from the environment) | the checkout's `agent-secrets` CLI (`packages/envoy/cmd/agent-secrets`), run directly from the devbox for the `secrets-old-uid-and-revocation` check's before/after-revocation reads |

**Why the production broker, with dummy rules, and not a development slot.** A dispatch-project
development slot cannot host this proof, for three reasons Plan D established: its broker verifies
tokens of the *staging* cluster's issuer, and the pods this harness launches carry the production
cluster's; it lives in the staging VPC and cannot reach production Dispatch; and it may not run a
Dispatch of its own (the GitHub App's credentials cannot cross accounts), so it can issue no
launcher credential — nothing, the daemon included, can enroll against it. Instead the proof runs
against the **production broker** with a rules file carrying only two throwaway secrets
(`LEGION_E2E_AUTO`: pod, automatic, inject; `LEGION_E2E_APPROVAL`: pod, approval by
`login:<name>` naming the same login as `LEGION_E2E_AGENT_SECRETS_OPERATOR`, inject — the broker's
rules permit only `operator` or
`login:<name>` approvers, never `issue_assignee`; both 3600 s, Sami's
values seeded after Plan D's apply) —
"before any real secret moves" is exactly this state, and it is what spec Acceptance 2's "a live
worker pod on a development slot" means here.

Two identities, so the runtime is proven under exactly the RBAC it ships with. The runtime and
the harness's own reads use the restricted one; the admin context only runs what an operator does
beside the daemon — `kubectl exec`, PVC phases, the pod uid cross-checks, a Secret's boot token
(hashed in process, never printed), node and EC2NodeClass reads for a network failure, and the
namespace list. Every evidence line names which one observed it (`[runtime]`, `[operator]`, or
`[harness]` for the listener and its resolver).

The harness hosts the worker stream itself, on the devbox's private address (from instance
metadata, never `0.0.0.0`) and port 13373 — the rigs' worker-stream port, which the devbox's
security group admits from Legion nodes beside the production daemon's 13370/13371 — and refuses
to start while anything holds it, naming the holder. Its resolver accepts
only each claim's current generation and records every hello with the claim, the generation, and
the hash of the token presented. The pods run a stub agent under the real Go shim: it appends its
pod's uid to a marker file in the tree volume's sessions directory, the file a resume names, and
sleeps, so the runtime's whole path runs with no model and no provider key. The runtime's settings:
a 5-minute boot timeout, 3 registration intervals, a 15-second termination grace, a 10-second
probe interval, storage class `gp2`, no resource requests, and no scheduling beyond the Legion
pool the runtime selects, whose NodePool floor gives every tree a node that fits it (below).

The checks, in order, each printing what it observed and then `CHECK <name>: PASS`:

| check | what it does and requires |
| :--- | :--- |
| `identity` | refuses to start unless the runtime context is set and authenticates as someone other than the operator; a SelfSubjectReview shows the assumed `…legion-daemon` role in group `legion-daemon`; `list secrets -n legion` is 403; a SelfSubjectRulesReview (`can-i --list`) in every namespace finds no grant beyond the plan's; access reviews, which reach EKS's webhook authorizer that a rules review cannot enumerate, deny every kind of impersonation, `serviceaccounts/token`, pod create and exec, secret list and create, PVC get, nodes, RBAC create/update/patch/escalate/bind, and Sandboxes outside `legion`, beside two positive controls |
| `installed` | `CheckInstalled` with production's `InstallRef` passes under the `resourceNames` grants |
| `boot-refusal-negative` | `CheckInstalled` naming `legion-no-such-controller` refuses, naming that Deployment and the 403 the `resourceNames` grant answers, without blaming the CRD |
| `image-probe` | `ProbeImage` on the image under test, with the daemon's own probe command (`--role-references` with the checkout's role prompts' references, and `--provider-env-dir`, the run having a provider key), its probe pod carrying the operator's pod, passes; its log confirms `daemon-api-version` equal to the daemon's contract and `agent-models=resolved` (every task agent the prompts dispatch resolved its model under the operator's pod), and the probe Sandbox is deleted |
| `image-probe-negative` | with `modelRoles.oracle` removed from the run's ConfigMap, `ProbeImage` on the same image is refused naming `task agent oracle` and `role oracle is not configured`; the ConfigMap is restored before the check ends, so every later check boots on it |
| `root-ready` | Spawn of the root: its Sandbox Ready, the returned incarnation the pod's uid, the init log (`pods/log`) carrying `workspace-init: /legion/workspaces/sjawhar/legion-smoke/s4a-1 on legion/S4A-1`, and a hello registered at generation 1 with that generation's token |
| `gvisor` | `uname -r` in the root pod is gVisor's emulated kernel (`…-gvisor`), not the node's, and the pod's `runtimeClassName` is `gvisor` |
| `operator-token` | the root pod runs as the fixture's ServiceAccount with `automountServiceAccountToken: false` and no API server token; the fixture's one projected token, at its mount, is a JWT for the fixture's audience, subject the pod's ServiceAccount, and exactly the fixture's lifetime, read into the harness's memory and only its claims printed |
| `pod-baseline` | the stub agent the root's shim started (read from `/proc/<pid>/environ`) has `PI_CONFIG_FILES` = the pod baseline's overlay on the state volume, then the operator's; `OTEL_SDK_DISABLED=true`, `PI_AUTO_QA=0`, `PI_CONFIG_DIR=.omp`, `OMP_SESSION_STORAGE=file`, and the operator's other variables; the shim itself (pid 1) has the operator's `PI_CONFIG_FILES` alone; the overlay is mode `444`; and the image's `omp config get compaction.remoteEndpoint`, run under the agent's environment in a repository whose `.omp/config.yml` sets it, reads `""` |
| `provider-key` | the stub agent's environment (`/proc/<pid>/environ`) carries `STAGE4A_PROVIDER_KEY` equal to the run's providers Secret's `stage4a` key, read back through the admin context and never printed; the shim's (pid 1) carries no such variable |
| `adopt-working-copy` | `AdoptWorkingCopy` with the implement App's bot identity; `jj log -r @ -T author` in `$LEGION_WORKSPACE` shows it |
| `worker-colocated` | a worker spawned while the root runs requires the tree's node (podAffinity on `legion.dev/tree`, topology `kubernetes.io/hostname`) and runs there |
| `secrets-two-pods-enrolled` | the root and the colocated worker — two pods on the operator's one ServiceAccount — are each enrolled with the broker on their hello, with the pod uid the runtime recorded; each carries exactly one projected token for audience `agent-secrets` (alone in its volume, the middleman token untouched beside it) and a 0600 `key.pem` and enrollment id owned by `legion`; `agent-secrets self --json` answers each with its own enrollment id and kind `pod`; the two ids differ |
| `secrets-automatic-grant` | the root's `agent-secrets LEGION_E2E_AUTO -- …` runs with the automatic rule's value (proven by its sha256 against the operator's `LEGION_E2E_AGENT_SECRETS_AUTO_SHA256`, never the value itself); the root's and the worker's separate `request`s each grant, with two distinct grant ids |
| `secrets-cross-pod-negative` | the worker's `agent-secrets status`/`revoke` naming the root's request or grant id is 403 `NOT_YOURS`, and the root's grant still works after the attempt; the worker's own key beside a copy of the root's enrollment id is 401 `PROOF_INVALID` (the proof's thumbprint is not the enrollment's) |
| `secrets-copied-token-negative` | an enrollment naming the worker's pod uid and thumbprint but the root's projected token is refused 403 `POD_IDENTITY_MISMATCH` — a copied token alone binds nothing |
| `secrets-self-enroll-negative` | from inside the root's pod, an enroll attempt bearing the pod's own boot token as if it were a launcher credential is refused 401, and the pod's enrollment (`agent-secrets self`) is unchanged after |
| `secrets-approval-ask` | a request for the approval-gated secret comes back pending with a credential-request record id (ruling 16: the broker's rules pick the approver at request time, naming no issue); the harness prints `STAGE4A: approve credential request <record id> for LEGION_E2E_APPROVAL on the Dispatch credential page as <operator>` and polls, exactly as the machine login does, for up to 10 minutes until the operator's real approval settles it granted (success), denied, or expired (both failures) — always attended, with no cancel-and-cleanup path |
| `suspend` | Suspend of the worker: when it returns the runtime's watch no longer holds the claim; Sandbox `Suspended`, pod gone, tree PVC `Bound`, `Probe(recorded)` gone; over the settle window Observe delivers no observation of the worker evaluated after Suspend returned (an observation's `At` is stamped as its evaluation ends, and Observe re-reads the recorded incarnation before it sends) |
| `no-affinity` | with the root suspended and no tree pod scheduled, a second worker carries no affinity, runs, and mounts the tree PVC; suspended, the resumed root carries none either |
| `resume` | Resume of the first worker: the affinity is back, a new incarnation, a hello at the next generation with its token, and the marker holds exactly the old and new pod uids |
| `same-agent-negative` | a Resume naming a session file the volume lacks: the init container refuses (`Refusing to start S4A-1 fresh`), observed as gone with the init log; resumed correctly, the marker holds two agents and never the refused pod |
| `kill-pod` | `kubectl exec … sh -c 'kill 1'` on the worker: gone with the old uid and the main container's exit code; `Resume(prev=dead)` relaunches through `Suspended` (the Sandbox's generation moves by exactly two) |
| `stale-incarnation` | across that relaunch, every observation carrying the new uid is alive or uncertain, the gone carried the old uid, and `Suspend(old)` leaves the Sandbox `Running` on the same pod |
| `secrets-old-uid-and-revocation` | the worker's pod is killed and its claim resumed to a new uid. Before the kill, a copy of its key directory (the harness's instrument — the accepted boundary is that whoever holds a pod's key *is* that pod until the daemon revokes it or the lease ends) works from the devbox; after the daemon's revocation (on the observed `Gone`) the same copy is 401 `PROOF_INVALID`. An enrollment for the new pod naming the *old* uid is refused 403 `POD_IDENTITY_MISMATCH`; enrolled on its own hello instead, the resumed pod gets a fresh automatic grant |
| `respawn-before-register` | a claim spawned on a token the resolver withholds, suspended before any hello, spawns again over its Sandbox: a new uid, the Secret's boot token rotated to generation 2's, and generation 2 registered |
| `concurrent-provision` | a new tree's root and a child worker spawned at once: both provision their workspace, the two `workspace-init` runs do not overlap (the runtime serializes them; `flock` does not reach across gVisor pods), and the volume holds one clone, with both jj workspaces, that passes `git fsck --connectivity-only` |
| `re-adopt` | the listener and runtime closed, one worker killed while none runs, then a fresh listener and `sandbox.New` with `ReconcileOrphans(known)`: the living claims are alive with their recorded incarnations and unchanged pods and Sandbox generations, the killed one is gone with its recorded uid, and every living shim says hello again with its current token |
| `orphan-sweep` | a running claim left out of `known` survives a sweep with a 1-hour grace, and one with a 1-second grace while it is the sweeping runtime's own unreleased launch (a claim launched after the daemon read its claims); once the runtime and listener are replaced, as a crash before the claim was persisted would leave them, a 1-second sweep deletes it. The suspended claim's Sandbox and every known one survive each sweep, and every running claim says hello again to the replaced runtime |
| `release-tree` | Release of every claim, the suspended one with a nil locator: no Sandbox, `-boot` Secret, pod, or tree PVC of the run is left (the operator's providers Secret stays for the teardown: Release never deletes an operator's object) |
| `namespace-clean` | the script's last step, after the teardown and outside the harness: the namespace's Sandboxes, Secrets, PVCs, pods and ConfigMaps that carry the run's project label or none are exactly the snapshot taken before the run |

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

What the run had to learn about production:

- **A tree needs a node that fits it.** Every pod of a tree requires the node of the tree's first
  scheduled pod, since the tree volume is a single-node EBS volume. With nothing more, Karpenter
  puts that pod on a `c7a.medium`, whose 8 pod slots its 7 daemonsets all but fill, so no second
  pod of the tree can ever join it. A CPU request on the root does not fix it: when a child is
  placed first, as the concurrent launch showed, the root must join the child's node, and there a
  2-CPU root beside another tree's root stayed Pending on `Insufficient cpu`. The `legion`
  NodePool's floor, `karpenter.k8s.aws/instance-cpu` Gt 3 (set in the cluster's own infrastructure
  code), keeps every Legion pod on a node of at least 4 vCPUs with room for the tree, and no pod
  requests anything.
- gVisor on production reports `4.19.0-gvisor` from `uname -r`.
- The worker image has no `kill` binary; the exec runs the shell's builtin.

## stage4b-sandbox-tree.sh

Stage 4b's gate for the Go coordinator: the Go daemon drives real issue trees on the Agent Sandbox
runtime. It runs in the production cluster's namespace `legion`, against production Dispatch, the
production Envoy listener and production NATS, in the disposable Dispatch project LEGSMOKE and the
smoke repository `sjawhar/legion-smoke`. The daemon runs on the devbox as the Legion daemon's
restricted identity, and its pods dial its worker stream on the devbox's private address.

```sh
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest> \
  LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> \
  LEGION_E2E_DISPATCH_URL=https://<dispatch> LEGION_E2E_ENVOY_URL=http://<listener>:<port> LEGION_E2E_NATS_URL=nats://<nats>:4222 \
  LEGION_E2E_DISPATCH_TOKEN_SECRET_ID=<secret id> LEGION_E2E_ENVOY_TOKEN_SECRET_ID=<secret id> \
  bash scripts/e2e/stage4b-sandbox-tree.sh        # → "stage 4b e2e: PASS", exit 0
STAGE4B_UNTIL=<checkpoint> …                      # a development run: stops after that checkpoint, never PASS
STAGE4B_DESIGN_GATE=root-issues STAGE4B_UNTIL=spec-posted …   # the design gate, armed, on tree 1 alone
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
`LEGION_E2E_DISPATCH_TOKEN_SECRET_ID` and `LEGION_E2E_ENVOY_TOKEN_SECRET_ID` are the Secrets Manager
ids of the production Dispatch agents' bearer and the production Envoy listener's API token, which
`prerequisites` reads with the devbox admin role into 0600 files. `prerequisites` refuses each of the
three when it is unset or malformed (the audience through
[`lib/model-gateway-audience.sh`](#libmodel-gateway-audiencesh)), again naming the variable and never
its value.

`STAGE4B_UNTIL` must name a checkpoint below; any other value is refused. `STAGE4B_SKIP_CONTROLLER=1`,
refused without `STAGE4B_UNTIL`, runs none of `controller`'s checks and only takes tree 3 out, printing `CHECK controller: SKIPPED (…)`,
so a development run can reach a later checkpoint while a defect of the controller's own is unfixed. `STAGE4B_EVIDENCE_DIR`
keeps the evidence (default: a fresh `/tmp` directory, printed at the end): the transcript, the
daemon log, `run.json` (source revision, image and plugin), the pod watch, each checked pod's spec,
every agent transcript (the tree pods' and, under `transcripts/controller/`, the operator's
controller's), the interest samples, the audit files and the negative controls. What the
run built is printed by [`lib/built-from.sh`](#libbuilt-fromsh). Its verdict is one line, just before the evidence line:
`stage 4b e2e: PASS`; `stage 4b e2e: FAIL (check <check>)`, after [notes](#libmodel-gateway-unservedsh)
on whether the controller could have failed that check for want of a model key; or
`stage 4b e2e: BLOCKED (check <check>)` when the checkpoint could not run and the teardown checks
(`namespace-clean`, `production-audit`) passed, which makes the run no verdict on the change while
the checkpoints before it stand, and gets no notes. A failed teardown check outranks every reason
the run stopped: it prints its own `CHECK <name>: FAIL` line (the audit's names the run's writes and
subscriptions outside LEGSMOKE, as the `production-audit` checkpoint does), and the verdict is
`stage 4b e2e: FAIL (check <teardown check>, in the teardown after check <check>)` whenever the
checkpoint that stopped the run did not fail itself: a `STAGE4B_UNTIL` run's last checkpoint, a
blocked checkpoint, a signal. Every verdict but the pass exits non-zero: 1, or the
stopping signal's 129, 130 or 143 when the teardown was clean.

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

Three roots are set todo under `admission_cap: 2`:
- Tree 1 runs the whole workflow with real agents to `done`, through one changes-requested review
  round in a thread the reviewer opens, lingers, and closes.
- Tree 2 runs through its planner beside tree 1's implementer, on its own node, carrying the
  repository-configuration fixture, and is then moved to backlog.
- Tree 3 is admitted when tree 2 leaves the line. It supplies the held phase the controller
  checkpoint needs, and is taken out from an operator shell.
- Tree 4 is admitted when tree 3 has been taken out. It supplies the deaths of a worker whose task
  is outstanding, and is taken out the same way.

**One run at a time.** The project, the durable consumer names, ports 13372 and 13373 (the rigs'
pair; the production daemon keeps 13370 and 13371) and the namespace label
`legion.dev/project=legsmoke` are shared.
- The run takes `~/.local/state/legion/e2e/stage4b.lock`, one path whatever `XDG_STATE_HOME` says.
- It owns the shared objects only after four checks pass: the lock, both ports free, no leftover
  `legsmoke` object in the namespace, and no `legion-go-LEGSMOKE-` consumer on the stream.
- A run refused at any of the four removes nothing.
- Each daemon start checks both ports again, since another process can take one after the
  prerequisites; once the daemon answers `/healthz`, both ports must be held by that daemon's
  process, or the boot fails naming the holder. A `/healthz` answer alone could come from another
  daemon on the same address.

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
  named by the Dispatch key: `legion-go-LEGSMOKE-dispatch` (`notifications.dispatch.issue.>`) and
  `legion-go-LEGSMOKE-github` (`notifications.github.sjawhar.legion-smoke.>`).
  - The teardown deletes both by exact name and prints `deleted` or `absent` for each.
  - `hygiene` fails if either remains, and the preflight refuses to start while either exists.
  - A SIGKILLed driver runs no trap, and the next run's preflight names what it left.
- **Production Dispatch, project LEGSMOKE**: three root issues per run. The preflight moves stale
  todo roots to backlog. The proof human's writes use the agents' bearer and name the session
  `legion-e2e4b-proof-human-<pid>` as their actor, which production Dispatch requires; it holds no
  claim, so the daemon sets back its status write on a live root as it does any outside session's.
  The run takes trees out with `legion status` from the operator shell, the daemon's own write.
- **`sjawhar/legion-smoke`**: the fixture branch `legion/<tree 2>`, and tree 1's pull request, which
  the proof human merges. The teardown closes any pull request the run left open, such as one from a
  run that stopped before the merge, and deletes each tree's branch `legion/<tree>`. Every one of
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
| `prerequisites` | the tools, the restricted context and the image by digest; the devbox `gh` acts as the proof human, `sjawhar-agent[bot]`; the lock and the two ports; nothing left in the namespace (Sandboxes, pods, PVCs, ConfigMaps) or on NATS from another run; only then does the run own the shared objects |
| `preflight` | the runtime identity is the daemon's restricted IAM role and cannot list Secrets; the Sandbox CRD and the `legion` NodePool's instance-cpu floor; LEGSMOKE has no todo root; the stream carries both halves of intake; a throwaway pod on the Legion pool reaches Dispatch, the listener, the gateway and NATS, each within three tries 5 s apart (a fresh node's first outbound connection can fail while it settles), and a service that never answers fails the check with every try's error |
| `pod-watch` | the namespace snapshot; the pod, node-event and node-memory watches start, and the Secret-value check (`lib/secret-leaks.ts`). The pod and node-event watches last the whole run: kubectl's own watch ends when the API server closes it at its watch timeout, so each lists, watches from that resourceVersion, resumes from the last version it saw when a watch ends, and lists again on 410 Gone, noting each in the transcript. Each watch asks the server to end it within 300 s, so a loop a killed driver left stops within five minutes; a watch that delivered nothing is resumed after a pause, and a line that does not parse ends that watch unrecorded |
| `boot` | the build's source is the one prerequisites recorded; `legion start --check-config` passes the `runtime: kubernetes` config, whose `pod` is the operator fixture's ([`deploy/kubernetes/operator-route`](../../deploy/kubernetes/operator-route/pod.yml)) with its ConfigMap renamed to the run's copy; the operator creates that ConfigMap from the fixture's `models.yml` and `overlay.yml`; the audit window opens and the interest sampler starts; the daemon boots, and the image probe passes (its first attempt's timeline is kept) |
| `admitted-issue-cap` | the three roots: two admitted and one waiting, in rank order |
| `spec-posted` | each admitted architect, prompted by nothing but the daemon's `catch-up` notice, posts its spec and registers the gate; with `gates.design: off` the daemon moves the tree to planning |
| `tree-separation` | tree 1's implementer and tree 2's planner run at once on different nodes, each tree on one node. Tree 2's planner has held for the driver: a tree 2 that left planning before the driver sent it anything fails here by name. The proof's instructions tell every phase worker that the daemon's task line (`Continue <title>. Issue: <key>. Phase: <phase>.`) is not the driver's instruction |
| `repository-configuration` | tree 2's workspace carries the fixture (`.omp/extensions/fixture.ts` and its `AGENTS.md`); the markers each loading path writes, and the agent's argv |
| `issue-cap-moves` | the proof human's `backlog` on tree 2's live root is set back: the next status write is `legion-daemon:LEGSMOKE`'s (a control re-attributing it must fail), tree 2's architect receives a `status-reasserted` notice naming the proof human, and tree 2 keeps its slot; `legion status … backlog` then frees the slot, tree 3 is admitted, and tree 2's pods are gone |
| `tree-moved` | tree 1 runs planner, implementer, tester, reviewer and retro to merging with real agents; the tester's adoption leaves a new empty change and keeps the implementer's author. Once both thermonuclear dispatches have an outcome, the reviewer's session, its subagents' sessions and each dispatch are kept under `review-pair/`. The reviewer first submits `COMMENT` and completes without a decision: there is no approval, the issue stays in `reviewing`, and the architect receives a completion-written `review-stuck` notice naming a commit of the pull request (`notice-review-stuck.jsonl`). The driver tells the architect only to handle that notice, naming no topic, head or decision. A message in the reviewer's session must carry the architect's `reply_role` and arrive after completion; earlier round reports cannot count. These messages are kept as `architect-ask.jsonl`; an ask before the driver's steer is noted as the stronger pass. Once asked, the reviewer requests changes with exactly one inline thread and a review naming its handoff head. The implementer appends the requested line and the tester verifies it. The re-review's decision is the reviewer's own; a second request for changes fails the proof. The thread is recorded after correction and at approval, then the issue leaves `reviewing` for retro or merging |
| `review-thread` | when the approval landed, the reviewer's thread read `isResolved: false` with the reviewer's own `Accepted:` as its newest submitted comment: the approval did not wait on a resolution only the pull request author's App can make, and that nothing makes before the merger's run (LEGION-316). Controls: the same record resolved, and with the implementer's reply as its newest comment, both fail |
| `completion-closed` | each phase worker of tree 1 — planner, implementer, tester, reviewer — is suspended once its phase ends, and its saved session answers every `handoff_complete` call of the legion tool, holds one success for each assignment it completed, and records the phase stall `closed` after that call, with no phase-stall follow-up after its last success: the 4b.13b acceptance's stall check (`stage3-4b13b-acceptance.sh`'s `pane-rule-phase-worker-and-stall`), which a suspension that stops the worker inside the call fails (LEGION-283). The sessions and each verdict are kept as `completion-<role>.jsonl` and `completion-<role>-verdict.json`. Its control: the planner's session cut at its `handoff_complete` call, the transcript such a suspension leaves, is refused |
| `review-pair` | the reviewer dispatched `thermonuclear-deep-review` and `thermonuclear-code-quality` by name, and one run of each completed. A run completes by the task-result block the reviewer received, whether by async delivery or a hub wait or jobs snapshot, saying `completed`. With no block, the subagent's own session beside the reviewer's must end in an accepted yield. Every turn of that session runs on the fixture overlay's `review` target: the task executor runs a subagent on its parent's model, silently, when the subagent's own does not resolve. A refusal (`Unknown agent`, `No model selected`) in a task result or in a run that did not complete fails with its text. tree-moved keeps the reviewer's session and the subagents' sessions as the pair settles, reading the tree volume, not the daemon |
| `first-turns` | every role on tree 1 completed a first turn in its pod |
| `token-rotation` | a pod's projected operator token (`/var/run/operator/token`, 3600 s, renewed by the kubelet at 80 %) is renewed: the token in the file was issued (its `iat`) after the pod started, in the same pod by uid. An exec that does not answer is never a token. A model turn after the renewal still runs on a model the operator fixture's `overlay.yml` gives a role |
| `idle-suspend` | a finished worker's Sandbox is Suspended with its pod gone and the tree volume bound |
| `kill-pod-resume` | once the merger's pod is running, a killed merger pod is relaunched on its session |
| `fence` | a pod the controller recreates on its own is never adopted. Once the relaunch's boot token is in the Secret, the replaced generation's token is refused, and the daemon logs `worker-stream: rejected hello (stale worker generation)` |
| `daemon-relaunch-count` | the daemon relaunched the merger, `resumed`, once for each pod the driver ended |
| `restart-mid-tree` | a daemon restart re-adopts the merger's pod and session |
| `controller` | `legion controller start` registers with the Sandbox daemon, and the controller's first turn starts itself: its session's first user message is the start message the command launches Oh My Pi with, and the model answers it, with nothing typed into its pane; tree 3's held notice reaches its session, which is under the run's own home, and `~/.omp/profiles` holds none of the controller's profile ([`lib/omp-home.sh`](#libomp-homesh)); `legion status … backlog` from the operator shell moves tree 3, and Dispatch shows it. Tree 3's planner is told to plan, and each launch of its implementer is killed once its agent is ready or in a turn with its task outstanding, so every death is charged whatever a relaunch's boot takes. Tree 3 is held by one of its implementer claim's budgets, `launch failures ran out` or `deaths with work outstanding ran out`, and any other hold fails. The reason must be the budget the daemon's own counters show at the bound (its `supervise: claim failed` line) and the one its implementer's last relaunch death leads to: charged as a death with work outstanding, or not. A death charged for a relaunch that never registered fails, since it could have had no work. The transcript names each relaunch's end, registration, death and charge. The notice reaching the controller is the checkpoint's point; the budgets' own rules are held by `packages/daemon/internal/supervise/budgets_test.go`. With tree 3 out and a slot free, the controller's walk wakes: its Oh My Pi was launched with the daemon's ``Design gate policy: `gates.design: off`.`` line; an unlabelled issue set to `todo` puts `todo on <KEY>` in its session, and the daemon's tick (`controller_wake_interval_seconds: 60`) `tick on LEGSMOKE`; a minute later that issue is still unlabelled and unrecorded, since the proof's scope says the controller hands Legion no issue, and it is set `done`; tree 3's root issue is claimed by its architect's session, and its events carry the `issue.claimed` |
| `daily-report` | the controller's daily report, which the proof's instructions exempt from their wait for a targeted message and fit to the run: an issue titled `Legion daily report (<run directory>)` appears in LEGSMOKE, parked in icebox and without the `legion` label, holding the controller session's message, which names tree 1 and the free slots within 2,000 characters; in the controller's session that message comes after the first `tick on LEGSMOKE` delivery, never in its start turn. `production-audit` then holds that the issue, like every write, is in LEGSMOKE |
| `deaths-with-work` | tree 4, admitted once tree 3 has left: its planner, killed once mid-turn, is sent its task again, told the turn was interrupted, and finishes planning; its implementer, killed after each ready with its task outstanding, is failed after 3 deaths (`budgets.deaths` 3, `supervise: claim failed` because "deaths with work outstanding ran out"), tree 4 is held and nothing relaunches it; `legion status … backlog` then takes tree 4 out |
| `done` | the merger's READY, the proof human's merge, the production check and sign-off take tree 1 to `done`; tree 1's events carry its architect's `issue.claimed`, and done leaves its root issue unclaimed. The READY packet names the pull request's head, and `reviewThreads` shows every thread resolved, the reviewer's included (`review-threads-at-ready.json`). Control: the same record with the reviewer's thread unresolved fails |
| `node-release` | tree 1's Sandboxes stay Suspended, its volume Bound, and no pod of the run is left on its node: after the pool's consolidation the node is gone, or, when a pod of another project (a production daemon running beside the run) is on it, Pending or Running, the node stays; the note says which, and a timeout lists what the node still held |
| `close` | at linger expiry tree 1's Sandboxes and tree volume are deleted |
| `re-admission` | tree 1 set todo again: the daemon logs `supervise: the tree volume was lost with the session; relaunching a fresh session` exactly once, and the fresh architect's workspace holds `.legion/workspace-recovered.json` naming `legion/<tree 1>` |
| `operator-close` | `legion claims close` on the Sandbox runtime: the close of re-admitted tree 1's live root is refused 409, and its claims, Sandboxes and pods are unchanged; `legion status … backlog` then takes tree 1 out; an operator-spawned tree closes with its worker live, the root and the worker are retired, and the tree's Sandboxes, pods and volume are gone |
| `pod-shape` | every Sandbox pod whose worker the pod watch ever saw ready is judged from the spec the watch recorded for it, deleted pods included, so no poll has to reach it: gVisor, the operator's ServiceAccount and one projected token, the run's route ConfigMap mounted as the profile's `models.yml`, the pool, Pod Security restricted, split provisioning. No pod's command, args or environment carries a value its Sandbox's `-boot` Secret held at any point in the run (`lib/secret-leaks.ts`), and every pod's Secret was seen. The watch is complete: every pod uid the shape watcher read, the driver ended, or the daemon launched is in it. Negative controls: a recorded pod with another runtime class, and a pod the watch never recorded |
| `pod-watch-verdict` | the pod watch is complete (as at `pod-shape`); no pod of the run was Evicted or had a container OOMKilled, and every claim process the daemon found dead (`supervise: process died`) was one the driver ended. The resume that finds the tree volume lost is the exception, by its detail (`the tree volume was lost: …`), counted by `re-admission`. The memory hog was OOMKilled. Synthetic OOMKilled and process-died controls both fail |
| `hygiene` | the daemon stopped, the teardown ran, and the run's consumers are gone; `namespace-clean` follows it |
| `namespace-clean` | the namespace's Sandboxes, Secrets, PVCs, pods and ConfigMaps that carry the run's project label or none are exactly the snapshot taken before the run |
| `production-audit` | no write by the run outside LEGSMOKE and no interest outside it; the verdict refuses a synthetic outside issue, and the collector finds a real outside writer's events |

## controller-start-tmux.sh

Task 4b.5's acceptance for `legion controller start`, on tmux. The Go daemon runs on a real Postgres
with this checkout's plugin in an isolated OMP profile, against a real Envoy listener and NATS, and
the command runs in real tmux panes, as an operator would. The pinned Oh My Pi is launched through
mise. The run needs no model route: the controller's one model turn, the start message
`legion controller start` opens it with, fails against the profile's default, a static-key
provider that listens nowhere, and no check reads its answer. The profile names the roles the
task agents use and that default, which is all the boot gate's agent-model check resolves.

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
| `check-config-passes` | `legion start --check-config` passes the run's tmux config. 4b.5's "`--check-config` passes on the proof's config" is the full-tree driver's, whose config is `runtime: kubernetes`: `stage4b-sandbox-tree.sh`'s `boot` checkpoint runs it before the daemon starts |
| `check-config-names-each-broken-key` | an unknown key, a zero `admission_cap`, a bad `envoy_url`, and a Dispatch URL without its token file are each refused by name, and neither App's `private_key_command` runs (names the key on each broken variant) |
| `gate-refuses-the-previous-contract` | the boot gate refuses the plugin with its manifest set to the contract before the checkout's, naming both contracts (the plugin at the bumped contract refuses the previous one by name) |
| `daemon-serves` | the daemon answers `/healthz` with no `controllerLocator` yet |
| `state-config-runs-no-key-command` | `legion state --config` on a config whose key command would fail still reads the state, and the command never runs (`legion state --config` runs no key command) |
| `operator-token-file-others-can-read-is-refused` | a 0640 operator token file is refused by path and mode, the daemon mints nothing, and no state directory is written (a group-readable operator token file is refused naming it) |
| `controller-claims-the-role` | the controller registers, `GET /legion/v1/state` shows `controllerLocator` (`runtime: tmux`, `external: true`), the Envoy role `legion-<project>-controller` names its session, and its Oh My Pi has the controller environment, its secret only as a 0600 file, interactive (claims the controller role, shows `controllerLocator`) |
| `ctrl-c-reaches-omp-not-the-cli` | one Ctrl-C leaves both `legion controller start` and Oh My Pi running (decision 3: Oh My Pi owns the terminal) |
| `status-from-an-operator-shell` | `legion status --operator-token-file` mints a grant with the operator bearer and reaches the status route; the rig has no Dispatch, so the route answers 500 naming it. A wrong bearer is refused 403 (decision 3's operator path; the status write itself is the full-tree proof's) |
| `second-start-revokes-the-first` | a second start takes the role and the locator, the daemon logs mints 1 and 2, the first capability's registration is refused 403, and a grant minted before the second start, which redeemed then (500 `DISPATCH_UNAVAILABLE`, past the grant check), is refused 403 `GRANT_UNAVAILABLE` after it (a second start revokes the first's capability) |
| `liveness-probe-against-the-listener` | `controller.Prober` on the live listener calls the second session alive and the first gone |
| `exit-code-is-oh-my-pis` | Ctrl-D quits Oh My Pi cleanly and the command exits 0, as Oh My Pi did. A non-zero code is carried through too; the stub-omp unit test (`cmd/legion/controller_test.go`, exit 3) holds that |

## dispatch-user-turns.sh

LEGION-394's acceptance: a person's direct Send or Aside from Dispatch's conversation page is the
session's own user turn, and everything else keeps its Envoy card. One real session — the pinned
Oh My Pi (the `github:sjawhar/oh-my-pi` mise tool) with this checkout's plugin in an isolated
profile, launched with `controller-start-tmux.sh`'s `operator_env` line, its cwd under `/tmp` —
registers with a real Envoy listener and NATS. Dispatch, built from the checkout with NATS on and
its trusted identity header, serves the SPA this checkout builds, and Playwright drives the
conversation page as the person the header names. The session's model turns go through the model
gateway on the operator's own hawk login ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)).

```bash
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/dispatch-user-turns.sh     # → "dispatch user turns e2e: PASS", exit 0, in six to eleven minutes (the listener's drop of the stopped session is most of the spread)
```

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_MODEL_GATEWAY_URL` | required | the model gateway's Anthropic endpoint; checked by [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh) |
| `DISPATCH_USER_TURNS_OMP` | the pinned build, `github:sjawhar/oh-my-pi@$(mise current github:sjawhar/oh-my-pi)` | another Oh My Pi, as a mise tool spec |
| `DISPATCH_USER_TURNS_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e-user-turns-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `logs/` (the listener, Dispatch and the SPA build), `checks/` (the page's screenshots and the session's pane at exit) and `session.jsonl`, the session's transcript |

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
| `the-page-shows-each-message-once` | a fresh page, opened while the session's stream still holds the turns it tagged (the ring lives in the process, so the restart below empties it): the replay the page is served carries the Send, the Aside and the retried Send as user messages tagged with their Dispatch message ids, beside their stored copies, and the page shows each person's message once |
| `the-session-stops` | the Send's own envelope is read back from the notification stream; Oh My Pi is killed and the listener drops the session |
| `a-send-while-the-session-is-down-fails` | the person sends the stopped session a Send, which Dispatch records as one failed attempt naming it |
| `a-replay-after-restart-gets-a-card` | once Oh My Pi is continued (`--continue`, the same session), the Send's own envelope is sent again: a card, and the Send is still one user message |
| `a-frame-forged-after-the-restart-naming-an-old-send-gets-a-card` | once the Send made while the session was down, which Dispatch recorded as failed, is more than a minute old, a frame forged with the listener token names it as a person's steer: it arrives as a card, with neither its stored nor its forged text a user message |
| `a-frame-forged-for-a-failed-send-inside-the-minute-gets-a-card` | the listener drops the session's registration (`DELETE /v1/sessions/{id}`), so the person's Send to it fails with no frame sent, and within 45 s a bare bus client publishes a frame naming that failed attempt straight onto the session's agent subject: it arrives as a card, with neither text a user message |
| `dispatch-records-only-the-turns-the-session-took` | Dispatch's thread read, which the Agents page reads, records the Send and the Aside accepted at attempt 1 and the carded Send at its retry, attempt 2, and never the BTW, the Send made while the session was down or the Send made while the listener listed no session |

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

Packs this checkout's `@sjawhar/pi-legion-envoy` the way the release packs it, and prints the
tarball's path: what `npm pack` ships (`package.json` `files`: `dist/` with the two bundles and
`prepack.sh`'s `dist/skills`, `agents/`, and the packed manifest). Every script that installs a
branch-built plugin packs through it: [`lib/install-plugin-profile.sh`](#libinstall-plugin-profilesh),
which `controller-start-tmux.sh`, `stage2-tmux-supervision.sh`, `stage3-devbox-workflow.sh`,
`stage3-4b13b-acceptance.sh` and `stage4b-sandbox-tree.sh` call, and the grant rig's branch mode
(`packages/pi-envoy/scripts/grant-rig/setup.sh`). The worker image packs on its own, as the release
does: `packages/daemon/docker/worker.Dockerfile`'s plugin `RUN` rewrites `omp.extensions` with `jq`
and runs `bun pm pack`, and `prepack.sh` refuses to pack any other `omp.extensions`, which holds all
of them to the same manifest.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
tarball=$(scripts/e2e/lib/pack-plugin.sh "$work/pack")
```

`<out dir>` is created when missing, and refused inside the checkout (jj would snapshot the tarball)
or when it already holds a `.tgz`. Stdout is exactly one line, the tarball's path; every step's own
output goes to stderr.

The steps are the release's, run in the checkout — a copy of `packages/pi-envoy` cannot build,
because `prepack.sh` copies `../../skills` and the bundle resolves `@legion/*` through the root's
`node_modules`:

1. save `packages/pi-envoy/package.json` and arm an `EXIT` trap that copies it back byte-identical
   (`.github/workflows/release.yaml`'s pi_envoy job saves it to `$RUNNER_TEMP/pi-envoy-manifest.json`
   in "Point extensions at the packed bundles");
2. rewrite `omp.extensions` to `["dist/envoy.js","dist/legion.js"]` with `jq` (the same step, and the
   `jq '.omp.extensions = …'` line of `packages/daemon/docker/worker.Dockerfile`'s plugin `RUN`);
3. `bun pm pack --destination <out dir>`, whose `prepack` builds `dist/` (the release's "Pack
   extension" step, `packages/pi-envoy/scripts/prepack.sh`); the bundles inline `package.json`, so
   they are built while it names the packed bundles, as the release builds them;
4. copy the saved manifest back and check it byte for byte (the release's "Restore committed
   manifest").

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

Runs in one checkout take turns from the save to the copy back, under a `flock` on the manifest
itself (rewritten and restored in place, so the lock's inode lasts the whole window); a run that
has to wait says so on stderr. Without the lock, a run that starts while another has the manifest
rewritten saves that rewrite as its "before" and puts it back at its own exit: both runs exit 0 and
jj snapshots the rewritten `package.json`. Two stage proofs in one checkout, or a stage proof and the
grant rig, can pack at the same time, and the lock takes them in turn.

## lib/install-plugin-profile.sh

Installs this checkout's `@sjawhar/pi-legion-envoy` into a named OMP profile, packed the way the
release packs it, so a stage proof runs the branch-built plugin and the user's own profiles are never
touched.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
. scripts/e2e/lib/omp-home.sh && make_omp_home "$work/omp-home"
manifest=$(scripts/e2e/lib/install-plugin-profile.sh --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/plugin")
# → $work/omp-home/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json
```

| flag | meaning |
| :--- | :--- |
| `--profile <name>` | the OMP profile to install into (`OMP_PROFILE=<name>`). Refused when OMP would read it as its default profile — empty, all whitespace, or `default` — since that is the profile every plain `omp` uses. Any other name goes to OMP as given, and OMP refuses one it cannot use. |
| `--home <dir>` | the `HOME` Oh My Pi runs under for the profile, which is then `<dir>/.omp/profiles/<name>`: the run's own home from [`make_omp_home`](#libomp-homesh). Refused when it is the caller's own `HOME`, when `HOME` is unset or empty (it could not be compared), and when `<dir>/.omp` does not exist. |
| `--dest <dir>` | where the tarball is unpacked. `omp plugin install` links this directory into the profile rather than copying it, so it **is** the installed plugin and must outlive the run. Refused inside the checkout (jj would snapshot it, symlinks resolved first) and when it exists and is not an empty directory (an unpack over an earlier build would keep that build's stale files). |

All three flags are required; each refusal names its flag and exits 2. Stdout is exactly one line, the
installed manifest's path as `HOME=<home> OMP_PROFILE=<name> omp plugin list --json` reports the plugin; that is
the manifest the daemon's contract gate reads under the same profile
(`pluginManifestPath`, `packages/daemon/internal/daemon/bootgate.go`). Every step's own output
goes to stderr.

The plugin is packed by [`lib/pack-plugin.sh`](#libpack-pluginsh), the release's pack steps run in the
checkout, into the run's `mktemp -d` directory (never beside `package.json`, so an interrupted run
strands no `.tgz` in the checkout). Then:

1. unpack the tarball into `<dir>` (`worker.Dockerfile`'s `mkdir -p /out/pi-legion-envoy` and
   `tar xzf ./*.tgz -C /out/pi-legion-envoy --strip-components=1`);
2. `OMP_PROFILE=<name> omp plugin install <dir>` (`worker.Dockerfile`'s
   `omp plugin install /opt/legion/pi-legion-envoy`);
3. `OMP_PROFILE=<name> omp plugin list --json` must show the plugin at the tarball's version,
   enabled, and resolving to `<dir>`.

Steps 2 and 3 run the Oh My Pi the daemon pins (`.omp-pin`, through `mise x <pin>`) under
`HOME=<home>`, from `<dir>`, rather than the `omp` on the caller's `PATH`: an operator's wrapper
there (the devbox's `~/.dotfiles/shims/omp`) reads its own files from `HOME`, which is the run's.

Each step cites its source by what it runs, never by line number: the lines move with every edit
above them.

The script creates the profile and `<dir>` and removes neither; the caller does, with its work
directory, which holds both (the profile holds `plugins/` — the link and `omp-plugins.lock.json` —
and OMP's `logs/`).

### The natives download

OMP's native modules (`pi_natives.linux-x64-{baseline,modern}.node`, ~350 MB) live in
`$HOME/.omp/natives/<omp version>/` — `$XDG_DATA_HOME/omp/natives/` when `$XDG_DATA_HOME/omp`
exists — and every profile under that `HOME` shares them (`getNativesDir`,
`@oh-my-pi/pi-natives/native/loader-state.js`); a profile has no natives of its own. OMP writes them
on its first run under a `HOME` that has not run this OMP version, and in this script that run is
`omp plugin install`. The run's own home links `.omp/natives` to the operator's
([`lib/omp-home.sh`](#libomp-homesh)), so a fresh CI runner, a container, or a newly bumped OMP pin
pays ~350 MB there, once, into the operator's cache; on a box where the pinned OMP has already run,
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
own hawk login, the route every devbox agent session uses (`~/.omp/agent/models.yml`:
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

## lib/model-gateway-unserved.sh

Says whether the key command [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) wrote
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
  Pi 18.2.9 keeps before it runs a failed key command again: a request inside it fails with no key
  and runs no command, so it leaves no line, and a check's first calls can fail on a starve from
  just before the check began.
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

Proves a tmux stage proof's agents reached the model only through the gateway: every agent turn
the isolated OMP profile recorded, each subagent's included, was served by the `anthropic`
provider, the one [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) routes to the
gateway and leaves enabled. Stage 2 runs it last, after `stop`. Stage 3 runs it once every agent
process has stopped and before it copies the transcripts, then again over the copies, which must
hold the same turns, sessions and subagents.

```sh
bash scripts/e2e/lib/check-model-route.sh --sessions "$work/omp-home/.omp/profiles/<profile>/agent/sessions" --control "$work/model-route-control"
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
| `until_true SECONDS WHAT CMD…` | runs CMD every half second until it succeeds. After SECONDS of wall time, each poll's own run included, it runs `timeout_hook` and fails naming WHAT. It says what it waits for on entry and every 60 s. A poll that never returns would hold the wait, so callers bound their remote calls. A guard that writes `$evidence/pane-endpoint-violation.txt` makes the next wait abort, naming the violation |
| `pick_port VAR` | assigns VAR a port from [`lib/free-port.sh`](#libfree-portsh) that no earlier pick of the run returned. It assigns in place: a command substitution would run it in a subshell and lose the run's set of picks |
| `start_process NAME CMD…` | starts CMD in the background, appending to NAME's log, and sets `NAME_pid` |
| `log_size NAME` | the size of NAME's log, the offset `await_start` reads a start's own lines from |
| `await_start NAME PID OFFSET SECONDS WHAT CMD…` | waits, bounded, for CMD to succeed while PID lives. It returns 2 when the service exited on `address already in use` after OFFSET, the one race a pick before the bind cannot close, so the caller picks again; any other exit fails naming the log |
| `stop_pid PID` | TERM, then KILL after 10 s; best effort, so a failed cleanup never hides the check that failed. An empty PID does nothing; a PID that cannot name one process of the run (0 however spelled, so any value with a leading zero, 1, a negative or non-numeric value, the calling shell or its process group's leader) is refused with a printed line, since `kill 0` signals the caller's whole process group |
| `stop_tree PID` | stops a background loop with every process under it: it freezes the loop, stops each child the same way, then kills the loop, so the `kubectl`, `jq` or `sleep` the loop was waiting on goes with it. It signals a pid only while its parent is the calling shell, so a pid the watcher left and another process reused is never hit, and refuses the same pids `stop_pid` does: `kill -STOP 0` would freeze the whole driver and a later `kill -KILL 0` end it. Stage 3's and Stage 4b's watchers are stopped with it |
| `run_processes` | prints every pid whose working directory or command line names `$work` |

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

`new_issue TITLE [PARENT] [SPEC]` creates each issue a proof drives. A root carries the Dispatch
label `legion`, which hands it to the Go daemon (the daemon admits no root without it), and `SPEC`
as its primary document, `smoke_spec` when `SPEC` is omitted. A child carries neither, since it runs
under its root's tree.

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

Every wait for an issue to reach one phase is `wait_for_phase ISSUE PHASE [SECONDS]`: 600 s, unless
the phase's worker runs a whole loop (a correction round, the retro) and the caller passes its own
bound. `round_correction_pushed ROUND` accepts the round's line only as an addition in
`smoke_file`'s patch, never in a notes file or in a `.legion/` handoff that quotes it.

The handoff checks read the daemon's phase record (the `phases` table joined to `issues`), not the
ids of the facts it processed, so they hold whatever format a handoff event id takes. A role's
`handoff_commit` is the commit its last accepted completion reported, and the daemon empties it
when a transition starts that role on a new phase (`clearHandoff`,
`packages/daemon/internal/workflow/effects.go`); the implementer's `rounds` counts its returns
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

The caller sets `operator` (the admin kubectl context), `namespace`, `run_label` (the run's
`legion.dev/project` label, named apart from `lib/workflow.sh`'s `project`, the Dispatch project
key), and either `label_prefix` (the prefix every run label of the proof carries, Stage 4a's
`s4a-`) or `label_exact` (the one fixed label the proof owns while it holds its run lock, Stage
4b's `legsmoke`), `record` (a file with one Sandbox name per line), `work`, `evidence`, and `torn_down` and
`compared` empty; it defines `begin`, `note`, `pass` and `fail`, which exits.

| function | does |
| :--- | :--- |
| `op ARGS…` | `kubectl --context $operator -n $namespace ARGS…`, bounded at 300 s (`timeout --foreground`, so a Ctrl-C still reaches kubectl) |
| `snapshot FILE` | writes the namespace's Sandboxes, Secrets, PVCs, pods and ConfigMaps (`kinds`) that carry the run's project label or none, sorted |
| `teardown` | runs once and never fails. It refuses a label without `label_prefix`, or other than `label_exact`, so a mistyped label cannot select another run's objects; deletes every recorded Sandbox by name, then the Sandboxes and ConfigMaps labelled with that exact project; then lists the project's objects every 2 s, up to 150 listings, until none is left. Secrets and PVCs the Sandboxes' own deletion has not taken by the 90th listing are deleted by that exact label once, on the first listing from then on that answers; three failed listings in a row end the wait, naming the context and its error |
| `namespace_clean` | the check `namespace-clean`: a fresh snapshot, written to `$evidence/namespace-after.txt`, must equal `$evidence/namespace-before.txt` |

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
