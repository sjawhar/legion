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
| `daemon-standin.ts` | Serves the worker's claim routes (`claims/register`, `claims/ready`), the grant routes and a phase completion (`GET /legion/v1/state`, `handoff/complete`) with the Go daemon's request and response shapes (`@legion/contracts/legion-go-api`) and the real grant rule: a grant lives 60 seconds and redeems any number of times while it lives; an unknown or expired id answers 403 `Invalid or expired grant`. Appends one JSON line per request to its log. |
| `setup.sh` | Creates the throwaway profile and the scratch state directory (below), in one of two plugin modes. |
| `run.ts` | `prompt` prints the worker's instructions; `drive` runs the headless leg over Oh My Pi's RPC mode; `tui` runs the terminal leg in a private tmux server; `analyze` scores any transcript. Both legs end with the same table. |

## Production-profile mode

`RIG_PLUGINS=production` makes the rig profile load the real `legion` profile's whole plugin tree:
`setup.sh` copies `~/.omp/profiles/legion/plugins` (about 240 MB: `package.json`, `bun.lock`,
`node_modules`) into `~/.omp/profiles/l12rig/plugins`, removes the profile's `agent/extensions`
so exactly one copy of each extension loads, and then swaps only the Legion plugin inside the
copy, for a tarball extracted in the installed package's place, so the rig runs exactly what
`npm pack` ships (`dist/` with the bundles and `dist/skills`, `agents/`, `package.json`):

- `RIG_LEGION_BUILD=branch` (default): the checkout's plugin, packed by
  `scripts/e2e/lib/pack-plugin.sh` as the release packs it — the same pack step the stage proofs'
  `install-plugin-profile.sh` uses;
- `RIG_LEGION_BUILD=<version>` (e.g. `1.17.1`): `npm pack @sjawhar/pi-legion-envoy@<version>`, the
  released tarball, exactly what `bun add` would install.

The rest of the copied tree stays, `secretsd` and the other plugins included. The Legion package
needs none of it: its bundles inline every dependency except the `@oh-my-pi/*` packages Oh My Pi
itself provides.

Why: Sami's ruling (2026-09-13, AGENTC-79) after pi-envoy 1.17.1 shipped broken — proven on a rig
that loaded the Legion extension alone, it failed on the first real worker because `secretsd`
(`github:sjawhar/forward#v3.0.2`) replaces the bash tool with Oh My Pi's legacy `{command,
timeout}` shim, which discards the tool call's `env`. Pre-merge proof runs against the production
plugin tree, `secretsd` included, and nothing in this rig works around that plugin: the worker
runs under the daemon's real launch prefix (`secrets GEMINI_API_KEY OPENAI_API_KEY -- <omp>`), and
`--no-secrets` is only for a shell that already exports the keys.

The copied tree is reused by later production runs; `RIG_REFRESH_PLUGINS=1` re-copies it. Without
`RIG_PLUGINS` the rig is extension-only: no `plugins/` directory (a stale copy is removed), the two
extensions symlinked from the checkout under test. The two modes never mix.

`setup.sh` never reads or writes `~/.omp/profiles/legion` beyond copying `plugins/`, `config.yml`,
and `models.yml`; only the operator edits that profile. Its first action after resolving the
profile names is a refusal (exit 2, nothing touched) when the rig profile and the source profile
resolve to the same directory — `RIG_PROFILE=legion`, or `RIG_SOURCE_PROFILE=l12rig`, would
otherwise `rm -rf` the very tree it copies from.

## Before runs

The regression evidence for LEGION-12 (1.17.0's text delivery) and LEGION-52 (1.17.1's `env`
delivery under `secretsd`) was recorded against the TypeScript `legion` of its time, which read the
plain `LEGION_GRANT`. The rig now builds the checkout's Go `legion`, which reads the pane's
`LEGION_GRANT_FILE` first and never falls back to the variable while that pointer is set, so a
released 1.17.x build (`RIG_LEGION_BUILD=1.17.1`) fails every `legion …` probe here (D) for that
reason and does not reproduce those failures.

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
| `legion` | The checkout's Go `legion`, built by `setup.sh` from `packages/daemon-go/cmd/legion`: the daemon's own binary, which `state/bin/legion` execs. |
| `state/bin/legion` | The launcher the daemon installs at boot (`workerbin.Install`): execs `$RIG/legion`. |
| `state/bin/record-grant` | Appends `<grant file contents> <mode> <LEGION_GRANT or ->` to `$RIG/seen-grants.log`, so the prompt never names the credential; the third field is what a 1.17.0 (text-delivery) command ran under. |
| `state/worker-bin/gh` | The `gh` shim the daemon installs at boot (`workerbin.InstallGh`), written by `setup.sh` the same way: drops its own directory from PATH and execs `legion gh`. |
| `state/gh` | The `GH_CONFIG_DIR` `legion gh` gives the gh it runs. |
| `ws` | The worker's workspace, an empty jj repository (the boot handshake sets a jj identity on it). |
| `rig-mode.json` | What `setup.sh` laid out: plugin mode, Legion build, checkout commit; copied into each run's `report.json`. |
| `standin.log` | The stand-in daemon's request log. |
| `seen-grants.log` | One line per `record-grant` call. |
| `runs/<label>-<time>/` | Per run: `prompt.txt`, `events.jsonl` (headless) or `pane.txt` (terminal), `stderr.log`, `table.txt`, `report.json`. |

## Environment the worker gets

`run.ts` builds it (`workerEnvironment`) from the current shell after removing `ANTHROPIC_API_KEY`,
every `LEGION_*` and `DISPATCH_*` value, `GH_CONFIG_DIR`, `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST`
(no Legion pane carries them), and every inherited `worker-bin` PATH entry (a rig started from a
Legion pane carries that pane's worker-bin first), then adds what the daemon's tmux runtime sets
for a phase-worker pane (`panePairs`, `packages/daemon-go/internal/runtime/tmux/spawn.go`), which is
the pane's for life, never per command:

| variable | value |
| --- | --- |
| `OMP_PROFILE`, `PI_PROFILE` | `l12rig` |
| `PI_CODING_AGENT_DIR` | `~/.omp/profiles/l12rig/agent` |
| `PI_NOTIFICATIONS`, `PI_NO_TITLE` | `off`, `1` |
| `LEGION_TREE`, `LEGION_ISSUE`, `LEGION_ROLE` | `RIG-1`, `RIG-1`, `implementer` |
| `LEGION_GENERATION`, `LEGION_PROJECT` | `1`, `l12rig` |
| `LEGION_BOOT_TOKEN_FILE` | `$RIG/state/secrets/boot` |
| `LEGION_DAEMON_URL` | `http://127.0.0.1:<port>` (the stand-in) |
| `LEGION_STATE_DIR`, `LEGION_WORKSPACE` | `$RIG/state`, `$RIG/ws` |
| `LEGION_GH_PATH`, `LEGION_GIT_PATH`, `LEGION_JJ_PATH` | the first `gh`, `git` and `jj` on the inherited PATH, as the daemon resolves them at boot |
| `PI_SHELL_PREFIX` | `PATH='$RIG/state/worker-bin:$RIG/state/bin:'${PATH#'…'} &&` (`shellprefix.For`) |
| `GIT_TERMINAL_PROMPT` | `0` |
| `LEGION_GRANT_FILE` | `$RIG/state/secrets/legion-l12rig-rig-1-implementer-grant` (`runtime.GrantFile`) |
| `PATH` | `$RIG/state/worker-bin`, then `$RIG/state/bin`, then the inherited PATH |

The launch argv is `secrets GEMINI_API_KEY OPENAI_API_KEY -- <omp>`, plus `--mode rpc` for the
headless leg (`--no-secrets` drops the prefix when the keys are already in the environment). No
Anthropic key is injected: omp's anthropic provider gets its gateway token from `!hawk-token` in
the profile's `models.yml`, and a set `ANTHROPIC_API_KEY` would bypass it.

## Which Oh My Pi binary

Neither script picks a binary: `run.ts` runs whatever `--omp` names. Point it at the build the
live Legion daemon's panes run, because that is the host whose write-back behaviour the rig is
measuring. The daemon resolves that build from `omp_invocation` in its `legion.yaml`
(`mise x github:sjawhar/oh-my-pi@<version> -- omp`), so read the version from there — on this
box `~/.config/legion/sjawhar-legion/legion.yaml` — and resolve it with `mise where`. The
repository's own pin (the root `.omp-pin`, a mise tool spec) is the build the worker image and the
live proofs install; use it only when that is the build you are comparing against. Do not
hard-code a build in a run book; the one the rig proved against is recorded in each run's
`report.json` (`omp`), beside the plugin mode and Legion build (`rigMode`).

## Running it

```sh
SRC=/path/to/checkout           # bun install --frozen-lockfile must have run here; go on PATH
# The build the live daemon runs: the `omp_invocation` line of its legion.yaml names it.
OMP_TOOL=$(sed -n 's/^omp_invocation: *"mise x \([^ ]*\) -- omp".*/\1/p' ~/.config/legion/sjawhar-legion/legion.yaml)
OMP=$(mise where "$OMP_TOOL")/bin/omp
RIG=/tmp/l54rig; mkdir -p $RIG
PORT=13399

# the branch's plugin, profile and Go legion
RIG_PLUGINS=production RIG_LEGION_BUILD=branch sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh $SRC $RIG

# stand-in daemon (keep it running across the runs below)
bun $SRC/packages/pi-envoy/scripts/grant-rig/daemon-standin.ts $PORT $RIG/standin.log $RIG/state/secrets/boot &

# headless leg, 33 bash calls, 3 task spawns (every legion probe on call 30 or later)
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts drive --rig $RIG --port $PORT --omp $OMP --label fixed-rpc

# terminal leg, 8 bash calls, 1 task spawn, driven through tmux -L l12rig
: > $RIG/standin.log; : > $RIG/seen-grants.log
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts tui --rig $RIG --port $PORT --omp $OMP --short --label fixed-tui

# negative control (outside the worker): a fabricated credential in the file must 403
printf '00000000-0000-4000-8000-000000000000' > $RIG/fake-grant && chmod 600 $RIG/fake-grant
printf 'protocol=https\nhost=github.com\n' | LEGION_GRANT_FILE=$RIG/fake-grant \
  LEGION_DAEMON_URL=http://127.0.0.1:$PORT $RIG/state/bin/legion credential get
# -> Unable to redeem LEGION_GRANT (403)
```

`--prompt-file <path>` on `drive` or `tui` replaces the built-in steps with the file's text (the LEGION-45 operation-log guard is proven this way: a prompt that asks the worker to run `jj -R "$LEGION_WORKSPACE" undo`, an `eval` and a `hub` start doing the same, and `jj op log` before and after); the A–G grant verdicts in the table then describe whatever bash calls the prompt caused and are not that run's evidence.

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
oracle and is exempt); **G** the environment probe shows
`GH_CONFIG_DIR= GH_TOKEN= GITHUB_TOKEN= GH_HOST=` (none set on the pane) and a `PATH=` beginning
with `$RIG/state/worker-bin` in which worker-bin occurs once.

Expected: `H=1/1 G=1 T=0 file=own-mint` on every record-grant call, A–G all PASS, every
redemption 200, `legion …` commands `exit=0`, one parent instance plus one per `task` spawn. The
prompt never asks for `legion handoff complete`: the extension refuses it in a phase worker's
shell, where the `legion` tool's `handoff_complete` ends the phase.

## Cleanup

```sh
tmux -L l12rig kill-server 2>/dev/null; rm -rf ~/.omp/profiles/l12rig "$RIG"
```
