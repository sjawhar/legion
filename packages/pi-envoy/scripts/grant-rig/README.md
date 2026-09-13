# Grant rig

Proves, on a real Oh My Pi phase worker, how the one-time credential reaches each shell command.
The Legion extension mints one grant from the daemon per bash tool call and writes it to the 0600
file the pane's `LEGION_GRANT_FILE` names (atomic rename, under `<state_dir>/secrets/`), and the
`legion` command-line tool reads that file first. The grant travels through neither the command
text — text is written back into the model's message and imitated (LEGION-12) — nor the bash
tool's `env` argument — a plugin that replaces the bash tool drops it (the `secretsd` plugin's
legacy shim, LEGION-52). The rig runs a worker against a stand-in daemon for 30+ commands, some
after `task` spawns, and checks every command.

Nothing here touches the real daemon, the real Envoy roles of any live issue, or the `legion`
Oh My Pi profile. The worker claims the role `legion-l12rig-rig-1-implementer` on the real Envoy
listener (inherited `ENVOY_URL`), which collides with nothing.

## Pieces

| file | what it is |
| --- | --- |
| `daemon-standin.ts` | Serves the worker boot handshake and the grant routes with the real request and response shapes (`LegionDaemonApi`) and the real grant rule: a grant lives 60 seconds and redeems any number of times while it lives; an unknown or expired id answers 403 `Invalid or expired grant`. Appends one JSON line per request to its log. |
| `setup.sh` | Creates the throwaway profile and the scratch state directory (below), in one of two plugin modes. |
| `run.ts` | `prompt` prints the worker's instructions; `drive` runs the headless leg over Oh My Pi's RPC mode; `tui` runs the terminal leg in a private tmux server; `analyze` scores any transcript. Both legs end with the same table. |

## Production-profile mode

`RIG_PLUGINS=production` makes the rig profile load the real `legion` profile's whole plugin tree:
`setup.sh` copies `~/.omp/profiles/legion/plugins` (about 240 MB: `package.json`, `bun.lock`,
`node_modules`) into `~/.omp/profiles/l12rig/plugins`, removes the profile's `agent/extensions`
so exactly one copy of each extension loads, and then swaps only the Legion plugin inside the
copy:

- `RIG_LEGION_BUILD=branch` (default): `bun run build` in the checkout's `packages/pi-envoy`, its
  `dist/envoy.js` and `dist/legion.js` copied over the installed package's `dist/` (the installed
  `package.json` stays — its `omp.extensions` already names those two files);
- `RIG_LEGION_BUILD=<version>` (e.g. `1.17.1`): `npm pack @sjawhar/pi-legion-envoy@<version>`
  extracted in place of the installed package, exactly what `bun add` would install.

Why: Sami's ruling (2026-09-13, AGENTC-79) after pi-envoy 1.17.1 shipped broken — proven on a rig
that loaded the Legion extension alone, it failed on the first real worker because `secretsd`
(`github:sjawhar/forward#v3.0.2`) replaces the bash tool with Oh My Pi's legacy `{command,
timeout}` shim, which discards the tool call's `env`. Pre-merge proof runs against the production
plugin tree, `secretsd` included, and nothing in this rig works around that plugin: the worker
runs under the daemon's real launch prefix (`secrets ANTHROPIC_API_KEY GEMINI_API_KEY
OPENAI_API_KEY -- <omp>`), and `--no-secrets` is only for a shell that already exports the keys.

The copied tree is reused by later production runs; `RIG_REFRESH_PLUGINS=1` re-copies it. Without
`RIG_PLUGINS` the rig is extension-only: no `plugins/` directory (a stale copy is removed), the two
extensions symlinked from the checkout under test. The two modes never mix.

`setup.sh` never reads or writes `~/.omp/profiles/legion` beyond copying `plugins/`, `config.yml`,
and `models.yml`; only the operator edits that profile.

## Before runs

The regression evidence is two runs of the same production-profile rig with a released Legion
plugin, both against `main`'s `legion` command-line tool (export `main`'s tree read-only as below
and point `setup.sh` at it — do not use the branch CLI for these, or 1.17.1 would not reproduce):

- `RIG_LEGION_BUILD=1.17.1`: env delivery under `secretsd`. Every `legion …` probe fails
  `LEGION_GRANT is missing`; D fails, B's mints still one per call, zero redemption lines.
- `RIG_LEGION_BUILD=1.17.0`: text delivery. `T>=1` on every call and `T>1` on some (the model's
  copies, classified in `X`), a 403 on a later `legion …` probe, F fails (the ids are in the
  transcript).

## Layout `setup.sh <checkout> [rig dir]` creates

Profile `~/.omp/profiles/l12rig/`:

- `agent/config.yml` and `agent/models.yml` copied from the `legion` profile (production model
  roles, `task.isolation.enabled: true`, memory off, bash runs unprompted).
- extension-only: `agent/extensions/{envoy,legion}.ts` symlinked to the checkout under test, no
  `plugins/`. Production: `plugins/` copied from the `legion` profile with the Legion plugin
  swapped, no `agent/extensions/`.

Scratch directory `$RIG` (default `mktemp -d /tmp/l12rig.XXXX`), standing in for the daemon's
`state_dir`:

| path | purpose |
| --- | --- |
| `state/secrets/boot` | The worker's boot token (`rig-boot`, mode 0600); the stand-in rejects any other. |
| `state/secrets/legion-l12rig-rig-1-implementer-grant` | The worker's `LEGION_GRANT_FILE`, written by the extension before each bash command (mode 0600). Never read, printed, or copied by hand. |
| `state/bin/legion` | The checkout's own `legion` command-line tool (`bun packages/daemon/src/cli/index.ts`), the way `<state_dir>/bin/legion` re-execs the daemon's runtime in production. |
| `state/bin/record-grant` | Appends `<grant file contents> <mode> <LEGION_GRANT or ->` to `$RIG/seen-grants.log`, so the prompt never names the credential; the third field is what a 1.17.0 (text-delivery) command ran under. |
| `state/worker-bin/gh` | Installed by `setup.sh` through the daemon's `worker-bin.ts`, as the daemon does at startup: the `gh` shim that routes through `legion gh`. A `main` checkout without that module gets none here; its 1.17.x extensions wrote the shim themselves. |
| `state/gh` | The pane's `GH_CONFIG_DIR`. |
| `ws` | The worker's workspace, an empty jj repository (the boot handshake sets a jj identity on it). |
| `rig-mode.json` | What `setup.sh` laid out: plugin mode, Legion build, checkout commit; copied into each run's `report.json`. |
| `standin.log` | The stand-in daemon's request log. |
| `seen-grants.log` | One line per `record-grant` call. |
| `runs/<label>-<time>/` | Per run: `prompt.txt`, `events.jsonl` (headless) or `pane.txt` (terminal), `stderr.log`, `table.txt`, `report.json`. |

## Environment the worker gets

`run.ts` builds it (`workerEnvironment`) from the current shell minus every `LEGION_*` and
`DISPATCH_*` value (and any inherited `worker-bin` PATH entry), then sets what the daemon sets
for a phase-worker pane — the launch keys and the static credential environment
(`ProcessManager.credentialProcessEnvironment`), which is the pane's for life, never per command:

| variable | value |
| --- | --- |
| `OMP_PROFILE`, `PI_PROFILE` | `l12rig` |
| `PI_CODING_AGENT_DIR` | `~/.omp/profiles/l12rig/agent` |
| `PI_NOTIFICATIONS`, `PI_NO_TITLE` | `off`, `1` |
| `LEGION_ROLE`, `LEGION_TREE`, `LEGION_ISSUE` | `implementer`, `RIG-1`, `RIG-1` |
| `LEGION_GENERATION`, `LEGION_PROJECT` | `1`, `l12rig` |
| `LEGION_BOOT_TOKEN_FILE` | `$RIG/state/secrets/boot` |
| `LEGION_DAEMON_URL` | `http://127.0.0.1:<port>` (the stand-in) |
| `LEGION_STATE_DIR`, `LEGION_WORKSPACE` | `$RIG/state`, `$RIG/ws` |
| `LEGION_GRANT_FILE` | `$RIG/state/secrets/legion-l12rig-rig-1-implementer-grant` |
| `GH_CONFIG_DIR` | `$RIG/state/gh` |
| `GH_TOKEN`, `GITHUB_TOKEN`, `GH_HOST` | empty |
| `PATH` | `$RIG/state/worker-bin`, then `$RIG/state/bin`, then the inherited PATH |

The launch argv is the daemon's own prefix, `secrets ANTHROPIC_API_KEY GEMINI_API_KEY
OPENAI_API_KEY -- <omp>`, plus `--mode rpc` for the headless leg (`--no-secrets` drops the
prefix when the keys are already in the environment).

## Which Oh My Pi binary

Neither script picks a binary: `run.ts` runs whatever `--omp` names. Point it at the build the
live Legion daemon's panes run, because that is the host whose write-back behaviour the rig is
measuring. The daemon resolves that build from `omp_invocation` in its `legion.yaml`
(`mise x github:sjawhar/oh-my-pi@<version> -- omp`), so read the version from there — on this
box `~/.config/legion/sjawhar-legion/legion.yaml` — and resolve it with `mise where`. The
repository's own pin (`OMP_FORK_PIN` in `packages/daemon/src/daemon/omp-pin.ts`, printed by
`bun packages/daemon/src/daemon/omp-pin.ts`) is the default a daemon falls back to when
`legion.yaml` sets no `omp_invocation`; use it only when that is the daemon you are comparing
against. Do not hard-code a build in a run book; the one the rig proved against is recorded in
each run's `report.json` (`omp`), beside the plugin mode and Legion build (`rigMode`).

## Running it

```sh
SRC=/path/to/checkout           # bun install --frozen-lockfile must have run here
# The build the live daemon runs: the `omp_invocation` line of its legion.yaml names it.
OMP_TOOL=$(sed -n 's/^omp_invocation: *"mise x \([^ ]*\) -- omp".*/\1/p' ~/.config/legion/sjawhar-legion/legion.yaml)
OMP=$(mise where "$OMP_TOOL")/bin/omp
RIG=/tmp/l54rig; mkdir -p $RIG
PORT=13399

# main's tree, read-only, for the before runs (its CLI ignores LEGION_GRANT_FILE)
mkdir -p $RIG/main
git --git-dir="$(jj -R "$SRC" git root)" archive "$(jj -R "$SRC" log -r main@origin --no-graph -T commit_id)" | tar -x -C $RIG/main
(cd $RIG/main && bun install --frozen-lockfile)

# stand-in daemon (keep it running across the runs below)
bun $SRC/packages/pi-envoy/scripts/grant-rig/daemon-standin.ts $PORT $RIG/standin.log $RIG/state/secrets/boot &

# before run 1: released 1.17.1 (env delivery) — expect `LEGION_GRANT is missing` on every legion probe
RIG_PLUGINS=production RIG_LEGION_BUILD=1.17.1 sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh $RIG/main $RIG
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts drive --rig $RIG --port $PORT --omp $OMP --label before-1.17.1

# before run 2: released 1.17.0 (text delivery) — expect imitation counts growing
: > $RIG/standin.log; : > $RIG/seen-grants.log
RIG_PLUGINS=production RIG_LEGION_BUILD=1.17.0 sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh $RIG/main $RIG
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts drive --rig $RIG --port $PORT --omp $OMP --label before-1.17.0

# the branch: headless leg, 34 bash calls, 3 task spawns (every legion probe on call 30 or later)
: > $RIG/standin.log; : > $RIG/seen-grants.log
RIG_PLUGINS=production RIG_LEGION_BUILD=branch sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh $SRC $RIG
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts drive --rig $RIG --port $PORT --omp $OMP --label fixed-rpc

# the branch: terminal leg, 9 bash calls, 1 task spawn, driven through tmux -L l12rig
: > $RIG/standin.log; : > $RIG/seen-grants.log
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts tui --rig $RIG --port $PORT --omp $OMP --short --label fixed-tui

# negative control (outside the worker): a fabricated credential in the file must 403
printf '00000000-0000-4000-8000-000000000000' > $RIG/fake-grant && chmod 600 $RIG/fake-grant
printf 'protocol=https\nhost=github.com\n' | LEGION_GRANT_FILE=$RIG/fake-grant \
  LEGION_DAEMON_URL=http://127.0.0.1:$PORT $RIG/state/bin/legion credential get
# -> Unable to redeem LEGION_GRANT (403)
```

Clear `standin.log` and `seen-grants.log` between runs; the analyzer pairs bash calls with grant
mints in order. To score a transcript by hand (either leg):

```sh
bun run.ts analyze --rig $RIG --transcript <session .jsonl> --standin-log $RIG/standin.log \
  --omp-log ~/.omp/profiles/l12rig/logs/omp.<date>.<pid>.log
```

## What the table means

Per bash call: `H` hook log lines for that tool call id and how many distinct extension
instances wrote them; `G` grant mints attributed to the call (1 when mints equal calls); `T`
credential lines in the model-visible command text; `file` (record-grant calls) whether the grant
file the command read held the grant minted for that call at mode 0600 (`own-mint`), else the
classification of what it held and its mode, or `-` when no file was there; `env` the keys the
model put in `arguments.env`, if any (informational — nothing reads them); `X` classifies every id
found in the text (`hook` for the 1.17.0 hook's own first block, `own-mint`, `earlier-mint`,
`copy` of a previous call's id, `non-v4`, `unminted`).

Verdicts: **A** the command text never mentions `LEGION_GRANT`, and no call needed an `env`
argument (every record-grant ran under the file's grant and no env carried the variable);
**B** one mint per bash call, every redemption 200; **C** every record-grant ran under its own
mint from a 0600 file; **D** the `legion …` probes print `exit=0` and never `Unable to redeem`;
**E** exactly one hook line per bash call, one parent instance (plus one instance per `task`
spawn); **F** no minted id appears in the transcript or the OMP log (the stand-in log is the
oracle and is exempt); **G** the `printenv` probe shows `GH_CONFIG_DIR=$RIG/state/gh`,
`GH_TOKEN= GITHUB_TOKEN= GH_HOST=`, and a `PATH=` beginning with `$RIG/state/worker-bin` in which
worker-bin occurs once.

Expected on the fixed code: `H=1/1 G=1 T=0 file=own-mint` on every record-grant call, A–G all
PASS, every redemption 200, `legion …` commands `exit=0`, one parent instance plus one per `task`
spawn. Expected on 1.17.1: D FAIL (`LEGION_GRANT is missing`), C FAIL (`file=-`), no redemption
lines. Expected on 1.17.0: `T>=1` on every call and `T>1` on some — the extras are model text —
A and F FAIL, and a 403 on any `legion …` command whose imitated line came last.

## Cleanup

```sh
tmux -L l12rig kill-server 2>/dev/null; rm -rf ~/.omp/profiles/l12rig "$RIG"
```
