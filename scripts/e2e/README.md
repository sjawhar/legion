# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these; `lib/` holds what
the stage scripts share.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state`, registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
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
- a free daemon port picked per run (20000–39999, checked with `ss`) and, on the devbox path, the
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

The `daemon-go` job in `.github/workflows/envoy-and-contracts.yaml` runs `go vet ./...` and
`go test ./...` in `packages/daemon-go` against its `postgres:16` service (`LEGION_TEST_PG_DSN`),
installs `tmux` (the daemon refuses to start without it), then runs this script with
`LEGION_E2E_PG_DSN` pointing at the same service — so the script runs no docker of its own there.
The job is gated on the workflow's `changes` filter (`daemon_go`: `packages/daemon-go/**`,
`go.work`, `scripts/e2e/**`).

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
   `packages/daemon/docker/worker.Dockerfile:59-60`);
3. `bun pm pack`, whose `prepack` builds `dist/` (`release.yaml:350-353`, `packages/pi-envoy/scripts/prepack.sh`);
4. copy the saved manifest back and check it byte for byte (`release.yaml:365-370`);
5. unpack the tarball into `<dir>` (`worker.Dockerfile:55-57, :62-63`);
6. `OMP_PROFILE=<name> omp plugin install <dir>` (`worker.Dockerfile:159`);
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
