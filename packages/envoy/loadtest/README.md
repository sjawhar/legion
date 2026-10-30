# Dispatch load test (`loadtest/`)

This package reproduces the load of the 2026-10-26 Dispatch outage on one 2-core machine. It runs
a local stack (Postgres, a latency proxy, NATS, the Envoy listener and Dispatch, all built from this
checkout), puts a measured load on it and writes tables of what happened.

- The outage-load reproduction report of 2026-10-27 (the root report, below), which ran on a
  2-vCPU pod, is the source of the tables' shape. This harness is the committed form of the
  harness that report used.

The harness measures. It changes no Dispatch, listener or outbox code. A fix of the outage runs the
same step before and after its change, and compares the two rows.

## Files

| File | What it is |
| --- | --- |
| `main.go` | The `loadgen` binary: subcommands `proxy`, `seed`, `seed-refs`, `refs-shape`, `refs-fetch`, `run`, `tables`, `series-sessions`, `trigger-sql`, `box-busy`, `replay-fetch`, `replay-pack`, `replay-unpack`, `replay-verify-bundle`, `replay-seed`, `replay-check-seed`, `replay-checksum`, `replay-logical-dump`, `replay-plan` |
| `api.go` | The Dispatch HTTP client, with the request trace that gives the arrival time; `callRaw` sends a body of any type (a multipart upload), and `callKeyed` and `callRetryKeyed` send a write under an idempotency key |
| `seed.go` | The seed: 4 projects, 1,000 issues and 8,000 comments through Dispatch's API, and `seed.json` |
| `refs.go` | The reference seed: the plan of 1,000 cited documents and their citations, the writes, the shape check (`refs-shape`) and the answer fetch (`refs-fetch`) |
| `run.go` | One step: its phases (the window, the dashboard streams, the event and request generators on one open-loop schedule, the probes) and `summary.json` |
| `sessions.go` | The simulated sessions: their NATS subscriptions and receipts, their listener registrations and heartbeats |
| `events.go` | The five write kinds, the event mix and each write's plan |
| `marker.go` | The write marker `lgm<nonce><counter>x`: its format, the run's nonce and the parse that keeps one run's markers |
| `routes.go` | The route-mix file parser and the request paths |
| `delivery.go` | The join of writes, database commits and receipts into delivery latencies |
| `sample.go` | The 5 s sampler: CPU, RSS, Postgres connections, goroutines, pool waiters, heap, NATS `/varz`. The transaction-id probe. |
| `lockprobe.go` | The 20 ms sampler of the commit-order lock |
| `locksampler.go` | The 1 s sampler of every ungranted lock in `pg_locks` |
| `pgpoll.go` | The poll loop of the lock probes (`pollPG`), and the error policy of the probes' reads, which the 5 s sampler shares: failed reads are counted, the first three are logged, and so is their total past three |
| `live.go` | The dashboard live sockets' reader of `GET /ws/live` event frames, and the live frame latencies |
| `sse.go` | The reader of `GET /api/v1/events`, the dashboard's stream on a Dispatch build that serves no `/ws/live` (The dashboard stream, below) |
| `dashboard.go` | The probe that chooses between the two, once, before the window |
| `proxy.go` | The TCP latency proxy toward Postgres |
| `tables.go` | `TABLES.md` from one or more `summary.json` files, the pprof tops and the goroutine summary |
| `guard.go` | `stack.json`, the endpoint flags and the check that every endpoint is the stack's own |
| `util.go` | Helpers that several files share: the sleep, the profile fetch, the JSON file write and the collector of a step's records |
| `replay_bundle.go` | The replay bundle: a project's state at an instant plus its window of writes (`state.json`, `docs/`, `window.jsonl`, `created-docs/`, `roster.json`, `bundle.meta.json`), with each document's listed version sizes, the open and answered asks and the resolved typed ask blocks still in their document's text (with the resolution), the version each typed ask block's answer or resolve landed at, and the current blocks of every anchored document; `readBundle`, which verifies every file's sha256, and `keepIssue`, the per-issue sample |
| `replay_fetch.go` | `loadgen replay-fetch`: reads a project's issues, event logs, artifact listings, document texts and anchored documents' blocks from a Dispatch, read-only (`DISPATCH_URL`, `DISPATCH_TOKEN_FILE`), caches every response under `<out>/raw/` |
| `replay_reconstruct.go` | What `replay-fetch`'s cached logs, listings and texts make of a bundle: the state at the instant, the window, each document's version sizes and blocks, the roster, the counts and the anchor rules |
| `replay_pack.go` | `loadgen replay-pack` and `replay-unpack`: a bundle (not `raw/`) as deterministic zstd tar parts, and back |
| `replay_get.go` | `loadgen replay-verify-bundle`: `readBundle` on an unpacked bundle (every file against `bundle.meta.json`'s sha256), then one line of counts; `stack.sh replay-get`'s last check |
| `replay-2026-10-26.json` | The replay manifest (The replay, below): the bundle's and the seed dump's parts, each a file artifact of the replay's issue (`LOADTEST_REPLAY_ISSUE`; the manifest names no issue) with its slug, version, bytes and sha256 |
| `replay_seed_plan.go` | What the replay seed sends, decided from the bundle alone: issue numbers and placeholders (`-keys-file`), which comments are recreated and in what order, the sentences an anchor may fall back to, approval and issue-field mappings |
| `replay_anchor.go` | The rest of the seed's pure decisions: `chooseAnchor`, where an anchor lands (its logged quote, else a sentence of its logged block or of the nearest surviving one), which the replay source calls as `substituteAnchor`; and `planHistory`, the size of every synthetic version so each changes as many bytes as production's, built from a padding paragraph and a removed run of sections |
| `replay_seed.go` | `loadgen replay-seed`: recreates a bundle's state through Dispatch's API, mapping every logged id to its local one in `loadtest.replay_ids` and each anchor's rule in `loadtest.replay_anchors` as it goes, so a killed seed resumes; `-ops-log` writes one CSV row per write with Dispatch's CPU ticks around it |
| `replay_seed_asks.go` | The seed's step 4 and step 5: an issue's asks (each one's thread, its answer and the version the answer landed at) and then its fields and claim |
| `replay_seed_ids.go` | `loadtest.replay_ids`, the cached logged-to-local id map the seed writes as it goes and a resumed seed reads |
| `replay_check.go` | `loadgen replay-check-seed` (each seeded document's version, version size changes, open anchored items, text and anchors against the bundle, the open-anchored-root distribution of the window's written documents, and `-out`, the results file stack.sh keeps), `replay-checksum` (row counts and an md5 of the versions and comment bodies) and `replay-logical-dump` (a seed with every uuid and timestamp masked, for `cmp`) |
| `replay_plan.go` | The replay request source's plan (`loadgen run -source replay`, The replay below): the pure plan of a bundle's window (transactions, issue sampling, each logged write's CLI requests, read actions) and `loadgen replay-plan` |
| `replay_run.go` | The replay's sending (`loadgen run -source replay`): the setup taken from the bundle before the step, id resolution through `loadtest.replay_ids` and the run's own responses, one connection and one 60 s deadline per action |
| `replay_edit.go` | The replay's edit ops: from the logged diff (`replace` of changed words, `insert` of an added paragraph) or substitutes, each one Dispatch 35efd326's edit route accepts on the client's text (The replay below); `TestReplayEditsBundle` (with `LOADTEST_REPLAY_BUNDLE`) replays every edit `replay-plan` plans at rate 1 against a model of those rules |
| `replay_record.go` | The replay's per-run record: `summary.json` `replay` (Dispatch's CPU over the window from `/proc`, cores at the full rate, idle-corrected, drain, writes, saturation) and `meta.json` `replay` |
| `replay-seed-len16.patch` | The allocation-free `len16` that `stack.sh replay-seed` applies to Dispatch at 35efd326 for the seed, with its tests (The replay, below) |
| `zz_loadtest_pprof.go.txt` | The pprof listener a Dispatch tree without `cmd/dispatch/pprof.go` gets when `stack.sh` builds it from another commit |
| `commit-trigger.sql` | The commit-time trigger for the delivery latency from commit |
| `refs-sample-2026-10-27.json` | The reference seed's input: for each of 160 production issues sampled on 2026-10-27, the version count of every member of its references answer. 160 arrays of numbers, no issue key and no name. |
| `routes-2026-10-26-1719Z.txt` | The default route mix |
| `stack.sh` | The local stack: `setup`, `start`, `seed`, `reset` (and `reset --replay`), `stop`, `status`, `build-info`, `loadgen`, `replay-seed-patch-check`, `replay-seed`, which seeds the LINUX replay, runs `replay-check-seed` and dumps the database only when it passed, and the replay's `replay-manifest`, `replay-get` and `replay-prepare` |
| `replay.sh` | The LINUX replay's `stack.sh` commands, sourced by it: `replay-seed-patch-check`, `replay-seed`, `replay-manifest`, `replay-get`, `replay-prepare` and the `reset --replay` that resets from a prepared template |
| `run-step.sh` | The one command for a named step |
| `run-series.sh` | The series of six steps, 0.25x to 10x |
| `common.sh` | What the three scripts share, sourced by each: the `LOADTEST_ROOT` and `LOADTEST_SRC` paths, the run lock (One run at a time, below), and the setup, start, seed and migration check before a step |
| `*_test.go` | Unit tests. They need no Postgres and no NATS: `cd packages/envoy && go test ./loadtest/` |
| `run-step.test.sh` | The test of the run lock. It fakes `go` and `uv` and needs no stack: `cd packages/envoy && bash loadtest/run-step.test.sh` |
| `common.test.sh` | The test of `lock_root`: a series step keeps the lock after its parent dies, a refusal names the live holder, a token without the holder's descriptor is refused, a lock held by a process that this PID namespace cannot see is refused as held (never as free), a caller takes the lock once its holder has ended, two callers waiting at once never name each other as the holder, and a record of another PID namespace whose pid runs here is refused without naming that pid. It needs no stack: `cd packages/envoy && bash loadtest/common.test.sh` |

## Requirements

- A Linux machine with 2 vCPUs, the shape of a Legion issue pod. The root report ran on such a
  pod: `nproc` 2, 19 GiB of memory, a gVisor sandbox. The samplers read `/proc`, so the harness
  runs on Linux only.
- No other heavy work on the machine during a step. `meta.json` records `box_busy_before`: the
  share of the box's CPU time that was busy over 5 s just before the step, from two `/proc/stat`
  reads. `run-step.sh` takes it before its reset, while Dispatch is stopped and the rest of the
  stack is idle (`loadgen box-busy`), so Dispatch's own start-up work is not in it; an idle box
  reads 0%. When it is above 10%, `run.log` warns that another process on the machine uses CPU. The
  load average proves nothing here: on gVisor `/proc/loadavg` always reads 0.00. Each Legion role
  runs in its own PID namespace, so the processes of other roles in the same pod are invisible to
  `ps` and to the sampler, but their CPU shows in this number.
- One run at a time on a `LOADTEST_ROOT` (One run at a time, below).
- The Go toolchain (`go`). `stack.sh setup` builds the binaries and installs `nats-server` with it.
  `loadgen tables` uses `go tool pprof`.
- `uv`. `stack.sh setup` installs Python 3.12 and the `pgserver` wheel (0.1.4) with it. The wheel
  holds Postgres 16.2 and pgvector. The harness uses no Docker.
- `jq`, `curl` and `flock` (util-linux).
- `git` is optional. Without it, `meta.json` and `seed.meta.json` record the build commit as
  `unknown`.
- Disk space under `LOADTEST_ROOT`. In the measured runs, each step directory held 20-25 MB, most
  of it `heap.txt`, and the Postgres data directory held about 520 MB: the seed template and the
  step's copy of it.

### Environment

| Variable | Default | What it sets |
| --- | --- | --- |
| `LOADTEST_ROOT` | `${TMPDIR:-/tmp}/dispatch-loadtest` | The directory that holds all the stack's state, binaries and step output. On a Legion pod, `$TMPDIR` is `/legion/scratch/tmp`, the issue's volume. |
| `LOADTEST_SRC` | The checkout that holds the script | The checkout that `stack.sh setup` builds `envoy-dispatch`, `envoy-listener` and `loadgen` from. |
| `LOADTEST_PG_PORT` | `15000 + 10k` | The Postgres port. |
| `LOADTEST_PROXY_PORT` | `15001 + 10k` | The latency proxy port. |
| `LOADTEST_NATS_PORT` | `15002 + 10k` | The NATS client port. |
| `LOADTEST_NATS_MONITOR_PORT` | `15003 + 10k` | The NATS monitoring port (`/varz`). |
| `LOADTEST_LISTENER_PORT` | `15004 + 10k` | The Envoy listener port. |
| `LOADTEST_DISPATCH_PORT` | `15005 + 10k` | The Dispatch HTTP port. |
| `LOADTEST_PPROF_PORT` | `15006 + 10k` | The Dispatch pprof port (`DISPATCH_PPROF_PORT`). |
| `LOADTEST_PROXY_RTT` | `1ms` | The round trip that the proxy adds, as a Go duration. Read when the proxy starts. |
| `LOADTEST_PG_DIRECT` | unset (`1` for a `--replay` step) | `1` connects Dispatch to Postgres directly, without the proxy. Read when Dispatch starts, so set it on `run-step.sh`. `stack.json` and `meta.json` record it as `pg_direct`. |
| `LOADTEST_DISPATCH_COMMIT` | unset | `stack.sh setup` builds `envoy-dispatch` alone from that commit (`run-step.sh --replay --dispatch-commit`). |
| `LOADTEST_GOMAXPROCS` | nproc | Dispatch's `GOMAXPROCS` (`run-step.sh --replay --gomaxprocs`). |
| `LOADTEST_REPLAY_ISSUE` | unset | The issue the replay's bundle and seed parts are file artifacts of. `stack.sh replay-get` and a `run-step.sh --replay` step require it and refuse before they start without it; the manifest names no issue. |
| `DISPATCH_URL`, `DISPATCH_TOKEN_FILE` | unset | Read by `stack.sh replay-get` alone, in its own shell, to download the replay's parts. |

Every process listens on `127.0.0.1`. The default ports follow `LOADTEST_ROOT`: the root's block
`k` is the first 8 hex digits of the sha256 of its physical path, modulo 200, and each port is
`15000 + 10k` plus the offset above, so every port lies between 15000 and 16996 and no two ports of
a root meet. The block is a hash, not a reservation: two roots land in one block about 1 pair in
200, and then they hold the same 7 ports. Two Legion roles of one pod, each with its own
`LOADTEST_ROOT`, therefore almost always run a stack each with no port variable set; on a collision
`stack.sh start` refuses the second stack (`check_port`, so no stack is ever shared) and the
refusal names the block and the remedies: set the `LOADTEST_*_PORT` variables, or move to another
`LOADTEST_ROOT`, which picks another block with probability 199/200. Each `LOADTEST_*_PORT`
overrides its own port, and `stack.json` keeps the port a `setup` recorded; a port that is not
this root's block default any more is refused without the block explanation, which would name a
block that has nothing to do with the port in use.

`stack.sh setup` writes the ports into `$LOADTEST_ROOT/stack.json`, and `stack.sh status` prints
them with the path of that file. A later `setup` keeps the ports the file records and moves only
those whose variable the environment sets, and it refuses to move a port while a stack process
runs. `start` refuses a port that another process holds: it names the process when this PID
namespace can see it, and says that it cannot see one when another Legion role of the pod, in its
own PID namespace, holds the port.

Every stack process starts under `env -i` with an explicit environment. The `DISPATCH_URL`,
`ENVOY_URL`, `ENVOY_NATS_URL`, `AWS_*`, `HOME` and `XDG_*` of your shell never reach the stack or
`loadgen`. The stack's `HOME` is `$LOADTEST_ROOT/home`. Dispatch keeps its signing key there, so
the key stays the same across steps. `stack.sh` stops each process by the pid in its pidfile
(`$LOADTEST_ROOT/run/<name>.pid`), never by name.

## Run one step

```bash
packages/envoy/loadtest/run-step.sh <name> --events <per s> --sessions <n> --requests <per s> \
  [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>] \
  [--route-mix <file>] [--human 0.1] [--live <n>] [--rng-seed 1] [--out <dir>]
```

A replay step takes `--replay` and the replay's flags instead (The replay, below).

Example, the 1x step of the root report:

```bash
packages/envoy/loadtest/run-step.sh step-1x --events 4 --sessions 400 --requests 60
```

The step name comes first. It can hold only letters, digits, `.`, `_` and `-`. Every flag takes a
value.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--events` | required | Writes per second. `0` sends no writes. |
| `--sessions` | required | Simulated sessions, 1 or more. |
| `--requests` | required | GET requests per second. `0` sends no requests. |
| `--duration` | `180s` | The measured window. More than 0. |
| `--warmup` | `30s` | Load before the window. Nothing in it is counted. |
| `--grace` | `20s` | The wait for late receipts after the generators stop. |
| `--event-mix` | `production-2026-10-26` | A preset name or `kind=weight,...` (Event mix, below). |
| `--route-mix` | the committed `routes-2026-10-26-1719Z.txt` | A route-mix file (Route mix, below). It must be a file. |
| `--human` | `0.1` | The share of writes that a person makes, 0 to 1. |
| `--live` | `max(1, round(requests/30))` | Dashboard streams, each subscribed to the event feed as a dashboard tab is: 2 at 60 req/s, 20 at 600 req/s. They open `GET /ws/live`, or `GET /api/v1/events` against a build that serves no live socket (The dashboard stream, below). |
| `--rng-seed` | `1` | The seed of the event and request schedules. |
| `--out` | `$LOADTEST_ROOT/runs/<name>` | The step directory. It must not exist, or it must be empty. |

Rates are used as given. `0` is a real zero. The event and request schedules are open-loop Poisson
from the RNG seed, so two runs of one step with one seed offer the same arrivals.

`run-step.sh` does these steps in order:

1. `stack.sh setup`: it builds the binaries from the checkout (incremental), so a step always runs
   the checkout's current code.
2. `stack.sh start`, then `stack.sh seed`. Each one does nothing when its work is already done. So
   `run-step.sh` is the one command from a fresh checkout. On the first run, the seed alone takes
   37 to 50 minutes.
3. The migration check (Before and after across commits, below).
4. `stack.sh reset`, with Dispatch's log in the step directory (The reset, below).
5. `loadgen run`. It writes the step's files.
6. It stops Dispatch. It also stops Dispatch when the step fails. Postgres, the proxy, NATS and the
   listener keep running. `stack.sh stop` stops them.
7. It copies the listener and NATS logs into the step directory and runs `loadgen tables` there.

A step of a series skips steps 1 to 3: `run-series.sh` did them once, before its first step (Run
the series, below).

Before step 1, `run-step.sh` takes the run lock of the root (One run at a time, below).

**A step whose stack process exits fails.** The sampler checks every stack process (Dispatch, the
listener, NATS, the proxy and Postgres) at each 5 s sample, and once more after the grace. A
process is alive while `/proc/<pid>` exists and is not a zombie. When one has exited,
`loadgen run` still writes `summary.json`, with `stack_exits` (each process's name, pid and the
time it was first seen gone), and the other files that it can take. Then it exits with status 1:
`loadgen run: dispatch (pid N) exited during the step (first seen gone at T); this row does not
measure the load`. `run-step.sh` renders `TABLES.md` for diagnosis, with the mark
`(dispatch exited)` after the step name, and exits with status 1, pointing at `loadgen run`'s
message and at `run.log`. A Dispatch crash or out-of-memory kill at a high step thus never reads as
the overload that the series measures.

**A step whose commit rows cannot be read fails.** The committed counts, the receipts and the
delivery latencies all come from the run's rows of `loadtest.event_commits` (Delivery latency,
below). When `loadgen` cannot read them, because the database has no `commit-trigger.sql` or
Postgres does not answer, it still writes `summary.json` with `events.commits_error` (why the read
failed), `meta.json` and `samples.json`. Then it exits with status 1, naming the commit-row read and
`events.commits_error`. `run-step.sh` renders `TABLES.md` for diagnosis, where every cell that
depends on those rows shows `-` and the step name carries the mark `(commit rows not read)` (The
tables, below), and exits with status 1; `run-series.sh` stops there. The other reads of the
database (the lock probe, the ungranted-lock sampler, the 5 s sampler's `pg_stat_activity`, the
transaction-id probe and the Postgres version in `meta.json`) feed none of those columns. A failure
of one of them marks its own table, or `meta.json`, and the step does not fail.

To run a step name again, remove its step directory first.

`loadgen run` also takes `-nats-conns` (default 50), the NATS connections that the sessions share.
`run-step.sh` does not pass it.

### One run at a time

A step resets the stack and measures the whole box, so one `LOADTEST_ROOT` runs one step or one
series at a time. A second `run-step.sh` on the root of a running step would stop that step's
Dispatch and listener in its reset, and the first step would finish as a row of connection
failures.

`run-step.sh` and `run-series.sh` take an exclusive `flock` on `$LOADTEST_ROOT/.run.lock`, kept on
file descriptor 9, before they touch the stack. The lock lasts until every process that holds that
descriptor has exited: the holder and the children that inherit it. Once locked, the holder empties
the lock file and writes its pid, its PID namespace (`/proc/self/ns/pid`), command, start time and
a random token into it. Another run on the root that sees a live holder is refused at once. While
it sees none, it tries the lock for up to 3 s, on descriptor 8 so that a waiting run never holds
descriptor 9 on the file, and takes it when the lock is freed in that time. These `stack.sh`
commands are refused the same way: `setup`, `start`, `seed`, `reset` and `stop`; `stack.sh
status`, `build-info` and `loadgen` take no lock. A refusal exits with status 1 and names the root
and the holder that it can find:

- the pid in the lock file, when that pid runs in the refused caller's PID namespace. The pid is
  trusted only when the lock file names that namespace;
- else, when that run has exited and its children still hold the lock, those processes;
- else the pid, start time and command that the lock file records for that run. Then the holder
  is not visible in this PID namespace: a run in another PID namespace, such as another Legion
  role in this pod on a shared `LOADTEST_ROOT`. A pid that the lock file records for another PID
  namespace is written as `pid=N in PID namespace pid:[M]`, since pid N here is an unrelated
  process. The refusal says not to remove `.run.lock`: that run still holds the lock, and a run
  that locked a new file would reset the stack under it.

The holder's own children pass: the holder exports the token as `LOADTEST_LOCK_TOKEN`, and a caller
whose token equals the lock file's and whose descriptor 9 is the holder's locked one keeps that
descriptor and goes on without locking. Thus the series' own `run-step.sh` calls, and the
`stack.sh` calls of a run, are not refused, and a series step, with the `loadgen run` that it
starts, keeps the root when `run-series.sh` is killed. A caller with the token but without the
descriptor is refused. No stack process holds the lock: `stack.sh` starts each one with
descriptor 9 closed.

## Run the series

```bash
packages/envoy/loadtest/run-series.sh [step flags]
```

The series runs six steps with `run-step.sh`, in order, each from a fresh reset. It writes them
into `$LOADTEST_ROOT/runs/series-<UTC time>/step-<f>x/`, and one `TABLES.md` across them in the
series directory.

| step | events/s | sessions | requests/s | live sockets |
| --- | --- | --- | --- | --- |
| step-0.25x | 1 | 100 | 15 | 1 |
| step-0.5x | 2 | 200 | 30 | 1 |
| step-1x | 4 | 400 | 60 | 2 |
| step-2x | 8 | 467 | 120 | 4 |
| step-5x | 20 | 667 | 300 | 10 |
| step-10x | 40 | 1,000 | 600 | 20 |

- Events are 4/s and requests 60/s at 1x, multiplied by the load factor f.
- Live sockets are `max(1, round(2f))`.
- Sessions are piecewise linear through the spec's three points: 100 at 0.25x, 400 at 1x and
  1,000 at 10x. That is 400·f up to 1x, and 400 + 600·(f − 1)/9 above it.
  `loadgen series-sessions <f>` prints the value.
- The series passes every other step flag to each step: `--duration`, `--warmup`, `--grace`,
  `--event-mix`, `--route-mix`, `--human`, `--rng-seed`. It refuses `--events`, `--sessions`,
  `--requests`, `--live` and `--out`, because it sets them.
- With the default window, a step takes about 5 minutes and a series about 30 minutes.
- `run-series.sh` runs the setup (one build of the checkout), the start, the seed and the migration
  check once, before the series directory exists, and reads the build's commit and dirty flag once
  (`stack.sh build-info`). It passes them to its steps in `LOADTEST_SERIES_BUILD`. Its steps skip
  the setup and record that build in `meta.json`, so every step of a series runs one build, even
  when the checkout changes during the series. A `run-step.sh` that does not hold the series' lock
  ignores `LOADTEST_SERIES_BUILD` and builds the checkout.
- The series holds the run lock for its whole run.
- A failed step stops the series. `run-series.sh` still writes the series `TABLES.md`, over the
  steps that ran (the failed step included when it wrote `summary.json`), and exits with status 1,
  naming the step.

## The stack (`stack.sh`)

| Command | What it does |
| --- | --- |
| `stack.sh setup` | Builds `envoy-dispatch`, `envoy-listener` and `loadgen` into `$LOADTEST_ROOT/bin`. Installs `nats-server` at the version that `packages/envoy/go.mod` pins, with `go install`. Installs Postgres with pgvector from the `pgserver` wheel. Runs `initdb`. Writes `stack.json`. Each run does it again, incrementally. |
| `stack.sh start` | Starts Postgres, the proxy, NATS and the listener. It keeps a process that already runs. It refuses a port that another process holds, and names that process when this PID namespace can see it. Dispatch starts only in `seed` and `reset`. |
| `stack.sh seed [--force]` | Makes the seed template `dispatch_seed` (The seed, below). It does nothing when `dispatch_seed` and `seed.meta.json` exist. `--force` drops the old seed and seeds again. |
| `stack.sh reset [LOG]` | Resets the stack to the seed and starts Dispatch, with its log in `LOG` (default `$LOADTEST_ROOT/logs/dispatch.log`). `run-step.sh` does this before each step. |
| `stack.sh reset --replay [LOG]` | The same from this build's prepared replay template (The replay, below), refused without one; before Dispatch starts, `loadgen replay-checksum -db dispatch` into `$LOADTEST_ROOT/replay/last-reset.json`. |
| `stack.sh replay-manifest --bundle-pack FILE --bundle DIR [--seed-dir DIR] [--part-version N]` | Prints a replay manifest from `replay-pack`'s JSON, the bundle's `bundle.meta.json` and `replay-seed`'s `seed.meta.json` (`"seed": null` without `--seed-dir`), every part at version N (default 1). |
| `stack.sh replay-get [--manifest FILE] [--from-dir DIR]` | Gets, verifies and unpacks the manifest's bundle, and restores its seed dump into the template `dispatch_replay_seed` (The replay, below). |
| `stack.sh replay-prepare` | Makes this build's replay template `dispatch_replay_<key>` from `dispatch_replay_seed` (The replay, below). |
| `stack.sh stop [NAME...]` | Stops `dispatch`, `listener`, `nats`, `proxy` and `postgres`, or only the named ones. |
| `stack.sh status` | Prints this root's ports and the `stack.json` that records them, each process's pid and ports, and the seed's migration, build commit and counts. |
| `stack.sh build-info` | Prints the checkout's commit and `true` or `false` for uncommitted changes in its tree, as `<commit> <dirty>`. `run-step.sh` records them in `meta.json`. |
| `stack.sh loadgen ARGS...` | Runs `$LOADTEST_ROOT/bin/loadgen` with the stack's environment, from the root. `run-step.sh` and `run-series.sh` call `loadgen` this way. |

`setup`, `start`, `seed`, `reset`, `stop`, `replay-seed`, `replay-get` and `replay-prepare` take the
run lock for their run, and are refused while another run holds it (One run at a time, above).

The processes:

- **Postgres** 16.2 with pgvector, from the `pgserver` wheel. `initdb` adds the root report's
  settings: `max_connections 800`, `shared_buffers 1GB`, `fsync on`,
  `log_min_duration_statement 50ms`, `log_lock_waits on`. It also adds
  `track_commit_timestamp = on`, for the delivery latency from commit, and
  `unix_socket_directories = ''`, for TCP only. Its logs are in `$LOADTEST_ROOT/pg/log`.
- **The latency proxy**, `loadgen proxy -rtt 1ms`. It holds each chunk for half the round trip in
  each direction. Dispatch reaches Postgres through it. In the root report, `-rtt 1ms` added about
  4 ms per round trip: `select 1` took 4.18 ms p50 through the proxy and 0.13 ms directly. A short
  sleep in gVisor oversleeps.
- **NATS** with JetStream: `nats-server -js -a 127.0.0.1 -n <stack name> -m <monitor port>`, on a
  fresh store directory at each start.
- **The listener**, with `ENVOY_MACHINE_ID=<stack name>` and a random API token.
- **Dispatch**, with a random agent token and header identity
  (`DISPATCH_IDENTITY=header:X-Dispatch-User`, `DISPATCH_IDENTITY_HEADER_TRUSTED=1`). It reaches
  the stack's listener (`ENVOY_URL`) and NATS. It gets no `AWS_*` variables, so it calls no
  embedding model. `DISPATCH_WEB_DIST` is an empty directory.

`stack.json` holds the stack name (`loadtest-<random>`), the two random tokens, every port and
every pid. It is mode 600. `loadgen` reads every endpoint and token from it.

## The seed

`stack.sh seed` makes the database that every step starts from. It takes 37 to 50 minutes on a
2-vCPU pod: 14 to 27 minutes for the issues and comments with the wait for the block index
(1,086 s and 1,600 s in two seeds on 2026-10-27, 14 minutes on 2026-10-29), and about 23 minutes
more for the references (2,232 s for the whole seed on 2026-10-29). It does these steps:

1. It creates database `dispatch` and starts Dispatch (log: `$LOADTEST_ROOT/logs/dispatch-seed.log`).
2. `loadgen seed` writes through Dispatch's API with 8 writers (log: `$LOADTEST_ROOT/logs/seed.log`):
   - 4 projects, `LGA`, `LGB`, `LGC` and `LGD`, with 400, 300, 200 and 100 issues.
   - One spec (`spec.md`) per issue. Every fifth issue (20%) gets a 40-50 KB spec with 18-row
     tables. The others get 4-12 KB specs with 4-row tables. Each spec has one `Revision token r0`
     line. Each table row has a unique cell `Requirement <project>-<n>-R<row> holds`, which
     anchored comments quote.
   - 8 comments per issue. Every second comment is anchored to a requirement cell. So each issue
     has 4 anchored and 4 plain root comments.
   - The route of each issue, in key order: issue i goes to session `ses-lg-<i>` (4 digits). Thus
     no step spends its warm-up on route changes.
3. `loadgen seed-refs` adds the `dispatch://` references, with 8 writers (log:
   `$LOADTEST_ROOT/logs/seed-refs.log`), so `GET /api/v1/issues/{key}/references` does the work it
   does in production:
   - 1,000 target documents, `ref-target-<n>.md`, on the 600 issues of `LGB`, `LGC` and `LGD`:
     target n on the n mod 600-th of them, in key order. No step writes a `ref-target-*` document,
     so the targets stay as the seed wrote them.
   - 19,471 versions in all. Each target's version count is drawn from
     `refs-sample-2026-10-27.json`, the per-member version counts of 160 production issues'
     references answers on 2026-10-27. Each target has 1 to 3 writers from a pool of 600, and the
     first of them creates it.
   - Each writer is a session actor whose origin is padded, so its one-actor author list encodes
     to 231 bytes, production's mean per version row.
   - One `references.md` per `LGA` issue i, in key order. It cites as many targets as the sampled
     issue at position i mod 160 had members, each as
     `dispatch://<KEY>/artifact/ref-target-<n>-md`. 310 of the 400 issues get one; the other 90
     cite nothing and get none.
   - The whole plan comes from one generator seeded 650, so every seed writes the same documents
     with the same writers. `refs_test.go` pins it.
4. It waits until the outbox publishes every seed event (at most 900 s). Then it waits until
   Dispatch's block-index backfill owes nothing (`document_block_versions` with a null version and
   `block_grammar_owed` both empty, at most 900 s; a build older than migration 0121 has neither
   table and skips this wait). Then it stops Dispatch. A seed that still owed the backfill would
   give every step about 100 s of it in its window: Dispatch indexes 20 documents every 2 s.
5. It deletes the idempotency keys of the reference writes (`request_keys`), which protect
   retries during the seed only, and checks that none is left. Left in the template, Dispatch's
   sweep of keys older than a day would delete about 19,800 rows inside the window of every step on
   a seed older than that. Then it runs `vacuum analyze` and installs the commit-time trigger
   (`loadgen trigger-sql`).
6. It writes `seed.meta.json` and renames the database to `dispatch_seed`, the template.

The seed content does not depend on the clock. Each issue's text comes from a generator seeded by
its project and number.

`$LOADTEST_ROOT/seed.json` is what `loadgen run` reads: the project keys and, per issue, its key,
project, `artifact_id` (the spec), `spec_name`, `spec_bytes`, `quotes` (the requirement cells),
`anchored_comment_ids`, `plain_comment_ids` and `route`. `loadgen seed-refs` reads the same file
for the issue keys.

`$LOADTEST_ROOT/seed.meta.json` records `migration` (the highest Dispatch migration applied),
`build_commit` and `build_dirty` (the checkout), `dispatch_commit`, `dispatch_binary_sha256` and
`dispatch_build` (the whole of `$LOADTEST_ROOT/bin/envoy-dispatch.json`: the Dispatch that made the
seed, another commit than `build_commit` when `setup` ran with `LOADTEST_DISPATCH_COMMIT`),
`created_at`, `seed_seconds`, `postgres_version` and `counts`:
projects, issues, comments, artifacts, artifact versions, `ref_targets` (the artifacts named like
`ref-target-%`), `ref_target_versions` (their versions), `ref_sources` (the artifacts named
`references.md`), `artifact_refs` (the rows of `refs` that point at an artifact) and events. The
reference plan writes 1,000 targets, 19,471 target versions and 310 source documents. Before the
references, one seed counted 4 projects, 1,000 issues, 8,000 comments, 1,000 artifacts, 1,000
artifact versions and 10,004 events.

### Read the references back

Two commands read what the reference seed wrote. Each needs Dispatch running, so each follows a
reset:

```bash
packages/envoy/loadtest/stack.sh reset
packages/envoy/loadtest/stack.sh loadgen refs-shape
packages/envoy/loadtest/stack.sh loadgen refs-fetch -out $LOADTEST_ROOT/references.tsv
```

`refs-shape` reads every `LGA` issue's references answer, with no query, and prints the members,
version rows and response bytes per issue (p50, p90, max, mean), the share of author lists that
differ and the author bytes per row. It passes when the mean version rows are within 25% of
production's 3,187, the distinct author-list share is in [0.03, 0.12], and the author bytes per
row are within 25% of 231. It prints `PASS` or `FAIL` and exits 1 on a failure.

`refs-fetch` writes one TSV row per seeded issue, in key order: key, status, ETag, body sha256 and
bytes. Two builds' answers compare row by row.

## The reset

Every step starts from the same state, so the before and after runs of a fix start equal.
`stack.sh reset` does these steps:

1. It stops Dispatch and the listener.
2. It drops database `dispatch` and creates it again from the template `dispatch_seed`.
3. It restarts NATS on a fresh JetStream store, then the listener.
4. It starts Dispatch on the step's build and waits up to 120 s until `/healthz` reports `ok` and
   `nats`. If not, it prints the end of the log and exits with status 1.

The generator's revision tokens start at `r0`, because the database is at the seed again.

## Before and after across commits

```bash
packages/envoy/loadtest/stack.sh setup         # build this checkout's Dispatch, listener and loadgen
packages/envoy/loadtest/stack.sh start
packages/envoy/loadtest/stack.sh seed --force  # seed with a build that has loadgen seed-refs
LOADTEST_SRC=<older checkout> packages/envoy/loadtest/run-step.sh step-1x-before \
  --events 4 --sessions 400 --requests 60
packages/envoy/loadtest/run-step.sh step-1x-after --events 4 --sessions 400 --requests 60
```

Seed with a checkout that has `loadgen seed-refs`. The references are part of the seed, and a
loadgen older than them cannot write them: `stack.sh seed` asks that loadgen for `seed-refs -h` and refuses
to drop the template, naming the cause. The references add no Dispatch migration, so a seed with
them sits at the migration level a seed without them sat at, and an older build's steps run on it.

`stack.sh seed` builds nothing. It uses the binaries of the last `setup`, so run `setup` with the
checkout that seeds first. `setup` builds `loadgen` from `LOADTEST_SRC` too, so that checkout must
hold this package.

`run-step.sh` with another `LOADTEST_SRC` prepares the stack for that checkout: it runs `setup`,
which builds that checkout's Dispatch, listener and loadgen into `$LOADTEST_ROOT/bin` over the ones
the seed used, `start` and `seed`, which does nothing while the template and `seed.meta.json` exist.
Then it compares the highest migration file in that checkout
(`packages/envoy/internal/dispatch/store/migrations`) with `seed.meta.json`. It refuses a build
that is older than the seed, and names both levels; a build at the seed's level runs. For a build
that is newer than the seed, it prints a note: a backfill that the new migrations start runs inside
each step of that build. For example, a build at migration 0128 on a seed at 0120 indexes the
blocks of all 1,000 specs in every step. A newer build migrates the restored copy forward when
Dispatch starts; an older build cannot start on a seed that a newer build made. To measure one
build alone, seed with it (`stack.sh seed --force`, with a checkout that has `seed-refs`).
`meta.json` records the build commit and whether the tree had uncommitted changes.

Committed rate at 1x varies from run to run. Across the runs of 2026-10-27 it was 0.34 to 2.14
events/s. The lowest ran a build at migration 0128 on a seed at 0120, with the block-index backfill
inside the step. With a seed made by the same build, eight 1x runs committed 0.74, 1.16, 1.38,
1.63, 1.77, 1.81, 1.83 and 1.95 events/s (median 1.70), each with the commit-order lock held in
99.6-100% of samples and 41-46 waiters (p50), and `box_busy_before` at most 1%. That number sees
only this pod's CPU: other pods on the same node are not in `/proc/stat`. So a before and after
compares several runs of each side (five or more), and their medians, not one run of each.

## The load

### Sessions

Each of N sessions (`ses-lg-0000`, `ses-lg-0001`, ...) works on one issue and watches four more:
session i has issue i mod min(N, 1,000), and the issues 37, 74, 111 and 148 places after it, with
the same wrap. A step with fewer than 1,000 sessions touches only the first N issues in key order.

- Each session registers with the listener as the pi plugin does: `POST /v1/interests/subscribe`,
  `self_subscribed: true`, port 0. It sends a heartbeat every 90 s. The heartbeats of all sessions
  are spread evenly.
- Each session subscribes over core NATS to its agent topic (`notifications.agent.<session>`) and
  to the topics of its five issues (`notifications.dispatch.issue.<key>.>`). The sessions share 50
  NATS connections. `run.log` records the subscriptions (N × 6 expected), the sessions registered
  and the registration errors.

### Event kinds

The event generator sends five kinds of write, each on its real route. Each write carries a
marker, `lgm<nonce><counter>x`: the run's nonce, 9 random decimal digits drawn when `loadgen run`
starts, then the write's counter in the run (1, 2, ...). `summary.json` (`config.marker_nonce`),
`meta.json` (`marker_nonce`) and the first line of `run.log` record the nonce. The run counts only
the commit rows and live frames whose marker carries its own nonce, so a run on a database that
earlier runs wrote to without a reset (`stack.sh loadgen run` resets nothing) still counts only its
own writes.

| Kind | Request | Primary event type | Marker field in the event |
| --- | --- | --- | --- |
| `comment` | `POST /api/v1/issues/{key}/comments` with `body` | `comment.created` | `payload.body` |
| `anchored` | The same, with `anchor: {artifact: "spec", quote}` on a seeded requirement cell | `comment.created` | `payload.body` |
| `reply` | The same, with `reply_to` a seeded root comment: an anchored root with probability 0.77, else a plain root | `comment.created` | `payload.body` |
| `edit` | `POST /api/v1/artifacts/{id}/edits`: one `replace` of `Revision token rN` with `rN+1`, with a `summary` | `artifact.version` | `payload.version.summary` |
| `upload` | `GET /api/v1/artifacts/{id}/text`, then `POST /api/v1/issues/{key}/artifacts` with `name: "spec.md"`, the same text with the token line changed, and a `summary` | `artifact.version` | `payload.version.summary` |

- The primary event types and marker fields were measured from the saved `envelope-*.json` files
  of a live step. The envelope's `payload` field holds the whole Dispatch event as a JSON string.
  The marker field is a path in that event.
- The 0.77 is from production: 1,217 of 1,576 replies were in anchored threads
  (the 2026-10-26 production write counts, §2 of the replay report of those writes).
- A write can add other events with its marker, for example an anchored comment's document
  snapshot or a reply's `comment.reopened`. `summary.json` counts them (`events.other_events`,
  `events.other_receipts`). The harness does not time them.
- Each write goes to one of the five issues of a random session. With probability `--human`, a
  person makes it: the request carries `X-Dispatch-User: person-<n>`, and route copies go to the
  agent topics. Otherwise the session makes it, with the stack's bearer token and
  `actor: {kind: "session", id}`.
- Edits and uploads to one issue run one at a time. The generator tracks each spec's revision
  token. When it does not see the response of an edit or upload, it reads the token from the
  document again (`GET /api/v1/artifacts/{id}/text`).

### Event mix

`--event-mix` is a preset name or `kind=weight,...`. The weights are relative. A kind with weight 0,
or no weight, is not sent. `loadgen run` refuses, before any connection, an unknown kind, a weight
that is not a number, a negative weight, a kind given twice and a mix whose weights are all 0. It
also refuses a mix with `reply` or `anchored` when `seed.json` has no root comment ids or no quotes.

| Preset | Weights | Source |
| --- | --- | --- |
| `production-2026-10-26` (default) | `comment=3,anchored=33,reply=35,edit=22,upload=7` | The 2026-10-26 production write counts for 22:55-23:34Z: 153 plain roots, 1,495 anchored roots, 1,576 replies, 1,329 document versions (§2 of the replay report of those writes). The 3 to 1 split of the versions into edits and uploads is INFERENCE from the production profile's write counts: `editArtifact` 6, `uploadArtifact` 2 (§3 of the production CPU profile of 2026-10-27). |
| `report-2026-10-27` | `comment=50,anchored=30,edit=20` | The root report's mix, so that its 1x row can be compared. |

### Route mix

The request generator sends GET requests at `--requests` per second, picked from a route-mix file.
One route per line. Blank lines and lines that start with `#` are skipped.

```text
<weight> <caller> <identity: person|agent> GET <path template>
22 browser person GET /api/v1/issues/{key}
```

- The weights are relative and must be more than 0.
- `person` sends `X-Dispatch-User: person-<0..6>`. `agent` sends the stack's bearer token.
- A route's name in `summary.json` and `TABLES.md` is `<caller> GET <path template>`.
- Placeholders:
  - `{key}`: the key of a random issue among the issues in use.
  - `{project}`: a random seeded project key.
  - `{artifact_id}`: the spec artifact id of that issue.
  - `{key_ref}`: `dispatch://<key>`, URL-escaped for a query string.

`loadgen run` refuses the file, with its line number, before any request when a line has:

- a field count other than 5,
- a weight that is not a positive number,
- an identity other than `person` or `agent`,
- a method other than `GET` (writes belong to the event generator),
- a path template that does not start with `/`, or has a `{` or `}` without its pair,
- an unknown placeholder,
- the same route as an earlier line.

It also refuses a file with no routes.

The default file, `routes-2026-10-26-1719Z.txt`, is built into the binary. It has 18 routes. Its
source is the request-source capture of 2026-10-26 17:19Z: 60 s of Dispatch's requests,
attributed to their callers. The capture gives the caller
groups: 39% one dashboard tab, 34% `dispatch read` calls, 17% Legion pods, 10% unattributed. The
split inside each group is INFERENCE from the capture's prose. These are the 18 routes of the root
report.

A request that the generator cannot start because 25,000 requests are in flight is dropped and
counted. 25,000 is more than 600 req/s × the 30 s client timeout.

### The dashboard stream

`--live` streams follow Dispatch's event feed as a dashboard tab does. Which request that is
depends on the build under test, and a step measures both kinds:

| The build serves | The step opens | `summary.json` `config.dashboard` |
| --- | --- | --- |
| `GET /ws/live` (one live socket per tab, from the live-socket release on) | `--live` live sockets, each subscribed to the events view | `live` |
| `GET /api/v1/events` (the server-sent-event feed, builds before that release) | `--live` SSE streams | `sse` |

The step probes the build once, before the window and after the guard: it dials `/ws/live`, and,
when the dial is refused (an older build answers 404), it asks for `/api/v1/events` with
`Accept: text/event-stream`. A build that serves neither fails the step before it starts, naming
what each request answered. `run.log` records the choice (`dashboard: <n> <kind> stream(s) on
<url>`). Both readers time every event frame the same way and fill the same `live` fields of
`summary.json` (Delivery from sending, and live socket frames, below), so the frame counts and
latencies of a step on either build are read from one place. A replay step at a commit from
before the live-socket release (`--dispatch-commit`) therefore reports `dashboard: sse`.

## The replay (`-source replay`)

`loadgen run -source replay` replaces the request generator with a replay bundle's logged writes:
LINUX's real write log of 2026-10-26 22:55:00Z-23:34:00Z, re-sent at the log's offsets from
22:55:00 as the dispatch CLI 10.25.1 sends each write, with read actions before each
write at a stated assumption (no read was logged). It runs against a stack whose Dispatch database
holds the restored replay seed (`stack.sh replay-seed`'s dump), whose `loadtest.replay_ids` maps
every logged comment, artifact, ask and issue to its local id; `loadgen run` reads that table once,
over the stack's Postgres URL, before the window.

```bash
# One replay step (setup, start, replay-get, replay-prepare, reset --replay, loadgen run, tables).
# LOADTEST_REPLAY_ISSUE names the issue the bundle's and the seed's parts are file artifacts of:
LOADTEST_REPLAY_ISSUE=<issue> packages/envoy/loadtest/run-step.sh replay-f0.05 --replay --rate 0.05
# The same on Dispatch at production's commit of 2026-10-26, Dispatch on one core:
LOADTEST_REPLAY_ISSUE=<issue> packages/envoy/loadtest/run-step.sh replay-f0.05-35ef --replay --rate 0.05 --dispatch-commit 35efd326 --gomaxprocs 1
# What a run would send, without a stack (counts only; no issue, it downloads nothing):
packages/envoy/loadtest/stack.sh loadgen replay-plan -replay-inputs "$LOADTEST_ROOT/replay/bundle" -replay-rate 0.05
```

**The bundle** (`loadgen replay-fetch`, read-only from a Dispatch; `replay_bundle.go`) is LINUX's
state at 22:55:00Z and its writes up to 23:34:00Z:

| File | What it holds |
| --- | --- |
| `state.json` | Every issue number up to the largest created before 22:55 (a placeholder for a number with no issue then), with each issue's fields, claim, artifacts (each document's listed version sizes, `version_sizes`), open and resolved comments with their anchors (`block_id`, quote), the asks open or answered at 22:55, the resolved typed ask blocks still in their document's text (with the resolution), and the version each typed ask block's answer or resolve landed at; `block_maps`, the current blocks of every document an anchor in the state or the window names. |
| `docs/<id>.md` | Each document's markdown at 22:55 (the nearest readable lower version when that one is not). |
| `window.jsonl` | The window's events as served. |
| `created-docs/<id>.md` | Version 1 of each document the window creates, the created issues' specs among them (`created_specs` maps each created issue to its spec). |
| `roster.json` | The session ids the state and the window name. |
| `bundle.meta.json` | The project, the instant, the window's end, `fetched_at`, `counts` and every other file's sha256. |

The 2026-10-26 bundle (`replay-2026-10-26.json`'s `bundle.counts`) holds 1,047 issues, 1,760
documents (81 MB of markdown), 28 files, 18,899 open comments (10,350 of them anchored), 68 open or
answered asks, 8 resolved typed ask blocks, 5,379 window events, 65 created documents, 109 created
specs, 385 block maps and a roster of 452 sessions. No document version lacks a size.

**The seed** (`stack.sh replay-seed --bundle DIR`, `loadgen replay-seed`) recreates the bundle's
state through Dispatch's API with Dispatch at 35efd326, production's commit on 2026-10-26
(migration 0114). The dump is at a migration no newer than main's, so every later commit migrates
it forward in `replay-prepare`. Every issue is recreated at its number, and every logged id is
mapped to its local one in `loadtest.replay_ids` as it goes, so a killed seed resumes
(`--resume`). Each anchor's rule is recorded in `loadtest.replay_anchors`. What it writes:

- **History at real sizes** (`planHistory`). A document gets production's version count, and
  version n changes as many bytes as production's version n did. Each version before the restore
  version is a base (the text at 22:55 with a run of sections removed and the typed ask blocks
  still to be answered or resolved left open) plus a padding paragraph of synthetic words sized to
  production's size. The restore version removes the padding and puts the run back, and the typed
  ask blocks' answers and resolves land at their logged versions, so the last version is the text
  at 22:55. Where the base cannot be made small enough, a version changes by production's change
  with the sign flipped: 21 versions, all on documents with typed ask blocks (`sign_flipped`).
- **Request bodies under Dispatch's 1 MiB.** Dispatch refuses a JSON body over 1 MiB (413
  `REQUEST_TOO_LARGE`), and padding paragraphs run to hundreds of KB, so no edit carries one
  whole. The paragraph is a prefix of a stream of words that each occur once in the document
  (`padAt`): a version that grows by d appends the stream's next d bytes, one that shrinks by d
  drops the last d, and one whose size does not change swaps its last word for a same-length one
  (`padSwap`), each as a replace of the paragraph's tail from the last whole word before the
  change (`padEdit`). A padding edit's body is its change plus about 130 bytes, so the largest
  single version change bounds it. The restore deletes the paragraph by its block id and
  re-inserts the run whole in that one edit (splitting it would add versions), so the run is
  chosen among the runs whose JSON-encoded markdown fits 1 MiB less 1 KiB (`chooseRun`); a
  document whose needed run does not fit gets the longest run that does, and its versions out of
  reach flip sign or are raised as before. In the 2026-10-26 bundle the bound changes no
  document's run.
  `TestRequestBodiesBundle` (with `LOADTEST_REPLAY_BUNDLE=<bundle dir>`, like
  `TestPlanHistoryBundle`) plans every body the seed sends and every one `replay-plan` plans at
  rate 1, fails on a body or a planned document version over 1 MiB, and logs the largest body of
  each kind. Largest in the 2026-10-26 bundle: a padding edit of 623,266 bytes (a 623,138-byte
  version change), a restore of 637,584 bytes, a replay upload of 665,020 bytes.
- **Comments.** Every open thread in creation order, and every resolved thread a window event
  refers to (created, then resolved). The other resolved threads are not recreated.
- **Anchors** (`chooseAnchor`). Most logged blocks no longer exist in today's block maps, so an
  anchor lands by the first rule that applies. `bundle.meta.json`'s `anchor_rules` counts the
  state's and the window's anchors by rule:

| Rule | Anchors | Where the anchor lands |
| --- | --- | --- |
| `logged_quote` | 4,110 | On the logged quote, which is not orphaned and occurs once in the text at 22:55. |
| `in_logged_block` | 960 | On a sentence that is unique in the text and lies inside the logged block, whose text occurs once in the text; the sentence is drawn by `fallbackOrder` from the rng seed, the document id and the item id. |
| `nearest_surviving_block` | 7,002 | On such a sentence in the nearest block, in current order, whose text occurs once and holds one, measured from the logged block (else from the block holding the logged quote, else from the quote's relative position). |
| `no_text` | 4 | Not counted by a rule: the document has no text at 22:55. |

`stack.sh replay-seed` then runs `loadgen replay-check-seed`, which compares the seeded database
with the bundle, and dumps the database only when the check passed. Its file is kept beside the
dump as `seed-check.json`, and in `seed.meta.json` and the manifest's `seed.check` without the
`*_list` fields:

| Field | What it is |
| --- | --- |
| `documents` | Documents checked. |
| `differences` | Differences from the bundle: a document with no local one, another version, another number of open anchored items, or another text. |
| `version_size_mismatches` | Versions whose stored size change is neither production's nor its negative. |
| `anchors_outside_logged_block` | Anchored items with no recorded rule or stored quote, or whose stored quote is not unique in the text or lies outside the span its rule chose. |
| `anchor_rules` | `logged_quote`, `in_logged_block`, `nearest_surviving_block`: anchors seeded by each rule. |
| `sign_flipped` | Versions whose size change is production's with the sign flipped. |
| `passed` | `differences`, `version_size_mismatches` and `anchors_outside_logged_block` are all 0. |
| `anchor_rules_all` | Every rule's count, `no_block_map` and `unanchorable` included. |
| `difference_list`, `version_size_mismatch_list`, `anchors_outside_list` | The lists, ids and numbers only. |

**The len16 patch.** The seed's Dispatch carries `replay-seed-len16.patch`, an allocation-free
`len16` (a string's length in UTF-16 units) that counts what 35efd326's
`len(utf16.Encode([]rune(s)))` counts, so the seed runs faster and persists the same state.
`stack.sh replay-seed-patch-check BUNDLE_DIR` runs the patch's tests (`TestLen16Seed`) with every
`*.md` under the bundle as the corpus. `--stock` seeds with the unpatched build
(`bin/dispatch-seed-stock`). Stock and patched seeds persist the same state when their logical
dumps are equal; `replay-logical-dump` masks every uuid and timestamp:

```bash
packages/envoy/loadtest/stack.sh replay-seed --bundle BUNDLE --keys-file KEYS --stock --out "$TMPDIR/seed-stock"
packages/envoy/loadtest/stack.sh loadgen replay-logical-dump -db dispatch_replay_build -out "$TMPDIR/stock.dump"
packages/envoy/loadtest/stack.sh replay-seed --bundle BUNDLE --keys-file KEYS --out "$TMPDIR/seed-patched"
packages/envoy/loadtest/stack.sh loadgen replay-logical-dump -db dispatch_replay_build -out "$TMPDIR/patched.dump"
cmp "$TMPDIR/stock.dump" "$TMPDIR/patched.dump"
```

**Running a replay step.** `LOADTEST_REPLAY_ISSUE=<issue> run-step.sh <name> --replay [--rate F]
[--reads RHO] [--read-mix M] [--uploads U] [--rng-seed N] [--from D --to D] [--roster N]
[--keys-file FILE] [--warmup D] [--gomaxprocs N] [--dispatch-commit REV] [--manifest FILE]
[--out DIR]` runs, in order (`LOADTEST_REPLAY_ISSUE` names the issue the manifest's parts are file
artifacts of; a `--replay` step without it is refused before anything starts):

1. `stack.sh setup` (incremental). `--dispatch-commit REV` builds `envoy-dispatch` alone from REV
   (`LOADTEST_DISPATCH_COMMIT`); a REV that names no commit is refused before anything starts.
2. `stack.sh start`. The synthetic seed (`dispatch_seed`) and its migration check are not used.
3. `stack.sh replay-get --manifest FILE` (default `replay-2026-10-26.json`), which downloads the
   parts from `LOADTEST_REPLAY_ISSUE`; once done, it only checks the sha256 of the parts and the
   bundle's files.
4. `stack.sh replay-prepare`: this build's template, made once per build.
5. `stack.sh reset --replay`, whose `last-reset.json` it copies into the step directory.
6. `loadgen run -source replay -events 0 -requests 0 -replay-inputs $LOADTEST_ROOT/replay/bundle`,
   with `--rate`, `--reads`, `--read-mix`, `--uploads`, `--from`, `--to`, `--roster` and
   `--keys-file` as `-replay-rate`, `-replay-reads`, `-replay-read-mix`, `-replay-uploads`,
   `-replay-from`, `-replay-to`, `-replay-roster` and `-replay-keys-file`, then `loadgen tables`.

`--events`, `--sessions`, `--requests`, `--duration`, `--grace`, `--event-mix`, `--route-mix`,
`--human` and `--live` are refused with `--replay`. A replay step opens 2 dashboard streams: the
live sockets of the build under test, or its SSE streams when it serves no `/ws/live` (The
dashboard stream, above). `summary.json` `config.dashboard` records which ran, and `config.live`
their count. The flags `run-step.sh` keeps for itself:

| Flag | Default | What it sets |
| --- | --- | --- |
| `--rng-seed N` | `1` | `loadgen run -rng-seed`: the issue sampling, the reads, the upload split and every substitute (Planning, below). |
| `--warmup D` | `30s` | The warm-up before the window: the roster registers, the dashboard streams open, no replay request. |
| `--gomaxprocs N` | nproc | `LOADTEST_GOMAXPROCS`, Dispatch's `GOMAXPROCS`. |
| `--dispatch-commit REV` | the checkout | `LOADTEST_DISPATCH_COMMIT`: `envoy-dispatch` alone is built from REV; `loadgen`, the listener and the manifest's seed stay the same. Every step's `meta.json` records that build as `dispatch_commit`, `dispatch_binary_sha256` and `dispatch_build`, beside the checkout's `build_commit`. |
| `--manifest FILE` | `replay-2026-10-26.json` | The manifest `replay-get` reads. |
| `--out DIR` | `$LOADTEST_ROOT/runs/<name>` | The step directory. |

Dispatch reaches Postgres directly (`LOADTEST_PG_DIRECT=1`) unless the environment sets
`LOADTEST_PG_DIRECT`; `meta.json`'s `pg_direct` records it.

**The manifest** (`replay-2026-10-26.json`, schema 1) names no URL, host or issue. It holds
`project`, `window` {`from`, `to`}, `bundle` {`tar_zst_sha256`, `bytes`, `fetched_at`,
`counts` (`bundle.meta.json`'s), `parts`} and `seed`, null or {`dump_sha256`, `bytes`,
`compression`, `migration`, `dispatch_commit`, `dispatch_tree`, `patch`, `patch_sha256`,
`go_version`, `postgres_version`, `seed_seconds`, `keys_file`, `counts`, `check` (the seed check
without its `*_list` fields), `parts`}. Each part is {`file`, `slug`, `version`, `bytes`,
`sha256`}; the slug is the file name lower-cased with every character outside `[a-z0-9]` a `-`,
and the issue those parts are file artifacts of is the operator's `LOADTEST_REPLAY_ISSUE`, not the
manifest's. A manifest whose `seed` is null gets the bundle alone: `replay-plan` runs on it;
`replay-prepare` refuses, as no seed was restored.

**Rebuilding the bundle and the seed.** Both are fixed inputs: rebuild them for another window, or
when the bundle's format changes, then commit the new manifest. `replay-fetch` reads
`DISPATCH_URL` and `DISPATCH_TOKEN_FILE` from its own environment, so it runs as the bare binary,
not through `stack.sh loadgen`:

```bash
# 1. The bundle, read-only (-concurrency 1 to 4; every response is cached under BUNDLE/raw/, so a rerun resumes):
"$LOADTEST_ROOT/bin/loadgen" replay-fetch -project LINUX -at 2026-10-26T22:55:00Z -until 2026-10-26T23:34:00Z -out BUNDLE
# 2. Its deterministic zstd tar parts of at most 24 MiB, with their sha256 in pack.json:
"$LOADTEST_ROOT/bin/loadgen" replay-pack -in BUNDLE -out PARTS/linux-replay-2026-10-26.tar.zst >pack.json
# 3. The seed, on a started stack: the len16 patch's tests on the bundle, then the seed and its check,
#    which writes the dump's parts, seed-check.json and seed.meta.json under SEED_OUT:
packages/envoy/loadtest/stack.sh replay-seed-patch-check BUNDLE
packages/envoy/loadtest/stack.sh replay-seed --bundle BUNDLE --out SEED_OUT
# 4. Each part (PARTS/*.part*, SEED_OUT/*.dump.part*) as a file artifact of the replay's issue:
dispatch artifact --issue <issue> --name linux-replay-2026-10-26.tar.zst.part00 --path PARTS/linux-replay-2026-10-26.tar.zst.part00
# 5. The manifest (without --seed-dir its seed is null; rerun with it once the seed is uploaded):
packages/envoy/loadtest/stack.sh replay-manifest --bundle-pack pack.json --bundle BUNDLE --seed-dir SEED_OUT \
  >packages/envoy/loadtest/replay-2026-10-26.json
```

`replay-manifest` writes `--part-version N` (default 1) as every part's `version`. A part uploaded
again under the same name is a new version of the same artifact: pass the version its upload made
(the 2026-10-26 bundle's parts are at version 2, so `--part-version 2`). The seed's wall time is
`seed_seconds` in `seed.meta.json` and in the manifest's `seed`.

**`stack.sh replay-get`** refuses before it reads or downloads anything unless
`LOADTEST_REPLAY_ISSUE` names the issue the parts are file artifacts of, naming the variable. It
keeps each part at `$LOADTEST_ROOT/replay/parts/<file>` when its sha256 matches; else it copies it
from `--from-dir DIR` or downloads `GET
$DISPATCH_URL/api/v1/issues/$LOADTEST_REPLAY_ISSUE/artifacts/<slug>/versions/<version>` with the
bearer token in `DISPATCH_TOKEN_FILE`, from the caller's environment (curl reads the header on its
standard input; the token is never printed). A part whose sha256 differs is deleted and refused,
naming the part and both hashes. Then the joined bundle parts must match `tar_zst_sha256`; they
are unpacked into `$LOADTEST_ROOT/replay/bundle` (not again while `replay/bundle.unpacked.json`
names the same parts and the bundle's `bundle.meta.json`), and `loadgen replay-verify-bundle`
checks every file. The joined seed parts must match `dump_sha256`; the dump is restored once
(`pg_restore --no-owner --no-privileges -j 2` into a fresh database) into the template
`dispatch_replay_seed`, recorded in `replay/seed-restored.json` {`seed_dump_sha256`, `migration`,
`restore_seconds`, `postgres_version`, `restored_at`, `check_passed`, `dispatch_commit`}. The same
dump is not restored again; another one drops `dispatch_replay_seed` and every prepared template
first. A null seed restores nothing. The pilot's 11 MB dump restored in 3 s.

**`stack.sh replay-prepare`** keys the template by the first 16 hex of `bin/envoy-dispatch.json`'s
`dispatch_binary_sha256`. It refuses, before anything starts, a build whose `newest_migration` is
older than the seed's migration (naming both) and a seed whose check did not pass. A template
recorded complete for the same seed dump is reused. Otherwise it creates `dispatch_replay_<key>`
from `dispatch_replay_seed`, restarts NATS on a fresh store and the listener, starts that build's
Dispatch on it directly (it migrates the copy forward; `/healthz` within 1,800 s), waits for the
block-index backfill (`document_block_versions` with a null version and `block_grammar_owed`
empty, when they exist; at most 3,600 s, logged every 30 s), then `wait_quiet` (no owed work, then
below 0.05 cores over 30 s), stops Dispatch and runs `vacuum analyze`. It installs no commit-time
trigger. `replay/prepared/<key>.json` records `key`, `dispatch_commit`, `dispatch_binary_sha256`,
`seed_dump_sha256`, `migration_before`, `migration_after`, `prepare_seconds`,
`backfill_wait_seconds`, `quiet_wait_seconds`, `created_at` and `last_used_at`. The three most
recently used templates are kept; older ones are dropped with their records. On the pilot seed
(20 seeded issues), the checkout's build (migration 114 to 129) took 152 s, 117 s of it the
backfill and 31 s the quiet wait; 35efd326 (114 to 114) took 34 s.

**`stack.sh reset --replay`** recreates database `dispatch` from this build's prepared template
(refused without one: run `replay-prepare`), then, before Dispatch starts, writes
`replay/last-reset.json` {`template`, `key`, `seed_dump_sha256`, `checksum` (`loadgen
replay-checksum -db dispatch`), `migration`, `prepared` (the template's record), `backfill_empty`
(the block index owes nothing, or its tables are absent), `reset_at`}. Two resets from one template
give one checksum.

The flags of `loadgen run -source replay`:

| Flag | Default | What it sets |
| --- | --- | --- |
| `-replay-inputs` | (required) | The bundle directory. |
| `-replay-rate` | `0.05` | The rate factor F, more than 0 and at most 1: the share of issues replayed, each issue kept or dropped whole (`keepIssue` of `-rng-seed` and the key; an issue created in the window is its own unit). Times are not scaled. F 0.1 and 0.15 were flagged saturated on the 2-core pod, so the default is 0.05. |
| `-replay-reads` | `1` | Read actions before each write, rho; a fraction is a Bernoulli draw per write. |
| `-replay-read-mix` | `read=0.55,docread=0.30,commentref=0.10,search=0.05` | The read actions' kinds and weights. |
| `-replay-uploads` | `0.25` | U: the share of named versions with a diff re-sent as a whole-document upload (the rest are edits), drawn per event from `-rng-seed`. |
| `-replay-from`, `-replay-to` | `0`, `39m` | The sub-window, from 22:55:00: events with from <= offset < to. |
| `-replay-roster` | `762` | Sessions registered with the listener: the bundle's roster ids it accepts as a session key, then `ses-replay-<n>`. |
| `-replay-keys-file` | (none) | Replay only the issues the file lists (as `replay-seed -keys-file`). |

With `-source replay`, `-events 0 -requests 0` are required, `-sessions` is refused (the roster is
the sessions), `-duration` is ignored and set to to - from, the warm-up stays, and `-live` defaults
to 2. Every bad input is refused by name before any connection; a `-replay-*` flag without
`-source replay` is refused too.

**Planning** is pure: one bundle, one set of flags, one `-rng-seed` and the document texts the
build under test serves (The client's text is the build's, below) give one plan, which
`loadgen replay-plan` prints as JSON counts (actions, requests per route, writes and reads per kind,
events not sent by reason, substitutes). Every random choice is a hash of the rng seed and the
choice's own key (an issue key, an event id, a document or item id), never a generator shared
between senders: the issue sample (`keepIssue`), the upload split, each write's read count and each
read's kind, document, comment and search words, substitute quotes (`fallbackOrder`), substitute
edits, synthetic specs and documents, file bytes and their multipart boundary. The plan is built
in log order before the window, and so is the client's copy of each document, so every edit,
upload and quote is fixed before anything is sent. At send time only the ids of the objects the
run creates, the events cursor (`last_seq`) and an ask's `edited_at` come from responses, and the
roster is the bundle's session ids then `ses-replay-<n>`. Two runs of one bundle, flags and rng seed
against one build send the same requests, action by action, whatever the timing
(`TestReplayDeterministic`); another rng
seed sends other content. They end with the same anchors and versions when Dispatch takes the
writes to each document in log order and none fails, so each document's writes are sent in plan
order (Sending, below): `TestReplayDeterministic` starts each document's writes in reverse, against
a fake Dispatch with random delays, and checks that both runs apply them in log order. A write that
fails (`writes.failed`) can still change what the later writes to its document store, or make them
fail. `replay.end_state` records what the run left (The record, below), so two runs can be compared.

A logged write is one transaction: the events of one actor id at one `created_at`. Each becomes
one action of one kind:

| Logged write | Kind | Requests (`->` in order, `\|\|` in parallel) |
| --- | --- | --- |
| anchored root comment | `anchored` | `GET /issues/{key}` -> `POST /issues/{key}/comments` with `anchor {artifact, quote}`: the logged quote when the client's current text holds it exactly once, else `substituteAnchor`'s (the seed's rules, The seed above, on the client's current text; counted in `substitutes.quote`) |
| reply | `reply` | `POST /issues/{key}/comments` with `reply_to` |
| plain root | `plain` | `POST /issues/{key}/comments` (with `ask_id` on an ask) |
| named version with a diff, by a session | `upload` (share U) or `edit` | `POST /issues/{key}/artifacts {name, content, summary}` with the client's whole text after the edit; or `GET /issues/{key}` -> `POST /artifacts/{id}/edits {ops, summary}` |
| unnamed version without a diff, by a session | `edit-unnamed` | `GET /issues/{key}` -> `POST /artifacts/{id}/edits {ops}` |
| named version without a diff, or by a person | not sent | server settlement, or a browser |
| status change by a session | `status` | `GET /issues/{key}` -> `PATCH /issues/{key}` |
| issue update by `legion-daemon:*` (an `issue.closed` with no `issue.updated` in its transaction included) | `daemon` | `GET /issues/{key}` -> `PATCH /issues/{key}` |
| other issue update | `fields` | `GET /issues/{key}` -> `PATCH` with the fields that changed from the previous snapshot (title, status, labels, priority, route, assignee, parent, blocked_by, repo, external_links); a write that changed none of them (a rank, claim or components change) sends the unchanged status and is counted `patch_unchanged` |
| message by the daemon | `daemon-message` | `GET /issues/{key}/events?order=desc&limit=200` -> `POST /issues/{key}/messages` |
| message by a session | `message` | `POST /issues/{key}/messages` |
| issue created | `issue-create` | `POST /issues {project, title, parent, spec, force}` -> `GET /projects/LINUX/architecture-sources` (404 at 35efd326, which does not fail the action) -> `GET /projects/LINUX/architecture-source`; the spec is its `created-docs/` version 1 (`bundle.meta.json`'s `created_specs`), else synthetic, of its first logged version's size, else of the window's created documents' median first version (counted in `spec_sizes`) |
| markdown artifact created | `doc-create` | `POST /issues/{key}/artifacts` with its `created-docs/` version 1 |
| file artifact created, file version | `file-create`, `file-version` | multipart `POST /issues/{key}/artifacts` with deterministic bytes of the logged size |
| comment resolved | `resolve` | `POST /comments/{id}/resolve` |
| ask opened, answered, resolved; issue claimed | `ask-open`, `ask-answer`, `ask-resolve`, `claim` | `POST /issues/{key}/asks`; `GET /asks/{id}` -> `POST /asks/{id}/answer` as the answering person (`X-Dispatch-User`, with the ask's `edited_at`); `POST /asks/{id}/resolve`; `POST /issues/{key}/claim` |
| `child.status`, `comment.anchor_refreshed`, `comment.reopened`, `issue.closed` beside its `issue.updated`, `message.delivery`/`accepted`, `user_state.*` | not sent | derived by the server |

All paths are under `/api/v1`. The client keeps each document's text (the build's own text, below;
its own edits; the created documents), so an upload sends the current text with no read. Every edit
holds to what `POST /artifacts/{id}/edits` accepts at Dispatch 35efd326 (`replay_edit.go` names
the code): a `find` or an insert's anchor is matched once against the document's rendered text,
inside one textblock, so it holds no list marker, escape, table pipe or line break (else `404
TARGET_NOT_FOUND`, `400 TARGET_SPANS_BLOCKS`, `409 TARGET_AMBIGUOUS`), and a `with` is inline (two
paragraphs, or the block's own marker, is `400 INVALID_OP`). So each op replaces a plain run of one
line (outside code spans, emphasis, links, autolinks and inline HTML, in no fence or directive) by
plain text that is never empty. An edit is the logged diff (`edits.diff`) when the diff is complete
(not cut at 4,094 characters), the client's text is the version it was taken from (each context and
removed line is where its hunk says), and every run of changed lines is either as many lines
changed as removed, each a `replace` of the changed words widened a word at a time until unique, or
a paragraph added between two blocks, an `insert` of it after the one-line paragraph or heading
before it, anchored on a unique plain run of that line; else `replace`s of unique sentences by
synthetic words that change the length by the logged size change, as many sentences as a shrink
takes (`edits.substitute`). A version with no usable sentence is not sent (`no_edit`). Dispatch then
holds the client's text byte for byte, but for an answered ask block's `answered_at`, which the
seed set when it answered.

**The client's text is the build's** (`readBaseTexts`, `replay.base_docs`). The bundle holds the
markdown the Dispatch it was fetched from served, and a build renders a stored document its own
way, so a newer build can serve another text for the same document, and can refuse the older one:
markdown it stores round-trips, markdown an older build rendered need not. At Dispatch 8a911ecc one
of the LINUX bundle's documents is such a text. Its stored table row holds a code span with an
unescaped `|`; 35efd326 served that row as it was and read it back with the cells past the table's
width dropped, while 8a911ecc renders the same row with the pipe escaped outside the code span and
refuses the older form (`400 INVALID_MARKDOWN`, "a table row holding 7 cells where its table has
5"). Planned against the bundle's text, every upload of that document is refused by that build and
by no other, which measures the two renderings instead of the build. So, after
`loadtest.replay_ids` is read and before the window opens, the run reads each document the plan
modelled (`GET /artifacts/{id}/text`, the agent bearer) and, for every text the build serves
otherwise than the bundle holds it, plans again from the build's text. Where the build serves the
bundle's text, every document included, nothing is replanned and the plan is byte for byte the one
`loadgen replay-plan` prints. Both the reads and the replan happen before the window starts
(`prepareReplay`), so they cost the measurement nothing, and a document the build will not serve
keeps the bundle's text and is counted `unread`; a plan no document of which can be read is
refused.

**Reads** (the stated assumption): before each write, rho read actions on its issue (on the parent
for a created issue), read j of n sent (n - j) s before the write, never before the sub-window:

| Read | Requests |
| --- | --- |
| `read` | `GET /issues/{key}` \|\| `GET /issues/{key}/references` -> `GET /issues/{key}/events?after=<last_seq-10>&limit=10` -> `GET /references?to=dispatch://KEY` \|\| `?from=...` (\|\| `GET /agents` when a session holds the issue's claim) |
| `docread` | `GET /issues/{key}` -> `GET /artifacts/{id}/text` \|\| `GET /issues/{key}/comments?artifact={id}` |
| `commentref` | `GET /comments/{id}` -> `GET /references?to=<comment ref>` \|\| `?from=...` |
| `search` | `GET /search?q=<two synthetic words>&project=LINUX&limit=20&offset=0` |

A `docread` or `commentref` on an issue with no document or comment is a `read`. The reads before
a write are planned before it: they see the claim before the write, and pick only the comments and
documents that exist at 22:55 or whose creating write is sent before them, so no read waits on a
later write.

**Sending**: each action starts at window start + (offset - from) on its own goroutine, on its
own connection (a fresh transport, as each CLI call is a new process), under one 60 s deadline from
that time over all its requests, with no retry; its first failed request ends it. A write on a
document (an anchored root; a reply or resolve in an anchored thread, whose projection writes the
root's document; an edit, upload or create; an anchored ask; a typed ask's answer or resolve) sends
its reads at their times, then, before its write, waits until the previous planned write on that
document has returned, succeeded or failed. The wait counts inside the 60 s deadline, and the
write's latency then counts from its scheduled time, so a slow Dispatch still shows as latency and
saturation (`doc_order_waits`, `doc_order_wait_s`). Each request is one record of
the Requests table, named `<method> <pattern> [<kind>]`. Replay writes carry no marker, so the
delivery columns stay empty. A logged id resolves to the response of the run's own action that
created it (waited for) or to `loadtest.replay_ids`; an action naming an object that neither has
(a reply to a comment whose creation failed, say) is not sent and is counted in
`skipped_missing_dep` by kind. The roster replaces the `ses-lg` sessions: the bundle's roster ids
the listener accepts as a session key (the NATS KV key alphabet; an actor id such as
`legion-daemon:LINUX` is no session id and is skipped, `roster_skipped`), then synthetic ids up to
`-replay-roster`. Each registers with a title, a directory and its home issue's topic (the issue
its claim held at 22:55, else one of the kept issues) plus further issues' topics until its
registration is about 458 bytes, and keeps the 90 s heartbeat; each subscribes the same topics on
NATS. A request the action expects to fail (the issue create's `GET
/projects/{project}/architecture-sources`, which 404s at 35efd326 as it does for the CLI) does not
end its action; summary.json counts it under the route's `tolerated`, not its `failures`. Every
route's non-2xx answers are counted by `<status> <code>` in `codes`, the code read from Dispatch's
error body.

**The record.** At the window start: one read of Dispatch's `/proc/<pid>/stat` (utime + stime),
then `heap-start.pprof` (`/debug/pprof/heap`, no GC), then the first write. At its end:
`heap-end.pprof`, then a `/proc` read, then the CPU profile's end, the goroutine dump and
`heap.pprof`. After the drain, while Dispatch still runs, the end state of every document the run
wrote is read into `end_state.json`: its version, size and anchors over the stack's Postgres URL,
and where each anchor's block stands in the document over the run's own Dispatch client (`GET
/api/v1/artifacts/{id}/blocks`, the agent bearer). In the step directory,
`go tool pprof -sample_index=alloc_space -diff_base=heap-start.pprof -top envoy-dispatch heap-end.pprof`
gives the bytes allocated in the window alone, by function, where `envoy-dispatch` is the step's
Dispatch binary (`$LOADTEST_ROOT/bin/envoy-dispatch` until the next `stack.sh setup` rebuilds it;
`meta.json`'s `dispatch_build` names it by sha256). `summary.json` `replay`:

| Field | What it is |
| --- | --- |
| `inputs` | `rate_factor`, `reads_per_write`, `read_mix`, `upload_share`, `rng_seed`, `from_s`, `to_s`, `bundle_sha256` (of `bundle.meta.json`), `seed_dump_sha256` (from `$LOADTEST_ROOT/replay/last-reset.json`, else `""`), `gomaxprocs` (Dispatch's, from its environment, else `LOADTEST_GOMAXPROCS` or nproc), `dispatch_commit` (from `$LOADTEST_ROOT/bin/envoy-dispatch.json`) |
| `window_s` | Between the two `/proc` reads. |
| `cpu_source` | `proc`, or `none` with no Dispatch pid (`-allow-foreign`); the CPU fields are null then. |
| `dispatch_cpu_s` | Dispatch's CPU-seconds over the window. |
| `cores_full_rate` | `dispatch_cpu_s / window_s / rate_factor`. |
| `idle_cores_before` | Dispatch's cores over the 30 s before the window (the warm-up: the roster's heartbeats and the dashboard streams, no replay request). |
| `cores_full_rate_idle_corrected` | `(dispatch_cpu_s - idle_cores_before × window_s) / window_s / rate_factor`. |
| `drain_cpu_s` | Dispatch's CPU from the window's end until every action returned (at most 300 s). |
| `writes` | By kind: `planned`, `sent`, `failed`. |
| `skipped_missing_dep` | Actions (reads too) not sent for a missing object, by kind. |
| `substitutes` | `quote`: anchored roots sent with a substitute quote; `edit`: versions (edits and uploads) whose content is a substitute. |
| `edits` | Edit requests by derivation: `diff`, `substitute`. |
| `inflight_at_end`, `timeouts` | Actions in flight at the window end; actions that hit the 60 s deadline. |
| `saturated`, `saturated_reasons` | Saturated when Dispatch's 5 s cores reach 0.9 × `saturation_cores` in at least 10% of the window's samples (`dispatch_cpu`), an action timed out (`timeouts`), or more than 20 actions were in flight at the end (`inflight_at_end`). |
| `saturation_cores`, `saturation_cores_from` | The cores the CPU criterion was measured against: the least of Dispatch's `GOMAXPROCS`, the box's nproc and the cgroup's CPU quota in cores, and which of the three set it (`gomaxprocs`, `nproc`, `cgroup_cpu_max` on a cgroup v2 box or `cgroup_cpu_quota` on a cgroup v1 one). `--gomaxprocs 3` on a 2-core pod is measured against 2 cores, not 3. |
| `roster` | Sessions registered. |
| `roster_skipped` | Bundle roster ids the listener refuses as a session key, replaced by synthetic ids. |
| `doc_order_waits` | Writes that found the previous write on their document still running and waited for it (Sending, above). |
| `doc_order_wait_s` | Those waits in seconds: `n`, `p50`, `p90`, `p99`, `max`. |
| `base_docs` | {`read`, `unread`, `differed`, `replanned`, `documents`}: the documents the plan modelled whose current markdown the build served and those it did not, how many it serves otherwise than the bundle holds them (their logged ids in `documents`, sorted), and whether the plan was built again from those texts (The client's text is the build's, above); null when the plan modelled no document. |
| `end_state` | {`documents`, `sha256`}: the documents the run wrote and the sha256 over one sorted line per document (its logged artifact id, its highest version number, that version's size, and, sorted, the sha256 of the quote and the block index of each unresolved anchored root comment on it). A block index is where the anchor's block stands in the document: `<i>` for the i-th top-level block, `<i>.<r>` for its r-th table row, `<i>.<r>.<c>` for that row's c-th cell, `-1` for a block id the document no longer holds. The digest covers the block index, not the block id, because Dispatch mints a fresh uuid for every block a write creates, so two runs that did the same work differ in their block ids. `end_state.json` in the step directory holds the lines' fields (ids, numbers and hashes, no quote text), keeps each anchor's `block_id` for debugging, and names the digested fields in its `digest` field. Null when a read failed (`-allow-foreign` with no Postgres or no API reachable). |

`meta.json` `replay` holds the same `inputs`, `cgroup_cpu_max`, `cgroup_cpu_quota`,
`heap_start_at`, `heap_end_at` and `last_reset` (`replay/last-reset.json`: the template,
`seed_dump_sha256`, `checksum`, ...); `nproc`, `build_commit` and `pg_direct` are `meta.json`'s
own. The cgroup's CPU quota is read from whichever hierarchy the box mounts:
`cgroup_cpu_max` is `/sys/fs/cgroup/cpu.max` (`<quota> <period>` in microseconds, `max` for no
quota) and is `""` on a cgroup v1 box, which has no such file; `cgroup_cpu_quota` is the quota
from either hierarchy, `{version, quota_us, period_us}` with `version` 2 for `cpu.max` and 1 for
`/sys/fs/cgroup/cpu/cpu.cfs_quota_us` and `cpu.cfs_period_us`, `quota_us` `-1` when no quota is
set, and null when neither hierarchy's files are there. The Dispatch that ran the step is
`meta.json`'s own top-level `dispatch_build` (the build JSON: `dispatch_commit`, `dispatch_tree`,
`dispatch_pprof_overlay`, `dispatch_binary_sha256`, `go_version`, `newest_migration`), with
`dispatch_commit` and `dispatch_binary_sha256` beside it, written once for every step, replay or
not (The step directory, below); `replay.inputs.dispatch_commit` names the same commit.

**Measuring a change.** This README states no merge rule; the rule is in the measuring change's
own issue. It states how to run a comparison and what to compare.

A change to Dispatch's write path is measured with six runs of one bundle, one set of flags, one
`--gomaxprocs` and one `--rng-seed`: three with `--dispatch-commit` at the commit before the change
and three at its head. The plans are equal unless the two builds serve different markdown for a
document the window writes, which `replay.base_docs.documents` names on each side, so the runs
differ only in Dispatch.

Run the six on one box in one session, interleaved base, head, base, head, base, head, never three
of one build then three of the other. The absolute cores figure is a property of the machine and
the hour, not of Dispatch alone: the same binary, the same inputs and the same work at 35efd326
gave 9.60-9.98 cores at the full rate on 2026-10-28 23:50Z-01:53Z and 14.65-14.88 cores on the
2026-10-29 afternoon, on the same 2-core pod type. A comparison is valid only between runs
interleaved on one box in one session; runs from two sessions, or two boxes, are not comparable.

```bash
export LOADTEST_REPLAY_ISSUE=<issue>
for i in 1 2 3; do
  packages/envoy/loadtest/run-step.sh base-$i --replay --rate 0.05 --rng-seed 1 --gomaxprocs 2 --dispatch-commit B
  packages/envoy/loadtest/run-step.sh head-$i --replay --rate 0.05 --rng-seed 1 --gomaxprocs 2 --dispatch-commit H
done
```

B is the commit before the change and H is its head; the six steps are `base-1`, `head-1`,
`base-2`, `head-2`, `base-3`, `head-3`, in that order.

What to compare, from each step's `summary.json` and `samples.json`:

| Read | Where | What it says |
| --- | --- | --- |
| The CPU | `summary.json` `replay.dispatch_cpu_s` | Dispatch's CPU-seconds over the window: the figure the three base runs and the three head runs are compared on. The spread is the highest base run's value less the lowest. |
| The same work | `summary.json` `replay.end_state.sha256` | Equal hashes show the runs did the same work. Within one build they are always equal; across the two builds they are equal when neither replanned (`base_docs.replanned` false on both) and the builds' answers are the same, and can differ when the builds' renderings differ (The client's text is the build's, above). |
| Not saturated | `summary.json` `replay.saturated` | False in every one of the six runs that are compared. A saturated run measures the box's limit, not the change (A saturated run, below). |
| Memory | `samples.json` `heap_inuse` and `rss_kib` key `dispatch`, over the samples with `in_window` true | Dispatch's heap in use and its resident size during the window. |

**A saturated run.** A run with `replay.saturated` true is discarded: it is not compared. Run it
again in the same place of the interleave, under a new name and with the same flags: a saturated
`base-2` is run again as `base-2b` before `head-2` starts (`run-step.sh` refuses a step name that
already holds a step). One upload past its 60 s deadline is enough to saturate a run, and at the
default F on a 2-core pod this happens in some full-window runs. If more than one run of one
comparison saturates, the reruns included, run the whole comparison again at a lower F, and name
that F.

The pull request quotes the six runs in their order, their `replay.dispatch_cpu_s`, the spread, the
command and the inputs (`replay.inputs`), and how many runs were saturated and run again.

**What the replay does not reproduce:**

- **GOMAXPROCS 16.** Production's Dispatch ran with 16 cores, and Go's garbage collector runs idle
  mark workers on idle processors, which count as CPU. On this box `GOMAXPROCS` is nproc or
  `--gomaxprocs`, so there are fewer idle workers to count.
- **The machine's own speed.** The cores figure is a property of the box at the hour of the run,
  not of Dispatch alone: the same binary, inputs and work at 35efd326 gave 9.60-9.98 cores at the
  full rate on 2026-10-28 23:50Z-01:53Z and 14.65-14.88 on the 2026-10-29 afternoon, on the same
  2-core pod type. Only runs interleaved on one box in one session compare (Measuring a change,
  above).
- **Reads.** None were logged: the read actions are an assumption, rho per write at the read mix.
- **The edit and upload split.** U is an assumption: a named version with a diff is an upload with
  share U, else an edit.
- **Historical version contents.** The seed's history is synthetic padding at production's sizes.
  The text of a version whose diff was cut at 4,094 characters, or that has no diff, is a
  substitute of the logged size change; a created spec whose version 1 was unreadable is synthetic,
  of the logged size (`spec_sizes`).
- **The 22:47Z restart's warm-up.** Production's Dispatch restarted at 22:47Z, 8 minutes before the
  window. The replay starts on a quiet, prepared template after a 30 s warm-up.
- **Other projects' writes and events with no issue key.** The bundle holds LINUX's issues' event
  logs only: other projects' writes (2.6% of the window's) and events with no issue key are absent.
- **Browser tabs.** Beyond the 2 dashboard streams, no browser: no document websockets, no browser
  document edits or settlements (not sent), no dashboard refetches.
- **Embeddings.** Dispatch gets no AWS credentials locally, so it computes none.
- **Cloud SQL and its proxy.** Dispatch reaches the stack's local Postgres directly.
- **Resolved comments the window never refers to.** The seed does not recreate them.
- **The roster's exact size.** 762 sessions is an inference; the bundle names 452, and the rest are
  synthetic.
- **Sampling.** At F below 1, whole issues are kept or dropped, and `cores_full_rate` divides by F:
  the full rate is an extrapolation.
- **Production block ids.** The block maps are today's blocks, and most anchors' logged blocks no
  longer exist in them, so an anchor lands by the seed's rules (7,002 of 12,076 on the nearest
  surviving block).
- **CLI start-up.** The time a CLI process takes to start between its requests.

## Delivery latency

A **receipt** is one envelope that one session gets on a topic it subscribes to. Its time is when
`loadgen`'s NATS handler runs. A receipt is matched to its write by the event `id` in the
envelope's payload.

A write's **primary event** is the event with the lowest id among the events that carry its marker
and have its kind's primary type. Only primary events on issue topics are timed.

- **From sent**: the receipt time minus the write's send time. The send time is when the client
  started the write's request. For an upload, it is the start of the `POST`, not of the `GET`
  before it. So the value also includes the client's own wait for a connection and the time to
  write the request.
- **From arrival**: the receipt time minus the write's arrival time. The arrival time is when the
  whole request is in Dispatch's socket (`httptrace.ClientTrace.WroteRequest`). When the trace does
  not see that, it is when the client started the request. So the value includes the time in
  Dispatch's accept queue, the header read, the pool wait, the lock and the commit, as the outage
  work's spec asks: "time spent queued before the commit counts" (its acceptance line 4). The
  client's own wait for a connection is in `summary.json` as `conn_wait_ms`, so a generator that
  queues its own requests shows it.
- **From commit**: the receipt time minus the commit time of the primary event's transaction.
  - The seed installs `loadtest.event_commits` and an `AFTER INSERT` trigger on `events`. For each
    new event that carries a marker, the trigger copies the event id, the type and the first marker
    in its payload. Events without a marker insert nothing.
  - After the grace period, `loadgen` reads `pg_xact_commit_timestamp(xmin)` of the rows whose
    marker is one of the run's in-window writes, its nonce included. This needs
    `track_commit_timestamp = on`.
  - The trigger runs inside the transaction and inside the commit-order lock that it measures. Its
    cost is one small insert, with no round trip.
  - The value compares a time from `loadgen`'s clock with a time from Postgres's clock. On the local
    stack both are the box's clock. Elsewhere the column needs the generator and Postgres on one
    clock.

The counts:

- **Offered**: the writes whose scheduled arrival is in the window.
- **Committed**: the offered writes whose primary event is in `loadtest.event_commits` with the
  write's marker, whatever their HTTP status. A write that hit the 30 s client timeout but committed
  counts, and gets delivery latencies. Another run's rows never count: their nonce differs.
- **Expected issue-topic receipts**: the committed primary events × the sessions that subscribe to
  the topic of the event's issue. **Got** is the issue-topic receipts of primary events.
  `missing` = expected − got.
- **Agent-topic receipts**: route copies on session agent topics. Counted (`agent_topic_receipts`),
  not timed.
- **`clock_anomalies`**: the receipts whose event committed before its write arrived. It must be 0
  on the local stack. A value above 0 means that the two clocks disagree.
- **`commit_times_missing`**: the primary events with no commit time. They get no latency from
  commit.

When `loadgen` cannot read the run's rows of `loadtest.event_commits`, `events.commits_error` holds
why. Then every count and latency that comes from the commit rows is 0 because nothing was read, not measured:
committed (also per kind), the issue-topic and agent-topic receipts, the expected receipts,
`missing`, `other_events`, `other_receipts`, `commit_times_missing`, `clock_anomalies` and every
delivery latency. So is `live.live_frame_ms_from_write_response`, and
`resources.dispatch_cpu_s_per_op` is null. The tables show those cells as `-` with the mark
`(commit rows not read)`, and the step fails (Run one step, above). A step with no write in the
window reads no rows: nothing depends on them.

Every percentile in the harness is the lower nearest rank: the value at index
⌊p/100 × (n − 1)⌋ of the sorted values.

## The step directory

| File | What it holds |
| --- | --- |
| `summary.json` | The step's results. Every table cell comes from it (The tables, below). `config.marker_nonce` is the run's marker nonce (Event kinds, above). `config.dashboard` is the dashboard stream the build under test served, `live` or `sse` (The dashboard stream, above), and `config.live` their count. `stack_exits` is there only when a stack process exited during the step: each entry's `name`, `pid` and `first_seen_gone`. Such a step failed. `events.commits_error` is there only when the run's commit rows could not be read (Delivery latency, above). Such a step failed too. |
| `TABLES.md` | The tables of this step. `loadgen tables` writes it. |
| `meta.json` | Mode 600 (owner only). The step name and arguments (`args`: the normalized flag list, each flag given to `loadgen run` once as `-name=value` in name order, with the values of `-token` and `-listener-token` replaced by `<redacted>`, the URL of each of `-url`, `-listener`, `-nats-mon` and `-pprof` read whole by Go's `net/url` and written without its userinfo and its `password` parameter, and `-nats` split at commas as nats.go splits it, each server without its own userinfo; the user name goes too, since nats.go takes a user name with no password as the NATS token. `-pg` is written from pgx's own parse of it (`pgconn.ParseConfig`, the parser `pgx.Connect` uses), never from the text given: `postgres://`, each host and port pgx would try, comma-separated (a port the URL leaves out as pgx fills it in, such as 5432), then `/` and the database, such as `postgres://127.0.0.1:15432/dispatch`; never its user, password, `sslpassword` or any other parameter, and `<redacted>` when pgx does not read it as a URL. No file of the step, and no line that `loadgen run` prints, holds a token, a URL password or a NATS token), `start`, `window_start`, `window_end`, `end`, `nproc`, CPU model, kernel, `box_busy_before` and `box_busy_before_window_s` (5), `GOMAXPROCS` of Dispatch (`unset` by default), the Postgres version (`postgres_version`: empty when it could not be read, and then `postgres_version_error` says why), `build_commit` and `build_dirty` (the checkout `stack.sh` built from), `dispatch_commit`, `dispatch_binary_sha256` and `dispatch_build` (the whole of `$LOADTEST_ROOT/bin/envoy-dispatch.json`: the Dispatch binary this step ran, which `--dispatch-commit`/`LOADTEST_DISPATCH_COMMIT` makes another commit than `build_commit`; empty, and `run.log` says so, when that file is absent), the seed meta, the `stack.json` path, `pg_direct` (always `false` under `-allow-foreign`, which takes nothing about the target from `stack.json`), `allow_foreign`, the pids and `marker_nonce`. `box_busy_before` is the share of the box's CPU time, all CPUs, that was not idle or I/O wait over 5 s just before the step, from two `/proc/stat` reads: `run-step.sh` reads it with `loadgen box-busy` before its reset and passes it as `-box-busy-before`; a `loadgen run` without that flag reads it itself (Requirements, above). `null` when `/proc/stat` gave no ticks. |
| `run.log` | `loadgen run`'s log: the configuration with the marker nonce, the guard, the dashboard choice (the stream count, `live` or `sse`, and the URL), a warning when `box_busy_before` is above 10%, the sampler's pids, the NATS subscriptions, the registrations, the window, failed writes, the first three failed reads of each Postgres probe and of the 5 s sampler's Postgres, pprof and NATS reads (and each one's total, past three), a failed read of the commit rows, of the transaction ids or of the Postgres version, the result line, and the failure line when a stack process exited or the commit rows were not read. An endpoint that a line names is written without its URL password. |
| `samples.json` | The 5 s samples: CPU ticks and RSS per process, `/proc/stat` ticks, Postgres connections by state, goroutines, pool waiters, heap (every 15 s), NATS `/varz`. `failed_reads` names a sample's reads that failed (`postgres`, `goroutines`, `heap`, `nats`); their values in that sample are 0 (`nats` empty) and enter no statistic. A read that failed once the run had ended is neither named nor counted. |
| `cpu.pprof` | Dispatch's CPU profile over the whole window. |
| `heap.pprof` | Dispatch's heap profile at the end of the window. |
| `heap-start.pprof`, `heap-end.pprof` | Under `-source replay`: Dispatch's heap profile at the window's start and end (The replay, above). |
| `end_state.json` | Under `-source replay`: the end state of every document the run wrote, read after the drain, and its sha256 (`replay.end_state`, The replay, above). |
| `last-reset.json` | A `--replay` step's copy of `$LOADTEST_ROOT/replay/last-reset.json` (The replay, above). |
| `heap.txt` | The same heap profile as text (`/debug/pprof/heap?debug=1`), with the runtime's memory statistics. |
| `goroutines.txt` | Dispatch's goroutine dump (`debug=2`) at the end of the window, after the CPU profile. |
| `goroutines.summary.txt` | The goroutines that wait for a pool connection, the goroutines in `events.(*Broker).Append`, other request and document goroutines, and the top states. |
| `cpu.top.txt`, `cpu.cum.txt` | `go tool pprof -top` of `cpu.pprof`: by own time (40 nodes) and by cumulative time (80 nodes). |
| `heap.top.txt` | `go tool pprof -top` of `heap.pprof`, by in-use bytes (40 nodes). |
| `envelope-<type>.json` | One envelope that a session got on an issue topic, per event type, with a marker when one arrived. |
| `envelope-agent.<type>.json` | The same for an agent topic. |
| `dispatch.log` | Dispatch's log for the step. |
| `listener.log`, `nats.log` | The listener's and NATS's logs since their restart in the reset. |

A file that a step could not take is absent, and `run.log` names the reason. The Postgres log stays
in `$LOADTEST_ROOT/pg/log`, the proxy log in `$LOADTEST_ROOT/logs/proxy.out`.

`loadgen tables <step dir>...` renders the tables of one or more steps again. With one directory it
writes `TABLES.md` in it. With more, it writes `TABLES.md` in the current directory, or the file
that `-out` names. Under `stack.sh loadgen`, the current directory is `$LOADTEST_ROOT`. It also
writes `cpu.top.txt`, `cpu.cum.txt`, `heap.top.txt` and `goroutines.summary.txt` in each step
directory. `-tops=false` does not write these files. `run-series.sh` uses `-tops=false` for the
series `TABLES.md`, because `run-step.sh` already wrote them for each step.

## The tables

`TABLES.md` has eight tables, with one row per step, and a ninth, Replay, when a step replayed.
The window is the measured window (`--duration`). "Cores" means CPU-seconds per second. A cell is
`-` when it has no value, and also when the read that it comes from failed; a failed read also
marks the step name with `(commit rows not read)`, `(1 failed read)` or `(<n> failed reads)`
(below), so no cell shows a failed read as a measured 0. A JSON path below is a field of the step's
`summary.json`.

### Events, delivery and requests

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(<process> exited)` when a stack process exited during the step, and `(commit rows not read)` when the run's commit rows could not be read (below). | `config.step`, `stack_exits`, `events.commits_error` |
| `start (UTC)` | date and time | The start of the window. | `meta.json` `window_start` |
| `sessions` | count | The simulated sessions. | `config.sessions` |
| `events/s target / offered / committed` | writes per second | The target rate, then the offered writes and the committed writes, each divided by the window. `0` when the target is 0. Committed is `-` when the commit rows were not read. | `config.events_per_s`, `events.offered_per_s`, `events.committed_per_s` |
| `write errors non-2xx / transport` | count | The offered writes that got a status outside 2xx, then the offered writes that ended with no complete response: a connection error, the 30 s client timeout, or a failed read of the spec before an upload. | `events.write_errors.non_2xx`, `events.write_errors.transport` |
| `delivery ms from arrival p50 / p99` | ms | Delivery latency from arrival, over every issue-topic receipt of a primary event. `-` when the commit rows were not read. | `events.delivery_ms_from_arrival` |
| `delivery ms from commit p50 / p99` | ms | Delivery latency from commit, over the same receipts. `-` when the commit rows were not read. | `events.delivery_ms_from_commit` |
| `issue-topic receipts got / expected` | count | The issue-topic receipts of primary events, then the expected number. `-` when the commit rows were not read. | `events.issue_topic_receipts`, `events.expected_issue_topic_receipts` |
| `req/s target / sent` | requests per second | The target rate, then the requests sent in the window divided by the window. `0` when the target is 0. The mark `(below 95%)` follows when the sent rate is below 95% of the target. | `config.requests_per_s`, `requests.sent_per_s`, `requests.below_target` |
| `req 2xx` | percent | The share of sent requests that got a 2xx status. | `requests.ok_2xx` ÷ `requests.sent` |
| `req ms p50 / p99` | ms | Request latency, from arrival (as for a write) to the end of the response body, over every sent request, failures included. A failed request counts with the time it took to fail: about 30,000 for the client timeout, and about 5,000 for the 5 s connect timeout. A request that never connected has no arrival, so its time starts when the client began the request. | `requests.latency_ms` |
| `requests dropped at the in-flight cap` | count | The requests scheduled in the window that the generator did not send, because 25,000 were in flight. | `requests.dropped_inflight_cap` |

**The below-target mark.** `(below 95%)` in `req/s target / sent` means that the step sent fewer
than 95% of its target requests per second. Its request columns then describe a lighter load than
its name says. `TABLES.md` repeats this under the table when a row has the mark.

**The stack-exit mark.** `(dispatch exited)` after the step name, in every table, means that the
named stack process (`dispatch`, `listener`, `nats`, `proxy` or `postgres`) exited during the step
(`stack_exits` in `summary.json`). The step failed, and its row does not measure the load: its
numbers show a stack without that process. `TABLES.md` repeats this under the first table when a
row has the mark.

**The commit-rows mark.** `(commit rows not read)` after the step name, in this table, the
write-latency-by-kind table, the resources table and the delivery-from-sending table, means that
`loadgen` could not read the run's rows of `loadtest.event_commits`: the database has no
`commit-trigger.sql`, or Postgres did not answer. `events.commits_error` says why. Every cell that
depends on those rows shows `-`, not a measured 0: here the committed rate, both delivery
latencies and the receipts; per kind the committed count; in the resources table the Dispatch
CPU-s per operation; and the delivery from sent and the live frame latency from write response. The
step failed (Run one step, above). `TABLES.md` repeats this under each table where a row has the
mark.

**Connect timeouts above 1x.** At 10x on the 2-vCPU pod (series run of 2026-10-27, step
`step-10x`), most requests and writes ended with `dial tcp …: i/o timeout` after 5 s, and
`req ms p50` was about 5,000. During a second 10x run, a second client (`curl` to `/healthz` from
another process) did not connect either, while about 3,200 connections to Dispatch's port waited
in `SYN_SENT` (`/proc/net/tcp`). It connected again when the generators stopped. Thus new
connections to Dispatch's port were not accepted on the box, and the cause is not the
generator's own client. `run.log` lists each failed write with its error.

### Write latency by kind

There is one column per kind, in this order: `comment`, `anchored`, `reply`, `edit`, `upload`.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above). | `config.step`, `events.commits_error` |
| `comment ms p50 / p99 (committed / offered)` | ms, then count | Write latency of the kind's writes that got a 2xx status: from arrival to the end of the response body. Then the committed and offered writes of the kind. `-` alone when the step offered no write of the kind. `-` before the counts when no write of the kind got a 2xx status. `-` for committed when the commit rows were not read. | `events.by_kind.comment.write_ms`, `.committed`, `.offered` |
| `anchored ms p50 / p99 (committed / offered)` | ms, then count | The same for `anchored`. | `events.by_kind.anchored` |
| `reply ms p50 / p99 (committed / offered)` | ms, then count | The same for `reply`. | `events.by_kind.reply` |
| `edit ms p50 / p99 (committed / offered)` | ms, then count | The same for `edit`. | `events.by_kind.edit` |
| `upload ms p50 / p99 (committed / offered)` | ms, then count | The same for `upload`. The write latency covers the `POST` only, not the `GET` before it. | `events.by_kind.upload` |

`events.by_kind.<kind>` also holds `primary_type` and the kind's delivery latencies.

### Resources

The sampler reads every 5 s. It uses the samples from the window start to 5 s after the window end.
A process's cores are its CPU ticks at the last of those samples minus the ticks at the first,
divided by 100 ticks per second and by the seconds between the two samples.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above), then `(1 failed read)` or `(<n> failed reads)` when n of the sampler's Postgres and pprof reads in the window failed (below). | `config.step`, `events.commits_error`, `resources.pg_read_errors`, `resources.pprof_read_errors` |
| `Dispatch cores mean / peak 5 s` | cores | Dispatch's mean cores in the window, then its highest cores in one 5 s sample interval. | `resources.cpu_cores_mean.dispatch`, `resources.cpu_cores_peak_5s.dispatch` |
| `Postgres cores` | cores | Mean cores of the stack's Postgres: the postmaster, its live children and the postmaster's time for children that exited. | `resources.cpu_cores_mean.postgres` |
| `listener cores` | cores | Mean cores of the listener. | `resources.cpu_cores_mean.listener` |
| `NATS cores` | cores | Mean cores of `nats-server`. | `resources.cpu_cores_mean.nats` |
| `loadgen cores` | cores | Mean cores of the `loadgen run` process: the generators, the sessions and the samplers. | `resources.cpu_cores_mean.loadgen` |
| `proxy cores` | cores | Mean cores of the latency proxy. | `resources.cpu_cores_mean.proxy` |
| `box busy` | percent | The share of the box's total CPU time (all CPUs) that was not idle or I/O wait, from `/proc/stat`. | `resources.sys_busy_frac` |
| `box busy before` | percent | The same share over 5 s just before the step, read by `run-step.sh` before its reset, while Dispatch is stopped: the other work on the machine, other roles' processes in the pod included. Above 10%, the step shared the box (`run.log` warns). | `meta.json` `box_busy_before` |
| `Dispatch RSS max MiB` | MiB | Dispatch's highest resident memory (`VmRSS`) in a sample. | `resources.rss_mib_max.dispatch` |
| `heap in use MiB p50 / max` | MiB | Dispatch's `HeapInuse` from `/debug/pprof/heap?debug=1`, read every 15 s. | `resources.heap_inuse_mib` |
| `goroutines p50 / max` | count | Dispatch's goroutines, from `/debug/pprof/goroutine?debug=1`. | `resources.goroutines` |
| `goroutines waiting for a pool connection p50 / max` | count | Dispatch's goroutines with a pgx pool `Acquire` frame (puddle) in their stack. | `resources.pool_acquire_goroutines` |
| `Postgres connections max` | count | The highest number of `pg_stat_activity` rows for database `dispatch`. It includes up to 4 connections of `loadgen`: the sampler, the lock probe, the ungranted-lock sampler and the transaction-id probe. | `resources.pg_conns_total.max` |
| `connections waiting on a lock p50` | count | The p50 of `dispatch` connections with state `active` and wait event type `Lock`, over the samples that saw at least one. `0` when no sample saw one. An advisory lock wait has this type. | `resources.pg_conns_by_state["active\|Lock"]` |
| `Dispatch CPU-s per operation` | CPU-seconds | Dispatch's mean cores divided by the operation rate. An operation is a committed write or a sent request in the window. `-` when the commit rows were not read. | `resources.dispatch_cpu_s_per_op` |

`resources` also holds every process's RSS, Postgres connections by state, and NATS messages in and
out per second and slow consumers. The NATS rates are over the first and last in-window `/varz`
reads that worked, `null` when fewer than two worked; slow consumers are the last such read's,
`null` when none worked. Under `-allow-foreign`, `loadgen` has no stack pids, so only the `loadgen`
cores have a value, and no stack process is checked for an exit.

A sample's read of `pg_stat_activity` fails when the query fails or the sampler cannot connect;
its read of Dispatch's goroutine or heap profile fails when the GET fails, answers other than 200,
or holds no goroutine total (no `HeapInuse` for the heap); its read of NATS `/varz` fails when the
GET fails, answers other than 200 or holds no JSON. `resources.pg_read_errors` counts the
in-window samples whose Postgres read failed, `resources.pprof_read_errors` the in-window goroutine
and heap reads that failed, and `resources.nats_read_errors` the in-window NATS reads that failed.
The row carries the mark `(1 failed read)` or `(<n> failed reads)` with the sum of the first two;
no column shows the NATS reads. `run.log` shows the first three of each kind and their total; its
totals count every sample from the warm-up to the end of the grace period, while
`pg_read_errors`, `pprof_read_errors` and `nats_read_errors` count in-window samples only, so the
two totals can differ. A failed read enters no statistic, so the Postgres, goroutine, pool-waiter
and heap columns are over the reads that worked, and a column with no read that worked shows `-`,
not 0. `TABLES.md` repeats this under the table when a row has the mark.

### Commit-order lock

The lock probe reads every 20 ms from 20 s after the window start to the window end. When the
window is shorter than 20 s, it starts at the window start. The lock is Dispatch's database-wide
advisory lock `0x4449535041544348` (`internal/dispatch/events/broker.go`), which orders event
commits.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` or `(<n> failed reads)` when the probe had failed reads. | `config.step`, `commit_order_lock.errors` |
| `time held` | percent | The share of samples in which a backend held the lock. | `commit_order_lock.held_fraction` |
| `waiters p50 / max` | count | The backends that wait for the lock, per sample. | `commit_order_lock.waiters` |
| `hold ms p50 / p90 / p99` | ms | Hold durations. One hold is a run of samples with the same holder pid and transaction start. It lasts from its first sample to the sample that ends it. A hold that is open at the last sample is not counted. The resolution is the sample interval. | `commit_order_lock.hold_ms` |
| `holds seen` | count | The holds counted for `hold ms`. | `commit_order_lock.holds_observed` |
| `what the holder was doing (share of held samples)` | text, percent | Up to four holder states, most frequent first, each with its share of held samples. A state is `<state> (<wait event type>:<wait event>)`, or `active: <query>` with the query cut to 70 characters. `idle in transaction (Client:ClientRead)` means that Postgres waits for Dispatch to send the next statement. | `commit_order_lock.holder_doing` (up to 12 entries) |

**The failed-reads mark.** `(<n> failed reads)`, or `(1 failed read)`, after the step name, in this
table and in the ungranted-locks table, means that n of that probe's reads of `pg_locks` failed: a
failed query, or a failed connect, which counts as one and leaves the probe with no sample.
`commit_order_lock.errors` and `ungranted_locks.errors` count them, and `run.log` shows the first
three of each probe and their total. The probe's samples miss those reads, so its shares are over
the reads that worked, and a row with no sample reads `-`, not 0: a probe that failed is never
shown as a lock nobody held. `TABLES.md` repeats this under the table when a row has the mark. The
transaction-ids table uses the mark `(1 failed read)` for its probe (Transaction ids, below). None
of these probes feeds the committed, receipt or delivery columns, so a failed read marks its own
table and does not fail the step.

When the event-lock change removes the commit-order lock, this table reads 0 by construction. No
backend can hold or wait for a lock that Dispatch does not take. The ungranted-locks table
(Ungranted locks, below) is then the measure of lock waits.

### Delivery from sending, and live socket frames

Each dashboard live socket (`--live`) opens `GET /ws/live` as `viewer-<i>` and sends the frame a
dashboard tab sends for the event feed, `{"type":"subscribe","sub":"events","view":{"kind":"events"}}`,
with no resume, so it follows the feed from its head. Dispatch answers `events_start`, then one
`event` frame per event: `{"type":"event","sub":"events","id":"<n>","event":<the event's JSON>}`. Only
`event` frames count as frames; `events_start`, `heartbeat` and every other frame type are read and
not counted. The frame time is when its message arrives. Only the frames that arrive in the window
count. A frame's event id is its `id`. An `ended` frame (the subscription fell behind) is answered
with the same subscribe frame on the same socket, as a tab does. When the socket ends (Dispatch
closes each at a random 30-60 min age with 4001, and at shutdown with 1012), or its subscription is
refused or ends before its `events_start`, the socket is dialed again a second later. Each socket
offers permessage-deflate, as a browser does. `live_bytes` is the bytes of every message the
sockets read, after decompression.

Against a build that serves no `/ws/live` (The dashboard stream, above), each stream is instead
`GET /api/v1/events` as `viewer-<i>`, with `Accept: text/event-stream`, asked again a second after
the stream ends. Dispatch writes each event there as `id: <n>`, `event: <type>` and `data: <the
event's JSON>`, closed by a blank line; a frame counts on that blank line, and `: open` and
`: heartbeat` comments do not. The frame time is when the blank line arrives, and a frame's event
id is its data's `id`, else its `id:` field. `live_bytes` is then the bytes of every stream read.
The columns below are the same for either stream; `config.dashboard` names which one ran.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above). | `config.step`, `events.commits_error` |
| `NATS delivery ms from sent p50 / p99` | ms | Delivery latency from sent (Delivery latency, above), over every issue-topic receipt of a primary event. `-` when the commit rows were not read. | `events.delivery_ms_from_sent` |
| `live frame ms from created_at p50 / p99` | ms | The frame time minus the `created_at` of the frame's event, over every frame with a `created_at`. | `live.live_frame_ms_from_created_at` |
| `live frame ms from write response p50 / p99` | ms | The frame time minus the time when the client read the write's response. Only the frame of a primary event counts. Its write must be in the window, have a 2xx status and be committed. A write with no complete response has no response time, so its frames do not count. `-` when the commit rows were not read. | `live.live_frame_ms_from_write_response` |
| `live frames` | count | The event frames of all live sockets that arrived in the window. | `live.live_frames` |
| `live order violations` | count | The frames whose event id is not more than the event id of the previous frame on the same connection. The check starts again on each new connection. A value above 0 means that a socket sent events out of id order. | `live.live_order_violations` |

### Ungranted locks

The ungranted-lock sampler reads `pg_locks` once a second, from the same start to the same end as the
lock probe (Commit-order lock, above). It reads over a direct Postgres connection, not through the
proxy. Each sample lists every heavyweight lock that a backend waits for (`granted` is false). The
sampler groups these waits by lock type, target and mode.

The target names the lock:

- `commit_order_lock`: Dispatch's commit-order advisory lock (Commit-order lock, above).
- `notify_commit_lock`: the commit lock of Postgres's `NOTIFY`. It is the `object` lock on database
  0 that `PreCommit_Notify` takes.
- A relation name, for a `relation` or `tuple` lock. For a relation in another database, `loadgen`
  reads the name after the step, with one short connection to that database. When that read
  fails, the target is `<database>:<oid>`.
- `row_wait`, for a `transactionid` lock: a wait for a row that another transaction locked.
- `<classid>:<objid>`, for another advisory lock.
- The ids of the lock, joined by `:`, for any other lock.

For each wait, the sampler records the waiter's statement and the statement of its first blocker.
The first blocker is the backend with the lowest pid that holds the same lock, granted, in the same
`pg_locks` read. For a relation lock, that holder can hold a mode that does not conflict with the
wait. Each statement is the `query` of `pg_stat_activity`, cut to 80 characters.

The sampler reads once a second, not every 20 ms as the lock probe does, because each read costs
Postgres CPU on the box it measures. Read every 20 ms with about 45 waiters at 1x, one read took 75
to 180 ms of a Postgres backend, and 1x committed 0.29 to 0.82 events/s, against 1.63 to 1.81
without the sampler, on the same build and seed (2026-10-27). It never calls `pg_blocking_pids`,
which takes exclusive access to Postgres's lock manager.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` or `(<n> failed reads)` when the sampler had failed reads (Commit-order lock, above). | `config.step`, `ungranted_locks.errors` |
| `samples with an ungranted lock` | percent | The share of samples that saw at least one ungranted lock. | `ungranted_locks.share_of_samples_with_any_ungranted` |
| `locks waited for: type target mode, samples with a waiter, waiters p50 / max` | text, percent, count | Up to four groups. The group with a waiter in the most samples comes first. Each group shows its lock type, target and mode. Then it shows the share of samples with at least one waiter in the group. Then it shows the waiters per sample, p50 / max. A sample with no waiter in the group counts as 0. | `ungranted_locks.groups[]`: `locktype`, `target`, `mode`, `share_of_samples_with_waiter`, `waiters_p50`, `waiters_max` |
| `top waiter query → first blocker query (count), per lock` | text, count | One entry for each group of the column before, in the same order. The entry is the most frequent pair of waiter statement and first-blocker statement, then the number of waits with that pair. `(none)` is an empty statement: the sampler found no blocker, or the backend had no statement text. | `ungranted_locks.groups[].top_pairs[0]`: `waiter_query`, `blocker_query`, `count` |

`ungranted_locks.samples` is the number of samples and `ungranted_locks.errors` the failed reads.
`groups` holds every group, and `top_pairs` holds up to 3 pairs per group, most frequent first. A
step whose sampler took no sample has `-` in every cell.

### Transaction ids

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` when the transaction-id probe's connect or one of its two reads failed (Commit-order lock, above). | `config.step`, `resources.xid_read_error` |
| `transaction ids per s` | transaction ids per second | The transaction ids that Postgres assigned per second in the window, in all databases of the server. The transaction-id probe reads the next transaction id (`pg_snapshot_xmax(pg_current_snapshot())`) at the window start and at the window end. It divides the difference by the seconds between the two reads. The probe assigns no transaction id itself. `-` with the mark when a read failed (`resources.xid_read_error` holds the error, and `run.log` shows it); `-` alone when `resources` is null. | `resources.xids_per_s` |

### Requests by route

There is one row per route and one column per step.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `route` | name | The route name, `<caller> GET <path template>`; under `-source replay`, `<method> <pattern> [<kind>]`. | the keys of `requests.routes` |
| `<step name>` | ms, then count | Request latency p50 / p99 of the route in that step, then (failures / count). A failure is a status outside 2xx or a transport error, an answer the action expected (`tolerated`) apart. The count is the sent requests of the route. `-` when the step sent no request on the route. | `requests.routes.<route>.latency_ms`, `.failures`, `.count` |

### Replay

One row per step that replayed (`-source replay`); the table is absent otherwise.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `F`, `rho`, `U` | factor, count, share | The rate factor, the read actions per write, the upload share. | `replay.inputs.rate_factor`, `.reads_per_write`, `.upload_share` |
| `window s` | s | The window between the two `/proc` reads. | `replay.window_s` |
| `Dispatch CPU s` | CPU-seconds | Dispatch's CPU over the window. | `replay.dispatch_cpu_s` |
| `cores at full rate` | cores | CPU-seconds / window / F. | `replay.cores_full_rate` |
| `idle-corrected` | cores | The same with the idle cores before the window taken off. | `replay.cores_full_rate_idle_corrected` |
| `saturated` | yes (reasons) or no | The saturation rule (The replay, above). | `replay.saturated`, `.saturated_reasons` |
| `writes sent / planned`, `failures` | count | Over every write kind. | `replay.writes` |
| `substitutes quote / edit` | count | Substituted anchor quotes and version contents. | `replay.substitutes` |

## The guard

`loadgen run`, `loadgen seed` and the reference commands (`seed-refs`, `refs-shape`, `refs-fetch`)
read every endpoint and token from `stack.json`. Never point them at the pod's live listener
(`127.0.0.1:9020`) or at production. A step writes comments and document versions at the rate it
is given. The guard stops a mistaken target before the first write:

1. An endpoint flag (`-url`, `-listener`, `-nats`, `-nats-mon`, `-pprof`, `-pg`, `-token`,
   `-listener-token`) that differs from `stack.json` is refused before any connection, unless
   `-allow-foreign` is given. The refusal names the flag and `-allow-foreign`. Under
   `-allow-foreign`, every one of these flags is required, and the first one left empty is refused
   by name before any connection; so is a URL flag whose value is not a URL with a scheme and a
   host as Go's `net/url` reads it (for `-nats`, a comma-separated list of servers, each naming a
   host), and a `-pg` that does not start with `postgres://` or `postgresql://` (case-sensitive,
   as pgx checks it) or that pgx's own parser (`pgconn.ParseConfig`) refuses. Each is refused by
   its name and the reason alone, never the value or pgx's error, since an error that printed the
   value would print its credentials.
2. Before its first write, `loadgen` checks:
   - NATS `/varz` `server_name` is the stack name.
   - Dispatch answers `GET /api/v1/projects` with the stack's random agent token.
   - The listener answers `/v1/sessions` with the stack's random API token, and refuses a random
     other token.

   A failed check exits before any write, and names the check.

The pod's live listener and production Dispatch hold other tokens, so a mistaken target fails the
check.

`-allow-foreign` is for a stack that is not local, such as the cluster load test's copy.
With it, every endpoint and token comes from its flag, and none from `stack.json`. A flag that fell
back to the local stack would have a run write to the remote Dispatch while it counted commits in
the local database or profiled the local Dispatch, with no error.
Give `-nats-server-name` to keep the NATS check. The sampler gets no pids. `meta.json` records
`allow_foreign`, and `pg_direct` false.

## Differences from production

- **Two vCPUs, shared.** Postgres, the proxy, NATS, the listener, Dispatch and `loadgen` share the
  2 CPUs. Production Dispatch has 16 CPUs and a 16-vCPU database instance (root report). So the
  load at which Dispatch fails here is lower than in production. The mechanism is what carries
  over.
- **gVisor.** Every syscall costs more in the sandbox. In the root report, `Syscall6` was 16-61% of
  Dispatch's CPU samples. INFERENCE: Dispatch's CPU per operation here is higher than in
  production, if the production node is not gVisor.
- **The database.** Local Postgres 16.2 with pgvector, from the `pgserver` wheel, with the settings
  above. Dispatch reaches it through the latency proxy at `-rtt 1ms`, which adds about 4 ms per
  round trip, not 1 ms. `LOADTEST_PG_DIRECT=1` removes the proxy.
- **Postgres 16, not 18.** The harness runs PostgreSQL 16.2 (the `pgserver` wheel), while
  production Dispatch runs PostgreSQL 18 since 2026-10-28 (`gcp:Dispatch`'s default database
  version is `POSTGRES_18`). So the harness compares a fix's before and after on one version, and
  absolute numbers on 18 come from the cluster load test.
- **Header identity.** A person's request names the person in `X-Dispatch-User`. Each such request
  also writes a `people` row. Production uses cookie identity. Agents use the shared bearer token
  and a session actor.
- **No embeddings.** Dispatch gets no AWS credentials or region. Meaning search is unavailable, and
  search falls back to keyword.
- **A smaller dataset.** 1,000 issues and about 10,000 events at the start of each step. A replay
  step starts on LINUX's 1,047 issues at their real sizes (The replay, above).
- **No browser documents.** No document websockets and no dashboard refetch fan-out. The browser
  share of the route mix is plain GETs. The live sockets subscribe to the event feed alone (no
  delivery or agent views), time each event frame, and do nothing more.
- **One listener, sessions on NATS.** The sessions receive from NATS directly, as the pi plugin's
  self-subscription does. They register with port 0, so the listener delivers nothing to them.
- **One process each.** One Dispatch process, one NATS server and one listener, all on
  `127.0.0.1`. Dispatch's `GOMAXPROCS` is `LOADTEST_GOMAXPROCS`, nproc by default; production's
  was 16.

## Differences from the root report

Compare the right rows:

- 0.25x and 0.5x run 100 and 200 sessions (the spec's definition). The report ran 350 and 367.
- Every step starts from the seed. The report's database grew across steps, which it ran in the
  order 1x, 2x, 5x, controls, 10x, 1x-rerun, 0.25x, 0.5x.
- Delivery is reported from arrival and from commit (the spec's columns), and from sent. The report
  had "after commit response" and "after event created_at".
- Committed writes come from the database. The report counted 2xx responses.
- The default event mix is production's (`production-2026-10-26`), which adds replies and uploads.
  `--event-mix report-2026-10-27` runs the report's mix. Compare that row with the report's
  `step-1x-rerun`.
- The CPU profile covers the whole window. The report took 30 s.
- The goroutine dump and the heap profile come at the end of the window, outside the CPU profile.
  The report took the dump inside the profile (its bias 6).
- The lock probe runs from 20 s into the window to its end. The report sampled for 60 s.
- Postgres CPU counts only the stack's postmaster, its children and its exited children. The
  report summed every process named `postgres` on the box.
- The request generator caps at 25,000 requests in flight. The report capped at 4,000.
- Dispatch is built from this checkout. The report built a branch with a patched pool size, which
  main now has (`sharedPoolSize = 64`).

## What this harness does not do

- The cluster load test can use `loadgen run -allow-foreign` with every endpoint and
  token flag and its own seed. It gets:
  - no `/proc` CPU or RSS columns, because it has no pids,
  - no committed counts, issue-topic receipts or delivery latencies at all when its database lacks
    `loadtest.event_commits`. The read of the commit rows fails, every cell that depends on them
    shows `-` with the mark `(commit rows not read)` (The tables, above), and `loadgen run` exits
    non-zero naming the read (`events.commits_error`). Such a database needs `commit-trigger.sql`
    (`loadgen trigger-sql` prints it) installed before the run, and `track_commit_timestamp = on`,
  - no delivery latency from commit when its database has the trigger but
    `track_commit_timestamp` is off: `commit_times_missing` counts those events, and the
    committed counts, receipts and the other latencies stay,
  - no reset per step.
- These belong to the cluster load test: 10-minute steps, one-hour soak trends (the 5 s series in
  `samples.json` is there for them), sampling of the production database and restarts during load.
- The browser-editor load work adds its browser-editor workload, its round-trip counter and its
  document-room sampler
  to this package.
- The replay of LINUX's write log is this package's second request source (`-source replay`). The
  route-mix file and `routes.go` stay its GET generator.
- `run-series.sh` passes no `-replay-*` flag: a replay step is one `run-step.sh <name> --replay`
  (The replay, above).
