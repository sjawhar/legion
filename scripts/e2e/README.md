# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these; `lib/` holds what
the stage scripts share.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state`, registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
| `stage2-tmux-supervision.sh` | the Go daemon supervises real Oh My Pi sessions — the pinned build with this checkout's plugin in an isolated profile — in its private tmux server, against a real Envoy listener and NATS: the plugin gate refuses another contract and a disabled plugin; an agent registers, holds its Envoy role and is ready; a task queued before ready runs once and a retried frame starts no second turn; a killed pane resumes the same session; suspend and resume keep it; a stale hello is refused; an agent that never registers is retired at the deadline and counted; a restart re-adopts every live pane; an orphan is reaped after the grace; the OMP process's environment is the isolated one. Devbox only |
| `verifiers-staging-token.sh` | `dispatch` and the Envoy listener authenticate a projected service-account token the staging EKS cluster actually minted — the right audience is accepted, the other binary's audience and a missing bearer are refused, each shared token still works, half an OIDC pair and an issuer that does not answer refuse the boot, and a refused token leaves its failure class in the log and nowhere else |

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
- a free daemon port picked per run by `lib/free-port.sh`: below the kernel's ephemeral range
  (20000 up to `ip_local_port_range`'s first port, 32767 on a default kernel), because every
  outbound socket the run opens before the daemon binds is autobound from that range and can take
  a port inside it, and checked with `ss`. On the devbox path the run also has the
  container `legion-e2e-pg-<pid>`.

After any exit — pass, failure, or an interrupt — the `EXIT` trap removes the container and
signals the daemon; `docker ps -a --filter name=legion-e2e-pg` comes back empty and no daemon is
left holding the run's port.

### How it fails

Every step is fatal and names its reason: the refusal that does not name its host, a `/healthz`
that never answers, a state document that is not boot 1 with cap 4 and no issues, a registry
without exactly one entry for this run, a `firstBootAt` that moved across the restart, a daemon
that ignored a stop (SIGKILLed on the way out, so nothing holds the port) or exited non-zero on
one. `stop_daemon` reads and judges the exit status, because `daemon.Run` returns 0 on a
cancelled context — a non-zero status there is a defect, not a stop.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`). The container
  entrypoint's bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and
  a daemon that connects in that window is reset by the peer.
- The "never the password" assertion is written `grep -q … && exit 1`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail
  the run.

### In CI

The `daemon-go` job in `.github/workflows/envoy-and-contracts.yaml` runs `go vet ./...` in
`packages/daemon-go`, installs `tmux` — the tmux runtime's tests drive a real tmux server and skip
without one, and the daemon refuses to start without it — then `go test ./...` against its
`postgres:16` service (`LEGION_TEST_PG_DSN`), then this script with `LEGION_E2E_PG_DSN` pointing at
the same service, so the script runs no docker of its own there. The job is gated on the
workflow's `changes` filter (`daemon_go`: `packages/daemon-go/**`, `go.work`, `scripts/e2e/**`,
`packages/pi-envoy/**` — the plugin's Go client and the manifest field the boot gate reads — and
`packages/contracts/fixtures/daemon-api/**`, the fixtures the Go goldens pin).

## stage2-tmux-supervision.sh

```sh
bash scripts/e2e/stage2-tmux-supervision.sh     # → "stage 2 e2e: PASS", exit 0, in about four minutes
```

**Devbox only.** It runs a real Oh My Pi that calls a real model, so it needs `go`, `docker`,
`jq`, `curl`, `ss`, `tmux`, `socat`, `bun`, `mise` (with the pinned OMP build it installs if
missing), and the `secrets` CLI holding `GEMINI_API_KEY_TESTS` (agent tier: no YubiKey touch). CI
runs the unit and integration tests, not this script; what only this run proves is OMP's real RPC
frames, `--resume`'s same-agent behaviour, the one-word `--append-system-prompt`, the plugin's
strict parse of the Go daemon's answers, the plugin gate against an installed manifest, the
provider-key path, and the Envoy role claim.

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
- **The provider key**: `provider_keys: {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}`. The daemon
  resolves the secret at boot and writes it as a daemon-held 0600 file; every pane's shim exports
  it to OMP alone. The run never reads the value; it checks the length in OMP's environment.
- **The daemon**: `legion.yaml` with a fresh project key per run (`S2E<pid><epoch>` — a retired
  claim is never spawned again, so a reused key would fail on a store that served an earlier run),
  a free port (as Stage 1 picks it; its second daemon and the listener get two more, distinct),
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
| `retried-frame-starts-no-second-turn` | stops the claim's shim (SIGSTOP) and delivers a task: every send goes unanswered, and the daemon sends the same delivery id again at the next sweep (`the prompt was lost to the transport` twice in its log); resumed, the shim hands OMP the first frame and answers the repeat from its record — the session file holds the task once, and no prompt failure is charged |
| `kill-pane-resumes-the-same-session` | `tmux kill-pane` on the architect: the next generation is ready in a new pane with the same session, its OMP started with `--resume=<session file>`, and the Envoy role still held |
| `suspend-keeps-the-session` | `legion claims suspend`: `suspended`, no locator, the pane gone, the session and its file kept |
| `resume-on-demand` | `legion claims resume`: `ready` at the next generation in a new pane, same session |
| `stale-generation-hello-refused` | writes a hello carrying generation 1's boot token to `<state_dir>/worker-stream.sock`: the daemon closes it with nothing written and logs `rejected hello (stale worker generation)` |
| `unregistered-agent-retired-at-the-deadline` | a second daemon whose `LEGION_OMP_PATH` stub answers the plugin gate and otherwise sleeps, with a 10 s registration deadline (5 s × 2): the claim's shim connects and its process lives, the agent never registers, and at the deadline the process is retired and one launch failure counted |
| `restart-readopts-the-live-panes` | SIGTERM, then start again: `boots` +1, both claims keep their generation, incarnation and pane, no pane opens or closes, and a task delivered after the restart runs once — over the connection the shim's reconnect hello opened |
| `omp-child-environment` | the boot log names the resolved pinned OMP binary for both probes and panes; the pane's process is `/bin/sh -c`; walking first children from it to `argv[0] == omp` finds that exact executable, never the OMP wrapper that remains first on the ordinary daemon PATH, whose `XDG_CONFIG_HOME` is under `<state_dir>/home`, which carries `GEMINI_API_KEY` (length only) that its shim does not, and no `GEMINI_API_KEY_TESTS`, `SOPS_AGE_KEY_FILE` or `SECRETSD_CONFIG` |
| `stray-pane-reaped-after-the-grace` | opens a window marked as the daemon's (`@legion_owner`) holding no recorded pane, and an unmarked one beside it: the periodic orphan sweep (every 60 s, 120 s grace) reaps the marked one no sooner than 120 s after it opened, and keeps the unmarked window and both claims' panes |
| `stop` | `legion claims stop` retires both claims; `legion stop` ends the daemon with exit 0 |

Every wait is bounded and names what it waited for; a failed assertion prints
`FAIL <check>: <why>` and exits 1, and any other failing command names the check it ended. The
`EXIT` trap — on a pass, a failure, or an interrupt — stops both daemons (SIGKILL after 10 s),
kills both private tmux servers, stops the listener, SIGKILLs any process still naming the work
directory in its command line or working directory, removes both containers and the OMP profile,
and removes the work directory when the run passed (keeping it, with `daemon.log`,
`deadline.log`, `listener.log` and each refusal's log, when it did not).

Three things the run had to learn about its surface:

- The shim is a Go process, which starts its child from whichever thread runs the goroutine: the
  first-child walk reads `/proc/<pid>/task/*/children`, never the main thread's list alone.
- OMP takes the session to resume as `--resume=<file>`, one argument.
- The shipped orphan sweep is the only reaper after boot (boot's own reconcile runs with grace 0),
  so the stray is opened after the restart and the check waits up to five minutes.

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
- the first free port at or above 14100 for `dispatch` and 14200 for the listener.

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
