# Pi Legion Extension

`@sjawhar/pi-legion` is the Oh My Pi plugin a Legion pane loads beside `@sjawhar/pi-envoy`
(`packages/pi-envoy`). It carries one extension entry, `extensions/legion.ts`: the Legion
lifecycle, which boots a root architect or a phase worker from the daemon's pane environment
(`LEGION_TREE`/`LEGION_ROLE`), registers the controller (`LEGION_CONTROLLER`), registers the
`legion` tool, whose operations mint their own daemon grants in-process, and holds the phase a
worker is in open until its `handoff_complete`. A session without that environment gets nothing from
it but the run records below, and those only where the deployment sets `RUN_RECORD_DIR`. The Envoy
messaging and Dispatch tools every session uses are `@sjawhar/pi-envoy`'s, and this
plugin reaches them through the in-process interface that package publishes
(`packages/pi-shared/AGENTS.md`): in a Legion session the entry refuses to run, naming the remedy,
when no `@sjawhar/pi-envoy` is loaded, when the loaded one speaks another interface version, or
when the pre-split package is still installed beside it (`AGENTS.md`, "What this plugin needs from
pi-envoy").

## Installing both into the daemon's profile

The Legion daemon's boot gate (`packages/daemon/internal/daemon/bootgate.go`) refuses to start
unless the Oh My Pi a pane will run loads this plugin at the daemon's contract
(`legion.daemonApiVersion` in this package's `package.json`, equal to `DaemonAPIVersion` in
`packages/daemon/internal/api/version.go`) and loads `@sjawhar/pi-envoy` with it, so both go into
the Oh My Pi profile the daemon's panes use:

```sh
omp plugin install @sjawhar/pi-envoy && omp plugin install @sjawhar/pi-legion
```

Both plugins release from one commit, with the same version, and the gate refuses a pair whose
interface versions differ, so upgrade them together. A daemon on the Kubernetes runtime needs no
install on the daemon host for its pods: the worker image carries both, packed from the same
checkout as the `legion` binary, at `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`, and
installs each into its `legion` profile (`packages/daemon/docker/worker.Dockerfile`,
`docs/kubernetes.md`). The operator's own machine, where `legion controller start` runs the
controller, installs both the same way.

## Published package

Released installs come from npm as `@sjawhar/pi-legion`. The tarball ships `dist/legion.js`, which
bundles every dependency except the Oh My Pi host packages (`@legion/pi-shared`, `@legion/contracts`
and `@legion/envoy-client` are inlined), `dist/THIRD_PARTY_NOTICES`, the nine Legion skills staged at
`dist/skills` (`legion-architect`, `legion-controller`, `legion-oracle`, `legion-retro`,
`legion-worker`, `leading-a-work-stream`, `ce-simplify-code`, `thermonuclear-code-quality`, `thermonuclear-deep-review`;
the manifest's `omp.skills: ["dist/skills"]` is what lets Oh My Pi discover them), and `agents/`,
the task agents Legion's prompts dispatch (`oracle`; the reviewer's pair
`thermonuclear-deep-review` and `thermonuclear-code-quality`; `deep-worker`; the planner's
`plan-gap-analyst` and `plan-reviewer`, which also review a root spec before every approval
request, `plan-reviewer` checking each of its factual claims), each
declaring its model as a role the operator maps (`docs/kubernetes.md`, "Model roles"). The Dispatch
and Envoy skills (`asd-ste100`, `dispatch`, `dispatch-first`,
`dispatch-writing-specs`, `envoy`) ship in `@sjawhar/pi-envoy`; a link from a Legion skill into one
of them is written `skill://dispatch/...`, which resolves by name once both are installed.

`scripts/pi-plugin-prepack.sh` at the repository root is both plugins' `prepack`: it builds the one
entry, writes the notices and stages this package's partition of `skills/`. It refuses to pack with
the committed manifest, whose `omp.extensions` names `extensions/legion.ts`; the release workflow's
`pi_legion_build` job (`.github/workflows/release.yaml`), the worker image and the e2e pack step
rewrite it to `["dist/legion.js"]` before `bun pm pack`. Only the e2e pack step restores it
afterwards: the build job's tree is discarded, and `pi_legion_publish` commits the version bump to
the committed manifest, touching `.version` alone. The manifest's `legion` key carries the daemon
API contract the plugin was built against; `src/daemon-api-version.test.ts` pins it to the daemon's
golden fixture.

## Run records

A deployment can have every agent run write one record: a run is one prompt-bounded turn of a
session, from Oh My Pi's `agent_start` to its `agent_end`. The plugin's part is a hook
(`src/run-record.ts`) that starts the deployment's record builder at each run's start and end; the
builder is the deployment's own program, not part of this repository, and appends one JSON line
per call to a spool, `<RUN_RECORD_DIR>/<YYYY-MM-DD>/<session id>.jsonl` (the UTC day of the line's
own time). A root timer on the host copies the spool to the operator's bucket with
`scripts/run_record_upload.py`.

### The hook

The hook reads these variables once, when the plugin loads. It is off, and registers no handler,
unless `RUN_RECORD_DIR` is set.

| Variable | Meaning |
| --- | --- |
| `RUN_RECORD_DIR` | The spool root. |
| `RUN_RECORD_BUILDER` | The record builder's absolute path. |
| `RUN_RECORD_PYTHON` | The interpreter that runs it; `python3` by default. |
| `RUN_RECORD_CACHE_DIR` | The builder's scan cache; `<RUN_RECORD_DIR>/.cache` by default. |
| `RUN_RECORD_HOST` | The host name each line carries; the machine's host name by default. |

The run's identity comes from the pane's own variables: `LEGION_PROJECT`, `LEGION_TREE`,
`LEGION_ISSUE`, `LEGION_ROLE`, `LEGION_GENERATION` and `LEGION_WORKSPACE`, each passed only when set;
the session id and transcript path are the session's own, the values the claim registers with the
daemon. A session with no transcript file records nothing.

- At `agent_start` the hook makes a UUIDv7 run id, reads the clock and the transcript's size, and
  runs `<RUN_RECORD_PYTHON> <RUN_RECORD_BUILDER> start --spool-dir D --run-id R --session-id S
  --session-file F --started-at T --host H [--project P] [--tree T] [--issue I] [--role R]
  [--generation N] [--parent-run-id R] [--workspace W] [--session-offset BYTES]`. Times are UTC ISO
  8601 with milliseconds (`2026-10-29T10:00:00.123Z`).
- At `agent_end` it runs the same command as `end`, with every `start` argument and then
  `--ended-at T --end-reason agent_end [--last-response-id ID] [--handoff-phase P
  --handoff-commit SHA] --cache-dir D`. `--last-response-id` is the `responseId` of the run's last
  assistant message; the handoff pair is the phase and commit of a `legion` tool `handoff_complete`
  that succeeded in the run.
- At `session_shutdown` with a run still open it ends that run the same way with
  `--end-reason shutdown`; the run's own `agent_end`, if Oh My Pi still sends one, finds no run open.
- A `task` subagent runs its own instance of the plugin in its parent's process, and the hook is
  registered before the plugin's subagent check, so a subagent's runs are recorded too, with
  `--parent-run-id` naming the run its parent's session has open (the parent's transcript is
  `<directory of the subagent's transcript>.jsonl`).

Each builder process starts detached, its output discarded, and no turn waits for it. When it
cannot start (`RUN_RECORD_BUILDER` unset or naming no file, the interpreter missing) the hook
appends the line itself, in one write, with error code `no_builder`; when the builder exits
non-zero, which it does only when it wrote no line, the hook appends it with `builder_exit:<code>`.
That line carries what the hook knows (identity, times, end reason, handoff, `branch` as
`legion/<issue>`), the builder's fields null, and on an end line `obligations` as
`{"rule": "syntactic_v12", "evaluated": "error", "error": <code>, "changes": []}`. Every failure
of the hook is logged and none reaches the turn.

The spool and the scan cache must be writable by the uid the panes run as: the builder appends to
the spool and takes a lock under the cache while it scans. The builder's `warm --workspace
<checkout> --cache-dir <RUN_RECORD_CACHE_DIR>` fills the cache once, so the first run's end does
not pay for a cold scan of the whole tree.

### The uploader

`scripts/run_record_upload.py` is a standard-library Python 3 script. Run it as root, every ten
minutes, from a timer of the host's own that starts the next pass ten minutes after the last one
ends, so passes never overlap (a pass that finds another holding its state lock also leaves at
once):

```sh
python3 run_record_upload.py upload --spool-dir <spool on the host> --bucket <bucket> \
  --project <project> [--host <host>] [--state-dir /var/lib/legion-run-records]
```

- Each complete line becomes one object,
  `gs://<bucket>/runs/v1/<project>/<host>/<YYYY-MM-DD>/<session_id>/<run_id>.<kind>.json`, holding
  the line's bytes without its newline. `--project` and `--host` are the uploader's own (`--host`
  defaults to the host's name), the day is the spool day directory, and `session_id`, `run_id` and
  `kind` come from the line. The bucket is a required input; nothing names one by default.
- The upload is write-once (`ifGenerationMatch=0`), so no object is ever replaced; a 412 means the
  object is already there and counts as uploaded.
- A trailing line with no newline yet is left for the next pass. A line that is not a JSON object
  of schema `run-record/1`, whose `kind` is not `start` or `end`, or whose `session_id` or `run_id`
  is not 1 to 128 of `A-Z a-z 0-9 . _ -`, is skipped and counted, and never read again.
- Root never opens the spool. The script reads it as the spool directory's owner (`setpriv
  --reuid=<uid> --regid=<gid> --clear-groups`, `--reader-uid`/`--reader-gid` to override), taking
  day directories and files only as real directories and regular files; the root process keeps the
  cloud identity and uploads only the bytes the reader hands it. How far into each spool file it has
  uploaded is root's, in `<state-dir>/state.json`. A failed upload stops that file at that line for
  the pass; the next pass resumes there.
- The token comes from the VM's metadata server; `--token-command '<command>'` takes it from a
  command that prints one instead. The identity needs to create objects in the bucket, and nothing
  else.
- `--dry-run` uploads nothing, records nothing, and prints each object name it would write.
- The last line of every pass counts what it did (`uploaded=`, `already=`, `invalid=`,
  `incomplete_files=`, `errors=`); the exit status is 1 when any upload or the reader failed, 2 for
  a bad argument, 0 otherwise.

### What a deployment does

None of this runs until a deployment turns it on: it installs a plugin release carrying the hook,
puts a read-only copy of the record builder on the host, sets the `RUN_RECORD_*` variables in its
panes, creates the spool and cache directories the panes own, grants the uploader's identity object
creation in its bucket, and adds the timer.

## Development install

The repository root `package.json` loads `../pi-envoy/extensions/envoy.ts` for dev sessions inside
this checkout and not this entry: `legion.ts` is inert outside a Legion pane, and a Legion pane runs
the installed packages. To run this entry from the checkout, link it into an Oh My Pi profile beside
the Envoy entry:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" ~/.omp/agent/extensions/envoy.ts
ln -sfn "$PWD/packages/pi-legion/extensions/legion.ts" ~/.omp/agent/extensions/legion.ts
```

The two cross-entry tests (`extensions/legion-role-claim.test.ts`,
`extensions/legion-phase-stall-omp.test.ts`) load both entries in one process; the second needs
the pinned Oh My Pi (`LEGION_TEST_OMP`). `CHANGELOG.md` records this package's releases; the history
before the split is in `packages/pi-envoy/CHANGELOG.md`.
