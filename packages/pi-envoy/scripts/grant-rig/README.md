# Grant rig

Proves, on a real Oh My Pi phase worker, how the one-time credential (`LEGION_GRANT`) reaches
each shell command. The Legion extension mints one grant from the daemon per bash tool call and
must deliver it through the bash tool's per-command `env`, never as text in the command — text
is written back into the model's message and imitated (LEGION-12). The rig runs a worker against
a stand-in daemon for 30+ commands, some after `task` spawns, and checks every command.

Nothing here touches the real daemon, the real Envoy roles of any live issue, or the `legion`
Oh My Pi profile. The worker claims the role `legion-l12rig-rig-1-implementer` on the real Envoy
listener (inherited `ENVOY_URL`), which collides with nothing.

## Pieces

| file | what it is |
| --- | --- |
| `daemon-standin.ts` | Serves the worker boot handshake and the grant routes with the real request and response shapes (`LegionDaemonApi`) and the real grant rule: a grant lives 60 seconds and redeems any number of times while it lives; an unknown or expired id answers 403 `Invalid or expired grant`. Appends one JSON line per request to its log. |
| `setup.sh` | Creates the throwaway profile and the scratch state directory (below). |
| `run.ts` | `prompt` prints the worker's instructions; `drive` runs the headless leg over Oh My Pi's RPC mode; `tui` runs the terminal leg in a private tmux server; `analyze` scores any transcript. Both legs end with the same table. |

## Layout `setup.sh <checkout> [rig dir]` creates

Profile `~/.omp/profiles/l12rig/agent/`:

- `config.yml` and `models.yml` copied from the `legion` profile (production model roles,
  `task.isolation.enabled: true`, memory off, bash runs unprompted).
- `extensions/envoy.ts` and `extensions/legion.ts` symlinked to the checkout under test (the
  developer install from the package README). No `plugins/` directory, so exactly one copy of
  each extension loads.

Scratch directory `$RIG` (default `mktemp -d /tmp/l12rig.XXXX`), standing in for the daemon's
`state_dir`:

| path | purpose |
| --- | --- |
| `state/secrets/boot` | The worker's boot token (`rig-boot`, mode 0600); the stand-in rejects any other. |
| `state/bin/legion` | The checkout's own `legion` command-line tool (`bun packages/daemon/src/cli/index.ts`), the way `<state_dir>/bin/legion` re-execs the daemon's runtime in production. |
| `state/bin/record-grant` | Appends the grant the calling command actually ran under to `$RIG/seen-grants.log`, so the prompt never names the variable. |
| `state/worker-bin/gh` | Written by the extension itself on the first bash call: the `gh` shim that routes through `legion gh`. |
| `ws` | The worker's workspace, an empty jj repository (the boot handshake sets a jj identity on it). |
| `standin.log` | The stand-in daemon's request log. |
| `seen-grants.log` | One line per `record-grant` call. |
| `runs/<label>-<time>/` | Per run: `prompt.txt`, `events.jsonl` (headless) or `pane.txt` (terminal), `stderr.log`, `table.txt`, `report.json`. |

## Environment the worker gets

`run.ts` builds it (`workerEnvironment`) from the current shell minus every `LEGION_*` and
`DISPATCH_*` value, then sets what the daemon sets for a phase-worker pane:

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
| `PATH` | `$RIG/state/bin` first |

The launch argv is the daemon's own prefix, `secrets ANTHROPIC_API_KEY GEMINI_API_KEY
OPENAI_API_KEY -- <omp>`, plus `--mode rpc` for the headless leg (`--no-secrets` drops the
prefix when the keys are already in the environment).

## Running it

```sh
SRC=/path/to/checkout           # bun install --frozen-lockfile must have run here
OMP=$(mise where github:sjawhar/oh-my-pi@18.1.18-sami.20260912-104423)/bin/omp
eval "$(sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh "$SRC")"   # prints RIG=…
PORT=13399
bun $SRC/packages/pi-envoy/scripts/grant-rig/daemon-standin.ts $PORT $RIG/standin.log $RIG/state/secrets/boot &

# headless leg: 31 bash calls, 3 task spawns
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts drive --rig $RIG --port $PORT --omp $OMP --label fixed-rpc

# terminal leg: 8 bash calls, 1 task spawn, driven through tmux -L l12rig
: > $RIG/standin.log; : > $RIG/seen-grants.log
bun $SRC/packages/pi-envoy/scripts/grant-rig/run.ts tui --rig $RIG --port $PORT --omp $OMP --short --label fixed-tui

# negative control (outside the worker): a grant the stand-in never minted must 403
printf 'protocol=https\nhost=github.com\n' | LEGION_GRANT=00000000-0000-4000-8000-000000000000 \
  LEGION_DAEMON_URL=http://127.0.0.1:$PORT $RIG/state/bin/legion credential get
# -> Unable to redeem LEGION_GRANT (403)
```

Clear `standin.log` and `seen-grants.log` between runs; the analyzer pairs bash calls with grant
mints in order. To score a transcript by hand (either leg):

```sh
bun run.ts analyze --rig $RIG --transcript <session .jsonl> --standin-log $RIG/standin.log \
  --omp-log ~/.omp/profiles/l12rig/logs/omp.<date>.<pid>.log
```

Reproducing on `main` without a second jj workspace: export its tree read-only and point the
profile symlinks at it (`setup.sh $RIG/main $RIG` re-links and reuses the scratch directory):

```sh
git --git-dir="$(jj -R "$SRC" git root)" archive "$(jj -R "$SRC" log -r main@origin --no-graph -T commit_id)" | tar -x -C $RIG/main
(cd $RIG/main && bun install --frozen-lockfile)
sh $SRC/packages/pi-envoy/scripts/grant-rig/setup.sh $RIG/main $RIG
```

## What the table means

Per bash call: `H` hook log lines for that tool call id and how many distinct extension
instances wrote them; `G` grant mints attributed to the call (1 when mints equal calls); `T`
credential lines in the model-visible command text; `env` whether `arguments.env.LEGION_GRANT`
equals the grant minted for that call; `X` classifies every id found in the text (`hook` for the
unfixed hook's own first block, `own-mint`, `earlier-mint`, `copy` of a previous call's id,
`non-v4`, `unminted`).

Expected on the fixed code: `H=1/1 G=1 T=0 env=own-mint` on every call, every redemption 200,
every `record-grant` line equal to its call's mint, `legion …` commands `exit=0`, one parent
instance plus one per `task` spawn. Expected on the unfixed code: `H=1` and `G=1` still (the hook
never multiplies), `T>=1` on every call and `T>1` on some — the extras are model text — and a
403 on any `legion …` command whose imitated line came last.

## Cleanup

```sh
tmux -L l12rig kill-server 2>/dev/null; rm -rf ~/.omp/profiles/l12rig "$RIG"
```
